package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

// recordReadOnlyWorktreeReclaimOnAbort marks a job's dispatch-allocated read-only
// worktree (#739) for daemon reclaim when the job is ABORTED (cancel / kill /
// supersede) instead of running to a terminal AdvanceJob. On main, reactive
// worktrees were allocated at RUN time, so a queued job carried none; #739 now
// allocates the worktree at DISPATCH (before enqueue), so a queued read-only ask
// owns a detached worktree on disk. An abort bypasses the engine's deferred
// cleanupReadOnlyDelegationWorktree, and neither the advance-retry pass (no advance
// marker) nor the reclaim pass (no cleanup marker) would otherwise dispose it — it
// leaks permanently, accumulating one worktree per aborted ask.
//
// This is store-only and best-effort, exactly like the sibling branch-lock release
// on the same abort paths: it writes the delegation_worktree_cleanup_skipped marker
// the reclaim pass already keys on, so reclaimSkippedDelegationWorktrees disposes
// the worktree on a later tick (the reclaim state gate accepts cancelled). It is
// gated on the worktree still existing on disk so an already-cleaned job (e.g. a
// blocked ask whose run already disposed it) is not turned into a permanent,
// never-reconciled reclaim candidate.
func recordReadOnlyWorktreeReclaimOnAbort(ctx context.Context, store *db.Store, job db.Job, payload JobPayload) {
	if !isReadOnlyDelegationWorktree(job.Type, payload) {
		return
	}
	path := strings.TrimSpace(payload.WorktreePath)
	if path == "" {
		return
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return
	}
	_ = store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_worktree_cleanup_skipped",
		Message: fmt.Sprintf("read-only worktree %s preserved for daemon reclaim: job aborted (%s) before its terminal cleanup ran (#739)", path, job.State),
	})
}

const JobEventSupersededStaleHead = "superseded_stale_head"

func RetryJob(ctx context.Context, store *db.Store, jobID string) (db.Job, error) {
	if store == nil {
		return db.Job{}, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, err
	}
	// A session job (#657) is executed by the calling session, never the engine, so
	// it must never be re-queued: retry transitions the job to 'queued', which the
	// daemon would then claim and Deliver to a real runtime with an empty session
	// payload (a session *implement* job could push a spurious branch/PR). Refuse
	// the retry outright, before any state transition. GetJob scans the
	// externally_driven column into the struct, so this predicate is reliable.
	if job.ExternallyDriven {
		return db.Job{}, fmt.Errorf("job %s is a session job (externally driven) and cannot be retried", job.ID)
	}
	switch job.State {
	case string(JobFailed), string(JobBlocked), string(JobCancelled):
	default:
		return db.Job{}, fmt.Errorf("job %s is %s; retry requires failed, blocked, or cancelled", job.ID, job.State)
	}
	if job.State == string(JobCancelled) {
		fromRunning, err := latestCancellationWasFromRunning(ctx, store, job.ID)
		if err != nil {
			return db.Job{}, err
		}
		if fromRunning {
			return db.Job{}, fmt.Errorf("job %s was cancelled while running; wait for the active worker to settle before retrying", job.ID)
		}
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return db.Job{}, err
	}
	payload.Result = nil
	// A human-requested retry is a fresh lifecycle for the operational-blocker
	// machinery (#532): drop any deferral hold so the retried job dispatches now
	// (a stale blocker_retry_at would silently park a cancel→retried job behind
	// the old hold with a contradictory stuck reason), and reset the attempt
	// budget so a post-exhaustion manual retry regains full deferral tolerance.
	payload.BlockerClass = ""
	payload.BlockerAttempts = 0
	payload.BlockerRetryAt = ""
	payload.BlockerSuggestedAction = ""
	payload.BlockerPreDelivery = false
	if manualRetryShouldClearReadOnlyWorktree(job, payload) {
		payload.WorktreePath = ""
		payload.HeadSHA = ""
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		return db.Job{}, err
	}
	transitioned, err := store.TransitionJobStatePayloadWithEvent(ctx, job.ID, job.State, string(JobQueued), encoded, db.JobEvent{
		JobID:   job.ID,
		Kind:    "retry_queued",
		Message: fmt.Sprintf("retry requested from %s", job.State),
	})
	if err != nil {
		return db.Job{}, err
	}
	if !transitioned {
		latest, getErr := store.GetJob(ctx, job.ID)
		if getErr != nil {
			return db.Job{}, getErr
		}
		return db.Job{}, fmt.Errorf("job %s is %s; retry requires failed, blocked, or cancelled", latest.ID, latest.State)
	}
	return store.GetJob(ctx, job.ID)
}

