package workflow

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
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

// TestHeartbeatKeepsOwnershipAcrossASlowPreEffect is the test the round-3 verdict said
// did not exist: nothing shrank escalationRecoveryLeaseTTL, so the ticker (TTL/3 = 40s)
// never fired in any checked-in test, and deleting the heartbeat loop left them all
// green.
//
// Here the TTL is shrunk and the worktree allocation is made SLOWER than the whole
// lease, so the pass survives only if renewal actually happens. A competing recoverer
// tries to take the fence at a moment when the original lease would already have lapsed.
//
// SEMANTIC REVERSION THIS KILLS: delete the renewal inside the heartbeat loop (or make
// its tick a no-op) and the competitor takes the fence, so this pass applies nothing.
func TestHeartbeatKeepsOwnershipAcrossASlowPreEffect(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	manager := pausedImplementEscalation(t, store, &engine)

	originalTTL := escalationRecoveryLeaseTTL
	escalationRecoveryLeaseTTL = 300 * time.Millisecond
	t.Cleanup(func() { escalationRecoveryLeaseTTL = originalTTL })

	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	// The pre-effect outlives the ORIGINAL lease by a wide margin, so only renewal can
	// keep this pass's ownership alive.
	var competitorTook atomic.Bool
	manager.onAdd = func() {
		time.Sleep(900 * time.Millisecond)
		now := time.Now().UTC()
		taken, err := store.AcquireEscalationRecoveryLease(context.Background(), "parent-job", round.RoundID,
			"competitor", now.Add(time.Minute), now)
		if err == nil && taken {
			competitorTook.Store(true)
		}
	}

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}
	if competitorTook.Load() {
		t.Fatal("a competitor took the fence during a slow pre-effect: the lease was not renewed")
	}
	// AND THE RUN THAT SHOULD SUCCEED DID: the retry landed exactly once.
	if got := countJobs(t, store, "/resume"); got != 1 {
		t.Fatalf("resume jobs = %d, want exactly 1: a renewed pass must still apply its effects", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("receipts = %d, want exactly 1", got)
	}
}

// TestHeartbeatCancelsThePassOnAuthoritativeLoss is the other half: when ownership is
// genuinely taken, the heartbeat must CANCEL the in-flight pre-effects so the losing
// pass stops mid-flight rather than finishing work it no longer owns.
//
// SEMANTIC REVERSION THIS KILLS: drop the cancellation (or treat loss as a transient
// error and keep going) and the losing pass runs to completion.
func TestHeartbeatCancelsThePassOnAuthoritativeLoss(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	manager := pausedImplementEscalation(t, store, &engine)

	originalTTL := escalationRecoveryLeaseTTL
	escalationRecoveryLeaseTTL = 300 * time.Millisecond
	t.Cleanup(func() { escalationRecoveryLeaseTTL = originalTTL })

	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	// Ownership is taken from under the pass WHILE its pre-effect runs, using a clock
	// far enough ahead that the steal is authoritative rather than a race.
	var cancelObserved atomic.Bool
	manager.onAddCtx = func(effectCtx context.Context) {
		now := time.Now().UTC()
		if _, err := store.AcquireEscalationRecoveryLease(context.Background(), "parent-job", round.RoundID,
			"thief", now.Add(time.Minute), now.Add(10*escalationRecoveryLeaseTTL)); err != nil {
			t.Errorf("steal the fence: %v", err)
			return
		}
		// THE ASSERTION THAT MAKES THIS A TEST OF CANCELLATION rather than of the commit
		// guard: the in-flight pre-effect must have its OWN context cancelled, so it stops
		// mid-flight instead of finishing work this pass no longer owns. Without this the
		// test passes even with the cancellation removed, because the fenced commit
		// refuses the losing pass anyway - a mutant proved exactly that.
		select {
		case <-effectCtx.Done():
			cancelObserved.Store(true)
		case <-time.After(2 * time.Second):
		}
	}

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}
	if !cancelObserved.Load() {
		t.Fatal("the in-flight pre-effect was never cancelled: a pass that lost ownership kept working")
	}
	// A PASS THAT LOST OWNERSHIP APPLIES NOTHING.
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0 after an authoritative ownership loss", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0 after an authoritative ownership loss", got)
	}
	if _, stillOpen := unsettledRound(t, store, "parent-job"); !stillOpen {
		t.Fatal("a pass that lost ownership settled the round")
	}
}

