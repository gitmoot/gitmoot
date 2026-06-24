package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// The #413 escape: a worktree-less delegation child (a delegation leg with an
// empty WorktreePath) inherits its coordinator's branch but can only resolve the
// repo's registered shared checkout, which sits on `main`. Before #413, the
// implement/review arms of defaultCheckout validated that shared checkout against
// payload.Branch and failed it with "checkout branch is main, not job branch
// <X>" — even though the engine's delegation_worktree_skipped fallback runs the
// child against the shared checkout (holding its branch lock) on purpose. #413
// extends #389's ask-arm escape to the implement/review arms for that child only.
//
// These tests reuse the daemon checkout seams (a temp git checkout on `main`, the
// real defaultCheckout) so they exercise the production code path end-to-end.

// T1 (implement) + LOAD-BEARING: the escape fires for a worktree-less delegation
// implement child (DelegationID set, WorktreePath "", Branch "feature-x") — it is
// NOT rejected against the `main` checkout — AND validateImplementationLock still
// runs (the unconditional branch-lock guard). This test FAILS against pre-fix
// code, where defaultCheckout returns the "checkout branch is main, not job
// branch feature-x" error from validateTargetCheckout before the lock is ever
// consulted.
func TestDefaultCheckoutEscapesWorktreelessDelegationImplementChild(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	agent := runtime.Agent{Name: "impl-agent"}
	child := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "feature-x", // inherited coordinator branch; checkout is on main
		DelegationID: "d-impl",
		ParentJobID:  "coordinator-job",
		// WorktreePath intentionally empty: only the shared checkout resolves.
	}
	job := db.Job{ID: "impl-child", Type: "implement"}

	// With a branch lock held by a DIFFERENT owner, the checkout-branch guard is
	// skipped (the escape) but the unconditional validateImplementationLock now
	// runs and rejects the foreign owner — proving the lock guard is still reached.
	// If the escape had not fired, the error would instead be the branch-mismatch
	// one, returned before the lock is ever consulted.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "feature-x", Owner: "someone-else"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned (%v, %v)", acquired, err)
	}
	if _, err := worker.defaultCheckout(ctx, job, child, agent); err == nil {
		t.Fatal("defaultCheckout accepted a worktree-less implement child whose branch lock is held by another owner")
	} else if strings.Contains(err.Error(), "not job branch") {
		t.Fatalf("escape did not fire: defaultCheckout still validated the shared checkout against the job branch: %v", err)
	} else if !strings.Contains(err.Error(), "locked by") {
		t.Fatalf("expected an implementation-lock error, got: %v", err)
	}

	// Hand the lock to the child's own agent.
	if released, err := store.ReleaseLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "feature-x", Owner: "someone-else"}); err != nil || !released {
		t.Fatalf("ReleaseLock returned (%v, %v)", released, err)
	}

	// Now hold the branch lock for the child's agent: the escape skips the branch
	// guard AND validateImplementationLock passes, so defaultCheckout returns the
	// shared checkout with NO error.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "feature-x", Owner: agent.Name}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned (%v, %v)", acquired, err)
	}
	got, err := worker.defaultCheckout(ctx, job, child, agent)
	if err != nil {
		t.Fatalf("defaultCheckout rejected a worktree-less implement child holding its branch lock: %v", err)
	}
	if got != checkout {
		t.Fatalf("defaultCheckout = %q, want shared checkout %q", got, checkout)
	}
}

// T2 guard preserved: a normal implement job (no DelegationID, Branch
// "feature-x") against the `main` checkout STILL fails the branch-identity guard
// with "checkout branch is main, not job branch feature-x". The escape must not
// weaken safety for ordinary jobs.
func TestDefaultCheckoutKeepsBranchGuardForNormalImplementJob(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	normal := workflow.JobPayload{
		Repo:   "owner/repo",
		Branch: "feature-x", // genuine branch mismatch against the main checkout
		// No DelegationID and no WorktreePath: an ordinary implement job.
	}
	job := db.Job{ID: "normal-impl", Type: "implement"}
	_, err := worker.defaultCheckout(ctx, job, normal, runtime.Agent{Name: "impl-agent"})
	if err == nil {
		t.Fatal("defaultCheckout accepted a normal implement job whose branch does not match the checkout")
	}
	if !strings.Contains(err.Error(), "checkout branch is main, not job branch feature-x") {
		t.Fatalf("expected the branch-identity guard to fire, got: %v", err)
	}
}

