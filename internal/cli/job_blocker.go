package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Issue #532 (first slice): classify OPERATIONAL blockers — failures caused by
// the environment the job ran in (runtime auth rejected, provider rate
// limit/quota), not by the agent's work — and defer the job for automatic
// re-dispatch instead of terminally failing it indistinguishably from a product
// failure.
//
// Scope discipline (all guards below are load-bearing):
//   - Only a job that ended JobFailed WITHOUT a stored gitmoot_result is
//     eligible: a stored result means the agent DID answer (a `failed` decision
//     is a product failure and must never be auto-retried).
//   - Delegation children (ParentJobID set) are excluded: their retry/failure
//     policy belongs to the delegation DAG (finalizeTimedOutDelegationChild →
//     advanceDelegations), which already owns retry semantics for children.
//   - Only the two cleanest classes are matched (runtime_auth, runtime_quota),
//     reusing the #552 classifyAuthQuota signatures (per line, with tightened
//     HTTP-status arms — see classifyAuthQuotaStrict) plus the adapters' typed
//     runtime.ErrClaudeAuthFailed sentinel, so detection stays grounded in
//     strings the adapters actually emit. String matching additionally requires
//     the typed workflow.DeliveryError marker, so only errors from the delivery
//     seam — never agent-authored contract/validation text — can classify.
//   - A run whose delivery COMPLETED (persisted RawOutputs) is never deferred,
//     and every deferred retry is at-least-once for side effects: Mailbox.Run
//     prepends a reconciliation notice to a blocker-retried prompt.
//   - Auto-retries are hard-bounded by maxOperationalBlockerRetries; when the
//     budget is exhausted the job stays terminally failed exactly like today.
//
// Every job that does not hit a classified blocker takes the existing path
// byte-identically: the mailbox's injected BlockerDeferrer
// (deferOperationalBlockerPreTerminal, #532 slice E) only diverts a run when it
// classifies AND every scope guard passes; otherwise Mailbox.Run fails the job
// exactly as before.

// blockerClass names a class of operational blocker. Persisted in the job
// payload (blocker_class) and rendered by the #552 stuck-reason surface, so the
// values are part of the observable CLI surface — do not rename casually.
type blockerClass string

const (
	blockerClassRuntimeAuth  blockerClass = "runtime_auth"
	blockerClassRuntimeQuota blockerClass = "runtime_quota"
	// blockerClassNetworkOutage (#532 slice D) classifies a transient network /
	// GitHub outage — a gh-CLI transport/DNS/TLS/5xx failure grounded in
	// internal/github's TransientError / IsTransientMessage signatures — that
	// reached a terminal job failure. It defers with a short backoff.
	blockerClassNetworkOutage blockerClass = "network_outage"
	// blockerClassCheckoutContention (#532 slice C) classifies a daemon-owned
	// pre-flight checkout failure: a branch-lock conflict (self-heals, short
	// exponential backoff) or a dirty/wrong-head checkout (usually needs a human;
	// defers with a suggested_action surfaced through the #552 stuck surface).
	blockerClassCheckoutContention blockerClass = "checkout_contention"
	// blockerClassRuntimeUnavailable (#1821, #1823) classifies a CAPABILITY
	// refusal: the runtime the job needs could not be executed at all, because
	// its binary is absent from the seat's PATH, or resolves to a published
	// unavailable shim that exits 126, or the sandbox could not resolve the
	// target.
	//
	// It is the ONLY class that does not defer, because it is the only one whose
	// condition does not clear on its own. The other four wait out a provider
	// window, an outage or a lock; a missing executable waits for an operator.
	// Re-dispatching it walks into the same wall: measured over this store's
	// history, 29 refusal deaths, ZERO recorded blocked, and 25 of the 29 belong
	// to an agent hit more than once - gm-review-opus eleven times.
	//
	// So it terminates the job BLOCKED. Nothing was wrong with the job, which is
	// why `failed` was the wrong state: `failed` invites the identical
	// re-dispatch, and eleven of them happened. This is the same judgement as
	// #1817's dispatch-time refusal (e16875b3), applied at the delivery seam
	// that #1817 cannot see - a published 126 shim RESOLVES on PATH, so an
	// absent-binary predicate at dispatch is silent about it.
	blockerClassRuntimeUnavailable blockerClass = "runtime_unavailable"
)

// blockerDeferredEventKind is the job_event kind recorded when a failed job is
// re-queued behind an operational blocker. It is a #552 stuck-reason event kind
// (see stuckReasonEventKinds) so `job list`/`job show` explain the held job.
const blockerDeferredEventKind = "blocker_deferred"