// TestBranchLockCollisionDoesNotSettleTheRetry is the P1 of the fea59486 review, and it
// is the case a type check cannot separate: a shared-checkout branch lock held by
// another agent refuses the retry with a BlockedError - the SAME Go type a recorded
// synthesis decision uses.
//
// Classified as a decision it commits a receipt and SETTLES the round, so a retry that
// dispatched nothing looks applied and the human's decision is lost for good. Classified
// structurally - the block came out of the allocation choke point, so the decision could
// not be ATTEMPTED - the receipt is withheld and the claim survives to be re-driven once
// the lock frees.
//
// SEMANTIC REVERSION THIS KILLS: stop marking the choke point's refusal (or key the
// guard on the error type again) and this round settles with no child dispatched.
func TestBranchLockCollisionDoesNotSettleTheRetry(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)
	// Force the shared-checkout fallback, which is the arm that takes a branch lock.
	engine.DelegationWorktrees = nil

	// ANOTHER AGENT ALREADY HOLDS THE BRANCH. This is the production trigger.
	taken, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-005", Owner: "someone-else",
	})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if !taken {
		t.Fatal("the competing agent could not take the branch lock: the test cannot observe the collision")
	}

	resolveErr := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "")
	var blocked BlockedError
	if !errors.As(resolveErr, &blocked) {
		t.Fatalf("ResolveEscalation error = %v, want a BlockedError from the lock collision", resolveErr)
	}

	// NOTHING WAS DISPATCHED, so nothing may be recorded as applied.
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0: the lock collision prevented any dispatch", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0: a refused allocation must not look applied", got)
	}
	// AND THE HUMAN'S DECISION SURVIVES for a later pass.
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("the round settled: the human retry decision is unrecoverable")
	}
	if !round.Claimed() {
		t.Fatal("the claim was discarded by a refused allocation")
	}
}

// TestRenewalErrorsCancelAtTheConfirmedExpiry is the P1 of the 2754115c review, and it
// is what round 2's heartbeat made possible: moving from a one-shot renewal to a retry
// loop turned the RETRY policy into an AUTHORITY policy, and a loop that retries past
// the lease it last confirmed extends authority the store never granted.
//
// THE TIMELINE, driven through the production heartbeat rather than a helper: the TTL is
// shrunk, every renewal write fails, and a pre-effect blocks. At the last confirmed
// expiry the fence becomes reclaimable by anyone, so the pass must cancel its own
// in-flight work at or before that instant - even though no renewal ever returned an
// authoritative "not held".
//
// SEMANTIC REVERSION THIS KILLS: turn the expiry bound back into an unconditional
// `continue` and the original pass keeps allocating while a competitor takes the fence.
func TestRenewalErrorsCancelAtTheConfirmedExpiry(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	manager := pausedImplementEscalation(t, store, &engine)

	originalTTL := escalationRecoveryLeaseTTL
	escalationRecoveryLeaseTTL = 300 * time.Millisecond
	t.Cleanup(func() { escalationRecoveryLeaseTTL = originalTTL })

	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	// EVERY renewal write fails, for the whole run.
	var renewAttempts atomic.Int64
	escalationRenewFaultHook = func(attempt int) error {
		renewAttempts.Store(int64(attempt))
		return errors.New("store unavailable")
	}
	t.Cleanup(func() { escalationRenewFaultHook = nil })

	var cancelledBeforeExpiry atomic.Bool
	var competitorOverlapped atomic.Bool
	manager.onAddCtx = func(effectCtx context.Context) {
		deadline := time.Now().UTC().Add(escalationRecoveryLeaseTTL)
		select {
		case <-effectCtx.Done():
			// (a) cancelled, and cancelled no later than the confirmed expiry.
			if !time.Now().UTC().After(deadline.Add(150 * time.Millisecond)) {
				cancelledBeforeExpiry.Store(true)
			}
		case <-time.After(3 * time.Second):
		}
		// (b) once this pass is cancelled, a competitor may take the fence - and the
		// original must no longer be running external work alongside it.
		now := time.Now().UTC()
		if taken, err := store.AcquireEscalationRecoveryLease(context.Background(), "parent-job",
			round.RoundID, "competitor", now.Add(time.Minute), now); err == nil && taken {
			competitorOverlapped.Store(true)
		}
	}

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}

	if renewAttempts.Load() < 2 {
		t.Fatalf("renewal attempts = %d, want at least 2: the heartbeat never retried through the error", renewAttempts.Load())
	}
	if !cancelledBeforeExpiry.Load() {
		t.Fatal("the in-flight pre-effect was not cancelled by the confirmed expiry: authority outlived the lease")
	}
	if !competitorOverlapped.Load() {
		t.Fatal("the fence never became reclaimable: this test cannot observe the hazard it is named for")
	}
	// (c) NO COMMIT from the cancelled pass.
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0 from a pass whose authority lapsed", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0", got)
	}
	if _, stillOpen := unsettledRound(t, store, "parent-job"); !stillOpen {
		t.Fatal("a pass whose authority lapsed settled the round")
	}
}

