package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// legsAtHeadForTest keeps the existing reviewLegsAtHead call sites readable now
// that it is an Engine method.
//
// Those call sites pass UNPERSISTED fixture rows, which would be unlike
// production if the recorder fired for them - job_events carries no foreign key,
// so an orphan write would succeed silently rather than failing loudly. It never
// fires: every one of those fixtures records a head, and HeadBoundExclusion
// refuses a non-empty head. That is ASSERTED here rather than reasoned about,
// so a future fixture that drops its head cannot start writing orphan events
// unnoticed.
func legsAtHeadForTest(t *testing.T, jobs []db.Job, event PullRequestEvent, round string) map[string]string {
	t.Helper()
	store := openEngineStore(t)
	engine := testEngine(store)
	legs, err := engine.reviewLegsAtHead(context.Background(), jobs, event, round)
	if err != nil {
		t.Fatalf("reviewLegsAtHead: %v", err)
	}
	for _, job := range jobs {
		if events := headBoundExclusionEvents(t, store, job.ID); len(events) != 0 {
			t.Fatalf("job %s: recorded %v against an unpersisted fixture row; persist the row or give it a head", job.ID, events)
		}
	}
	return legs
}

func seedHeadlessReviewJob(t *testing.T, store *db.Store, jobID string, externallyDriven bool, round string) db.Job {
	t.Helper()
	encoded, err := marshalPayload(JobPayload{
		Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227",
		HeadSHA: "", ReviewRound: round,
		Result: &AgentResult{Decision: "changes_requested", Summary: "headless"},
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	job := db.Job{ID: jobID, Agent: "audit", Type: "review", State: string(JobSucceeded), Payload: encoded}
	event := db.JobEvent{Kind: string(JobSucceeded), Message: "changes_requested"}
	if externallyDriven {
		if err := store.CreateExternallyDrivenJobWithEvent(context.Background(), job, event); err != nil {
			t.Fatalf("CreateExternallyDrivenJobWithEvent: %v", err)
		}
	} else if err := store.CreateJobWithEvent(context.Background(), job, event); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	stored, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	// The predicate reads ExternallyDriven off the row, so a read path that did
	// not select the column would silently give every session row the weaker
	// reason. ListJobs uses jobColumns, which includes it; asserted here so a
	// future projection change cannot make this test quietly meaningless.
	if stored.ExternallyDriven != externallyDriven {
		t.Fatalf("stored ExternallyDriven = %v, want %v: the read path does not carry the column", stored.ExternallyDriven, externallyDriven)
	}
	return stored
}

// TestReviewLegsAtHeadRecordsWhyItDroppedAHeadlessRow: behaviour unchanged, the
// silence removed.
func TestReviewLegsAtHeadRecordsWhyItDroppedAHeadlessRow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	job := seedHeadlessReviewJob(t, store, "session-review-1", true, "review-1")
	event := PullRequestEvent{Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227", HeadSHA: "head-a"}

	legs, err := engine.reviewLegsAtHead(ctx, []db.Job{job}, event, "review-1")
	if err != nil {
		t.Fatalf("reviewLegsAtHead: %v", err)
	}
	if len(legs) != 0 {
		t.Fatalf("legs = %v, want none: a headless row must not be seen as a leg at head-a", legs)
	}
	messages := headBoundExclusionEvents(t, store, "session-review-1")
	if len(messages) != 1 {
		t.Fatalf("exclusion events = %d (%v), want 1", len(messages), messages)
	}
	if !strings.Contains(messages[0], "review_loop.reviewLegsAtHead") {
		t.Errorf("message = %q, want it to name the consumer", messages[0])
	}
	if !strings.Contains(messages[0], HeadBoundExclusionSessionRow) {
		t.Errorf("message = %q, want the session reason", messages[0])
	}
}

// TestReviewLegsAtHeadStaysSilentForARowAtAnotherHead is the arm that keeps the
// record meaningful. A row WITH an engine-observed head that simply is not this
// one was never the #2008 class, and recording it would bury the 41 rows that
// are among thousands that are not.
func TestReviewLegsAtHeadStaysSilentForARowAtAnotherHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	encoded, err := marshalPayload(JobPayload{
		Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227",
		HeadSHA: "head-b", ReviewRound: "review-1",
		Result: &AgentResult{Decision: "approved", Summary: "other head"},
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	job := db.Job{ID: "other-head-review", Agent: "audit", Type: "review", State: string(JobSucceeded), Payload: encoded}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{Kind: string(JobSucceeded), Message: "approved"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	stored, err := store.GetJob(ctx, "other-head-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	event := PullRequestEvent{Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227", HeadSHA: "head-a"}

	if _, err := engine.reviewLegsAtHead(ctx, []db.Job{stored}, event, "review-1"); err != nil {
		t.Fatalf("reviewLegsAtHead: %v", err)
	}
	if messages := headBoundExclusionEvents(t, store, "other-head-review"); len(messages) != 0 {
		t.Fatalf("exclusion events = %v, want none: this row has an engine-observed head and is simply not this one", messages)
	}
}

// TestFollowUpReviewScopesRecordsWhyItDroppedAHeadlessRow is the second consumer
// in this slice. Its drop is the `previousHead == ""` arm.
func TestFollowUpReviewScopesRecordsWhyItDroppedAHeadlessRow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	job := seedHeadlessReviewJob(t, store, "session-review-2", true, "review-1")
	event := PullRequestEvent{Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227", HeadSHA: "head-a"}

	scopes, err := engine.followUpReviewScopes(ctx, event, []string{"audit"}, []db.Job{job})
	if err != nil {
		t.Fatalf("followUpReviewScopes: %v", err)
	}
	if len(scopes) != 0 {
		t.Fatalf("scopes = %v, want none: a headless row must not scope a follow-up round", scopes)
	}
	messages := headBoundExclusionEvents(t, store, "session-review-2")
	if len(messages) != 1 {
		t.Fatalf("exclusion events = %d (%v), want 1", len(messages), messages)
	}
	if !strings.Contains(messages[0], "review_loop.followUpReviewScopes") {
		t.Errorf("message = %q, want it to name the consumer", messages[0])
	}
}

// TestFollowUpReviewScopesStaysSilentForTheCurrentHead is the same discipline on
// the other consumer: a row AT the evaluated head is skipped because it is not a
// previous round, not for want of a head.
func TestFollowUpReviewScopesStaysSilentForTheCurrentHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	encoded, err := marshalPayload(JobPayload{
		Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227",
		HeadSHA: "head-a", ReviewRound: "review-1",
		Result: &AgentResult{Decision: "approved", Summary: "current head"},
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	job := db.Job{ID: "current-head-review", Agent: "audit", Type: "review", State: string(JobSucceeded), Payload: encoded}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{Kind: string(JobSucceeded), Message: "approved"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	stored, err := store.GetJob(ctx, "current-head-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	event := PullRequestEvent{Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227", HeadSHA: "head-a"}

	if _, err := engine.followUpReviewScopes(ctx, event, []string{"audit"}, []db.Job{stored}); err != nil {
		t.Fatalf("followUpReviewScopes: %v", err)
	}
	if messages := headBoundExclusionEvents(t, store, "current-head-review"); len(messages) != 0 {
		t.Fatalf("exclusion events = %v, want none", messages)
	}
}

// TestBothReviewLoopConsumersRecordSeparately proves the record identifies WHICH
// consumer excluded the row rather than merely that something did. Two consumers
// dropping one row must leave two distinguishable records, and the at-most-once
// key must not collapse them into one.
func TestBothReviewLoopConsumersRecordSeparately(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	job := seedHeadlessReviewJob(t, store, "session-review-3", true, "review-1")
	event := PullRequestEvent{Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227", HeadSHA: "head-a"}

	for range 3 {
		if _, err := engine.reviewLegsAtHead(ctx, []db.Job{job}, event, "review-1"); err != nil {
			t.Fatalf("reviewLegsAtHead: %v", err)
		}
		if _, err := engine.followUpReviewScopes(ctx, event, []string{"audit"}, []db.Job{job}); err != nil {
			t.Fatalf("followUpReviewScopes: %v", err)
		}
	}

	messages := headBoundExclusionEvents(t, store, "session-review-3")
	if len(messages) != 2 {
		t.Fatalf("exclusion events = %d (%v), want exactly 2: one per consumer, stable across repeats", len(messages), messages)
	}
	var sawLegs, sawScopes bool
	for _, message := range messages {
		if strings.Contains(message, "review_loop.reviewLegsAtHead") {
			sawLegs = true
		}
		if strings.Contains(message, "review_loop.followUpReviewScopes") {
			sawScopes = true
		}
	}
	if !sawLegs || !sawScopes {
		t.Fatalf("messages = %v, want one from each consumer", messages)
	}
}
