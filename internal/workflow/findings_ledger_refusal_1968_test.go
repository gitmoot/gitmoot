package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1968, THE PROPERTY THE WHOLE REFUSAL RESTS ON.
//
// PHOBOS's condition on this slice, after gm-integrity found a live P0 whose
// only record was a review job nobody read: a P0 with no title must not be
// silently dropped by the gate that refuses it. The refusal is safe only if it
// costs the itemised obligation and NOT the merge block.
//
// It is safe, and this is why: the merge gate reads the review's VERDICT, not
// the ledger. effectiveReviewDecisionForPayload takes Result.Decision and
// Result.Severity, so a changes_requested P0 review whose every finding was
// refused still refuses the head. This test exists because that is an invariant
// of a DIFFERENT file than the one #1968 changes, so nothing else would notice
// if it stopped holding, and the refusal would silently become a way to unblock
// a head by saying nothing.
func TestRefusedFindingsDoNotUnblockTheHeadTheyWereReportedAgainst(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-p0", Agent: "reviewer", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested",
			Summary:  "a blocking concern the reviewer never articulated",
			Severity: "P0",
		},
	})

	// The premise: the ledger is empty, exactly as it would be if every finding
	// in that verdict had been refused for articulating nothing.
	rows, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 9)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("premise broken: ledger holds %d row(s), want 0", len(rows))
	}

	err = (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "reviewer",
		ReviewBlockingSeverity: "P2",
	}, "head123")
	var blocked mergeBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("gate returned %T %v, want mergeBlocked; if a zero-row ledger stops blocking, refusing a bare P0 finding becomes a way to merge a head by saying nothing", err, err)
	}
}
