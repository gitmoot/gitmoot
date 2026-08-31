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
	retryEvent := db.JobEvent{
		JobID:   job.ID,
		Kind:    "retry_queued",
		Message: fmt.Sprintf("retry requested from %s", job.State),
	}
	transitioned := false
	if task, ok, err := dismissedTaskForJob(ctx, store, payload); err != nil {
		return db.Job{}, err
	} else if ok {
		taskState := string(TaskPlanned)
		if job.Type == "implement" && strings.TrimSpace(payload.Branch) != "" && strings.TrimSpace(payload.WorktreePath) != "" {
			taskState = string(TaskImplementing)
		}
		transitioned, err = store.TransitionJobStatePayloadWithEventAndTaskTransition(ctx,
			job.ID, job.State, string(JobQueued), encoded, retryEvent,
			task.ID, string(TaskDismissed), taskState, "task_recovered_job_retry",
			fmt.Sprintf("restored dismissed task before retrying job %s", job.ID))
	} else {
		transitioned, err = store.TransitionJobStatePayloadWithEvent(ctx, job.ID, job.State, string(JobQueued), encoded, retryEvent)
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
	_, _ = store.DeleteResourceLocksByOwner(ctx, job.ID)
	payload, perr := unmarshalPayload(job.Payload)
	if perr != nil {
		return
	}
	// An implement delegation leg that dies here never runs the engine's terminal
	// cleanupImplementDelegationWorktree (that fires from AdvanceJob), so its
	// per-delegation branch lock would leak exactly like the success path did before
	// #617. Gated to worktree-isolated implement legs.
	if released, rerr := releaseDelegationBranchLock(ctx, store, job.Type, payload); rerr == nil && released {
		_ = store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_branch_lock_released",
			Message: fmt.Sprintf("released delegation branch lock %s on %s (#617)", strings.TrimSpace(payload.Branch), cause.verb),
		})
	}
	// A top-level task implement owns the task lane rather than an ephemeral
	// delegation lane. Task dismissal stays with the stale-task reconciler, which
	// owns remote cleanup and the audit event; this excludes only this exact
	// implementing task from the atomic release check, and every other task, every
	// unknown/review state and every non-terminal job still vetoes.
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
	// Dispose a #739 dispatch-time read-only worktree that dying before running
	// would otherwise leak.
	recordReadOnlyWorktreeReclaimOnAbort(ctx, store, job, payload)
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
// The pending marker's message CARRIES THE LIFECYCLE GENERATION it was written
// for, because the debt is about one run and `gitmoot job retry` can start
// another. Paying an old PR-closed debt against a newer lifecycle would stamp
// that run with the old synthetic failure and advance the parent on it, and a
// newer run that SUCCEEDS would leave the marker outstanding forever. A debt
// whose generation no longer matches is therefore VOIDED, never paid.
const (
	JobEventSupersedeFinalizePending   = "supersede_finalize_pending"
	JobEventSupersedeFinalizeCompleted = "supersede_finalize_completed"

	supersedeFinalizeGenerationPrefix = "generation="
)

// formatSupersedeFinalizeDebt and parseSupersedeFinalizeDebt are the marker's
// wire format. db.JobEvent carries no numeric column, so the generation rides in
// the message behind a fixed prefix; the reason follows it verbatim so the
// retry's synthetic result reads exactly as the original attempt's would have.
func formatSupersedeFinalizeDebt(generation int64, reason string) string {
	return fmt.Sprintf("%s%d: %s", supersedeFinalizeGenerationPrefix, generation, reason)
}

func parseSupersedeFinalizeDebt(message string) (int64, string, bool) {
	if !strings.HasPrefix(message, supersedeFinalizeGenerationPrefix) {
		return 0, message, false
	}
	rest := message[len(supersedeFinalizeGenerationPrefix):]
	separator := strings.Index(rest, ": ")
	if separator < 0 {
		return 0, message, false
	}
	generation, err := strconv.ParseInt(rest[:separator], 10, 64)
	if err != nil {
		return 0, message, false
	}
	return generation, rest[separator+2:], true
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
		Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration, reason),
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
		Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration, reason),
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
		supersedeDebtInterleaveHook(ctx, supersedeDebtStageAfterRead)
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
// own re-read (which gates the UNANCHORED cleanups), and the anchored payload
// write inside the finalizer.
const (
	supersedeDebtStageAfterRead      = "after-read"
	supersedeDebtStageAfterClaim     = "after-claim"
	supersedeDebtStageBeforeFinalize = "before-finalize"
)