// blockerExhaustedEventKind is recorded when a classified blocker recurs after
// the auto-retry budget is spent; the job stays terminally failed and this event
// documents why no further auto-retry happened.
const blockerExhaustedEventKind = "blocker_retries_exhausted"

// blockerBlockedEventKind is recorded when a capability refusal terminates a job
// BLOCKED instead of failed. A #552 stuck-reason kind, so `job list`/`job show`
// explain a job an operator has to unblock rather than retry.
const blockerBlockedEventKind = "blocker_runtime_unavailable"

// runtimeUnavailableSignatures are the renderings a capability refusal actually
// produces on this host. Measured, not guessed - every one of them appears in
// this store's job_events:
//
//   - "exit status 126" is POSIX "found but not executable", and is what a
//     published unavailable shim returns by design (#1974).
//   - "executable file not found in $PATH" is exec.LookPath's wording, which
//     killed two #1910 review legs.
//   - "resolve sandbox target" is the sandbox wrapper's own failure when the
//     target binary cannot be resolved inside the seat.
//   - "runtime unavailable" is the daemon's staging refusal.
//
// The last three close a gap found in review of this change (#2022 P2): a
// refusal that happens AFTER execLookPath already resolved the target is not
// an unresolved-target failure and matched none of the first four. The sandbox
// ends in a bare `syscall.Exec` (internal/sandbox/exec_linux.go), which returns
// an UNWRAPPED errno, so these are the renderings that reach the delivery seam:
//
//   - "exec format error" is ENOEXEC: the target resolved and is not
//     executable - a truncated or corrupt shim, or a text file where a binary
//     was expected.
//   - "apply strict landlock ruleset" and "landlock abi" are the sandbox's own
//     wrapped refusals to start at all. Deliberately strict with no BestEffort
//     downgrade, so they are capability facts rather than warnings.
//
// TWO RENDERINGS ARE DELIBERATELY REFUSED, and the reason is the same one that
// excludes a bare "126": "permission denied" (EACCES) and "no such file or
// directory" (ENOENT) also reach this seam from a genuine capability refusal,
// and they ALSO reach it from an agent's own file operations, a denied cache
// directory, a missing repository path. This predicate decides a TERMINAL
// state, so a signature that is right about the cause only some of the time is
// worse than a miss: a miss keeps today's behaviour, a false positive blocks
// work that would have succeeded on retry. Those two cases stay UNCOVERED and
// TestRuntimeUnavailableRefusesAmbiguousErrnoText pins that, so a later reader
// closing "the rest of the gap" has to argue with a test rather than a comment.
//
// A bare "126" is deliberately NOT a signature: it appears in SHAs, job ids and
// token counts, and measured store-wide a loose "%126%" match returns 234
// events against 13 for "exit status 126", an 18x inflation.
var runtimeUnavailableSignatures = []string{
	"exit status 126",
	"executable file not found in $path",
	"resolve sandbox target",
	"runtime unavailable",
	"exec format error",
	"apply strict landlock ruleset",
	"landlock abi",
}

// isRuntimeUnavailableMessage reports whether text names a capability refusal.
// Case-folded once by the caller.
func isRuntimeUnavailableMessage(text string) bool {
	for _, sig := range runtimeUnavailableSignatures {
		if strings.Contains(text, sig) {
			return true
		}
	}
	return false
}

// maxOperationalBlockerRetries hard-bounds automatic re-dispatches per job. It
// counts classified deferrals over the job's lifetime (persisted in the payload
// as blocker_attempts), so a flapping blocker can never retry a job forever.
const maxOperationalBlockerRetries = 3

// authBlockerRetryDelay is the earliest-retry delay for a runtime auth failure.
// Token rotation/re-login is a human action with no machine-readable ETA, so the
// deferral simply re-probes on a coarse cadence (bounded by the retry budget)
// instead of hammering the provider. A future slice can tighten this to
// "next tick after a doctor-style probe passes" (#532 design comment).
const authBlockerRetryDelay = 5 * time.Minute

// quotaBlockerFallbackDelay is used when a rate-limit/quota error carries no
// parseable reset time.
const quotaBlockerFallbackDelay = 15 * time.Minute

// quotaBlockerMaxParsedDelay caps a parsed reset delay so a garbled "try again
// in N hours" can never park a job indefinitely. Eight days covers a provider's
// real weekly quota window while remaining bounded.
const quotaBlockerMaxParsedDelay = 8 * 24 * time.Hour

