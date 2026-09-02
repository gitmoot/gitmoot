package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// blockingWorktreeManager refuses allocation with a BlockedError, which is the
// production trigger for the resolution transaction's ALTERNATIVE OUTCOME.
type blockingWorktreeManager struct {
	fakeWorktreeManager
}

func (b *blockingWorktreeManager) AddWorktree(_ context.Context, _ string, _ string, _ string) error {
	return BlockedError{Reason: "worktree allocation refused for the test"}
}

// TestRefusedAllocationBlocksUnderTheFenceAndNeverRepeats covers P1-1
// (engine.go:464). The refused-allocation branch must be REACHABLE from production:
// the block and its task_event commit under the fence with NO receipt and NO prepared
// jobs, a pass that lost the fence lands nothing at all, and a recovery pass cannot
// repeat the block.
func TestRefusedAllocationBlocksUnderTheFenceAndNeverRepeats(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)
	// Swap in a manager that refuses, so the refusal comes from the production path
	// rather than from a test hook.
	engine.DelegationWorktrees = &blockingWorktreeManager{}

	blockedErr := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "")
	var blocked BlockedError
	if !errors.As(blockedErr, &blocked) {
		t.Fatalf("ResolveEscalation error = %v, want BlockedError from the refused allocation", blockedErr)
	}

	// THE BLOCK LANDED, under the fence, with its event.
	task, err := store.GetTask(ctx, "task-5")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
	blockEvents := 0
	events, err := store.ListTaskEvents(ctx, "task-5")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == "workflow_blocked" {
			blockEvents++
		}
	}
	if blockEvents != 1 {
		t.Fatalf("workflow_blocked task events = %d, want exactly 1", blockEvents)
	}

	// NO RECEIPT and NO DISPATCH: a refused decision keeps its claim and queues nothing.
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0: a refused decision must not be recorded as applied", got)
	}
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0: a refused decision must dispatch nothing", got)
	}
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("the refusal settled the round: the human decision is gone")
	}
	if !round.Claimed() {
		t.Fatal("the refusal discarded the claim")
	}

	// AND IT CANNOT REPEAT. Recovery re-drives the same refusal; the block must not
	// be written a second time.
	if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil && !errors.As(err, &blocked) {
		t.Fatalf("recovery pass: %v", err)
	}
	events, err = store.ListTaskEvents(ctx, "task-5")
	if err != nil {
		t.Fatalf("ListTaskEvents after recovery: %v", err)
	}
	repeat := 0
	for _, event := range events {
		if event.Kind == "workflow_blocked" {
			repeat++
		}
	}
	if repeat != 1 {
		t.Fatalf("workflow_blocked task events after recovery = %d, want still exactly 1", repeat)
	}
}

// TestTerminalTaskWinnerRefusesTheWholeResolution covers P1-2
// (store_escalation_rounds.go:544). If another worker moves the task to a terminal
// state between capture and commit, the guarded task write affects zero rows - and
// that single boolean must abort EVERYTHING: no prepared job, no event, no receipt.
func TestTerminalTaskWinnerRefusesTheWholeResolution(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)

	// The concurrent winner lands in the capture/commit seam, through the same hook
	// the crash tests use, so the race is deterministic.
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		task, err := store.GetTask(hookCtx, "task-5")
		if err != nil {
			t.Fatalf("hook GetTask: %v", err)
		}
		task.State = string(TaskMerged)
		if err := store.UpsertTask(hookCtx, task); err != nil {
			t.Fatalf("hook UpsertTask: %v", err)
		}
		return nil
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}

	// THE WINNER KEPT THE TASK.
	task, err := store.GetTask(ctx, "task-5")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskMerged) {
		t.Fatalf("task state = %q, want merged: the resolution overwrote a terminal winner", task.State)
	}
	// AND NOTHING ELSE LANDED.
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0: stale work was dispatched against a terminal lifecycle", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0: the decision was recorded as applied when it was not", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", "delegation_escalation_retry"); got != 0 {
		t.Fatalf("verb events = %d, want 0", got)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); !ok {
		t.Fatal("the refused commit settled the round anyway")
	}
}

