package cli

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestHandleRunJobErrorTimedOutDelegationChildTriggersRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := seedDelegationCoordinator(t, store, "parent-job", []workflow.Delegation{
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api", Retry: 1},
	})

	childID := "parent-job/delegation/api"
	markDelegationChildTimedOut(t, store, childID)
	child, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(child) returned error: %v", err)
	}

	// The daemon's run-error path must turn the timeout kill into a terminal
	// failed child AND drive the parent DAG so the delegation's retry fires.
	if err := worker.handleRunJobError(ctx, childID, context.DeadlineExceeded); err != nil {
		t.Fatalf("handleRunJobError returned error: %v", err)
	}

	// The timed-out child is now terminal failed (not stranded in running).
	finalized, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(child after) returned error: %v", err)
	}
	if finalized.State != string(workflow.JobFailed) {
		t.Fatalf("timed-out child state = %q, want failed", finalized.State)
	}

	// Retry budget (Retry:1) is consumed by the timeout: a retry job is enqueued.
	retry, err := store.GetJob(ctx, "parent-job/delegation/api/retry/1")
	if err != nil {
		t.Fatalf("retry job not enqueued after timeout: %v", err)
	}
	if retry.State != string(workflow.JobQueued) || retry.DelegationID != "api" {
		t.Fatalf("retry job = %+v, want queued review of delegation api", retry)
	}
	events, err := store.ListJobEvents(ctx, "parent-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "delegation_retry") {
		t.Fatalf("parent events = %+v, want delegation_retry", events)
	}
	_ = child
}

func TestHandleRunJobErrorTimedOutDelegationChildBlocksParentWhenNoRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := seedDelegationCoordinator(t, store, "parent-job", []workflow.Delegation{
		// Retry:0 + default block_parent failure_policy: a timeout must block the
		// shared parent task, not strand the child.
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
	})

	childID := "parent-job/delegation/api"
	markDelegationChildTimedOut(t, store, childID)

	// handleRunJobError swallows the BlockedError (the child is finalized and the
	// DAG advanced), returning nil for a clean terminal outcome.
	if err := worker.handleRunJobError(ctx, childID, context.DeadlineExceeded); err != nil {
		t.Fatalf("handleRunJobError returned error: %v", err)
	}

	finalized, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(child) returned error: %v", err)
	}
	if finalized.State != string(workflow.JobFailed) {
		t.Fatalf("timed-out child state = %q, want failed", finalized.State)
	}
	// No retry: the failure_policy (block_parent) fired on the shared task.
	if _, err := store.GetJob(ctx, "parent-job/delegation/api/retry/1"); err == nil {
		t.Fatal("no retry job should be enqueued when Retry=0")
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked by block_parent failure_policy", task.State)
	}
}

func TestHandleRunJobErrorTimedOutDelegationChildEnqueuesContinuationOnContinuePolicy(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := seedDelegationCoordinator(t, store, "parent-job", []workflow.Delegation{
		// continue failure_policy: a timed-out child must resolve the delegation so
		// the coordinator continuation is enqueued once every sibling is terminal.
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "continue"},
	})

	childID := "parent-job/delegation/api"
	markDelegationChildTimedOut(t, store, childID)

	if err := worker.handleRunJobError(ctx, childID, context.DeadlineExceeded); err != nil {
		t.Fatalf("handleRunJobError returned error: %v", err)
	}

	// The coordinator continuation job is enqueued (the timeout failure resolved
	// the only delegation under the continue policy).
	if _, err := store.GetJob(ctx, "parent-job/continuation"); err != nil {
		t.Fatalf("continuation job not enqueued after timed-out child under continue policy: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "parent-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "delegation_continuation_enqueued") {
		t.Fatalf("parent events = %+v, want delegation_continuation_enqueued", events)
	}
}
