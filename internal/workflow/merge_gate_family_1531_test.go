package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// Runtime-family comparison is a secondary diversity signal. Different agent
// identities satisfy the merge gate's independence requirement even when their
// runtime families match; the shared family remains visible as an advisory.
func TestMergeGateReportsASameFamilyAdvisoryForDifferentAgentNames(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "wave-impl", "codex")
	seedFamilyAgent(t, store, "g7-review", "codex")
	gate := PolicyMergeGate{Store: store}

	implementers := map[string]implementerIdentity{
		"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"},
	}
	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "g7-review", false, "codex", implementers)
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if !same {
		t.Fatal("a same-family review did not produce the expected diversity advisory")
	}
	for _, want := range []string{"g7-review", "codex", "wave-impl", "advisory"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("advisory reason %q does not name %q", reason, want)
		}
	}
}

// A genuinely different family needs no diversity advisory.
func TestMergeGateAcceptsACrossFamilyApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "wave-impl", "codex")
	seedFamilyAgent(t, store, "gm-review-opus", "claude")
	gate := PolicyMergeGate{Store: store}

	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "gm-review-opus", false, "claude",
		map[string]implementerIdentity{"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if same {
		t.Fatalf("a cross-family approval produced a same-family advisory: %q", reason)
	}
}

// The runtime recorded on the job wins over the registry default. Both agents
// are registered as codex, but the reviewer actually ran on kimi, so no
// same-family advisory is warranted.
func TestMergeGateUsesTheRuntimeAJobActuallyRanOn(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "wave-impl", "codex")
	seedFamilyAgent(t, store, "g7-review", "codex")
	gate := PolicyMergeGate{Store: store}

	same, _, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "g7-review", false, "kimi",
		map[string]implementerIdentity{"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if same {
		t.Fatal("a reviewer that ran on kimi was compared using its codex registry default")
	}
}

// An unresolved family is disclosed rather than silently presented as diverse.
// It is advisory because reviewer identity and substantive evidence are the hard
// merge requirements.
func TestMergeGateRecordsAnUnresolvableFamilyAdvisory(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "wave-impl", "codex")
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "lens-ephemeral-abc", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 1527,
	})
	gate := PolicyMergeGate{Store: store}

	// The reviewer is an ephemeral agent with no parent recorded: not in the
	// registry, no recorded runtime, and nothing for #2004's parent recovery to
	// walk to. #1531 shipped this falling THROUGH to the name check because
	// refusing then would have blocked the native review fanout. #2004 removed
	// that objection by making the fanout's own ephemeral legs resolvable, so what
	// remains here is a genuinely unknown family, and the gate now refuses it.
	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "lens-ephemeral-abc", false, "",
		map[string]implementerIdentity{"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if !same {
		t.Fatal("an unresolvable family did not produce an advisory")
	}
	if !strings.Contains(reason, "lens-ephemeral-abc") {
		t.Fatalf("advisory reason = %q, want the unresolvable reviewer named", reason)
	}
	events, err := store.ListJobEvents(ctx, "review-job")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == mergeGateFamilyUnresolvedEventKind && strings.Contains(event.Message, "lens-ephemeral-abc") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no unresolved-family advisory event naming the reviewer; events = %+v", events)
	}
}

func seedFamilyAgent(t *testing.T, store *db.Store, name string, runtime string) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:         name,
		Runtime:      runtime,
		Capabilities: []string{"review", "implement"},
	}); err != nil {
		t.Fatalf("UpsertAgent(%s): %v", name, err)
	}
}

// The gate-level discriminator: same-family independent identities remain
// eligible, and the production path persists the advisory before merging.
func TestPolicyMergeGateAllowsSameFamilyIndependentApprovalEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	// Same family, different names: the exact PR #1527 pairing.
	seedFamilyAgent(t, store, "wave-impl", "codex")
	seedFamilyAgent(t, store, "g7-review", "codex")

	payload := JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 9,
		HeadSHA: "head123", TaskID: "task-9", ReviewRound: "review-1",
	}
	implementPayload := payload
	implementPayload.ReviewRound = ""
	implementPayload.EffectiveRuntime = "codex"
	implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "wave-impl", Type: "implement"}, implementPayload)

	reviewPayload := payload
	reviewPayload.EffectiveRuntime = "codex"
	reviewPayload.Result = &AgentResult{
		Decision: "approved", Summary: "approved",
		Evidence: "executed", EvidenceDeclared: true,
		TestsRun: []string{"focused production-path check"},
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "g7-review", Type: "review"}, reviewPayload)

	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || len(gh.merges) != 1 {
		t.Fatalf("the gate refused an independent substantive review solely because its runtime family matched: decision=%+v merges=%+v", decision, gh.merges)
	}
	events, err := store.ListJobEvents(ctx, "review-job")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == mergeGateFamilyAdvisoryEventKind && strings.Contains(event.Message, "same runtime family") {
			return
		}
	}
	t.Fatalf("same-family review merged without a persisted diversity advisory: events=%+v", events)
}
