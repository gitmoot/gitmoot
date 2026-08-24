package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestRetryJobRequeuesTerminalJobAndPreservesPayload(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	payload := `{"repo":"owner/repo","template_id":"thermo","template_resolved_commit":"abc123","template_content":"Review deeply.","raw_outputs":["raw"],"result":{"decision":"approved","summary":"stale"}}`
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "ask", State: string(JobFailed), Payload: payload}, db.JobEvent{
		Kind:    string(JobFailed),
		Message: "failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	job, err := RetryJob(ctx, store, "job-1")
	if err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}

	if job.State != string(JobQueued) {
		t.Fatalf("job after retry = %+v", job)
	}
	storedPayload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if storedPayload.Result != nil || len(storedPayload.RawOutputs) != 1 || storedPayload.RawOutputs[0] != "raw" ||
		storedPayload.TemplateID != "thermo" || storedPayload.TemplateResolvedCommit != "abc123" || storedPayload.TemplateContent != "Review deeply." {
		t.Fatalf("payload after retry = %+v, want stale result cleared and raw output preserved", storedPayload)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 2 || events[1].Kind != "retry_queued" || !strings.Contains(events[1].Message, "failed") {
		t.Fatalf("events = %+v, want retry event preserving prior events", events)
	}
}

// TestRetryJobClearsOperationalBlockerContext proves a human-requested retry is
// a fresh lifecycle for the #532 machinery: (a) a cancel→retry of a held job
// must not silently re-enter the old hold (a stale blocker_retry_at hours out
// would park the retried job with a contradictory #552 stuck reason), and (b) a
// post-exhaustion retry must regain the full deferral budget instead of
// terminally failing on its first ordinary transient 429.
func TestRetryJobClearsOperationalBlockerContext(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	hold := time.Now().UTC().Add(6 * time.Hour).Format(time.RFC3339Nano)
	payload := `{"repo":"owner/repo","branch":"main","blocker_class":"runtime_quota","blocker_attempts":3,"blocker_retry_at":"` + hold + `"}`
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-held", Agent: "audit", Type: "ask", State: string(JobFailed), Payload: payload}, db.JobEvent{
		Kind:    string(JobFailed),
		Message: "operational blocker exhausted",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	job, err := RetryJob(ctx, store, "job-held")
	if err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	if job.State != string(JobQueued) {
		t.Fatalf("job after retry = %+v", job)
	}
	stored, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if stored.BlockerClass != "" || stored.BlockerAttempts != 0 || stored.BlockerRetryAt != "" {
		t.Fatalf("payload after manual retry still carries blocker context: %+v", stored)
	}
}

func TestRetryJobClearsReadOnlyNoTaskWorktreePath(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	payload := `{"repo":"owner/repo","branch":"main","delegation_id":"plan-glm","worktree_path":"/tmp/gitmoot/worktrees/owner--repo/delegations/root/plan-glm/pool-isolation","head_sha":"abc123","result":{"decision":"failed","summary":"stale"}}`
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "root/delegation/plan-glm", Agent: "council-glm", Type: "ask", State: string(JobFailed), Payload: payload}, db.JobEvent{
		Kind:    string(JobFailed),
		Message: "failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	job, err := RetryJob(ctx, store, "root/delegation/plan-glm")
	if err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}

	storedPayload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if storedPayload.WorktreePath != "" {
		t.Fatalf("manual read-only retry kept stale WorktreePath = %q", storedPayload.WorktreePath)
	}
	if storedPayload.HeadSHA != "" {
		t.Fatalf("manual read-only retry kept stale HeadSHA = %q", storedPayload.HeadSHA)
	}
}

func TestRetryJobPreservesTaskWorktreePath(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	payload := `{"repo":"owner/repo","branch":"feature","task_id":"task-1","delegation_id":"implement","worktree_path":"/tmp/gitmoot/worktrees/owner--repo/task-1","head_sha":"abc123","result":{"decision":"failed","summary":"stale"}}`
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "root/delegation/implement", Agent: "builder", Type: "implement", State: string(JobFailed), Payload: payload}, db.JobEvent{
		Kind:    string(JobFailed),
		Message: "failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	job, err := RetryJob(ctx, store, "root/delegation/implement")
	if err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}

	storedPayload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if storedPayload.WorktreePath != "/tmp/gitmoot/worktrees/owner--repo/task-1" {
		t.Fatalf("task retry WorktreePath = %q, want preserved task worktree", storedPayload.WorktreePath)
	}
	if storedPayload.HeadSHA != "abc123" {
		t.Fatalf("task retry HeadSHA = %q, want preserved", storedPayload.HeadSHA)
	}
}

