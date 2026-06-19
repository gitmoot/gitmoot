package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

// TestEngineWallClockBudgetTripEnqueuesFinalize pins the #338 wall-clock backstop:
// a coordination tree that has run longer than MaxDelegationWallClock does not
// dispatch its next generation; instead it gets one graceful finalize continuation.
func TestEngineWallClockBudgetTripEnqueuesFinalize(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	engine := testEngine(store)
	// Drive the clock MaxDelegationWallClock + 1h past the root's created_at so the
	// tree is over its wall-clock budget while everything else is in bounds.
	engine.Now = func() time.Time { return time.Now().Add(MaxDelegationWallClock + time.Hour) }

	insertCompletedJob(t, store, db.Job{ID: "slow", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-009", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "slow tree", Delegations: []Delegation{
			{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
			{ID: "d1", Agent: "w", Action: "ask", Prompt: "work"},
		}},
	})
	if err := engine.AdvanceJob(ctx, "slow"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if jobExists(t, store, "slow/delegation/d0") {
		t.Fatal("over-walltime delegations must not be dispatched")
	}
	if got := countJobEvents(t, store, "slow", "delegation_walltime_exceeded"); got != 1 {
		t.Fatalf("delegation_walltime_exceeded events = %d, want 1", got)
	}
	cont, err := unmarshalPayload(mustJob(t, store, "slow/continuation").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("finalize continuation must carry DelegationFinalize: %+v", cont)
	}
	if got := countJobEvents(t, store, "slow", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
}

// TestEngineWithinWallClockBudgetDispatches is the control: a fresh tree (default
// clock, ~0 elapsed) does not trip the wall-clock backstop and dispatches normally.
func TestEngineWithinWallClockBudgetDispatches(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store) // default clock => elapsed ~0

	insertCompletedJob(t, store, db.Job{ID: "fresh", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-009", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "fresh tree", Delegations: []Delegation{
			{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
		}},
	})
	if err := engine.AdvanceJob(ctx, "fresh"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if !jobExists(t, store, "fresh/delegation/d0") {
		t.Fatal("in-budget delegations must be dispatched")
	}
	if got := countJobEvents(t, store, "fresh", "delegation_walltime_exceeded"); got != 0 {
		t.Fatalf("delegation_walltime_exceeded events = %d, want 0", got)
	}
}

func TestParseJobTimestamp(t *testing.T) {
	want := time.Date(2026, 6, 19, 15, 1, 11, 0, time.UTC)
	for _, in := range []string{"2026-06-19 15:01:11", "2026-06-19T15:01:11Z"} {
		got, err := parseJobTimestamp(in)
		if err != nil {
			t.Fatalf("parseJobTimestamp(%q) returned error: %v", in, err)
		}
		if !got.Equal(want) {
			t.Fatalf("parseJobTimestamp(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseJobTimestamp("not-a-timestamp"); err == nil {
		t.Fatal("parseJobTimestamp on garbage should error")
	}
}

// TestRootWallClockExceededFailsOpen pins that a missing root timestamp never
// blocks dispatch (the backstop fails open).
func TestRootWallClockExceededFailsOpen(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.Now = func() time.Time { return time.Now().Add(100 * time.Hour) }
	if exceeded, _ := engine.rootWallClockExceeded(ctx, "no-such-root"); exceeded {
		t.Fatal("unknown root must not be reported as over wall-clock budget")
	}
}
