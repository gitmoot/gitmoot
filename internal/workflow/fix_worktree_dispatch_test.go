package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestReviewFixAllocationFailureRefusesDispatch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.FixWorktreeAllocator = func(context.Context, FixWorktreeRequest) (FixWorktreeAllocation, error) {
		return FixWorktreeAllocation{}, errors.New("disk full")
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		LeadAgent: "lead", Result: &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	advanceErr := engine.AdvanceJob(ctx, "review-job")
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "implement" {
			t.Fatalf("allocation failure enqueued implement job %s", job.ID)
		}
	}
	if advanceErr == nil || advanceErr.Error() != "allocate review fix worktree: disk full" {
		t.Fatalf("AdvanceJob error = %v, want allocation refusal", advanceErr)
	}
}

func TestOrdinaryImplementDispatchDoesNotAllocateFixWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	allocations := 0
	engine.FixWorktreeAllocator = func(context.Context, FixWorktreeRequest) (FixWorktreeAllocation, error) {
		allocations++
		return FixWorktreeAllocation{Path: "/wrong"}, nil
	}
	err := engine.dispatch(ctx, JobRequest{
		ID: "ordinary-implement", Agent: "lead", Action: "implement", Repo: "gitmoot/gitmoot",
		Branch: "task-ordinary", TaskID: "task-ordinary", LeadAgent: "lead",
	}, taskRef{ID: "task-ordinary"})
	if err != nil {
		t.Fatalf("dispatch ordinary implement: %v", err)
	}
	if allocations != 0 {
		t.Fatalf("fix allocator calls = %d, want 0 for ordinary implement dispatch", allocations)
	}
	job := mustJob(t, store, "ordinary-implement")
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.FixWorktree || payload.WorktreePath != "" {
		t.Fatalf("ordinary implement payload gained fix isolation: %+v", payload)
	}
}