// quotaBlockerMinParsedDelay floors a parsed reset delay so a sub-second
// provider hint ("try again in 1.898s") plus jitter can never re-dispatch
// inside a still-closed window and burn the retry budget in seconds.
const quotaBlockerMinParsedDelay = 5 * time.Second

// networkBlockerRetryDelay is the short base backoff for a transient network /
// GitHub outage (#532 slice D). Outages clear on their own quickly, so the hold
// is short (plus jitter, bounded by the shared 3-attempt budget) rather than the
// minutes-long auth/quota holds.
const networkBlockerRetryDelay = 30 * time.Second

// blockerClassification is the classifier verdict for one failed run.
type blockerClassification struct {
	Class   blockerClass
	RetryAt time.Time // earliest safe automatic re-dispatch (UTC)
	Detail  string    // first line of the causing error, for events/UX
	// QuotaResetAt is the provider-declared reset instant, or the bounded
	// fallback instant when no reset could be parsed. RetryAt may include fleet
	// jitter; role unavailability must use this exact, non-jittered boundary.
	QuotaResetAt     time.Time
	QuotaResetParsed bool
	// QuotaResetMentioned distinguishes a malformed/unknown reset hint from a
	// quota error that supplied no reset hint at all. Both use the bounded
	// fallback, but diagnostics should not conflate them.
	QuotaResetMentioned bool
	// SuggestedAction, when set, is a concrete human-facing remedy surfaced through
	// the #552 stuck surface (job list/show) and persisted in the payload. Only the
	// checkout dirty/wrong-head sub-class sets it today (#532 slice C); auth/quota/
	// network defer on a condition that clears on its own and leave it empty.
	SuggestedAction string
}

// classifyOperationalBlocker inspects a RunJob error and reports whether it is a
// classifiable operational blocker. Detection composes with #552: the string
// signatures are the classifyAuthQuota matcher `job list` already uses to label
// stuck jobs — applied per line and with tightened HTTP-status arms (see
// classifyAuthQuotaStrict) because HERE a match triggers automatic re-runs, not
// a cosmetic label — plus the typed runtime.ErrClaudeAuthFailed sentinel the
// Claude adapter attaches to genuine credential rejections. Engine-routed
// outcomes (awaiting-human, blocked) and cancellations are never classified.
//
// String matching only runs for an error that provably originated from the
// DELIVERY seam (workflow.DeliveryError): a gitmoot_result contract/validation
// failure — including the post-repair-loop parse error, whose text is
// agent-authored and can mention "quota"/"rate limit" in a summary or a
// delegation id — is a PRODUCT failure and must never be auto-retried (#532
// design: "missing gitmoot_result contract output ... treat as
// product/contract, not operational").
func classifyOperationalBlocker(cause error, now time.Time) (blockerClassification, bool) {
	if cause == nil {
		return blockerClassification{}, false
	}
	var awaiting workflow.AwaitingHumanError
	var blocked workflow.BlockedError
	if errors.As(cause, &awaiting) || errors.As(cause, &blocked) ||
		errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return blockerClassification{}, false
	}
	text := cause.Error()
	detail := firstLineTrimmed(text)
	if errors.Is(cause, runtime.ErrClaudeAuthFailed) {
		// Typed sentinel: attached by the Claude adapter to a classified genuine
		// credential rejection, so it is trustworthy without the delivery gate.
		return blockerClassification{Class: blockerClassRuntimeAuth, RetryAt: now.Add(authBlockerRetryDelay), Detail: detail}, true
	}
	if github.AsTransient(cause) {
		// A typed github.TransientError can reach a TERMINAL job failure directly
		// from a daemon-owned github op (not the runtime delivery seam), so it is
		// trustworthy without the DeliveryError marker — the network signatures live
		// in internal/github and a 4xx/permission/conflict is never tagged transient.
		return blockerClassification{Class: blockerClassNetworkOutage, RetryAt: now.Add(networkBlockerRetryDelay + blockerRetryJitter(networkBlockerRetryDelay)), Detail: detail}, true
	}
	var delivery workflow.DeliveryError
	if !errors.As(cause, &delivery) {
		return blockerClassification{}, false
	}
	switch classifyAuthQuotaStrict(text) {
	case "throttled":
		resetAt, parsed := parseQuotaResetAt(text, now)
		delay := resetAt.Sub(now)
		return blockerClassification{
			Class: blockerClassRuntimeQuota, RetryAt: resetAt.Add(blockerRetryJitter(delay)), Detail: detail,
			QuotaResetAt: resetAt, QuotaResetParsed: parsed, QuotaResetMentioned: quotaResetHintMentioned(text),
		}, true
	case "auth failing":
		return blockerClassification{Class: blockerClassRuntimeAuth, RetryAt: now.Add(authBlockerRetryDelay), Detail: detail}, true
	}
	// CAPABILITY refusal (#1821): the runtime could not be executed at all.
	// Checked AFTER auth/quota so a 401/429 keeps its more specific class, and
	// BEFORE the network arm because neither predicate should be able to claim
	// the other's text.
	//
	// RetryAt is deliberately ZERO. Every other class returns an instant to wait
	// until; this one has nothing to wait for, and the zero value is what
	// deferOperationalBlockerPreTerminal reads to route it to a terminal block
	// instead of a re-queue.
	if isRuntimeUnavailableMessage(strings.ToLower(text)) {
		return blockerClassification{
			Class:  blockerClassRuntimeUnavailable,
			Detail: detail,
			SuggestedAction: "the runtime this job needs is not executable in the seat: install or re-stage it, " +
				"or dispatch to an agent on a runtime that is. Retrying this job unchanged will hit the same wall.",
		}, true
	}
	// Network / GitHub outage that surfaced through the delivery seam: the agent's
	// own `gh` subprocess printed a transport/DNS/5xx signature to stderr (which
	// becomes DeliveryError text, never a TransientError value). Reuse the SAME
	// internal/github signature set so both the typed and the delivery paths agree.
	// Checked AFTER auth/quota so a 401/429 keeps its more specific class.
	if github.IsTransientMessage(text) {
		return blockerClassification{Class: blockerClassNetworkOutage, RetryAt: now.Add(networkBlockerRetryDelay + blockerRetryJitter(networkBlockerRetryDelay)), Detail: detail}, true
	}
	return blockerClassification{}, false
}

