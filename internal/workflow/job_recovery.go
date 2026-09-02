package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// recordReadOnlyWorktreeReclaimOnAbort marks a job's dispatch-allocated
// read-only or fix worktree for daemon reclaim when the job is ABORTED (cancel /
// kill / supersede) instead of running to a terminal AdvanceJob. These
// worktrees exist before enqueue, so an abort bypasses their deferred terminal
// cleanup; without the marker they would leak permanently.
//
// This is store-only and best-effort, exactly like the sibling branch-lock release
// on the same abort paths: it writes the delegation_worktree_cleanup_skipped marker
// the reclaim pass already keys on, so reclaimSkippedDelegationWorktrees disposes
// the worktree on a later tick (the reclaim state gate accepts cancelled). It is
// gated on the worktree still existing on disk so an already-cleaned job (e.g. a
// blocked ask whose run already disposed it) is not turned into a permanent,
// never-reconciled reclaim candidate.
func recordReadOnlyWorktreeReclaimOnAbort(ctx context.Context, store *db.Store, job db.Job, payload JobPayload) {
	if !isReadOnlyDelegationWorktree(job.Type, payload) && !isFixWorktree(job.Type, payload) {
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
		Message: fmt.Sprintf("dispatch worktree %s preserved for daemon reclaim: job aborted (%s) before its terminal cleanup ran", path, job.State),
	})
}

const JobEventSupersededStaleHead = "superseded_stale_head"

// JobEventSupersededPullRequestClosed marks a QUEUED job terminated because the
// pull request it was dispatched for is no longer open (#1673). A merged or closed
// PR cannot be implemented or reviewed, so the leg is not waiting for capacity, it
// is waiting for a condition that has become impossible. The event kind is the
// legible half: a terminal state with a reason can be read, an indefinite `queued`
// row is camouflage that inflates every queue-depth check forever.
const JobEventSupersededPullRequestClosed = "superseded_pr_closed"

// SupersedeAdvanceLeaseTTL is how far ahead the advance's OWNERSHIP LEASE is set,
// and renewed. It is not a budget for the whole advance: the owner renews at every
// barrier and before every slow phase, so a legitimately slow allocation, fetch or
// enqueue never lapses mid-flight. Its only job is to bound how long an ABANDONED
// owner — a pass that was killed — can block a retry, which is why it is short.
const SupersedeAdvanceLeaseTTL = 2 * time.Minute

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
	// A supersession recovery's parent-advance CRITICAL SECTION is the one window in
	// which a re-queue is destructive rather than merely racy: AdvanceJob emits
	// irreversible parent effects (a failure policy applied, a dependent enqueued, a
	// continuation minted), and none can be taken back once the lifecycle they were
	// computed from is gone. The exclusion therefore rides in the SAME statement as
	// the terminal-to-queued transition below — BOTH arms — rather than being read
	// here, because a recovery pass can take ownership between a pre-check and the
	// write.
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
	retryEvent := db.JobEvent{
		JobID:   job.ID,
		Kind:    "retry_queued",
		Message: fmt.Sprintf("retry requested from %s", job.State),
	}
	transitioned := false
	// NOT `if task, ok, err := ...`: that form SHADOWS err, so an error from either
	// transition below was assigned to the inner variable and never returned — the
	// caller saw the generic "retry requires failed, blocked, or cancelled" message
	// instead of the real refusal. The advance-ownership refusal has to reach the
	// operator verbatim, so the lookup gets its own statement.
	task, hasDismissedTask, lookupErr := dismissedTaskForJob(ctx, store, payload)
	if lookupErr != nil {
		return db.Job{}, lookupErr
	}
	if hasDismissedTask {
		taskState := string(TaskPlanned)
		if job.Type == "implement" && strings.TrimSpace(payload.Branch) != "" && strings.TrimSpace(payload.WorktreePath) != "" {
			taskState = string(TaskImplementing)
		}
		transitioned, err = store.TransitionJobStatePayloadWithEventAndTaskTransition(ctx,
			job.ID, job.State, string(JobQueued), encoded, time.Now().UTC(), retryEvent,
			task.ID, string(TaskDismissed), taskState, "task_recovered_job_retry",
			fmt.Sprintf("restored dismissed task before retrying job %s", job.ID))
	} else {
		transitioned, err = store.TransitionJobStatePayloadWithEventUnlessAdvanceOwned(ctx, job.ID, job.State, string(JobQueued), encoded, time.Now().UTC(), retryEvent)
	}
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

func dismissedTaskForJob(ctx context.Context, store *db.Store, payload JobPayload) (db.Task, bool, error) {
	if taskID := strings.TrimSpace(payload.TaskID); taskID != "" {
		task, err := store.GetTask(ctx, taskID)
		if err == nil && task.State == string(TaskDismissed) {
			return task, true, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return db.Task{}, false, err
		}
	}
	if strings.TrimSpace(payload.Repo) == "" || strings.TrimSpace(payload.Branch) == "" {
		return db.Task{}, false, nil
	}
	task, err := store.GetTaskByRepoBranch(ctx, payload.Repo, payload.Branch)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Task{}, false, nil
	}
	if err != nil {
		return db.Task{}, false, err
	}
	return task, task.State == string(TaskDismissed), nil
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
	case "ask", "review", "produce":
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
	releaseAbortedJobResources(ctx, store, job, abortCauseCancel)
	return store.GetJob(ctx, job.ID)
}

// abortCause names why a job is being ended outside its terminal engine path. It
// exists only so the audit messages keep saying what actually happened: a cancel
// reads "on cancel", a supersede reads "on supersede", and neither pretends to be
// the other.
type abortCause struct {
	verb string
	noun string
}

var (
	abortCauseCancel    = abortCause{verb: "cancel", noun: "cancellation"}
	abortCauseSupersede = abortCause{verb: "supersede", noun: "supersession"}
)

