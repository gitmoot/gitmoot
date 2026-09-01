package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

func unsettledRound(t *testing.T, store *db.Store, jobID string) (db.EscalationRound, bool) {
	t.Helper()
	round, ok, err := store.UnsettledEscalationRound(context.Background(), jobID)
	if err != nil {
		t.Fatalf("UnsettledEscalationRound(%s): %v", jobID, err)
	}
	return round, ok
}

// TestTransientEffectFailureBeyondTheBoundKeepsTheClaimAndTheSlot is the v4 P1: a
// retry bound must never make a human's decision disposable. A transient cause — a
// lock, a dependency outage — must leave the claim intact, the slot held, and the
// tree blocked until an operator acts, NOT silently settled.
//
// MUTATION PROOF: settle the round on retry exhaustion (the rejected v3 behaviour)
// and the slot is released and the decision lost.
func TestTransientEffectFailureBeyondTheBoundKeepsTheClaimAndTheSlot(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedEscalationTree(t, store, engine)

	boom := errors.New("transient dependency outage")
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error { return boom }
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); !errors.Is(err, boom) {
		t.Fatalf("ResolveEscalation error = %v, want the injected transient failure", err)
	}
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("the slot was released by a transient failure: the decision is gone")
	}
	if !round.Claimed() {
		t.Fatal("the claim was rolled back; the human's decision must be preserved")
	}
	claimPayload := round.ClaimPayload

	// Sweep past the bound. Every pass must preserve the claim.
	for i := 0; i < escalationRecoveryAttemptBound+2; i++ {
		if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil && !errors.Is(err, boom) {
			t.Fatalf("recovery pass %d: %v", i, err)
		}
	}
	round, ok = unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("retry exhaustion released the slot: a bound must never discard a claim")
	}
	if !round.NeedsRepair() {
		t.Fatalf("round integrity = %q, want needs_repair after the bound", round.IntegrityState)
	}
	if round.ClaimPayload != claimPayload || round.ClaimVerb != string(ResumeContinue) {
		t.Fatalf("the preserved claim changed: verb=%q payload=%q", round.ClaimVerb, round.ClaimPayload)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationNeedsRepairEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1 across repeated sweeps", escalationNeedsRepairEvent, got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0: nothing was applied", escalationEffectsCompletedEvent, got)
	}

	// The parked round is terminal for the sweep: no further attempts, no churn.
	if recovered, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil || recovered != 0 {
		t.Fatalf("sweep over a parked round recovered = %d (err %v), want 0", recovered, err)
	}

	// REPAIR-RETRY through the engine's operator entry, with the cause cleared.
	resolutionEffectsHook = nil
	if err := engine.RepairEscalationRound(ctx, "parent-job", round.RoundID, false, "operator", ""); err != nil {
		t.Fatalf("RepairEscalationRound(retry): %v", err)
	}
	if recovered, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil || recovered != 1 {
		t.Fatalf("recovery after repair recovered = %d (err %v), want 1", recovered, err)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatal("the repaired round never settled")
	}
	if _, err := store.GetJob(ctx, delegationContinuationID("parent-job")); err != nil {
		t.Fatalf("the preserved decision was not applied: %v", err)
	}
}

