package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

// seedAskGateCoordinator inserts a HEALTHY coordinator whose result carries
// human_questions[] (the ask-gate, #445). Unlike the escalate_human seed, no
// child fails: the pause is opened by the asking job's OWN AdvanceJob.
func seedAskGateCoordinator(t *testing.T, store *db.Store, questions []HumanQuestion) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision:       "approved",
			Summary:        "need a decision before fanning out",
			HumanQuestions: questions,
		},
	})
}

func TestAskGatePausesHealthyResult(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	notifier := &recordingNotifier{}
	engine := testEngine(store)
	engine.EscalationNotifier = notifier

	seedAskGateCoordinator(t, store, []HumanQuestion{
		{ID: "q1", Prompt: "Target v2 or v3 API?", Choices: []string{"v2", "v3"}},
	})

	err := engine.AdvanceJob(ctx, "parent-job")
	var awaiting AwaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("AdvanceJob(parent) error = %v, want AwaitingHumanError", err)
	}

	// The task is paused at awaiting_human, NOT blocked or failed.
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)

	// No continuation is enqueued: zero compute while waiting.
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("ask-gate must NOT enqueue a continuation while awaiting a human")
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 0 {
		t.Fatalf("delegation_continuation_enqueued events = %d, want 0", got)
	}

	// Exactly one escalation-requested event, tagged Kind="ask" with the question.
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationRequestedEvent, got)
	}
	rec, exists, err := engine.loadEscalation(ctx, "parent-job")
	if err != nil || !exists {
		t.Fatalf("loadEscalation = (%+v, %v, %v), want a record", rec, exists, err)
	}
	if rec.Kind != escalationKindAsk {
		t.Fatalf("escalation record Kind = %q, want %q", rec.Kind, escalationKindAsk)
	}
	if len(rec.Questions) != 1 || rec.Questions[0].ID != "q1" {
		t.Fatalf("escalation record Questions = %+v, want one q1", rec.Questions)
	}

	// Notifier invoked once, best-effort, flagged as an ask with the questions.
	if len(notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(notifier.calls))
	}
	if c := notifier.calls[0]; !c.Ask || c.CoordinatorJobID != "parent-job" || len(c.Questions) != 1 {
		t.Fatalf("notifier request = %+v, want Ask=true coordinator parent-job one question", c)
	}
}

func TestAskGateIsIdempotentWithinOpenRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	notifier := &recordingNotifier{}
	engine := testEngine(store)
	engine.EscalationNotifier = notifier

	seedAskGateCoordinator(t, store, []HumanQuestion{{ID: "q1", Prompt: "v2 or v3?"}})

	for i := 0; i < 3; i++ {
		err := engine.AdvanceJob(ctx, "parent-job")
		var awaiting AwaitingHumanError
		if !errors.As(err, &awaiting) {
			t.Fatalf("AdvanceJob(parent) iteration %d error = %v, want AwaitingHumanError", i, err)
		}
	}
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events after re-advance = %d, want 1 (one-shot)", escalationRequestedEvent, got)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("notifier calls after re-advance = %d, want 1", len(notifier.calls))
	}
}

func TestAskGatePausesEvenWhenNotifierErrors(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{errOnNotify: true}

	seedAskGateCoordinator(t, store, []HumanQuestion{{ID: "q1", Prompt: "v2 or v3?"}})

	err := engine.AdvanceJob(ctx, "parent-job")
	var awaiting AwaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("AdvanceJob(parent) error = %v, want AwaitingHumanError even when notifier errors", err)
	}
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1 despite notifier error", escalationRequestedEvent, got)
	}
}

