package workflow

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1419: NOTHING COUNTS RELOCATIONS, SO SIX FIX ROUNDS LOOK LIKE SIX UNITS OF
// PROGRESS.
//
// The count was never missing. nextReviewRound already derives maxReviewRound to
// build the label, and reviewRoundCount already turns it into a memory-harvest
// score. What was missing is that neither tells the people in the loop, so a
// lane converging on a fix and a lane chasing one defect around a file emit
// identical success signals: a job, a review, a verdict, green CI.
//
// Measured on this repository the night this landed, over one campaign:
// PR #2078 five changes_requested rounds on ONE hand-written list, each round a
// new direction - advertised an unusable key, hid a usable one, duplicated an
// entry, classified a field from an unmerged branch, misclassified a working
// key. Nothing counted those as one defect relocating.

func roundThresholdEvents(t *testing.T, store *db.Store, jobID string) []db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	var found []db.JobEvent
	for _, event := range events {
		if event.Kind == "review_round_threshold" {
			found = append(found, event)
		}
	}
	return found
}

// advanceReviewRounds drives `rounds` real review->changes_requested cycles
// through HandlePullRequestOpened, and returns the review job ids per round.
func advanceReviewRounds(t *testing.T, rounds int) (*db.Store, []string) {
	t.Helper()
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	// Follow-up rounds are SCOPED, so the engine needs a changed-files resolver
	// or it refuses before any round is created. The scope content is irrelevant
	// here; what is under test is the round count.
	engine.ReviewChangedFiles = func(_ context.Context, _ string, _ int, _ string, _ string) ([]string, error) {
		return []string{"internal/boundary.go"}, nil
	}
	insertCompletedJob(t, store, db.Job{ID: "initial-implement", Agent: "lead", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-1678", PullRequest: 1678,
		TaskID: "task-1678", LeadAgent: "lead",
	})

	ids := make([]string, 0, rounds)
	for round := 1; round <= rounds; round++ {
		head := strings.Repeat(string(rune('a'+round-1)), 40)
		if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent(head)); err != nil {
			t.Fatalf("HandlePullRequestOpened(round %d): %v", round, err)
		}
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		var newest string
		for _, job := range jobs {
			if job.Type == "review" && strings.Contains(job.ID, "review-"+strconv.Itoa(round)) {
				newest = job.ID
			}
		}
		if newest == "" {
			t.Fatalf("round %d produced no review job", round)
		}
		ids = append(ids, newest)
		// Close the round with a real changes_requested so the next head opens the
		// next round rather than reusing this one.
		completeQueuedReview(t, store, mustJob(t, store, newest), JobPayload{
			Repo: "gitmoot/gitmoot", Branch: "task-1678", PullRequest: 1678, HeadSHA: head,
			TaskID: "task-1678", ReviewRound: "review-" + strconv.Itoa(round), LeadAgent: "lead",
		}, AgentResult{Decision: "changes_requested", Summary: "fix the boundary check"})
	}
	return store, ids
}

// THE ACCEPTANCE CRITERION, both halves. Three cycles warn on the third; two
// produce none.
func TestThirdReviewRoundWarnsAndEarlierRoundsDoNot(t *testing.T) {
	store, ids := advanceReviewRounds(t, 3)
	if len(ids) != 3 {
		t.Fatalf("expected three review jobs, got %d", len(ids))
	}
	for i, id := range ids[:2] {
		if events := roundThresholdEvents(t, store, id); len(events) != 0 {
			t.Fatalf("round %d warned (%d events); a warning on every round is noise, not a signal", i+1, len(events))
		}
	}
	third := roundThresholdEvents(t, store, ids[2])
	if len(third) != 1 {
		t.Fatalf("round 3 emitted %d review_round_threshold events, want 1", len(third))
	}
	// The count must be IN the message: a warning that does not say how many
	// times leaves the reader doing the counting the engine just did.
	if !strings.Contains(third[0].Message, "review round 3") {
		t.Fatalf("warning does not name the count: %s", third[0].Message)
	}
	// And it must ask for the impossibility statement, which is the part of
	// #1419 that actually changed the outcome on the measured instance.
	if !strings.Contains(third[0].Message, "NO version of") {
		t.Fatalf("warning does not ask what no version of the code may do: %s", third[0].Message)
	}
}

