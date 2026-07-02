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

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/github"
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
		delay := parseQuotaResetDelay(text)
		return blockerClassification{Class: blockerClassRuntimeQuota, RetryAt: now.Add(delay + blockerRetryJitter(delay)), Detail: detail}, true
	case "auth failing":
		return blockerClassification{Class: blockerClassRuntimeAuth, RetryAt: now.Add(authBlockerRetryDelay), Detail: detail}, true
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
// limit, quota, authentication, unauthorized, auth+invalid) are unchanged from
// #552. The first matching line wins.
func classifyAuthQuotaStrict(text string) string {
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		if l == "" {
			continue
		}
		httpCtx := strings.Contains(l, "http") || strings.Contains(l, "status code") || strings.Contains(l, "retry-after")
		switch {
		case strings.Contains(l, "usage limit"), strings.Contains(l, "rate limit"),
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

// parseQuotaResetDelay extracts the provider-declared reset delay from a
// rate-limit/quota error. A bare number with no unit is seconds (the
// Retry-After header shape) — but a number followed by an UNRECOGNIZED unit
// word ("try again in 5 fortnights") is unparseable, because reading it as
// seconds would re-dispatch inside a still-closed window and burn the retry
// budget. Unparseable messages (e.g. codex's "try again at Jun 14th") fall
// back to quotaBlockerFallbackDelay; parsed values are clamped to
// [quotaBlockerMinParsedDelay, quotaBlockerMaxParsedDelay].
func parseQuotaResetDelay(text string) time.Duration {
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
				return clampQuotaDelay(time.Duration(n * float64(scale)))
			}
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
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, jobID, events.EventJobDeferred, string(workflow.JobQueued), message)
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
