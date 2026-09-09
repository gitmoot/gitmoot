package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1713, through the PRODUCTION gate path rather than the collector helper,
// which the issue's acceptance requires explicitly: "Exercise reviewer
// eligibility through the production merge-gate path, not only the collector
// helper."
//
// The defect: attribution counted matching implement jobs without asking whether
// they ever ran, so a leg CANCELLED OR FAILED BEFORE EXECUTION permanently
// disqualified its assigned agent from reviewing that pull request. Measured in
// this host's store: 747 of 2504 implement jobs (29.8%) have no `running` event,
// 693 of those carry a pull request, and on gitmoot/gitmoot six PRs had their
// reviewer pool narrowed by a leg that never executed.
// seedNeverRanImplementLeg inserts an implement job for the same PR whose agent
// is the reviewer, in a terminal state, WITHOUT a running event. Deliberately
// local rather than reusing insertCompletedJob: that helper forces
// JobSucceeded, and the state is the variable under test here.
func seedNeverRanImplementLeg(t *testing.T, store *db.Store, id, agent string, state JobState, payload JobPayload) {
	t.Helper()
	seedMergeGateFixtureAgent(t, store, agent)
	implementPayload := payload
	implementPayload.ReviewRound = ""
	implementPayload.Result = nil
	encoded, err := marshalPayload(implementPayload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	job := db.Job{ID: id, Agent: agent, Type: "implement", State: string(state), Payload: encoded}
	// The seeded event is the TERMINAL one only. No `running` event is written,
	// which is the whole point: this is a leg that never reached execution.
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: string(state), Message: "seeded"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
}

// A leg that never reached execution must NOT disqualify its agent. Without the
// fix the gate refuses this merge as a self-review.
func TestGateAcceptsAReviewerWhoseImplementLegNeverRan(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("c", 40)
	payload := JobPayload{
		Repo: "mobile/app", Branch: "task-13", PullRequest: 13, HeadSHA: head,
		TaskID: "task-13",
	}
	reviewPayload := payload
	reviewPayload.ReviewRound = "review-1"
	reviewPayload.Result = &AgentResult{
		Decision: "approved",
		Summary:  "exact-head review",
		TestsRun: []string{"go test ./internal/ -> ok"},
	}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-13", Agent: "g6-review-sol", Type: "review"}, reviewPayload)

	// The poison: an implement leg assigned to the REVIEWER that was cancelled
	// before it ever executed.
	seedNeverRanImplementLeg(t, store, "implement-13-dead", "g6-review-sol", JobCancelled, payload)

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 13, TaskID: "task-13", Reviewer: "g6-review-sol",
	}, head)
	if err != nil {
		t.Fatalf("the gate disqualified a reviewer over an implement leg that never ran: %v", err)
	}
}

// A leg that DID reach execution must still disqualify its agent, however it
// ended. This is the arm that keeps the fix from becoming "attribute nothing",
// and it is the dangerous direction: an agent that implemented the change must
// never review it.
func TestGateStillDisqualifiesAReviewerWhoseImplementLegRan(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("d", 40)
	payload := JobPayload{
		Repo: "mobile/app", Branch: "task-14", PullRequest: 14, HeadSHA: head,
		TaskID: "task-14",
	}
	reviewPayload := payload
	reviewPayload.ReviewRound = "review-1"
	reviewPayload.Result = &AgentResult{
		Decision: "approved",
		Summary:  "exact-head review",
		TestsRun: []string{"go test ./internal/ -> ok"},
	}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-14", Agent: "g6-review-sol", Type: "review"}, reviewPayload)

	// Same shape as above, but this leg RAN and then failed. A failure after
	// execution can still have pushed a branch or edited the tree, so its agent
	// is an implementer.
	seedNeverRanImplementLeg(t, store, "implement-14-ran", "g6-review-sol", JobFailed, payload)
	if err := store.AddJobEventIfAbsent(ctx, db.JobEvent{JobID: "implement-14-ran", Kind: jobRunningEventKind, Message: "running"}); err != nil {
		t.Fatalf("seed running event: %v", err)
	}

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 14, TaskID: "task-14", Reviewer: "g6-review-sol",
	}, head)
	if err == nil {
		t.Fatal("the gate accepted a self-review: the reviewer's implement leg REACHED execution and must still disqualify it")
	}
	if !strings.Contains(err.Error(), "the implementing agent") {
		t.Fatalf("gate refused for the wrong reason: %v", err)
	}
}

// SUCCESS IMPLIES EXECUTION even with no event. If a succeeded implement leg's
// running event were ever absent - pruned, or written by an older build -
// excluding it would let the implementing agent review its own work, which is
// the one error this predicate must not make.
func TestASucceededImplementLegDisqualifiesEvenWithNoRunningEvent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("e", 40)
	payload := JobPayload{
		Repo: "mobile/app", Branch: "task-15", PullRequest: 15, HeadSHA: head,
		TaskID: "task-15",
	}
	reviewPayload := payload
	reviewPayload.ReviewRound = "review-1"
	reviewPayload.Result = &AgentResult{
		Decision: "approved",
		Summary:  "exact-head review",
		TestsRun: []string{"go test ./internal/ -> ok"},
	}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-15", Agent: "g6-review-sol", Type: "review"}, reviewPayload)

	// Succeeded, and deliberately NO running event.
	seedNeverRanImplementLeg(t, store, "implement-15-ok", "g6-review-sol", JobSucceeded, payload)

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 15, TaskID: "task-15", Reviewer: "g6-review-sol",
	}, head)
	if err == nil {
		t.Fatal("a SUCCEEDED implement leg with no running event failed to disqualify its agent")
	}
	// ASSERTING THE REASON, not merely that it refused. Without the succeeded
	// floor the gate still errors here - but for "no implementer at all", because
	// the fixture's other implement leg is also event-free - so an err != nil
	// assertion passes while the floor is gone. Measured: that is exactly what
	// happened when the floor was mutated out, and it killed a different test.
	if !strings.Contains(err.Error(), "the implementing agent") {
		t.Fatalf("gate refused for the wrong reason, so this does not pin the succeeded floor: %v", err)
	}
}
