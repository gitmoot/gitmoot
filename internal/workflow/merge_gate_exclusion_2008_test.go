package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// seedGateReview persists a review row for task-9 on mobile/app#9.
func seedGateReview(t *testing.T, store *db.Store, jobID, headSHA string, externallyDriven bool) {
	t.Helper()
	ctx := context.Background()
	encoded, err := marshalPayload(JobPayload{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9",
		HeadSHA: headSHA, ReviewRound: "review-1",
		Result: &AgentResult{Decision: "approved", Summary: "seeded"},
	})
	if err != nil {
		t.Fatalf("marshalPayload(%s): %v", jobID, err)
	}
	job := db.Job{ID: jobID, Agent: "g6-review-sol", Type: "review", State: string(JobSucceeded), Payload: encoded}
	event := db.JobEvent{Kind: string(JobSucceeded), Message: "approved"}
	if externallyDriven {
		if err := store.CreateExternallyDrivenJobWithEvent(ctx, job, event); err != nil {
			t.Fatalf("CreateExternallyDrivenJobWithEvent(%s): %v", jobID, err)
		}
	} else if err := store.CreateJobWithEvent(ctx, job, event); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", jobID, err)
	}
	stored, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	if stored.ExternallyDriven != externallyDriven {
		t.Fatalf("stored ExternallyDriven = %v, want %v: the read path does not carry the column", stored.ExternallyDriven, externallyDriven)
	}
}

// TestMergeGateRecordsWhyAHeadlessReviewCannotFillASlot is #2008's last
// head-keyed consumer.
//
// A headless review still cannot enter reviewsAtHead and still cannot satisfy a
// reviewer slot. That is correct and unchanged: a verdict's head must be
// engine-observed, never caller-asserted (#1990). Only the silence goes.
func TestMergeGateRecordsWhyAHeadlessReviewCannotFillASlot(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("a", 40)
	seedGateReview(t, store, "session-gate-review", "", true)

	// The gate is expected to REFUSE: no review exists at this head. The refusal
	// is the unchanged behaviour; the record is the change.
	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, head)
	if err == nil {
		t.Fatal("a headless review satisfied a reviewer slot at this head: the exclusion became a head binding")
	}

	messages := headBoundExclusionEvents(t, store, "session-gate-review")
	if len(messages) != 1 {
		t.Fatalf("exclusion events = %d (%v), want 1", len(messages), messages)
	}
	if !strings.Contains(messages[0], "merge_gate.ensureFinalReviewCaptured") {
		t.Errorf("message = %q, want it to name the consumer", messages[0])
	}
	if !strings.Contains(messages[0], HeadBoundExclusionSessionRow) {
		t.Errorf("message = %q, want the session reason", messages[0])
	}
}

// TestMergeGateExclusionNeverWritesAHead is the campaign trap at the consumer
// where binding a head would be most damaging: this one decides merges.
func TestMergeGateExclusionNeverWritesAHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("a", 40)
	seedGateReview(t, store, "session-gate-review", "", true)

	_ = (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, head)

	job, err := store.GetJob(ctx, "session-gate-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if strings.TrimSpace(payload.HeadSHA) != "" {
		t.Fatalf("head_sha = %q, want empty: the gate made its exclusion legible by BINDING A HEAD", payload.HeadSHA)
	}
}

// TestMergeGateStaysSilentForAReviewAtAnotherHead is the arm that keeps the
// record meaningful. A row WITH an engine-observed head that is simply not this
// one was never the #2008 class.
func TestMergeGateStaysSilentForAReviewAtAnotherHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	seedGateReview(t, store, "other-head-gate-review", headB, false)

	_ = (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, headA)

	if messages := headBoundExclusionEvents(t, store, "other-head-gate-review"); len(messages) != 0 {
		t.Fatalf("exclusion events = %v, want none: this row has an engine-observed head", messages)
	}
}

// TestMergeGateExclusionRecordDoesNotGrowPerEvaluation pins the at-most-once
// guarantee at the consumer that runs most often. The gate is evaluated on every
// poll of a pull request in review.
func TestMergeGateExclusionRecordDoesNotGrowPerEvaluation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("a", 40)
	seedGateReview(t, store, "session-gate-review", "", true)
	gate := PolicyMergeGate{Store: store}
	request := MergeRequest{Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol"}

	for range 4 {
		_ = gate.ensureFinalReviewCaptured(ctx, request, head)
	}
	if messages := headBoundExclusionEvents(t, store, "session-gate-review"); len(messages) != 1 {
		t.Fatalf("exclusion events after 4 evaluations = %d, want 1", len(messages))
	}
}
