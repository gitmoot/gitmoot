package db

import (
	"context"
	"fmt"
	"testing"
)

// fanOutReviewPayload builds a stored review payload the way production writes
// one. panel chooses between the two shapes a coordinator announcement can have:
// the declared panel still visible (native review), or stripped by the pipeline
// mailbox seam with only the classification left behind.
func fanOutReviewPayload(headSHA string, decision string, panel string) string {
	return fmt.Sprintf(
		`{"repo":"gitmoot/gitmoot","pull_request":9,"head_sha":%q,"result":{"decision":%q,"summary":"s"%s}}`,
		headSHA, decision, panel)
}

const (
	declaredPanel = `,"delegations":[{"id":"lens-a","agent":"r1","action":"review","prompt":"p"}]`
	strippedPanel = `,"fan_out":true`
	noPanel       = ``
)

func seedFanOutReviewJob(t *testing.T, store *Store, id, agent, payload string) {
	t.Helper()
	if err := store.CreateJob(context.Background(), Job{
		ID: id, Agent: agent, Type: "review", State: "succeeded",
		Repo: "gitmoot/gitmoot", PullRequest: 9, Payload: payload,
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

// #1685 P2. SucceededReviewVerdicts is canonical same-head verdict history: the
// review loop reads it to decide whether dispatching again would repeat a stable
// verdict. A coordinator announcement entering that history suppresses the very
// retry that would have produced a real verdict.
func TestSucceededReviewVerdictsExcludesFanOuts(t *testing.T) {
	ctx := context.Background()
	store := openAwaitedFactTestStore(t)
	seedFanOutReviewJob(t, store, "review-declared-fanout", "coordinator-a", fanOutReviewPayload("head123", "approved", declaredPanel))
	seedFanOutReviewJob(t, store, "review-stripped-fanout", "coordinator-b", fanOutReviewPayload("head123", "approved", strippedPanel))
	seedFanOutReviewJob(t, store, "review-real-verdict", "reviewer-y", fanOutReviewPayload("head123", "approved", noPanel))

	verdicts, err := store.SucceededReviewVerdicts(ctx, "gitmoot/gitmoot", 9)
	if err != nil {
		t.Fatalf("SucceededReviewVerdicts: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdict history = %+v, want only the real leaf verdict", verdicts)
	}
	if verdicts[0].JobID != "review-real-verdict" {
		t.Fatalf("verdict history kept %q, want review-real-verdict", verdicts[0].JobID)
	}
}

// The awaited review-verdict fact wakes a waiting coordinator. Satisfying it on
// an announcement reports an answer nobody gave.
func TestAwaitedReviewVerdictFactIgnoresFanOuts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		panel string
		want  bool
	}{
		{name: "declared panel", panel: declaredPanel, want: false},
		{name: "stripped panel", panel: strippedPanel, want: false},
		{name: "real leaf verdict", panel: noPanel, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := reviewVerdictFact("review-1", "agent-1", "succeeded",
				fanOutReviewPayload("head123", "approved", tc.panel),
				func(string) string { return "P3" })
			if ok != tc.want {
				t.Fatalf("reviewVerdictFact ok = %v, want %v", ok, tc.want)
			}
		})
	}
}

// End to end through the production write path: CreateJob resolves awaited facts
// in the same transaction, so a settling fan-out must leave the waiter waiting
// and the panel's synthesized leaf verdict must then release it.
func TestAwaitedFactWaitsThroughFanOutAndResolvesOnLeafVerdict(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	fact := subscribeReviewFact(t, store, "coordinator", "gitmoot/gitmoot", 9, "head123")

	settleReviewJob(t, store, "review-fanout", "coordinator-a", fanOutReviewPayload("head123", "approved", declaredPanel))
	if state := awaitedFactState(t, store, fact.ID); state != "waiting" {
		t.Fatalf("awaited fact state after a fan-out = %q, want waiting: an announcement satisfied the wake", state)
	}

	settleReviewJob(t, store, "review-leaf", "reviewer-y", fanOutReviewPayload("head123", "approved", noPanel))
	if state := awaitedFactState(t, store, fact.ID); state == "waiting" {
		t.Fatal("the real leaf verdict never released the waiter")
	}
}

// settleReviewJob drives the production settlement path: a review job is created
// running and TRANSITIONS to succeeded carrying its result, which is the write
// that resolves awaited facts in the same transaction.
func settleReviewJob(t *testing.T, store *Store, id, agent, payload string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: id, Agent: agent, Type: "review", State: "running",
		Repo: "gitmoot/gitmoot", PullRequest: 9, Payload: payload,
	}, JobEvent{Kind: "running", Message: "started"}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", id, err)
	}
	changed, err := store.TransitionJobStatePayloadWithEvent(ctx, id, "running", "succeeded", payload,
		JobEvent{Kind: "succeeded", Message: "settled"})
	if err != nil || !changed {
		t.Fatalf("TransitionJobStatePayloadWithEvent(%s) changed=%v err=%v", id, changed, err)
	}
}

func awaitedFactState(t *testing.T, store *Store, id int64) string {
	t.Helper()
	var state string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT state FROM awaited_facts WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatalf("read awaited fact %d: %v", id, err)
	}
	return state
}