// TestParkedRoundBlocksANewOpenerAndOrdinaryAdvance pins both halves of the block. A
// claimed-but-unapplied decision must stop the coordinator: advancing past it would
// proceed as if it had been applied.
//
// MUTATION PROOF: allow advance past needs_repair and the continuation is enqueued.
func TestParkedRoundBlocksANewOpenerAndOrdinaryAdvance(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &recordingNotifier{}
	engine.EscalationNotifier = notifier
	sink := &recordingSink{}
	engine.EventSink = sink
	pausedEscalationTree(t, store, engine)

	// An unreplayable verb parks the round on the first sweep: an integrity cause, not
	// a transient one.
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no live round to claim")
	}
	if _, err := store.ClaimEscalationRound(ctx, "parent-job", round.RoundID, "not-a-verb", 0, `{"reason":"not-a-verb"}`, time.Now().UTC()); err != nil {
		t.Fatalf("ClaimEscalationRound: %v", err)
	}
	if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	parked, ok := unsettledRound(t, store, "parent-job")
	if !ok || !parked.NeedsRepair() {
		t.Fatalf("round = %+v, want parked in needs_repair", parked)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationNeedsRepairEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", escalationNeedsRepairEvent, got)
	}
	// OPERATOR-VISIBLE: the attention surface carries the same fact as the report.
	if got := len(sink.byType(events.EventJobNeedsAttention)); got == 0 {
		t.Fatal("a parked round emitted no attention event: a blocked coordinator must not be silent")
	}
	reports, err := engine.EscalationRoundsNeedingRepair(ctx)
	if err != nil {
		t.Fatalf("EscalationRoundsNeedingRepair: %v", err)
	}
	if len(reports) != 1 || reports[0].RoundID != parked.RoundID {
		t.Fatalf("repair report = %+v, want the parked round", reports)
	}

	// A NEW OPENER is blocked: the slot is still held.
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})
	beforeRequests := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent)
	_ = engine.AdvanceJob(ctx, "parent-job/delegation/ui")
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != beforeRequests {
		t.Fatalf("%s events = %d, want %d: a new round opened while a claim was unapplied", escalationRequestedEvent, got, beforeRequests)
	}

	// ORDINARY ADVANCE is refused with the cause, and enqueues nothing.
	err = engine.AdvanceJob(ctx, "parent-job")
	if err == nil {
		t.Fatal("AdvanceJob proceeded past a needs_repair round")
	}
	if !strings.Contains(err.Error(), "needs operator repair") {
		t.Fatalf("AdvanceJob error = %v, want one naming the repair block", err)
	}
	if _, err := store.GetJob(ctx, delegationContinuationID("parent-job")); err == nil {
		t.Fatal("a blocked advance still enqueued its continuation")
	}
}

// TestOnlyADeletedCoordinatorReleasesWithoutEffects pins Class I as the ONE
// structural impossibility, and that its release is named and recorded.
func TestOnlyADeletedCoordinatorReleasesWithoutEffects(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedEscalationTree(t, store, engine)

	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no live round")
	}
	if _, err := store.ClaimEscalationRound(ctx, "parent-job", round.RoundID, string(ResumeContinue), 0, `{"reason":"continue"}`, time.Now().UTC()); err != nil {
		t.Fatalf("ClaimEscalationRound: %v", err)
	}
	// The coordinator row is GONE. There is no store method for this because nothing
	// in production deletes a job; a purge, a restored snapshot or a hand-repaired
	// database can still leave a round whose coordinator is absent.
	if err := store.ExecForTest(ctx, `DELETE FROM jobs WHERE id = ?`, "parent-job"); err != nil {
		t.Fatalf("delete the coordinator row: %v", err)
	}

	if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatal("a round whose coordinator no longer exists still holds the slot")
	}
	settled, err := store.GetEscalationRound(ctx, "parent-job", round.RoundID)
	if err != nil {
		t.Fatalf("GetEscalationRound: %v", err)
	}
	if settled.SettledReason == "" {
		t.Fatal("the no-op release recorded no named reason")
	}
	if _, err := store.GetJob(ctx, delegationContinuationID("parent-job")); err == nil {
		t.Fatal("the no-op release applied effects")
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationNeedsRepairEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0: a deleted coordinator is not an integrity failure", escalationNeedsRepairEvent, got)
	}
}

// TestRepairSupersedeDiscardsOnlyByAnExplicitHumanAct pins the only path that may
// drop a claimed decision, and that it unblocks the coordinator.
func TestRepairSupersedeDiscardsOnlyByAnExplicitHumanAct(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedEscalationTree(t, store, engine)

	round, _ := unsettledRound(t, store, "parent-job")
	if _, err := store.ClaimEscalationRound(ctx, "parent-job", round.RoundID, "not-a-verb", 0, `{"reason":"not-a-verb"}`, time.Now().UTC()); err != nil {
		t.Fatalf("ClaimEscalationRound: %v", err)
	}
	if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}

	// A supersede with no reason is refused: discarding intent needs a stated why.
	if err := engine.RepairEscalationRound(ctx, "parent-job", round.RoundID, true, "operator", ""); err == nil {
		t.Fatal("supersede without a reason was accepted")
	}
	if err := engine.RepairEscalationRound(ctx, "parent-job", round.RoundID, true, "operator", "leg was deleted upstream"); err != nil {
		t.Fatalf("RepairEscalationRound(supersede): %v", err)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatal("supersede did not release the slot")
	}
	settled, err := store.GetEscalationRound(ctx, "parent-job", round.RoundID)
	if err != nil {
		t.Fatalf("GetEscalationRound: %v", err)
	}
	if settled.SettledBy != "operator" || !strings.Contains(settled.SettledReason, "deleted upstream") {
		t.Fatalf("supersede recorded by=%q reason=%q, want the operator and their reason", settled.SettledBy, settled.SettledReason)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationSupersededEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationSupersededEvent, got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0: supersede applies no effects", escalationEffectsCompletedEvent, got)
	}
}

