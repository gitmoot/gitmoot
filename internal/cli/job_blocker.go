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

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
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
//     reusing the #552 classifyAuthQuota signatures plus the adapters' typed
//     runtime.ErrClaudeAuthFailed sentinel, so detection stays grounded in
//     strings the adapters actually emit.
//   - Auto-retries are hard-bounded by maxOperationalBlockerRetries; when the
//     budget is exhausted the job stays terminally failed exactly like today.
//
// Every job that does not hit a classified blocker takes the existing path
// byte-identically: the hook in jobWorker.run only diverts when
// deferOperationalBlocker returns true.

// blockerClass names a class of operational blocker. Persisted in the job
// payload (blocker_class) and rendered by the #552 stuck-reason surface, so the
// values are part of the observable CLI surface — do not rename casually.
type blockerClass string

const (
	blockerClassRuntimeAuth  blockerClass = "runtime_auth"
	blockerClassRuntimeQuota blockerClass = "runtime_quota"
)

// blockerDeferredEventKind is the job_event kind recorded when a failed job is
// re-queued behind an operational blocker. It is a #552 stuck-reason event kind
// (see stuckReasonEventKinds) so `job list`/`job show` explain the held job.
const blockerDeferredEventKind = "blocker_deferred"

// blockerExhaustedEventKind is recorded when a classified blocker recurs after
// the auto-retry budget is spent; the job stays terminally failed and this event
// documents why no further auto-retry happened.
const blockerExhaustedEventKind = "blocker_retries_exhausted"

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
// in N hours" can never park a job for days.
const quotaBlockerMaxParsedDelay = 24 * time.Hour

// blockerClassification is the classifier verdict for one failed run.
type blockerClassification struct {
	Class   blockerClass
	RetryAt time.Time // earliest safe automatic re-dispatch (UTC)
	Detail  string    // first line of the causing error, for events/UX
}

// classifyOperationalBlocker inspects a RunJob error and reports whether it is a
// classifiable operational blocker. Detection composes with #552: the string
// signatures are the SAME classifyAuthQuota matcher `job list` already uses to
// label stuck jobs, plus the typed runtime.ErrClaudeAuthFailed sentinel the
// Claude adapter attaches to genuine credential rejections. Engine-routed
// outcomes (awaiting-human, blocked) and cancellations are never classified.
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
		return blockerClassification{Class: blockerClassRuntimeAuth, RetryAt: now.Add(authBlockerRetryDelay), Detail: detail}, true
	}
	switch classifyAuthQuota(text) {
	case "throttled":
		delay := parseQuotaResetDelay(text)
		return blockerClassification{Class: blockerClassRuntimeQuota, RetryAt: now.Add(delay + blockerRetryJitter(delay)), Detail: detail}, true
	case "auth failing":
		return blockerClassification{Class: blockerClassRuntimeAuth, RetryAt: now.Add(authBlockerRetryDelay), Detail: detail}, true
	}
	return blockerClassification{}, false
}

// quotaResetInRe matches relative reset hints the runtimes actually emit, e.g.
// codex's "Please try again in 32 seconds", generic "retry after 60 s(econds)",
// and HTTP "Retry-After: 120" (bare integers are seconds).
var quotaResetInRe = regexp.MustCompile(`(?i)(?:try again in|retry after|retry in|retry-after:?)\s*(\d+)\s*(seconds?|minutes?|hours?|s\b|m\b|h\b)?`)

// quotaResetEpochRe matches the Claude CLI usage-limit shape
// "Claude AI usage limit reached|<unix-epoch>".
var quotaResetEpochRe = regexp.MustCompile(`\|(\d{10})\b`)

// parseQuotaResetDelay extracts the provider-declared reset delay from a
// rate-limit/quota error. Unparseable messages (e.g. codex's "try again at Jun
// 14th") fall back to quotaBlockerFallbackDelay; parsed values are clamped to
// (0, quotaBlockerMaxParsedDelay].
func parseQuotaResetDelay(text string) time.Duration {
	if m := quotaResetInRe.FindStringSubmatch(text); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 {
			unit := time.Second
			switch {
			case strings.HasPrefix(strings.ToLower(m[2]), "m"):
				unit = time.Minute
			case strings.HasPrefix(strings.ToLower(m[2]), "h"):
				unit = time.Hour
			}
			return clampQuotaDelay(time.Duration(n) * unit)
		}
	}
	if m := quotaResetEpochRe.FindStringSubmatch(text); m != nil {
		epoch, err := strconv.ParseInt(m[1], 10, 64)
		if err == nil {
			if until := time.Until(time.Unix(epoch, 0)); until > 0 {
				return clampQuotaDelay(until)
			}
		}
	}
	return quotaBlockerFallbackDelay
}

func clampQuotaDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return quotaBlockerFallbackDelay
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

// deferOperationalBlocker re-queues a job that just terminally failed on a
// classifiable operational blocker, preserving its resumable context. Called by
// jobWorker.run strictly AFTER RunJob returned an error, i.e. off the hot path:
// it reads the (already failed) job back, and only when every scope guard passes
// does it write the blocker fields into the payload and flip failed→queued with
// a blocker_deferred event. It returns (true, nil) exactly when the job was
// deferred, so the caller skips the terminal failure comment/log; every other
// outcome leaves the job exactly as the existing path put it.
func (w jobWorker) deferOperationalBlocker(ctx context.Context, jobID string, cause error) (bool, error) {
	classification, ok := classifyOperationalBlocker(cause, time.Now().UTC())
	if !ok {
		return false, nil
	}
	latest, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	// Only a run that Mailbox.fail closed as JobFailed is eligible; a blocked/
	// cancelled/queued job belongs to other machinery (permission block, kill,
	// pre-flight) that already owns its semantics.
	if latest.State != string(workflow.JobFailed) {
		return false, nil
	}
	payload, err := daemonJobPayload(latest)
	if err != nil {
		return false, nil
	}
	// A stored result means the agent answered: decision=failed is a PRODUCT
	// failure and is never auto-retried. Delegation children keep the DAG's own
	// retry/failure-policy path (#409 machinery), untouched by this slice.
	if payload.Result != nil || strings.TrimSpace(payload.ParentJobID) != "" {
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
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	// Payload first, transition second: a crash in between leaves the job
	// terminally failed (today's behavior) with extra context in the payload —
	// never a queued job missing its hold timestamp.
	if err := w.Store.UpdateJobPayload(ctx, jobID, string(encoded)); err != nil {
		return false, err
	}
	transitioned, err := w.Store.TransitionJobStateWithEvent(ctx, jobID, string(workflow.JobFailed), string(workflow.JobQueued), db.JobEvent{
		JobID: jobID,
		Kind:  blockerDeferredEventKind,
		Message: fmt.Sprintf("%s: attempt %d/%d, retry at %s: %s",
			classification.Class, attempt, maxOperationalBlockerRetries, retryAt, classification.Detail),
	})
	if err != nil {
		return false, err
	}
	return transitioned, nil
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
