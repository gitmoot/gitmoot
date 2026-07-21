package github

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// fakeClock is a manually-advanced clock. Its sleep does NOT block real time: it
// advances the clock by the requested duration and returns, so limiter waits are
// deterministic and instant in tests.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	cancel context.Context
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) totalSlept() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total time.Duration
	for _, d := range c.slept {
		total += d
	}
	return total
}

func TestInertLimiterAcquireIsInstant(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(RateLimiterConfig{Now: clock.Now, Sleep: clock.Sleep})
	for i := 0; i < 5; i++ {
		if err := l.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		l.Release()
	}
	if got := clock.totalSlept(); got != 0 {
		t.Fatalf("inert limiter slept %s, want 0 (byte-identical single-call latency)", got)
	}
	if state := l.Snapshot(); state.InBackoff {
		t.Fatalf("inert limiter should never be in backoff")
	}
}

func TestMinIntervalSpacesStarts(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(RateLimiterConfig{MinInterval: time.Second, Now: clock.Now, Sleep: clock.Sleep})
	// First call starts immediately; each subsequent call waits one interval.
	for i := 0; i < 4; i++ {
		if err := l.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		l.Release()
	}
	// 3 gaps of 1s after the first immediate start.
	if got := clock.totalSlept(); got != 3*time.Second {
		t.Fatalf("total spacing = %s, want 3s", got)
	}
}

func TestConcurrencyCapLimitsInFlight(t *testing.T) {
	l := NewRateLimiter(RateLimiterConfig{MaxConcurrent: 2})
	var inFlight, peak int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := l.Acquire(context.Background()); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			l.Release()
		}()
	}
	close(start)
	wg.Wait()
	if peak > 2 {
		t.Fatalf("peak in-flight = %d, want <= 2 (concurrency cap)", peak)
	}
}

func TestSecondaryLimitBacksOffWithExponentialFallback(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    60 * time.Second,
		MaxBackoff:     5 * time.Minute,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	// First episode: no Retry-After -> base backoff (60s).
	l.NoteSecondaryLimit(0)
	if state := l.Snapshot(); !state.InBackoff || state.BackoffRemaining != 60*time.Second {
		t.Fatalf("after first hit: %+v, want InBackoff with 60s remaining", state)
	}
	// A pending call must wait out the full window.
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()
	if got := clock.totalSlept(); got != 60*time.Second {
		t.Fatalf("waited %s, want 60s", got)
	}
	// Second episode (window already elapsed): exponential -> 120s.
	l.NoteSecondaryLimit(0)
	if state := l.Snapshot(); state.BackoffRemaining != 120*time.Second {
		t.Fatalf("after second episode: remaining=%s, want 120s", state.BackoffRemaining)
	}
	if state := l.Snapshot(); state.SecondaryHits != 2 {
		t.Fatalf("SecondaryHits = %d, want 2", state.SecondaryHits)
	}
}

func TestSecondaryLimitRespectsRetryAfter(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    60 * time.Second,
		MaxBackoff:     5 * time.Minute,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	// Retry-After larger than MaxBackoff is honored verbatim (the server told us).
	l.NoteSecondaryLimit(600 * time.Second)
	if state := l.Snapshot(); state.BackoffRemaining != 600*time.Second {
		t.Fatalf("remaining = %s, want 600s (Retry-After honored over MaxBackoff)", state.BackoffRemaining)
	}
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()
	if got := clock.totalSlept(); got != 600*time.Second {
		t.Fatalf("waited %s, want 600s", got)
	}
}

func TestSecondaryLimitClampsPathologicalRetryAfter(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    60 * time.Second,
		MaxBackoff:     5 * time.Minute,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	// A spurious/misparsed 24h Retry-After must NOT freeze GitHub I/O for a day: it is
	// clamped to max(maxBackoff, RetryAfterCeiling) = 30m so the daemon self-recovers.
	l.NoteSecondaryLimit(24 * time.Hour)
	if state := l.Snapshot(); state.BackoffRemaining != RetryAfterCeiling {
		t.Fatalf("remaining = %s, want %s (pathological Retry-After clamped)", state.BackoffRemaining, RetryAfterCeiling)
	}
}

func TestSecondaryLimitClampCeilingHonorsLargerMaxBackoff(t *testing.T) {
	clock := newFakeClock()
	// An operator who deliberately configures a MaxBackoff larger than the ceiling is
	// never clamped below their own choice.
	l := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    60 * time.Second,
		MaxBackoff:     45 * time.Minute,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	l.NoteSecondaryLimit(24 * time.Hour)
	if state := l.Snapshot(); state.BackoffRemaining != 45*time.Minute {
		t.Fatalf("remaining = %s, want 45m (clamp ceiling respects larger MaxBackoff)", state.BackoffRemaining)
	}
}

func TestConcurrentSecondaryHitsDoNotEscalateStreak(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    60 * time.Second,
		MaxBackoff:     10 * time.Minute,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	// Five simultaneous failures inside one window must not blow the exponent up:
	// the streak advances once, so the window stays at the base (60s), not 60s<<4.
	for i := 0; i < 5; i++ {
		l.NoteSecondaryLimit(0)
	}
	if state := l.Snapshot(); state.BackoffRemaining != 60*time.Second {
		t.Fatalf("remaining = %s, want 60s (concurrent duplicates must not escalate)", state.BackoffRemaining)
	}
}

