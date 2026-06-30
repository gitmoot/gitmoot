package workflow

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

// roOnlyWorktreeManager satisfies ReadOnlyWorktreeManager (force-remove) but
// implements neither BranchDeleter nor BranchExistenceChecker — a degraded
// manager used to exercise the interface-degradation paths in
// cleanupImplementDelegationWorktree.
type roOnlyWorktreeManager struct{ removed []string }

func (m *roOnlyWorktreeManager) AddWorktree(context.Context, string, string, string) error {
	return nil
}
func (m *roOnlyWorktreeManager) AddDetachedWorktree(context.Context, string, string) error { return nil }
func (m *roOnlyWorktreeManager) RemoveWorktreeForce(_ context.Context, path string) error {
	m.removed = append(m.removed, path)
	return nil
}

// deleterNoCheckerWorktreeManager satisfies ReadOnlyWorktreeManager + BranchDeleter
// but NOT BranchExistenceChecker.
type deleterNoCheckerWorktreeManager struct {
	removed []string
	deleted []string
}

func (m *deleterNoCheckerWorktreeManager) AddWorktree(context.Context, string, string, string) error {
	return nil
}
func (m *deleterNoCheckerWorktreeManager) AddDetachedWorktree(context.Context, string, string) error {
	return nil
}
func (m *deleterNoCheckerWorktreeManager) RemoveWorktreeForce(_ context.Context, path string) error {
	m.removed = append(m.removed, path)
	return nil
}
func (m *deleterNoCheckerWorktreeManager) DeleteBranch(_ context.Context, branch string) error {
	m.deleted = append(m.deleted, branch)
	return nil
}

func jobEventMessages(t *testing.T, store *db.Store, jobID, kind string) []string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s) returned error: %v", jobID, err)
	}
	var msgs []string
	for _, e := range events {
		if e.Kind == kind {
			msgs = append(msgs, e.Message)
		}
	}
	return msgs
}

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

// TestCleanupImplementDelegationWorktreePreservesWhileConsumerLive pins the #332
// guard's failure direction: cleanup is DESTRUCTIVE, so a succeeded leg whose
// branch a sibling integration step may still merge must be PRESERVED on any
// uncertainty — a parent fetch error, and a consumer that has not yet reached a
// terminal state.
func TestCleanupImplementDelegationWorktreePreservesWhileConsumerLive(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	branch := "gitmoot-delegation-x-produce"
	manager := &fakeWorktreeManager{existingBranches: map[string]bool{branch: true}}
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	legPayload := JobPayload{
		ParentJobID:  "parent-job",
		DelegationID: "produce",
		WorktreePath: t.TempDir(),
		Branch:       branch,
		Result:       &AgentResult{Decision: "implemented"},
	}

	// (a) Parent not readable yet (no parent job in the store) -> "cannot determine"
	// must preserve, not delete.
	engine.cleanupImplementDelegationWorktree(ctx, "parent-job/delegation/produce", "implement", legPayload)
	if len(manager.removedForce) != 0 || len(manager.deletedBranches) != 0 {
		t.Fatalf("unreadable parent must preserve the leg (fail safe): removedForce=%+v deletedBranches=%+v", manager.removedForce, manager.deletedBranches)
	}

	// Seed the parent and a consumer integration step that is still QUEUED.
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "owner/repo",
		Result: &AgentResult{
			Decision: "approved",
			Delegations: []Delegation{
				{ID: "produce", Action: "implement", Prompt: "produce"},
				{ID: "integrate", Action: "review", Prompt: "verify", Deps: []string{"produce"}},
			},
		},
	})
	consumerEncoded, err := marshalPayload(JobPayload{ParentJobID: "parent-job", DelegationID: "integrate", Deps: []string{"produce"}})
	if err != nil {
		t.Fatalf("marshalPayload(integrate) returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "parent-job/delegation/integrate", Agent: "verifier", Type: "review",
		State: string(JobQueued), ParentJobID: "parent-job", DelegationID: "integrate",
		Payload: consumerEncoded,
	}, db.JobEvent{Kind: string(JobQueued), Message: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent(integrate) returned error: %v", err)
	}

	// (b) Consumer still live (queued) -> preserve.
	engine.cleanupImplementDelegationWorktree(ctx, "parent-job/delegation/produce", "implement", legPayload)
	if len(manager.removedForce) != 0 || len(manager.deletedBranches) != 0 {
		t.Fatalf("live integration consumer must preserve the leg: removedForce=%+v deletedBranches=%+v", manager.removedForce, manager.deletedBranches)
	}
}