// GateResumeOutcome is the result of MaybeResumeOnGatesCleared (#682): whether the
// blocked stage was auto-re-queued and, if not, why the resume was withheld.
type GateResumeOutcome struct {
	// Resumed is true iff the blocked job was re-queued through RetryJob.
	Resumed bool
	// Reason is a human-readable explanation of the outcome — the re-queue on
	// success, or why the resume was skipped (no gates, gates still open, session
	// job, or an awaiting-human pause that must not be bypassed).
	Reason string
}

// MaybeResumeOnGatesCleared auto-re-runs a blocked stage the moment its LAST gate
// is satisfied (#682), reusing the existing RetryJob machinery (which already
// resurrects blocked jobs) so the resumed stage — and, via the normal delegation
// DAG, everything downstream — wakes back up without any polling. It is the
// resume-on-clear seam the `gitmoot job gates clear` command calls after marking a
// gate satisfied; it is idempotent and safe to call when nothing should happen.
//
// It deliberately does NOT resume in three cases so the gate feature complements,
// never replaces, the human-escalation path:
//   - a job that still has an open gate (the blocker is only partially cleared);
//   - a session job (ExternallyDriven, #657) — RetryJob refuses these outright, and
//     resurrecting one would let the daemon Deliver an empty session payload;
//   - a stage whose delegation tree is paused awaiting a human (escalate_human /
//     ask-gate, #305/#340/#445) — clearing a resource gate must not bypass the
//     human's retry|continue|abort decision, which is driven from the coordinator
//     via `gitmoot resume`, not this child.
//
// A job that recorded no gates at all is a no-op (Resumed=false), so callers that
// invoke it unconditionally stay byte-identical for the non-gated path.
func MaybeResumeOnGatesCleared(ctx context.Context, store *db.Store, jobID string) (GateResumeOutcome, error) {
	if store == nil {
		return GateResumeOutcome{}, fmt.Errorf("store is required")
	}
	total, open, err := store.CountJobGates(ctx, jobID)
	if err != nil {
		return GateResumeOutcome{}, err
	}
	if total == 0 {
		return GateResumeOutcome{Reason: "no gates recorded for this job"}, nil
	}
	if open > 0 {
		return GateResumeOutcome{Reason: fmt.Sprintf("%d of %d gate(s) still open", open, total)}, nil
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return GateResumeOutcome{}, err
	}
	if job.State != string(JobBlocked) {
		return GateResumeOutcome{Reason: fmt.Sprintf("job is %s, not blocked; not auto-resumed", job.State)}, nil
	}
	if job.ExternallyDriven {
		return GateResumeOutcome{Reason: "session job (externally driven) is not auto-resumed"}, nil
	}
	// Pipeline stage jobs (#681) resume at the RUN level (ResumePipelineRun mints
	// attempt+1 with a new job id); retrying the old stage job here would orphan
	// its re-execution outside the run's stage rows. Refuse, even for gate rows
	// recorded before the mailbox-side exclusion existed.
	if payload, perr := ParseJobPayload(job.Payload); perr == nil && payload.Sender == PipelineJobSender {
		return GateResumeOutcome{Reason: "pipeline stage job; resume the run via `gitmoot pipeline resume`, job gates do not re-run stages"}, nil
	}
	awaiting, err := blockedJobAwaitingHuman(ctx, store, job)
	if err != nil {
		return GateResumeOutcome{}, err
	}
	if awaiting {
		return GateResumeOutcome{Reason: "tree is paused awaiting a human; resume via `gitmoot resume`, gates do not bypass the human"}, nil
	}
	if _, err := RetryJob(ctx, store, jobID); err != nil {
		return GateResumeOutcome{}, err
	}
	_ = store.AddJobEvent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    "gates_cleared_resume",
		Message: "all gates satisfied; re-queued the blocked stage (#682)",
	})
	return GateResumeOutcome{Resumed: true, Reason: "all gates satisfied; re-queued the blocked stage"}, nil
}

