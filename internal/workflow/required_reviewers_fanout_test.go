package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

// Phantom-verdict class #1351/#1417/#1557, D0. allRequiredReviewersApproved
// SEEDS the reviewer whose job is being advanced, before the loop reads any
// stored row. The loop's own ResultIsFanOut skip therefore cannot reach that
// boundary: it stops other fan-out rows from crediting a slot, never the
// coordinator crediting ITSELF.
//
// Reachability is source-verified rather than assumed: a ready outcome here
// leads to setTaskState(TaskReadyToMerge) at engine_routing_merge.go:739, :787
// and :843, and the panel-specific defence (highRiskLensQuorumMet) only runs
// when payload.RiskTier is RiskTierHigh - empty on the #1910 panel that
// motivated this.
func TestAllRequiredReviewersApprovedRefusesAFanOutCoordinatorsOwnSlot(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store, ReviewBlockingSeverity: func(string) string { return reviewseverity.P2 }}

	payload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 1910,
		TaskID:      "review-pr-1910",
		ReviewRound: "review-1",
		Reviewers:   []string{"panel-coordinator"},
		Result: &AgentResult{
			Decision:    "approved",
			Summary:     "Exact-head dispatch only, not the final verdict.",
			Delegations: []Delegation{{ID: "lens-correctness"}, {ID: "lens-security"}, {ID: "lens-regression"}},
		},
	}
	if !ResultIsFanOut(payload.Result) {
		t.Fatal("fixture is not a fan-out, so it cannot exercise the coordinator's own slot")
	}

	ready, err := engine.allRequiredReviewersApproved(ctx, "panel-coordinator", payload)
	if err != nil {
		t.Fatalf("allRequiredReviewersApproved returned error: %v", err)
	}
	if ready {
		t.Fatal("a fan-out coordinator satisfied its own required-reviewer slot; it announced a panel and approved nothing")
	}
}

// The paired control, and the half a one-sided guard would break: an ordinary
// reviewer must still credit its own slot from the seed, because its row is the
// one being advanced and the loop has not stored it yet.
func TestAllRequiredReviewersApprovedStillCreditsAnOrdinaryReviewersOwnSlot(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store, ReviewBlockingSeverity: func(string) string { return reviewseverity.P2 }}

	payload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 1910,
		TaskID:      "review-pr-1910",
		ReviewRound: "review-1",
		Reviewers:   []string{"solo-reviewer"},
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "read the diff, ran the suite",
		},
	}
	if ResultIsFanOut(payload.Result) {
		t.Fatal("control fixture must NOT be a fan-out")
	}

	ready, err := engine.allRequiredReviewersApproved(ctx, "solo-reviewer", payload)
	if err != nil {
		t.Fatalf("allRequiredReviewersApproved returned error: %v", err)
	}
	if !ready {
		t.Fatal("an ordinary reviewer lost credit for its own slot; the fan-out exclusion must not reject valid input")
	}
}
