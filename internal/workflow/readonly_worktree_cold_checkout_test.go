package workflow

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

// coldPRCheckout builds a real three-repo fixture whose working checkout
// genuinely LACKS the PR head commit object:
//
//	origin (bare)  <- seed pushes main, then pushes the PR head to
//	                  refs/pull/<n>/head AFTER the checkout was cloned
//	checkout       <- cloned at main; never fetched the PR ref
//
// It returns the checkout path and the PR head SHA, which is a valid object in
// origin and an UNKNOWN object in checkout.
func coldPRCheckout(t *testing.T, pullRequest int) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	checkout := filepath.Join(root, "checkout")

	runWorktreeGit(t, root, "init", "--bare", "--initial-branch=main", origin)
	runWorktreeGit(t, root, "clone", origin, seed)
	runWorktreeGit(t, seed, "config", "user.email", "test@example.com")
	runWorktreeGit(t, seed, "config", "user.name", "Test")
	runWorktreeGit(t, seed, "checkout", "-b", "main")
	runWorktreeGit(t, seed, "commit", "--allow-empty", "-m", "base")
	runWorktreeGit(t, seed, "push", "origin", "main")

	// The daemon's checkout is cloned (cold) BEFORE the PR head exists.
	runWorktreeGit(t, root, "clone", origin, checkout)

	runWorktreeGit(t, seed, "commit", "--allow-empty", "-m", "pr head")
	runWorktreeGit(t, seed, "push", "origin", "HEAD:refs/pull/"+strconv.Itoa(pullRequest)+"/head")
	head, err := gitutil.NewHostClient(seed).HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA(seed): %v", err)
	}

	// Precondition: the object really is absent from the checkout.
	cmd := exec.Command("git", "cat-file", "-e", head+"^{commit}")
	cmd.Dir = checkout
	if err := cmd.Run(); err == nil {
		t.Fatalf("fixture is not cold: checkout already has %s", head)
	}
	return checkout, head
}

// TestReadOnlyFanoutColdCheckoutFetchesPullRequestRef drives the ENGINE's
// read-only fan-out allocation (dispatchDelegations -> allocateAndEnqueueDelegation
// -> AllocateReadOnlyDelegationWorktree -> real `git worktree add --detach <sha>`)
// against a checkout that does not have the PR head object.
//
// Without the pull/<n>/head fetch fallback the first sibling's allocation fails
// with "not a valid object name" and dispatchDelegations returns on the FIRST
// failure (engine_delegation.go:226), leaving the coordinator with ZERO children.
func TestReadOnlyFanoutColdCheckoutFetchesPullRequestRef(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "lens-a", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "lens-b", []string{"review"}, "gitmoot/gitmoot")

	checkout, head := coldPRCheckout(t, 7)
	home := t.TempDir()
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = checkout
	engine.DelegationWorktrees = gitutil.NewHostClient(checkout)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "main",
		PullRequest: 7,
		HeadSHA:     head,
		TaskID:      "task-5",
		TaskTitle:   "Parent",
		Sender:      "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d1", Agent: "lens-a", Action: "review", Prompt: "lens one"},
				{ID: "d2", Agent: "lens-b", Action: "review", Prompt: "lens two"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	for _, id := range []string{"parent-job/delegation/d1", "parent-job/delegation/d2"} {
		child := mustJob(t, store, id)
		payload, perr := unmarshalPayload(child.Payload)
		if perr != nil {
			t.Fatalf("unmarshalPayload(%s): %v", id, perr)
		}
		if strings.TrimSpace(payload.WorktreePath) == "" {
			t.Fatalf("%s has no worktree path", id)
		}
		at, herr := gitutil.NewHostClient(payload.WorktreePath).HeadSHA(ctx)
		if herr != nil {
			t.Fatalf("HeadSHA(%s): %v", payload.WorktreePath, herr)
		}
		if at != head {
			t.Fatalf("%s worktree head = %s, want the PR head %s", id, at, head)
		}
	}

	events, eerr := store.ListJobEvents(ctx, "parent-job")
	if eerr != nil {
		t.Fatalf("ListJobEvents: %v", eerr)
	}
	fetched := 0
	for _, event := range events {
		if event.Kind == "delegation_worktree_pr_ref_fetched" {
			fetched++
		}
	}
	if fetched == 0 {
		t.Fatalf("cold-checkout recovery must be observable; events = %+v", events)
	}
}

// TestReadOnlyFanoutColdCheckoutWithoutPullRequestKeepsOriginalError pins the
// scoping of that fallback: with no PR number there is no pull/<n>/head to
// fetch, so the allocation failure must surface UNCHANGED (no fetch attempt, no
// rewrapping) rather than turning into a "fetch PR ref" error.
func TestReadOnlyFanoutColdCheckoutWithoutPullRequestKeepsOriginalError(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "lens-a", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "lens-b", []string{"review"}, "gitmoot/gitmoot")

	checkout, head := coldPRCheckout(t, 7)
	engine := testEngine(store)
	engine.Home = t.TempDir()
	engine.DelegationCheckout = checkout
	engine.DelegationWorktrees = gitutil.NewHostClient(checkout)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "review"}, JobPayload{
		Repo:      "gitmoot/gitmoot",
		Branch:    "main",
		HeadSHA:   head,
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d1", Agent: "lens-a", Action: "review", Prompt: "lens one"},
				{ID: "d2", Agent: "lens-b", Action: "review", Prompt: "lens two"},
			},
		},
	})

	err := engine.AdvanceJob(ctx, "parent-job")
	if err == nil {
		t.Fatal("AdvanceJob must fail: the checkout has no such object and no PR ref to fetch")
	}
	if !strings.Contains(err.Error(), "invalid reference") {
		t.Fatalf("error = %v, want the raw git worktree failure", err)
	}
	if strings.Contains(err.Error(), "fetch PR ref") || strings.Contains(err.Error(), "after fetch") {
		t.Fatalf("PR-less fan-out must not attempt a pull/<n>/head fetch: %v", err)
	}
}
