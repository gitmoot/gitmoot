package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
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

// #2526 review (P1): disposal must prove the directory is the one its cleanup
// episode moved there. Before that episode's receipt completes it waits; a
// directory at the trash name that git does not list as a worktree of this
// checkout is never deleted; and a record whose name does not belong to its
// expected path is refused.
func TestCleanupTrashDisposalRequiresOwnershipProof(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	fake := db.PendingCleanupTrash{JobID: world.jobID, ExpectedPath: world.path, Generation: 0, Path: cleanupTrashPath(world.path, time.Now())}
	if err := world.engine.DisposeCleanupTrash(ctx, fake); !errors.Is(err, ErrCleanupTrashNotReady) {
		t.Fatalf("disposal before the episode completed: %v, want ErrCleanupTrashNotReady", err)
	}
	if err := world.engine.cleanupReadOnlyDelegationWorktree(ctx, world.jobID, "review", world.payload); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// A stray directory at a trash name of the same episode, not a worktree.
	stray := cleanupTrashPath(world.path, time.Now().Add(time.Hour))
	if err := os.MkdirAll(filepath.Join(stray, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := fake
	record.Path = stray
	if err := world.engine.DisposeCleanupTrash(ctx, record); err == nil || !strings.Contains(err.Error(), "not a registered worktree") {
		t.Fatalf("disposal of an unregistered directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stray, "keep")); err != nil {
		t.Fatalf("the stray directory was touched: %v", err)
	}
	other := record
	other.ExpectedPath = filepath.Join(filepath.Dir(world.path), "someone-else")
	if err := world.engine.DisposeCleanupTrash(ctx, other); err == nil || !strings.Contains(err.Error(), "not the cleanup trash") {
		t.Fatalf("disposal of a name that is not the expected path's: %v", err)
	}
}
