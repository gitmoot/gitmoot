package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// #1850 round 3 F1, P1, ADOPTED AS A PERMANENT REGRESSION. The reviewer built
// this probe, proved the defect with it, and deleted it; it is the only executed
// evidence the class exists, so it lives here now.
//
// THE INVARIANT, NOT THE ASSIGNMENT. The #1822 ledger has two consumers that
// must agree on what is mandatory: the merge gate, which refuses a verdict
// naming an unobserved finding's uid, and the review brief, which is the only
// way a reviewer can learn that uid. When they disagree the merge wedges
// permanently.
//
// Round 2 wedged because the brief passed an empty scope. Round 3 wedged AGAIN
// through a narrower door: an engine field documented as "the SAME resolver the
// merge gate uses" that NOTHING EVER ASSIGNED, while the gate's copy was wired.
// A test asserting "engine.X != nil" would pin the assignment and miss the next
// variant, so this pins the property: whatever ledger resolvers the gate gets
// from production wiring, the engine must get too. Both now read the SAME
// LedgerResolvers value, so a divergence requires deleting a wiring line, and
// then this fails by name.
func TestLedgerResolversAreWiredOnBothConsumers(t *testing.T) {
	// The construction shape the existing engine test uses: a real checkout path
	// and a GhClient. No store is needed because only the wired seams matter.
	runner := &p2ProbeSubprocessRunner{}
	gh := &github.GhClient{MaxRetries: 1, Limiter: github.NewRateLimiter(github.RateLimiterConfig{})}
	checkout := t.TempDir()

	engine := daemonWorkflowEngineForRunner(nil, gh, checkout, "", runner, nil)
	gate := newDaemonPolicyMergeGateForRunner(nil, gh, checkout, runner)

	gateChanged := gate.LedgerResolvers.ChangedSince != nil
	gateAncestor := gate.LedgerResolvers.IsAncestor != nil
	gatePathExists := gate.LedgerResolvers.PathExistsAtHead != nil
	engineChanged := engine.LedgerResolvers.ChangedSince != nil
	engineAncestor := engine.LedgerResolvers.IsAncestor != nil
	enginePathExists := engine.LedgerResolvers.PathExistsAtHead != nil

	if gatePathExists != enginePathExists {
		t.Fatalf("locator-existence resolver wired on gate=%v but engine=%v; the gate can demand an obligation the brief cannot disclose, which wedges the merge permanently",
			gatePathExists, enginePathExists)
	}
	if gateChanged != engineChanged {
		t.Fatalf("changed-files resolver wired on gate=%v but engine=%v; the two consumers would compute different obligation sets",
			gateChanged, engineChanged)
	}
	if gateAncestor != engineAncestor {
		t.Fatalf("ancestry resolver wired on gate=%v but engine=%v; the gate and brief would disagree about findings from rewritten branch lines",
			gateAncestor, engineAncestor)
	}
	// POSITIVE CONTROL: with a checkout present BOTH must be wired, so the
	// equality above cannot be satisfied by both sides being nil.
	if !gatePathExists || !gateChanged || !gateAncestor {
		t.Fatalf("with a checkout present the gate must hold every ledger resolver, got changed=%v ancestor=%v pathExists=%v; the equality would otherwise be vacuous",
			gateChanged, gateAncestor, gatePathExists)
	}
}

func TestDaemonLedgerAncestryResolverDistinguishesDivergedHeads(t *testing.T) {
	repo, base := gitFixtureRepo(t, "base\n")
	runGit(t, repo, "checkout", "-b", "observed")
	if err := os.WriteFile(filepath.Join(repo, "observed.txt"), []byte("finding branch\n"), 0o600); err != nil {
		t.Fatalf("write observed branch: %v", err)
	}
	runGit(t, repo, "add", "observed.txt")
	runGit(t, repo, "commit", "-m", "observed branch")
	observed := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))

	runGit(t, repo, "checkout", "-b", "current", base)
	if err := os.WriteFile(filepath.Join(repo, "current.txt"), []byte("rewritten branch\n"), 0o600); err != nil {
		t.Fatalf("write current branch: %v", err)
	}
	runGit(t, repo, "add", "current.txt")
	runGit(t, repo, "commit", "-m", "current branch")
	current := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))

	resolver := daemonLedgerIsAncestor(nil, repo, subprocess.ExecRunner{})
	if resolver == nil {
		t.Fatal("daemon ledger ancestry resolver is nil for a checkout")
	}
	got, err := resolver(context.Background(), "owner/repo", 2173, observed, current)
	if err != nil {
		t.Fatalf("resolve diverged ancestry: %v", err)
	}
	if got {
		t.Fatalf("observed head %s reported as ancestor of diverged head %s", observed, current)
	}
	// Positive control: the same resolver must recognize the shared base.
	got, err = resolver(context.Background(), "owner/repo", 2173, base, current)
	if err != nil {
		t.Fatalf("resolve ancestor: %v", err)
	}
	if !got {
		t.Fatalf("base head %s was not recognized as ancestor of %s", base, current)
	}
}

func TestDaemonLedgerAncestryResolverFallsBackToCompareStatus(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{status: "ahead", want: true},
		{status: "identical", want: true},
		{status: "behind", want: false},
		{status: "diverged", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			client := &reviewCompareClient{result: github.CompareResult{Status: tt.status}}
			resolver := daemonLedgerIsAncestor(client, "", nil)
			got, err := resolver(context.Background(), "owner/repo", 2173, strings.Repeat("a", 40), strings.Repeat("b", 40))
			if err != nil {
				t.Fatalf("resolve compare status %q: %v", tt.status, err)
			}
			if got != tt.want {
				t.Fatalf("compare status %q resolved ancestor=%v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestDaemonLedgerAncestryResolverFallsBackWhenReviewedHeadIsMissingLocally(t *testing.T) {
	repo, current := gitFixtureRepo(t, "current\n")
	missingReviewedHead := strings.Repeat("a", 40)
	client := &reviewCompareClient{result: github.CompareResult{Status: "diverged"}}
	resolver := daemonLedgerIsAncestor(client, repo, subprocess.ExecRunner{})

	got, err := resolver(context.Background(), "owner/repo", 2173, missingReviewedHead, current)
	if err != nil {
		t.Fatalf("resolve ancestry after local commit miss: %v", err)
	}
	if got {
		t.Fatal("compare fallback reported a missing, diverged reviewed head as an ancestor")
	}
	if client.calls != 1 || client.base != missingReviewedHead || client.head != current {
		t.Fatalf("compare calls=%d base=%q head=%q, want one exact-head fallback for %q...%q",
			client.calls, client.base, client.head, missingReviewedHead, current)
	}
}
