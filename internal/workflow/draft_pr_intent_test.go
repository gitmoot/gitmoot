package workflow

import (
	"context"
	"testing"
)

func TestEnqueuePersistsPullRequestReadyOptOut(t *testing.T) {
	store := openEngineStore(t)
	seedAgent(t, store, "builder", []string{"implement"}, "owner/repo")
	job, err := (Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}).Enqueue(context.Background(), JobRequest{
		ID: "implement-ready", Agent: "builder", Action: "implement", Repo: "owner/repo",
		Branch: "task-ready", TaskID: "task-ready", Instructions: "implement",
		PullRequestReady: true,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if !payload.PullRequestReady {
		t.Fatalf("payload dropped explicit ready opt-out: %+v", payload)
	}
}
