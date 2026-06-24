package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// TestEngineBackstopTripEnqueuesFinalizeContinuation pins the #305 "Later"
// graceful-finalize behavior: when a termination backstop trips (here, width), the
// offending delegations are NOT dispatched, but instead of stopping silently the
// coordinator gets one terminal finalize continuation.
func TestEngineBackstopTripEnqueuesFinalizeContinuation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	dels := make([]Delegation, 0, MaxDelegationWidth+1)
	for i := 0; i <= MaxDelegationWidth; i++ {
		dels = append(dels, Delegation{ID: fmt.Sprintf("d%d", i), Agent: "w", Action: "ask", Prompt: "work"})
	}
	insertCompletedJob(t, store, db.Job{ID: "wide", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-005", TaskID: "task-5", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "too wide", Delegations: dels},
	})
	if err := engine.AdvanceJob(ctx, "wide"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if jobExists(t, store, "wide/delegation/d0") {
		t.Fatal("over-width delegations must not be dispatched")
	}
	cont, err := unmarshalPayload(mustJob(t, store, "wide/continuation").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("finalize continuation must carry DelegationFinalize: %+v", cont)
	}
	if got := countJobEvents(t, store, "wide", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
	// The finalize occupies the continuation slot so a later advanceDelegations on
	// the tripped coordinator cannot enqueue a normal continuation that collides
	// with the finalize job's deterministic id.
	if got := countJobEvents(t, store, "wide", "delegation_continuation_enqueued"); got != 1 {
		t.Fatalf("delegation_continuation_enqueued events = %d, want 1", got)
	}

	// Idempotent on re-advance: a second AdvanceJob must not enqueue a duplicate
	// finalize continuation or re-emit the finalize event.
	if err := engine.AdvanceJob(ctx, "wide"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}
	if got := countJobEvents(t, store, "wide", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("after re-advance: delegation_finalize_enqueued events = %d, want 1", got)
	}
}

// TestEngineFinalizeContinuationIsTerminal pins that a finalize continuation is
// terminal: even if the coordinator returned delegations, they are ignored.
func TestEngineFinalizeContinuationIsTerminal(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "fin", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-005", TaskID: "task-5", Sender: "coord",
		DelegationFinalize: true,
		Result: &AgentResult{Decision: "approved", Summary: "final synthesis", Delegations: []Delegation{
			{ID: "again", Agent: "w", Action: "ask", Prompt: "still more work"},
		}},
	})
	if err := engine.AdvanceJob(ctx, "fin"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if jobExists(t, store, "fin/delegation/again") {
		t.Fatal("a finalize continuation's delegations must be ignored (terminal)")
	}
	if got := countJobEvents(t, store, "fin", "delegation_finalized"); got != 1 {
		t.Fatalf("delegation_finalized events = %d, want 1", got)
	}
}

func TestBuildFinalizeContinuationPrompt(t *testing.T) {
	prompt := buildFinalizeContinuationPrompt(
		"",
		&AgentResult{Delegations: []Delegation{{ID: "x", Agent: "w", Action: "ask"}}},
		"per-root job budget of 64 reached",
	)
	for _, want := range []string{"termination backstop", "per-root job budget of 64 reached", "EMPTY delegations", "ignored", `delegation "x"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("finalize prompt missing %q:\n%s", want, prompt)
		}
	}
}