// TestCrashedTTLClaimIsRecovered closes the v3 P1-1: "ttl" is a replayable verb
// through the same shared switch, so a crash between a TTL claim and its effects is
// repaired instead of being permanently unreplayable.
func TestCrashedTTLClaimIsRecovered(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	base := time.Now().UTC()
	engine.Now = func() time.Time { return base }
	pausedEscalationTree(t, store, engine)

	expired := engine
	expired.Now = func() time.Time { return base.Add(49 * time.Hour) }

	boom := errors.New("crash before the receipt")
	fired := false
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		if fired {
			return nil
		}
		fired = true
		return boom
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if _, err := expired.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour); !errors.Is(err, boom) {
		t.Fatalf("TTL sweep error = %v, want the injected crash", err)
	}
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok || !round.Claimed() || round.ClaimVerb != string(ResumeTTL) {
		t.Fatalf("round = %+v, want a claimed ttl round still holding the slot", round)
	}

	if recovered, err := expired.RecoverUnfinishedEscalationResolutions(ctx); err != nil || recovered != 1 {
		t.Fatalf("TTL recovery recovered = %d (err %v), want 1", recovered, err)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatal("the recovered TTL round never settled")
	}
	if _, err := store.GetJob(ctx, delegationContinuationID("parent-job")); err != nil {
		t.Fatalf("the TTL verb's finalize continuation was not replayed: %v", err)
	}
}

// TestConcurrentRecoverersSettleExactlyOnce pins the receipt's exactly-once-ness
// under concurrency: it is an affected-row predicate on the round, so there is
// nothing to over-count.
func TestConcurrentRecoverersSettleExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedEscalationTree(t, store, engine)

	round, _ := unsettledRound(t, store, "parent-job")
	if _, err := store.ClaimEscalationRound(ctx, "parent-job", round.RoundID, string(ResumeContinue), 0, `{"reason":"continue"}`, time.Now().UTC()); err != nil {
		t.Fatalf("ClaimEscalationRound: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = engine.RecoverUnfinishedEscalationResolutions(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent recoverer[%d]: %v", i, err)
		}
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", escalationEffectsCompletedEvent, got)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatal("the round never settled")
	}
}

// TestOrdinaryAdvanceIsUntouchedWithoutAParkedRound is the success control for the
// block: every guard added here has a version that refuses ordinary work.
func TestOrdinaryAdvanceIsUntouchedWithoutAParkedRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "plain-parent", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-plain", TaskID: "task-plain", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: "continue"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "plain-parent"); err != nil {
		t.Fatalf("ordinary AdvanceJob was blocked: %v", err)
	}
	if _, err := store.GetJob(ctx, "plain-parent/delegation/api"); err != nil {
		t.Fatalf("the ordinary advance did not fan out: %v", err)
	}
}