// releaseAbortedJobResources runs the best-effort cleanups a job that dies BEFORE
// its terminal engine path owes: the resource locks it still holds, its
// per-delegation branch lock, its task lane lock, and its dispatch-time read-only
// worktree. Every one is swallowed on error, because incidental cleanup must never
// roll back a successful abort — but skipping them leaks locks that block the next
// same-repo work, so any path that ends a job outside AdvanceJob has to call this.
func releaseAbortedJobResources(ctx context.Context, store *db.Store, job db.Job, cause abortCause) {
	// A stranded runtime-session lock whose deferred release never ran would
	// otherwise make the next job on that session wait out the full TTL. This only
	// makes the existing TTL-based reaper release happen sooner: the same brief
	// same-session window already exists when a long-running job's lock TTL lapses
	// while its runtime is still in flight, and abandoning the job signals intent.
	_, _ = store.DeleteResourceLocksByOwner(ctx, job.ID, time.Now().UTC())
	releaseAbortedJobSideResources(ctx, store, job, cause)
}

// releaseSupersededJobResourcesAtGeneration is releaseAbortedJobResources for a
// caller that decided one POLL AGO. EVERY step is conditional on the job still
// being the settled lifecycle named by atGeneration, evaluated inside the write
// statement that performs it, so generation N's cleanup can never touch what
// generation N+1 acquired while this pass was mid-flight.
//
// It RETURNS its error. The previous version swallowed one, and a swallowed error
// let the caller record the debt paid while the cleanup had not run — the debt was
// then unrecoverable. A caller that cannot prove the cleanup ran must leave the
// marker outstanding.
//
// guarded false means the row moved: the branch, task-lane and worktree cleanups
// all belong to the abort of THAT run, and another run owns the row now.
func releaseSupersededJobResourcesAtGeneration(ctx context.Context, store *db.Store, job db.Job, cause abortCause, atGeneration int64) (bool, error) {
	_, guarded, err := store.ReleaseSupersededJobResourceLocksAtGeneration(ctx, job.ID, atGeneration, time.Now().UTC())
	if err != nil || !guarded {
		return false, err
	}
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageAfterResourceCommit); err != nil {
			return true, err
		}
	}
	payload, perr := unmarshalPayload(job.Payload)
	if perr != nil {
		return true, nil
	}
	// An implement delegation leg that dies here never runs the engine's terminal
	// cleanupImplementDelegationWorktree, so its per-delegation branch lock would
	// leak (#617). The unguarded force-release deletes by (repo, branch) alone, so a
	// retry that re-queued and re-acquired the SAME delegation lane lost it to this
	// pass; the generation rides in the DELETE's own predicate instead.
	if isImplementDelegationWorktree(job.Type, payload) {
		repo := strings.TrimSpace(payload.Repo)
		branch := strings.TrimSpace(payload.Branch)
		if repo != "" && branch != "" {
			released, rerr := store.ForceReleaseDelegationBranchLockAtJobGeneration(ctx, repo, branch, job.ID, atGeneration, db.BranchLockEvent{
				Kind:    "released",
				Message: "released after delegation leg reached a terminal state (#617)",
			})
			if rerr != nil {
				return true, rerr
			}
			if released {
				if _, eerr := store.AddJobEventAtGeneration(ctx, db.JobEvent{
					JobID:   job.ID,
					Kind:    "delegation_branch_lock_released",
					Message: fmt.Sprintf("released delegation branch lock %s on %s (#617)", branch, cause.verb),
				}, atGeneration); eerr != nil {
					return true, eerr
				}
			}
		}
	}
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeTaskLane); err != nil {
			return true, err
		}
	}
	// The task lane's inactivity vetoes are NOT sufficient for this caller. The
	// tasks veto deliberately excludes the exact implementing task being cleaned up,
	// so a retry of that same task cannot re-assert the lane through it, and the jobs
	// veto only fires while the retry is non-terminal — a retry that re-queued, ran
	// and settled terminal passes both and loses its lane. The generation therefore
	// rides in the DELETE's own predicate, its errors propagate, and a lost guard
	// stops the remaining cleanup.
	if job.Type == "implement" && strings.TrimSpace(payload.TaskID) != "" && strings.TrimSpace(payload.DelegationID) == "" {
		repo := strings.TrimSpace(payload.Repo)
		branch := strings.TrimSpace(payload.Branch)
		if repo != "" && branch != "" {
			lock, lerr := store.GetBranchLock(ctx, repo, branch)
			switch {
			case lerr == nil:
				released, rerr := store.ReleaseTaskLaneBranchLockAtJobGeneration(ctx, lock, strings.TrimSpace(payload.TaskID), job.ID, atGeneration, db.BranchLockEvent{
					Kind: "released", Message: fmt.Sprintf("released after task implement %s left no non-terminal branch work (#1565)", cause.noun),
				})
				if rerr != nil {
					return true, rerr
				}
				if released {
					written, eerr := store.AddJobEventAtGeneration(ctx, db.JobEvent{
						JobID: job.ID, Kind: "task_lane_lock_released",
						Message: fmt.Sprintf("released task lane lock %s on %s (#1565)", branch, cause.verb),
					}, atGeneration)
					if eerr != nil {
						return true, eerr
					}
					if !written {
						// The row moved between the guarded delete and its audit row. Stop:
						// the remaining cleanup belongs to a lifecycle that no longer owns
						// this job, and the debt stays outstanding for a re-drive.
						return false, nil
					}
				}
			case errors.Is(lerr, sql.ErrNoRows):
			default:
				return true, lerr
			}
		}
	}
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeReclaim); err != nil {
			return true, err
		}
	}
	// The reclaim marker is the dangerous one to append blind: the worktree path is
	// derived from the job id, so it is the SAME path a retry is using, and an
	// unguarded marker hands a live run's checkout to the reclaim pass.
	if err := recordSupersededReadOnlyWorktreeReclaimAtGeneration(ctx, store, job, payload, atGeneration); err != nil {
		return true, err
	}
	return true, nil
}

