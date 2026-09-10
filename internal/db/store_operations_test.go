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

func TestRevertAgentTemplateVersion(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	base := AgentTemplate{ID: "planner", Name: "Planner", SourceRepo: "o/r", SourceRef: "main", SourcePath: "p.md", ResolvedCommit: "abc", Content: "v1 content"}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	v1, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("latest v1: %v", err)
	}
	// Upsert a changed v2: UpsertAgentTemplate mints it `current` and supersedes v1,
	// which is how a second version now comes to exist — the pending/promote candidate
	// path went with the SkillOpt loop (#1752).
	v2Template := base
	v2Template.Content = "v2 content"
	v2Template.ResolvedCommit = "def"
	if err := store.UpsertAgentTemplate(ctx, v2Template); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	v2, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("latest v2: %v", err)
	}
	if v2.VersionID == v1.VersionID {
		t.Fatalf("v2 did not mint a new version: %q", v2.VersionID)
	}

	// Revert to v1.
	reverted, err := store.RevertAgentTemplateVersion(ctx, "planner", v1.VersionID)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.State != "current" {
		t.Fatalf("reverted state = %q", reverted.State)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if current.Content != "v1 content" {
		t.Fatalf("template content after revert = %q, want v1 content", current.Content)
	}
	// v2 is now superseded.
	v2After, err := store.GetAgentTemplateVersionByID(ctx, v2.VersionID)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if v2After.State != "superseded" {
		t.Fatalf("v2 state after revert = %q", v2After.State)
	}
	// Reverting a non-superseded version is refused.
	if _, err := store.RevertAgentTemplateVersion(ctx, "planner", v2.VersionID); err != nil {
		t.Fatalf("revert back to v2 (superseded now) should work: %v", err)
	}
	if _, err := store.RevertAgentTemplateVersion(ctx, "planner", v2.VersionID); err == nil {
		t.Fatal("reverting the CURRENT version should be refused")
	}
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
		TemplateID:     "worker-tpl",
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
		got.TemplateID != "worker-tpl" || got.AutonomyPolicy != "auto" ||
		strings.Join(got.Capabilities, ",") != "implement,review" {
		t.Fatalf("re-pin altered preserved fields: %+v", got)
	}
	// No agent row matched is an error, not a silent no-op.
	if err := store.UpdateAgentRuntimeRef(ctx, "ghost", "sess-x"); err == nil {
		t.Fatal("re-pinning an unregistered agent must error")
	}
}

// AgentActiveJobCount is the busy pre-flight behind `agent restart`'s refusal to
// rebind an agent with in-flight work (cli/agent.go) and the heartbeat
// scheduler's max_background skip (cli/daemon_supervision.go). Its state list is
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

// TestUpdateAgentAutonomyPolicyMovesEveryPlaneThatCanBecomeEffective covers
// #2134 F2 across two review rounds.
//
// Round one wrote only `agents`, so instance-only agents - which GetAgent
// resolves at store_agents.go:108 and dispatch therefore uses - had no
// in-place writer at all.
//
// Round two wrote the instance only when no row matched, mirroring GetAgent's
// precedence, on the reasoning that touching a plane dispatch is not reading
// is worse than leaving it stale. Review disproved that with an executed
// probe, reproduced here as the third arm: RemoveAgent deletes `agents` and
// not `agent_instances`, so a stale instance OUTLIVES the row that outranked
// it and then becomes authoritative, silently re-widening a tightened agent.
func TestUpdateAgentAutonomyPolicyMovesEveryPlaneThatCanBecomeEffective(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()

	// INSTANCE-ONLY: dispatchable through GetAgent's fallback, so it must be
	// writable in place.
	if err := store.UpsertAgentInstance(ctx, AgentInstance{
		Name: "ephemeral", Type: "worker", Runtime: "codex", RuntimeRef: "ref-1",
		RepoFullName: "gitmoot/gitmoot", Role: "worker", AutonomyPolicy: "read-only", State: "idle",
	}); err != nil {
		t.Fatalf("UpsertAgentInstance: %v", err)
	}
	if err := store.UpdateAgentAutonomyPolicy(ctx, "ephemeral", "danger-full-access", nil); err != nil {
		t.Fatalf("refused a dispatchable instance-only agent: %v", err)
	}
	got, err := store.GetAgent(ctx, "ephemeral")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.AutonomyPolicy != "danger-full-access" {
		t.Errorf("instance-only policy = %q, want danger-full-access", got.AutonomyPolicy)
	}

	// BOTH PLANES PRESENT: both must move. Start them WIDE and tighten, so a
	// missed plane leaves a permission granted rather than merely stale.
	for _, seed := range []func() error{
		func() error {
			return store.UpsertAgent(ctx, Agent{
				Name: "both", Role: "worker", Runtime: "codex", RuntimeRef: "ref-2",
				AutonomyPolicy: "danger-full-access", HealthStatus: "idle",
			})
		},
		func() error {
			return store.UpsertAgentInstance(ctx, AgentInstance{
				Name: "both", Type: "worker", Runtime: "codex", RuntimeRef: "ref-2",
				RepoFullName: "gitmoot/gitmoot", Role: "worker",
				AutonomyPolicy: "danger-full-access", State: "idle",
			})
		},
	} {
		if err := seed(); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := store.UpdateAgentAutonomyPolicy(ctx, "both", "read-only", nil); err != nil {
		t.Fatalf("UpdateAgentAutonomyPolicy(both): %v", err)
	}
	instance, err := store.GetAgentInstance(ctx, "both")
	if err != nil {
		t.Fatalf("GetAgentInstance: %v", err)
	}
	if instance.AutonomyPolicy != "read-only" {
		t.Errorf("instance policy = %q, want read-only: a plane left wide can become effective later", instance.AutonomyPolicy)
	}

	// THE PROBE THAT DISPROVED ROUND TWO, through the real production path:
	// remove the winning row and see what dispatch would now resolve.
	removed, err := store.RemoveAgent(ctx, "both")
	if err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}
	if !removed {
		t.Fatal("RemoveAgent reported nothing removed")
	}
	survivor, err := store.GetAgent(ctx, "both")
	if err != nil {
		t.Fatalf("GetAgent after remove: %v", err)
	}
	if survivor.AutonomyPolicy != "read-only" {
		t.Errorf("after removing the agents row GetAgent resolves %q, want read-only: "+
			"the surviving instance silently re-widened an agent that was tightened",
			survivor.AutonomyPolicy)
	}

	if err := store.UpdateAgentAutonomyPolicy(ctx, "nosuch", "auto", nil); err == nil {
		t.Error("accepted an agent present in neither plane")
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
// The commit is forced to fail by closing the pool from inside the barrier,
// which is the only way found to reach this branch through the real function
// rather than a synthesized error.
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
