package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// Cleanup trash (#2524). Removing a delegation worktree used to run
// `git worktree remove --force` under the store-wide exclusive cleanup
// lifecycle fence; on the sshfs worktree mount that took 15-40 s for a large
// worktree, and every job claim in every repository waited for it. Under the
// fence the worktree is now only moved aside with `git worktree move` (a
// rename, about half a second, and refused across file systems, so never a
// copy) into a sibling directory whose name no cleanup obligation can have as
// its expected path. Its files are deleted after the fence is released, under
// the checkout mutation lock only, because that deletion still rewrites the
// checkout's shared worktree metadata.
//
// The move is recorded durably before it happens (CleanupTrashPendingEvent)
// and the deletion after it succeeds (CleanupTrashRemovedEvent), so a trash
// directory left by a crash or a failed deletion is found later
// (Store.PendingCleanupTrashes) and deleted by DisposeCleanupTrash.
const (
	CleanupTrashPendingEvent = "delegation_worktree_trash_pending"
	CleanupTrashRemovedEvent = "delegation_worktree_trash_removed"
	cleanupTrashPrefix       = ".gitmoot-cleanup-trash-"
)

// cleanupTrashPath is a fresh sibling of path for its trash directory.
func cleanupTrashPath(path string, now time.Time) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return filepath.Join(filepath.Dir(path), fmt.Sprintf("%s%s-%d", cleanupTrashPrefix, hex.EncodeToString(sum[:6]), now.UnixNano()))
}

// isCleanupTrashPath reports whether path names a trash directory this file
// creates; disposal refuses anything else.
func isCleanupTrashPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && strings.HasPrefix(filepath.Base(path), cleanupTrashPrefix)
}

// moveWorktreeToTrash moves the worktree at path aside and returns the trash
// path. A manager that cannot move worktrees removes it in place, as before.
// A failed move fails the attempt (the worktree stays where it was, since git
// renames atomically) and the reclaim pass retries it under its failure
// budget; it never falls back to the slow in-place removal under the fence.
func (e Engine) moveWorktreeToTrash(ctx context.Context, jobID, path string) (string, error) {
	manager := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	mover, ok := e.DelegationWorktrees.(WorktreeMover)
	if !ok {
		if err := manager.RemoveWorktreeForce(ctx, path); err != nil {
			return "", fmt.Errorf("remove producer worktree: %w", err)
		}
		return "", nil
	}
	trash := cleanupTrashPath(path, e.now().UTC())
	if err := e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: jobID, Kind: CleanupTrashPendingEvent, Message: trash}); err != nil {
		return "", fmt.Errorf("record cleanup trash: %w", err)
	}
	if err := mover.MoveWorktree(ctx, path, trash); err != nil {
		if _, statErr := os.Lstat(trash); errors.Is(statErr, os.ErrNotExist) {
			// Nothing was created; close the pending record now. Otherwise the
			// sweep finds and deletes whatever is there.
			err = errors.Join(err, e.recordCleanupTrashRemoved(ctx, jobID, trash))
		}
		return "", fmt.Errorf("move producer worktree aside: %w", err)
	}
	return trash, nil
}

// DisposeCleanupTrash deletes a trash directory moveWorktreeToTrash created for
// jobID and records that it is gone. A trash path that does not exist (the move
// never happened, or an earlier disposal finished) is recorded as removed. It
// holds the checkout mutation lock, never the cleanup lifecycle fence. The
// engine must be bound to the checkout that owns the worktree.
func (e Engine) DisposeCleanupTrash(ctx context.Context, jobID, trash string) (err error) {
	if !isCleanupTrashPath(trash) {
		return fmt.Errorf("refusing to dispose %q: not a cleanup trash path", trash)
	}
	if _, statErr := os.Lstat(trash); errors.Is(statErr, os.ErrNotExist) {
		return e.recordCleanupTrashRemoved(ctx, jobID, trash)
	} else if statErr != nil {
		return statErr
	}
	release, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, e.DelegationCheckout, "cleanup-trash:"+jobID, time.Now().UTC())
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, release(context.WithoutCancel(ctx)))
	}()
	if err := e.DelegationWorktrees.(ReadOnlyWorktreeManager).RemoveWorktreeForce(ctx, trash); err != nil {
		return fmt.Errorf("remove cleanup trash %s: %w", trash, err)
	}
	return e.recordCleanupTrashRemoved(ctx, jobID, trash)
}

func (e Engine) recordCleanupTrashRemoved(ctx context.Context, jobID, trash string) error {
	return e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: jobID, Kind: CleanupTrashRemovedEvent, Message: trash})
}
