package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
)

// dispatchReviewWorktree drives prepareLocalReviewWorktree the way a review dispatch
// does, against ONE home and store so a repo-stable task id resolves to the SAME
// worktree path on every call. That reuse is the condition under test: with a
// per-home store the paths would differ and none of these tests could fail.
func dispatchReviewWorktree(t *testing.T, store *db.Store, home, repoDir, head string) (string, string, error) {
	t.Helper()
	request, path, err := prepareLocalReviewWorktree(
		context.Background(),
		store,
		db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir},
		github.Repository{Owner: "owner", Name: "repo"},
		localAgentDispatchRequest{Home: home, PullRequest: 17, HeadSHA: head},
	)
	return request.TaskID, path, err
}

func worktreeHead(t *testing.T, repoDir, path string) string {
	t.Helper()
	head, err := (gitutil.Client{Dir: repoDir}).HeadSHAAt(context.Background(), path)
	if err != nil {
		t.Fatalf("HeadSHAAt(%s): %v", path, err)
	}
	return head
}

// TestReviewWorktreeReSyncsToRequestedHeadAcrossRounds is the acceptance mutant for
// #1415: two dispatches at DIFFERENT heads through ONE stable task id, and the
// second must be reviewing the second head.
//
// This test could not have failed before #1354 C1. While the head was part of the
// review task id, the second dispatch resolved a different worktree path and the
// creation branch checked out the requested commit implicitly. C1 made the id
// repo-stable, so the path is reused and nothing re-synced it -- the invariant was
// real, load-bearing, and unowned because it could not fail while the key varied.
func TestReviewWorktreeReSyncsToRequestedHeadAcrossRounds(t *testing.T) {
	repoDir, oldHead, newHead := c1ReviewRepository(t)
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() { _ = store.Close() })

	firstTaskID, firstPath, err := dispatchReviewWorktree(t, store, home, repoDir, oldHead)
	if err != nil {
		t.Fatalf("first dispatch at %s: %v", oldHead, err)
	}
	if got := worktreeHead(t, repoDir, firstPath); got != oldHead {
		t.Fatalf("first dispatch worktree head = %s, want %s", got, oldHead)
	}

	secondTaskID, secondPath, err := dispatchReviewWorktree(t, store, home, repoDir, newHead)
	if err != nil {
		t.Fatalf("second dispatch at %s: %v", newHead, err)
	}

	// The reuse condition itself. If these ever diverge the test has stopped
	// exercising the defect and would pass for the wrong reason.
	if secondTaskID != firstTaskID {
		t.Fatalf("task id changed between rounds (%s -> %s); the stable-identity reuse path was not exercised", firstTaskID, secondTaskID)
	}
	if secondPath != firstPath {
		t.Fatalf("worktree path changed between rounds (%s -> %s); the reuse path was not exercised", firstPath, secondPath)
	}

	if got := worktreeHead(t, repoDir, secondPath); got != newHead {
		t.Fatalf("second dispatch reviewed %s, want the requested head %s (reused worktree was not re-synced)", got, newHead)
	}
}

// TestReviewWorktreeSameHeadRedispatchDoesNotMutate pins the cheap path: a
// redispatch at the head the worktree already holds must succeed and leave the
// checkout alone.
func TestReviewWorktreeSameHeadRedispatchDoesNotMutate(t *testing.T) {
	repoDir, oldHead, _ := c1ReviewRepository(t)
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() { _ = store.Close() })

	if _, _, err := dispatchReviewWorktree(t, store, home, repoDir, oldHead); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	_, path, err := dispatchReviewWorktree(t, store, home, repoDir, oldHead)
	if err != nil {
		t.Fatalf("same-head redispatch returned an error: %v", err)
	}
	if got := worktreeHead(t, repoDir, path); got != oldHead {
		t.Fatalf("same-head redispatch moved the worktree to %s, want %s", got, oldHead)
	}
}

// TestReviewWorktreeRefusesToDiscardUncommittedChanges is the fail-loudly half. A
// reused worktree at the wrong head with uncommitted work must REFUSE, naming both
// SHAs -- never reset over it. A dirty checkout is unreviewed work, and discarding
// it silently is the worse failure.
func TestReviewWorktreeRefusesToDiscardUncommittedChanges(t *testing.T) {
	repoDir, oldHead, newHead := c1ReviewRepository(t)
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() { _ = store.Close() })

	_, path, err := dispatchReviewWorktree(t, store, home, repoDir, oldHead)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("uncommitted reviewer edit\n"), 0o644); err != nil {
		t.Fatalf("dirty the worktree: %v", err)
	}

	_, _, err = dispatchReviewWorktree(t, store, home, repoDir, newHead)
	if err == nil {
		t.Fatalf("dispatch at %s over a dirty worktree at %s succeeded; it must refuse", newHead, oldHead)
	}
	for _, want := range []string{oldHead, newHead, "uncommitted changes"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q; the operator cannot act on it", err.Error(), want)
		}
	}
	// The refusal must not have moved anything.
	if got := worktreeHead(t, repoDir, path); got != oldHead {
		t.Fatalf("refused dispatch still moved the worktree to %s, want %s left untouched", got, oldHead)
	}
}
