package workflow

import (
	"context"
	"testing"
)

// pipelineFanOutResult is the shape a review-stage coordinator returns: an
// approved announcement whose delegations[] name the panel. Written as raw agent
// output so the test enters through the real settlement path.
const pipelineFanOutResult = `{"gitmoot_result":{"decision":"approved","summary":"Convening a three-reviewer panel on the PR with diverse lenses.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"lens-a","agent":"audit","action":"review","prompt":"review for correctness and security"},{"id":"lens-b","agent":"audit","action":"review","prompt":"review for performance and maintainability"}]}}`

// #1685 P1. Mailbox.Run STRIPS delegations for every non-orchestrate pipeline
// job, so a stage can never spawn phantom children. That strip also erased the
// only evidence that the result was a fan-out, and an approved coordinator
// announcement then reached pipeline auto-merge indistinguishable from a real
// leaf approval. A review stage cannot set orchestrate:true, so this seam is the
// ONLY path a production pipeline fan-out takes.
//
// Asserted on the PERSISTED payload, because that is what every downstream
// consumer reads. A fixture that sets Delegations on a stored payload after
// settlement never crosses this seam and cannot observe the defect.
func TestMailboxRunPreservesFanOutClassificationAcrossPipelineStrip(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	adapter := &fakeDelivery{outputs: []string{pipelineFanOutResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "pipeline-review", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot",
		Sender: PipelineJobSender,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	result, err := mailbox.Run(ctx, "pipeline-review", shellAgent(), adapter)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The instructions are gone: a leaf stage must never spawn children.
	if len(result.Delegations) != 0 {
		t.Fatalf("pipeline stage kept %d delegation(s); a leaf must not fan out", len(result.Delegations))
	}
	// The classification survives, and the canonical predicate still sees it.
	if !result.FanOut {
		t.Fatal("stripping the delegations erased the fan-out classification")
	}
	if !ResultIsFanOut(&result) {
		t.Fatal("ResultIsFanOut is blind to a stripped pipeline fan-out")
	}

	job, err := store.GetJob(ctx, "pipeline-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.Result == nil {
		t.Fatal("settled job persisted no result")
	}
	if len(payload.Result.Delegations) != 0 {
		t.Fatalf("persisted payload kept delegations: %+v", payload.Result.Delegations)
	}
	if !ResultIsFanOut(payload.Result) {
		t.Fatalf("the PERSISTED payload is not classified as a fan-out: %+v", payload.Result)
	}
}

// ACCEPTANCE: an ordinary pipeline review leaf must not be stamped. A guard that
// marks every stage a fan-out would stop pipeline auto-merge entirely.
func TestMailboxRunLeavesOrdinaryPipelineLeafUnclassified(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"Reviewed the diff at this head; no blocking issues.","findings":[],"changes_made":[],"tests_run":["go test ./... -> ok"],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "pipeline-leaf", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot",
		Sender: PipelineJobSender,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	result, err := mailbox.Run(ctx, "pipeline-leaf", shellAgent(), adapter)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.FanOut || ResultIsFanOut(&result) {
		t.Fatalf("an ordinary leaf approval was classified as a fan-out: %+v", result)
	}
}
