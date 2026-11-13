package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// The #413 escape: a worktree-less delegation child (a delegation leg with an
// empty WorktreePath) inherits its coordinator's branch but can only resolve the
// repo's registered shared checkout, which sits on `main`. The review arm of
// defaultCheckoutForRunner must not validate that shared checkout against
// payload.Branch for that child only. These tests reuse the daemon checkout
// seams (a temp git checkout on `main`, the real defaultCheckoutForRunner).

// T4 read-only review escape: a worktree-less delegation review child (no
// TaskID/PR pairing, so it takes the validateTargetCheckoutForRunner branch) runs against
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
		// No WorktreePath, no TaskID, no PullRequest: the else-arm validateTargetCheckoutForRunner path.
	}
	job := db.Job{ID: "review-child", Type: "review"}
	got, err := worker.defaultCheckoutForRunner(ctx, job, child, runtime.Agent{Name: "review-agent"}, subprocess.ExecRunner{})
	if err != nil {
		t.Fatalf("defaultCheckoutForRunner rejected a worktree-less review child: %v", err)
	}
	if got != checkout {
		t.Fatalf("defaultCheckoutForRunner = %q, want shared checkout %q", got, checkout)
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
		// No DelegationID, no WorktreePath, no TaskID/PR -> validateTargetCheckoutForRunner.
	}
	job := db.Job{ID: "normal-review", Type: "review"}
	_, err := worker.defaultCheckoutForRunner(ctx, job, normal, runtime.Agent{Name: "review-agent"}, subprocess.ExecRunner{})
	if err == nil {
		t.Fatal("defaultCheckoutForRunner accepted a normal review job whose branch does not match the checkout")
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