// code429Re / code401Re match an HTTP status code as a standalone token, so a
// bare digit run embedded in a hex job id ("local-ask-18be4290fad9"), a PR
// number ("#4291"), or a SHA never satisfies the status-code arm.
var (
	code429Re = regexp.MustCompile(`\b429\b`)
	code401Re = regexp.MustCompile(`\b401\b`)
)

// classifyAuthQuotaStrict is the #552 classifyAuthQuota matcher with the extra
// precision the BEHAVIORAL #532 call site needs. Adapter failures concatenate a
// whole stderr dump plus commandError's trailing exec suffix ("...: exit status
// N"), so a whole-text scan lets "status" from the exec suffix combine with an
// unrelated "429" digit run on a DIFFERENT line into a false runtime_quota.
// Here the matcher runs per line, requires the 401/429 token (word-boundary)
// to be co-located with genuine HTTP context on the SAME line, and "HTTP
// context" means http/status code/retry-after — NOT the bare "status" that
// every "exit status N" exec error carries. The word signatures (usage/rate
// limit, quota, authentication, unauthorized, auth+invalid) are extended with
// Claude's provider wording ("weekly limit", and durable "hit your ... limit"
// variants). The first matching line wins.
func classifyAuthQuotaStrict(text string) string {
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		if l == "" {
			continue
		}
		httpCtx := strings.Contains(l, "http") || strings.Contains(l, "status code") || strings.Contains(l, "retry-after")
		switch {
		case strings.Contains(l, "usage limit"), strings.Contains(l, "rate limit"),
			strings.Contains(l, "weekly limit"),
			claudeHitYourQuotaLimit(l),
			strings.Contains(l, "quota"), strings.Contains(l, "limit resets"),
			httpCtx && code429Re.MatchString(l):
			return "throttled"
		case strings.Contains(l, "authentication"), strings.Contains(l, "unauthorized"),
			httpCtx && code401Re.MatchString(l),
			authWordRe.MatchString(l) && strings.Contains(l, "invalid"):
			return "auth failing"
		}
	}
	return ""
}

// claudeQuotaSignalRe matches a genuine quota/usage word (allowing a suffix,
// e.g. "weekly", "credits", "resets") at a word boundary, so an unrelated
// message can't false-match by embedding the signal mid-word (e.g. "rate"
// inside "generate").
var claudeQuotaSignalRe = regexp.MustCompile(`\b(week|usage|quota|credit|reset|rate)`)

