package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// #1531: the merge gate's independence check compared reviewer and implementer
// AGENT NAMES, never runtime or model, so two agents with different names and
// the same family satisfied a gate whose entire purpose is cross-family review.
//
// MEASURED INSTANCE, PR #1527 at head 2e0dd2ee: implementer `wave-impl` and
// reviewer `g7-review` are both codex/gpt-5.6-sol. `g7-review != wave-impl` as
// strings, so the self-approval check passed. That panel was caught only because
// the gate failed closed for an unrelated bookkeeping reason and never reached
// this test, which is the worse shape: a noisy failure mode masking a silent one.
//
// WHY THIS IS IMPLEMENTABLE NOW AND WAS NOT WHEN THE ISSUE WAS FILED. Its own
// comment defers the fix because `effective_runtime` was recorded on 11% of
// review jobs, and "doing (2) without (1) produces a gate that silently passes
// whenever the field is absent". Re-measured on jobs since 2026-08-25, with the
// agent-registry fallback counted: reviews 1335 of 1432 resolvable, implement
// jobs 427 of 449. The prerequisite landed as #1528, and `ResolveRuntimeFamily`
// already names this gate as its second consumer.
func TestMergeGateRefusesASameFamilyApprovalWithADifferentAgentName(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "wave-impl", "codex")
	seedFamilyAgent(t, store, "g7-review", "codex")
	gate := PolicyMergeGate{Store: store}

	implementers := map[string]implementerIdentity{
		"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"},
	}
	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "g7-review", "codex", implementers)
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if !same {
		t.Fatal("a reviewer on the implementer's own runtime family was accepted as independent; the gate's bar is cross-family review and distinct agent names are not evidence of independence")
	}
	for _, want := range []string{"g7-review", "codex", "wave-impl", "cross-family"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("refusal reason %q does not name %q, so an operator cannot act on it", reason, want)
		}
	}
}

// The control that decides whether this is an independence check or a blanket
// refusal: a genuinely different family must still pass.
func TestMergeGateAcceptsACrossFamilyApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "wave-impl", "codex")
	seedFamilyAgent(t, store, "gm-review-opus", "claude")
	gate := PolicyMergeGate{Store: store}

	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "gm-review-opus", "claude",
		map[string]implementerIdentity{"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if same {
		t.Fatalf("a cross-family approval was refused as same-family: %q", reason)
	}
}

// THE RECORDED RUNTIME WINS OVER THE REGISTRY DEFAULT, which is what makes an
// override-run job attribute correctly. Both agents are registered as codex, but
// the reviewer actually RAN on kimi, so it is independent despite the registry.
// Without this precedence the gate would refuse a legitimate override.
func TestMergeGateUsesTheRuntimeAJobActuallyRanOn(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "wave-impl", "codex")
	seedFamilyAgent(t, store, "g7-review", "codex")
	gate := PolicyMergeGate{Store: store}

	same, _, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "g7-review", "kimi",
		map[string]implementerIdentity{"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if same {
		t.Fatal("a reviewer that RAN on kimi was refused because its registry default is codex; the runtime recorded on the job must win")
	}
}

// AN UNRESOLVABLE FAMILY MUST NOT READ AS CLEAN. It falls through to the name
// check, exactly as before, and records why it could not be compared. The issue
// warns about precisely the opposite outcome: "a gate that silently passes
// whenever the field is absent", which looks installed and is not.
//
// Refusing on the residue is not an option today: it is dominated by EPHEMERAL
// and TEMP agents, which are deliberately absent from the registry, and the
// native review fanout dispatches its lens legs as ephemeral by construction.
func TestMergeGateRecordsAnUnresolvableFamilyRatherThanClaimingAClean(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "wave-impl", "codex")
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "lens-ephemeral-abc", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 1527,
	})
	gate := PolicyMergeGate{Store: store}

	// The reviewer is an ephemeral agent: not in the registry, no recorded runtime.
	same, _, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "lens-ephemeral-abc", "",
		map[string]implementerIdentity{"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if same {
		t.Fatal("an unresolvable family was reported as a same-family match; absence is not evidence")
	}
	events, err := store.ListJobEvents(ctx, "review-job")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "merge_gate_family_unresolved" && strings.Contains(event.Message, "lens-ephemeral-abc") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no merge_gate_family_unresolved event naming the reviewer; the gate would report a check it never performed. events = %+v", events)
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

// THE GATE-LEVEL DISCRIMINATOR, and the test the issue actually asked for: "a
// firing test where reviewer and implementer have DIFFERENT NAMES and the SAME
// family, asserting the gate REFUSES".
//
// It is the PR #1527 shape end to end through Evaluate, with a real merge client
// that would perform the merge if the gate let it. The unit tests above cannot
// serve as a before/after because the helper they call does not exist on base;
// this one does, so it measures behaviour rather than compilation.
func TestPolicyMergeGateRefusesSameFamilyApprovalEndToEnd(t *testing.T) {
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
	reviewPayload.Result = &AgentResult{Decision: "approved", Summary: "approved"}
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
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("the gate MERGED a head whose only approval came from the implementer's own runtime family: decision=%+v merges=%+v", decision, gh.merges)
	}
	if !strings.Contains(decision.Reason.Render(), "same family as implementer") {
		t.Fatalf("decision reason = %q, want it to name the family collision", decision.Reason.Render())
	}
}
