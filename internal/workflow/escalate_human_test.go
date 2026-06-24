package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

// recordingNotifier captures EscalationNotifier calls for the best-effort
// notification assertions. errOnNotify makes NotifyEscalation fail so the test
// can prove the pause still succeeds when the notifier errors.
type recordingNotifier struct {
	calls       []EscalationRequest
	errOnNotify bool
}

func (n *recordingNotifier) NotifyEscalation(_ context.Context, request EscalationRequest) error {
	n.calls = append(n.calls, request)
	if n.errOnNotify {
		return errors.New("notify failed")
	}
	return nil
}

// seedEscalateHumanCoordinator inserts a coordinator with a single escalate_human
// delegation plus an independent sibling, advances it once to dispatch the
// children, then completes the escalate_human leg as failed. It returns the
// engine so the test can assert on the resulting pause.
func seedEscalateHumanCoordinator(t *testing.T, store *db.Store, engine Engine) {
	t.Helper()
	ctx := context.Background()
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "escalate_human"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
}

func TestEscalateHumanPausesAndEnqueuesNoContinuation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	notifier := &recordingNotifier{}
	engine := testEngine(store)
	engine.EscalationNotifier = notifier

	seedEscalateHumanCoordinator(t, store, engine)

	err := engine.AdvanceJob(ctx, "parent-job/delegation/api")
	var awaiting AwaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("AdvanceJob(api) error = %v, want AwaitingHumanError", err)
	}

	// The shared parent task is paused, NOT blocked.
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)

	// No continuation is enqueued: the tree consumes zero compute while waiting.
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("escalate_human must NOT enqueue a continuation while awaiting a human")
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 0 {
		t.Fatalf("delegation_continuation_enqueued events = %d, want 0", got)
	}

	// Exactly one one-shot escalation event, carrying the failing leg + reason.
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationRequestedEvent, got)
	}
	rec, exists, err := engine.loadEscalation(ctx, "parent-job")
	if err != nil || !exists {
		t.Fatalf("loadEscalation = (%+v, %v, %v), want a record", rec, exists, err)
	}
	if rec.DelegationID != "api" || rec.ChildJobID != "parent-job/delegation/api" || rec.Reason != "api broke" {
		t.Fatalf("escalation record = %+v, want delegation api / child / reason", rec)
	}

	// Notifier invoked once, best-effort, with the resume context.
	if len(notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(notifier.calls))
	}
	if c := notifier.calls[0]; c.CoordinatorJobID != "parent-job" || c.DelegationID != "api" {
		t.Fatalf("notifier request = %+v, want coordinator parent-job / delegation api", c)
	}
}

func TestEscalateHumanRequestedEventIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	notifier := &recordingNotifier{}
	engine := testEngine(store)
	engine.EscalationNotifier = notifier

	seedEscalateHumanCoordinator(t, store, engine)

	// Advance the failing leg twice (mirrors a concurrent / repeated child advance).
	for i := 0; i < 2; i++ {
		err := engine.AdvanceJob(ctx, "parent-job/delegation/api")
		var awaiting AwaitingHumanError
		if !errors.As(err, &awaiting) {
			t.Fatalf("AdvanceJob(api) iteration %d error = %v, want AwaitingHumanError", i, err)
		}
	}
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events after re-advance = %d, want 1 (one-shot)", escalationRequestedEvent, got)
	}
	// The notifier must not re-fire on the idempotent re-advance.
	if len(notifier.calls) != 1 {
		t.Fatalf("notifier calls after re-advance = %d, want 1", len(notifier.calls))
	}
}

func TestEscalateHumanPausesEvenWhenNotifierErrors(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{errOnNotify: true}

	seedEscalateHumanCoordinator(t, store, engine)

	err := engine.AdvanceJob(ctx, "parent-job/delegation/api")
	var awaiting AwaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("AdvanceJob(api) error = %v, want AwaitingHumanError even when notifier errors", err)
	}
	// The pause is durable regardless of the notifier failure.
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1 despite notifier error", escalationRequestedEvent, got)
	}
}

