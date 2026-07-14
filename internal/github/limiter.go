package github

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// RateLimiter is the process-wide GitHub-call scheduler (#683). Every gh call
// routed through GhClient.run acquires a slot from it before the subprocess exec
// and releases the slot afterward, so a single shared limiter smooths the bursts
// the daemon's polling + many concurrent agent gh calls would otherwise fire in
// parallel. It also carries the SECONDARY (abuse-detection) rate-limit backoff:
// when a gh call ultimately FAILS with a 403/429 "secondary rate limit" it records
// a process-wide pause so every OTHER pending GitHub call waits the cooldown out
// (respecting Retry-After, else exponential fallback) instead of retry-storming the
// abuse detector, which only prolongs the block.
//
// The zero-value / DefaultRateLimiter() is INERT: MaxConcurrent 0 (no concurrency
// cap), MinInterval 0 (no spacing), BackoffEnabled false (secondary hits are
// counted but never pause). An inert limiter makes Acquire an immediate no-op, so
// single-call latency and existing behavior are byte-identical until an operator
// (via ConfigureDefault from the [github] config section, applied at daemon start)
// turns knobs on. Calls are NEVER dropped — they queue/delay and only ctx
// cancellation aborts an Acquire.
type RateLimiter struct {
	mu sync.Mutex

	maxConcurrent    int
	minInterval      time.Duration
	backoffEnabled   bool
	baseBackoff      time.Duration
	maxBackoff       time.Duration
	callsPerHourWarn int

	// sem is the concurrency semaphore; nil when maxConcurrent <= 0 (unlimited).
	sem chan struct{}

	// lastStart is the reserved start time of the most recently admitted call,
	// used to space successive starts by minInterval.
	lastStart time.Time
	// backoffUntil is the wall-clock instant before which no call may start while
	// a secondary-limit pause is active.
	backoffUntil time.Time
	// backoffStreak escalates the exponential fallback across successive secondary
	// EPISODES (it is not bumped by concurrent duplicate reports inside one window).
	backoffStreak int
	// secondaryHits is a lifetime counter of observed secondary-limit failures,
	// surfaced by Snapshot for observability.
	secondaryHits int
	// callTimes is a sliding daemon-local window of gh calls made through this
	// limiter. It is accounting only: calls are never rejected at the threshold.
	callTimes   []time.Time
	callsWarned bool

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	logf  func(string, ...any)
}

// DefaultBaseBackoff / DefaultMaxBackoff bound the exponential secondary-limit
// fallback used when a response carries no Retry-After. GitHub's secondary
// cooldowns are typically ~60s to a few minutes, so the base is one minute and the
// cap five — wide enough to actually clear the abuse window, finite enough that the
// daemon resumes on its own once the window passes.
const (
	DefaultBaseBackoff = 60 * time.Second
	DefaultMaxBackoff  = 5 * time.Minute
)

// RetryAfterCeiling is the hard upper bound on a server-supplied Retry-After that
// the limiter will honor. A Retry-After is normally trusted verbatim (the server
// knows how long its abuse window is), but an unbounded value — a pathological or
// misparsed 'Retry-After: 86400', a proxy error body, a corrupted string — would
// otherwise freeze EVERY GitHub call process-wide for that entire duration with no
// self-recovery (NoteSuccess can't fire while all calls are paused), a self-inflicted
// outage for an unattended-reliability tool. Clamping to 30m keeps the daemon
// resuming on its own within a bounded window while still comfortably covering any
// real GitHub secondary cooldown (seconds to a few minutes). The effective ceiling is
// max(maxBackoff, RetryAfterCeiling) so an operator who deliberately configures a
// larger MaxBackoff is never clamped below their own choice.
const RetryAfterCeiling = 30 * time.Minute

// RateLimiterConfig is the resolved, code-level limiter configuration. The config
// package (which must not import this package) builds it from the [github] section
// and hands it to ConfigureDefault; tests build it directly with fake clocks.
type RateLimiterConfig struct {
	// MaxConcurrent caps in-flight gh calls process-wide. 0 (the default) means
	// unlimited — no concurrency gate.
	MaxConcurrent int
	// MinInterval is the minimum spacing between successive call STARTS. 0 (the
	// default) means no spacing.
	MinInterval time.Duration
	// BackoffEnabled turns the secondary-rate-limit pause on. When false, secondary
	// hits are still counted (Snapshot.SecondaryHits) but never pause calls.
	BackoffEnabled bool
	// BaseBackoff / MaxBackoff bound the exponential fallback used when a secondary
	// response carries no Retry-After. Zero values fall back to the Default*Backoff
	// constants.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// CallsPerHourWarn logs once when the sliding one-hour call count reaches this
	// threshold. 0 disables the warning; it never gates calls.
	CallsPerHourWarn int

	// Now / Sleep / Logf are optional injection seams (tests supply a fake clock and
	// capture logs). Nil values fall back to the real clock, a ctx-aware sleep, and a
	// no-op logger.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
	Logf  func(string, ...any)
}

