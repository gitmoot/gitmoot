package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// seedTaskReview persists a review row for task review-pr-227 with the given
// head, decision and session flag, and asserts the flag survived the round trip
// so a future projection change cannot make these tests quietly meaningless.
func seedTaskReview(t *testing.T, store *db.Store, jobID, headSHA, decision string, externallyDriven bool) db.Job {
	t.Helper()
	ctx := context.Background()
	encoded, err := marshalPayload(JobPayload{
		Repo: "owner/repo", Branch: "main", PullRequest: 227, TaskID: "review-pr-227",
		HeadSHA: headSHA, ReviewRound: "review-1",
		Result: &AgentResult{Decision: decision, Summary: "seeded"},
	})
	if err != nil {
		t.Fatalf("marshalPayload(%s): %v", jobID, err)
	}
	job := db.Job{ID: jobID, Agent: "audit", Type: "review", State: string(JobSucceeded), Payload: encoded}
	event := db.JobEvent{Kind: string(JobSucceeded), Message: decision}
	if externallyDriven {
		if err := store.CreateExternallyDrivenJobWithEvent(ctx, job, event); err != nil {
			t.Fatalf("CreateExternallyDrivenJobWithEvent(%s): %v", jobID, err)
		}
	} else if err := store.CreateJobWithEvent(ctx, job, event); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", jobID, err)
	}
	stored, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	if stored.ExternallyDriven != externallyDriven {
		t.Fatalf("stored ExternallyDriven = %v, want %v: the read path does not carry the column", stored.ExternallyDriven, externallyDriven)
	}
	return stored
}

// seedCurrentHead records the observed pull request row approvalSupersedesChangesRequested
// requires. Without it that function exits BEFORE its objection scan with "no
// observed pull request row records a current head", so a test seeded without it
// would exercise a precondition rather than the consumer under test.
func seedCurrentHead(t *testing.T, store *db.Store, headSHA string) {
	t.Helper()
	if err := store.UpsertPullRequest(context.Background(), db.PullRequest{
		RepoFullName: "owner/repo", Number: 227,
		URL:        "https://github.com/owner/repo/pull/227",
		HeadBranch: "main", BaseBranch: "main", HeadSHA: headSHA, State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
}

// TestApprovalScanRecordsWhyAHeadlessObjectionDidNotBlock covers
// approvalSupersedesChangesRequested. A headless objection can never equal the
// approving head, so it never keeps an approval from clearing. That is correct -
// it names no head to be current at - and it was silent.
func TestApprovalScanRecordsWhyAHeadlessObjectionDidNotBlock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedCurrentHead(t, store, "head-a")
	seedTaskReview(t, store, "session-objection", "", "changes_requested", true)

	approval := JobPayload{
		Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227",
		HeadSHA: "head-a", ReviewRound: "review-1",
		Result: &AgentResult{Decision: "approved", Summary: "approves head-a"},
	}
	cleared, reason, _, err := engine.approvalSupersedesChangesRequested(ctx, approval)
	if err != nil {
		t.Fatalf("approvalSupersedesChangesRequested: %v", err)
	}
	// BEHAVIOUR UNCHANGED: the headless objection does not stand.
	if !cleared {
		t.Fatalf("approval did not clear (%q): a headless objection must not block, only be visible", reason)
	}
	messages := headBoundExclusionEvents(t, store, "session-objection")
	if len(messages) != 1 {
		t.Fatalf("exclusion events = %d (%v), want 1", len(messages), messages)
	}
	if !strings.Contains(messages[0], "approvalSupersedesChangesRequested") {
		t.Errorf("message = %q, want it to name the consumer", messages[0])
	}
}

// TestApprovalScanStillLetsAHeadedObjectionBlock is the control. Without it the
// test above passes under a rule that stops objections blocking at all, which
// would be a merge-safety regression wearing a visibility fix's clothes.
func TestApprovalScanStillLetsAHeadedObjectionBlock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedCurrentHead(t, store, "head-a")
	seedTaskReview(t, store, "headed-objection", "head-a", "changes_requested", false)

	approval := JobPayload{
		Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227",
		HeadSHA: "head-a", ReviewRound: "review-1",
		Result: &AgentResult{Decision: "approved", Summary: "approves head-a"},
	}
	cleared, reason, _, err := engine.approvalSupersedesChangesRequested(ctx, approval)
	if err != nil {
		t.Fatalf("approvalSupersedesChangesRequested: %v", err)
	}
	if cleared {
		t.Fatal("an objection AT the approving head must still block")
	}
	if !strings.Contains(reason, "requested changes") {
		t.Errorf("reason = %q", reason)
	}
	if messages := headBoundExclusionEvents(t, store, "headed-objection"); len(messages) != 0 {
		t.Fatalf("exclusion events = %v, want none: this row has an engine-observed head", messages)
	}
}