func TestEscalateHumanPausedTreeIsNotABudgetFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	// A paused tree must not be a block, a finalize, or a budget-trip event.
	for _, kind := range []string{
		"delegation_finalize_enqueued",
		"delegation_walltime_exceeded",
		"delegation_budget_exceeded",
		"delegation_cost_exceeded",
	} {
		if got := countJobEvents(t, store, "parent-job", kind); got != 0 {
			t.Fatalf("%s events = %d, want 0 for a paused tree", kind, got)
		}
	}
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatal("a paused-for-human tree must not be blocked")
	}
}

// TestRootWallClockExcludesPausedTime pins that the wall-clock backstop subtracts
// time the tree spent paused awaiting a human. A tree whose raw age exceeds the
// budget only because of a long pause is NOT reported as over budget.
func TestRootWallClockExcludesPausedTime(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)

	base := time.Now().UTC()
	// "Now" is MaxDelegationWallClock + 5h past the root creation: raw elapsed is
	// over budget.
	now := base.Add(MaxDelegationWallClock + 5*time.Hour)
	engine.Now = func() time.Time { return now }

	// Seed a root coordinator job.
	insertCompletedJob(t, store, db.Job{ID: "root", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "b", TaskID: "task-5", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "x"},
	})

	// Record a 6h pause window on the root via the escalation events, so excluding
	// it brings active elapsed back under budget.
	pausedAt := base.Add(MaxDelegationWallClock - time.Hour)
	resolvedAt := pausedAt.Add(6 * time.Hour)
	addEscalationEvent(t, store, "root", escalationRequestedEvent, EscalationRecord{DelegationID: "api", PausedAt: pausedAt.Format(time.RFC3339)})
	addEscalationEvent(t, store, "root", escalationResolvedEvent, EscalationRecord{DelegationID: "api", PausedAt: resolvedAt.Format(time.RFC3339)})

	exceeded, elapsed := engine.rootWallClockExceeded(ctx, "root")
	if exceeded {
		t.Fatalf("wall-clock should exclude the 6h pause; got exceeded with elapsed %s", elapsed)
	}

	// Control: with no pause recorded, the same clock IS over budget.
	insertCompletedJob(t, store, db.Job{ID: "root2", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "b", TaskID: "task-6", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "x"},
	})
	if exceeded, _ := engine.rootWallClockExceeded(ctx, "root2"); !exceeded {
		t.Fatal("a tree with no pause and the same clock must be over wall-clock budget")
	}
}

func addEscalationEvent(t *testing.T, store *db.Store, jobID, kind string, rec EscalationRecord) {
	t.Helper()
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal escalation record: %v", err)
	}
	if err := store.AddJobEvent(context.Background(), db.JobEvent{JobID: jobID, Kind: kind, Message: string(encoded)}); err != nil {
		t.Fatalf("AddJobEvent: %v", err)
	}
}

func TestResolveEscalationRetryReenqueuesLeg(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "use the staging endpoint"); err != nil {
		t.Fatalf("ResolveEscalation(retry) returned error: %v", err)
	}
	resumeJobID := "parent-job/delegation/api/resume"
	if !jobExists(t, store, resumeJobID) {
		t.Fatal("retry must re-enqueue the failing leg")
	}
	// The human's guidance is folded into the re-run leg's prompt.
	payload, err := unmarshalPayload(mustJob(t, store, resumeJobID).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if want := "use the staging endpoint"; !containsSubstr(payload.Instructions, want) {
		t.Fatalf("retry leg instructions = %q, want it to fold in %q", payload.Instructions, want)
	}
	// The task leaves awaiting_human and the resolution is recorded once.
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskAwaitingHuman) {
		t.Fatal("resume must clear awaiting_human")
	}
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationResolvedEvent, got)
	}

	// Idempotent: a second resume is a no-op (no duplicate resolution event).
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); err != nil {
		t.Fatalf("second ResolveEscalation returned error: %v", err)
	}
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events after second resume = %d, want 1 (idempotent)", escalationResolvedEvent, got)
	}
}