var supersedeDebtInterleaveHook func(ctx context.Context, stage string)

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
// reason and generation come from the marker the sweep actually wrote.
func latestSupersedeFinalizeDebt(ctx context.Context, store *db.Store, jobID string) (supersedeFinalizeDebt, error) {
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		return supersedeFinalizeDebt{}, err
	}
	debt := supersedeFinalizeDebt{}
	for _, event := range events {
		switch event.Kind {
		case JobEventSupersedeFinalizePending:
			generation, reason, anchored := parseSupersedeFinalizeDebt(event.Message)
			debt = supersedeFinalizeDebt{pending: true, anchored: anchored, generation: generation, reason: reason}
		case JobEventSupersedeFinalizeCompleted:
			debt = supersedeFinalizeDebt{}
		}
	}
	if debt.pending && strings.TrimSpace(debt.reason) == "" {
		debt.reason = "queued job superseded: its pull request is no longer open"
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
	return store.AddJobEvent(ctx, db.JobEvent{
		JobID: job.ID,
		Kind:  JobEventSupersedeFinalizeCompleted,
		Message: fmt.Sprintf("voided without action: the debt was incurred by %s, and the job is now %s at generation %d",
			detail, job.State, job.LifecycleGeneration),
	})
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
		supersedeDebtInterleaveHook(ctx, supersedeDebtStageAfterClaim)
	}
	current, err := store.GetJob(ctx, job.ID)
	if err != nil {
		return err
	}
	if current.LifecycleGeneration != generation || !isSupersededTerminalState(current.State) {
		return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, nil)
	}
	// A queued job dying here owes the SAME cleanups a cancel does — resource locks,
	// a per-delegation branch lock, the task lane lock, a dispatch-time read-only
	// worktree. Copying only the worktree half (the shape SupersedeStaleHeadJob
	// carries, where the targeted review legs hold none of the others) would leak
	// locks that block the next same-repo work.
	releaseAbortedJobResources(ctx, store, current, abortCauseSupersede)
	if engine == nil {
		return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, nil)
	}
	payload, perr := unmarshalPayload(current.Payload)
	if perr != nil || strings.TrimSpace(payload.ParentJobID) == "" {
		return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, nil)
	}
	if supersedeDebtInterleaveHook != nil {
		supersedeDebtInterleaveHook(ctx, supersedeDebtStageBeforeFinalize)
	}
	finalized, finalizeErr := engine.finalizeSupersededDelegationChildAtGeneration(ctx, job.ID, reason, generation)
	if finalizeErr != nil && !isDelegationPolicyOutcome(finalizeErr) {
		return finalizeErr
	}
	if !finalized && finalizeErr == nil {
		// The finalizer returns this both when the result is ALREADY stamped (a
		// previous attempt got that far and then failed) and when it refused because
		// the row moved to another lifecycle. Only the first owes a parent advance,
		// and advancing the coordinator on a run that is no longer this one is the
		// same defect the anchor exists to stop — so re-read and require the anchored
		// lifecycle before touching the parent.
		latest, err := store.GetJob(ctx, job.ID)
		if err != nil {
			return err
		}
		if latest.LifecycleGeneration != generation || !isSupersededTerminalState(latest.State) {
			return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, nil)
		}
		// AdvanceJob is re-entrant (the post-delivery retry pass re-calls it on the
		// same job), so driving it again on an already-advanced parent is a no-op.
		if err := engine.AdvanceJob(ctx, job.ID); err != nil {
			if !isDelegationPolicyOutcome(err) {
				return err
			}
			finalizeErr = err
		}
	}
	return recordSupersedeFinalizationCompleted(ctx, store, job.ID, reason, generation, finalizeErr)
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
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    JobEventSupersedeFinalizeCompleted,
		Message: reason,
	}); err != nil {
		return err
	}
	return outcome
}
