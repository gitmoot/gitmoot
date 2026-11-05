package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
// fence a detached worktree is now only moved aside with `git worktree move` (a
// rename, about half a second, refused across file systems and onto an existing
// path, so never a copy and never onto something else) into a sibling whose
// name carries a hash of the expected path, so no cleanup obligation can have it
// as its expected path. Its files are deleted after the fence is released.
//
// The move is recorded durably before it happens (CleanupTrashPendingEvent,
// with the episode's expected path and lifecycle generation), so a directory
// left by a crash or a failed deletion is found later. Deletion
// (DisposeCleanupTrash) proves ownership first, under the checkout mutation
// lock that the move itself holds: the record's episode receipt is complete
// (the receipt completes only after the move), the name matches the expected
// path, the directory is a real directory (not a symlink) beside a contained
// expected path, and git lists it as a worktree of this checkout.
const (
	CleanupTrashPendingEvent = "delegation_worktree_trash_pending"
	CleanupTrashRemovedEvent = "delegation_worktree_trash_removed"
	// CleanupTrashRetryEvent records a disposal that could not finish yet; the
	// sweep skips the entry for a while so it cannot hold back the others.
	CleanupTrashRetryEvent = "delegation_worktree_trash_retry"
	cleanupTrashPrefix     = ".gitmoot-cleanup-trash-"
)

// ErrCleanupTrashNotReady means the trash's cleanup episode has not completed
// yet; the entry stays pending and is retried later.
var ErrCleanupTrashNotReady = errors.New("cleanup trash episode not complete")

// WorktreeRegistrar reports whether a path is a worktree registered in the
// checkout (git worktree list).
type WorktreeRegistrar interface {
	WorktreeRegistered(ctx context.Context, path string) (bool, error)
}

func cleanupTrashNamePrefix(expected string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(expected)))
	return cleanupTrashPrefix + hex.EncodeToString(sum[:6]) + "-"
}

// cleanupTrashPath is a fresh sibling of expected for its trash directory.
func cleanupTrashPath(expected string, now time.Time) string {
	return filepath.Join(filepath.Dir(expected), fmt.Sprintf("%s%d", cleanupTrashNamePrefix(expected), now.UnixNano()))
}

// moveWorktreeToTrash moves the worktree at path aside and returns the trash
// path. A manager that cannot move worktrees removes it in place, as before. A
// failed move fails the attempt (git renames atomically, so the worktree stays
// where it was) and the reclaim pass retries it under its failure budget; it
// never falls back to the slow in-place removal under the fence. Called with
// the cleanup fence and the checkout mutation lock held.
func (e Engine) moveWorktreeToTrash(ctx context.Context, jobID, path string, generation int64) (string, error) {
	manager := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	mover, ok := e.DelegationWorktrees.(WorktreeMover)
	if !ok {
		if err := manager.RemoveWorktreeForce(ctx, path); err != nil {
			return "", fmt.Errorf("remove producer worktree: %w", err)
		}
		return "", nil
	}
	record := db.PendingCleanupTrash{JobID: jobID, Path: cleanupTrashPath(path, e.now().UTC()), ExpectedPath: filepath.Clean(path), Generation: generation}
	if _, err := os.Lstat(record.Path); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("cleanup trash destination %s already exists or cannot be checked: %v", record.Path, err)
	}
	message, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	if err := e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: jobID, Kind: CleanupTrashPendingEvent, Message: string(message)}); err != nil {
		return "", fmt.Errorf("record cleanup trash: %w", err)
	}
	if err := mover.MoveWorktree(ctx, path, record.Path); err != nil {
		if _, statErr := os.Lstat(record.Path); errors.Is(statErr, os.ErrNotExist) {
			// Nothing was created, and we still hold the checkout lock: close
			// the record now. Otherwise the sweep handles what is there.
			err = errors.Join(err, e.recordCleanupTrashRemoved(ctx, jobID, record.Path))
		}
		return "", fmt.Errorf("move producer worktree aside: %w", err)
	}
	return record.Path, nil
}

// DisposeCleanupTrash deletes the trash directory of a pending record after
// proving it is the one its cleanup episode moved there, and records that it is
// gone. It holds the checkout mutation lock, never the cleanup lifecycle fence.
// The engine must be bound to the checkout that owns the worktree. An episode
// that has not completed returns ErrCleanupTrashNotReady and leaves the record.
func (e Engine) DisposeCleanupTrash(ctx context.Context, record db.PendingCleanupTrash) (err error) {
	trash := filepath.Clean(strings.TrimSpace(record.Path))
	expected := filepath.Clean(strings.TrimSpace(record.ExpectedPath))
	if !filepath.IsAbs(trash) || !filepath.IsAbs(expected) || trash != record.Path ||
		filepath.Dir(trash) != filepath.Dir(expected) ||
		!strings.HasPrefix(filepath.Base(trash), cleanupTrashNamePrefix(expected)) {
		return fmt.Errorf("refusing to dispose %q: not the cleanup trash of %q", record.Path, record.ExpectedPath)
	}
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	registrar, listOK := e.DelegationWorktrees.(WorktreeRegistrar)
	if !ok || !listOK || strings.TrimSpace(e.DelegationCheckout) == "" {
		return fmt.Errorf("cleanup trash %s: no checkout worktree manager", trash)
	}
	job, payload, err := e.jobPayload(ctx, record.JobID)
	if err != nil {
		return err
	}
	contained := payload
	contained.WorktreePath = expected
	if err := ValidateDelegationCleanupTarget(e.Home, job.ID, job.Type, contained); err != nil {
		return fmt.Errorf("cleanup trash %s: expected path not contained: %w", trash, err)
	}
	// The move holds this lock too, so from here on no move to this name is in
	// flight and the receipt read below is final for the episode.
	release, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, e.DelegationCheckout, "cleanup-trash:"+job.ID, time.Now().UTC())
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, release(context.WithoutCancel(ctx)))
	}()
	receipt, err := e.Store.GetCleanupReceipt(ctx, db.CleanupObligationResourceID(job.ID, expected), record.Generation)
	if errors.Is(err, sql.ErrNoRows) || err == nil && strings.TrimSpace(receipt.RemovedAt) == "" {
		return ErrCleanupTrashNotReady
	}
	if err != nil {
		return err
	}
	info, statErr := os.Lstat(trash)
	if errors.Is(statErr, os.ErrNotExist) {
		return e.recordCleanupTrashRemoved(ctx, job.ID, trash)
	}
	if statErr != nil {
		return statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to dispose %s: not a directory", trash)
	}
	registered, err := registrar.WorktreeRegistered(ctx, trash)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("refusing to dispose %s: not a registered worktree of %s", trash, e.DelegationCheckout)
	}
	if err := manager.RemoveWorktreeForce(ctx, trash); err != nil {
		return fmt.Errorf("remove cleanup trash %s: %w", trash, err)
	}
	return e.recordCleanupTrashRemoved(ctx, job.ID, trash)
}

func (e Engine) recordCleanupTrashRemoved(ctx context.Context, jobID, trash string) error {
	return e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: jobID, Kind: CleanupTrashRemovedEvent, Message: trash})
}
