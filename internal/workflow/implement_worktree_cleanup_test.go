package workflow

import (
	"context"
	"os"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestIsImplementDelegationWorktree(t *testing.T) {
	base := JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1", Branch: "gitmoot-delegation-x-d1"}
	if !isImplementDelegationWorktree("implement", base) {
		t.Fatal("implement delegation worktree child not detected")
	}
	if isImplementDelegationWorktree("ask", base) {
		t.Fatal("ask child must not be treated as an implement worktree")
	}
	if isImplementDelegationWorktree("review", base) {
		t.Fatal("review child must not be treated as an implement worktree")
	}
	if isImplementDelegationWorktree("implement", JobPayload{WorktreePath: "/wt/d1", Branch: "b"}) {
		t.Fatal("non-delegation job (no delegation id) must not match")
	}
	if isImplementDelegationWorktree("implement", JobPayload{DelegationID: "d1", Branch: "b"}) {
		t.Fatal("implement child without a worktree path must not match")
	}
	if isImplementDelegationWorktree("implement", JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1"}) {
		t.Fatal("implement child without a branch must not match")
	}
}

func TestCleanupImplementDelegationWorktreeRemovesWorktreeAndBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	branch := "gitmoot-delegation-x-d1"
	manager := &fakeWorktreeManager{existingBranches: map[string]bool{branch: true}}
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	// The worktree path must exist on disk so the idempotency stat check proceeds.
	wt := t.TempDir()
	payload := JobPayload{
		DelegationID: "d1",
		WorktreePath: wt,
		Branch:       branch,
		Result:       &AgentResult{Decision: "implemented"},
	}
	engine.cleanupImplementDelegationWorktree(ctx, "job-1", "implement", payload)

	if len(manager.removedForce) != 1 || manager.removedForce[0] != wt {
		t.Fatalf("removedForce = %+v, want one force-remove of %q", manager.removedForce, wt)
	}
	if len(manager.deletedBranches) != 1 || manager.deletedBranches[0] != branch {
		t.Fatalf("deletedBranches = %+v, want one delete of %q", manager.deletedBranches, branch)
	}
	if got := countJobEvents(t, store, "job-1", "delegation_worktree_removed"); got != 1 {
		t.Fatalf("delegation_worktree_removed event count = %d, want 1", got)
	}
	if got := countJobEvents(t, store, "job-1", "delegation_worktree_cleanup_failed"); got != 0 {
		t.Fatalf("cleanup must not emit cleanup_failed events, got %d", got)
	}

	// Idempotent: a second cleanup once both worktree and branch are gone is a
	// silent no-op (no re-lock, no extra removal/delete, no spurious event).
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("RemoveAll returned error: %v", err)
	}
	manager.existingBranches[branch] = false
	engine.cleanupImplementDelegationWorktree(ctx, "job-1", "implement", payload)
	if len(manager.removedForce) != 1 || len(manager.deletedBranches) != 1 {
		t.Fatalf("idempotent cleanup must be a no-op: removedForce=%+v deletedBranches=%+v", manager.removedForce, manager.deletedBranches)
	}

	// No-op for a read-only child (cleaned by cleanupReadOnlyDelegationWorktree).
	manager.removedForce = nil
	manager.deletedBranches = nil
	engine.cleanupImplementDelegationWorktree(ctx, "job-2", "ask", JobPayload{DelegationID: "d2", WorktreePath: t.TempDir(), Branch: "b2"})
	if len(manager.removedForce) != 0 || len(manager.deletedBranches) != 0 {
		t.Fatalf("read-only child must be a no-op: removedForce=%+v deletedBranches=%+v", manager.removedForce, manager.deletedBranches)
	}
}

func TestCleanupImplementDelegationWorktreeSkipsIntegrationDep(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	branch := "gitmoot-delegation-x-d1"
	manager := &fakeWorktreeManager{existingBranches: map[string]bool{branch: true}}
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	// Seed a parent whose result has a sibling (the integration step) that depends
	// on the succeeded leg d1: its branch must NOT be torn down (#332).
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "owner/repo",
		Result: &AgentResult{
			Decision: "approved",
			Delegations: []Delegation{
				{ID: "d1", Action: "implement", Prompt: "produce"},
				{ID: "integrate", Action: "review", Prompt: "verify", Deps: []string{"d1"}},
			},
		},
	})

	wt := t.TempDir()
	payload := JobPayload{
		ParentJobID:  "parent-job",
		DelegationID: "d1",
		WorktreePath: wt,
		Branch:       branch,
		Result:       &AgentResult{Decision: "implemented"},
	}
	engine.cleanupImplementDelegationWorktree(ctx, "parent-job/delegation/d1", "implement", payload)

	if len(manager.removedForce) != 0 || len(manager.deletedBranches) != 0 {
		t.Fatalf("succeeded leg feeding a pending integration must be kept: removedForce=%+v deletedBranches=%+v", manager.removedForce, manager.deletedBranches)
	}
}
