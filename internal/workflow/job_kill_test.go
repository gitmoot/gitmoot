package workflow

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// TestEngineKilledRootEnqueuesFinalizeAndDispatchesZero pins the #341 operator
// kill switch: once a tree's root is marked killed, the coordinator's next
// dispatchDelegations does not dispatch any new delegations; instead it gets one
// graceful finalize continuation and emits delegation_killed.
func TestEngineKilledRootEnqueuesFinalizeAndDispatchesZero(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "root", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-009", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "killed tree", Delegations: []Delegation{
			{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
			{ID: "d1", Agent: "w", Action: "ask", Prompt: "work"},
		}},
	})

	// Operator kills the tree before the coordinator's next dispatch.
	if err := store.SetRootJobKilled(ctx, "root"); err != nil {
		t.Fatalf("SetRootJobKilled returned error: %v", err)
	}

	if err := engine.AdvanceJob(ctx, "root"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if jobExists(t, store, "root/delegation/d0") || jobExists(t, store, "root/delegation/d1") {
		t.Fatal("a killed root must not dispatch any new delegations")
	}
	if got := countJobEvents(t, store, "root", "delegation_killed"); got != 1 {
		t.Fatalf("delegation_killed events = %d, want 1", got)
	}
	cont, err := unmarshalPayload(mustJob(t, store, "root/continuation").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("kill finalize continuation must carry DelegationFinalize: %+v", cont)
	}
	if got := countJobEvents(t, store, "root", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
}

// TestEngineUnkilledRootDispatches is the control: a tree whose root was never
// killed dispatches its delegations normally and emits no delegation_killed.
func TestEngineUnkilledRootDispatches(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "live", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-009", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "live tree", Delegations: []Delegation{
			{ID: "d0", Agent: "w", Action: "ask", Prompt: "work"},
		}},
	})
	if err := engine.AdvanceJob(ctx, "live"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if !jobExists(t, store, "live/delegation/d0") {
		t.Fatal("an un-killed root must dispatch its delegations")
	}
	if got := countJobEvents(t, store, "live", "delegation_killed"); got != 0 {
		t.Fatalf("delegation_killed events = %d, want 0", got)
	}
}

// TestKillDelegationTreeMarksRootAndErrorsOnMissing covers the workflow entry
// point used by the CLI: it marks an existing root, emits the event, and errors
// on an unknown root id.
func TestKillDelegationTreeMarksRootAndErrorsOnMissing(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "root", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "tree"},
	})

	root, err := KillDelegationTree(ctx, store, "root")
	if err != nil {
		t.Fatalf("KillDelegationTree returned error: %v", err)
	}
	if !root.RootKilled {
		t.Fatalf("returned root job should report RootKilled=true: %+v", root)
	}
	killed, err := store.IsRootJobKilled(ctx, "root")
	if err != nil {
		t.Fatalf("IsRootJobKilled returned error: %v", err)
	}
	if !killed {
		t.Fatal("root should be marked killed after KillDelegationTree")
	}
	if got := countJobEvents(t, store, "root", "delegation_killed"); got != 1 {
		t.Fatalf("delegation_killed events = %d, want 1", got)
	}

	if _, err := KillDelegationTree(ctx, store, "no-such-root"); err == nil {
		t.Fatal("KillDelegationTree on a missing root must error")
	}
}

// TestKillDelegationTreeResolvesChildToRoot pins that passing ANY tree member's
// id (a child/continuation, not just the root) kills the whole tree — the engine
// and daemon only consult the root's flag, so a child id must resolve to it.
func TestKillDelegationTreeResolvesChildToRoot(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "root", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "tree"},
	})
	insertCompletedJob(t, store, db.Job{ID: "root/delegation/c1", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Sender: "coord", RootJobID: "root", DelegationID: "d1",
		Result: &AgentResult{Decision: "approved", Summary: "child"},
	})

	// Operator passes a CHILD id; the whole tree (its root) must be killed.
	root, err := KillDelegationTree(ctx, store, "root/delegation/c1")
	if err != nil {
		t.Fatalf("KillDelegationTree(child) returned error: %v", err)
	}
	if root.ID != "root" {
		t.Fatalf("kill should resolve a child id to the root; got %q", root.ID)
	}
	if killed, _ := store.IsRootJobKilled(ctx, "root"); !killed {
		t.Fatal("the ROOT must be marked killed when an operator passes a child id")
	}
	if killed, _ := store.IsRootJobKilled(ctx, "root/delegation/c1"); killed {
		t.Fatal("the child row itself should not carry the kill flag")
	}
	if got := countJobEvents(t, store, "root", "delegation_killed"); got != 1 {
		t.Fatalf("delegation_killed events on root = %d, want 1", got)
	}
}