func claudeHitYourQuotaLimit(line string) bool {
	if !strings.Contains(line, "hit your") || !strings.Contains(line, "limit") {
		return false
	}
	return claudeQuotaSignalRe.MatchString(line)
}

// quotaResetInRe matches relative reset hints the providers actually emit, e.g.
// codex's "Please try again in 32 seconds", OpenAI's decimal "try again in
// 1.898s", abbreviated "retry in 5 min", and HTTP "Retry-After: 120" (bare
// integers are seconds). Each unit alternation ends on \b so an unknown unit
// WORD ("5 mint") never half-matches; the number/unit separator is [ \t]* (not
// \s*) so the unit is only ever taken from the SAME line as the number.
var quotaResetInRe = regexp.MustCompile(`(?i)(?:try again in|retry after|retry in|retry-after:?)\s*(\d+(?:\.\d+)?)[ \t]*(seconds?\b|secs?\b|s\b|minutes?\b|mins?\b|m\b|hours?\b|hrs?\b|h\b)?`)

// quotaResetEpochRe matches the Claude CLI usage-limit shape
// "Claude AI usage limit reached|<unix-epoch>".
var quotaResetEpochRe = regexp.MustCompile(`\|(\d{10})\b`)

// quotaResetAbsoluteRe matches Claude's current natural-language weekly reset
// hint, including small punctuation variations:
//
//	"resets Jul 28, 1am (Europe/Berlin)"
//	"resets on July 28 at 1:00 AM (Europe/Berlin)"
//
// The named zone is intentionally required. Guessing a timezone from a local
// abbreviation would make the durable refusal window unsafe.
var quotaResetAbsoluteRe = regexp.MustCompile(`(?i)\bresets?\s+(?:on\s+)?([a-z]{3,9})\s+(\d{1,2})(?:st|nd|rd|th)?(?:,\s*|\s+)(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([^)]+)\)`)

// parseQuotaResetAt extracts the provider-declared reset instant. Its bool says
// whether a provider hint was successfully parsed; false returns the existing
// bounded 15-minute fallback. Relative, epoch, and absolute values share the
// same min/max clamp.
func parseQuotaResetAt(text string, now time.Time) (time.Time, bool) {
	now = now.UTC()
	if idx := quotaResetInRe.FindStringSubmatchIndex(text); idx != nil {
		unit := ""
		if idx[4] >= 0 {
			unit = text[idx[4]:idx[5]]
		}
		if unit != "" || !startsWithLetter(text[idx[1]:]) {
			n, err := strconv.ParseFloat(text[idx[2]:idx[3]], 64)
			if err == nil && n > 0 {
				scale := time.Second
				switch {
				case strings.HasPrefix(strings.ToLower(unit), "m"):
					scale = time.Minute
				case strings.HasPrefix(strings.ToLower(unit), "h"):
					scale = time.Hour
				}
				return now.Add(clampQuotaDelay(time.Duration(n * float64(scale)))), true
			}
		}
	}
	if m := quotaResetEpochRe.FindStringSubmatch(text); m != nil {
		epoch, err := strconv.ParseInt(m[1], 10, 64)
		if err == nil {
			if delay := time.Unix(epoch, 0).Sub(now); delay > 0 {
				return now.Add(clampQuotaDelay(delay)), true
			}
		}
	}
	if m := quotaResetAbsoluteRe.FindStringSubmatch(text); m != nil {
		month, ok := parseQuotaResetMonth(m[1])
		day, dayErr := strconv.Atoi(m[2])
		hour, hourErr := strconv.Atoi(m[3])
		minute := 0
		var minuteErr error
		if m[4] != "" {
			minute, minuteErr = strconv.Atoi(m[4])
		}
		zoneName := strings.TrimSpace(m[6])
		location, zoneErr := time.LoadLocation(zoneName)
		if ok && dayErr == nil && hourErr == nil && minuteErr == nil && zoneErr == nil &&
			day >= 1 && day <= 31 && hour >= 1 && hour <= 12 && minute >= 0 && minute <= 59 {
			if strings.EqualFold(m[5], "am") {
				if hour == 12 {
					hour = 0
				}
			} else if hour != 12 {
				hour += 12
			}
			localNow := now.In(location)
			candidate := time.Date(localNow.Year(), month, day, hour, minute, 0, 0, location)
			// Reject normalized impossible dates such as Feb 31.
			if candidate.Month() == month && candidate.Day() == day {
				if !candidate.After(localNow) {
					// Claude omits the year. Only roll forward across the one
					// credible boundary: a December observation naming January.
					// A same-day reset a few minutes in the past is more likely
					// provider/host skew or delayed delivery; parking the role for
					// a clamped eight days would be actively unsafe.
					if localNow.Month() != time.December || month != time.January {
						return now.Add(quotaBlockerFallbackDelay), false
					}
					candidate = time.Date(localNow.Year()+1, month, day, hour, minute, 0, 0, location)
				}
				if delay := candidate.Sub(now); delay > 0 {
					if clamped := clampQuotaDelay(delay); clamped != delay {
						return now.Add(clamped), true
					}
					return candidate, true
				}
			}
		}
	}
	return now.Add(quotaBlockerFallbackDelay), false
}