// TestRenewalErrorsAfterSuccessesStillCompleteTheRun is the control that keeps the
// expiry bound from becoming a bound that rejects VALID work: renewals that succeed for
// a while and only then start failing must not cancel the pass, because each success
// moved the confirmed expiry forward. A bound anchored on the ORIGINAL expiry would kill
// a run that legitimately held its lease the whole time.
func TestRenewalErrorsAfterSuccessesStillCompleteTheRun(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	manager := pausedImplementEscalation(t, store, &engine)

	originalTTL := escalationRecoveryLeaseTTL
	escalationRecoveryLeaseTTL = 300 * time.Millisecond
	t.Cleanup(func() { escalationRecoveryLeaseTTL = originalTTL })

	// The first two renewals succeed - each advancing the confirmed expiry - and only
	// then does the store start failing.
	escalationRenewFaultHook = func(attempt int) error {
		if attempt <= 2 {
			return nil
		}
		return errors.New("store unavailable after two good renewals")
	}
	t.Cleanup(func() { escalationRenewFaultHook = nil })

	// Work that spans several ticks, so the late errors are genuinely reached.
	manager.onAdd = func() { time.Sleep(450 * time.Millisecond) }

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}

	// THE RUN THAT SHOULD SUCCEED DID: the retry landed exactly once and the round is
	// settled, because the confirmed expiry moved with each successful renewal.
	if got := countJobs(t, store, "/resume"); got != 1 {
		t.Fatalf("resume jobs = %d, want exactly 1: late renewal errors cancelled a run that held its lease", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("receipts = %d, want exactly 1", got)
	}
}

// TestLateRenewalDoesNotExtendAuthorityPastThePersistedExpiry is the P1 of the 6ea2f6b9
// review. The bound must be the expiry the STORE HOLDS, not a clock reading taken after
// the call returns: a slow renewal write persists an expiry that may already have
// elapsed by the time it comes back, and accepting that as "confirmed" claims authority
// the row no longer grants.
//
// The test forces exactly that: the renewal is delayed past the TTL it writes, so a
// competitor can legitimately acquire at the persisted deadline - and the original pass
// must already have cancelled its in-flight pre-effect.
//
// SEMANTIC REVERSION THIS KILLS: accept a late-returning successful renewal as confirmed
// (drop the post-write expiry check), and the original keeps working while a competitor
// owns the round.
func TestLateRenewalDoesNotExtendAuthorityPastThePersistedExpiry(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	manager := pausedImplementEscalation(t, store, &engine)

	originalTTL := escalationRecoveryLeaseTTL
	escalationRecoveryLeaseTTL = 300 * time.Millisecond
	t.Cleanup(func() { escalationRecoveryLeaseTTL = originalTTL })

	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	// Every heartbeat renewal is DELAYED past the expiry it writes, so no renewal can
	// ever confirm authority beyond the persisted deadline.
	escalationRenewFaultHook = func(attempt int) error {
		time.Sleep(2 * escalationRecoveryLeaseTTL)
		return nil
	}
	t.Cleanup(func() { escalationRenewFaultHook = nil })

	var cancelled atomic.Bool
	var competitorAcquired atomic.Bool
	manager.onAddCtx = func(effectCtx context.Context) {
		select {
		case <-effectCtx.Done():
			cancelled.Store(true)
		case <-time.After(3 * time.Second):
		}
		// At the persisted deadline the fence is reclaimable, and the original pass must
		// already have stopped - which the assertion above establishes.
		now := time.Now().UTC()
		if taken, err := store.AcquireEscalationRecoveryLease(context.Background(), "parent-job",
			round.RoundID, "competitor", now.Add(time.Minute), now); err == nil && taken {
			competitorAcquired.Store(true)
		}
	}

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}
	if !cancelled.Load() {
		t.Fatal("the in-flight pre-effect kept running past the persisted expiry: authority was extended by a late renewal")
	}
	if !competitorAcquired.Load() {
		t.Fatal("the fence never became reclaimable: this test cannot observe the hazard it is named for")
	}
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0 from a pass whose authority lapsed", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0", got)
	}
}
