package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestNewDaemonPolicyMergeGateIncludesWorktreeCleaner(t *testing.T) {
	gate := newDaemonPolicyMergeGateForRunner(nil, githubtest.NoopClient{}, "/tmp/gitmoot-checkout", subprocess.ExecRunner{})

	if gate.Worktrees == nil {
		t.Fatal("daemon merge gate missing worktree cleaner")
	}
	if gate.CheckoutPath != "/tmp/gitmoot-checkout" {
		t.Fatalf("checkout path = %q", gate.CheckoutPath)
	}
	if !gate.DeleteBranch {
		t.Fatal("daemon merge gate should delete merged branches")
	}
}

func TestDaemonMergeGateCanBeDisabledByEnvironment(t *testing.T) {
	t.Setenv("GITMOOT_DISABLE_NATIVE_MERGE_GATE", "1")
	gate := daemonMergeGate{}

	decision, err := gate.Evaluate(context.Background(), workflow.MergeRequest{
		Repo:        "owner/repo",
		PullRequest: 1,
	})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready {
		t.Fatalf("decision.Ready = true, want disabled gate to block")
	}
	if !strings.Contains(decision.Reason.Render(), "GITMOOT_DISABLE_NATIVE_MERGE_GATE") {
		t.Fatalf("decision reason = %q", decision.Reason)
	}
}

func TestDaemonMergeGatePreservesInjectedGitHubClient(t *testing.T) {
	fake := githubtest.NoopClient{}
	gate := daemonMergeGate{GitHub: fake}

	if got := gate.githubClient("/tmp/checkout"); got != fake {
		t.Fatalf("github client = %#v, want injected fake %#v", got, fake)
	}
}