// NewRateLimiter builds a limiter from cfg, filling in the real clock / sleep and
// a no-op logger when the injection seams are unset.
func NewRateLimiter(cfg RateLimiterConfig) *RateLimiter {
	base := cfg.BaseBackoff
	if base <= 0 {
		base = DefaultBaseBackoff
	}
	max := cfg.MaxBackoff
	if max <= 0 {
		max = DefaultMaxBackoff
	}
	if max < base {
		max = base
	}
	l := &RateLimiter{
		maxConcurrent:    cfg.MaxConcurrent,
		minInterval:      cfg.MinInterval,
		backoffEnabled:   cfg.BackoffEnabled,
		baseBackoff:      base,
		maxBackoff:       max,
		callsPerHourWarn: cfg.CallsPerHourWarn,
		now:              cfg.Now,
		sleep:            cfg.Sleep,
		logf:             cfg.Logf,
	}
	if l.maxConcurrent > 0 {
		l.sem = make(chan struct{}, l.maxConcurrent)
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.sleep == nil {
		l.sleep = sleepContext
	}
	if l.logf == nil {
		l.logf = func(string, ...any) {}
	}
	return l
}

// NoteCall records one admitted gh invocation in the sliding one-hour window.
// It is observability-only and never delays or rejects the call.
func (l *RateLimiter) NoteCall() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneCallsLocked(now)
	l.callTimes = append(l.callTimes, now)
	if l.callsPerHourWarn > 0 && len(l.callTimes) >= l.callsPerHourWarn && !l.callsWarned {
		l.callsWarned = true
		l.logf("github: %d calls in the last hour reached calls_per_hour_warn=%d (daemon-local approximate count)", len(l.callTimes), l.callsPerHourWarn)
	}
}

func (l *RateLimiter) pruneCallsLocked(now time.Time) {
	cutoff := now.Add(-time.Hour)
	first := 0
	for first < len(l.callTimes) && l.callTimes[first].Before(cutoff) {
		first++
	}
	if first > 0 {
		l.callTimes = append([]time.Time(nil), l.callTimes[first:]...)
	}
	if l.callsPerHourWarn <= 0 || len(l.callTimes) < l.callsPerHourWarn {
		l.callsWarned = false
	}
}

