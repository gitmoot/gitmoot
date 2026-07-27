package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestEngineReviewChangesRequestedStopsAtAutomaticFixRoundCap(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	for _, round := range []string{"review-1", "review-2"} {
		insertReviewChangesRequested(t, store, round, round)
		if err := engine.AdvanceJob(ctx, round); err != nil {
			t.Fatalf("%s AdvanceJob returned error: %v", round, err)
		}
	}
	if got := automaticFixJobCount(t, store, "task-7"); got != 2 {
		t.Fatalf("automatic fix jobs below cap = %d, want 2", got)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-round-3", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		ReviewRound: "review-3",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "another fix"},
	})

	err := engine.AdvanceJob(ctx, "review-round-3")

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "after 2 automatic fix dispatches") || !strings.Contains(blocked.Reason, "maximum automatic fix rounds is 3") {
		t.Fatalf("AdvanceJob error = %v, want round-cap BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
	if got := automaticFixJobCount(t, store, "task-7"); got != 2 {
		t.Fatalf("automatic fix jobs after cap = %d, want unchanged count 2", got)
	}
	events, eventErr := store.ListTaskEvents(ctx, "task-7")
	if eventErr != nil {
		t.Fatal(eventErr)
	}
	if len(events) != 3 ||
		events[0].Kind != AutomaticFixDispatchedEventKind ||
		events[1].Kind != AutomaticFixDispatchedEventKind ||
		events[2].Kind != AdvancementStoppedRoundCapKind ||
		!strings.Contains(events[2].Reason, "after 2 automatic fix dispatches") {
		t.Fatalf("task events = %+v", events)
	}

	insertReviewChangesRequested(t, store, "delayed-review-2", "review-2")
	if err := engine.AdvanceJob(ctx, "delayed-review-2"); err != nil {
		t.Fatalf("stale lower-round AdvanceJob error = %v, want stale-review no-op", err)
	}
	if err := engine.AdvanceJob(ctx, "review-round-3"); !errors.As(err, &blocked) {
		t.Fatalf("replayed capped AdvanceJob error = %v, want cap BlockedError", err)
	}
	insertReviewChangesRequested(t, store, "review-round-4", "review-4")
	if err := engine.AdvanceJob(ctx, "review-round-4"); !errors.As(err, &blocked) {
		t.Fatalf("fourth-round AdvanceJob error = %v, want cap BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
	if got := automaticFixJobCount(t, store, "task-7"); got != 2 {
		t.Fatalf("automatic fix jobs after delayed/replayed reviews = %d, want 2", got)
	}
	events, eventErr = store.ListTaskEvents(ctx, "task-7")
	if eventErr != nil || len(events) != 3 {
		t.Fatalf("cap events after delayed/replayed reviews = %+v err=%v", events, eventErr)
	}
}

func TestEngineReviewChangesRequestedEmptyReviewRoundCannotBypassAutomaticFixCap(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.JobID = nil
	summaries := []string{
		"fix first empty-round finding",
		"fix second empty-round finding",
		"fix third empty-round finding",
		"fix fourth empty-round finding",
	}
	var blocked BlockedError
	for i, summary := range summaries {
		jobID := fmt.Sprintf("empty-round-review-%d", i+1)
		insertReviewChangesRequestedWithSummary(t, store, jobID, "", summary)
		err := engine.AdvanceJob(ctx, jobID)
		if i < 2 {
			if err != nil {
				t.Fatalf("%s AdvanceJob returned error: %v", jobID, err)
			}
			continue
		}
		if !errors.As(err, &blocked) {
			t.Fatalf("%s AdvanceJob error = %v, want cap BlockedError", jobID, err)
		}
	}

	assertTaskState(t, store, "task-7", TaskBlocked)
	if got := automaticFixJobCount(t, store, "task-7"); got != 2 {
		t.Fatalf("automatic fix jobs = %d, want exactly 2", got)
	}
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 ||
		events[0].Kind != AutomaticFixDispatchedEventKind ||
		events[1].Kind != AutomaticFixDispatchedEventKind ||
		events[2].Kind != AdvancementStoppedRoundCapKind {
		t.Fatalf("task events = %+v", events)
	}
	if !strings.Contains(events[2].Reason, "after 2 automatic fix dispatches") {
		t.Fatalf("cap reason = %q", events[2].Reason)
	}
}

func TestEngineReviewChangesRequestedIdempotentRetryDoesNotDoubleCountAutomaticFix(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.JobID = nil
	insertReviewChangesRequestedWithSummary(t, store, "review-job", "", "fix once")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("first AdvanceJob returned error: %v", err)
	}
	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("idempotent AdvanceJob returned error: %v", err)
	}
	if got := automaticFixJobCount(t, store, "task-7"); got != 1 {
		t.Fatalf("automatic fix jobs = %d, want 1", got)
	}
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != AutomaticFixDispatchedEventKind {
		t.Fatalf("task events = %+v", events)
	}
}

func TestEngineReviewChangesRequestedHoldAndUnholdRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", State: string(TaskReviewing), Branch: "task-7",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID: "task-7", Kind: TaskHoldSetManualEventKind, Reason: "coordinator inspection",
	}); err != nil {
		t.Fatal(err)
	}
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "held-review", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	if err := engine.AdvanceJob(ctx, "held-review"); err != nil {
		t.Fatalf("held AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	if automaticFixJobCount(t, store, "task-7") != 0 {
		t.Fatal("automatic fix was dispatched while held")
	}
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Kind != AdvancementSkippedHeldEventKind {
		t.Fatalf("held task events = %+v", events)
	}

	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID: "task-7", Kind: TaskHoldClearedManualEventKind, Reason: "inspection complete",
	}); err != nil {
		t.Fatal(err)
	}
	insertCompletedJob(t, store, db.Job{ID: "unheld-review", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})
	if err := engine.AdvanceJob(ctx, "unheld-review"); err != nil {
		t.Fatalf("unheld AdvanceJob returned error: %v", err)
	}
	if automaticFixJobCount(t, store, "task-7") != 1 {
		t.Fatal("automatic fix was not dispatched after unhold")
	}
}