// TestCleanupConsumedImplementLegWorktreesAfterIntegrationTerminal covers the
// #478 integration shape: once the integration step that consumed a leg reaches a
// terminal state, the leg's branch+worktree are reclaimed (they are preserved
// while the consumer is live, then nothing else would ever delete them).
func TestCleanupConsumedImplementLegWorktreesAfterIntegrationTerminal(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	legBranch := "gitmoot-delegation-x-produce"
	manager := &fakeWorktreeManager{existingBranches: map[string]bool{legBranch: true}}
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "owner/repo",
		Result: &AgentResult{
			Decision: "approved",
			Delegations: []Delegation{
				{ID: "produce", Action: "implement", Prompt: "produce"},
				{ID: "integrate", Action: "review", Prompt: "verify", Deps: []string{"produce"}},
			},
		},
	})

	legWt := t.TempDir()
	insertCompletedJob(t, store, db.Job{
		ID: "parent-job/delegation/produce", Agent: "producer", Type: "implement",
		ParentJobID: "parent-job", DelegationID: "produce",
	}, JobPayload{
		ParentJobID: "parent-job", DelegationID: "produce", WorktreePath: legWt, Branch: legBranch,
		Result: &AgentResult{Decision: "implemented"},
	})
	// The integration step consumed the leg and is now terminal (succeeded).
	insertCompletedJob(t, store, db.Job{
		ID: "parent-job/delegation/integrate", Agent: "verifier", Type: "review",
		ParentJobID: "parent-job", DelegationID: "integrate",
	}, JobPayload{
		ParentJobID: "parent-job", DelegationID: "integrate", Deps: []string{"produce"},
		Result: &AgentResult{Decision: "approved"},
	})

	consumerPayload := JobPayload{ParentJobID: "parent-job", DelegationID: "integrate", Deps: []string{"produce"}}
	engine.cleanupConsumedImplementLegWorktrees(ctx, consumerPayload)

	if len(manager.removedForce) != 1 || manager.removedForce[0] != legWt {
		t.Fatalf("consumed leg worktree must be removed once the integration is terminal: removedForce=%+v", manager.removedForce)
	}
	if len(manager.deletedBranches) != 1 || manager.deletedBranches[0] != legBranch {
		t.Fatalf("consumed leg branch must be deleted once the integration is terminal: deletedBranches=%+v", manager.deletedBranches)
	}
}