// T3 worktree delegation child unaffected: a delegation child WITH a worktree
// (DelegationID + WorktreePath set) still routes through the existing
// isDelegationWorktreeChild validation in validateTargetCheckout — a wrong-branch
// worktree fails, a right-branch worktree passes. The new escape (gated on an
// EMPTY WorktreePath) must not touch this path.
func TestDefaultCheckoutLeavesWorktreeDelegationChildValidated(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	// Right-branch worktree: the child's worktree is on its job branch -> passes.
	rightCheckout := createDaemonWorkerGitCheckout(t, "feature-x")
	seedDaemonWorkerRepo(t, store, "owner/repo", rightCheckout)
	worker := defaultJobWorker(store, io.Discard)
	agent := runtime.Agent{Name: "impl-agent"}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "feature-x", Owner: agent.Name}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned (%v, %v)", acquired, err)
	}
	rightChild := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "feature-x",
		DelegationID: "d-impl",
		ParentJobID:  "coordinator-job",
		WorktreePath: rightCheckout, // worktree present and on the job branch
	}
	if _, err := worker.defaultCheckout(ctx, db.Job{ID: "wt-right", Type: "implement"}, rightChild, agent); err != nil {
		t.Fatalf("defaultCheckout rejected a worktree delegation child on its job branch: %v", err)
	}

	// Wrong-branch worktree: the child's worktree is on `main`, not its job branch
	// -> the existing isDelegationWorktreeChild branch check still rejects it. This
	// proves the escape does not bypass validation for worktree-bearing children.
	wrongStore := daemonWorkerStore(t)
	wrongCheckout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, wrongStore, "owner/repo", wrongCheckout)
	wrongWorker := defaultJobWorker(wrongStore, io.Discard)
	if acquired, err := wrongStore.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "feature-x", Owner: agent.Name}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned (%v, %v)", acquired, err)
	}
	wrongChild := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "feature-x",
		DelegationID: "d-impl",
		ParentJobID:  "coordinator-job",
		WorktreePath: wrongCheckout, // worktree present but on the wrong branch
	}
	_, err := wrongWorker.defaultCheckout(ctx, db.Job{ID: "wt-wrong", Type: "implement"}, wrongChild, agent)
	if err == nil {
		t.Fatal("defaultCheckout accepted a worktree delegation child on the wrong branch")
	}
	if !strings.Contains(err.Error(), "not job branch feature-x") {
		t.Fatalf("expected the worktree branch check to fire, got: %v", err)
	}
}

// T4 read-only review escape: a worktree-less delegation review child (no
// TaskID/PR pairing, so it takes the validateTargetCheckout branch) runs against
// the shared `main` checkout with NO error. Review is read-only, so the escape is
// trivially safe and there is no branch lock involved.
func TestDefaultCheckoutEscapesWorktreelessDelegationReviewChild(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	child := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "feature-x",
		DelegationID: "d-review",
		ParentJobID:  "coordinator-job",
		// No WorktreePath, no TaskID, no PullRequest: the else-arm validateTargetCheckout path.
	}
	job := db.Job{ID: "review-child", Type: "review"}
	got, err := worker.defaultCheckout(ctx, job, child, runtime.Agent{Name: "review-agent"})
	if err != nil {
		t.Fatalf("defaultCheckout rejected a worktree-less review child: %v", err)
	}
	if got != checkout {
		t.Fatalf("defaultCheckout = %q, want shared checkout %q", got, checkout)
	}
}

// A normal review job (no delegation) with a genuine branch mismatch against the
// `main` checkout STILL fails — the review-arm escape, like the implement arm, is
// gated strictly on the worktree-less delegation child.
func TestDefaultCheckoutKeepsBranchGuardForNormalReviewJob(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	normal := workflow.JobPayload{
		Repo:   "owner/repo",
		Branch: "feature-x",
		// No DelegationID, no WorktreePath, no TaskID/PR -> validateTargetCheckout.
	}
	job := db.Job{ID: "normal-review", Type: "review"}
	_, err := worker.defaultCheckout(ctx, job, normal, runtime.Agent{Name: "review-agent"})
	if err == nil {
		t.Fatal("defaultCheckout accepted a normal review job whose branch does not match the checkout")
	}
	if !strings.Contains(err.Error(), "checkout branch is main, not job branch feature-x") {
		t.Fatalf("expected the branch-identity guard to fire, got: %v", err)
	}
}

// isWorktreeLessDelegationChild is the exact complement of
// isDelegationWorktreeChild and must NOT fire for a continuation (which carries a
// ParentJobID but an empty DelegationID and routes through the ask arm). This
// pins the predicate's "DelegationID, not ParentJobID" contract.
func TestIsWorktreeLessDelegationChildPredicate(t *testing.T) {
	cases := []struct {
		name    string
		payload workflow.JobPayload
		want    bool
	}{
		{"delegation leg, no worktree", workflow.JobPayload{DelegationID: "d1"}, true},
		{"delegation leg, no worktree, with parent", workflow.JobPayload{DelegationID: "d1", ParentJobID: "p1"}, true},
		{"delegation leg WITH worktree", workflow.JobPayload{DelegationID: "d1", WorktreePath: "/tmp/wt"}, false},
		{"continuation: parent but no delegation id", workflow.JobPayload{ParentJobID: "p1"}, false},
		{"ordinary job", workflow.JobPayload{}, false},
		{"whitespace-only delegation id", workflow.JobPayload{DelegationID: "   "}, false},
		{"whitespace-only worktree path counts as empty", workflow.JobPayload{DelegationID: "d1", WorktreePath: "   "}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorktreeLessDelegationChild(tc.payload); got != tc.want {
				t.Fatalf("isWorktreeLessDelegationChild(%+v) = %v, want %v", tc.payload, got, tc.want)
			}
			// Complement invariant: the two predicates are mutually exclusive when a
			// DelegationID is present.
			if strings.TrimSpace(tc.payload.DelegationID) != "" {
				if isWorktreeLessDelegationChild(tc.payload) == isDelegationWorktreeChild(tc.payload) {
					t.Fatalf("predicates are not complementary for %+v", tc.payload)
				}
			}
		})
	}
}
