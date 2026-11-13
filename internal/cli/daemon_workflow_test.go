package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/daemon"
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

// TestBuildEscalationCommentIsNotParsedAsCommand pins the #340 regression where
// the escalation notification, posted by the daemon on its own PR, led a line
// with "@<handle> Gitmoot paused…" and so was parsed by ParseCommand as a
// "@<agent> <action=Gitmoot>" command — making the daemon reply with a spurious
// "unsupported command action 'Gitmoot'" ack. The body must @-mention the human
// (so GitHub still notifies) without any line that ParseCommand treats as a
// command.
func TestBuildEscalationCommentIsNotParsedAsCommand(t *testing.T) {
	req := workflow.EscalationRequest{
		CoordinatorJobID: "coord-123",
		DelegationID:     "failing-leg",
		Reason:           "child returned failed",
		Question:         "how should we proceed?",
	}
	body := buildEscalationComment("jerryfane", req)

	// The human is still mentioned (GitHub notification preserved)...
	if !strings.Contains(body, "@jerryfane") {
		t.Fatalf("escalation comment dropped the @-mention; body:\n%s", body)
	}
	// ...but never as the first token of a line (which ParseCommand would treat
	// as a `@<agent> <action>` command), and no line starts with a bare /gitmoot.
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "@") {
			t.Fatalf("escalation comment line starts with an @-mention (parses as a command): %q", line)
		}
		if fields[0] == "/gitmoot" {
			t.Fatalf("escalation comment line starts with a bare /gitmoot (parses as a command): %q", line)
		}
	}

	// And end-to-end: the daemon's own parser yields no command for any line of
	// the notification body, so the daemon never acks it as an (un)routable
	// command.
	for _, line := range strings.Split(body, "\n") {
		if cmd, ok := daemon.ParseCommand(line); ok {
			t.Fatalf("escalation comment line parsed into command %+v; want none: %q\nbody:\n%s", cmd, line, body)
		}
	}
}

// TestBuildAskGateComment pins the #445 ask-gate notification: when the request
// is flagged Ask, the comment quotes each question (id + prompt + choices) and
// offers the `answer` resume verb, NOT the failure retry/continue/abort verbs.
// It must also be unparseable as a command (same regression guard as #340).
func TestBuildAskGateComment(t *testing.T) {
	req := workflow.EscalationRequest{
		CoordinatorJobID: "coord-123",
		Ask:              true,
		Questions: []workflow.HumanQuestion{
			{ID: "q1", Prompt: "Target v2 or v3 API?", Choices: []string{"v2", "v3"}},
		},
	}
	body := buildEscalationComment("jerryfane", req)

	if !strings.Contains(body, "@jerryfane") {
		t.Fatalf("ask-gate comment dropped the @-mention; body:\n%s", body)
	}
	if !strings.Contains(body, "q1") || !strings.Contains(body, "Target v2 or v3 API?") {
		t.Fatalf("ask-gate comment must quote the question; body:\n%s", body)
	}
	if !strings.Contains(body, "choices: v2, v3") {
		t.Fatalf("ask-gate comment must render choices; body:\n%s", body)
	}
	if !strings.Contains(body, "resume coord-123 answer") {
		t.Fatalf("ask-gate comment must offer the answer verb; body:\n%s", body)
	}
	if strings.Contains(body, "retry <instructions>") || strings.Contains(body, "abort` —") {
		t.Fatalf("ask-gate comment must NOT offer the failure verbs; body:\n%s", body)
	}
	// Same command-injection guard as the failure comment.
	for _, line := range strings.Split(body, "\n") {
		if cmd, ok := daemon.ParseCommand(line); ok {
			t.Fatalf("ask-gate comment line parsed into command %+v; want none: %q\nbody:\n%s", cmd, line, body)
		}
	}
}

// #1277, coverage axis: finalizerRan x executor-identity-differs.
//
// The write-ahead persist above closes the #390 TOCTOU — the daemon's PR-watcher
// must never observe a PR for this branch with the skip flag still unwritten. It
// is correctly IDENTITY-BLIND in production (daemon_workflow.go: `if
// payload.SkipNativeReviewFanout`), but every committed finalizer fixture runs
// with job.Agent == payload.LeadAgent, so nothing pinned that blindness.
//
// A FINALIZER_REQUIRES_EXECUTOR_IDENTITY mutant — adding
// `&& payload.LeadAgent == job.Agent` to that condition — survived BOTH committed
// finalizer tests and all three cells of the workflow-package mixed-identity
// matrix. The shape it breaks is real: runtime_session_busy delegates execution
// to a TEMPORARY WORKER, so job.Agent is the worker while LeadAgent and the branch
// lock owner remain the original agent. Under the mutant the flag is not written
// ahead, and the PR opens on a branch whose lock still reads false.
//
// Test-only guard: production behaviour is unchanged, this freezes it.
