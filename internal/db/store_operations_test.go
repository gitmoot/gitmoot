package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openStoreOperationsTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// UpdateAgentRuntimeRef is the self-heal path's re-pin (#443): it updates only
// runtime_ref and leaves every other column alone. Its coverage used to ride
// along inside TestUpdateAgentRuntime, whose subject went with the dashboard
// TUI (#1753), so it is pinned directly here.
func TestUpdateAgentRuntimeRef(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	original := Agent{
		Name:           "worker",
		Role:           "implement",
		Runtime:        "codex",
		RuntimeRef:     "sess-abc",
		RepoScope:      "owner/repo",
		Capabilities:   []string{"implement", "review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}
	if err := store.UpsertAgent(ctx, original); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	if err := store.UpdateAgentRuntimeRef(ctx, "worker", "sess-def"); err != nil {
		t.Fatalf("re-pin runtime_ref: %v", err)
	}
	got, err := store.GetAgent(ctx, "worker")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.RuntimeRef != "sess-def" {
		t.Fatalf("runtime_ref = %q, want sess-def", got.RuntimeRef)
	}
	// In place: the runtime itself and every other field survive the re-pin.
	if got.Runtime != "codex" || got.Role != "implement" || got.RepoScope != "owner/repo" ||
		got.AutonomyPolicy != "auto" ||
		strings.Join(got.Capabilities, ",") != "implement,review" {
		t.Fatalf("re-pin altered preserved fields: %+v", got)
	}
	// No agent row matched is an error, not a silent no-op.
	if err := store.UpdateAgentRuntimeRef(ctx, "ghost", "sess-x"); err == nil {
		t.Fatal("re-pinning an unregistered agent must error")
	}
}

// AgentActiveJobCount is the busy pre-flight behind `agent restart`'s refusal to
// rebind an agent with in-flight work (cli/agent.go). Its state list is
// ('queued','running') and BOTH halves are load-bearing: a queued job is work
// that has not started, so rebinding past it orphans it.
//
// The queued half used to be pinned by TestDeleteAgentChecked, which died with
// its method in #1753 - and both surviving CLI-level tests of the guard seed
// only "running", so a mutant narrowing the state list to ('running') survived
// the whole suite (#1787 review round 3, F1). This pins it at the store, where
// the state list actually lives.
func TestAgentActiveJobCountCountsQueuedAndRunning(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{Name: "worker", Runtime: "codex"}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	for _, tc := range []struct {
		state string
		want  int
	}{
		{"queued", 1},
		{"running", 1},
		{"succeeded", 0},
		{"failed", 0},
		{"cancelled", 0},
	} {
		jobID := "job-" + tc.state
		if err := store.CreateJob(ctx, Job{ID: jobID, Agent: "worker", Type: "ask", State: tc.state}); err != nil {
			t.Fatalf("create %s job: %v", tc.state, err)
		}
		got, err := store.AgentActiveJobCount(ctx, "worker")
		if err != nil {
			t.Fatalf("AgentActiveJobCount after %s: %v", tc.state, err)
		}
		if got != tc.want {
			t.Fatalf("AgentActiveJobCount with one %s job = %d, want %d: the busy count must include queued AND running work and nothing else", tc.state, got, tc.want)
		}
		if _, err := store.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, jobID); err != nil {
			t.Fatalf("clear %s job: %v", tc.state, err)
		}
	}
	// Scoped to the named agent, not a global count.
	if err := store.CreateJob(ctx, Job{ID: "other", Agent: "someone-else", Type: "ask", State: "queued"}); err != nil {
		t.Fatalf("create foreign job: %v", err)
	}
	if got, err := store.AgentActiveJobCount(ctx, "worker"); err != nil || got != 0 {
		t.Fatalf("AgentActiveJobCount = %d err=%v, want 0: another agent's queued job is not this agent's busy work", got, err)
	}
}

// TestUpdateAgentAutonomyPolicyWritesTheRegisteredAgent covers the store half
// of `agent policy` (#2132): the registered row moves in place, and a name with
// no agents row is refused. The agent_instances plane #2134 F2 also wrote is
// gone with the table (#2590).
func TestUpdateAgentAutonomyPolicyWritesTheRegisteredAgent(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{
		Name: "worker", Role: "worker", Runtime: "codex", RuntimeRef: "ref-2",
		AutonomyPolicy: "danger-full-access", HealthStatus: "idle",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := store.UpdateAgentAutonomyPolicy(ctx, "worker", "read-only", nil); err != nil {
		t.Fatalf("UpdateAgentAutonomyPolicy(worker): %v", err)
	}
	got, err := store.GetAgent(ctx, "worker")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.AutonomyPolicy != "read-only" {
		t.Errorf("policy = %q, want read-only", got.AutonomyPolicy)
	}
	if err := store.UpdateAgentAutonomyPolicy(ctx, "nosuch", "auto", nil); err == nil {
		t.Error("accepted an unregistered agent")
	}
}

// TestUpdateAgentAutonomyPolicyBarrierRollsBackEveryPlane covers #2134 F1's
// cross-plane protocol at the store boundary: a barrier failure must leave the
// rows exactly as they were, because the caller's other plane did not move
// either.
func TestUpdateAgentAutonomyPolicyBarrierRollsBackEveryPlane(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{
		Name: "guarded", Role: "worker", Runtime: "codex", RuntimeRef: "ref-3",
		AutonomyPolicy: "read-only", HealthStatus: "idle",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	barrier := errors.New("config plane unwritable")
	err := store.UpdateAgentAutonomyPolicy(ctx, "guarded", "danger-full-access", func() error {
		return barrier
	})
	if !errors.Is(err, ErrAgentPolicyBarrier) {
		t.Errorf("error = %v, want it to wrap ErrAgentPolicyBarrier so the caller can tell rollback from a split write", err)
	}
	if !errors.Is(err, barrier) {
		t.Errorf("error = %v, want the barrier's own cause preserved", err)
	}
	after, err := store.GetAgent(ctx, "guarded")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if after.AutonomyPolicy != "read-only" {
		t.Errorf("policy = %q after a failed barrier, want read-only: the transaction must roll back", after.AutonomyPolicy)
	}
}

// TestUpdateAgentAutonomyPolicyReportsACommitFailureDistinctly pins the one
// thing the caller's compensation depends on: a commit that fails AFTER a
// successful barrier must be distinguishable from a barrier failure. They
// demand opposite responses - a barrier failure means nothing moved, a commit
// failure means the caller's plane moved and must be rolled back - so a
// collapsed sentinel silently turns a partial write into a silent one.
//
// SCOPE, because review measured this precisely and the distinction matters:
// cancelling the transaction's context reaches UpdateAgentAutonomyPolicy's real
// tx.Commit error arm after a successful barrier, which is the branch the
// caller's compensation depends on. It does NOT exercise a driver-level SQLite
// COMMIT failure: Go's database/sql checks the transaction context first and
// returns context.Canceled before calling the driver's Commit. A driver-level
// commit fault would need a fault-injecting driver seam, which does not exist
// here. Closing the pool does not work at all, because the transaction holds
// its own connection.
func TestUpdateAgentAutonomyPolicyReportsACommitFailureDistinctly(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{
		Name: "doomed", Role: "worker", Runtime: "codex", RuntimeRef: "ref-9",
		AutonomyPolicy: "read-only", HealthStatus: "idle",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	// Cancelling the transaction's own context is what makes the commit fail:
	// closing the pool does not, because the transaction holds its connection.
	txCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	barrierRan := false
	err := store.UpdateAgentAutonomyPolicy(txCtx, "doomed", "danger-full-access", func() error {
		barrierRan = true
		// The barrier SUCCEEDS - this is the caller's other plane being
		// written - and only then is the commit made impossible.
		cancel()
		return nil
	})
	if !barrierRan {
		t.Fatal("barrier never ran, so this test is not exercising the commit path")
	}
	if err == nil {
		t.Fatal("commit failure reported success")
	}
	if !errors.Is(err, ErrAgentPolicyCommit) {
		t.Errorf("error = %v, want ErrAgentPolicyCommit", err)
	}
	if errors.Is(err, ErrAgentPolicyBarrier) {
		t.Errorf("error = %v, must NOT read as a barrier failure: the caller would then skip compensation "+
			"and leave its own plane silently widened", err)
	}
}
