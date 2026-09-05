//go:build e2e

package workflow

import (
	"context"
	"testing"
)

// This file is the end-to-end proof for #617: an ephemeral implement delegation's
// per-delegation checkout/branch lock (gitmoot-delegation-<parent-short>-<id>-...)
// must be released when the leg reaches ANY terminal state — success, failed, AND
// cancelled — symmetric with the CreateLock AllocateDelegationWorktree takes at
// dispatch. Before the fix the whole fan-out could SUCCEED and still strand every
// leg's branch lock (no worker process remained to release it), so the next
// same-repo orchestration mis-read the stale locks as live workers and was refused
// until a manual `lock release --force`.
//
// It drives the REAL engine fan-out (AdvanceJob on a coordinator dispatches the
// ephemeral implement legs, acquiring one branch lock each) and the REAL terminal
// transitions (AdvanceJob on a succeeded/failed leg, CancelJob on a queued leg),
// then asserts on the store's branch_locks table — deterministic, no LLM, no daemon.
//
// FAILS ON UNMODIFIED MAIN: the success sub-test asserts ZERO branch locks remain
// after every leg succeeds; on main the six locks stay held, so it goes RED (this is
// the bug reproduction).
//
// MUTATION: delete the releaseDelegationBranchLock call in
// cleanupImplementDelegationWorktree (internal/workflow/worktree.go) and the success
// sub-test goes RED again — the locks are stranded after success exactly as #617
// reported.

// TestDelegationBranchLockReleasedOnSuccess_617_E2E is the primary reproduction and
// the mutation target: a six-way ephemeral implement burst succeeds in full, and
// every gitmoot-delegation-* branch lock must be gone afterward. On unmodified main
// (and if the release is mutated out) the locks remain and this goes RED.
func TestDelegationBranchLockReleasedOnSuccess_617_E2E(t *testing.T) {
	ctx := context.Background()
	engine, store := newBurst617Engine(t)

	legIDs := fanOutEphemeralImplementBurst(t, engine, store, "burst-success", 6)
	if got := countBranchLocks(t, store, burst617Repo); got != 6 {
		t.Fatalf("after fan-out: %d branch locks held, want 6 (the burst must acquire one per leg)", got)
	}

	// Every leg succeeds (decision "implemented"), exactly as the issue's fan-out did.
	for _, legID := range legIDs {
		completeDelegationChild(t, store, legID, JobSucceeded, AgentResult{Decision: "implemented", Summary: "opened a PR"})
		if err := engine.AdvanceJob(ctx, legID); err != nil {
			t.Fatalf("AdvanceJob(%s) on a succeeded leg returned error: %v", legID, err)
		}
	}

	if got := countBranchLocks(t, store, burst617Repo); got != 0 {
		t.Fatalf("#617: %d delegation branch lock(s) still held after the whole burst SUCCEEDED, want 0 — they are stranded with no worker to release them and block the next same-repo burst", got)
	}

	// The unblock proof: a SECOND same-repo burst dispatches cleanly (fresh locks),
	// where before the fix the stale locks would have been mis-read as live workers.
	secondLegs := fanOutEphemeralImplementBurst(t, engine, store, "burst-success-2", 6)
	if got := countBranchLocks(t, store, burst617Repo); got != 6 {
		t.Fatalf("second same-repo burst: %d branch locks held, want 6 (the re-burst must be accepted, not refused)", got)
	}
	for _, legID := range secondLegs {
		completeDelegationChild(t, store, legID, JobSucceeded, AgentResult{Decision: "implemented", Summary: "opened a PR"})
		if err := engine.AdvanceJob(ctx, legID); err != nil {
			t.Fatalf("AdvanceJob(%s) on second-burst leg returned error: %v", legID, err)
		}
	}
	if got := countBranchLocks(t, store, burst617Repo); got != 0 {
		t.Fatalf("after the second burst succeeded: %d branch locks still held, want 0", got)
	}
}

// TestDelegationBranchLockReleasedOnFailure_617_E2E: a leg that reaches the FAILED
// terminal state must also release its branch lock. AdvanceJob on a failed leg
// blocks its (task-less) ref and returns a BlockedError, but the terminal cleanup —
// and the branch-lock release — fires from the deferred cleanup on every return
// path, so the lock is still reclaimed.
func TestDelegationBranchLockReleasedOnFailure_617_E2E(t *testing.T) {
	ctx := context.Background()
	engine, store := newBurst617Engine(t)

	legIDs := fanOutEphemeralImplementBurst(t, engine, store, "burst-failed", 6)
	if got := countBranchLocks(t, store, burst617Repo); got != 6 {
		t.Fatalf("after fan-out: %d branch locks held, want 6", got)
	}

	for _, legID := range legIDs {
		completeDelegationChild(t, store, legID, JobFailed, AgentResult{Decision: "failed", Summary: "boom"})
		// A failed leg blocks its ref; AdvanceJob may return a BlockedError. The
		// deferred cleanup (and its branch-lock release) already fired regardless, so
		// tolerate the error and assert on the released lock below.
		_ = engine.AdvanceJob(ctx, legID)
	}

	if got := countBranchLocks(t, store, burst617Repo); got != 0 {
		t.Fatalf("#617: %d delegation branch lock(s) still held after the burst FAILED, want 0", got)
	}
}

// TestDelegationBranchLockReleasedOnCancel_617_E2E: a queued leg cancelled before it
// runs must release its branch lock too — a cancel bypasses the engine's terminal
// cleanup entirely (that fires from AdvanceJob), so CancelJob itself must release the
// lock (#617), or a cancelled burst strands its locks exactly like the success path
// once did.
func TestDelegationBranchLockReleasedOnCancel_617_E2E(t *testing.T) {
	ctx := context.Background()
	engine, store := newBurst617Engine(t)

	legIDs := fanOutEphemeralImplementBurst(t, engine, store, "burst-cancel", 6)
	if got := countBranchLocks(t, store, burst617Repo); got != 6 {
		t.Fatalf("after fan-out: %d branch locks held, want 6", got)
	}

	for _, legID := range legIDs {
		if _, err := CancelJob(ctx, store, legID); err != nil {
			t.Fatalf("CancelJob(%s) returned error: %v", legID, err)
		}
	}

	if got := countBranchLocks(t, store, burst617Repo); got != 0 {
		t.Fatalf("#617: %d delegation branch lock(s) still held after the burst was CANCELLED, want 0", got)
	}
}

// TestDelegationBranchLockReleasedOnKill_617_E2E: `gitmoot job kill` cancels the
// queued legs of a tree before they start; those legs already acquired their branch
// locks at dispatch, so KillDelegationTree must release them (#617).
func TestDelegationBranchLockReleasedOnKill_617_E2E(t *testing.T) {
	ctx := context.Background()
	engine, store := newBurst617Engine(t)

	// Kill guards on the leg carrying RootJobID != its own id; the fan-out sets
	// RootJobID to the coordinator, so kill the coordinator's tree.
	legIDs := fanOutEphemeralImplementBurst(t, engine, store, "burst-kill", 6)
	if got := countBranchLocks(t, store, burst617Repo); got != 6 {
		t.Fatalf("after fan-out: %d branch locks held, want 6", got)
	}
	_ = legIDs

	if _, err := KillDelegationTree(ctx, store, "burst-kill"); err != nil {
		t.Fatalf("KillDelegationTree returned error: %v", err)
	}

	if got := countBranchLocks(t, store, burst617Repo); got != 0 {
		t.Fatalf("#617: %d delegation branch lock(s) still held after the tree was KILLED, want 0", got)
	}
}
