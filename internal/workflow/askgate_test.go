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

// seedAskWithDelegationsCoordinator inserts a coordinator whose HEALTHY result
// carries BOTH human_questions[] AND delegations[] (the goal explicitly endorses a
// coordinator that fans out AND asks). synthesisRule is applied to each delegation
// (e.g. "vote"/"quorum"). The ask-gate short-circuits AdvanceJob BEFORE
// dispatchDelegations, so these delegations are NEVER dispatched and no children
// exist for them.
func seedAskWithDelegationsCoordinator(t *testing.T, store *db.Store, synthesisRule string) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision:       "approved",
			Summary:        "fan out, but I need a decision first",
			HumanQuestions: []HumanQuestion{{ID: "q1", Prompt: "Target v2 or v3 API?", Choices: []string{"v2", "v3"}}},
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", SynthesisRule: synthesisRule},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", SynthesisRule: synthesisRule},
			},
		},
	})
}

// TestAskGateWithVoteDelegationsAnswerEnqueuesContinuation is the regression for
// the HIGH finding: a result carrying BOTH human_questions[] AND
// synthesis_rule:vote delegations[] must NOT deadlock on `answer`. Because the
// ask-gate short-circuits dispatch, the delegations never ran; the answer-driven
// continuation must not run the vote synthesis gate against an empty children map
// (which would block the parent and silently lose the answer).
func TestAskGateWithVoteDelegationsAnswerEnqueuesContinuation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedAskWithDelegationsCoordinator(t, store, "vote")

	// AdvanceJob pauses at the ask-gate BEFORE any delegation child is created.
	err := engine.AdvanceJob(ctx, "parent-job")
	var awaiting AwaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("AdvanceJob(parent) error = %v, want AwaitingHumanError", err)
	}
	if jobExists(t, store, "parent-job/delegation/api") || jobExists(t, store, "parent-job/delegation/ui") {
		t.Fatal("ask-gate must short-circuit BEFORE dispatching delegations")
	}
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)

	// The human answers. The vote/quorum synthesis gate must NOT fire (the
	// delegations were never dispatched), so this enqueues the coordinator
	// continuation and clears the pause rather than blocking.
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAnswer, "q1: v3"); err != nil {
		t.Fatalf("ResolveEscalation(answer) returned error = %v, want nil (must not block on un-dispatched vote delegations)", err)
	}

	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("answer must enqueue the coordinator continuation even when un-dispatched delegations declared vote")
	}
	// The pause is cleared: the task left awaiting_human (planned), not blocked.
	task, err := store.GetTask(ctx, "task-5")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State == string(TaskAwaitingHuman) || task.State == string(TaskBlocked) {
		t.Fatalf("task state = %q, want it to leave awaiting_human/blocked after the answer", task.State)
	}
	// The continuation carries the human's answer and does NOT render the
	// un-dispatched delegations as "not enqueued (dependencies unmet)".
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !strings.Contains(cont.HumanAnswer, "v3") {
		t.Fatalf("continuation HumanAnswer = %q, want it to carry v3", cont.HumanAnswer)
	}
	if strings.Contains(cont.Instructions, "not enqueued (dependencies unmet)") {
		t.Fatalf("continuation prompt must not present un-dispatched ask delegations as deps-unmet:\n%s", cont.Instructions)
	}
}

// TestAskGateWithQuorumDelegationsAnswerEnqueuesContinuation mirrors the vote
// regression for synthesis_rule:quorum, which deadlocked identically.
func TestAskGateWithQuorumDelegationsAnswerEnqueuesContinuation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}

	seedAskWithDelegationsCoordinator(t, store, "quorum")

	if err := engine.AdvanceJob(ctx, "parent-job"); err == nil {
		t.Fatal("expected AwaitingHumanError")
	}
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAnswer, "q1: v3"); err != nil {
		t.Fatalf("ResolveEscalation(answer) returned error = %v, want nil (must not block on un-dispatched quorum delegations)", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("answer must enqueue the coordinator continuation even when un-dispatched delegations declared quorum")
	}
	task, err := store.GetTask(ctx, "task-5")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State == string(TaskAwaitingHuman) || task.State == string(TaskBlocked) {
		t.Fatalf("task state = %q, want it to leave awaiting_human/blocked after the answer", task.State)
	}
}

