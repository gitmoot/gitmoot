package workflow

import (
	"context"
	"fmt"
	"github.com/gitmoot/gitmoot/internal/db"
	"testing"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

const burst617Repo = "jerryfane/noted"

// fanOutEphemeralImplementBurst inserts a coordinator whose result declares n
// ephemeral, workspace-write implement delegations (mirroring the issue's six-way
// codex burst) and drives its AdvanceJob so the engine dispatches every leg —
// allocating one gitmoot-delegation-* branch lock per leg. It returns the leg job
// ids in dispatch order. Each leg is left queued, exactly as the daemon would see it
// before running the worker.
func fanOutEphemeralImplementBurst(t *testing.T, engine Engine, store *db.Store, coordinatorID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	seedAgent(t, store, "coord-617", []string{"ask"}, burst617Repo)

	dels := make([]Delegation, 0, n)
	for i := 0; i < n; i++ {
		dels = append(dels, Delegation{
			ID:        fmt.Sprintf("impl-%d-command", i),
			Action:    "implement",
			Prompt:    "implement a distinct small subcommand and open a PR",
			Ephemeral: &EphemeralSpec{Runtime: "codex", AutonomyPolicy: "workspace-write"},
		})
	}
	insertCompletedJob(t, store, db.Job{ID: coordinatorID, Agent: "coord-617", Type: "ask"}, JobPayload{
		Repo:   burst617Repo,
		Branch: "main",
		Sender: "coord-617",
		Result: &AgentResult{Decision: "approved", Summary: "fan out the burst", Delegations: dels},
	})

	if err := engine.AdvanceJob(ctx, coordinatorID); err != nil {
		t.Fatalf("AdvanceJob(%s) fan-out returned error: %v", coordinatorID, err)
	}

	legIDs := make([]string, 0, n)
	for _, d := range dels {
		legID := coordinatorID + "/delegation/" + d.ID
		if !jobExists(t, store, legID) {
			t.Fatalf("ephemeral implement leg %s was not dispatched", legID)
		}
		legIDs = append(legIDs, legID)
	}
	return legIDs
}

func countBranchLocks(t *testing.T, store *db.Store, repo string) int {
	t.Helper()
	locks, err := store.ListBranchLocks(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListBranchLocks(%s) returned error: %v", repo, err)
	}
	return len(locks)
}

func newBurst617Engine(t *testing.T) (Engine, *db.Store) {
	t.Helper()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.Home = t.TempDir()
	engine.DelegationCheckout = t.TempDir()
	// A fake worktree manager: AllocateDelegationWorktree still writes the REAL branch
	// lock to the store (that is the entity #617 leaks); the on-disk worktree/branch
	// are stubbed. existingBranches stays empty so allocation takes the create path.
	engine.DelegationWorktrees = &fakeWorktreeManager{existingBranches: map[string]bool{}}
	return engine, store
}
