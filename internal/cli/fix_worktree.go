package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// allocateFixWorktree creates an independent writable clone for one review fix
// job. A linked worktree cannot check out payload.Branch while the lane owner's
// registered checkout already has that branch checked out; an independent clone
// has its own refs and HEAD, so the fix can commit and push without touching the
// owner's index, files, or current branch.
func allocateFixWorktree(ctx context.Context, store *db.Store, home string, checkout string, request workflow.FixWorktreeRequest) (workflow.FixWorktreeAllocation, error) {
	return allocateFixWorktreeForRunner(ctx, store, home, checkout, request, subprocess.ExecRunner{})
}

func allocateFixWorktreeForRunner(ctx context.Context, store *db.Store, home string, checkout string, request workflow.FixWorktreeRequest, runner subprocess.Runner) (allocation workflow.FixWorktreeAllocation, retErr error) {
	if store == nil {
		return allocation, errors.New("fix worktree store is required")
	}
	checkout = strings.TrimSpace(checkout)
	if checkout == "" {
		return allocation, errors.New("fix worktree source checkout is required")
	}
	branch := strings.TrimSpace(request.Branch)
	if branch == "" {
		return allocation, errors.New("fix worktree branch is required")
	}
	path, err := workflow.FixWorktreePath(home, request.Repo, request.JobID)
	if err != nil {
		return allocation, err
	}
	allocation.Path = path
	if _, err := os.Stat(path); err == nil {
		// A persisted job owns an existing path even if it is temporarily dirty or
		// otherwise unreadable. Never destroy it during an idempotent re-dispatch;
		// verify only its branch identity. With no job row, the directory is an
		// interrupted pre-enqueue allocation and must be recreated from a fresh fetch
		// rather than silently reusing a potentially stale head.
		if _, jobErr := store.GetJob(ctx, request.JobID); jobErr == nil {
			branchAtPath, branchErr := jobGitClient(path, runner).CurrentBranch(ctx)
			if branchErr == nil && branchAtPath == branch {
				return allocation, nil
			}
			return allocation, fmt.Errorf("existing fix worktree %s is not on branch %s", path, branch)
		} else if !errors.Is(jobErr, sql.ErrNoRows) {
			return allocation, jobErr
		}
		if err := os.RemoveAll(path); err != nil {
			return allocation, fmt.Errorf("remove incomplete fix worktree %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return allocation, fmt.Errorf("inspect fix worktree %s: %w", path, err)
	}

	source := jobGitClient(checkout, runner)
	remoteURL, err := source.OriginRemoteConfigured(ctx)
	if err != nil {
		return allocation, fmt.Errorf("resolve fix worktree origin: %w", err)
	}
	release, err := workflow.AcquireCheckoutMutationLock(ctx, store, checkout, "fix-worktree:"+request.JobID, time.Now().UTC())
	if err != nil {
		return allocation, fmt.Errorf("lock source checkout for fix worktree: %w", err)
	}
	defer func() {
		if release != nil {
			_ = release(context.Background())
		}
	}()
	if err := source.FetchRemote(ctx, "origin"); err != nil {
		return allocation, fmt.Errorf("fetch fix branch: %w", err)
	}
	head, err := source.RevParse(ctx, "refs/remotes/origin/"+branch)
	if err != nil {
		return allocation, fmt.Errorf("resolve remote fix branch %s: %w", branch, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return allocation, err
	}
	if err := source.CloneLocalNoCheckout(ctx, path); err != nil {
		return allocation, fmt.Errorf("clone fix worktree: %w", err)
	}
	allocation.Created = true
	defer func() {
		if retErr != nil && allocation.Created {
			_ = os.RemoveAll(path)
		}
	}()
	clone := jobGitClient(path, runner)
	if err := clone.SetRemoteURL(ctx, "origin", remoteURL); err != nil {
		return allocation, fmt.Errorf("bind fix worktree origin: %w", err)
	}
	if err := clone.CheckoutBranchAt(ctx, branch, head); err != nil {
		return allocation, fmt.Errorf("checkout fix branch %s: %w", branch, err)
	}
	return allocation, nil
}