// TestPreEffectOwnershipLossLeavesTheReplacementsLockAlone covers P1-3 in its
// PRODUCTION SHAPE. The earlier version of this test used "other-recoverer" as the
// replacement branch-lock owner and asserted the stale pass HANDED THE LOCK BACK. Both
// halves were wrong.
//
// The shared-checkout lock is owned by request.Agent (engine_delegation.go), an identity
// that is STABLE ACROSS RECOVERY PASSES rather than unique to the lease holder. So a
// replacement pass acquires the very same repo/branch/agent lock legitimately - and a
// stale pass "releasing its own lock" by that tuple DELETES THE LIVE LOCK the new pass
// believes it holds, turning a bounded leak into a silent mutual-exclusion failure.
//
// SEMANTIC REVERSION THIS KILLS: re-introduce the handback and the replacement's lock
// disappears.
func TestPreEffectOwnershipLossLeavesTheReplacementsLockAlone(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)
	// FORCE THE SHARED-CHECKOUT FALLBACK: with no worktree manager the leg takes a real
	// BRANCH LOCK, which is the resource this finding is about.
	engine.DelegationWorktrees = nil
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		now := time.Now().UTC()
		taken, err := store.AcquireEscalationRecoveryLease(hookCtx, "parent-job", round.RoundID, "other-recoverer",
			now.Add(time.Minute), now.Add(2*escalationRecoveryLeaseTTL))
		if err != nil {
			t.Fatalf("hook acquire: %v", err)
		}
		if !taken {
			t.Fatal("the hook could not take ownership: the test cannot observe loss")
		}
		return nil
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}

	// NO EFFECTS from a pass that lost ownership.
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0 after ownership loss", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0 after ownership loss", got)
	}
	// NOTHING RECORDED on the round either: the pre-effect record is owner-scoped.
	stored, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("the round vanished: the claim must survive an ownership loss")
	}
	if strings.TrimSpace(stored.PreEffectBranch) != "" {
		t.Fatalf("a pass that lost ownership recorded pre-effects: %+v", stored)
	}

	// THE REPLACEMENT'S LOCK SURVIVES. The replacement holds it under the REAL
	// production identity - the same agent - which is exactly why a handback keyed on
	// that tuple would delete it.
	lock := db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-005", Owner: "builder"}
	if _, err := store.AcquireLock(ctx, lock); err != nil {
		t.Fatalf("replacement AcquireLock: %v", err)
	}
	held, err := store.ListBranchLocks(ctx, "gitmoot/gitmoot")
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	found := false
	for _, existing := range held {
		if existing.Branch == "task-005" && existing.Owner == "builder" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the replacement pass's branch lock was deleted by the stale pass; locks = %+v", held)
	}
}

// TestReleasingAFenceYouDoNotOwnReportsFalse covers the tenth guarded write,
// ReleaseEscalationRecoveryLease, whose affected-row count was previously discarded
// inside the store. Zero rows is legitimate - but it must be REPORTED, so a caller can
// tell "I released it" from "it was never mine".
func TestReleasingAFenceYouDoNotOwnReportsFalse(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	now := time.Now().UTC()
	held, err := store.AcquireEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "owner-a",
		now.Add(escalationRecoveryLeaseTTL), now)
	if err != nil {
		t.Fatalf("AcquireEscalationRecoveryLease: %v", err)
	}
	if !held {
		t.Fatal("owner-a did not take the fence")
	}

	// A NON-OWNER releases nothing and is told so.
	released, err := store.ReleaseEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "owner-b")
	if err != nil {
		t.Fatalf("release by non-owner: %v", err)
	}
	if released {
		t.Fatal("a non-owner released someone else's fence")
	}
	// AND THE FENCE IS STILL HELD: owner-b cannot take it.
	stolen, err := store.AcquireEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "owner-b",
		now.Add(escalationRecoveryLeaseTTL), now)
	if err != nil {
		t.Fatalf("AcquireEscalationRecoveryLease(b): %v", err)
	}
	if stolen {
		t.Fatal("the fence was taken after a failed release")
	}

	// THE OWNER releases, and is told it happened.
	released, err = store.ReleaseEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "owner-a")
	if err != nil {
		t.Fatalf("release by owner: %v", err)
	}
	if !released {
		t.Fatal("the owner's release reported false")
	}
}

// TestOwnershipLostBeforeRenewalAppliesNothing covers the RENEWAL's own false case,
// which is a different window from RecordEscalationRoundPreEffects: ownership can be
// gone before the pre-effects even start. The renewal exists because git work has no
// bound and a fixed lease can lapse under it.
//
// SEMANTIC REVERSION THIS KILLS: ignore the renewal result and this pass runs its
// pre-effects and commits while a second recoverer owns the round.
func TestOwnershipLostBeforeRenewalAppliesNothing(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	manager := pausedImplementEscalation(t, store, &engine)
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}
	// INVOCATIONS, not distinct paths. The property here is "a pass without ownership
	// must not even ATTEMPT git work"; allocation is idempotent by key, so counting
	// distinct worktrees cannot see a second attempt at all.
	attemptsBefore := len(manager.calls)

	escalationPreRenewHook = func(hookCtx context.Context, jobID string, roundID string) {
		now := time.Now().UTC()
		taken, err := store.AcquireEscalationRecoveryLease(hookCtx, jobID, roundID, "other-recoverer",
			now.Add(time.Minute), now.Add(2*escalationRecoveryLeaseTTL))
		if err != nil {
			t.Fatalf("hook acquire: %v", err)
		}
		if !taken {
			t.Fatal("the hook could not take ownership: the test cannot observe the renewal")
		}
	}
	t.Cleanup(func() { escalationPreRenewHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}

	// NOTHING RAN: not the pre-effects, not the effects, not the receipt.
	if got := len(manager.calls); got != attemptsBefore {
		t.Fatalf("worktree attempts %d -> %d: a pass without ownership ran git work", attemptsBefore, got)
	}
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0", got)
	}
	if _, stillOpen := unsettledRound(t, store, "parent-job"); !stillOpen {
		t.Fatalf("the round was settled by a pass that had lost ownership (round %s)", round.RoundID)
	}
}