func TestRetryJobRecoversDismissedTaskAtomically(t *testing.T) {
	tests := []struct {
		name      string
		jobType   string
		payload   string
		wantState TaskState
	}{
		{name: "implement artifacts", jobType: "implement", payload: `{"repo":"owner/repo","branch":"feature","task_id":"task-1","worktree_path":"/tmp/task-1"}`, wantState: TaskImplementing},
		{name: "non implement", jobType: "review", payload: `{"repo":"owner/repo","branch":"feature","task_id":"task-1"}`, wantState: TaskPlanned},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", State: string(TaskDismissed), Branch: "feature", WorktreePath: "/tmp/task-1"}); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Type: test.jobType, State: string(JobFailed), Payload: test.payload}, db.JobEvent{Kind: string(JobFailed)}); err != nil {
				t.Fatal(err)
			}
			job, err := RetryJob(ctx, store, "job-1")
			if err != nil || job.State != string(JobQueued) {
				t.Fatalf("RetryJob job=%+v err=%v", job, err)
			}
			task, _ := store.GetTask(ctx, "task-1")
			events, _ := store.ListTaskEvents(ctx, "task-1")
			if task.State != string(test.wantState) || len(events) != 1 || events[0].Kind != "task_recovered_job_retry" || events[0].FromState != string(TaskDismissed) || events[0].ToState != string(test.wantState) {
				t.Fatalf("task=%+v events=%+v", task, events)
			}
		})
	}
}

func TestRetryJobRefusalLeavesDismissedTaskUntouched(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", State: string(TaskDismissed), Branch: "feature"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Type: "implement", State: string(JobCancelled), Payload: `{"repo":"owner/repo","branch":"feature","task_id":"task-1","worktree_path":"/tmp/task-1"}`}, db.JobEvent{Kind: string(JobCancelled), Message: "cancel requested from running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RetryJob(ctx, store, "job-1"); err == nil {
		t.Fatal("RetryJob accepted unsettled running cancellation")
	}
	task, _ := store.GetTask(ctx, "task-1")
	events, _ := store.ListTaskEvents(ctx, "task-1")
	if task.State != string(TaskDismissed) || len(events) != 0 {
		t.Fatalf("task=%+v events=%+v", task, events)
	}
}

func TestRetryJobRejectsNonTerminalJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "ask", State: string(JobQueued)}, db.JobEvent{Kind: string(JobQueued), Message: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if _, err := RetryJob(ctx, store, "job-1"); err == nil {
		t.Fatal("RetryJob accepted queued job")
	}
}

func TestRetryJobAllowsQueuedCancellationButRejectsRunningCancellation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for _, jobID := range []string{"queued-cancel", "running-cancel"} {
		if err := store.CreateJobWithEvent(ctx, db.Job{ID: jobID, Agent: "audit", Type: "ask", State: string(JobCancelled), Payload: `{"repo":"owner/repo"}`}, db.JobEvent{
			Kind:    string(JobCancelled),
			Message: "cancel requested from " + strings.TrimSuffix(jobID, "-cancel"),
		}); err != nil {
			t.Fatalf("CreateJobWithEvent %s returned error: %v", jobID, err)
		}
	}
	if _, err := RetryJob(ctx, store, "queued-cancel"); err != nil {
		t.Fatalf("RetryJob rejected queued cancellation: %v", err)
	}
	if _, err := RetryJob(ctx, store, "running-cancel"); err == nil {
		t.Fatal("RetryJob accepted running cancellation")
	}
}

func TestRetryJobAllowsRunningCancellationAfterWorkerSettles(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "ask", State: string(JobCancelled), Payload: `{"repo":"owner/repo"}`}, db.JobEvent{
		Kind:    string(JobCancelled),
		Message: "cancel requested from running",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-1", Kind: "cancel_settled", Message: "cancelled job worker settled"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	if _, err := RetryJob(ctx, store, "job-1"); err != nil {
		t.Fatalf("RetryJob rejected settled running cancellation: %v", err)
	}
}

func TestRetryJobRejectsRunningSupersededReviewUntilWorkerSettles(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "review", State: string(JobRunning), Payload: `{"repo":"owner/repo"}`}, db.JobEvent{
		Kind:    string(JobRunning),
		Message: "running",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	job, transitioned, err := SupersedeStaleHeadJob(ctx, store, "job-1", "review job superseded_stale_head: PR #1 moved from head \"old\" to \"new\"")
	if err != nil {
		t.Fatalf("SupersedeStaleHeadJob returned error: %v", err)
	}
	if !transitioned || job.State != string(JobCancelled) {
		t.Fatalf("superseded job transitioned=%v state=%q, want cancelled transition", transitioned, job.State)
	}
	if _, err := RetryJob(ctx, store, "job-1"); err == nil {
		t.Fatal("RetryJob accepted running superseded review before worker settled")
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) < 2 || events[1].Kind != JobEventSupersededStaleHead || !strings.HasPrefix(events[1].Message, "cancel requested from running") {
		t.Fatalf("events = %+v, want running supersede marker", events)
	}
	if _, err := SettleCancelledRunningJob(ctx, store, "job-1", "cancelled job worker settled"); err != nil {
		t.Fatalf("SettleCancelledRunningJob returned error: %v", err)
	}
	if _, err := RetryJob(ctx, store, "job-1"); err != nil {
		t.Fatalf("RetryJob rejected settled running superseded review: %v", err)
	}
}