// TestEscalateHumanRetryFailingAgainRePausesAndRemainsResolvable is the
// regression for the round-based escalation fix (#340): a `retry` resume that
// itself fails again must open a FRESH escalation round (re-pause the tree,
// re-notify the human, enqueue no continuation) and remain resolvable/finalizable
// — never stranded in planned with nothing in flight.
func TestEscalateHumanRetryFailingAgainRePausesAndRemainsResolvable(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	notifier := &recordingNotifier{}
	engine := testEngine(store)
	engine.EscalationNotifier = notifier

	// Round 1: the leg fails and the tree pauses awaiting a human.
	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError on the first failure")
	}
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events after round 1 = %d, want 1", escalationRequestedEvent, got)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("notifier calls after round 1 = %d, want 1", len(notifier.calls))
	}

	// The human resumes with `retry`: the failing leg is re-enqueued and round 1
	// is resolved (the task leaves awaiting_human, planned for now).
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "try the staging endpoint"); err != nil {
		t.Fatalf("ResolveEscalation(retry) returned error: %v", err)
	}
	resumeJobID := "parent-job/delegation/api/resume"
	if !jobExists(t, store, resumeJobID) {
		t.Fatal("retry must re-enqueue the failing leg")
	}
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events after retry resume = %d, want 1", escalationResolvedEvent, got)
	}
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskAwaitingHuman) {
		t.Fatal("retry resume must clear awaiting_human")
	}

	// The re-run ALSO fails — a normal, expected outcome (the leg already failed
	// once). Advancing it must open a FRESH escalation round, not silently strand.
	completeDelegationChild(t, store, resumeJobID, JobFailed, AgentResult{Decision: "failed", Summary: "api still broken"})
	err := engine.AdvanceJob(ctx, resumeJobID)
	var awaiting AwaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("AdvanceJob(resume) error = %v, want AwaitingHumanError (re-pause)", err)
	}

	// Re-pause: the task is awaiting_human again so the dashboard Attention section
	// re-shows it.
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)

	// A SECOND requested event exists (a fresh round), and the human was re-notified.
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 2 {
		t.Fatalf("%s events after re-pause = %d, want 2 (a new round)", escalationRequestedEvent, got)
	}
	if len(notifier.calls) != 2 {
		t.Fatalf("notifier calls after re-pause = %d, want 2 (re-notified)", len(notifier.calls))
	}

	// Still zero compute: no continuation was enqueued by the re-pause.
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("a re-pause must NOT enqueue a continuation")
	}

	// The fresh round is OPEN again, so a follow-up `/gitmoot resume ... abort`
	// resolves and finalizes it — proving the tree is not permanently stranded.
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAbort, "give up"); err != nil {
		t.Fatalf("ResolveEscalation(abort) on the re-pause returned error: %v", err)
	}
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 2 {
		t.Fatalf("%s events after abort = %d, want 2 (both rounds resolved)", escalationResolvedEvent, got)
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("abort on the re-pause must route to the graceful finalize continuation: %+v", cont)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
}

// TestAutoFinalizeExpiredEscalationsCatchesRePause pins that a re-paused tree (a
// retried leg that failed again, opening a new round) that nobody answers is
// still caught by the wall-clock TTL backstop — the anti-stranding guarantee
// holds across rounds (#340).
func TestAutoFinalizeExpiredEscalationsCatchesRePause(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	base := time.Now().UTC()
	engine.Now = func() time.Time { return base }

	// Round 1 pause → retry → re-run fails again → re-pause (round 2 open).
	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError on the first failure")
	}
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation(retry) returned error: %v", err)
	}
	resumeJobID := "parent-job/delegation/api/resume"
	completeDelegationChild(t, store, resumeJobID, JobFailed, AgentResult{Decision: "failed", Summary: "still broken"})
	if err := engine.AdvanceJob(ctx, resumeJobID); err == nil {
		t.Fatal("expected AwaitingHumanError on the re-pause")
	}
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)

	// Within TTL: the OPEN re-pause is not finalized.
	if finalized, err := engine.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour); err != nil || finalized != 0 {
		t.Fatalf("within-TTL re-pause finalized = %d (err %v), want 0", finalized, err)
	}

	// Past TTL: the never-answered re-pause auto-finalizes gracefully.
	engine.Now = func() time.Time { return base.Add(49 * time.Hour) }
	finalized, err := engine.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour)
	if err != nil {
		t.Fatalf("AutoFinalizeExpiredEscalations returned error: %v", err)
	}
	if finalized != 1 {
		t.Fatalf("past-TTL re-pause finalized = %d, want 1", finalized)
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("TTL auto-finalize of a re-pause must enqueue a finalize continuation: %+v", cont)
	}
}