func TestResolveEscalationAnswerEnqueuesContinuationWithAnswers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedAskGateCoordinator(t, store, []HumanQuestion{
		{ID: "q1", Prompt: "Target v2 or v3 API?"},
		{ID: "q2", Prompt: "Use legacy auth?"},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAnswer, "q1: v3\nq2: no"); err != nil {
		t.Fatalf("ResolveEscalation(answer) returned error: %v", err)
	}

	// The coordinator continuation is enqueued and carries the answer block.
	contID := delegationContinuationID("parent-job")
	if !jobExists(t, store, contID) {
		t.Fatal("answer must enqueue the coordinator continuation")
	}
	cont, err := unmarshalPayload(mustJob(t, store, contID).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !strings.Contains(cont.HumanAnswer, "v3") || !strings.Contains(cont.HumanAnswer, "q1") {
		t.Fatalf("continuation HumanAnswer = %q, want q1 -> v3", cont.HumanAnswer)
	}
	// The continuation prompt renders the labelled human-answers block at the top.
	if !strings.Contains(cont.Instructions, "Human answers to your questions") {
		t.Fatalf("continuation prompt missing answer block: %q", cont.Instructions)
	}
	if !strings.Contains(cont.Instructions, "v3") || !strings.Contains(cont.Instructions, "no") {
		t.Fatalf("continuation prompt missing answers: %q", cont.Instructions)
	}

	// The resolution event records Kind=ask + the parsed answers.
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationResolvedEvent, got)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_ask_answered"); got != 1 {
		t.Fatalf("delegation_ask_answered events = %d, want 1", got)
	}

	// Idempotent: a duplicate answer resume is a no-op (no double continuation).
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAnswer, "q1: v3\nq2: no"); err != nil {
		t.Fatalf("duplicate ResolveEscalation(answer) returned error: %v", err)
	}
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 1 {
		t.Fatalf("%s events after duplicate = %d, want 1 (idempotent)", escalationResolvedEvent, got)
	}
}

func TestResolveEscalationAnswerSingleQuestionConvenience(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedAskGateCoordinator(t, store, []HumanQuestion{{ID: "q1", Prompt: "v2 or v3?"}})
	if err := engine.AdvanceJob(ctx, "parent-job"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	// A single question accepts a bare answer body (no "<id>:" prefix needed).
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAnswer, "go with v3"); err != nil {
		t.Fatalf("ResolveEscalation(answer) returned error: %v", err)
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !strings.Contains(cont.HumanAnswer, "go with v3") {
		t.Fatalf("continuation HumanAnswer = %q, want the bare body mapped to q1", cont.HumanAnswer)
	}
}

func TestResolveEscalationAnswerRejectsOnFailureRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	// A FAILURE escalate_human round (not an ask round).
	seedEscalateHumanCoordinator(t, store, engine)
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	err := engine.ResolveEscalation(ctx, "parent-job", ResumeAnswer, "q1: v3")
	if err == nil {
		t.Fatal("answer must be rejected on a failure escalation round")
	}
	if !strings.Contains(err.Error(), "answer") {
		t.Fatalf("error = %v, want a clear answer/round mismatch message", err)
	}
	// The pause is intact: no continuation, no resolution.
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0 (rejected verb has no side effect)", escalationResolvedEvent, got)
	}
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)
}

func TestRetryContinueAbortRejectedOnAskRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedAskGateCoordinator(t, store, []HumanQuestion{{ID: "q1", Prompt: "v2 or v3?"}})
	if err := engine.AdvanceJob(ctx, "parent-job"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	for _, verb := range []ResumeDecision{ResumeRetry, ResumeContinue, ResumeAbort} {
		if err := engine.ResolveEscalation(ctx, "parent-job", verb, "anything"); err == nil {
			t.Fatalf("%s must be rejected on an ask round", verb)
		}
	}
	// The ask pause is intact after every rejected failure verb.
	if got := countJobEvents(t, store, "parent-job", escalationResolvedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0", escalationResolvedEvent, got)
	}
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)
}

