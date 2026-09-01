package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// pausedEscalationTree seeds a coordinator paused on an OPEN escalation round
// through the production path, which is the only state in which resolution races
// are reachable.
func pausedEscalationTree(t *testing.T, store *db.Store, engine Engine) {
	t.Helper()
	ctx := context.Background()
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-5", RepoFullName: "gitmoot/gitmoot", Branch: "task-005", GoalID: "g1",
		Title: "Parent", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError on the failing leg")
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1 before the race", escalationRequestedEvent, got)
	}
}

// TestConcurrentResumeAndTTLResolveExactlyOnce is the P1 from head 6de58194. A human
// resume racing TTL auto-finalization both observed requested=1/resolved=0 and both
// appended, driving the counters to 2/2; the NEXT legitimate round then opened to
// 2/2 and could never be resolved again.
//
// MUTATION PROOF: replace CloseHumanRound's conditional append with a plain
// AddJobEvent and two resolved events land, after which the re-escalation assertion
// at the end of this test fails.
func TestConcurrentResumeAndTTLResolveExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	base := time.Now().UTC()
	engine.Now = func() time.Time { return base }
	pausedEscalationTree(t, store, engine)

	// The TTL sweep sees the pause as expired at the same moment the human resumes.
	expired := engine
	expired.Now = func() time.Time { return base.Add(49 * time.Hour) }

	var wg sync.WaitGroup
	var resumeErr, ttlErr error
	var finalized int
	wg.Add(2)
	go func() {
		defer wg.Done()
		resumeErr = engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "")
	}()
	go func() {
		defer wg.Done()
		finalized, ttlErr = expired.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour)
	}()
	wg.Wait()
	if resumeErr != nil {
		t.Fatalf("human resume: %v", resumeErr)
	}
	if ttlErr != nil {
		t.Fatalf("TTL finalize: %v", ttlErr)
	}

	resolved := countWorkflowJobEvents(t, store, "parent-job", escalationResolvedEvent)
	if resolved != 1 {
		t.Fatalf("%s events = %d, want exactly 1: resume and TTL both resolved one round (finalized=%d)", escalationResolvedEvent, resolved, finalized)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1: the winner's effects were not receipted once", escalationEffectsCompletedEvent, got)
	}

	// THE CONSEQUENCE THAT MATTERS: a later legitimate round must still be
	// resolvable, i.e. requested=2 with resolved=1. The retry verb re-enqueues the
	// leg (no continuation in flight), so a second failure genuinely re-pauses.
	resumeJobID := "parent-job/delegation/api/resume"
	if _, err := store.GetJob(ctx, resumeJobID); err != nil {
		t.Fatalf("the winning resolver did not enqueue the retry leg: %v", err)
	}
	completeDelegationChild(t, store, resumeJobID, JobFailed, AgentResult{Decision: "failed", Summary: "failed again"})
	if err := engine.AdvanceJob(ctx, resumeJobID); err == nil {
		t.Fatal("expected the re-escalation to pause again")
	}
	requested := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent)
	resolved = countWorkflowJobEvents(t, store, "parent-job", escalationResolvedEvent)
	if requested != 2 || resolved != 1 {
		t.Fatalf("counters after re-escalation = requested %d / resolved %d, want 2/1: the new round is unresolvable", requested, resolved)
	}
	open, err := engine.escalationOpen(ctx, "parent-job")
	if err != nil {
		t.Fatalf("escalationOpen: %v", err)
	}
	if !open {
		t.Fatal("the re-escalated round reports CLOSED: it can never be resolved")
	}
}

// TestConcurrentResumesResolveExactlyOnce is the two-caller variant: a duplicate
// resume comment poll must not double-run the verb's effects.
func TestConcurrentResumesResolveExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedEscalationTree(t, store, engine)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "try again")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ResolveEscalation[%d]: %v", i, err)
		}
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", escalationResolvedEvent, got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", escalationEffectsCompletedEvent, got)
	}
	// The retry leg is enqueued once: its id is deterministic, so a second run would
	// be a no-op, but the resolved-event count above is what bounds the effects.
	if _, err := store.GetJob(ctx, "parent-job/delegation/api/resume"); err != nil {
		t.Fatalf("the winning resume did not enqueue its leg: %v", err)
	}
}

// TestASingleResumeStillResolvesAndActs is the run that must SUCCEED. Every guard
// added here has a version that refuses an ordinary resume.
func TestASingleResumeStillResolvesAndActs(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedEscalationTree(t, store, engine)

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); err != nil {
		t.Fatalf("ResolveEscalation(continue): %v", err)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationResolvedEvent, got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationEffectsCompletedEvent, got)
	}
	if _, err := store.GetJob(ctx, delegationContinuationID("parent-job")); err != nil {
		t.Fatalf("continue did not enqueue the coordinator continuation: %v", err)
	}
	assertTaskState(t, store, "task-5", TaskPlanned)
}

