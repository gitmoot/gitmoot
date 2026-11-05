package workflow

import (
	"context"
	"errors"
	"os"
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
	if isCleanupTrashPath(path) {
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
	if pending, err := world.store.PendingCleanupTrashes(ctx, 10); err != nil || len(pending) != 1 || pending[0].Path != trash {
		t.Errorf("pending trash = %+v (%v), want exactly %s", pending, err, trash)
	}
	close(manager.release)
	if err := <-done; err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Lstat(trash); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trash %s still exists after disposal: %v", trash, err)
	}
	if pending, err := world.store.PendingCleanupTrashes(ctx, 10); err != nil || len(pending) != 0 {
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
	pending, err := world.store.PendingCleanupTrashes(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending trash = %+v (%v), want one", pending, err)
	}
	if _, err := os.Lstat(pending[0].Path); err != nil {
		t.Fatalf("pending trash %s is missing: %v", pending[0].Path, err)
	}
	manager.fail = nil
	if err := world.engine.DisposeCleanupTrash(ctx, pending[0].JobID, pending[0].Path); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := os.Lstat(pending[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trash still exists after retry: %v", err)
	}
	if again, _ := world.store.PendingCleanupTrashes(ctx, 10); len(again) != 0 {
		t.Errorf("pending trash after retry = %+v", again)
	}
	if err := world.engine.DisposeCleanupTrash(ctx, world.jobID, world.path); err == nil {
		t.Error("DisposeCleanupTrash accepted a path that is not a trash directory")
	}
}