func TestAskGateAutoFinalizesAfterTTL(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	base := time.Now().UTC()
	engine.Now = func() time.Time { return base }
	seedAskGateCoordinator(t, store, []HumanQuestion{{ID: "q1", Prompt: "v2 or v3?"}})
	if err := engine.AdvanceJob(ctx, "parent-job"); err == nil {
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

	// Past TTL: the unanswered ask round auto-finalizes gracefully, exactly like a
	// failure escalation (it rides the same event kinds).
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
		t.Fatalf("TTL auto-finalize of an ask must enqueue a finalize continuation: %+v", cont)
	}
}

func TestAskGatePausedTimeExcludedFromWallClock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	base := time.Now().UTC()
	engine.Now = func() time.Time { return base }
	seedAskGateCoordinator(t, store, []HumanQuestion{{ID: "q1", Prompt: "v2 or v3?"}})
	if err := engine.AdvanceJob(ctx, "parent-job"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	// 10h later, still paused (no resolved event): the pause window counts.
	now := base.Add(10 * time.Hour)
	paused := engine.rootPausedDuration(ctx, "parent-job", now)
	if paused < 9*time.Hour {
		t.Fatalf("rootPausedDuration = %s, want >= 9h (the open ask pause is excluded from wall-clock)", paused)
	}
}

func TestAskGateBudgetNeutral(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedAskGateCoordinator(t, store, []HumanQuestion{{ID: "q1", Prompt: "v2 or v3?"}})
	if err := engine.AdvanceJob(ctx, "parent-job"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}

	// The pause enqueues no job: the only job in the tree is the asking coordinator.
	count, err := engine.countRootDelegationJobs(ctx, "parent-job")
	if err != nil {
		t.Fatalf("countRootDelegationJobs returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("root job count while paused = %d, want 1 (the pause is budget-neutral)", count)
	}

	// After the answer, the single continuation occupies one slot.
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAnswer, "v3"); err != nil {
		t.Fatalf("ResolveEscalation(answer) returned error: %v", err)
	}
	count, err = engine.countRootDelegationJobs(ctx, "parent-job")
	if err != nil {
		t.Fatalf("countRootDelegationJobs returned error: %v", err)
	}
	if count != 2 {
		t.Fatalf("root job count after answer = %d, want 2 (coordinator + its single continuation)", count)
	}
}

func TestNoHumanQuestionsIsByteIdenticalNoPause(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	// A healthy result WITHOUT human_questions[] never pauses: no escalation event,
	// task is not awaiting_human.
	insertCompletedJob(t, store, db.Job{ID: "plain-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-9",
		Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "done, no questions"},
	})
	if err := engine.AdvanceJob(ctx, "plain-job"); err != nil {
		t.Fatalf("AdvanceJob(plain) returned error: %v", err)
	}
	if got := countJobEvents(t, store, "plain-job", escalationRequestedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0 for a result without human_questions", escalationRequestedEvent, got)
	}
	if task, _ := store.GetTask(ctx, "task-9"); task.State == string(TaskAwaitingHuman) {
		t.Fatal("a result without human_questions must not pause at awaiting_human")
	}
}

func TestParseHumanAnswers(t *testing.T) {
	questions := []HumanQuestion{{ID: "q1", Prompt: "a"}, {ID: "q2", Prompt: "b"}}
	got := parseHumanAnswers(questions, "q1: v3\nq2: no")
	if got["q1"] != "v3" || got["q2"] != "no" {
		t.Fatalf("parseHumanAnswers = %+v, want q1=v3 q2=no", got)
	}
	// An unmatched id is surfaced under its literal key, never dropped.
	got = parseHumanAnswers(questions, "q1: v3\nqX: stray")
	if got["q1"] != "v3" || got["qX"] != "stray" {
		t.Fatalf("parseHumanAnswers = %+v, want q1 + unmatched qX surfaced", got)
	}
	// Single-question convenience.
	got = parseHumanAnswers([]HumanQuestion{{ID: "only", Prompt: "x"}}, "just do v3")
	if got["only"] != "just do v3" {
		t.Fatalf("parseHumanAnswers single = %+v, want only=just do v3", got)
	}
}