// releaseAbortedJobSideResources is the non-resource-lock half of an abort's
// cleanup for a caller acting on a lifecycle it observed JUST NOW (cancel, kill).
// Each step carries its own atomic predicate, and no generation anchor is needed
// because there is no poll-long gap between the decision and these writes.
func releaseAbortedJobSideResources(ctx context.Context, store *db.Store, job db.Job, cause abortCause) {
	payload, perr := unmarshalPayload(job.Payload)
	if perr != nil {
		return
	}
	if released, rerr := releaseDelegationBranchLock(ctx, store, job.Type, payload); rerr == nil && released {
		_ = store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_branch_lock_released",
			Message: fmt.Sprintf("released delegation branch lock %s on %s (#617)", strings.TrimSpace(payload.Branch), cause.verb),
		})
	}
	if job.Type == "implement" && strings.TrimSpace(payload.TaskID) != "" && strings.TrimSpace(payload.DelegationID) == "" {
		repo := strings.TrimSpace(payload.Repo)
		branch := strings.TrimSpace(payload.Branch)
		if repo != "" && branch != "" {
			if lock, lerr := store.GetBranchLock(ctx, repo, branch); lerr == nil {
				if released, rerr := store.ReleaseBranchLockIfInactiveWithEvent(ctx, lock, strings.TrimSpace(payload.TaskID), time.Time{}, db.BranchLockEvent{
					Kind: "released", Message: fmt.Sprintf("released after task implement %s left no non-terminal branch work (#1565)", cause.noun),
				}); rerr == nil && released {
					_ = store.AddJobEvent(ctx, db.JobEvent{
						JobID: job.ID, Kind: "task_lane_lock_released",
						Message: fmt.Sprintf("released task lane lock %s on %s (#1565)", branch, cause.verb),
					})
				}
			}
		}
	}
	recordReadOnlyWorktreeReclaimOnAbort(ctx, store, job, payload)
}

// recordSupersededReadOnlyWorktreeReclaimAtGeneration is the reclaim marker with a
// lifecycle anchor. The path comes from the payload the superseded run carried;
// because it is derived from the job id, a retry uses the SAME path, so an
// unguarded marker would hand a live checkout to the daemon's reclaim pass.
func recordSupersededReadOnlyWorktreeReclaimAtGeneration(ctx context.Context, store *db.Store, job db.Job, payload JobPayload, atGeneration int64) error {
	if !isReadOnlyDelegationWorktree(job.Type, payload) && !isFixWorktree(job.Type, payload) {
		return nil
	}
	path := strings.TrimSpace(payload.WorktreePath)
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err := store.AddJobEventAtGeneration(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_worktree_cleanup_skipped",
		Message: fmt.Sprintf("dispatch worktree %s preserved for daemon reclaim: job aborted (%s) before its terminal cleanup ran", path, job.State),
	}, atGeneration)
	return err
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

// JobEventSupersedeFinalizePending and JobEventSupersedeFinalizeCompleted bracket
// the follow-up work a closed-PR supersession owes AFTER its terminal state
// write: the abort cleanups, and for a delegation child the synthetic result and
// the parent advance.
//
// They exist because the terminal write moves the job out of `queued` while the
// closed-PR sweep selects only queued jobs. A crash or error in between left a
// child no sweep could rediscover and a coordinator that waited forever. The
// pending marker is written in the SAME transaction as the state write, so the
// debt is durable from the instant the job stops being queued;
// CompletePendingSupersedeFinalization pays it and records completion.
//
// The pending marker's message IS THE LIFECYCLE GENERATION it was written for,
// as a canonical decimal and nothing else, because the debt is about one run and
// `gitmoot job retry` can start another. Paying an old PR-closed debt against a
// newer lifecycle would stamp that run with the old synthetic failure and advance
// the parent on it, and a newer run that SUCCEEDS would leave the marker
// outstanding forever. A debt whose generation no longer matches is therefore
// VOIDED, never paid.
//
// The message carries the generation ALONE, with the human reason recovered from
// the superseded_pr_closed event written in the same transaction, because the
// classification must mean the same thing here and in SQL. The earlier
// `generation=<n>: <reason>` prefix could not: `generation=abc: …` parsed as
// unanchored while SQL read it as anchored-shaped, and `generation=01: …` parsed
// as 1 while SQL's anchored comparison missed it. Both produced a debt no path
// could close. A canonical decimal is expressible in both languages exactly —
// see supersedeFinalizationAnchoredPendingSQL.
const (
	JobEventSupersedeFinalizePending   = "supersede_finalize_pending"
	JobEventSupersedeFinalizeCompleted = "supersede_finalize_completed"
)

// formatSupersedeFinalizeDebt renders the marker message: the generation, canonical
// decimal, nothing else.
func formatSupersedeFinalizeDebt(generation int64) string {
	return strconv.FormatInt(generation, 10)
}

// parseSupersedeFinalizeDebt is the exact Go twin of the SQL classification. It
// accepts ONLY a canonical decimal — the FormatInt round-trip rejects `01`, `+1`
// and whitespace — so a message either names a generation in both languages or
// names none in both.
func parseSupersedeFinalizeDebt(message string) (int64, bool) {
	generation, err := strconv.ParseInt(message, 10, 64)
	if err != nil {
		return 0, false
	}
	if strconv.FormatInt(generation, 10) != message {
		return 0, false
	}
	return generation, true
}

// SupersedeClosedPullRequestJob terminates a QUEUED job whose pull request is no
// longer open (#1673). It is deliberately narrower than SupersedeStaleHeadJob in
// two ways.
//
// QUEUED ONLY. A running job is doing work whose output may still be worth
// harvesting, and killing it mid-flight is a different decision with different
// evidence; the stranded population this addresses has never started.
//
// NO PARENT PROPAGATION. Like CancelJob, this writes one job's terminal state and
// nothing else. A delegation child must NOT come through here: `cancelled` is
// rejected by finalizeTimedOutJob's state gate (engine_run_budgets.go), so a
// cancelled child would never advance its coordinator and the strand would move
// from the child to the parent. The daemon routes children through
// FinalizeClosedPullRequestDelegationChild instead.
//
// It takes the OBSERVED row rather than an id because the caller's verdict — this
// pull request is no longer open, so this leg is pointless — is about the run it
// saw. Anchoring the write on that row's lifecycle generation means a job that
// completed and was re-queued between the observation and the write loses the
// CAS instead of having its newer run cancelled.
func SupersedeClosedPullRequestJob(ctx context.Context, store *db.Store, observed db.Job, reason string) (db.Job, bool, error) {
	if store == nil {
		return db.Job{}, false, fmt.Errorf("store is required")
	}
	if strings.TrimSpace(observed.ID) == "" {
		return db.Job{}, false, fmt.Errorf("observed job id is required")
	}
	if observed.State != string(JobQueued) {
		return observed, false, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "queued job superseded: its pull request is no longer open"
	}
	transitioned, err := store.TransitionJobStateWithEventAtGeneration(ctx, observed.ID, observed.State, observed.LifecycleGeneration, string(JobCancelled), db.JobEvent{
		JobID:   observed.ID,
		Kind:    JobEventSupersededPullRequestClosed,
		Message: reason,
	}, db.JobEvent{
		JobID:   observed.ID,
		Kind:    JobEventSupersedeFinalizePending,
		Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration),
	})
	if err != nil {
		return db.Job{}, false, err
	}
	if !transitioned {
		latest, getErr := store.GetJob(ctx, observed.ID)
		if getErr != nil {
			return db.Job{}, false, getErr
		}
		return latest, false, nil
	}
	if err := completeSupersedeFinalization(ctx, nil, store, observed, reason, observed.LifecycleGeneration); err != nil {
		return db.Job{}, true, err
	}
	updated, err := store.GetJob(ctx, observed.ID)
	return updated, true, err
}

