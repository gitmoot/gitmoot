package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

const JobEventSupersededStaleHead = "superseded_stale_head"

func RetryJob(ctx context.Context, store *db.Store, jobID string) (db.Job, error) {
	if store == nil {
		return db.Job{}, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, err
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

func CancelJob(ctx context.Context, store *db.Store, jobID string) (db.Job, error) {
	if store == nil {
		return db.Job{}, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, err
	}
	switch job.State {
	case string(JobQueued), string(JobRunning):
	default:
		return db.Job{}, fmt.Errorf("job %s is %s; cancel requires queued or running", job.ID, job.State)
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
		return db.Job{}, fmt.Errorf("job %s is %s; cancel requires queued or running", latest.ID, latest.State)
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
	updated, err := store.GetJob(ctx, job.ID)
	return updated, true, err
}
