package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestRetryJobRequeuesTerminalJobAndPreservesPayload(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	payload := `{"repo":"owner/repo","raw_outputs":["raw"],"result":{"decision":"approved","summary":"stale"}}`
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
	if storedPayload.Result != nil || len(storedPayload.RawOutputs) != 1 || storedPayload.RawOutputs[0] != "raw" {
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
