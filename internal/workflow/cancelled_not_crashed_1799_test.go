package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1799, directive 108042. The gate labelled an explicitly cancelled review job
// a "crashed reviewer at evaluated head" and escalated for a requeue or merge
// bridge. A cancellation is neither a crash nor reviewer evidence, and the wrong
// label sends an operator to the wrong remedy.
//
// Measured in this store before the fix: 534 cancelled review jobs would have
// been reported as crashed reviewers, 346 of them carrying the discriminator in
// their own events - "cancel requested from queued" 196, "from running" 92,
// "from blocked" 59. Both instances the directive names reproduce:
// local-review-g7-review-18d189d4598f1a86 (cancel requested from queued) and
// local-review-gm-review-opus-18d18c6aa78d3aec (cancel requested from running,
// then cancel_settled).

func seed1799Review(t *testing.T, store *db.Store, id, agent string, state JobState, payload JobPayload) {
	t.Helper()
	seedMergeGateFixtureAgent(t, store, agent)
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	job := db.Job{ID: id, Agent: agent, Type: "review", State: string(state), Payload: encoded}
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: string(state), Message: "seeded"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
}

// A CANCELLED slot must be diagnosed as cancelled, and must still refuse.
// Both halves matter: the label is the defect, and the refusal is the behaviour
// that must not change - a cancellation is not a verdict either.
func TestGateCallsACancelledReviewSlotCancelledNotCrashed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("7", 40)
	payload := JobPayload{
		Repo: "mobile/app", Branch: "task-1799", PullRequest: 1799, HeadSHA: head,
		TaskID: "task-1799", ReviewRound: "review-1",
	}
	implPayload := payload
	implPayload.ReviewRound = ""
	implPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	seedMergeGateFixtureAgent(t, store, "implementer-1799")
	insertCompletedJob(t, store, db.Job{ID: "implement-1799", Agent: "implementer-1799", Type: "implement"}, implPayload)
	seed1799Review(t, store, "review-1799-cancelled", "g6-review-sol", JobCancelled, payload)

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 1799, TaskID: "task-1799", Reviewer: "g6-review-sol",
	}, head)
	if err == nil {
		t.Fatal("a cancelled review slot at the evaluated head did not refuse; a cancellation is not a verdict")
	}
	if strings.Contains(err.Error(), "crashed reviewer") {
		t.Fatalf("an explicitly cancelled slot is still reported as a crashed reviewer: %v", err)
	}
	for _, want := range []string{"cancelled", "not crashed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name the cancellation (%q): %v", want, err)
		}
	}
}

// The other arm: a genuinely FAILED reviewer must still be reported as crashed.
// Without this the fix could be over-applied into calling every terminal
// non-success a cancellation, which loses the distinction in the other
// direction.
func TestGateStillCallsAFailedReviewSlotCrashed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("8", 40)
	payload := JobPayload{
		Repo: "mobile/app", Branch: "task-1800", PullRequest: 1800, HeadSHA: head,
		TaskID: "task-1800", ReviewRound: "review-1",
	}
	implPayload := payload
	implPayload.ReviewRound = ""
	implPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	seedMergeGateFixtureAgent(t, store, "implementer-1800")
	insertCompletedJob(t, store, db.Job{ID: "implement-1800", Agent: "implementer-1800", Type: "implement"}, implPayload)
	seed1799Review(t, store, "review-1800-failed", "g6-review-sol", JobFailed, payload)

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 1800, TaskID: "task-1800", Reviewer: "g6-review-sol",
	}, head)
	if err == nil {
		t.Fatal("a failed review slot did not refuse")
	}
	if !strings.Contains(err.Error(), "crashed reviewer") {
		t.Fatalf("a FAILED reviewer is no longer reported as crashed, so the distinction was lost in the other direction: %v", err)
	}
}