// FinalizeClosedPullRequestDelegationChild terminates a QUEUED delegation child
// whose pull request is no longer open, and — unlike the top-level path — makes the
// coordinator move. It transitions the child to `failed` with the same legible
// event kind and then hands it to the finalizer the worker already uses for a
// child that ended without a result, which stamps the synthetic result the parent's
// advanceDelegations requires.
//
// The state differs from the top-level path on purpose: finalizeTimedOutJob accepts
// running/failed/blocked and REJECTS cancelled, so `failed` is the only terminal
// state from which a child can still advance its parent. The event kind, not the
// state, is what makes the reason legible.
//
// Like the top-level path it settles the OBSERVED lifecycle generation, and it
// records the finalization debt atomically with the state write so a failure
// anywhere after the transition is recoverable rather than a permanent strand.
func (e Engine) FinalizeClosedPullRequestDelegationChild(ctx context.Context, observed db.Job, reason string) (bool, error) {
	if err := e.validate(); err != nil {
		return false, err
	}
	if strings.TrimSpace(observed.ID) == "" {
		return false, fmt.Errorf("observed job id is required")
	}
	payload, err := unmarshalPayload(observed.Payload)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(payload.ParentJobID) == "" || observed.State != string(JobQueued) {
		return false, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "queued delegation child superseded: its pull request is no longer open"
	}
	transitioned, err := e.Store.TransitionJobStateWithEventAtGeneration(ctx, observed.ID, observed.State, observed.LifecycleGeneration, string(JobFailed), db.JobEvent{
		JobID:   observed.ID,
		Kind:    JobEventSupersededPullRequestClosed,
		Message: reason,
	}, db.JobEvent{
		JobID:   observed.ID,
		Kind:    JobEventSupersedeFinalizePending,
		Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration),
	})
	if err != nil {
		return false, err
	}
	if !transitioned {
		return false, nil
	}
	return true, completeSupersedeFinalization(ctx, &e, e.Store, observed, reason, observed.LifecycleGeneration)
}

// CompletePendingSupersedeFinalization re-drives the follow-up work a supersession
// recorded and did not finish, for the LIFECYCLE THAT INCURRED IT.
//
// The debt names its generation, and `gitmoot job retry` can put the job back in
// queued at a newer one. Three outcomes, and only the first does any work:
//
//	SAME generation, terminal state -> pay it. The cleanups are individually
//	idempotent and finalizeTimedOutJob refuses a job that already carries a
//	result, so re-running a partly-paid debt is safe.
//
//	DIFFERENT generation -> VOID it. A newer run owns the job. Paying would stamp
//	that run with the old PR-closed failure and advance the parent on it; and if
//	the newer run succeeds, leaving the marker would keep this job a candidate on
//	every poll forever. The newer run settles through its own normal path, so the
//	right answer is to close the debt without acting.
//
//	LIVE state (queued/running) -> VOID it, for the same reason: a re-queue bumps
//	the generation, so a live row is by construction a different lifecycle.
//
// An unparseable marker predates this format (only reachable on a development
// database, since the marker is written and read by this same code) and is voided
// too: closing one stale debt is bounded, mislabeling a live run is not.
func (e Engine) CompletePendingSupersedeFinalization(ctx context.Context, jobID string) (bool, error) {
	if err := e.validate(); err != nil {
		return false, err
	}
	job, err := e.Store.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	debt, err := latestSupersedeFinalizeDebt(ctx, e.Store, jobID)
	if err != nil {
		return false, err
	}
	if !debt.pending {
		return false, nil
	}
	terminal := isSupersededTerminalState(job.State)
	if !debt.anchored || !terminal || debt.generation != job.LifecycleGeneration {
		return true, recordSupersedeFinalizationVoided(ctx, e.Store, job, debt)
	}
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageAfterRead); err != nil {
			return false, err
		}
	}
	// CLAIM the run, rather than acting on the read above. The read and the writes
	// live in different transactions, so between them `gitmoot job retry` can queue
	// a new lifecycle and a worker can claim it — and finalizing by job id alone
	// would then stamp the superseded run's failure onto a job that is running.
	// The self-transition re-asserts (state, generation) inside the statement, so a
	// moved row loses the claim and is re-classified instead. It writes no event:
	// one row per poll for as long as a fault persists is the unbounded
	// job_events growth the advance-retry markers exist to avoid.
	claimed, err := e.Store.TransitionJobStateAtGeneration(ctx, job.ID, job.State, job.LifecycleGeneration, job.State)
	if err != nil {
		return false, err
	}
	if !claimed {
		latest, getErr := e.Store.GetJob(ctx, jobID)
		if getErr != nil {
			return false, getErr
		}
		return true, recordSupersedeFinalizationVoided(ctx, e.Store, latest, debt)
	}
	return true, completeSupersedeFinalization(ctx, &e, e.Store, job, debt.reason, job.LifecycleGeneration)
}