func TestEngineReviewChangesRequestedHoldQueryFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.TaskHoldStatus = func(context.Context, string) (bool, error) {
		return false, errors.New("hold query unavailable")
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err == nil || !strings.Contains(err.Error(), "hold query unavailable") {
		t.Fatalf("AdvanceJob error = %v, want propagated hold query error", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	if automaticFixJobCount(t, store, "task-7") != 0 {
		t.Fatal("automatic fix was dispatched after an indeterminate hold query")
	}
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != AdvancementSkippedHeldEventKind || !strings.Contains(events[0].Reason, "could not be determined") {
		t.Fatalf("task events = %+v", events)
	}
}

func TestEngineReviewChangesRequestedUnheldBelowCapStillDispatchesFix(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	if automaticFixJobCount(t, store, "task-7") != 1 {
		t.Fatal("automatic fix was not dispatched below the cap")
	}
}

func TestEngineReviewChangesRequestedHoldWinsAtRoundCap(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", State: string(TaskReviewing), Branch: "task-7",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{TaskID: "task-7", Kind: TaskHoldSetManualEventKind}); err != nil {
		t.Fatal(err)
	}
	engine := testEngine(store)
	insertReviewChangesRequested(t, store, "held-cap-review", "review-3")

	if err := engine.AdvanceJob(ctx, "held-cap-review"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil || len(events) != 2 || events[1].Kind != AdvancementSkippedHeldEventKind {
		t.Fatalf("events = %+v err=%v", events, err)
	}
}

func TestEngineReviewChangesRequestedUsesCanonicalBranchTaskControls(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "canonical-task", RepoFullName: "gitmoot/gitmoot", State: string(TaskReviewing), Branch: "shared-branch",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID: "canonical-task", Kind: TaskHoldSetManualEventKind, Reason: "hold canonical task",
	}); err != nil {
		t.Fatal(err)
	}
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "stale-task-review", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "shared-branch",
		PullRequest: 7,
		TaskID:      "stale-payload-task",
		LeadAgent:   "lead",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	if err := engine.AdvanceJob(ctx, "stale-task-review"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "canonical-task", TaskChangesRequested)
	if _, err := store.GetTask(ctx, "stale-payload-task"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale payload task was created: %v", err)
	}
	events, err := store.ListTaskEvents(ctx, "canonical-task")
	if err != nil || len(events) != 2 || events[1].Kind != AdvancementSkippedHeldEventKind {
		t.Fatalf("canonical task events = %+v err=%v", events, err)
	}
}

