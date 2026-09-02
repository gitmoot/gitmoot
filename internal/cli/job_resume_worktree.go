package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// resumeSelfDirtyWorktreeForRunner recognizes one narrowly-owned
// checkout-contention case: a queued, top-level implement job being retried into
// its own dirty task worktree after the prior runtime owner died. It bypasses
// only the dirty pre-flight guard; every predicate miss or probe/persistence
// error returns false so jobWorker.run takes the existing checkout-contention
// path unchanged.
func (w jobWorker) resumeSelfDirtyWorktreeForRunner(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, cause error, runner subprocess.Runner) (string, workflow.JobPayload, bool) {
	kind, _ := classifyCheckoutContention(cause)
	if kind != checkoutContentionDirty || cause == nil || !strings.Contains(cause.Error(), "has uncommitted changes") {
		return "", payload, false
	}
	if job.Type != "implement" || job.State != string(workflow.JobQueued) {
		return "", payload, false
	}
	if strings.TrimSpace(payload.ParentJobID) != "" || strings.TrimSpace(payload.DelegationID) != "" {
		return "", payload, false
	}
	if strings.TrimSpace(payload.WorktreePath) != "" || strings.TrimSpace(payload.TaskID) == "" || w.Store == nil {
		return "", payload, false
	}

	task, err := w.Store.GetTask(ctx, payload.TaskID)
	if err != nil || strings.TrimSpace(task.WorktreePath) == "" {
		return "", payload, false
	}
	if task.RepoFullName != payload.Repo || task.Branch != payload.Branch {
		return "", payload, false
	}
	resolvedCheckout, ok, err := w.taskWorktreeCheckout(ctx, payload)
	if err != nil || !ok || resolvedCheckout == "" {
		return "", payload, false
	}
	normalizedTaskWorktree, err := normalizeTaskWorktreePath(task.WorktreePath)
	if err != nil || normalizedTaskWorktree == "" || !sameCheckoutPath(resolvedCheckout, normalizedTaskWorktree) {
		return "", payload, false
	}

	// The normal checkout validator reports dirtiness before checking HEAD, then
	// defaultCheckoutForRunner checks the unconditional implementation branch lock. Since
	// this recovery path bypasses only that dirty result, positively re-establish
	// both later invariants before allowing delivery.
	expectedHead := strings.TrimSpace(payload.HeadSHA)
	if expectedHead == "" {
		return "", payload, false
	}
	head, err := jobGitClient(resolvedCheckout, runner).HeadSHA(ctx)
	if err != nil || head != expectedHead {
		return "", payload, false
	}
	if err := w.validateImplementationLock(ctx, payload, implementationLockOwner(agent, payload)); err != nil {
		return "", payload, false
	}

	// Never re-deliver into a worktree that may still have an owner. The process
	// probe is the lock-independent gate; the runtime lease covers a worker whose
	// cwd is not observable. Any indeterminate process or lease state fails closed.
	live, known := taskWorktreeLiveness(resolvedCheckout)
	if live || !known {
		return "", payload, false
	}
	leaseHeld, err := runtimeOwnerLeaseHeld(ctx, w.Store, job.ID, time.Now().UTC())
	if err != nil || leaseHeld {
		return "", payload, false
	}

	if payload.BlockerAttempts < 0 || payload.BlockerAttempts >= maxOperationalBlockerRetries {
		return "", payload, false
	}
	attempt := payload.BlockerAttempts + 1
	resumed := payload
	resumed.BlockerClass = string(blockerClassCheckoutContention)
	resumed.BlockerAttempts = attempt
	resumed.BlockerRetryAt = ""
	resumed.BlockerSuggestedAction = ""
	resumed.BlockerPreDelivery = true
	resumed.ResumedSelfDirtyWorktree = true
	encoded, err := json.Marshal(resumed)
	if err != nil {
		return "", payload, false
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		return "", payload, false
	}
	message := fmt.Sprintf("%s: attempt %d/%d; prior attempt's completed work present uncommitted; re-delivering",
		resolvedCheckout, attempt, maxOperationalBlockerRetries)
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "worktree_resumed", Message: message}); err != nil {
		return "", payload, false
	}
	return resolvedCheckout, resumed, true
}