// TestARoundOneCrashCannotAdmitRoundTwoUntilSettlement is the sequence directive
// 104725 named, asserted in the honest form for the chosen mechanism: the slot is
// held from OPEN through SETTLEMENT, so while round 1 is claimed-but-unapplied round
// 2 CANNOT EXIST — which is why round 1's replay can never clear round 2's pause.
//
// The coordinator is NOT re-queued, so the generation guard is silent here: the
// exclusion is doing the work, exactly as designed.
//
// MUTATION PROOF: key the partial unique index on resolved_at IS NULL (vacate the
// slot at claim time) and round 2 opens, after which round 1's replay clobbers it.
func TestARoundOneCrashCannotAdmitRoundTwoUntilSettlement(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &recordingNotifier{}
	engine.EscalationNotifier = notifier
	// BOTH legs are escalate_human here: the sibling must be an ESCALATION opener, not
	// a block_parent leg, or the task state this test watches would move for an
	// unrelated and legitimate reason.
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-5", RepoFullName: "gitmoot/gitmoot", Branch: "task-005", GoalID: "g1",
		Title: "Parent", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-005", TaskID: "task-5", TaskTitle: "Parent", Sender: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "escalate_human"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", FailurePolicy: "escalate_human"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent): %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected the first failure to pause")
	}

	before := mustJob(t, store, "parent-job")
	boom := errors.New("crash before the receipt")
	fired := false
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		if fired {
			return nil
		}
		fired = true
		return boom
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "try again"); !errors.Is(err, boom) {
		t.Fatalf("ResolveEscalation error = %v, want the injected crash", err)
	}
	round1, ok := unsettledRound(t, store, "parent-job")
	if !ok || !round1.Claimed() {
		t.Fatalf("round 1 = %+v, want claimed and still holding the slot", round1)
	}
	if after := mustJob(t, store, "parent-job"); after.LifecycleGeneration != before.LifecycleGeneration {
		t.Fatalf("lifecycle_generation moved (%d -> %d): the generation guard would mask the property under test",
			before.LifecycleGeneration, after.LifecycleGeneration)
	}

	// A sibling failure tries to open round 2 while round 1 is unsettled.
	taskBefore, err := store.GetTask(ctx, "task-5")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	eventsBefore, err := store.ListTaskEvents(ctx, "task-5")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	requestsBefore := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent)
	notificationsBefore := len(notifier.calls)

	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})
	_ = engine.AdvanceJob(ctx, "parent-job/delegation/ui")

	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != requestsBefore {
		t.Fatalf("%s events = %d, want %d: round 2 opened while round 1 was unsettled", escalationRequestedEvent, got, requestsBefore)
	}
	if got := len(notifier.calls); got != notificationsBefore {
		t.Fatalf("notifications = %d, want %d: an unopened round announced", got, notificationsBefore)
	}
	taskAfter, err := store.GetTask(ctx, "task-5")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if taskAfter.State != taskBefore.State {
		t.Fatalf("task state moved %q -> %q while round 1 was unsettled", taskBefore.State, taskAfter.State)
	}
	eventsAfter, err := store.ListTaskEvents(ctx, "task-5")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("task events = %d, want %d: the blocked opener wrote to the trail", len(eventsAfter), len(eventsBefore))
	}

	// ROUND 1 LEAVES NO STRANDED DEBT: the sweep settles it exactly once.
	resolutionEffectsHook = nil
	if recovered, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil || recovered != 1 {
		t.Fatalf("recovery recovered = %d (err %v), want 1", recovered, err)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatal("round 1 still holds the slot after recovery")
	}

	// ONLY THEN does round 2 open, announcing exactly once: the block was bounded.
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err == nil {
		t.Fatal("expected the sibling failure to pause once the slot was free")
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != requestsBefore+1 {
		t.Fatalf("%s events = %d, want %d after settlement", escalationRequestedEvent, got, requestsBefore+1)
	}
	if got := len(notifier.calls); got != notificationsBefore+1 {
		t.Fatalf("notifications = %d, want %d after settlement", got, notificationsBefore+1)
	}
}