func TestEngineReviewChangesRequestedHoldRaceIsSerializedWithEnqueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	reached := make(chan struct{})
	release := make(chan struct{})
	engine.BeforeAutomaticFixEnqueue = func() {
		close(reached)
		<-release
	}
	insertReviewChangesRequested(t, store, "review-job", "review-1")
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.AdvanceJob(ctx, "review-job")
	}()
	<-reached
	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID: "task-7", Kind: TaskHoldSetManualEventKind, Reason: "racing coordinator hold",
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if got := automaticFixJobCount(t, store, "task-7"); got != 0 {
		t.Fatalf("automatic fix jobs = %d, want 0", got)
	}
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil || len(events) != 2 || events[1].Kind != AdvancementSkippedHeldEventKind {
		t.Fatalf("events = %+v err=%v", events, err)
	}
}

func TestEngineReviewChangesRequestedCapCountIsSerializedWithEnqueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.JobID = nil
	insertReviewChangesRequestedWithSummary(t, store, "first-review", "", "first fix")
	if err := engine.AdvanceJob(ctx, "first-review"); err != nil {
		t.Fatalf("first AdvanceJob returned error: %v", err)
	}

	insertReviewChangesRequestedWithSummary(t, store, "racing-review-a", "", "racing fix a")
	insertReviewChangesRequestedWithSummary(t, store, "racing-review-b", "", "racing fix b")
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	engine.BeforeAutomaticFixEnqueue = func() {
		arrived <- struct{}{}
		<-release
	}
	errs := make(chan error, 2)
	go func() { errs <- engine.AdvanceJob(ctx, "racing-review-a") }()
	go func() { errs <- engine.AdvanceJob(ctx, "racing-review-b") }()
	<-arrived
	<-arrived
	close(release)

	var succeeded, capped int
	for range 2 {
		err := <-errs
		var blocked BlockedError
		switch {
		case err == nil:
			succeeded++
		case errors.As(err, &blocked):
			capped++
		default:
			t.Fatalf("racing AdvanceJob returned unexpected error: %v", err)
		}
	}
	if succeeded != 1 || capped != 1 {
		t.Fatalf("racing results: succeeded=%d capped=%d, want 1 each", succeeded, capped)
	}
	if got := automaticFixJobCount(t, store, "task-7"); got != 2 {
		t.Fatalf("automatic fix jobs = %d, want exactly 2", got)
	}
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil {
		t.Fatal(err)
	}
	dispatches, capEvents := 0, 0
	for _, event := range events {
		switch event.Kind {
		case AutomaticFixDispatchedEventKind:
			dispatches++
		case AdvancementStoppedRoundCapKind:
			capEvents++
		}
	}
	if dispatches != 2 || capEvents != 1 {
		t.Fatalf("task events = %+v; dispatches=%d cap_events=%d", events, dispatches, capEvents)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineTaskHoldDoesNotAffectApprovedReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", State: string(TaskReviewing), Branch: "task-7",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{TaskID: "task-7", Kind: TaskHoldSetManualEventKind}); err != nil {
		t.Fatal(err)
	}
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	insertCompletedJob(t, store, db.Job{ID: "approved-review", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})

	if err := engine.AdvanceJob(ctx, "approved-review"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func automaticFixJobCount(t *testing.T, store *db.Store, taskID string) int {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, job := range jobs {
		if job.Type != "implement" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			t.Fatalf("decode implement job %s: %v", job.ID, err)
		}
		if payload.TaskID == taskID {
			count++
		}
	}
	return count
}

func insertReviewChangesRequested(t *testing.T, store *db.Store, jobID, round string) {
	t.Helper()
	insertReviewChangesRequestedWithSummary(t, store, jobID, round, "another fix")
}

func insertReviewChangesRequestedWithSummary(t *testing.T, store *db.Store, jobID, round, summary string) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: jobID, Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		ReviewRound: round,
		Result:      &AgentResult{Decision: "changes_requested", Summary: summary},
	})
}
