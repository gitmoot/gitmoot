package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// Issue #532 slice C: classify the daemon-owned pre-flight CHECKOUT strings —
// which today TERMINALLY fail a job through finishQueuedJob — and defer the job
// for automatic re-dispatch instead. Two sub-classes of the checkout_contention
// class, both keyed off strings the daemon itself emits (validateTargetCheckout /
// validateReviewCheckout / validateImplementationLock):
//
//   - branch-LOCK contention ("branch %s is locked by %s"): another worker holds
//     the branch lock. It self-heals the moment that worker finishes (#328/#329
//     checkout-key serialization already makes most contention never even reach
//     here), so it defers with a SHORT EXPONENTIAL backoff and no suggested_action.
//   - DIRTY / WRONG-HEAD ("checkout %s has uncommitted changes", "checkout head
//     is %s, not …", "checkout branch is %s, not …"): the registered checkout is
//     not in the state the job needs. That usually needs a human to clean/reset
//     the checkout, so it defers with a fixed backoff AND a suggested_action
//     surfaced through the #552 stuck surface.
//
// Scope discipline (mirrors slice A):
//   - Delegation children (ParentJobID set) are EXCLUDED and keep their existing
//     DAG routing (finishQueuedJob → finalizePreflightDelegationChild → the
//     delegation failure_policy, #409). This slice only diverts non-delegation
//     jobs whose pre-flight checkout failed.
//   - Auto-retries share the SAME hard budget as the other classes
//     (maxOperationalBlockerRetries): a contention that outlives the budget stays
//     terminally failed exactly like today (with the suggested_action preserved in
//     the payload/stuck surface).
//   - The job is deferred from the QUEUED state (the pre-flight runs before the
//     mailbox claims queued→running), so no state transition happens — the hold is
//     just the payload fields + a blocker_deferred event, and listPendingQueuedJobs'
//     queuedJobBlockerHeld gate holds it. Because finishQueuedJob is never called,
//     NO job.failed precedes the additive job.deferred (the pre-terminal property,
//     same as slice E's mailbox seam).

type checkoutContentionKind int

const (
	checkoutContentionNone checkoutContentionKind = iota
	checkoutContentionLock
	checkoutContentionDirty
)

// checkoutLockBaseBackoff / checkoutLockMaxBackoff bound the SHORT exponential
// backoff for self-healing branch-lock contention: 2s, 4s, 8s across the budget,
// clamped so a bit-shift can never park a job long.
const (
	checkoutLockBaseBackoff = 2 * time.Second
	checkoutLockMaxBackoff  = 30 * time.Second
	// checkoutDirtyBackoff is the fixed hold for a dirty/wrong-head checkout: it
	// usually needs a human, so give them a window to clean the checkout before the
	// next auto-retry (bounded by the shared budget) rather than hammering it.
	checkoutDirtyBackoff = 2 * time.Minute
)

// classifyCheckoutContention maps a daemon pre-flight checkout error to its
// sub-class and, for the human-needing sub-class, a concrete suggested_action.
// A non-checkout error returns checkoutContentionNone so the caller falls through
// to today's terminal path byte-identically.
func classifyCheckoutContention(cause error) (checkoutContentionKind, string) {
	if cause == nil {
		return checkoutContentionNone, ""
	}
	text := cause.Error()
	// Match ONLY the three daemon-owned pre-flight string families the #532 design
	// names: branch-lock contention, a dirty checkout, and a wrong HEAD. The
	// branch-identity guard ("checkout branch is X, not job branch Y") is
	// deliberately NOT matched — it is a routing/config mismatch (a job resolving a
	// checkout on the wrong branch), not a self-healing or human-clean-the-checkout
	// condition, so it keeps its existing terminal path.
	switch {
	case strings.Contains(text, "is locked by"):
		return checkoutContentionLock, ""
	case strings.Contains(text, "has uncommitted changes"):
		return checkoutContentionDirty, "the registered checkout has uncommitted changes; commit, stash, or discard them (git checkout -- .) so the job's pre-flight can pass, then it auto-retries"
	case strings.Contains(text, "checkout head is"):
		return checkoutContentionDirty, "the registered checkout is on the wrong commit; sync it to the head the job expects (git fetch && git reset --hard <sha>), then it auto-retries"
	}
	return checkoutContentionNone, ""
}