func TestCancelJobCancelsQueuedOrRunningJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "ask", State: string(JobRunning)}, db.JobEvent{Kind: string(JobRunning), Message: "running"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	job, err := CancelJob(ctx, store, "job-1")
	if err != nil {
		t.Fatalf("CancelJob returned error: %v", err)
	}

	if job.State != string(JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 2 || events[1].Kind != string(JobCancelled) {
		t.Fatalf("events = %+v, want cancellation event", events)
	}
}

func TestCancelJobReleasesRuntimeSessionLock(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "ask", State: string(JobRunning)}, db.JobEvent{Kind: string(JobRunning), Message: "running"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	const lockKey = "runtime:codex:session-1"
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  "job-1",
		OwnerToken:  "token-1",
		ExpiresAt:   now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not acquire the runtime-session lock")
	}

	job, err := CancelJob(ctx, store, "job-1")
	if err != nil {
		t.Fatalf("CancelJob returned error: %v", err)
	}
	if job.State != string(JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}

	if _, err := store.GetResourceLock(ctx, lockKey); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetResourceLock after cancel error = %v, want sql.ErrNoRows (lock should be released)", err)
	}

	// A different job must be able to re-acquire the freed key immediately.
	reacquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  "job-2",
		OwnerToken:  "token-2",
		ExpiresAt:   now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("second AcquireResourceLock returned error: %v", err)
	}
	if !reacquired {
		t.Fatal("second job could not re-acquire the freed runtime-session lock")
	}
}

// TestCancelJobReleasesInactiveTaskLaneLock kills the pre-fix mutant that leaves
// the production task row implementing before attempting the lane release. Once
// this top-level implement is cancelled and no other non-terminal work references
// repo+branch, the operator's cancel must immediately free the lane for a replacement writer.
func TestCancelJobReleasesInactiveTaskLaneLock(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const branch = "feature/cancelled-writer"
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-cancelled", RepoFullName: "owner/repo", State: string(TaskImplementing), Branch: branch,
	}); err != nil {
		t.Fatal(err)
	}
	acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: branch, Owner: "lead"})
	if err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	payload, err := json.Marshal(JobPayload{Repo: "owner/repo", Branch: branch, TaskID: "task-cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "job-cancelled-writer", Agent: "lead", Type: "implement", State: string(JobQueued), Repo: "owner/repo", Payload: string(payload),
	}, db.JobEvent{Kind: string(JobQueued), Message: "queued"}); err != nil {
		t.Fatal(err)
	}

	job, err := CancelJob(ctx, store, "job-cancelled-writer")
	if err != nil || job.State != string(JobCancelled) {
		t.Fatalf("CancelJob job=%+v err=%v", job, err)
	}
	if _, err := store.GetBranchLock(ctx, "owner/repo", branch); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBranchLock after cancel error = %v, want sql.ErrNoRows", err)
	}
	task, err := store.GetTask(ctx, "task-cancelled")
	if err != nil || task.State != string(TaskDismissed) {
		t.Fatalf("task after cancel = %+v, err=%v; want dismissed", task, err)
	}
	events, err := store.ListBranchLockEvents(ctx, "owner/repo", branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "released" || !strings.Contains(events[0].Message, "cancellation") {
		t.Fatalf("branch lock events = %+v, want cancellation release", events)
	}
}

