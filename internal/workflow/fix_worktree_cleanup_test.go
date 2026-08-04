package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestCleanupFixWorktreeRemovesCloneWithoutDeletingTaskBranchLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := FixWorktreePath(home, "owner/repo", "fix-job")
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "owned.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "feature/fix", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock = %v, %v", acquired, err)
	}
	engine := testEngine(store)
	engine.Home = home
	engine.cleanupFixWorktree(ctx, "fix-job", "implement", JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", WorktreePath: path, FixWorktree: true,
	})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fix worktree still exists after cleanup: %v", err)
	}
	if _, err := store.GetBranchLock(ctx, "owner/repo", "feature/fix"); err != nil {
		t.Fatalf("task branch lock was removed with independent clone: %v", err)
	}
	if got := countJobEvents(t, store, "fix-job", "delegation_worktree_removed"); got != 1 {
		t.Fatalf("delegation_worktree_removed events = %d, want 1", got)
	}
}

func TestAdvanceFixPreservesCloneWhenFinalizerFails(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := FixWorktreePath(home, "owner/repo", "fix-job")
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "review-pr-7", RepoFullName: "owner/repo", Branch: "feature/fix", State: string(TaskImplementing)}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "fix-job", Agent: "lead", Type: "implement"}, JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", TaskID: "review-pr-7",
		WorktreePath: path, FixWorktree: true,
		Result: &AgentResult{Decision: "implemented", Summary: "fixed"},
	})
	engine := testEngine(store)
	engine.Home = home
	engine.ImplementationFinalizer = fakeImplementationFinalizer{err: errors.New("push failed")}
	if err := engine.AdvanceJob(ctx, "fix-job"); err == nil || err.Error() != "push failed" {
		t.Fatalf("AdvanceJob error = %v, want push failed", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fix clone was removed after resumable finalizer failure: %v", err)
	}
}