// TestAdvanceJobTerminalImplementLegFiresCleanup drives a delegated implement
// child to a terminal result through engine.AdvanceJob() and asserts the defer
// wiring actually tears the worktree+branch down — covering the placement of the
// defer (a future early-return added above it, or a dropped/misplaced defer,
// would be caught). It checks both a succeeded (implemented) leg and a terminal
// failed leg.
func TestAdvanceJobTerminalImplementLegFiresCleanup(t *testing.T) {
	ctx := context.Background()

	t.Run("implemented", func(t *testing.T) {
		store := openEngineStore(t)
		engine := testEngine(store)
		engine.Home = t.TempDir()
		engine.DelegationCheckout = t.TempDir()
		branch := "gitmoot-delegation-x-d1"
		manager := &fakeWorktreeManager{existingBranches: map[string]bool{branch: true}}
		engine.DelegationWorktrees = manager

		wt := t.TempDir()
		insertCompletedJob(t, store, db.Job{ID: "leg-impl", Agent: "producer", Type: "implement", DelegationID: "d1"}, JobPayload{
			DelegationID: "d1", WorktreePath: wt, Branch: branch,
			Result: &AgentResult{Decision: "implemented", Summary: "done"},
		})
		if err := engine.AdvanceJob(ctx, "leg-impl"); err != nil {
			t.Fatalf("AdvanceJob(leg-impl) returned error: %v", err)
		}
		if len(manager.removedForce) != 1 || manager.removedForce[0] != wt {
			t.Fatalf("terminal implement leg AdvanceJob must force-remove the worktree: removedForce=%+v", manager.removedForce)
		}
		if len(manager.deletedBranches) != 1 || manager.deletedBranches[0] != branch {
			t.Fatalf("terminal implement leg AdvanceJob must delete the branch: deletedBranches=%+v", manager.deletedBranches)
		}
	})

	t.Run("failed", func(t *testing.T) {
		store := openEngineStore(t)
		engine := testEngine(store)
		engine.Home = t.TempDir()
		engine.DelegationCheckout = t.TempDir()
		branch := "gitmoot-delegation-x-d2"
		manager := &fakeWorktreeManager{existingBranches: map[string]bool{branch: true}}
		engine.DelegationWorktrees = manager

		wt := t.TempDir()
		insertCompletedJob(t, store, db.Job{ID: "leg-fail", Agent: "producer", Type: "implement", DelegationID: "d2"}, JobPayload{
			DelegationID: "d2", WorktreePath: wt, Branch: branch,
			Result: &AgentResult{Decision: "failed", Summary: "boom"},
		})
		// A failed leg blocks its (empty) task ref, so AdvanceJob returns a
		// BlockedError; the cleanup defer must still fire on that return path.
		err := engine.AdvanceJob(ctx, "leg-fail")
		var blocked BlockedError
		if err != nil && !errors.As(err, &blocked) {
			t.Fatalf("AdvanceJob(leg-fail) returned unexpected error: %v", err)
		}
		if len(manager.removedForce) != 1 || manager.removedForce[0] != wt {
			t.Fatalf("terminal failed leg AdvanceJob must force-remove the worktree: removedForce=%+v", manager.removedForce)
		}
		if len(manager.deletedBranches) != 1 || manager.deletedBranches[0] != branch {
			t.Fatalf("terminal failed leg AdvanceJob must delete the branch: deletedBranches=%+v", manager.deletedBranches)
		}
	})
}