func TestNoteSuccessResetsStreak(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    60 * time.Second,
		MaxBackoff:     10 * time.Minute,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	l.NoteSecondaryLimit(0) // 60s
	// Advance past the window and record recovery.
	clock.Sleep(context.Background(), 60*time.Second)
	l.NoteSuccess()
	// A later, unrelated episode starts from the base again (not escalated).
	l.NoteSecondaryLimit(0)
	if state := l.Snapshot(); state.BackoffRemaining != 60*time.Second {
		t.Fatalf("remaining = %s, want 60s (NoteSuccess should reset streak)", state.BackoffRemaining)
	}
}

func TestBackoffDisabledCountsButDoesNotPause(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: false,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	l.NoteSecondaryLimit(600 * time.Second)
	state := l.Snapshot()
	if state.InBackoff {
		t.Fatalf("backoff disabled but limiter paused")
	}
	if state.SecondaryHits != 1 {
		t.Fatalf("SecondaryHits = %d, want 1 (still counted for observability)", state.SecondaryHits)
	}
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()
	if got := clock.totalSlept(); got != 0 {
		t.Fatalf("waited %s, want 0 (backoff disabled)", got)
	}
}

func TestAcquireRespectsContextCancel(t *testing.T) {
	l := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    time.Hour,
		MaxBackoff:     time.Hour,
	})
	l.NoteSecondaryLimit(0) // ~1h backoff, real clock
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire err = %v, want context.Canceled", err)
	}
}

// --- detection helpers ---

func TestIsSecondaryRateLimit(t *testing.T) {
	cases := []struct {
		name string
		res  subprocess.Result
		want bool
	}{
		{"secondary phrasing", subprocess.Result{Stderr: "HTTP 403: You have exceeded a secondary rate limit"}, true},
		{"abuse phrasing", subprocess.Result{Stderr: "You have triggered an abuse detection mechanism"}, true},
		{"primary rate limit is not secondary", subprocess.Result{Stderr: "API rate limit exceeded"}, false},
		{"429 alone is not secondary", subprocess.Result{Stderr: "HTTP 429: too many requests"}, false},
		{"clean", subprocess.Result{Stdout: "{}"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSecondaryRateLimit(tc.res); got != tc.want {
				t.Fatalf("isSecondaryRateLimit = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		res  subprocess.Result
		want time.Duration
	}{
		{"header form", subprocess.Result{Stderr: "Retry-After: 42"}, 42 * time.Second},
		{"message form", subprocess.Result{Stderr: "please retry after 90 seconds"}, 90 * time.Second},
		{"absent", subprocess.Result{Stderr: "secondary rate limit"}, 0},
		{"http-date ignored", subprocess.Result{Stderr: "Retry-After: Wed, 21 Oct 2015 07:28:00 GMT"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.res); got != tc.want {
				t.Fatalf("parseRetryAfter = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestConfigureDefaultResetsState(t *testing.T) {
	t.Cleanup(func() { ConfigureDefault(RateLimiterConfig{}) })
	ConfigureDefault(RateLimiterConfig{BackoffEnabled: true, BaseBackoff: time.Minute, MaxBackoff: time.Minute})
	DefaultLimiter().NoteSecondaryLimit(0)
	if !DefaultLimiter().Snapshot().InBackoff {
		t.Fatalf("expected default limiter in backoff")
	}
	// Reconfiguring installs a fresh limiter, clearing the poisoned backoff window.
	ConfigureDefault(RateLimiterConfig{})
	if DefaultLimiter().Snapshot().InBackoff {
		t.Fatalf("reconfigure should reset live backoff state")
	}
}

func TestRateLimiterSlidingCallWindowWarnsOnThresholdCrossing(t *testing.T) {
	clock := newFakeClock()
	warnings := 0
	limiter := NewRateLimiter(RateLimiterConfig{
		CallsPerHourWarn: 2,
		Now:              clock.Now,
		Sleep:            clock.Sleep,
		Logf:             func(string, ...any) { warnings++ },
	})
	limiter.NoteCall()
	if got := limiter.Snapshot().CallsInLastHour; got != 1 || warnings != 0 {
		t.Fatalf("after one call count=%d warnings=%d", got, warnings)
	}
	limiter.NoteCall()
	limiter.NoteCall()
	if got := limiter.Snapshot().CallsInLastHour; got != 3 || warnings != 1 {
		t.Fatalf("above threshold count=%d warnings=%d, want 3/1", got, warnings)
	}
	if err := clock.Sleep(context.Background(), time.Hour+time.Second); err != nil {
		t.Fatal(err)
	}
	if got := limiter.Snapshot().CallsInLastHour; got != 0 {
		t.Fatalf("expired count=%d, want 0", got)
	}
	limiter.NoteCall()
	limiter.NoteCall()
	if warnings != 2 {
		t.Fatalf("warnings after re-crossing = %d, want 2", warnings)
	}
}