// seedCoordinatorWithAskingChildren inserts a coordinator with `n` independent
// review delegations and dispatches them via AdvanceJob, returning their child job
// ids. The children share one parent task (task-5) and one coordinator (parent-job).
func seedCoordinatorWithAskingChildren(t *testing.T, store *db.Store, engine Engine, ids ...string) {
	t.Helper()
	ctx := context.Background()
	dels := make([]Delegation, 0, len(ids))
	for _, id := range ids {
		seedAgent(t, store, id, []string{"review"}, "jerryfane/gitmoot")
		dels = append(dels, Delegation{ID: id, Agent: id, Action: "review", Prompt: "review " + id})
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision:    "approved",
			Summary:     "fan out",
			Delegations: dels,
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
}

// TestAskGateChildRoutesPauseToCoordinator is the regression for the MEDIUM
// finding at line 705: a healthy delegation CHILD that returns human_questions[]
// must (a) record the ask round on the COORDINATOR (not the child) so the human
// resumes the coordinator whose continuation carries the answer, and (b) NOT let
// the parent DAG advance enqueue the coordinator continuation while the round is
// open (zero compute, no contradiction).
func TestAskGateChildRoutesPauseToCoordinator(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	notifier := &recordingNotifier{}
	engine := testEngine(store)
	engine.EscalationNotifier = notifier

	seedCoordinatorWithAskingChildren(t, store, engine, "api")

	// The only child finishes HEALTHY but carrying a question.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{
		Decision:       "approved",
		Summary:        "need a call",
		HumanQuestions: []HumanQuestion{{ID: "q1", Prompt: "v2 or v3?"}},
	})
	err := engine.AdvanceJob(ctx, "parent-job/delegation/api")
	var awaiting AwaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("AdvanceJob(api child) error = %v, want AwaitingHumanError", err)
	}

	// The shared parent task is paused.
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)

	// The round is recorded on the COORDINATOR, not the child.
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("coordinator %s events = %d, want 1 (the child's ask routes to the coordinator)", escalationRequestedEvent, got)
	}
	if got := countJobEvents(t, store, "parent-job/delegation/api", escalationRequestedEvent); got != 0 {
		t.Fatalf("child %s events = %d, want 0 (the round is the coordinator's)", escalationRequestedEvent, got)
	}

	// The coordinator continuation must NOT be enqueued while the ask round is open:
	// the tree must not proceed past the question even though the asking child was
	// the last/only sibling to finish.
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("the coordinator continuation must NOT be enqueued while a child's ask round is open")
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 0 {
		t.Fatalf("delegation_continuation_enqueued events = %d, want 0 while the ask round is open", got)
	}

	// The resume target the human is told to use is the COORDINATOR.
	if len(notifier.calls) != 1 || notifier.calls[0].CoordinatorJobID != "parent-job" {
		t.Fatalf("notifier calls = %+v, want one targeting CoordinatorJobID=parent-job", notifier.calls)
	}

	// Answering the COORDINATOR enqueues its single continuation and clears the pause.
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeAnswer, "q1: v3"); err != nil {
		t.Fatalf("ResolveEscalation(coordinator answer) returned error: %v", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("answering the coordinator must enqueue its continuation")
	}
}

// TestAskGateSiblingsShareOneRoundOnCoordinator is the regression for the MEDIUM
// finding at line 4126: two sibling delegation children that BOTH return
// human_questions[] must open exactly ONE shared round on the coordinator — the
// first asking sibling opens it, the second hits the open-round guard — not two
// independent rounds that ping-pong the shared task.
func TestAskGateSiblingsShareOneRoundOnCoordinator(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	notifier := &recordingNotifier{}
	engine := testEngine(store)
	engine.EscalationNotifier = notifier

	seedCoordinatorWithAskingChildren(t, store, engine, "a", "b")

	// Both siblings finish HEALTHY, each carrying its own question.
	completeDelegationChild(t, store, "parent-job/delegation/a", JobSucceeded, AgentResult{
		Decision:       "approved",
		Summary:        "a asks",
		HumanQuestions: []HumanQuestion{{ID: "qa", Prompt: "a: v2 or v3?"}},
	})
	completeDelegationChild(t, store, "parent-job/delegation/b", JobSucceeded, AgentResult{
		Decision:       "approved",
		Summary:        "b asks",
		HumanQuestions: []HumanQuestion{{ID: "qb", Prompt: "b: x or y?"}},
	})

	// First sibling opens the single round on the coordinator.
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/a"); err == nil {
		t.Fatal("AdvanceJob(a) expected AwaitingHumanError")
	}
	// Second sibling must hit the open-round guard, NOT open a second round.
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/b"); err == nil {
		t.Fatal("AdvanceJob(b) expected AwaitingHumanError (open-round guard)")
	}

	// Exactly ONE escalation round on the coordinator, shared by both siblings.
	if got := countJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("coordinator %s events = %d, want 1 (single shared round across siblings)", escalationRequestedEvent, got)
	}
	// Neither child records its own round.
	if got := countJobEvents(t, store, "parent-job/delegation/a", escalationRequestedEvent); got != 0 {
		t.Fatalf("child a %s events = %d, want 0", escalationRequestedEvent, got)
	}
	if got := countJobEvents(t, store, "parent-job/delegation/b", escalationRequestedEvent); got != 0 {
		t.Fatalf("child b %s events = %d, want 0", escalationRequestedEvent, got)
	}
	// The human is notified once (one logical pause), not twice.
	if len(notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1 (single round = single @-tag)", len(notifier.calls))
	}
	assertTaskState(t, store, "task-5", TaskAwaitingHuman)

	// No continuation while the shared round is open.
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("no coordinator continuation may be enqueued while the shared ask round is open")
	}
}