// TestAdvanceJobTerminalImplementLegCleansWhileSelfHoldsLock is the HIGH #478
// regression that the original #536 fix introduced: cleanupImplementDelegationWorktree
// is deferred inside AdvanceJob, which runs inside RunJob WHILE the daemon still
// holds the job's OWN runtime-session lock (the daemon releases it only after RunJob
// returns). The earlier liveness gate keyed on ANY held lock, so on a perfectly
// healthy completion the run's own live lock made the gate refuse cleanup, leaking
// the worktree + gitmoot-delegation-* branch for EVERY normally-completing child.
//
// The fix excludes the run's OWN lock (matched by the owner token threaded through
// the run context). This test reproduces the healthy path: the child's runtime lock
// is held by THIS process (live PID, unexpired lease) AND the run context carries
// that lock's owner token, then AdvanceJob drives the child to terminal. The
// worktree+branch MUST be removed.
//
// The companion sub-case (no self token in ctx, same live lock) pins that a FOREIGN
// live owner still blocks the destructive cleanup — i.e. the test fails if the gate
// is simply removed, not just if the self-exclusion is added. Before the fix the
// first sub-case fails (cleanup refused despite the lock being the run's own).
func TestAdvanceJobTerminalImplementLegCleansWhileSelfHoldsLock(t *testing.T) {
	const ownerToken = "self-owner-token-536"
	acquireSelfLock := func(t *testing.T, store *db.Store, jobID string) {
		t.Helper()
		now := time.Now().UTC()
		acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
			ResourceKey: "runtime:codex:session-self",
			OwnerJobID:  jobID,
			OwnerToken:  ownerToken,
			OwnerPID:    int64(os.Getpid()),
			ExpiresAt:   now.Add(4 * time.Hour).Format(time.RFC3339Nano),
		}, now)
		if err != nil || !acquired {
			t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
		}
	}

	newEngine := func(t *testing.T, store *db.Store, branch string) (Engine, *fakeWorktreeManager) {
		t.Helper()
		engine := testEngine(store)
		engine.Home = t.TempDir()
		engine.DelegationCheckout = t.TempDir()
		engine.OwnerPIDLive = func(int64) bool { return true } // own daemon PID is live
		manager := &fakeWorktreeManager{existingBranches: map[string]bool{branch: true}}
		engine.DelegationWorktrees = manager
		return engine, manager
	}

	t.Run("self-held lock does not block healthy cleanup", func(t *testing.T) {
		store := openEngineStore(t)
		branch := "gitmoot-delegation-x-self"
		engine, manager := newEngine(t, store, branch)
		wt := t.TempDir()
		jobID := "leg-self"
		insertCompletedJob(t, store, db.Job{ID: jobID, Agent: "producer", Type: "implement", DelegationID: "d1"}, JobPayload{
			DelegationID: "d1", WorktreePath: wt, Branch: branch,
			Result: &AgentResult{Decision: "implemented", Summary: "done"},
		})
		acquireSelfLock(t, store, jobID)

		// The daemon threads the run's lock owner token into the run context; AdvanceJob
		// (and its deferred cleanup) inherits it.
		ctx := WithRuntimeSelfOwnerToken(context.Background(), ownerToken)
		if err := engine.AdvanceJob(ctx, jobID); err != nil {
			t.Fatalf("AdvanceJob(%s) returned error: %v", jobID, err)
		}
		if len(manager.removedForce) != 1 || manager.removedForce[0] != wt {
			t.Fatalf("healthy completion must force-remove the worktree even with the run's own lock held: removedForce=%+v", manager.removedForce)
		}
		if len(manager.deletedBranches) != 1 || manager.deletedBranches[0] != branch {
			t.Fatalf("healthy completion must delete the branch: deletedBranches=%+v", manager.deletedBranches)
		}
		if got := countJobEvents(t, store, jobID, "delegation_worktree_cleanup_skipped"); got != 0 {
			t.Fatalf("healthy completion must not skip cleanup, got %d skip events", got)
		}
	})

	t.Run("foreign live lock still blocks cleanup", func(t *testing.T) {
		store := openEngineStore(t)
		branch := "gitmoot-delegation-x-foreign"
		engine, manager := newEngine(t, store, branch)
		wt := t.TempDir()
		jobID := "leg-foreign"
		insertCompletedJob(t, store, db.Job{ID: jobID, Agent: "producer", Type: "implement", DelegationID: "d2"}, JobPayload{
			DelegationID: "d2", WorktreePath: wt, Branch: branch,
			Result: &AgentResult{Decision: "implemented", Summary: "done"},
		})
		acquireSelfLock(t, store, jobID)

		// No self token in ctx: the held lock is a FOREIGN live owner (e.g. a recovery /
		// retry path advancing the job while the original worker still holds the lock).
		if err := engine.AdvanceJob(context.Background(), jobID); err != nil {
			t.Fatalf("AdvanceJob(%s) returned error: %v", jobID, err)
		}
		if len(manager.removedForce) != 0 || len(manager.deletedBranches) != 0 {
			t.Fatalf("foreign live owner must block destructive cleanup: removedForce=%+v deletedBranches=%+v", manager.removedForce, manager.deletedBranches)
		}
		if got := countJobEvents(t, store, jobID, "delegation_worktree_cleanup_skipped"); got != 1 {
			t.Fatalf("foreign live owner must skip cleanup once, got %d skip events", got)
		}
		if _, err := os.Stat(wt); err != nil {
			t.Fatalf("worktree must be preserved on disk: stat err = %v", err)
		}
	})
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

