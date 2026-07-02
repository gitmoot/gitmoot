package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
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