// blockedJobAwaitingHuman reports whether a blocked job's delegation tree is paused
// awaiting a human (#305/#340/#445), so a cleared resource gate does not bypass the
// human. It checks both the durable signals: the SHARED task state
// (awaiting_human), and an OPEN escalation round on the job or its coordinator
// parent (requested > resolved). A normal block_parent/blocked stage sets its task
// to `blocked` (not awaiting_human) with no escalation, so this returns false and
// the stage auto-resumes.
func blockedJobAwaitingHuman(ctx context.Context, store *db.Store, job db.Job) (bool, error) {
	payload, err := unmarshalPayload(job.Payload)
	if err == nil {
		if taskID := strings.TrimSpace(payload.TaskID); taskID != "" {
			task, terr := store.GetTask(ctx, taskID)
			if terr == nil && task.State == string(TaskAwaitingHuman) {
				return true, nil
			}
		}
	}
	openIDs, err := store.JobIDsWithOpenEscalation(ctx)
	if err != nil {
		return false, err
	}
	parentID := strings.TrimSpace(payload.ParentJobID)
	for _, id := range openIDs {
		if id == job.ID || (parentID != "" && id == parentID) {
			return true, nil
		}
	}
	return false, nil
}

func manualRetryShouldClearReadOnlyWorktree(job db.Job, payload JobPayload) bool {
	if strings.TrimSpace(payload.WorktreePath) == "" {
		return false
	}
	if strings.TrimSpace(payload.TaskID) != "" {
		return false
	}
	switch strings.TrimSpace(job.Type) {
	case "ask", "review":
		return true
	default:
		return false
	}
}

func latestCancellationWasFromRunning(ctx context.Context, store *db.Store, jobID string) (bool, error) {
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Kind == "cancel_settled" {
			return false, nil
		}
		switch event.Kind {
		case string(JobCancelled), JobEventSupersededStaleHead:
			return strings.HasPrefix(event.Message, "cancel requested from running"), nil
		}
	}
	return false, nil
}

func SettleCancelledRunningJob(ctx context.Context, store *db.Store, jobID string, message string) (bool, error) {
	if store == nil {
		return false, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	if job.State != string(JobCancelled) {
		return false, nil
	}
	fromRunning, err := latestCancellationWasFromRunning(ctx, store, job.ID)
	if err != nil {
		return false, err
	}
	if !fromRunning {
		return false, nil
	}
	if message == "" {
		message = "cancelled job worker settled"
	}
	return true, store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "cancel_settled", Message: message})
}