// TestRecoveryReplaysTheClaimedRoundsOwnRequest pins RoundID PAIRING: a coordinator
// accumulates rounds over its life, and recovery must read the request record of the
// round it is replaying. Reading "the latest requested event" instead would replay
// one round's decision against another round's delegation — review 18d142f4's P1-3.
//
// MUTATION PROOF: drop the round_id match in loadEscalationForRound and the wrong
// leg is re-enqueued.
func TestRecoveryReplaysTheClaimedRoundsOwnRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-5", RepoFullName: "gitmoot/gitmoot", Branch: "task-005", GoalID: "g1",
		Title: "Parent", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-005", TaskID: "task-5", TaskTitle: "Parent", Sender: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "escalate_human"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", FailurePolicy: "escalate_human"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent): %v", err)
	}

	// ROUND 1 belongs to the API leg, and is resolved and settled completely.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected round 1 to pause")
	}
	// Round 1 resolves with RETRY, which re-enqueues the api leg and leaves NO
	// continuation in flight - a continuation would make the sibling's escalate_human
	// fold instead of opening round 2.
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "retry the api leg"); err != nil {
		t.Fatalf("resolve round 1: %v", err)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatal("round 1 did not settle")
	}
	// The retried api leg SUCCEEDS, so nothing re-pauses on api.
	completeDelegationChild(t, store, "parent-job/delegation/api/resume", JobSucceeded, AgentResult{Decision: "approved", Summary: "api fixed"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api/resume"); err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advance the resumed api leg: %v", err)
	}

	// ROUND 2 belongs to the UI leg. Its resolution crashes before the receipt.
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err == nil {
		t.Fatal("expected round 2 to pause")
	}
	boom := errors.New("crash before the receipt")
	fired := false
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		if fired {
			return nil
		}
		fired = true
		return boom
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "retry the ui leg"); !errors.Is(err, boom) {
		t.Fatalf("resolve round 2 error = %v, want the injected crash", err)
	}

	// Recovery must replay ROUND 2's request: the UI leg, not the API leg.
	resolutionEffectsHook = nil
	if recovered, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil || recovered != 1 {
		t.Fatalf("recovery recovered = %d (err %v), want 1", recovered, err)
	}
	if _, err := store.GetJob(ctx, "parent-job/delegation/ui/resume"); err != nil {
		t.Fatalf("recovery did not replay round 2's own leg: %v", err)
	}
	// The api resume leg exists from ROUND 1 legitimately, so its presence proves
	// nothing; the discriminator is that ROUND 2's own leg was replayed. A claim paired
	// with round 1's request record would have re-enqueued api (idempotent, no new
	// row) and left ui/resume absent - which the assertion above catches.
	if round, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatalf("round 2 still unsettled after recovery: %+v", round)
	}
}

// TestADuplicateResumeAfterSettlementOpensNothing is the deterministic form of the
// defect the FULL SUITE caught and the focused run did not: once a round is settled
// the coordinator's slot is free, and a late duplicate resume must find nothing to
// claim rather than MINTING a round nobody opened.
//
// The concurrent resume-versus-TTL test only sees this when the winner settles before
// the loser looks, which is timing-dependent; this pins the ordering.
//
// MUTATION PROOF: adopt a round whenever no row exists (drop the legacy-evidence
// guard) and the duplicate resume opens and claims a second round.
func TestADuplicateResumeAfterSettlementOpensNothing(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedEscalationTree(t, store, engine)

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatal("the round did not settle")
	}
	resolvedBefore := countWorkflowJobEvents(t, store, "parent-job", escalationResolvedEvent)
	requestedBefore := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent)

	// The duplicate: a second resume comment poll, arriving after settlement.
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); err != nil {
		t.Fatalf("duplicate ResolveEscalation: %v", err)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationResolvedEvent); got != resolvedBefore {
		t.Fatalf("%s events = %d, want %d: a duplicate resume claimed a round nobody opened", escalationResolvedEvent, got, resolvedBefore)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != requestedBefore {
		t.Fatalf("%s events = %d, want %d", escalationRequestedEvent, got, requestedBefore)
	}
	if round, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatalf("a duplicate resume minted a round: %+v", round)
	}

	// AND THE HELPER'S CONTRACT DIRECTLY. The production duplicate above is caught one
	// level earlier by the resolver's idempotency pre-check, so it cannot exercise the
	// adoption decision itself; the reachable production path is the CONCURRENT one,
	// where a loser passes that pre-check and only then finds the slot free (that is
	// how the full suite caught this). This pins the decision that path depends on: a
	// coordinator with no legacy round must never be given one.
	if round, ok, err := engine.adoptOrLoadUnsettledRound(ctx, "parent-job", EscalationRecord{}); err != nil {
		t.Fatalf("adoptOrLoadUnsettledRound: %v", err)
	} else if ok {
		t.Fatalf("adoption minted a round for a coordinator with nothing open: %+v", round)
	}
	if round, ok := unsettledRound(t, store, "parent-job"); ok {
		t.Fatalf("adoption wrote a round row: %+v", round)
	}
}