// supersedeDebtInterleaveHook is a test seam for the windows a read cannot close:
// between the classification, the claim, and each anchored write, `gitmoot job
// retry` can queue a new lifecycle and a worker can claim it. Production leaves it
// nil, so the pass is byte-identical; a test sets it to perform that interleaving
// at one named stage and prove the guard owning that stage refuses.
//
// One stage per guard, so a test kills exactly one: the claim CAS, the payment's
// own re-read (which gates the UNANCHORED cleanups), the window between that
// re-read and the resource cleanup it authorises, the anchored payload write
// inside the finalizer, and the window between reading the latest debt marker and
// closing it.
const (
	supersedeDebtStageAfterRead           = "after-read"
	supersedeDebtStageAfterClaim          = "after-claim"
	supersedeDebtStageBeforeCleanup       = "before-cleanup"
	supersedeDebtStageAfterResourceCommit = "after-resource-commit"
	supersedeDebtStageBeforeTaskLane      = "before-task-lane"
	supersedeDebtStageBeforeReclaim       = "before-reclaim"
	supersedeDebtStageBeforeFinalize      = "before-finalize"
	supersedeDebtStageBeforeAdvance       = "before-advance"
	supersedeDebtStageBeforeAdvanceClaim  = "before-advance-claim"
	supersedeDebtStageBeforeClosure       = "before-closure"
)

// The parent-advance bracket's durable trace. Claimed and confirmed are both
// ANCHORED appends, so between them the lifecycle is provably unchanged; the
// superseded kind records the one case where it changed mid-advance and the debt
// was deliberately left outstanding.
const (
	JobEventSupersedeAdvanceClaimed    = "supersede_advance_claimed"
	JobEventSupersedeAdvanceConfirmed  = "supersede_advance_confirmed"
	JobEventSupersedeAdvanceSuperseded = "supersede_advance_superseded"
)

// The parent-effect barrier points inside advanceDelegations. Each names one
// EFFECT CLASS the directive requires to be unreachable from a superseded
// lifecycle: the child snapshot the pass reasons from, the failure policy, the
// dependent enqueue, and the coordinator continuation.
const (
	supersedeAdvanceBarrierChildSnapshot    = "child-snapshot"
	supersedeAdvanceBarrierFailurePolicy    = "failure-policy"
	supersedeAdvanceBarrierDependentEnqueue = "dependent-enqueue"
	supersedeAdvanceBarrierContinuation     = "continuation"
)

// supersedeAdvanceRolledBackError is returned when a barrier finds the anchored
// child lifecycle gone. It aborts the advance BEFORE the effect it guards, so no
// parent mutation is ever attributable to a superseded run. The recovery treats it
// as "not advanced": the debt stays outstanding and the next poll re-drives it
// against whatever lifecycle owns the row then.
type supersedeAdvanceRolledBackError struct {
	JobID      string
	Generation int64
	Barrier    string
}

func (e supersedeAdvanceRolledBackError) Error() string {
	return fmt.Sprintf("supersede advance aborted at the %s barrier: job %s left lifecycle %d", e.Barrier, e.JobID, e.Generation)
}

// assertSupersedeAdvanceAnchor is the barrier itself. It is a no-op for every
// ordinary AdvanceJob caller (supersedeAdvance is nil), so the delegation path is
// byte-identical outside supersession recovery.
//
// The check is a read, and it is deliberately NOT the only protection: RetryJob
// refuses to re-queue a job whose supersession advance is in flight, so the
// rollover this barrier looks for cannot normally happen at all. The barrier is
// what makes that guarantee verifiable per effect class rather than assumed.
func (e Engine) assertSupersedeAdvanceAnchor(ctx context.Context, barrier string) error {
	if e.supersedeAdvance == nil {
		return nil
	}
	if supersedeAdvanceBarrierHook != nil {
		supersedeAdvanceBarrierHook(ctx, barrier)
	}
	// RENEW BEFORE CHECKING, at every barrier. The lease is short so an abandoned
	// owner unblocks retries quickly; renewing here is what keeps a slow BUT LIVE
	// advance from being treated as abandoned while it is still working. A renewal
	// that fails means this pass no longer owns the advance, so the barrier aborts
	// before the effect it guards rather than acting without ownership.
	if err := e.renewSupersedeAdvanceLease(ctx); err != nil {
		return err
	}
	current, err := e.Store.GetJob(ctx, e.supersedeAdvance.JobID)
	if err != nil {
		return err
	}
	if current.LifecycleGeneration != e.supersedeAdvance.Generation || !isSupersededTerminalState(current.State) {
		return supersedeAdvanceRolledBackError{
			JobID:      e.supersedeAdvance.JobID,
			Generation: e.supersedeAdvance.Generation,
			Barrier:    barrier,
		}
	}
	return nil
}

// supersedeAdvanceAnchor pins an AdvanceJob pass to one child lifecycle.
type supersedeAdvanceAnchor struct {
	JobID      string
	Generation int64
	// LockKey and Token identify the ownership lease this pass holds. Every
	// irreversible parent effect renews and re-verifies them, so an effect can only
	// land while this pass still owns the advance.
	LockKey string
	Token   string
}

func (e Engine) renewSupersedeAdvanceLease(ctx context.Context) error {
	anchor := e.supersedeAdvance
	if anchor == nil || strings.TrimSpace(anchor.LockKey) == "" || strings.TrimSpace(anchor.Token) == "" {
		return nil
	}
	now := time.Now().UTC()
	// The renewal predicate requires the lease to be STILL UNEXPIRED and the job to
	// be STILL on the granted generation. Once an expired lease let a retry commit
	// N+1, this token is permanently dead: it must renew zero rows, and every later
	// barrier and effect fails closed rather than resurrecting it.
	renewed, err := e.Store.RenewAdvanceOwnershipLease(ctx, db.AdvanceOwnership{
		LockKey:      anchor.LockKey,
		OwnerToken:   anchor.Token,
		OwnerJobID:   anchor.JobID,
		AtGeneration: anchor.Generation,
	}, now.Add(SupersedeAdvanceLeaseTTL), now)
	if err != nil {
		return err
	}
	if !renewed {
		return supersedeAdvanceRolledBackError{JobID: anchor.JobID, Generation: anchor.Generation, Barrier: "ownership-lease"}
	}
	return nil
}

// supersedeAdvanceBarrierHook is a test seam that fires AT each barrier, which is
// the only place a test can interleave a retry inside AdvanceJob's parent-effect
// section. Nil in production.
var supersedeAdvanceBarrierHook func(ctx context.Context, barrier string)

var supersedeDebtInterleaveHook func(ctx context.Context, stage string) error