// checkoutLockBackoff returns the SHORT exponential backoff for the attempt-th
// branch-lock deferral (1-based), clamped to [base, max].
func checkoutLockBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := checkoutLockBaseBackoff << (attempt - 1)
	if d <= 0 || d > checkoutLockMaxBackoff {
		return checkoutLockMaxBackoff
	}
	return d
}

// deferCheckoutContention re-queues (holds) a non-delegation job whose daemon
// pre-flight checkout validation just failed on a classified contention string,
// instead of terminally failing it via finishQueuedJob. It operates on the QUEUED
// job the daemon is dispatching: it writes the blocker hold fields + a
// blocker_deferred event and emits the additive job.deferred, then returns true so
// the caller skips finishQueuedJob. Every non-matching / excluded / budget-spent
// case returns false so the existing terminal path runs unchanged.
func (w jobWorker) deferCheckoutContention(ctx context.Context, job db.Job, payload workflow.JobPayload, cause error) (bool, error) {
	kind, action := classifyCheckoutContention(cause)
	if kind == checkoutContentionNone {
		return false, nil
	}
	// Delegation children keep the DAG's own routing (finishQueuedJob →
	// finalizePreflightDelegationChild); never divert them here.
	if strings.TrimSpace(payload.ParentJobID) != "" {
		return false, nil
	}
	// The pre-flight runs before the mailbox claims the job, so only a still-queued
	// job is eligible; anything else belongs to other machinery.
	if job.State != string(workflow.JobQueued) {
		return false, nil
	}
	attempt := payload.BlockerAttempts + 1
	if attempt > maxOperationalBlockerRetries {
		_ = w.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: job.ID,
			Kind:  blockerExhaustedEventKind,
			Message: fmt.Sprintf("operational blocker %s recurred after %d auto-retries; job stays failed: %s",
				blockerClassCheckoutContention, maxOperationalBlockerRetries, firstLineTrimmed(cause.Error())),
		})
		return false, nil
	}
	delay := checkoutDirtyBackoff
	if kind == checkoutContentionLock {
		delay = checkoutLockBackoff(attempt)
	}
	retryAt := time.Now().UTC().Add(delay).Format(time.RFC3339Nano)
	detail := firstLineTrimmed(cause.Error())
	payload.BlockerClass = string(blockerClassCheckoutContention)
	payload.BlockerAttempts = attempt
	payload.BlockerRetryAt = retryAt
	payload.BlockerSuggestedAction = action
	// PRE-DELIVERY (#532): this hold trips at the daemon pre-flight, BEFORE Mailbox.Run
	// delivers a prompt, so the agent never executed — a re-dispatch is a first run with
	// no side effects to reconcile. Mark it so Mailbox.Run suppresses the slice-F
	// reconciliation notice (which would otherwise falsely tell the agent a prior
	// attempt ran and may have pushed branches / opened PRs).
	payload.BlockerPreDelivery = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		return false, err
	}
	message := fmt.Sprintf("%s: attempt %d/%d, retry at %s: %s",
		blockerClassCheckoutContention, attempt, maxOperationalBlockerRetries, retryAt, detail)
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: blockerDeferredEventKind, Message: message}); err != nil {
		return false, err
	}
	// Additive job.deferred, best-effort and nil-safe when [events] is OFF. No
	// job.failed precedes it: finishQueuedJob was never called (pre-terminal).
	emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, job.ID, events.EventJobDeferred, string(workflow.JobQueued), message)
	return true, nil
}
