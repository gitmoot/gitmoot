package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1850 F6, P2, AND IT IS THE ONE THAT MATTERS MOST. The prior version of this
// file claimed a routing mutant would not survive; measured, it did. Deleting
// the EnsureLedgerObligationsObserved call from merge_gate.go left the ENTIRE
// internal/workflow package green, so the guards pinned the FUNCTION while the
// call site was unpinned - which is exactly how F1 (no production writer) and
// F4 (nil change set) could both be true with a passing suite.
//
// This test enters through PolicyMergeGate.ensureFinalReviewCaptured, the
// production acceptance path, with a store holding an unobserved open finding.
// Delete the ledger call and this fails by name.
func TestPolicyMergeGateRefusesWhenAPriorFindingIsUnobservedAtThisHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "mobile/app", PullRequest: 9, HeadSHA: headA, ObserverJob: "review-1",
		State: db.FindingOpen, Severity: "P1", RoundLabel: "F-1", Title: "unfixed defect",
		File: "internal/run.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test ./internal/ -> FAIL"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	// A fully reported, approved review at the new head. Everything the gate
	// normally demands is present, so the ONLY thing that can refuse here is the
	// ledger.
	insertLedgerGateReview(t, store, headB)

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, headB)
	if err == nil {
		t.Fatal("the gate accepted a verdict at a head where a prior open finding carries no observation; the ledger call is not on the production path")
	}
	if !strings.Contains(err.Error(), "findings ledger") {
		t.Fatalf("gate refused for the wrong reason: %v", err)
	}
	if !strings.Contains(err.Error(), "mobile/app#9-f1") {
		t.Fatalf("refusal does not name the finding uid, so it is not actionable: %v", err)
	}
}

// The passing case for the same path, written first and kept passing: the SAME
// gate, the SAME reviewer, with the finding observed at this head, must merge.
// A guard that cannot accept a legitimate verdict is worse than no guard.
func TestPolicyMergeGateAcceptsWhenThePriorFindingIsObservedHere(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "mobile/app", PullRequest: 9, HeadSHA: headA, ObserverJob: "review-1",
		State: db.FindingOpen, Severity: "P1", Title: "defect", File: "internal/run.go",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "mobile/app", PullRequest: 9, HeadSHA: headB, ObserverJob: "review-2",
		ContinuesUID: uid, State: db.FindingAnswered, Severity: "P1", Title: "defect",
		File: "internal/run.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test ./internal/ -> ok"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("observe at headB: %v", err)
	}
	insertLedgerGateReview(t, store, headB)

	if err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, headB); err != nil {
		t.Fatalf("the gate refused a verdict that observed every prior finding: %v", err)
	}
}

// insertLedgerGateReview inserts one independent, head-bound, fully reported
// approval so the ONLY thing left that can refuse the gate is the ledger.
func insertLedgerGateReview(t *testing.T, store *db.Store, head string) {
	t.Helper()
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-ledger", Agent: "g6-review-sol", Type: "review"}, JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: head,
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "exact-head review",
			TestsRun: []string{"go test ./internal/ -> ok"},
		},
	})
}
