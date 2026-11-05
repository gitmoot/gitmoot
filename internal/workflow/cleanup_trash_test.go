package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

// slowTrashManager is the real git client whose removal of a cleanup trash
// directory waits for release, standing in for a slow sshfs deletion.
type slowTrashManager struct {
	gitutil.Client
	entered chan string
	release chan struct{}
	fail    error
}

func (m *slowTrashManager) RemoveWorktreeForce(ctx context.Context, path string) error {
	if strings.HasPrefix(filepath.Base(path), cleanupTrashPrefix) {
		m.entered <- path
		<-m.release
		if m.fail != nil {
			return m.fail
		}
	}
	return m.Client.RemoveWorktreeForce(ctx, path)
}

// #2524: deleting a worktree's files held the store-wide exclusive cleanup
// lifecycle fence, so every job claim in every repo waited for the whole
// (15-40 s on sshfs) deletion. The fence may now cover only the move aside:
// while the files are still being deleted, a claim's shared fence must be
// available, the worktree must already be gone from its path, and its
// receipt must already be complete.
func TestWorktreeFilesAreDeletedOutsideTheCleanupFence(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	manager := &slowTrashManager{Client: world.manager, entered: make(chan string, 1), release: make(chan struct{})}
	world.engine.DelegationWorktrees = manager
	done := make(chan error, 1)
	go func() {
		done <- world.engine.cleanupReadOnlyDelegationWorktree(ctx, world.jobID, "review", world.payload)
	}()
	var trash string
	select {
	case trash = <-manager.entered:
	case err := <-done:
		t.Fatalf("cleanup finished without deleting a trash directory: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("cleanup never reached the trash deletion")
	}
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, releaseClaim, err := world.store.AcquireJobLifecycle(claimCtx)
	if err != nil {
		close(manager.release)
		t.Fatalf("a job claim could not take the shared lifecycle fence while the files were deleted: %v", err)
	}
	releaseClaim()
	if _, err := os.Lstat(world.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the worktree is still at %s while its files are deleted: %v", world.path, err)
	}
	if !strings.HasPrefix(trash, strings.TrimSuffix(world.path, "readonly-seat")) {
		t.Errorf("trash %s is not a sibling of %s", trash, world.path)
	}
	if pending, err := world.store.PendingCleanupTrashes(ctx, 10, time.Now().Add(-time.Hour)); err != nil || len(pending) != 1 || pending[0].Path != trash || pending[0].ExpectedPath != world.path {
		t.Errorf("pending trash = %+v (%v), want exactly %s", pending, err, trash)
	}
	close(manager.release)
	if err := <-done; err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Lstat(trash); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trash %s still exists after disposal: %v", trash, err)
	}
	if pending, err := world.store.PendingCleanupTrashes(ctx, 10, time.Now().Add(-time.Hour)); err != nil || len(pending) != 0 {
		t.Errorf("pending trash after disposal = %+v (%v), want none", pending, err)
	}
}

// #2524: a failed trash deletion does not fail or undo the cleanup (the
// worktree is already gone from its path); the trash stays recorded as
// pending and a later DisposeCleanupTrash deletes it.
func TestFailedTrashDeletionIsRetriedFromThePendingRecord(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	manager := &slowTrashManager{Client: world.manager, entered: make(chan string, 4), release: make(chan struct{}), fail: errors.New("sshfs: connection reset")}
	close(manager.release)
	world.engine.DelegationWorktrees = manager
	if err := world.engine.cleanupReadOnlyDelegationWorktree(ctx, world.jobID, "review", world.payload); err != nil {
		t.Fatalf("a failed trash deletion failed the cleanup: %v", err)
	}
	pending, err := world.store.PendingCleanupTrashes(ctx, 10, time.Now().Add(-time.Hour))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending trash = %+v (%v), want one", pending, err)
	}
	if _, err := os.Lstat(pending[0].Path); err != nil {
		t.Fatalf("pending trash %s is missing: %v", pending[0].Path, err)
	}
	manager.fail = nil
	if err := world.engine.DisposeCleanupTrash(ctx, pending[0]); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := os.Lstat(pending[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trash still exists after retry: %v", err)
	}
	if again, _ := world.store.PendingCleanupTrashes(ctx, 10, time.Now().Add(-time.Hour)); len(again) != 0 {
		t.Errorf("pending trash after retry = %+v", again)
	}
	impostor := pending[0]
	impostor.Path = world.path
	if err := world.engine.DisposeCleanupTrash(ctx, impostor); err == nil {
		t.Error("DisposeCleanupTrash accepted a path that is not the trash of its expected path")
	}
}

