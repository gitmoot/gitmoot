package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestEngineStartTaskBranchCreatesBranchAndLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	brancher := &fakeBranchCreator{}

	task, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:       "jerryfane/gitmoot",
		GoalID:     "goal-1",
		TaskID:     "task-8",
		TaskTitle:  "Branch Rules",
		Branch:     "task-8",
		BaseBranch: "main",
		Owner:      "lead",
	}, brancher)

	if err != nil {
		t.Fatalf("StartTaskBranch returned error: %v", err)
	}
	if task.ID != "task-8" || task.Branch != "task-8" || task.State != string(TaskImplementing) {
		t.Fatalf("task = %+v", task)
	}
	lock, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-8")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.Owner != "lead" {
		t.Fatalf("lock owner = %q, want lead", lock.Owner)
	}
	if len(brancher.calls) != 1 || brancher.calls[0].branch != "task-8" || brancher.calls[0].base != "main" {
		t.Fatalf("branch calls = %+v", brancher.calls)
	}
}

func TestEngineStartTaskBranchReleasesLockOnBranchCreateFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	brancher := &fakeBranchCreator{err: errors.New("git failed")}

	_, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, brancher)

	if err == nil {
		t.Fatal("StartTaskBranch succeeded despite branch failure")
	}
	if _, lockErr := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-8"); !errors.Is(lockErr, sql.ErrNoRows) {
		t.Fatalf("lock after failure error = %v, want sql.ErrNoRows", lockErr)
	}
}

func TestEngineStartTaskBranchPreservesExistingTaskMetadata(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-8",
		RepoFullName: "jerryfane/gitmoot",
		GoalID:       "goal-1",
		Title:        "Branch Rules",
		State:        string(TaskPlanned),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	engine := testEngine(store)

	task, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, &fakeBranchCreator{})

	if err != nil {
		t.Fatalf("StartTaskBranch returned error: %v", err)
	}
	if task.GoalID != "goal-1" || task.Title != "Branch Rules" {
		t.Fatalf("task metadata = goal %q title %q", task.GoalID, task.Title)
	}
}

func TestEngineStartTaskBranchPreservesExistingSameOwnerLockOnFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-8", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	brancher := &fakeBranchCreator{err: errors.New("git failed")}

	_, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, brancher)

	if err == nil {
		t.Fatal("StartTaskBranch succeeded despite branch failure")
	}
	lock, lockErr := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-8")
	if lockErr != nil {
		t.Fatalf("GetBranchLock returned error: %v", lockErr)
	}
	if lock.Owner != "lead" {
		t.Fatalf("lock owner = %q, want lead", lock.Owner)
	}
}

func TestEngineStartTaskBranchBlocksWhenBranchLocked(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-8", Owner: "other"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}

	_, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, &fakeBranchCreator{})

	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want BlockedError", err)
	}
}

func TestEngineStartTaskBranchRejectsBranchAssignedToOtherTask(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-existing", GoalID: "goal-1", Title: "Existing", State: string(TaskPlanned), Branch: "task-8"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	brancher := &fakeBranchCreator{}

	_, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, brancher)

	if err == nil || !strings.Contains(err.Error(), "another task") {
		t.Fatalf("error = %v, want branch assignment error", err)
	}
	if len(brancher.calls) != 0 {
		t.Fatalf("branch was created despite assignment conflict: %+v", brancher.calls)
	}
}

func TestEngineStartTaskBranchAllowsSameBranchInAnotherRepo(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-existing", RepoFullName: "jerryfane/other", GoalID: "goal-1", Title: "Existing", State: string(TaskPlanned), Branch: "task-8"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	engine := testEngine(store)
	brancher := &fakeBranchCreator{}

	task, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:      "jerryfane/gitmoot",
		GoalID:    "goal-1",
		TaskID:    "task-8",
		TaskTitle: "Task 8",
		Branch:    "task-8",
		Owner:     "lead",
	}, brancher)
	if err != nil {
		t.Fatalf("StartTaskBranch returned error: %v", err)
	}
	if task.RepoFullName != "jerryfane/gitmoot" {
		t.Fatalf("task repo = %q, want jerryfane/gitmoot", task.RepoFullName)
	}
}

type fakeBranchCreator struct {
	err   error
	calls []branchCall
}

type branchCall struct {
	branch string
	base   string
}

func (f *fakeBranchCreator) CreateBranch(_ context.Context, branch string, base string) error {
	f.calls = append(f.calls, branchCall{branch: branch, base: base})
	return f.err
}
