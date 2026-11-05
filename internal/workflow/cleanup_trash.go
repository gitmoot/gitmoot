package workflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
// with the episode's expected path and generation and the worktree's own git
// administrative directory), so a directory left by a crash or a failed
// deletion is found later. Deletion (DisposeCleanupTrash) proves the directory
// is that worktree, under the checkout mutation lock the move itself holds:
// the name belongs to the contained expected path, the directory is a real
// directory (not a symlink), its .git names the recorded administrative
// directory, that directory holds the nonce written just before this move
// (incarnation-specific: git deletes the directory with its worktree, so a
// reused administrative path has none), and git lists it as a worktree of this
// checkout. It does not wait
// for the episode's receipt (#2526 review): the worktree left its path when
// the move succeeded, and an interrupted episode whose receipt is superseded
// later must not keep its trash forever.
const (
	CleanupTrashPendingEvent = "delegation_worktree_trash_pending"
	CleanupTrashRemovedEvent = "delegation_worktree_trash_removed"
	// CleanupTrashRetryEvent records a disposal that could not finish yet; the
	// sweep skips the entry for a while so it cannot hold back the others.
	CleanupTrashRetryEvent = "delegation_worktree_trash_retry"
	cleanupTrashPrefix     = ".gitmoot-cleanup-trash-"
	// cleanupTrashNonceFile lives in the worktree's git administrative
	// directory; git ignores unknown files there and removes the directory
	// with the worktree.
	cleanupTrashNonceFile = "gitmoot-cleanup-trash-nonce"
	// cleanupTrashSmallFileLimit bounds the .git pointer and nonce reads, which
	// run under the store-wide cleanup fence.
	cleanupTrashSmallFileLimit = 4096
)

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
func (e Engine) moveWorktreeToTrash(ctx context.Context, jobID, path string, generation int64) (db.PendingCleanupTrash, error) {
	manager := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	mover, ok := e.DelegationWorktrees.(WorktreeMover)
	if !ok {
		if err := manager.RemoveWorktreeForce(ctx, path); err != nil {
			return db.PendingCleanupTrash{}, fmt.Errorf("remove producer worktree: %w", err)
		}
		return db.PendingCleanupTrash{}, nil
	}
	gitDir, err := worktreeGitDir(path)
	if err != nil {
		return db.PendingCleanupTrash{}, fmt.Errorf("identify producer worktree before moving it aside: %w", err)
	}
	nonce, err := writeCleanupTrashNonce(gitDir)
	if err != nil {
		return db.PendingCleanupTrash{}, fmt.Errorf("mark producer worktree before moving it aside: %w", err)
	}
	record := db.PendingCleanupTrash{JobID: jobID, Path: cleanupTrashPath(path, e.now().UTC()), ExpectedPath: filepath.Clean(path), Generation: generation, GitDir: gitDir, Nonce: nonce}
	if _, err := os.Lstat(record.Path); !errors.Is(err, os.ErrNotExist) {
		return db.PendingCleanupTrash{}, fmt.Errorf("cleanup trash destination %s already exists or cannot be checked: %v", record.Path, err)
	}
	message, err := json.Marshal(record)
	if err != nil {
		return db.PendingCleanupTrash{}, err
	}
	if err := e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: jobID, Kind: CleanupTrashPendingEvent, Message: string(message)}); err != nil {
		return db.PendingCleanupTrash{}, fmt.Errorf("record cleanup trash: %w", err)
	}
	if err := mover.MoveWorktree(ctx, path, record.Path); err != nil {
		if _, statErr := os.Lstat(record.Path); errors.Is(statErr, os.ErrNotExist) {
			// Nothing was created, and we still hold the checkout lock: close
			// the record now. Otherwise the sweep handles what is there.
			err = errors.Join(err, e.recordCleanupTrashRemoved(ctx, jobID, record.Path))
		}
		return db.PendingCleanupTrash{}, fmt.Errorf("move producer worktree aside: %w", err)
	}
	return record, nil
}

// readSmallRegularFile reads a regular file without following a symlink,
// without blocking on a FIFO, and at most limit bytes; anything else fails.
func readSmallRegularFile(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("%s is not a small regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
}

// worktreeGitDir returns the git administrative directory a linked worktree's
// .git file names ("gitdir: <path>", absolute or relative to the worktree);
// git worktree move keeps it. The directory must be a real directory.
func worktreeGitDir(worktree string) (string, error) {
	data, err := readSmallRegularFile(filepath.Join(worktree, ".git"), cleanupTrashSmallFileLimit)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	value, ok := strings.CutPrefix(line, "gitdir:")
	value = strings.TrimSpace(value)
	if !ok || value == "" || strings.ContainsAny(value, "\n\r") {
		return "", fmt.Errorf("%s/.git is not a linked worktree pointer", worktree)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(worktree, value)
	}
	value = filepath.Clean(value)
	info, err := os.Lstat(value)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("worktree git dir %s is not a directory", value)
	}
	return value, nil
}

// writeCleanupTrashNonce writes a fresh random nonce into gitDir. It never
// opens what is already at the marker name (#2526 review: an existing FIFO
// there would block the open under the store-wide fence): the nonce goes into
// a newly created file (O_EXCL, no-follow, nonblocking) that is then renamed
// over the marker name, which replaces any non-directory entry atomically.
func writeCleanupTrashNonce(gitDir string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(raw)
	temp := filepath.Join(gitDir, cleanupTrashNonceFile+"."+nonce+".tmp")
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := file.WriteString(nonce)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	if err := os.Rename(temp, filepath.Join(gitDir, cleanupTrashNonceFile)); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	return nonce, nil
}

// DisposeCleanupTrash deletes the trash directory of a pending record after
// proving it is the worktree its cleanup episode moved there, and records that
// it is gone. It holds the checkout mutation lock, never the cleanup lifecycle
// fence. The engine must be bound to the checkout that owns the worktree.
func (e Engine) DisposeCleanupTrash(ctx context.Context, record db.PendingCleanupTrash) (err error) {
	trash := filepath.Clean(strings.TrimSpace(record.Path))
	expected := filepath.Clean(strings.TrimSpace(record.ExpectedPath))
	if !filepath.IsAbs(trash) || !filepath.IsAbs(expected) || trash != record.Path || !filepath.IsAbs(record.GitDir) || len(record.Nonce) != 32 ||
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
	// flight: what is at the path now is all there will be.
	release, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, e.DelegationCheckout, "cleanup-trash:"+job.ID, time.Now().UTC())
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, release(context.WithoutCancel(ctx)))
	}()
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
	gitDir, err := worktreeGitDir(trash)
	if err != nil || gitDir != filepath.Clean(record.GitDir) {
		return fmt.Errorf("refusing to dispose %s: it is not the worktree moved there (git dir %q, recorded %q, %v)", trash, gitDir, record.GitDir, err)
	}
	if nonce, err := readSmallRegularFile(filepath.Join(gitDir, cleanupTrashNonceFile), cleanupTrashSmallFileLimit); err != nil || string(nonce) != record.Nonce {
		return fmt.Errorf("refusing to dispose %s: it is not the worktree moved there (nonce mismatch, %v)", trash, err)
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