// supersedeFinalizeDebt is the state of a job's supersession debt: whether one is
// outstanding, which lifecycle incurred it, and the reason to reuse when paying.
type supersedeFinalizeDebt struct {
	pending    bool
	anchored   bool
	generation int64
	reason     string
}

// latestSupersedeFinalizeDebt applies the same last-one-wins rule as the store's
// candidate query: the highest-id event among exactly the two marker kinds
// decides. Reading it in Go as well keeps a direct caller honest, and lets the
// generation come from the marker the sweep actually wrote.
//
// The reason is recovered from the superseded_pr_closed event written in the SAME
// transaction as the marker, because the marker's message is now the generation
// alone. That keeps one classification rule shared with SQL and still lets a
// retried payment produce the same synthetic result text the first attempt would
// have.
func latestSupersedeFinalizeDebt(ctx context.Context, store *db.Store, jobID string) (supersedeFinalizeDebt, error) {
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		return supersedeFinalizeDebt{}, err
	}
	debt := supersedeFinalizeDebt{}
	reason := ""
	for _, event := range events {
		switch event.Kind {
		case JobEventSupersededPullRequestClosed:
			if trimmed := strings.TrimSpace(event.Message); trimmed != "" {
				reason = trimmed
			}
		case JobEventSupersedeFinalizePending:
			generation, anchored := parseSupersedeFinalizeDebt(event.Message)
			debt = supersedeFinalizeDebt{pending: true, anchored: anchored, generation: generation}
		case JobEventSupersedeFinalizeCompleted:
			debt = supersedeFinalizeDebt{}
		}
	}
	if debt.pending {
		debt.reason = reason
		if debt.reason == "" {
			debt.reason = "queued job superseded: its pull request is no longer open"
		}
	}
	return debt, nil
}

// recordSupersedeFinalizationVoided closes a debt that belongs to a lifecycle
// this job no longer has. It writes the same completion kind — the marker family
// is what the candidate query reads — with a message that says plainly no work
// was done, so the audit trail never implies a stale finalization ran.
func recordSupersedeFinalizationVoided(ctx context.Context, store *db.Store, job db.Job, debt supersedeFinalizeDebt) error {
	detail := fmt.Sprintf("generation %d", debt.generation)
	if !debt.anchored {
		detail = "an unanchored marker"
	}
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeClosure); err != nil {
			return err
		}
	}
	// Conditional for the same reason completion is: a void races the same retry.
	// If a newer pending marker has landed, writing this one would close a debt
	// that belongs to a lifecycle nobody has looked at yet.
	_, err := store.CloseSupersedeFinalizationDebtAtGeneration(ctx, job.ID,
		fmt.Sprintf("voided without action: the debt was incurred by %s, and the job is now %s at generation %d",
			detail, job.State, job.LifecycleGeneration),
		debt.generation, debt.anchored)
	return err
}

// completeSupersedeFinalization pays the recorded debt and, only once every step
// has succeeded, records that it is paid. engine is nil for a top-level job,
// which owes cleanups but no parent propagation.
//
// A BlockedError or AwaitingHumanError from the child finalizer is the parent's
// failure_policy ACTING, not this path failing, so the debt is settled and the
// error is returned for the caller to classify. Any other error leaves the
// pending marker in place, which is what makes the next poll retry it.
func completeSupersedeFinalization(ctx context.Context, engine *Engine, store *db.Store, job db.Job, reason string, generation int64) error {
	// Every step below belongs to ONE lifecycle, so start from a fresh read rather
	// than the caller's row: the inline caller's copy still shows `queued` (it was
	// taken before the terminal transition), and a recovery caller's copy can be
	// stale in the other direction — a retry may have queued and a worker claimed a
	// newer run since it decided. A moved row does nothing here beyond conditionally
	// closing the debt it was carrying: releasing another run's locks or worktree
	// would be worse than leaving this debt for the next poll.
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageAfterClaim); err != nil {
			return err
		}
	}
	current, err := store.GetJob(ctx, job.ID)
	if err != nil {
		return err
	}
	if current.LifecycleGeneration != generation || !isSupersededTerminalState(current.State) {
		return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, nil)
	}
	// The window F-1 named: validated above, cleanup below, two statements apart.
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeCleanup); err != nil {
			return err
		}
	}
	// A queued job dying here owes the SAME cleanups a cancel does — resource locks,
	// a per-delegation branch lock, the task lane lock, a dispatch-time read-only
	// worktree. Every one is anchored on the claimed lifecycle inside the statement
	// that performs it, and the error is PROPAGATED: a cleanup that could not be
	// proven to have run must leave the debt outstanding rather than be recorded
	// paid, which is how a swallowed SQLITE_BUSY_SNAPSHOT lost cleanup debt before.
	guarded, cleanupErr := releaseSupersededJobResourcesAtGeneration(ctx, store, current, abortCauseSupersede, generation)
	if cleanupErr != nil {
		return cleanupErr
	}
	if !guarded {
		return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, nil)
	}
	if engine == nil {
		return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, nil)
	}
	payload, perr := unmarshalPayload(current.Payload)
	if perr != nil || strings.TrimSpace(payload.ParentJobID) == "" {
		return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, nil)
	}
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeFinalize); err != nil {
			return err
		}
	}
	finalized, finalizeErr := engine.finalizeSupersededDelegationChildAtGeneration(ctx, job.ID, reason, generation)
	if finalizeErr != nil && !isDelegationPolicyOutcome(finalizeErr) {
		return finalizeErr
	}
	if !finalized && finalizeErr == nil {
		// The finalizer returns this both when the result is ALREADY stamped (a
		// previous attempt got that far and then failed) and when it refused because
		// the row moved to another lifecycle. Only the first owes a parent advance.
		advanced, err := engine.advanceSupersededChildAtGeneration(ctx, job.ID, generation)
		if err != nil {
			if !isDelegationPolicyOutcome(err) {
				return err
			}
			finalizeErr = err
		} else if !advanced {
			// The lifecycle moved before or during the advance. The debt stays
			// outstanding so the next poll re-drives it against whatever run owns the
			// row then; closing it here would bury a parent that is still waiting.
			return nil
		}
	}
	return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, finalizeErr)
}

