package workflow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

// lockingEscalationNotifier counts NotifyEscalation calls from concurrent workers.
type lockingEscalationNotifier struct {
	calls atomic.Int64
}

func (n *lockingEscalationNotifier) NotifyEscalation(ctx context.Context, request EscalationRequest) error {
	n.calls.Add(1)
	return nil
}

// TestConcurrentSiblingEscalationsOpenExactlyOneRound is the concurrency test the
// previous suite could not express: its sibling-round tests were SEQUENTIAL, so both
// workers observed the round guard at different times and the defect they were meant
// to catch could not appear.
//
// Two settled sibling children are advanced IN PARALLEL through the production
// AdvanceJob entry. Both reach the coordinator's escalate_human policy, both see a
// closed round by any pre-check, and exactly one may open it.
//
// MUTATION PROOF: remove the requested<=resolved guard from the round-open INSERT's
// WHERE clause and two request events plus two notifications land.
func TestConcurrentSiblingEscalationsOpenExactlyOneRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	notifier := &lockingEscalationNotifier{}
	engine.EscalationNotifier = notifier
	sink := &recordingSink{}
	engine.EventSink = sink

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

	// BOTH legs fail before either is advanced, so both parallel passes observe an
	// all-terminal batch and both race to open the coordinator's human round.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, childID := range []string{"parent-job/delegation/api", "parent-job/delegation/ui"} {
		wg.Add(1)
		go func(idx int, id string) {
			defer wg.Done()
			errs[idx] = engine.AdvanceJob(ctx, id)
		}(i, childID)
	}
	wg.Wait()
	for i, err := range errs {
		// An escalate_human pause reports itself as a delegation-policy outcome; any
		// other error is a real fault.
		if err != nil && !isDelegationPolicyOutcome(err) {
			t.Fatalf("concurrent AdvanceJob[%d]: %v", i, err)
		}
	}

	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1: concurrent siblings opened duplicate rounds", escalationRequestedEvent, got)
	}
	if got := notifier.calls.Load(); got != 1 {
		t.Fatalf("escalation notifications = %d, want exactly 1", got)
	}
	if got := len(sink.byType(events.EventJobNeedsAttention)); got != 1 {
		t.Fatalf("job.needs_attention emissions = %d, want exactly 1", got)
	}
	if task, err := store.GetTask(ctx, "task-5"); err != nil {
		t.Fatalf("GetTask: %v", err)
	} else if task.State != string(TaskAwaitingHuman) {
		t.Fatalf("parent task state = %q, want awaiting_human: the winning opener did not pause", task.State)
	}
	// THE ASSERTION THIS SUITE WAS MISSING: task events. A loser classified as a
	// refusal writes a FALSE landed-work row here, and job-event/notification counts
	// cannot see it.
	taskEvents, err := store.ListTaskEvents(ctx, "task-5")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range taskEvents {
		if event.Kind == TaskEventMergedRegressionRefused {
			t.Fatalf("a concurrent opener loser wrote a false landed-work refusal: %+v", event)
		}
	}
}

// TestConcurrentSiblingAskGatesOpenExactlyOneRound is the same property for the ask
// gate, which opens rounds through the identical path. Without it the fix could be
// proven on one opener while its twin regressed.
func TestConcurrentSiblingAskGatesOpenExactlyOneRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	notifier := &lockingEscalationNotifier{}
	engine.EscalationNotifier = notifier

	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-ask", RepoFullName: "gitmoot/gitmoot", Branch: "task-ask", GoalID: "g1",
		Title: "ask", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "ask-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-ask", TaskID: "task-ask", LeadAgent: "coord",
		Result: &AgentResult{
			Decision:       "blocked",
			Summary:        "needs a human",
			HumanQuestions: []HumanQuestion{{ID: "q1", Prompt: "which branch?"}},
		},
	})
	job := mustJob(t, store, "ask-job")
	payload := mustPayload(t, store, "ask-job")
	ref := taskRef{ID: "task-ask", Repo: "gitmoot/gitmoot", Branch: "task-ask"}

	var wg sync.WaitGroup
	results := make([]bool, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = engine.pauseAwaitingHumanAnswer(ctx, job, payload, ref)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent pauseAwaitingHumanAnswer[%d]: %v", i, err)
		}
	}
	if got := countWorkflowJobEvents(t, store, "ask-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1: concurrent ask gates opened duplicate rounds", escalationRequestedEvent, got)
	}
	if got := notifier.calls.Load(); got != 1 {
		t.Fatalf("escalation notifications = %d, want exactly 1", got)
	}
}

// TestEscalationRefusedOnAMergedParentAnnouncesNothing is the P1-A regression through
// the production escalation path. A coordinator whose task already MERGED cannot move
// to awaiting_human, so the round must not exist at all: an event without its pause
// leaves requested > resolved forever and announces a pause that never happened.
//
// MUTATION PROOF: let the round-open commit its event when the task write is refused
// (drop the rollback) and this test sees one event and one notification.
func TestEscalationRefusedOnAMergedParentAnnouncesNothing(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &lockingEscalationNotifier{}
	engine.EscalationNotifier = notifier
	sink := &recordingSink{}
	engine.EventSink = sink
	child, _ := seedFailurePolicyTree(t, store, engine, "escalate_human")

	// The landed-work record: the parent's task merged while the leg was failing.
	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	task.State = string(TaskMerged)
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask(merged): %v", err)
	}

	err = engine.AdvanceJob(ctx, child)
	if err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("AdvanceJob: %v", err)
	}

	if got, gerr := store.GetTask(ctx, "task-7"); gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	} else if got.State != string(TaskMerged) {
		t.Fatalf("task state = %q, want merged: the landed-work record was overwritten", got.State)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0: a refused pause still opened a round", escalationRequestedEvent, got)
	}
	if got := notifier.calls.Load(); got != 0 {
		t.Fatalf("escalation notifications = %d, want 0: a refused pause called a human", got)
	}
	if got := len(sink.byType(events.EventJobNeedsAttention)); got != 0 {
		t.Fatalf("job.needs_attention emissions = %d, want 0", got)
	}
	// The refusal is durable: the landed-work trace explains why nothing moved.
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
	if refusals == 0 {
		t.Fatal("the refused pause left no durable trace")
	}
}

// TestFirstEscalationOnALiveParentStillOpens is the run that should SUCCEED: one
// legitimate first escalation must open exactly one round, pause the task, and notify
// once. Every guard added above has a version that would reject this.
func TestFirstEscalationOnALiveParentStillOpens(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &lockingEscalationNotifier{}
	engine.EscalationNotifier = notifier
	sink := &recordingSink{}
	engine.EventSink = sink
	child, _ := seedFailurePolicyTree(t, store, engine, "escalate_human")

	err := engine.AdvanceJob(ctx, child)
	var awaiting AwaitingHumanError
	if err != nil && !errors.As(err, &awaiting) && !isDelegationPolicyOutcome(err) {
		t.Fatalf("AdvanceJob: %v", err)
	}
	if task, gerr := store.GetTask(ctx, "task-7"); gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	} else if task.State != string(TaskAwaitingHuman) {
		t.Fatalf("parent task state = %q, want awaiting_human: a legitimate first escalation was refused", task.State)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", escalationRequestedEvent, got)
	}
	if got := notifier.calls.Load(); got != 1 {
		t.Fatalf("escalation notifications = %d, want exactly 1", got)
	}
	if got := len(sink.byType(events.EventJobNeedsAttention)); got != 1 {
		t.Fatalf("job.needs_attention emissions = %d, want exactly 1", got)
	}
}