// A FOURTH ROUND STILL WARNS. The threshold is a floor, not a one-shot: the
// measured instance reached SIX, and a warning that fires once and goes quiet
// would have been silent for rounds four, five and six - the expensive ones.
func TestRoundsPastTheThresholdKeepWarning(t *testing.T) {
	store, ids := advanceReviewRounds(t, 4)
	if events := roundThresholdEvents(t, store, ids[3]); len(events) != 1 {
		t.Fatalf("round 4 emitted %d warnings, want 1; the threshold is a floor, not a one-shot", len(events))
	}
}

// #2087 review, P1: THE HIGH-RISK PATH RETURNED BEFORE THE RECORDER.
//
// dispatchHighRiskReview is entered when risk tiers are enabled and the tier is
// HIGH, and it returns before the native reviewer loop - so the lens fan-out,
// which is the path a HARD problem takes, was the one path that never said how
// many rounds it had taken. The reviewer found it by driving four rounds of one
// relocating finding through it and scanning every job for the event.
//
// That is the worst possible place for this gap: a PR that keeps relocating a
// defect is exactly a PR likely to be classified high risk.
func TestHighRiskReviewAlsoWarnsPastTheThreshold(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true
	engine.ReviewChangedFiles = func(_ context.Context, _ string, _ int, _ string, _ string) ([]string, error) {
		return []string{"internal/auth/session.go"}, nil
	}

	// FOUR REAL ROUNDS THROUGH THE PRODUCTION PATH, not a direct call to the
	// recorder. An earlier draft of this test invoked recordReviewRoundThreshold
	// itself for round 3 - the same helper-only shape #2061 f3 filed against me
	// hours ago, reappearing in the test for the fix that finding prompted.
	//
	// Each round: open at a new head, then close the round's lens jobs with
	// changes_requested on the SAME finding id, which is the relocation shape
	// #1419 targets.
	seen := map[string]int{}
	for round := 1; round <= 4; round++ {
		event := highRiskEvent()
		event.HeadSHA = strings.Repeat(string(rune('a'+round-1)), 40)
		if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
			t.Fatalf("HandlePullRequestOpened(round %d): %v", round, err)
		}
		coordID := "review-coordinator/task-7/review-" + strconv.Itoa(round)
		if _, err := store.GetJob(ctx, coordID); err != nil {
			t.Fatalf("round %d produced no coordinator %q: %v", round, coordID, err)
		}
		seen[coordID] = countJobEvents(t, store, coordID, "review_round_threshold")

		for _, lens := range []string{LensCorrectness, LensSecurity} {
			id := coordID + "/delegation/" + lens
			job, err := store.GetJob(ctx, id)
			if err != nil {
				continue
			}
			payload, err := unmarshalPayload(job.Payload)
			if err != nil {
				t.Fatalf("unmarshal %s: %v", id, err)
			}
			completeQueuedReview(t, store, job, payload, AgentResult{
				Decision: "changes_requested", Severity: "P1", Summary: "the same defect, relocated",
				Findings: []json.RawMessage{json.RawMessage(`{"uid":"F-1","severity":"P1","title":"one defect, moving"}`)},
			})
		}
	}

	for round := 1; round <= 2; round++ {
		id := "review-coordinator/task-7/review-" + strconv.Itoa(round)
		if seen[id] != 0 {
			t.Fatalf("round %d warned %d times; a warning on every round is noise", round, seen[id])
		}
	}
	for round := 3; round <= 4; round++ {
		id := "review-coordinator/task-7/review-" + strconv.Itoa(round)
		if seen[id] != 1 {
			t.Fatalf("round %d on the HIGH-RISK path emitted %d warnings, want 1; "+
				"the lens fan-out is the path a hard problem takes", round, seen[id])
		}
	}

}