// advanceSupersededChildAtGeneration is the parent-advance half of the recovery,
// bracketed so a retry cannot have its coordinator settled on a superseded run's
// verdict.
//
// AdvanceJob is multi-statement and has no lifecycle anchor of its own, so a
// re-read before calling it only narrows the window; it cannot close it. The
// bracket does two things a read cannot:
//
//	BEFORE  an anchored event append CLAIMS the advance. It is a conditional
//	        INSERT, so a row that has moved refuses it and no advance runs at all.
//	AFTER   a second anchored append CONFIRMS the lifecycle never moved while
//	        AdvanceJob was running. If it refuses, the advance may have mutated the
//	        parent from a stale verdict, so the durable trace says so and the debt
//	        is left outstanding: the next poll re-drives the advance against the run
//	        that owns the row then, and AdvanceJob is re-entrant, so the repair is
//	        the same call.
//
// advanced false means no parent mutation can be attributed to this lifecycle and
// the caller must NOT close the debt.
// coordinatorRoundNeedsRepair reports whether the coordinator this job would advance is
// holding an escalation round parked in needs_repair.
//
// Read-only, and deliberately not escalationRepairBlock: that one blocks the task as a
// side effect, which is right for an advance that is being refused mid-flight and wrong
// for a pre-flight check that runs on every recovery pass.
func (e Engine) coordinatorRoundNeedsRepair(ctx context.Context, jobID string) (bool, error) {
	job, err := e.Store.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	coordinatorID := strings.TrimSpace(job.ParentJobID)
	if coordinatorID == "" {
		return false, nil
	}
	round, ok, err := e.Store.UnsettledEscalationRound(ctx, coordinatorID)
	if err != nil {
		return false, err
	}
	return ok && round.NeedsRepair(), nil
}

func (e Engine) advanceSupersededChildAtGeneration(ctx context.Context, jobID string, generation int64) (bool, error) {
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeAdvanceClaim); err != nil {
			return false, err
		}
	}
	// REFUSE BEFORE CLAIMING ANYTHING. A coordinator whose escalation round is parked in
	// needs_repair cannot be advanced, so this pass must not take the ownership lease or
	// write the claim bracket for work it cannot do (#1673).
	//
	// WRITES NOTHING, INCLUDING NO TASK BLOCK, and that is a real behavioural difference
	// rather than a detail: because this pre-flight always short-circuits when the round
	// is parked, escalationRepairBlock's blockTask is now UNREACHABLE from this route, so
	// the coordinator's task keeps whatever state the escalation left it in
	// (awaiting_human) instead of moving to blocked. That matches the round's own design
	// - engine_escalation_resume.go states a parked round deliberately does not set a task
	// state - and the operator signal is the round's needs-repair event plus the repair
	// command, not this path. An earlier version of this comment claimed the guard still
	// blocked the task; review measured that false once the pre-flight was in front of it.
	//
	// The typed check after the advance stays as defence in depth: this one is a hygiene
	// fix, that one is the correctness barrier.
	if parked, err := e.coordinatorRoundNeedsRepair(ctx, jobID); err != nil {
		return false, err
	} else if parked {
		return false, nil
	}
	// OWNERSHIP FIRST, and it is a renewable lease rather than an age-bounded
	// marker: RetryJob's exclusion predicate reads this lock, so taking it here is
	// what makes the retry refusal atomic with the transition rather than advisory.
	// An owner that is genuinely working renews it; only a dead owner lets it lapse.
	lockKey := db.SupersedeAdvanceLockKeyPrefix + jobID
	token := fmt.Sprintf("supersede-advance-%s-%d-%d", jobID, generation, time.Now().UTC().UnixNano())
	now := time.Now().UTC()
	owned, err := e.Store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  jobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(SupersedeAdvanceLeaseTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return false, err
	}
	if !owned {
		// Another pass owns this advance right now. Not an error: the debt stays
		// outstanding and the owner either finishes it or its lease lapses.
		return false, nil
	}
	defer func() {
		// Release explicitly on EVERY exit, so a finished advance stops blocking
		// retries immediately instead of waiting out a lease.
		_, _ = e.Store.ReleaseResourceLock(ctx, lockKey, jobID, token)
	}()
	claimed, err := e.Store.AddJobEventAtGeneration(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    JobEventSupersedeAdvanceClaimed,
		Message: formatSupersedeFinalizeDebt(generation),
	}, generation)
	if err != nil || !claimed {
		return false, err
	}
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeAdvance); err != nil {
			return false, err
		}
	}
	// The advance runs on a COPY of the engine carrying the anchor, so every
	// parent-effect class inside advanceDelegations renews ownership and re-asserts
	// the child's lifecycle immediately before it acts. Prevention, not detection: an
	// aborted barrier returns before the effect it guards.
	anchored := e
	anchored.supersedeAdvance = &supersedeAdvanceAnchor{JobID: jobID, Generation: generation, LockKey: lockKey, Token: token}
	// PARENT DAG ONLY, and the reason is STRUCTURE rather than a specific escape. A full
	// AdvanceJob on this path would run lens normalization (rewriting the result and the
	// job's payload and state), continue into the review merge gate, and register
	// deferred worktree teardown - all under a recovery pass with no validated checkout.
	// This operation cannot reach any of it.
	//
	// WHAT IT DOES NOT CLAIM, corrected after review measured it: a full advance would
	// NOT dispatch this child's own delegations on the shape this path can mint. A
	// superseded child's result carries decision "failed", and the parent-side block
	// short-circuits on it (delegationFailureHandledByPolicy) long before
	// dispatchDelegations. The earlier version of this comment asserted that fan-out as
	// fact and cited a mutant that only reached the dispatch through decision "approved",
	// which the supersession cannot produce. The value here is that the exclusion is
	// structural instead of resting on an early return continuing to sit ahead of a
	// dispatch call in a long function.
	advanceErr := anchored.AdvanceParentDAGForTerminalChild(ctx, jobID)
	var rolledBack supersedeAdvanceRolledBackError
	if errors.As(advanceErr, &rolledBack) {
		e.recordSupersedeAdvanceRaced(ctx, jobID, generation, advanceErr)
		return false, nil
	}
	if AsEscalationRepairRequired(advanceErr) {
		// THE ADVANCE DID NOT HAPPEN, so it must not be confirmed and the debt must not be
		// settled. A round parked in needs_repair is CLEARABLE - the work becomes possible
		// again the moment `gitmoot escalation repair` runs - which makes it categorically
		// unlike the failure policies below, where the graph has DECIDED.
		//
		// Measured before this branch existed: isDelegationPolicyOutcome matches any
		// BlockedError, so the refusal fell through to the confirmation write, the caller
		// recorded the finalization debt PAID, and after the operator repaired the round
		// nothing re-drove the parent advance. The coordinator waited forever, which is
		// worse than the unguarded advance the guard replaced.
		//
		// false with a nil error is exactly the "lifecycle moved" contract the caller
		// already implements: leave the debt outstanding, raise no poll error, re-drive on
		// the next poll.
		return false, nil
	}
	if advanceErr != nil && !isDelegationPolicyOutcome(advanceErr) {
		// A retry that lands in a window no barrier covers CAUSES failures rather than
		// wrong results: production's RetryJob clears the child's result, so
		// AdvanceJob's own read finds none and refuses. Distinguish that from a real
		// fault by re-testing the anchor — if the lifecycle moved, the error is this
		// race, the debt simply stays outstanding, and no poll-level error is raised.
		superseded, probeErr := e.supersedeAdvanceLifecycleMoved(ctx, jobID, generation)
		if probeErr != nil {
			return false, probeErr
		}
		if superseded {
			e.recordSupersedeAdvanceRaced(ctx, jobID, generation, advanceErr)
			return false, nil
		}
		return false, advanceErr
	}
	confirmed, err := e.Store.AddJobEventAtGeneration(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    JobEventSupersedeAdvanceConfirmed,
		Message: formatSupersedeFinalizeDebt(generation),
	}, generation)
	if err != nil {
		return false, err
	}
	if !confirmed {
		e.recordSupersedeAdvanceRaced(ctx, jobID, generation, nil)
		return false, nil
	}
	return true, advanceErr
}

