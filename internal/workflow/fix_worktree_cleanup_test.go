package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestCleanupFixWorktreePreservesCloneAndTaskBranchLock pins the deletion
// boundary at the TERMINAL cleanup path, which is the one a site-local fix of the
// aged pass left deleting. A fix clone is a standalone object database and no
// unlink here can be conditional on the bytes a proof examined, so this path hands
// the clone to an operator instead of removing it.
//
// MUTATION PROOF: restore os.RemoveAll(path) here and this fails on the surviving
// directory and the missing handoff event.
func TestCleanupFixWorktreePreservesCloneAndTaskBranchLock(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(path, "owned.txt")); err != nil {
		t.Fatalf("terminal cleanup deleted the standalone clone: %v", err)
	}
	if _, err := store.GetBranchLock(ctx, "owner/repo", "feature/fix"); err != nil {
		t.Fatalf("task branch lock was removed with independent clone: %v", err)
	}
	if got := countJobEvents(t, store, "fix-job", "delegation_worktree_removed"); got != 0 {
		t.Fatalf("delegation_worktree_removed events = %d, want 0: nothing was removed", got)
	}
	// This path runs NO proofs, so it must record the unproved retention rather
	// than borrow the aged pass's "proved disposable" wording.
	if got := countJobEvents(t, store, "fix-job", "delegation_worktree_retained_unproved"); got != 1 {
		t.Fatalf("unproved retention events = %d, want 1", got)
	}
	if got := countJobEvents(t, store, "fix-job", "delegation_worktree_reclaimable_manual"); got != 0 {
		t.Fatalf("terminal cleanup claimed a proof it never ran: %d handoff events", got)
	}
	// The obligation stays OPEN: a clone still on disk must not read as retired.
	obligation, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID("fix-job", path))
	if err != nil {
		t.Fatalf("GetCleanupObligation: %v", err)
	}
	if obligation.State == string(db.CleanupObligationRemoved) {
		t.Fatalf("obligation marked removed while the clone is still on disk: %+v", obligation)
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

// This enters through AdvanceJob, not cleanupFixWorktree directly. Replacing the
// successful-advance cleanup with os.RemoveAll(path) must lose owned.txt and fail.
func TestAdvanceFixSuccessPreservesCloneForOperator(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := FixWorktreePath(home, "owner/repo", "fix-success")
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "owned.txt"), []byte("committed fix\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "review-pr-8", RepoFullName: "owner/repo", Branch: "feature/fix", State: string(TaskImplementing)}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	payload := JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", TaskID: "review-pr-8",
		WorktreePath: path, FixWorktree: true,
		Result: &AgentResult{Decision: "implemented", Summary: "fixed"},
	}
	insertCompletedJob(t, store, db.Job{ID: "fix-success", Agent: "lead", Type: "implement"}, payload)
	engine := testEngine(store)
	engine.Home = home
	engine.ImplementationFinalizer = fakeImplementationFinalizer{payload: payload}

	if err := engine.AdvanceJob(ctx, "fix-success"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(path, "owned.txt")); err != nil || string(got) != "committed fix\n" {
		t.Fatalf("successful AdvanceJob did not preserve clone bytes: %q, %v", got, err)
	}
	if got := countJobEvents(t, store, "fix-success", "delegation_worktree_removed"); got != 0 {
		t.Fatalf("removed events = %d, want 0", got)
	}
	if got := countJobEvents(t, store, "fix-success", "delegation_worktree_retained_unproved"); got != 1 {
		t.Fatalf("unproved retention events = %d, want 1", got)
	}
}

func TestCleanupFixWorktreeDoesNotTreatDanglingSymlinkAsRemoved(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := FixWorktreePath(home, "owner/repo", "fix-symlink")
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(filepath.Join(home, "missing-target"), path); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	engine := testEngine(store)
	engine.Home = home
	_ = engine.cleanupFixWorktree(ctx, "fix-symlink", "implement", JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", WorktreePath: path, FixWorktree: true,
	})
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("dangling symlink was removed: %v", err)
	}
	if got := countJobEvents(t, store, "fix-symlink", "delegation_worktree_removed"); got != 0 {
		t.Fatalf("dangling symlink produced %d removed events", got)
	}
}
