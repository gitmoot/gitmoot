package workflow

import (
	"context"
	"math"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// floatNear reports whether got is within eps of want; the price math is in
// floating dollars so equality checks must tolerate representation error.
func floatNear(got, want, eps float64) bool {
	return math.Abs(got-want) <= eps
}

// TestPriceForModelTiers pins the substring tiering (mirroring ruflo's
// modelTier()): a model id is matched case-insensitively against opus → sonnet →
// haiku (most-specific first), and anything unrecognized — including empty —
// falls back to the mid-tier (Sonnet) default rather than zero.
func TestPriceForModelTiers(t *testing.T) {
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{"claude-opus-4-8", ModelPrice{InputPerToken: 15.0 / 1e6, OutputPerToken: 75.0 / 1e6}},
		{"claude-sonnet-4-5", ModelPrice{InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6}},
		{"claude-3-5-HAIKU", ModelPrice{InputPerToken: 0.25 / 1e6, OutputPerToken: 1.25 / 1e6}},
		{"", defaultUnknownModelPrice},
		{"gpt-5.5", defaultUnknownModelPrice},
		{"  Opus  ", ModelPrice{InputPerToken: 15.0 / 1e6, OutputPerToken: 75.0 / 1e6}},
	}
	for _, tc := range cases {
		got := priceForModel(tc.model)
		if !floatNear(got.InputPerToken, tc.want.InputPerToken, 1e-12) ||
			!floatNear(got.OutputPerToken, tc.want.OutputPerToken, 1e-12) {
			t.Fatalf("priceForModel(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

// TestCostFromTokens pins the post-call accounting formula
// (input × input_price + output × output_price) and the negative-spend clamp:
// negative token counts contribute 0, so a bad usage write cannot refund cost.
func TestCostFromTokens(t *testing.T) {
	// 1,000,000 input + 1,000,000 output on Opus => $15 + $75 = $90.
	if got := costFromTokens("claude-opus-4-8", 1_000_000, 1_000_000); !floatNear(got, 90.0, 1e-9) {
		t.Fatalf("costFromTokens(opus, 1e6, 1e6) = %v, want 90.0", got)
	}
	// Sonnet: 1,000,000 input + 1,000,000 output => $3 + $15 = $18.
	if got := costFromTokens("claude-sonnet-4-5", 1_000_000, 1_000_000); !floatNear(got, 18.0, 1e-9) {
		t.Fatalf("costFromTokens(sonnet, 1e6, 1e6) = %v, want 18.0", got)
	}
	// Negative counts clamp to 0 — no refund past the cap.
	if got := costFromTokens("claude-opus-4-8", -5, -7); got != 0 {
		t.Fatalf("costFromTokens(opus, -5, -7) = %v, want 0 (clamped)", got)
	}
	// Unknown model is priced at the mid-tier fallback, never free.
	if got := costFromTokens("some-unknown-model", 1_000_000, 0); !floatNear(got, 3.0, 1e-9) {
		t.Fatalf("costFromTokens(unknown, 1e6, 0) = %v, want 3.0 (Sonnet fallback)", got)
	}
}

// seedCostTree builds a coordination tree rooted at rootID whose children carry a
// known model id and output token count, so the per-root dollar cost is
// deterministic. It mirrors seedTreeWithUsage but threads a Model through each
// child's payload so priceForModel resolves to a real tier rather than the
// unknown fallback.
func seedCostTree(t *testing.T, store *db.Store, rootID, model string, n, perChildTokens int, rootDelegations []Delegation) {
	t.Helper()
	ctx := context.Background()
	insertCompletedJob(t, store, db.Job{ID: rootID, Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-380", TaskID: "task-380", Sender: "coord", Model: model,
		Result: &AgentResult{Decision: "approved", Summary: "tree", Delegations: rootDelegations},
	})
	for i := 0; i < n; i++ {
		childID := rootID + "/child" + string(rune('a'+i))
		insertCompletedJob(t, store, db.Job{ID: childID, Agent: "w", Type: "ask"}, JobPayload{
			Repo: "jerryfane/gitmoot", Branch: "task-380", TaskID: "task-380", Sender: "w",
			RootJobID: rootID, Model: model,
			Result: &AgentResult{Decision: "approved", Summary: "child"},
		})
		if err := store.UpdateJobUsage(ctx, childID, 0, perChildTokens); err != nil {
			t.Fatalf("UpdateJobUsage(%s) returned error: %v", childID, err)
		}
	}
}

// TestSumRootDelegationCost pins the per-root cost aggregation: it prices each
// job's measured token usage through its model and sums the root coordinator plus
// every job pointing back at it, ignoring jobs from other roots.
func TestSumRootDelegationCost(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)

	// Root R on Opus: coordinator (1e6 in / 1e6 out => $90) + one Opus child
	// (1e6 out => $75) => $165 total.
	insertCompletedJob(t, store, db.Job{ID: "R", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", TaskID: "task-380", Sender: "coord", Model: "claude-opus-4-8",
		Result: &AgentResult{Decision: "approved", Summary: "root"},
	})
	if err := store.UpdateJobUsage(ctx, "R", 1_000_000, 1_000_000); err != nil {
		t.Fatalf("UpdateJobUsage(R) returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "R/c1", Agent: "w", Type: "ask"}, JobPayload{
		RootJobID: "R", Model: "claude-opus-4-8", Result: &AgentResult{Decision: "approved", Summary: "c1"},
	})
	if err := store.UpdateJobUsage(ctx, "R/c1", 0, 1_000_000); err != nil {
		t.Fatalf("UpdateJobUsage(R/c1) returned error: %v", err)
	}
	// Unrelated root S with usage that must NOT be counted toward R.
	insertCompletedJob(t, store, db.Job{ID: "S", Agent: "coord", Type: "ask"}, JobPayload{
		Model: "claude-opus-4-8", Result: &AgentResult{Decision: "approved", Summary: "other root"},
	})
	if err := store.UpdateJobUsage(ctx, "S", 1_000_000, 1_000_000); err != nil {
		t.Fatalf("UpdateJobUsage(S) returned error: %v", err)
	}

	got, err := engine.sumRootDelegationCost(ctx, "R")
	if err != nil {
		t.Fatalf("sumRootDelegationCost returned error: %v", err)
	}
	if want := 90.0 + 75.0; !floatNear(got, want, 1e-9) {
		t.Fatalf("sumRootDelegationCost(R) = %v, want %v", got, want)
	}
}

// TestEngineCostBudgetTripEnqueuesFinalize pins #380: a coordination tree whose
// measured cost has reached the dollar budget does not dispatch its next
// generation; instead it gets one graceful finalize continuation and emits
// delegation_cost_usd_exceeded — exactly mirroring the token-budget trip.
func TestEngineCostBudgetTripEnqueuesFinalize(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	engine := testEngine(store)
	engine.MaxDelegationCostUSD = 100.0

	// Two Opus children of 1,000,000 output tokens each => $75 + $75 = $150 spent
	// >= $100 budget.
	seedCostTree(t, store, "over", "claude-opus-4-8", 2, 1_000_000, []Delegation{
		{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
		{ID: "d1", Agent: "w", Action: "ask", Prompt: "work"},
	})

	if err := engine.AdvanceJob(ctx, "over"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if jobExists(t, store, "over/delegation/d0") || jobExists(t, store, "over/delegation/d1") {
		t.Fatal("over-budget delegations must not be dispatched")
	}
	if got := countJobEvents(t, store, "over", "delegation_cost_usd_exceeded"); got != 1 {
		t.Fatalf("delegation_cost_usd_exceeded events = %d, want 1", got)
	}
	cont, err := unmarshalPayload(mustJob(t, store, "over/continuation").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("finalize continuation must carry DelegationFinalize: %+v", cont)
	}
	if got := countJobEvents(t, store, "over", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
}

// TestEngineWithinCostBudgetDispatches is the control: a tree whose measured cost
// is strictly under the dollar budget dispatches its next generation normally and
// emits no cost-exceeded event.
func TestEngineWithinCostBudgetDispatches(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	engine := testEngine(store)
	engine.MaxDelegationCostUSD = 100.0

	// One Opus child of 1,000,000 output tokens => $75 spent < $100 budget.
	seedCostTree(t, store, "under", "claude-opus-4-8", 1, 1_000_000, []Delegation{
		{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
	})

	if err := engine.AdvanceJob(ctx, "under"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if !jobExists(t, store, "under/delegation/d0") {
		t.Fatal("in-budget delegations must be dispatched")
	}
	if got := countJobEvents(t, store, "under", "delegation_cost_usd_exceeded"); got != 0 {
		t.Fatalf("delegation_cost_usd_exceeded events = %d, want 0", got)
	}
}

// TestEngineZeroCostBudgetIsUnlimited is the byte-identical-default control: with
// the cost budget left at 0 (the default), a tree that has spent a large amount
// still dispatches and never trips the cost backstop, proving the check is fully
// skipped when unset.
func TestEngineZeroCostBudgetIsUnlimited(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	engine := testEngine(store) // MaxDelegationCostUSD defaults to 0 => unlimited

	// A child with a huge Opus token count; with the budget 0 it must be ignored.
	seedCostTree(t, store, "unbounded", "claude-opus-4-8", 1, 100_000_000, []Delegation{
		{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
	})

	if err := engine.AdvanceJob(ctx, "unbounded"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if !jobExists(t, store, "unbounded/delegation/d0") {
		t.Fatal("with budget 0 (unlimited), delegations must still be dispatched")
	}
	if got := countJobEvents(t, store, "unbounded", "delegation_cost_usd_exceeded"); got != 0 {
		t.Fatalf("delegation_cost_usd_exceeded events = %d, want 0 with unlimited budget", got)
	}
}