// CancelJob is the single-job abandon verb (#631). It transitions a queued,
// running, or blocked job to cancelled and best-effort releases the locks the
// job still owns. A blocked job is one paused awaiting a human (an operator
// permission gate or an unrecoverable BlockedError), so dismissing it is the
// same abandon intent as cancelling a queued/running one — cancel is that verb.
//
// Scope is deliberately a single row: cancel does NOT propagate to a delegation
// tree, touch task locks/state, or set the RootKilled flag. Abandoning a whole
// delegation tree is a different verb (job kill / KillDelegationTree); routing
// dismissal through the kill machinery would over-reach a lone blocked leg into
// its siblings and coordinator. isTerminalJobState already treats blocked and
// cancelled identically, so a blocked->cancelled move changes no delegation
// barrier disposition.
//
// Dismissal is retry-reversible: RetryJob accepts cancelled jobs, so a dismissed
// blocked job can be resurrected. That is accepted behavior — the settle gate
// that guards retry after a running-cancel does not apply to a cancel from
// blocked (a blocked job has no active worker to outrace).
func CancelJob(ctx context.Context, store *db.Store, jobID string) (db.Job, error) {
	if store == nil {
		return db.Job{}, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, err
	}
	switch job.State {
	case string(JobQueued), string(JobRunning), string(JobBlocked):
	default:
		return db.Job{}, fmt.Errorf("job %s is %s; cancel requires queued, running or blocked", job.ID, job.State)
	}
	transitioned, err := store.TransitionJobStateWithEvent(ctx, job.ID, job.State, string(JobCancelled), db.JobEvent{
		JobID:   job.ID,
		Kind:    string(JobCancelled),
		Message: fmt.Sprintf("cancel requested from %s", job.State),
	})
	if err != nil {
		return db.Job{}, err
	}
	if !transitioned {
		latest, getErr := store.GetJob(ctx, job.ID)
		if getErr != nil {
			return db.Job{}, getErr
		}
		return db.Job{}, fmt.Errorf("job %s is %s; cancel requires queued, running or blocked", latest.ID, latest.State)
	}
	// Best-effort: release any resource locks the cancelled job still owns (e.g. a
	// stranded runtime-session lock whose deferred release never ran because the
	// job was killed) so the next job on that runtime session does not wait out
	// the full lock TTL. This only makes the existing TTL-based reaper release
	// happen sooner for a cancelled job — the same brief same-session window
	// already exists when a long-running job's lock TTL lapses while its runtime
	// is still in flight; cancelling signals intent to abandon the job. A fully
	// race-free release would have to reap the runtime process first (separate,
	// larger change). We swallow the error on purpose: lock cleanup is incidental
	// and must never make a successful cancel fail.
	_, _ = store.DeleteResourceLocksByOwner(ctx, job.ID)
	// Best-effort: an implement delegation leg cancelled here never runs the engine's
	// terminal cleanupImplementDelegationWorktree (that fires from AdvanceJob, which a
	// cancel bypasses), so its per-delegation branch lock would otherwise leak exactly
	// like the success path did before #617. Release it symmetric with
	// AllocateDelegationWorktree's CreateLock so a cancelled burst does not strand
	// gitmoot-delegation-* locks that block the next same-repo orchestration. Gated to
	// worktree-isolated implement legs and swallowed on error: lock cleanup is
	// incidental and must never make a successful cancel fail.
	if payload, perr := unmarshalPayload(job.Payload); perr == nil {
		if released, rerr := releaseDelegationBranchLock(ctx, store, job.Type, payload); rerr == nil && released {
			_ = store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_branch_lock_released",
				Message: fmt.Sprintf("released delegation branch lock %s on cancel (#617)", strings.TrimSpace(payload.Branch)),
			})
		}
		// Symmetric with the branch-lock release above: dispose a #739 dispatch-time
		// read-only worktree that this cancel-before-run would otherwise leak.
		recordReadOnlyWorktreeReclaimOnAbort(ctx, store, job, payload)
	}
	return store.GetJob(ctx, job.ID)
}

func SupersedeStaleHeadJob(ctx context.Context, store *db.Store, jobID string, reason string) (db.Job, bool, error) {
	if store == nil {
		return db.Job{}, false, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, false, err
	}
	switch job.State {
	case string(JobQueued), string(JobRunning):
	default:
		return job, false, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "review job superseded by newer pull request head"
	}
	if job.State == string(JobRunning) && !strings.HasPrefix(reason, "cancel requested from running") {
		reason = "cancel requested from running: " + reason
	}
	transitioned, err := store.TransitionJobStateWithEvent(ctx, job.ID, job.State, string(JobCancelled), db.JobEvent{
		JobID:   job.ID,
		Kind:    JobEventSupersededStaleHead,
		Message: reason,
	})
	if err != nil {
		return db.Job{}, false, err
	}
	if !transitioned {
		latest, getErr := store.GetJob(ctx, job.ID)
		if getErr != nil {
			return db.Job{}, false, getErr
		}
		return latest, false, nil
	}
	// Defensive symmetry with CancelJob: a superseded job that carried a #739
	// dispatch-time read-only worktree must not leak it (no-op for the review jobs
	// this path targets today, which run in a per-PR task worktree, not a #739 seat).
	if payload, perr := unmarshalPayload(job.Payload); perr == nil {
		recordReadOnlyWorktreeReclaimOnAbort(ctx, store, job, payload)
	}
	updated, err := store.GetJob(ctx, job.ID)
	return updated, true, err
}
