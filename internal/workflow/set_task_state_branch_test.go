package workflow

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// TestSetTaskStateBranchConflict pins the fix for the reported
// "UNIQUE constraint failed: tasks.repo_full_name, tasks.branch" during workflow
// advancement. When advancement calls setTaskState with a ref whose branch is already
// owned by a DIFFERENT task (a fresh task id re-running a phase on the same branch after
// a transient failure), it must advance the branch's canonical task in place rather than
// crash on the tasks(repo_full_name, branch) partial-unique index.
func TestSetTaskStateBranchConflict(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	e := Engine{Store: store}

	const repo = "owner/repo"
	const branch = "council/slug-v1"

	// Seed the branch's canonical task.
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: repo, GoalID: "g1", Title: "impl", State: string(TaskImplementing), Branch: branch}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Advancement with a DIFFERENT (fresh) id on the same branch must NOT crash.
	if err := e.setTaskState(ctx, taskRef{ID: "task-fresh", Repo: repo, GoalID: "g1", Title: "review", Branch: branch}, TaskReviewing); err != nil {
		t.Fatalf("setTaskState onto a branch owned by another task must not error, got: %v", err)
	}

	// The branch's canonical task advanced in place; no duplicate was created.
	got, err := store.GetTaskByRepoBranch(ctx, repo, branch)
	if err != nil {
		t.Fatalf("GetTaskByRepoBranch: %v", err)
	}
	if got.ID != "task-a" {
		t.Errorf("branch task id = %q, want task-a (canonical id preserved)", got.ID)
	}
	if got.State != string(TaskReviewing) {
		t.Errorf("branch task state = %q, want %q (advanced in place)", got.State, string(TaskReviewing))
	}
	if _, err := store.GetTask(ctx, "task-fresh"); err == nil {
		t.Errorf("GetTask(task-fresh) unexpectedly succeeded; no duplicate should be created on the taken branch")
	}

	// Sanity: the normal same-id path still advances the task directly.
	if err := e.setTaskState(ctx, taskRef{ID: "task-a", Repo: repo, GoalID: "g1", Title: "impl", Branch: branch}, TaskBlocked); err != nil {
		t.Fatalf("setTaskState same-id: %v", err)
	}
	if got, _ := store.GetTask(ctx, "task-a"); got.State != string(TaskBlocked) {
		t.Errorf("same-id advance state = %q, want %q", got.State, string(TaskBlocked))
	}
}