func parseQuotaResetMonth(value string) (time.Month, bool) {
	for month := time.January; month <= time.December; month++ {
		long := strings.ToLower(month.String())
		value = strings.ToLower(strings.TrimSpace(value))
		if value == long || (len(value) == 3 && value == long[:3]) {
			return month, true
		}
	}
	return 0, false
}

func quotaResetHintMentioned(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "reset") || strings.Contains(text, "try again") || strings.Contains(text, "retry")
}

// startsWithLetter reports whether rest — the text immediately after a matched
// bare number — begins (after spaces/tabs) with a letter, i.e. the number
// carried a unit word quotaResetInRe does not recognize, so the bare-integer-
// seconds default would mis-schedule the retry and the caller must treat the
// message as unparseable.
func startsWithLetter(rest string) bool {
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsLetter(r)
}

func clampQuotaDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return quotaBlockerFallbackDelay
	}
	// Floor, not fallback: a genuinely short provider reset ("try again in
	// 1.898s") is honored, but never so tight that clock skew/jitter re-dispatches
	// inside a still-closed window.
	if d < quotaBlockerMinParsedDelay {
		return quotaBlockerMinParsedDelay
	}
	if d > quotaBlockerMaxParsedDelay {
		return quotaBlockerMaxParsedDelay
	}
	return d
}

// blockerRetryJitter returns a small random smear (up to 10% of the delay,
// capped at 30s) added to a quota reset so a fleet of deferred jobs does not
// re-dispatch in the same instant the window opens.
func blockerRetryJitter(delay time.Duration) time.Duration {
	max := delay / 10
	if max > 30*time.Second {
		max = 30 * time.Second
	}
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max)))
}

