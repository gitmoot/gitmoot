package workflow

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// seedTreeWithUsage builds a small coordination tree rooted at rootID: the root
// coordinator plus n already-completed children (each pointing back at the root
// via RootJobID) carrying perChildTokens output tokens. It returns nothing; the
// caller asserts via engine.AdvanceJob(rootID).
func seedTreeWithUsage(t *testing.T, store *db.Store, rootID string, n int, perChildTokens int, rootDelegations []Delegation) {
	t.Helper()
	ctx := context.Background()
	insertCompletedJob(t, store, db.Job{ID: rootID, Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-009", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "tree", Delegations: rootDelegations},
	})
	for i := 0; i < n; i++ {
		childID := rootID + "/child" + string(rune('a'+i))
		insertCompletedJob(t, store, db.Job{ID: childID, Agent: "w", Type: "ask"}, JobPayload{
			Repo: "jerryfane/gitmoot", Branch: "task-009", TaskID: "task-9", Sender: "w",
			RootJobID: rootID,
			Result:    &AgentResult{Decision: "approved", Summary: "child"},
		})
		// Seed the child's runtime token usage (input 0, output perChildTokens) so
		// the per-root sum is deterministic and independent of any live runtime.
		if err := store.UpdateJobUsage(ctx, childID, 0, perChildTokens); err != nil {
			t.Fatalf("UpdateJobUsage(%s) returned error: %v", childID, err)
		}
	}
}

// TestEngineTokenBudgetTripEnqueuesFinalize pins the #338 Part B per-root token
// budget: a coordination tree whose children have already used at least the
// budget does not dispatch its next generation; instead it gets one graceful
// finalize continuation and emits delegation_cost_exceeded.
func TestEngineTokenBudgetTripEnqueuesFinalize(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	engine := testEngine(store)
	engine.MaxDelegationTokenBudget = 1000

	// Two children of 600 tokens each => 1200 used >= 1000 budget.
	seedTreeWithUsage(t, store, "over", 2, 600, []Delegation{
		{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
		{ID: "d1", Agent: "w", Action: "ask", Prompt: "work"},
	})

	if err := engine.AdvanceJob(ctx, "over"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if jobExists(t, store, "over/delegation/d0") || jobExists(t, store, "over/delegation/d1") {
		t.Fatal("over-budget delegations must not be dispatched")
	}
	if got := countJobEvents(t, store, "over", "delegation_cost_exceeded"); got != 1 {
		t.Fatalf("delegation_cost_exceeded events = %d, want 1", got)
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

// TestEngineWithinTokenBudgetDispatches is the control: a tree whose usage is
// strictly under the budget dispatches its next generation normally and emits no
// cost-exceeded event.
func TestEngineWithinTokenBudgetDispatches(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	engine := testEngine(store)
	engine.MaxDelegationTokenBudget = 1000

	// One child of 300 tokens => 300 used < 1000 budget.
	seedTreeWithUsage(t, store, "under", 1, 300, []Delegation{
		{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
	})

	if err := engine.AdvanceJob(ctx, "under"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if !jobExists(t, store, "under/delegation/d0") {
		t.Fatal("in-budget delegations must be dispatched")
	}
	if got := countJobEvents(t, store, "under", "delegation_cost_exceeded"); got != 0 {
		t.Fatalf("delegation_cost_exceeded events = %d, want 0", got)
	}
}

// TestEngineZeroTokenBudgetIsUnlimited is the byte-identical-default control:
// with the budget left at 0 (the default), a tree that has used a large number of
// tokens still dispatches and never trips the cost backstop, proving the check is
// fully skipped when unset.
func TestEngineZeroTokenBudgetIsUnlimited(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	engine := testEngine(store) // MaxDelegationTokenBudget defaults to 0 => unlimited

	// A child with a huge token count; with budget 0 it must be ignored entirely.
	seedTreeWithUsage(t, store, "unbounded", 1, 1_000_000, []Delegation{
		{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
	})

	if err := engine.AdvanceJob(ctx, "unbounded"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if !jobExists(t, store, "unbounded/delegation/d0") {
		t.Fatal("with budget 0 (unlimited), delegations must still be dispatched")
	}
	if got := countJobEvents(t, store, "unbounded", "delegation_cost_exceeded"); got != 0 {
		t.Fatalf("delegation_cost_exceeded events = %d, want 0 with unlimited budget", got)
	}
}

// TestSumRootDelegationTokens pins the per-root aggregation: it sums input +
// output across the root coordinator and every job pointing back at it, and
// ignores jobs belonging to other roots.
func TestSumRootDelegationTokens(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)

	// Root R: coordinator (10 in / 20 out) + two children (100 out, 250 out).
	insertCompletedJob(t, store, db.Job{ID: "R", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "root"},
	})
	if err := store.UpdateJobUsage(ctx, "R", 10, 20); err != nil {
		t.Fatalf("UpdateJobUsage(R) returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "R/c1", Agent: "w", Type: "ask"}, JobPayload{
		RootJobID: "R", Result: &AgentResult{Decision: "approved", Summary: "c1"},
	})
	if err := store.UpdateJobUsage(ctx, "R/c1", 0, 100); err != nil {
		t.Fatalf("UpdateJobUsage(R/c1) returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "R/c2", Agent: "w", Type: "ask"}, JobPayload{
		RootJobID: "R", Result: &AgentResult{Decision: "approved", Summary: "c2"},
	})
	if err := store.UpdateJobUsage(ctx, "R/c2", 0, 250); err != nil {
		t.Fatalf("UpdateJobUsage(R/c2) returned error: %v", err)
	}
	// An unrelated root S with usage that must NOT be counted toward R.
	insertCompletedJob(t, store, db.Job{ID: "S", Agent: "coord", Type: "ask"}, JobPayload{
		Result: &AgentResult{Decision: "approved", Summary: "other root"},
	})
	if err := store.UpdateJobUsage(ctx, "S", 999, 999); err != nil {
		t.Fatalf("UpdateJobUsage(S) returned error: %v", err)
	}

	got, err := engine.sumRootDelegationTokens(ctx, "R")
	if err != nil {
		t.Fatalf("sumRootDelegationTokens returned error: %v", err)
	}
	if want := 10 + 20 + 100 + 250; got != want {
		t.Fatalf("sumRootDelegationTokens(R) = %d, want %d", got, want)
	}
}
