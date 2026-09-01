package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestReviewFixAllocationFailureRefusesDispatch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	enableAutoFix(t, store, 7)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.FixWorktreeAllocator = func(context.Context, FixWorktreeRequest) (FixWorktreeAllocation, error) {
		return FixWorktreeAllocation{}, errors.New("disk full")
	}
	insertCompletedJob(t, store, db.Job{ID: "original-implement", Agent: "lead", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		Result: &AgentResult{Decision: "implemented"},
	})
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
		if job.Type == "implement" && job.ID != "original-implement" {
			t.Fatalf("allocation failure enqueued auto-fix implement job %s", job.ID)
		}
	}
	if advanceErr == nil || advanceErr.Error() != "allocate review fix worktree: disk full" {
		t.Fatalf("AdvanceJob error = %v, want allocation refusal", advanceErr)
	}
}

// This enters through review advancement and forces the enqueue itself to fail
// after allocation. Replacing SetAsideFixClone with os.RemoveAll loses the only
// copy of owned.txt and fails the survivor assertion.
func TestReviewFixEnqueueFailureSetsCloneAside(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	enableAutoFix(t, store, 7)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "original-implement", Agent: "lead", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		Result: &AgentResult{Decision: "implemented"},
	})
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		LeadAgent: "lead", Result: &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	engine := testEngine(store)
	var clone string
	engine.FixWorktreeAllocator = func(_ context.Context, request FixWorktreeRequest) (FixWorktreeAllocation, error) {
		clone = filepath.Join(t.TempDir(), request.JobID)
		if err := os.MkdirAll(clone, 0o755); err != nil {
			return FixWorktreeAllocation{}, err
		}
		if err := os.WriteFile(filepath.Join(clone, "owned.txt"), []byte("only copy\n"), 0o600); err != nil {
			return FixWorktreeAllocation{}, err
		}
		conflictPayload, err := marshalPayload(JobPayload{Repo: "other/repo"})
		if err != nil {
			return FixWorktreeAllocation{}, err
		}
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: request.JobID, Agent: "other", Type: "ask", State: string(JobQueued), Payload: conflictPayload,
		}, db.JobEvent{Kind: string(JobQueued), Message: "conflicting row"}); err != nil {
			return FixWorktreeAllocation{}, err
		}
		return FixWorktreeAllocation{Path: clone, Created: true}, nil
	}

	if err := engine.AdvanceJob(ctx, "review-job"); err == nil {
		t.Fatal("AdvanceJob succeeded despite conflicting enqueue row")
	}
	if _, err := os.Lstat(clone); !os.IsNotExist(err) {
		t.Fatalf("managed clone path still exists after set-aside: %v", err)
	}
	survivors, err := FixCloneQuarantines(clone)
	if err != nil {
		t.Fatalf("FixCloneQuarantines: %v", err)
	}
	if len(survivors) != 1 {
		t.Fatalf("survivors = %v, want one preserved clone", survivors)
	}
	if got, err := os.ReadFile(filepath.Join(survivors[0], "owned.txt")); err != nil || string(got) != "only copy\n" {
		t.Fatalf("set-aside clone bytes = %q, %v", got, err)
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