// deferOperationalBlockerPreTerminal re-queues a RUNNING job whose delivery just
// failed on a classifiable operational blocker, preserving its resumable context.
// It is the injected workflow.Engine.BlockerDeferrer, called by Mailbox.Run at the
// delivery-seam failure point BEFORE the terminal transition (#532 slice E): it
// reads the (still running) job back, and only when every scope guard passes does
// it write the blocker fields into the payload and flip running→queued with a
// blocker_deferred event + additive job.deferred emit. It returns (true, nil)
// exactly when the job was deferred, so Mailbox.Run skips m.fail and NO job.failed
// is emitted first (slice A deferred failed→queued AFTER the terminal transition —
// the flap this slice removes). Every other outcome returns false so Run takes its
// existing terminal path unchanged.
//
// SIDE-EFFECT SEMANTICS: the auto-retry is AT-LEAST-ONCE. A run whose FIRST
// delivery completed (persisted RawOutputs) is never deferred — completed
// delivery proves side-effectful execution, so the repair-loop failure of an
// already-executed job keeps today's terminal path. A blocker that hits
// MID-first-turn can still have executed partial work (pushed a branch, opened
// a PR) before the provider cut it off; that retry cannot be made exactly-once
// from here, so Mailbox.Run prepends a reconciliation notice to every
// blocker-retried prompt (payload.BlockerAttempts > 0) telling the agent to
// verify and reuse prior artifacts instead of duplicating them.
func (w jobWorker) deferOperationalBlockerPreTerminal(ctx context.Context, jobID string, cause error) (bool, error) {
	latest, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	// The pre-terminal seam runs while the job is still RUNNING (Mailbox.Run claimed
	// it queued→running before delivering); anything else means the run already
	// resolved and this seam does not apply.
	if latest.State != string(workflow.JobRunning) {
		return false, nil
	}
	payload, err := daemonJobPayload(latest)
	if err != nil {
		return false, nil
	}
	classification, ok := classifyOperationalBlocker(cause, time.Now().UTC())
	if !ok {
		return false, nil
	}
	// CAPABILITY REFUSAL (#1821): terminate BLOCKED instead of re-queueing.
	//
	// Placed BEFORE the retry guards below on purpose, and the difference is not
	// cosmetic. Those guards exist to protect an at-least-once RE-RUN: a stored
	// result, a delegation child owned by the DAG's retry policy, persisted raw
	// outputs proving side effects. This path re-runs NOTHING, so none of those
	// hazards apply to it.
	//
	// A delegation child in particular MUST reach this branch. A child that
	// cannot execute its runtime is precisely the staged-review failure #1823
	// names, and a blocked child lands in the merge gate's parked arm, where
	// ensureDelegatedReviewEvidence already refuses the parent. That is the
	// non-fallback clause - strong reviewer unavailable must never degrade to
	// cheap reviewer approved - enforced by the row's STATE rather than by a
	// rule anyone has to remember.
	//
	// The one guard that does apply is a stored Result: if the agent answered,
	// this is a product outcome and the runtime plainly executed, whatever the
	// error text says.
	if classification.Class == blockerClassRuntimeUnavailable {
		if payload.Result != nil {
			return false, nil
		}
		return w.blockOnRuntimeUnavailable(ctx, jobID, payload, classification)
	}
	// A stored result means the agent answered: decision=failed is a PRODUCT
	// failure and is never auto-retried. Delegation children keep the DAG's own
	// retry/failure-policy path (#409 machinery), untouched by this slice.
	if payload.Result != nil || strings.TrimSpace(payload.ParentJobID) != "" {
		return false, nil
	}
	// Duplicate-side-effect gate: persisted raw outputs prove a delivery COMPLETED
	// a full agent turn (the #495 repair loop persists them before re-asking), so
	// this failure came after side-effectful execution — re-running the full
	// prompt could push duplicate branches / open duplicate PRs. Keep it terminal.
	if len(payload.RawOutputs) > 0 {
		return false, nil
	}
	attempt := payload.BlockerAttempts + 1
	if attempt > maxOperationalBlockerRetries {
		_ = w.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: jobID,
			Kind:  blockerExhaustedEventKind,
			Message: fmt.Sprintf("operational blocker %s recurred after %d auto-retries; job stays failed: %s",
				classification.Class, maxOperationalBlockerRetries, classification.Detail),
		})
		return false, nil
	}
	retryAt := classification.RetryAt.Format(time.RFC3339Nano)
	payload.BlockerClass = string(classification.Class)
	payload.BlockerAttempts = attempt
	payload.BlockerRetryAt = retryAt
	payload.BlockerSuggestedAction = classification.SuggestedAction
	// MID-DELIVERY (#532): the agent WAS delivered a prompt and may have executed side
	// effects before the blocker cut it off, so this retry is at-least-once — clear any
	// pre-delivery marker a prior checkout_contention hold left so Mailbox.Run still
	// prepends the slice-F reconciliation notice.
	payload.BlockerPreDelivery = false
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	// Payload first, transition second: a crash in between leaves the job RUNNING
	// with extra context in the payload — the stale-running recovery requeues it —
	// never a queued job missing its hold timestamp.
	if err := w.Store.UpdateJobPayload(ctx, jobID, string(encoded)); err != nil {
		return false, err
	}
	message := fmt.Sprintf("%s: attempt %d/%d, retry at %s: %s",
		classification.Class, attempt, maxOperationalBlockerRetries, retryAt, classification.Detail)
	transitioned, err := w.Store.TransitionJobStateWithEvent(ctx, jobID, string(workflow.JobRunning), string(workflow.JobQueued), db.JobEvent{
		JobID:   jobID,
		Kind:    blockerDeferredEventKind,
		Message: message,
	})
	if err != nil {
		return false, err
	}
	if transitioned {
		// PRE-TERMINAL (#532 slice E): m.fail was never called, so NO job.failed was
		// emitted for this run. Emit the additive job.deferred as the FIRST-CLASS
		// terminal-set transition for this run — the [events] stream sees the deferral
		// directly, with no preceding failed→deferred flap. Best-effort and nil-safe
		// when [events] is OFF, mirroring the daemon's other emits.
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, jobID, daemonTerminalDeferred, string(workflow.JobQueued), message)
	}
	return transitioned, nil
}