// DefaultRateLimiter returns the inert limiter used before any ConfigureDefault:
// no concurrency cap, no spacing, backoff off. Acquire on it is an immediate no-op.
func DefaultRateLimiter() *RateLimiter {
	return NewRateLimiter(RateLimiterConfig{})
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Acquire blocks until a call may start: it waits out any active secondary-limit
// backoff, takes a concurrency slot (when a cap is set), and enforces the minimum
// start spacing. It returns only nil (admitted) or ctx.Err() (cancelled) — a call
// is never dropped. On a non-nil return the caller must NOT invoke Release; on nil
// it must Release exactly once.
func (l *RateLimiter) Acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Take the concurrency slot first so at most maxConcurrent goroutines proceed to
	// the spacing/backoff wait at once.
	if l.sem != nil {
		select {
		case l.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Reserve a start time spaced by minInterval, atomically across goroutines.
	l.mu.Lock()
	start := l.now()
	if l.minInterval > 0 {
		if spaced := l.lastStart.Add(l.minInterval); spaced.After(start) {
			start = spaced
		}
	}
	l.lastStart = start
	l.mu.Unlock()

	// Wait until both the reserved start time AND the (possibly-extending) backoff
	// window have passed. The loop re-reads backoffUntil each pass because a
	// concurrent secondary hit can extend it while we sleep.
	for {
		l.mu.Lock()
		now := l.now()
		target := start
		if l.backoffEnabled && l.backoffUntil.After(target) {
			target = l.backoffUntil
		}
		wait := target.Sub(now)
		l.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		if err := l.sleep(ctx, wait); err != nil {
			if l.sem != nil {
				<-l.sem
			}
			return err
		}
	}
}

// Release frees the concurrency slot taken by a successful Acquire.
func (l *RateLimiter) Release() {
	if l == nil || l.sem == nil {
		return
	}
	select {
	case <-l.sem:
	default:
	}
}

// NoteSecondaryLimit records a process-wide secondary-rate-limit pause after a gh
// call ultimately failed with one. retryAfter, when > 0, is honored (the server told
// us how long) but clamped to max(maxBackoff, RetryAfterCeiling) so a pathological
// value cannot freeze GitHub I/O indefinitely; otherwise an exponential fallback
// (baseBackoff shifted
// by the episode streak, capped at maxBackoff) is used. Concurrent duplicate
// reports inside an already-active window extend it (to cover a larger Retry-After)
// but do not escalate the streak, so a burst of simultaneous failures does not blow
// the exponent up. It always counts the hit for observability even when backoff is
// disabled.
func (l *RateLimiter) NoteSecondaryLimit(retryAfter time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secondaryHits++
	if !l.backoffEnabled {
		return
	}
	now := l.now()
	fresh := !now.Before(l.backoffUntil) // not currently inside an active window
	if fresh {
		l.backoffStreak++
	}
	delay := retryAfter
	if delay <= 0 {
		shift := l.backoffStreak - 1
		if shift < 0 {
			shift = 0
		}
		delay = l.baseBackoff
		for i := 0; i < shift; i++ {
			delay *= 2
			if delay >= l.maxBackoff {
				delay = l.maxBackoff
				break
			}
		}
		if delay > l.maxBackoff {
			delay = l.maxBackoff
		}
	} else {
		// A server-supplied Retry-After is trusted over the exponential fallback, but
		// clamped to a sane ceiling so a pathological/misparsed value cannot freeze the
		// whole GitHub subsystem indefinitely (see RetryAfterCeiling).
		ceiling := l.maxBackoff
		if RetryAfterCeiling > ceiling {
			ceiling = RetryAfterCeiling
		}
		if delay > ceiling {
			l.logf("github: Retry-After %s exceeds ceiling %s; clamping",
				delay.Round(time.Second), ceiling.Round(time.Second))
			delay = ceiling
		}
	}
	until := now.Add(delay)
	if until.After(l.backoffUntil) {
		l.backoffUntil = until
	}
	l.logf("github: secondary rate limit hit; pausing GitHub calls for %s (until %s, hit #%d)",
		delay.Round(time.Second), l.backoffUntil.UTC().Format(time.RFC3339), l.secondaryHits)
}

// NoteSuccess records a clean gh call, resetting the exponential episode streak so
// a later, unrelated secondary hit starts its backoff from the base again.
func (l *RateLimiter) NoteSuccess() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.backoffStreak = 0
	l.mu.Unlock()
}

// RateLimiterState is a point-in-time snapshot of the limiter for status/log
// surfaces.
type RateLimiterState struct {
	MaxConcurrent    int
	MinInterval      time.Duration
	BackoffEnabled   bool
	InBackoff        bool
	BackoffRemaining time.Duration
	SecondaryHits    int
	CallsInLastHour  int
}

// Snapshot returns the limiter's current configuration and live backoff state.
func (l *RateLimiter) Snapshot() RateLimiterState {
	if l == nil {
		return RateLimiterState{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneCallsLocked(l.now())
	state := RateLimiterState{
		MaxConcurrent:   l.maxConcurrent,
		MinInterval:     l.minInterval,
		BackoffEnabled:  l.backoffEnabled,
		SecondaryHits:   l.secondaryHits,
		CallsInLastHour: len(l.callTimes),
	}
	if l.backoffEnabled {
		if remaining := l.backoffUntil.Sub(l.now()); remaining > 0 {
			state.InBackoff = true
			state.BackoffRemaining = remaining
		}
	}
	return state
}

// --- process-global default limiter -----------------------------------------

var (
	defaultLimiterMu sync.RWMutex
	defaultLimiter   = DefaultRateLimiter()
)

// ConfigureDefault installs a freshly-built shared limiter as the process default
// used by every GhClient that does not carry its own Limiter. A fresh limiter is
// built each call so reconfiguration resets live backoff state (and so a test that
// configures the default never poisons the next test's clean slate). In-flight
// calls that already Acquired the previous limiter keep using it for their matching
// Release (GhClient.run captures the limiter once per call).
func ConfigureDefault(cfg RateLimiterConfig) {
	limiter := NewRateLimiter(cfg)
	defaultLimiterMu.Lock()
	defaultLimiter = limiter
	defaultLimiterMu.Unlock()
}

// DefaultLimiter returns the current process-global limiter.
func DefaultLimiter() *RateLimiter {
	defaultLimiterMu.RLock()
	defer defaultLimiterMu.RUnlock()
	return defaultLimiter
}

// DefaultLimiterSnapshot returns the process-global limiter's live state.
func DefaultLimiterSnapshot() RateLimiterState {
	return DefaultLimiter().Snapshot()
}

// --- secondary-limit detection ----------------------------------------------

// isSecondaryRateLimit reports whether a gh result carries GitHub's SECONDARY
// (abuse-detection) rate-limit signature — the burst/concurrency limit that the
// #683 incident tripped while the primary quota was fine. It is deliberately
// narrower than isRateLimit: only the secondary/abuse phrasing (which arrives as a
// 403, occasionally a 429) engages the process-wide pause.
func isSecondaryRateLimit(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "secondary rate limit") ||
		strings.Contains(text, "abuse detection") ||
		strings.Contains(text, "abuse-detection") ||
		strings.Contains(text, "triggered an abuse")
}

var retryAfterPattern = regexp.MustCompile(`(?i)retry[- ]after[:\s]+(\d+)`)

// parseRetryAfter extracts a Retry-After delay (in seconds) from a gh result when
// the header/message carries one, returning 0 when absent so the caller falls back
// to the exponential backoff. Only the delta-seconds form is parsed (gh surfaces
// the header as an integer); an HTTP-date Retry-After is treated as absent.
func parseRetryAfter(result subprocess.Result) time.Duration {
	match := retryAfterPattern.FindStringSubmatch(result.Stdout + "\n" + result.Stderr)
	if len(match) < 2 {
		return 0
	}
	seconds, err := strconv.Atoi(match[1])
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
