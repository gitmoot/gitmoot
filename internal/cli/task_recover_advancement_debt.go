package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// taskRecoverAdvanceSettlementKind is the settlement this recovery appends. It
// is NOT a new vocabulary word: advance_retry_skipped already closes
// advancement debt in workflow's own predicate
// (internal/workflow/task_liveness.go:164), which is exactly why clearing the
// debt with it makes EVERY consumer of that shared predicate stop treating the
// job as live. No kind set is read, altered or duplicated here; the duplicated
// global sets in task_liveness.go, daemon_worker.go and store_jobs.go are
// deliberately untouched.
//
// It also says the truthful thing. advance_completed or advance_retried would
// claim an advance happened; advance_retry_skipped records that one was
// deliberately not retried, which is what a human running task recover has
// decided.
const taskRecoverAdvanceSettlementKind = "advance_retry_skipped"

// settleStaleAdvancementDebtForRecovery is the #1174 correction: a supported
// `gitmoot task recover` must reclaim a task parked in awaiting_human_merge
// whose only apparently-live job is TERMINAL and held live solely by stale
// advancement debt.
//
// THE MEASURED INCIDENT. Task adhoc-c65bb333 sat in awaiting_human_merge while
// job local-implement-checkin-builder-18d2c814b3b36343 was SUCCEEDED with a
// trailing advance_retry naming obsolete head ae06783e, against a checkout and
// PR already at the approved cb415318. jobKeepsTaskLive
// (task_liveness.go:139) reports a settled job with open debt as live, so
// recovery refused, and native advancement expects `reviewing` rather than
// awaiting_human_merge, so neither path could progress.
//
// IT RETURNS false RATHER THAN REFUSING, so the caller keeps ownership of the
// refusal text and message drift is impossible.
//
// WHY THE RE-CHECK IS THE PROOF. "Live solely by advancement debt" is asserted
// by nobody here: after appending the settlement this asks the SAME exported
// predicate the caller used. If the job is still live it had another reason and
// the caller refuses as before. Re-deriving liveness in this package would mean
// copying workflow's event-kind list into a fourth file, which is precisely
// what the directive forbids.
func settleStaleAdvancementDebtForRecovery(
	ctx context.Context,
	store *db.Store,
	task db.Task,
	active db.Job,
	owner string,
) (bool, error) {
	if store == nil {
		return false, nil
	}
	// GATE ONE: only the parked state this correction is scoped to. A task in
	// implementing or reviewing has a live lifecycle and must keep the refusal.
	if strings.TrimSpace(task.State) != string(workflow.TaskAwaitingHumanMerge) {
		return false, nil
	}
	// GATE TWO: terminal, and NOT cancelled.
	//
	// Queued and running are excluded by settledness. Cancelled is excluded
	// EXPLICITLY even though IsSettledJobState includes it (types.go:144),
	// because a cancelled-from-running job is kept live by an independent rule
	// at task_liveness.go:142 - appending a settlement there would clear a debt
	// that was never the obstacle. With both excluded, the only liveness reason
	// left at task_liveness.go:139 is advancementPending, which is what makes
	// this gate exact rather than approximate.
	if !workflow.IsSettledJobState(active.State) || active.State == string(workflow.JobCancelled) {
		return false, nil
	}

	// IfAbsent, not AddJobEvent: a repeated `task recover` must not stack
	// duplicate settlement rows on the same job.
	if err := store.AddJobEventIfAbsent(ctx, db.JobEvent{
		JobID: active.ID,
		Kind:  taskRecoverAdvanceSettlementKind,
		Message: fmt.Sprintf(
			"advancement debt settled by task recover for task %s owner %s: job is terminal (%s) and its outstanding advance names a head the checkout has moved past; recovery re-arms the task without replaying review or rolling back the approved head (#1174)",
			task.ID, strings.TrimSpace(owner), active.State,
		),
	}); err != nil {
		return false, fmt.Errorf("settle stale advancement debt on job %s: %w", active.ID, err)
	}

	// THE RE-CHECK. Same exported predicate the caller used.
	stillActive, stillLive, err := workflow.FindLiveTaskJob(ctx, store, task)
	if err != nil {
		return false, err
	}
	if stillLive {
		// Another reason keeps it live, or a different job does. Either way the
		// caller's refusal is correct and the settlement above was a no-op for
		// liveness purposes.
		_ = stillActive
		return false, nil
	}
	return true, nil
}