// TestCleanupImplementDelegationWorktreeRefusesWhileRuntimeOwnerActive is the
// #536 regression: when a delegation child's terminal state was synthesized by
// stale recovery / dirty-checkout validation WHILE the original runtime worker was
// still running, the worker's runtime-session lock lingers with an unexpired lease
// (and possibly a live owner PID). The DESTRUCTIVE cleanup must REFUSE to
// force-remove the worktree (and delete its branch) in that window so the live
// worker is not orphaned onto a deleted cwd and its dirty work is preserved for
// salvage. Once the lock has expired/released (the healthy case), cleanup runs.
//
// Without the liveness gate the first row ("live owner") would force-remove the
// worktree out from under the running worker.
func TestCleanupImplementDelegationWorktreeRefusesWhileRuntimeOwnerActive(t *testing.T) {
	cases := []struct {
		name        string
		acquireLock bool
		pidLive     bool
		expiresIn   time.Duration
		wantRemoved bool
	}{
		{name: "live owner unexpired lease preserves worktree", acquireLock: true, pidLive: true, expiresIn: 4 * time.Hour, wantRemoved: false},
		{name: "unexpired lease dead pid preserves worktree", acquireLock: true, pidLive: false, expiresIn: 4 * time.Hour, wantRemoved: false},
		{name: "expired lease dead pid cleans worktree", acquireLock: true, pidLive: false, expiresIn: -time.Minute, wantRemoved: true},
		{name: "no runtime lock cleans worktree", acquireLock: false, wantRemoved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			engine := testEngine(store)
			engine.OwnerPIDLive = func(int64) bool { return tc.pidLive }
			branch := "gitmoot-delegation-x-d1"
			manager := &fakeWorktreeManager{existingBranches: map[string]bool{branch: true}}
			engine.DelegationCheckout = t.TempDir()
			engine.DelegationWorktrees = manager

			wt := t.TempDir()
			// Decision "failed" mirrors the bug: the recovered copy fails on the dirty
			// checkout. Failed legs are always merge-safe, so the merge guard does not
			// short-circuit and the liveness guard is exercised.
			payload := JobPayload{DelegationID: "d1", WorktreePath: wt, Branch: branch, Result: &AgentResult{Decision: "failed"}}

			jobID := "parent-job/delegation/task-7302"
			if tc.acquireLock {
				now := time.Now().UTC()
				acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
					ResourceKey: "runtime:codex:session-536",
					OwnerJobID:  jobID,
					OwnerToken:  "token-536",
					OwnerPID:    4242,
					ExpiresAt:   now.Add(tc.expiresIn).Format(time.RFC3339Nano),
				}, now)
				if err != nil || !acquired {
					t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
				}
			}

			engine.cleanupImplementDelegationWorktree(ctx, jobID, "implement", payload)

			if tc.wantRemoved {
				if len(manager.removedForce) != 1 || manager.removedForce[0] != wt {
					t.Fatalf("removedForce = %+v, want one force-remove of %q", manager.removedForce, wt)
				}
				if got := countJobEvents(t, store, jobID, "delegation_worktree_cleanup_skipped"); got != 0 {
					t.Fatalf("cleanup must not be skipped, got %d skip events", got)
				}
			} else {
				if len(manager.removedForce) != 0 || len(manager.deletedBranches) != 0 {
					t.Fatalf("cleanup must be refused while owner active: removedForce=%+v deletedBranches=%+v", manager.removedForce, manager.deletedBranches)
				}
				if got := countJobEvents(t, store, jobID, "delegation_worktree_cleanup_skipped"); got != 1 {
					t.Fatalf("delegation_worktree_cleanup_skipped event count = %d, want 1", got)
				}
				if _, err := os.Stat(wt); err != nil {
					t.Fatalf("worktree must be preserved on disk: stat err = %v", err)
				}
			}
		})
	}
}