func TestResolveEscalationContinueEnqueuesContinuation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}
	// The independent sibling also finishes so the continuation has all outcomes.
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobSucceeded, AgentResult{Decision: "approved", Summary: "ui ok"})

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); err != nil {
		t.Fatalf("ResolveEscalation(continue) returned error: %v", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("continue must enqueue the coordinator continuation")
	}
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationResolvedEvent, got)
	}
}

func TestResolveEscalationAbortFinalizes(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAbort, "not worth it"); err != nil {
		t.Fatalf("ResolveEscalation(abort) returned error: %v", err)
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("abort must route to the graceful finalize continuation: %+v", cont)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
}

func TestResolveEscalationRejectsUnknownAndUnpaused(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	// Unknown verb is rejected before any side effect.
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeDecision("frobnicate"), ""); err == nil {
		t.Fatal("ResolveEscalation must reject an unknown decision")
	}
	// A coordinator with no pending escalation is rejected.
	insertCompletedJob(t, store, db.Job{ID: "no-escalation", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", TaskID: "task-7", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "x"},
	})
	if err := engine.ResolveEscalation(ctx, "no-escalation", ResumeRetry, ""); err == nil {
		t.Fatal("ResolveEscalation must reject a coordinator with no pending escalation")
	}
}

func TestAutoFinalizeExpiredEscalations(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	base := time.Now().UTC()
	engine.Now = func() time.Time { return base }
	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	// Within TTL: no finalize.
	finalized, err := engine.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour)
	if err != nil {
		t.Fatalf("AutoFinalizeExpiredEscalations returned error: %v", err)
	}
	if finalized != 0 {
		t.Fatalf("finalized = %d within TTL, want 0", finalized)
	}
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("within-TTL pause must not finalize")
	}

	// Advance the clock past the TTL: the paused tree auto-finalizes gracefully.
	engine.Now = func() time.Time { return base.Add(49 * time.Hour) }
	finalized, err = engine.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour)
	if err != nil {
		t.Fatalf("AutoFinalizeExpiredEscalations returned error: %v", err)
	}
	if finalized != 1 {
		t.Fatalf("finalized = %d past TTL, want 1", finalized)
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("TTL auto-finalize must enqueue a finalize continuation: %+v", cont)
	}
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1 after TTL finalize", escalationResolvedEvent, got)
	}

	// Idempotent: a second scan does nothing (already resolved).
	finalized, err = engine.AutoFinalizeExpiredEscalations(ctx, 48*time.Hour)
	if err != nil {
		t.Fatalf("second AutoFinalizeExpiredEscalations returned error: %v", err)
	}
	if finalized != 0 {
		t.Fatalf("second scan finalized = %d, want 0 (idempotent)", finalized)
	}
}

func TestParseResumeDecision(t *testing.T) {
	for _, in := range []string{"retry", "RETRY", " continue ", "abort"} {
		if _, ok := ParseResumeDecision(in); !ok {
			t.Fatalf("ParseResumeDecision(%q) ok = false, want true", in)
		}
	}
	if _, ok := ParseResumeDecision("nope"); ok {
		t.Fatal("ParseResumeDecision(nope) ok = true, want false")
	}
}

// containsSubstr is a tiny local helper to avoid importing strings just for one
// assertion (the test file already pulls its other deps).
func containsSubstr(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