// supersedeAdvanceLifecycleMoved reports whether the row has left the lifecycle the
// advance was claimed for. It reads the row rather than probing with a write,
// because by this point the question is only how to CLASSIFY an outcome that has
// already happened; the anchored appends above are what decide whether anything is
// attributed to this lifecycle.
func (e Engine) supersedeAdvanceLifecycleMoved(ctx context.Context, jobID string, generation int64) (bool, error) {
	current, err := e.Store.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	return current.LifecycleGeneration != generation || !isSupersededTerminalState(current.State), nil
}

// recordSupersedeAdvanceRaced leaves the durable trace for an advance whose
// lifecycle moved. Best-effort: the debt is already staying outstanding, and an
// unwritable audit row must not turn a benign race into a poll error.
func (e Engine) recordSupersedeAdvanceRaced(ctx context.Context, jobID string, generation int64, cause error) {
	message := fmt.Sprintf("lifecycle %d was superseded while its parent advance ran; the debt stays outstanding for re-drive", generation)
	if cause != nil {
		message += ": " + cause.Error()
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    JobEventSupersedeAdvanceSuperseded,
		Message: message,
	})
}

// isSupersededTerminalState reports whether a job row is in one of the two states
// a closed-PR supersession writes. It is the "is this still the settled run I owe
// work for?" half of every anchor check: a `queued` or `running` row means a retry
// owns the job now, and its generation will have moved with it.
func isSupersededTerminalState(state string) bool {
	return state == string(JobCancelled) || state == string(JobFailed)
}

// isDelegationPolicyOutcome reports whether an error is the parent's
// failure_policy ACTING rather than the finalization failing. block_parent
// surfaces as BlockedError and escalate_human as AwaitingHumanError; both mean
// the DAG reached a decision, so the debt is paid and the caller classifies.
func isDelegationPolicyOutcome(err error) bool {
	var blocked BlockedError
	var awaiting AwaitingHumanError
	return errors.As(err, &blocked) || errors.As(err, &awaiting)
}

// recordSupersedeFinalizationCompleted closes the durable debt for ONE lifecycle.
// It runs only once every owed step has either succeeded or ended in a policy
// outcome; any other failure returns earlier and leaves the pending marker for
// the next poll.
//
// The close is CONDITIONAL on the latest pending marker still being the one this
// payment claimed. A retried run that gets superseded again writes a NEWER pending
// marker, and the candidate query is last-one-wins by event id — so an
// unconditional completion written afterwards would silently clear a debt nobody
// ever paid. When that has happened the newer debt is left outstanding and the next
// poll picks it up.
func recordSupersedeFinalizationCompleted(ctx context.Context, store *db.Store, jobID string, reason string, generation int64, outcome error) error {
	debt, err := latestSupersedeFinalizeDebt(ctx, store, jobID)
	if err != nil {
		return err
	}
	if !debt.pending || !debt.anchored || debt.generation != generation {
		return outcome
	}
	if supersedeDebtInterleaveHook != nil {
		if err := supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeClosure); err != nil {
			return err
		}
	}
	// The read above only decides WHETHER to attempt the close; the store re-asserts
	// the same predicate inside the INSERT, so a pending marker for a newer
	// lifecycle appended in between cannot be overwritten by this one.
	if _, err := store.CloseSupersedeFinalizationDebtAtGeneration(ctx, jobID, reason, generation, true); err != nil {
		return err
	}
	return outcome
}

// SetClosedPRChildPreAdvanceHookForTest installs an interruption at the window between
// the atomic terminal commit and the parent advancement — the window the round-2 #1763
// P1 occupied. The actuator that recovers it lives in internal/cli, so the boundary can
// only be exercised end-to-end from there (#1673).
//
// It is an ADAPTER over supersedeDebtInterleaveHook rather than a second hook. Both
// named the same boundary, and the merge of #1731 with #1763 left the standalone one
// with no production call site at all: it still compiled, its test still passed against
// main's code, and it constrained nothing here. Two seams at one boundary is how that
// happens quietly, so there is one seam and this is a view onto its before-advance
// stage.
func SetClosedPRChildPreAdvanceHookForTest(hook func(ctx context.Context) error) {
	if hook == nil {
		supersedeDebtInterleaveHook = nil
		return
	}
	supersedeDebtInterleaveHook = func(ctx context.Context, stage string) error {
		if stage != supersedeDebtStageBeforeAdvance {
			return nil
		}
		return hook(ctx)
	}
}