// TestAClaimedResolutionWhoseEffectsFailIsRecovered is the debt the claim-before-act
// ordering creates, and the reason the receipt exists. A crash between the claim and
// the effects must not strand the tree: a closed round is no candidate for any other
// sweep, so without the receipt gap nothing would ever look at it again.
//
// MUTATION PROOF: delete the JobIDsWithUnfinishedEscalationResolution sweep call and
// the stranded tree is never repaired.
func TestAClaimedResolutionWhoseEffectsFailIsRecovered(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedEscalationTree(t, store, engine)

	// The crash: the effects run, the receipt never lands.
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

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); !errors.Is(err, boom) {
		t.Fatalf("ResolveEscalation error = %v, want the injected crash", err)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1: the claim must be durable", escalationResolvedEvent, got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0: the crash must leave the resolution unfinished", escalationEffectsCompletedEvent, got)
	}
	// A duplicate resume cannot repair it: the round is closed, so the resolver is a
	// no-op. This is exactly why the recovery sweep is required.
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); err != nil {
		t.Fatalf("duplicate resume after a claimed resolution: %v", err)
	}

	// The next poll repairs it, THROUGH THE PRODUCTION DAEMON ENTRY. Calling the
	// recovery helper directly would pass even if nothing ever invoked it.
	if _, err := engine.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour); err != nil {
		t.Fatalf("AutoFinalizeExpiredEscalations: %v", err)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1 after recovery", escalationEffectsCompletedEvent, got)
	}
	if _, err := store.GetJob(ctx, delegationContinuationID("parent-job")); err != nil {
		t.Fatalf("the recovered resolution did not enqueue its continuation: %v", err)
	}
	assertTaskState(t, store, "task-5", TaskPlanned)

	// And it is not re-driven forever: a finished resolution is no longer a candidate.
	if recovered, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil || recovered != 0 {
		t.Fatalf("second recovery pass recovered = %d (err %v), want 0", recovered, err)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1: recovery ran twice", escalationEffectsCompletedEvent, got)
	}
}

// TestConcurrentOpenerLoserWritesNoMergedRegressionAudit is the P2. A concurrent
// LOSER's task is genuinely awaiting_human, but awaiting_human is a merged-regression
// target, so classifying a loser as a refusal wrote a FALSE
// task_merged_regression_refused row into the durable lifecycle audit — on exactly
// the path the concurrency tests exercise, which never asserted task events.
//
// MUTATION PROOF: collapse HumanRoundRefused back into the already-open outcome and
// the false audit row appears.
func TestConcurrentOpenerLoserWritesNoMergedRegressionAudit(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

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
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})

	var wg sync.WaitGroup
	for _, childID := range []string{"parent-job/delegation/api", "parent-job/delegation/ui"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = engine.AdvanceJob(ctx, id)
		}(childID)
	}
	wg.Wait()

	events, err := store.ListTaskEvents(ctx, "task-5")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == TaskEventMergedRegressionRefused {
			t.Fatalf("a concurrent opener LOSER wrote a false landed-work refusal: %+v", event)
		}
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", escalationRequestedEvent, got)
	}
}

// TestGenuineMergedRefusalStillRecordsItsAudit is the positive half: the audit row
// must still be written when the refusal is real, or the P2 fix would have deleted a
// durable lifecycle record instead of correcting its attribution.
func TestGenuineMergedRefusalStillRecordsItsAudit(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	child, _ := seedFailurePolicyTree(t, store, engine, "escalate_human")

	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	task.State = string(TaskMerged)
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask(merged): %v", err)
	}

	if err := engine.AdvanceJob(ctx, child); err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("AdvanceJob: %v", err)
	}
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	refusals := 0
	for _, event := range events {
		if event.Kind == TaskEventMergedRegressionRefused {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("%s events = %d, want exactly 1 for a genuine refusal", TaskEventMergedRegressionRefused, refusals)
	}
}

// TestTTLClaimRefusesAfterAHumanResolvesInTheWindow makes the resume-versus-TTL race
// DETERMINISTIC. The parallel test above is timing-dependent: whichever caller wins
// first, the loser often never reaches its append, so an unguarded TTL append can
// survive it. This one places the human resume exactly in the TTL sweep's
// candidate-to-claim window.
//
// MUTATION PROOF: replace the TTL sweep's CloseHumanRound with a plain AddJobEvent
// and two resolved events land here.
func TestTTLClaimRefusesAfterAHumanResolvesInTheWindow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	base := time.Now().UTC()
	engine.Now = func() time.Time { return base }
	pausedEscalationTree(t, store, engine)

	expired := engine
	expired.Now = func() time.Time { return base.Add(49 * time.Hour) }

	fired := false
	escalationTTLPreClaimHook = func(hookCtx context.Context, jobID string) {
		if fired {
			return
		}
		fired = true
		// The human answers while the sweep holds a stale candidate.
		if err := engine.ResolveEscalation(hookCtx, jobID, ResumeRetry, ""); err != nil {
			t.Errorf("human resume inside the TTL window: %v", err)
		}
	}
	t.Cleanup(func() { escalationTTLPreClaimHook = nil })

	finalized, err := expired.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour)
	if err != nil {
		t.Fatalf("AutoFinalizeExpiredEscalations: %v", err)
	}
	if !fired {
		t.Fatal("the TTL claim window was never entered; the test proves nothing")
	}
	if finalized != 0 {
		t.Fatalf("finalized = %d, want 0: the sweep finalized a round a human had resolved", finalized)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1: the TTL sweep double-resolved", escalationResolvedEvent, got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", escalationEffectsCompletedEvent, got)
	}
	// The human's verb is the one that ran: a retry leg, not a finalize continuation.
	if _, err := store.GetJob(ctx, "parent-job/delegation/api/resume"); err != nil {
		t.Fatalf("the human's retry leg is missing: %v", err)
	}
	if _, err := store.GetJob(ctx, delegationContinuationID("parent-job")); err == nil {
		t.Fatal("the TTL sweep enqueued its finalize continuation for a round it did not own")
	}
}