// blockOnRuntimeUnavailable terminates a RUNNING job BLOCKED because the runtime
// it needs could not be executed. It is the capability-refusal arm of
// deferOperationalBlockerPreTerminal and returns the same (tookOwnership, err)
// contract, so Mailbox.Run skips m.fail and no job.failed is emitted first.
//
// It writes the payload BEFORE the transition, in one call, for the reason the
// deferral path documents: a crash between the two would otherwise leave a
// blocked job with no recorded reason, which is the silent row this whole slice
// exists to abolish.
func (w jobWorker) blockOnRuntimeUnavailable(ctx context.Context, jobID string, payload workflow.JobPayload, classification blockerClassification) (bool, error) {
	payload.BlockerClass = string(classification.Class)
	payload.BlockerSuggestedAction = classification.SuggestedAction
	// No RetryAt and no attempt increment: the OPERATIONAL-BLOCKER actuator will
	// not re-dispatch this job, and a recorded retry instant would be a promise
	// this layer does not keep.
	//
	// THAT CLAIM IS SCOPED TO THIS LAYER, and review of this change (#2022)
	// established the boundary rather than letting the comment overstate it: for
	// a DELEGATION CHILD, requeueDelegation gates purely on the delegation
	// edge's own d.Retry against the child's RetryCount
	// (internal/workflow/engine_delegation.go) with zero awareness of blocker
	// class, and JobBlocked has been a settled state since #632. So a blocked
	// child on an edge declaring Retry > 0 CAN be re-dispatched by the DAG, into
	// the same wall, up to that edge's budget.
	//
	// Not fixed here, deliberately: teaching requeueDelegation about blocker
	// class is a change to the delegation engine, not to this classifier, and it
	// would be the wrong thing to smuggle into a slice whose subject is the
	// terminal state. What this layer guarantees is that the job is BLOCKED with
	// a recorded cause and a remedy, which is what makes the DAG's retry visible
	// as a repeat rather than a first attempt.
	payload.BlockerRetryAt = ""
	payload.BlockerPreDelivery = false
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	message := fmt.Sprintf("%s: %s (no automatic retry; the condition does not clear on its own): %s",
		classification.Class, classification.SuggestedAction, classification.Detail)
	transitioned, err := w.Store.TransitionJobStatePayloadWithEvent(ctx, jobID,
		string(workflow.JobRunning), string(workflow.JobBlocked), string(encoded), db.JobEvent{
			JobID:   jobID,
			Kind:    blockerBlockedEventKind,
			Message: message,
		})
	if err != nil {
		return false, err
	}
	if transitioned {
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, jobID, daemonTerminalBlocked, string(workflow.JobBlocked), message)
	}
	return transitioned, nil
}

// restorePreIsolationPayloadForDeferredJob handles the pool-isolation ×
// blocker-deferral interaction: an isolation-dispatched job (#394) that
// DEFERRED on an operational blocker is queued again, but
// allocatePoolIsolationWorktree rewrote its payload to point at the isolation
// worktree the pool reap removes on completion. Restore the pre-isolation
// payload while carrying the blocker fields over, so the held job re-evaluates
// (and can be re-isolated) cleanly on re-dispatch. Best-effort and strictly
// scoped: any job that is not queued with a live blocker hold is left untouched.
func restorePreIsolationPayloadForDeferredJob(ctx context.Context, store *db.Store, jobID string, payloadBeforeIsolation string) {
	job, err := store.GetJob(ctx, jobID)
	if err != nil || job.State != string(workflow.JobQueued) {
		return
	}
	current, err := daemonJobPayload(job)
	if err != nil || strings.TrimSpace(current.BlockerRetryAt) == "" {
		return
	}
	var restored workflow.JobPayload
	if err := json.Unmarshal([]byte(payloadBeforeIsolation), &restored); err != nil {
		return
	}
	restored.BlockerClass = current.BlockerClass
	restored.BlockerAttempts = current.BlockerAttempts
	restored.BlockerRetryAt = current.BlockerRetryAt
	restored.BlockerSuggestedAction = current.BlockerSuggestedAction
	restored.BlockerPreDelivery = current.BlockerPreDelivery
	encoded, err := json.Marshal(restored)
	if err != nil {
		return
	}
	_ = store.UpdateJobPayload(ctx, jobID, string(encoded))
}

// queuedJobBlockerHeld reports whether a queued job is still inside its
// operational-blocker hold window (payload.blocker_retry_at in the future).
// Jobs without the field — every job that never hit a classified blocker —
// return false on the cheap empty-string check, and a malformed timestamp also
// returns false so a bad write can strand nothing.
func queuedJobBlockerHeld(job db.Job, now time.Time) bool {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return false
	}
	raw := strings.TrimSpace(payload.BlockerRetryAt)
	if raw == "" {
		return false
	}
	retryAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false
	}
	return now.Before(retryAt)
}
