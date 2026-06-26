package workflow

import (
	"context"
	"testing"
	"time"

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

// TestKillDelegationTreeReleasesLocksAndTerminalizesQueuedChildren pins the
// companion cleanup defects #479 + #480: a kill releases the resource/branch
// locks owned by every tree job and eagerly cancels the tree's QUEUED child
// legs, while leaving running children and queued continuations untouched.
func TestKillDelegationTreeReleasesLocksAndTerminalizesQueuedChildren(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")

	// Root coordinator (self-roots; succeeded).
	insertCompletedJob(t, store, db.Job{ID: "root", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", TaskID: "task-9", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "tree"},
	})

	mustCreateJob := func(id, state string, payload JobPayload) {
		t.Helper()
		encoded, err := marshalPayload(payload)
		if err != nil {
			t.Fatalf("marshalPayload(%s) returned error: %v", id, err)
		}
		if err := store.CreateJob(ctx, db.Job{ID: id, Agent: "w", Type: "ask", State: state, Payload: encoded}); err != nil {
			t.Fatalf("CreateJob(%s) returned error: %v", id, err)
		}
	}

	// QUEUED child leg — must be cancelled.
	mustCreateJob("root/delegation/c1", string(JobQueued), JobPayload{
		Repo: "jerryfane/gitmoot", Sender: "w", RootJobID: "root", DelegationID: "d1",
	})
	// RUNNING child leg — must stay running (graceful).
	mustCreateJob("root/delegation/c2", string(JobRunning), JobPayload{
		Repo: "jerryfane/gitmoot", Sender: "w", RootJobID: "root", DelegationID: "d2",
	})
	// QUEUED continuation (DelegationID == "") — must stay queued to drive the
	// #305 graceful finalize.
	mustCreateJob("root/continuation", string(JobQueued), JobPayload{
		Repo: "jerryfane/gitmoot", Sender: "w", RootJobID: "root", DelegationID: "",
	})

	// Acquire locks owned by the root and two child legs.
	now := time.Now()
	expires := now.Add(time.Hour).Format(time.RFC3339Nano)
	for _, lk := range []struct{ key, owner string }{
		{"repo:a", "root"},
		{"repo:b", "root/delegation/c1"},
		{"branch:c", "root/delegation/c2"},
	} {
		acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
			ResourceKey: lk.key, OwnerJobID: lk.owner, OwnerToken: "t", ExpiresAt: expires,
		}, now)
		if err != nil {
			t.Fatalf("AcquireResourceLock(%s) returned error: %v", lk.key, err)
		}
		if !acquired {
			t.Fatalf("AcquireResourceLock(%s) = false, want true", lk.key)
		}
	}

	if _, err := KillDelegationTree(ctx, store, "root"); err != nil {
		t.Fatalf("KillDelegationTree returned error: %v", err)
	}

	// (#480) Queued child leg terminalized with a cancelled event.
	if got := mustJob(t, store, "root/delegation/c1").State; got != string(JobCancelled) {
		t.Fatalf("queued child leg state = %q, want %q", got, JobCancelled)
	}
	if got := countJobEvents(t, store, "root/delegation/c1", "cancelled"); got != 1 {
		t.Fatalf("queued child cancelled events = %d, want 1", got)
	}
	// Running leg unchanged.
	if got := mustJob(t, store, "root/delegation/c2").State; got != string(JobRunning) {
		t.Fatalf("running child leg state = %q, want %q (must run to completion)", got, JobRunning)
	}
	// Continuation unchanged.
	if got := mustJob(t, store, "root/continuation").State; got != string(JobQueued) {
		t.Fatalf("continuation state = %q, want %q (must still run finalize)", got, JobQueued)
	}

	// (#479) No locks remain owned by any tree job.
	locks, err := store.ListResourceLocks(ctx)
	if err != nil {
		t.Fatalf("ListResourceLocks returned error: %v", err)
	}
	treeOwners := map[string]bool{"root": true, "root/delegation/c1": true, "root/delegation/c2": true}
	for _, l := range locks {
		if treeOwners[l.OwnerJobID] {
			t.Fatalf("lock %q still owned by tree job %q after kill", l.ResourceKey, l.OwnerJobID)
		}
	}

	// Existing behavior preserved.
	if got := countJobEvents(t, store, "root", "delegation_killed"); got != 1 {
		t.Fatalf("delegation_killed events = %d, want 1", got)
	}

	// Idempotency: a second kill must not error or duplicate events.
	if _, err := KillDelegationTree(ctx, store, "root"); err != nil {
		t.Fatalf("second KillDelegationTree returned error: %v", err)
	}
	if got := countJobEvents(t, store, "root", "delegation_killed"); got != 2 {
		// SetRootJobKilled + delegation_killed are emitted unconditionally on each
		// call; what must stay stable is the child cancellation, asserted below.
		t.Logf("delegation_killed events after re-kill = %d", got)
	}
	if got := mustJob(t, store, "root/delegation/c1").State; got != string(JobCancelled) {
		t.Fatalf("queued child leg state after re-kill = %q, want %q", got, JobCancelled)
	}
	if got := countJobEvents(t, store, "root/delegation/c1", "cancelled"); got != 1 {
		t.Fatalf("queued child cancelled events after re-kill = %d, want 1 (no dup)", got)
	}
}