// #2526 review: disposal must prove the directory is the worktree its cleanup
// episode moved there, and nothing else: a different worktree registered at
// that exact trash path, or a plain directory there, is never deleted; a record
// whose name does not belong to its expected path is refused.
func TestCleanupTrashDisposalRequiresOwnershipProof(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	gitDir, err := worktreeGitDir(world.path)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := world.engine.moveWorktreeToTrash(ctx, world.jobID, world.path, 0)
	trash := moved.Path
	if err != nil {
		t.Fatalf("move aside: %v", err)
	}
	pending, err := world.store.PendingCleanupTrashes(ctx, 10, time.Now().Add(-time.Hour))
	if err != nil || len(pending) != 1 || pending[0].Path != trash || pending[0].GitDir != gitDir {
		t.Fatalf("pending = %+v (%v), want %s with git dir %s", pending, err, trash, gitDir)
	}
	record := pending[0]
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = world.manager.Dir()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Replace the moved worktree with a different worktree at the same path.
	if err := world.manager.RemoveWorktreeForce(ctx, trash); err != nil {
		t.Fatal(err)
	}
	git("worktree", "prune")
	git("worktree", "add", "--detach", trash, "HEAD")
	if err := world.engine.DisposeCleanupTrash(ctx, record); err == nil || !strings.Contains(err.Error(), "not the worktree moved there") {
		t.Fatalf("disposal of a replacement worktree at the trash path: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(trash, ".git")); err != nil {
		t.Fatalf("the replacement worktree was touched: %v", err)
	}
	if err := world.manager.RemoveWorktreeForce(ctx, trash); err != nil {
		t.Fatal(err)
	}
	// A plain directory at the trash path.
	if err := os.MkdirAll(filepath.Join(trash, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := world.engine.DisposeCleanupTrash(ctx, record); err == nil {
		t.Fatal("disposal of a directory that is not a worktree succeeded")
	}
	if _, err := os.Lstat(filepath.Join(trash, "keep")); err != nil {
		t.Fatalf("the directory was touched: %v", err)
	}
	other := record
	other.ExpectedPath = filepath.Join(filepath.Dir(world.path), "someone-else")
	if err := world.engine.DisposeCleanupTrash(ctx, other); err == nil || !strings.Contains(err.Error(), "not the cleanup trash") {
		t.Fatalf("disposal of a name that is not the expected path's: %v", err)
	}
}

// #2526 review (F5): a moved worktree is disposed even though its episode's
// receipt never completed; an interrupted episode whose receipt is later
// superseded must not keep its trash forever.
func TestMovedTrashIsDisposedWithoutACompletedReceipt(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	moved, err := world.engine.moveWorktreeToTrash(ctx, world.jobID, world.path, 0)
	trash := moved.Path
	if err != nil {
		t.Fatalf("move aside: %v", err)
	}
	pending, err := world.store.PendingCleanupTrashes(ctx, 10, time.Now().Add(-time.Hour))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v (%v)", pending, err)
	}
	if err := world.engine.DisposeCleanupTrash(ctx, pending[0]); err != nil {
		t.Fatalf("disposal: %v", err)
	}
	if _, err := os.Lstat(trash); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trash still exists: %v", err)
	}
	if again, _ := world.store.PendingCleanupTrashes(ctx, 10, time.Now().Add(-time.Hour)); len(again) != 0 {
		t.Fatalf("still pending: %+v", again)
	}
}