// TestCleanupImplementDelegationWorktreeNoBranchDeleter pins that when the manager
// cannot delete branches (no BranchDeleter), the worktree is removed, the branch
// is intentionally kept, and the emitted event does NOT falsely claim the branch
// was removed. Re-advances are silent no-ops (no repeated removed event).
func TestCleanupImplementDelegationWorktreeNoBranchDeleter(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &roOnlyWorktreeManager{}
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	wt := t.TempDir()
	payload := JobPayload{DelegationID: "d1", WorktreePath: wt, Branch: "gitmoot-delegation-x-d1", Result: &AgentResult{Decision: "implemented"}}
	engine.cleanupImplementDelegationWorktree(ctx, "job-1", "implement", payload)

	if len(manager.removed) != 1 || manager.removed[0] != wt {
		t.Fatalf("worktree must still be removed without a deleter: removed=%+v", manager.removed)
	}
	msgs := jobEventMessages(t, store, "job-1", "delegation_worktree_removed")
	if len(msgs) != 1 {
		t.Fatalf("delegation_worktree_removed event count = %d, want 1", len(msgs))
	}
	if strings.Contains(msgs[0], "branch") {
		t.Fatalf("event must not claim a branch was removed when no deleter exists: %q", msgs[0])
	}

	// Re-advance with the worktree already gone is a silent no-op (no deleter, so
	// nothing is pending) — no second removed event.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("RemoveAll returned error: %v", err)
	}
	engine.cleanupImplementDelegationWorktree(ctx, "job-1", "implement", payload)
	if msgs := jobEventMessages(t, store, "job-1", "delegation_worktree_removed"); len(msgs) != 1 {
		t.Fatalf("re-advance without a deleter must be a no-op: removed events = %d", len(msgs))
	}
}

// TestCleanupImplementDelegationWorktreeNoChecker pins that without a
// BranchExistenceChecker, an already-removed worktree is a sufficient idempotent
// short-circuit: a re-advance must NOT re-run DeleteBranch on the already-deleted
// branch (which would emit a spurious cleanup_failed event).
func TestCleanupImplementDelegationWorktreeNoChecker(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &deleterNoCheckerWorktreeManager{}
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	branch := "gitmoot-delegation-x-d1"
	wt := t.TempDir()
	payload := JobPayload{DelegationID: "d1", WorktreePath: wt, Branch: branch, Result: &AgentResult{Decision: "implemented"}}
	engine.cleanupImplementDelegationWorktree(ctx, "job-1", "implement", payload)
	if len(manager.removed) != 1 || len(manager.deleted) != 1 {
		t.Fatalf("first cleanup must remove worktree and delete branch: removed=%+v deleted=%+v", manager.removed, manager.deleted)
	}

	// Re-advance with the worktree gone must short-circuit (no second DeleteBranch,
	// no spurious cleanup_failed event) even though no checker can confirm the
	// branch is gone.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("RemoveAll returned error: %v", err)
	}
	engine.cleanupImplementDelegationWorktree(ctx, "job-1", "implement", payload)
	if len(manager.deleted) != 1 {
		t.Fatalf("re-advance without a checker must not re-delete the branch: deleted=%+v", manager.deleted)
	}
	if got := countJobEvents(t, store, "job-1", "delegation_worktree_cleanup_failed"); got != 0 {
		t.Fatalf("re-advance without a checker must not emit cleanup_failed, got %d", got)
	}
}
