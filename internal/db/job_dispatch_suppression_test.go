package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSuppressJobDispatchExcludesQueuedJobAndRetryClears(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	const payload = `{"repo":"owner/repo"}`
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "suppressed", Agent: "worker", Type: "ask", State: "queued", Payload: payload,
	}, JobEvent{Kind: "queued", Message: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	if queued, err := store.ListQueuedJobs(ctx); err != nil || len(queued) != 1 {
		t.Fatalf("initial ListQueuedJobs = %+v, err=%v; want one", queued, err)
	}

	suppressed, err := store.SuppressJobDispatchWithEvent(ctx, "suppressed", JobEvent{
		Kind: "foreground_runtime_dispatch_suppressed", Message: "runtime evidence invalid",
	})
	if err != nil || !suppressed {
		t.Fatalf("SuppressJobDispatchWithEvent = %v, %v; want true, nil", suppressed, err)
	}
	if queued, err := store.ListQueuedJobs(ctx); err != nil || len(queued) != 0 {
		t.Fatalf("suppressed ListQueuedJobs = %+v, err=%v; want none", queued, err)
	}
	if count, err := store.CountQueuedJobsForRepo(ctx, "owner/repo"); err != nil || count != 0 {
		t.Fatalf("suppressed CountQueuedJobsForRepo = %d, err=%v; want 0", count, err)
	}
	events, err := store.ListJobEvents(ctx, "suppressed")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	if len(events) != 2 || events[1].Kind != "foreground_runtime_dispatch_suppressed" {
		t.Fatalf("events = %+v, want queued then suppression audit", events)
	}

	cancelled, err := store.TransitionJobStateWithEvent(ctx, "suppressed", "queued", "cancelled", JobEvent{Kind: "cancelled", Message: "cancelled"})
	if err != nil || !cancelled {
		t.Fatalf("cancel suppressed job = %v, %v; want true, nil", cancelled, err)
	}
	retried, err := store.TransitionJobStatePayloadWithEvent(ctx, "suppressed", "cancelled", "queued", payload, JobEvent{Kind: "retry_queued", Message: "retry"})
	if err != nil || !retried {
		t.Fatalf("retry suppressed job = %v, %v; want true, nil", retried, err)
	}
	queued, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs after retry: %v", err)
	}
	if len(queued) != 1 || queued[0].ID != "suppressed" {
		t.Fatalf("ListQueuedJobs after retry = %+v, want suppression cleared", queued)
	}
}

func TestSuppressedJobTaskRecoveryRetryClearsDispatchSuppression(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	const payload = `{"repo":"owner/repo","task_id":"task-1"}`
	if err := store.UpsertTask(ctx, Task{ID: "task-1", RepoFullName: "owner/repo", State: "dismissed"}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "task-suppressed", Agent: "worker", Type: "review", State: "queued", Payload: payload,
	}, JobEvent{Kind: "queued", Message: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	if suppressed, err := store.SuppressJobDispatchWithEvent(ctx, "task-suppressed", JobEvent{Kind: "dispatch_suppressed"}); err != nil || !suppressed {
		t.Fatalf("SuppressJobDispatchWithEvent = %v, %v; want true, nil", suppressed, err)
	}
	if cancelled, err := store.TransitionJobStateWithEvent(ctx, "task-suppressed", "queued", "cancelled", JobEvent{Kind: "cancelled"}); err != nil || !cancelled {
		t.Fatalf("cancel suppressed job = %v, %v; want true, nil", cancelled, err)
	}

	retried, err := store.TransitionJobStatePayloadWithEventAndTaskTransition(
		ctx, "task-suppressed", "cancelled", "queued", payload, JobEvent{Kind: "retry_queued"},
		"task-1", "dismissed", "planned", "task_recovered_job_retry", "retry",
	)
	if err != nil || !retried {
		t.Fatalf("task recovery retry = %v, %v; want true, nil", retried, err)
	}
	queued, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs after task retry: %v", err)
	}
	if len(queued) != 1 || queued[0].ID != "task-suppressed" {
		t.Fatalf("ListQueuedJobs after task retry = %+v, want suppression cleared", queued)
	}
}