// TestNextReviewRoundRecordsWhyAHeadlessRowIsNotTheOpenRound covers
// nextReviewRound. A headless row can never be the round already open at this
// head, so it does not prevent a new one.
func TestNextReviewRoundRecordsWhyAHeadlessRowIsNotTheOpenRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedTaskReview(t, store, "session-round", "", "changes_requested", true)
	event := PullRequestEvent{Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227", HeadSHA: "head-a"}

	round, _, err := engine.nextReviewRound(ctx, event)
	if err != nil {
		t.Fatalf("nextReviewRound: %v", err)
	}
	// BEHAVIOUR UNCHANGED: a headless row does not hold the head's round open, so
	// the next round is minted. Its ReviewRound still counts toward the counter,
	// which is round-keyed and out of scope here.
	if round != "review-2" {
		t.Fatalf("round = %q, want review-2: a headless row must not hold the head's round open", round)
	}
	messages := headBoundExclusionEvents(t, store, "session-round")
	if len(messages) != 1 {
		t.Fatalf("exclusion events = %d (%v), want 1", len(messages), messages)
	}
	if !strings.Contains(messages[0], "nextReviewRound") {
		t.Errorf("message = %q, want it to name the consumer", messages[0])
	}
}

// TestNextReviewRoundStillReusesTheRoundAtThisHead is the control: a row that
// DID record this head still holds its round open.
func TestNextReviewRoundStillReusesTheRoundAtThisHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedTaskReview(t, store, "headed-round", "head-a", "changes_requested", false)
	event := PullRequestEvent{Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227", HeadSHA: "head-a"}

	round, _, err := engine.nextReviewRound(ctx, event)
	if err != nil {
		t.Fatalf("nextReviewRound: %v", err)
	}
	if round != "review-1" {
		t.Fatalf("round = %q, want review-1: a row at this head holds its round open", round)
	}
	if messages := headBoundExclusionEvents(t, store, "headed-round"); len(messages) != 0 {
		t.Fatalf("exclusion events = %v, want none", messages)
	}
}

// TestEveryEngineConsumerRecordsUnderItsOwnName proves the record identifies
// WHICH consumer excluded the row. ClaimJobEvent keys on the exact
// (job_id, kind, message) triple, so two consumers sharing a name collapse into
// one event and the row loses that information.
func TestEveryEngineConsumerRecordsUnderItsOwnName(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedCurrentHead(t, store, "head-a")
	job := seedTaskReview(t, store, "session-shared", "", "changes_requested", true)
	event := PullRequestEvent{Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227", HeadSHA: "head-a"}
	approval := JobPayload{
		Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227",
		HeadSHA: "head-a", ReviewRound: "review-1",
		Result: &AgentResult{Decision: "approved", Summary: "approves head-a"},
	}

	for range 3 {
		if _, _, _, err := engine.approvalSupersedesChangesRequested(ctx, approval); err != nil {
			t.Fatalf("approvalSupersedesChangesRequested: %v", err)
		}
		if _, _, err := engine.nextReviewRound(ctx, event); err != nil {
			t.Fatalf("nextReviewRound: %v", err)
		}
	}
	_ = job

	messages := headBoundExclusionEvents(t, store, "session-shared")
	if len(messages) != 2 {
		t.Fatalf("exclusion events = %d (%v), want exactly 2: one per consumer, stable across repeats", len(messages), messages)
	}
	var sawApproval, sawRound bool
	for _, message := range messages {
		if strings.Contains(message, "approvalSupersedesChangesRequested") {
			sawApproval = true
		}
		if strings.Contains(message, "nextReviewRound") {
			sawRound = true
		}
	}
	if !sawApproval || !sawRound {
		t.Fatalf("messages = %v, want one from each consumer", messages)
	}
}

// TestFixDispatchScanRecordsWhyAHeadlessRowIsNotALeg covers
// dispatchFixWhenHeadHasSettled, the third consumer in this slice.
//
// The calling verdict carries a head and NO result, so the scan runs and the
// function returns at its `verdictPayload.Result == nil` arm without attempting
// a dispatch. That keeps the test on the consumer under test rather than on
// branch locks and lead-agent resolution.
func TestFixDispatchScanRecordsWhyAHeadlessRowIsNotALeg(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedTaskReview(t, store, "session-leg", "", "changes_requested", true)
	seedTaskReview(t, store, "other-head-leg", "head-b", "changes_requested", false)

	caller := seedTaskReview(t, store, "caller-review", "head-a", "approved", false)
	callerPayload := JobPayload{
		Repo: "owner/repo", PullRequest: 227, TaskID: "review-pr-227",
		HeadSHA: "head-a", ReviewRound: "review-1",
	}

	if err := engine.dispatchFixWhenHeadHasSettled(ctx, caller, callerPayload, taskRef{}); err != nil {
		t.Fatalf("dispatchFixWhenHeadHasSettled: %v", err)
	}

	messages := headBoundExclusionEvents(t, store, "session-leg")
	if len(messages) != 1 {
		t.Fatalf("exclusion events = %d (%v), want 1", len(messages), messages)
	}
	if !strings.Contains(messages[0], "dispatchFixWhenHeadHasSettled") {
		t.Errorf("message = %q, want it to name the consumer", messages[0])
	}
	// A row at ANOTHER head has an engine-observed head and is not this class.
	if other := headBoundExclusionEvents(t, store, "other-head-leg"); len(other) != 0 {
		t.Fatalf("exclusion events for a headed row = %v, want none", other)
	}
}