// TestCancelJobTaskLaneReleaseFailsClosed kills mutants that dismiss through a
// queued successor or widen the implementing-state allowlist to review/unknown
// states. Those states must retain both task state and lane ownership.
func TestCancelJobTaskLaneReleaseFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		taskState string
		otherJob  bool
	}{
		{name: "queued successor", taskState: string(TaskImplementing), otherJob: true},
		{name: "review-owned task", taskState: string(TaskReviewing)},
		{name: "unknown task state", taskState: "future_state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			branch := "feature/cancel-veto-" + strings.ReplaceAll(test.name, " ", "-")
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-cancelled", RepoFullName: "owner/repo", State: test.taskState, Branch: branch,
			}); err != nil {
				t.Fatal(err)
			}
			acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: branch, Owner: "lead"})
			if err != nil || !acquired {
				t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
			}
			payload, err := json.Marshal(JobPayload{Repo: "owner/repo", Branch: branch, TaskID: "task-cancelled"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID: "job-cancelled", Agent: "lead", Type: "implement", State: string(JobQueued), Repo: "owner/repo", Payload: string(payload),
			}, db.JobEvent{Kind: string(JobQueued), Message: "queued"}); err != nil {
				t.Fatal(err)
			}
			if test.otherJob {
				if err := store.CreateJobWithEvent(ctx, db.Job{
					ID: "job-successor", Agent: "lead", Type: "implement", State: string(JobQueued), Repo: "owner/repo", Payload: string(payload),
				}, db.JobEvent{Kind: string(JobQueued), Message: "queued successor"}); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := CancelJob(ctx, store, "job-cancelled"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.GetBranchLock(ctx, "owner/repo", branch); err != nil {
				t.Fatalf("branch lock was released despite %s: %v", test.name, err)
			}
			stored, err := store.GetTask(ctx, "task-cancelled")
			if err != nil || stored.State != test.taskState {
				t.Fatalf("task after cancel = %+v, err=%v; want state %q retained", stored, err, test.taskState)
			}
		})
	}
}

func TestCancelJobRejectsTerminalJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "ask", State: string(JobSucceeded)}, db.JobEvent{Kind: string(JobSucceeded), Message: "succeeded"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if _, err := CancelJob(ctx, store, "job-1"); err == nil {
		t.Fatal("CancelJob accepted succeeded job")
	}
}

// TestCancelJobDismissesBlockedJob covers the #631 abandon verb: a blocked job
// (paused awaiting a human) cancels like a queued/running one — the transition
// lands, the event records "cancel requested from blocked", and the job's
// resource locks are released so a stranded gate does not hold them out.
func TestCancelJobDismissesBlockedJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "ask", State: string(JobBlocked)}, db.JobEvent{Kind: string(JobBlocked), Message: "awaiting a human"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const lockKey = "runtime:codex:session-1"
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  "job-1",
		OwnerToken:  "token-1",
		ExpiresAt:   now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not acquire the lock")
	}

	job, err := CancelJob(ctx, store, "job-1")
	if err != nil {
		t.Fatalf("CancelJob returned error: %v", err)
	}
	if job.State != string(JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 2 || events[1].Kind != string(JobCancelled) || events[1].Message != "cancel requested from blocked" {
		t.Fatalf("events = %+v, want a cancellation event recorded from blocked", events)
	}
	if _, err := store.GetResourceLock(ctx, lockKey); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetResourceLock after cancel error = %v, want sql.ErrNoRows (lock should be released)", err)
	}
}

// TestCancelJobRefusesNonCancellableStates proves the widened guard still admits
// only queued/running/blocked: the three terminal states are refused with the
// updated message.
func TestCancelJobRefusesNonCancellableStates(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for _, state := range []JobState{JobSucceeded, JobFailed, JobCancelled} {
		id := "job-" + string(state)
		if err := store.CreateJobWithEvent(ctx, db.Job{ID: id, Agent: "audit", Type: "ask", State: string(state)}, db.JobEvent{Kind: string(state), Message: string(state)}); err != nil {
			t.Fatalf("CreateJobWithEvent %s returned error: %v", state, err)
		}
		_, err := CancelJob(ctx, store, id)
		if err == nil {
			t.Fatalf("CancelJob accepted %s job", state)
		}
		if !strings.Contains(err.Error(), "cancel requires queued, running or blocked") {
			t.Fatalf("CancelJob %s error = %q, want the widened refuse message", state, err)
		}
	}
}

// TestCancelJobLostCASRaceRefuses covers the recheck at the CAS seam: several
// concurrent cancels race one blocked job that lands terminal mid-flight. Exactly
// one wins (a single cancellation event, no double-cancel); every loser — whether
// it lost the CAS (recheck path) or read the already-cancelled row (initial
// guard) — is refused with the widened message.
func TestCancelJobLostCASRaceRefuses(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "ask", State: string(JobBlocked)}, db.JobEvent{Kind: string(JobBlocked), Message: "awaiting a human"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	const racers = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		start   = make(chan struct{})
		success int
		errs    []error
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := CancelJob(ctx, store, "job-1")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			success++
		}()
	}
	close(start)
	wg.Wait()

	if success != 1 {
		t.Fatalf("concurrent cancels succeeded %d times, want exactly 1", success)
	}
	if len(errs) != racers-1 {
		t.Fatalf("got %d refusals, want %d", len(errs), racers-1)
	}
	for _, err := range errs {
		if !strings.Contains(err.Error(), "cancel requires queued, running or blocked") {
			t.Fatalf("loser error = %q, want the widened refuse message", err)
		}
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	cancelled := 0
	for _, ev := range events {
		if ev.Kind == string(JobCancelled) {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatalf("recorded %d cancellation events, want exactly 1 (no double-cancel)", cancelled)
	}
}
