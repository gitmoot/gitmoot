package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func openAwaitedFactTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func awaitedReviewPayload(t *testing.T, repo string, pullRequest int, headSHA, decision, workflowID string) string {
	t.Helper()
	payload := map[string]any{
		"repo": repo, "pull_request": pullRequest, "head_sha": headSHA,
		"workflow_id": workflowID, "result": map[string]any{"decision": decision},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal review payload: %v", err)
	}
	return string(encoded)
}

func subscribeReviewFact(t *testing.T, store *Store, role, repo string, pullRequest int, headSHA string) AwaitedFact {
	t.Helper()
	key, err := ReviewVerdictSubjectKey(repo, pullRequest, headSHA)
	if err != nil {
		t.Fatalf("ReviewVerdictSubjectKey: %v", err)
	}
	fact, err := store.SubscribeAwaitedFact(context.Background(), AwaitedFactSubscription{
		WaiterRole: role, SubjectKind: AwaitedFactSubjectReviewVerdict,
		SubjectKey: key, Deadline: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("SubscribeAwaitedFact: %v", err)
	}
	return fact
}

func TestSubscribeAwaitedFactRechecksConcurrentProducerCommit(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	payload := awaitedReviewPayload(t, "acme/widget", 42, "head-race", "approved", "review-race")

	producer, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx producer: %v", err)
	}
	if _, err := producer.ExecContext(ctx, `
INSERT INTO jobs(id, agent, type, state, payload, root_id, workflow_id, repo, pull_request)
VALUES ('review-race', 'reviewer', 'review', 'succeeded', ?, 'review-race', 'review-race', 'acme/widget', 42)`, payload); err != nil {
		t.Fatalf("insert producer verdict: %v", err)
	}

	started := make(chan struct{})
	completed := make(chan struct {
		fact AwaitedFact
		err  error
	}, 1)
	go func() {
		close(started)
		key, keyErr := ReviewVerdictSubjectKey("acme/widget", 42, "head-race")
		if keyErr != nil {
			completed <- struct {
				fact AwaitedFact
				err  error
			}{err: keyErr}
			return
		}
		fact, subscribeErr := store.SubscribeAwaitedFact(ctx, AwaitedFactSubscription{
			WaiterRole: "lane", SubjectKind: AwaitedFactSubjectReviewVerdict,
			SubjectKey: key, Deadline: time.Now().UTC().Add(time.Hour),
		})
		completed <- struct {
			fact AwaitedFact
			err  error
		}{fact: fact, err: subscribeErr}
	}()
	<-started
	select {
	case result := <-completed:
		t.Fatalf("subscription completed before producer commit: fact=%+v err=%v", result.fact, result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := producer.Commit(); err != nil {
		t.Fatalf("commit producer verdict: %v", err)
	}
	result := <-completed
	if result.err != nil {
		t.Fatalf("SubscribeAwaitedFact after producer commit: %v", result.err)
	}
	if result.fact.State != AwaitedFactStateSatisfied {
		t.Fatalf("subscription state = %q, want %q after fact committed before registration completed", result.fact.State, AwaitedFactStateSatisfied)
	}
}

func TestAwaitedFactProducerCommitSatisfiesExactHead(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	initial := awaitedReviewPayload(t, "acme/widget", 43, "head-exact", "", "review-producer")
	if err := store.CreateJobWithEvent(ctx, Job{ID: "review-producer", Agent: "reviewer", Type: "review", State: "running", Payload: initial}, JobEvent{Kind: "running", Message: "started"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	fact := subscribeReviewFact(t, store, "lane", "acme/widget", 43, "head-exact")
	final := awaitedReviewPayload(t, "acme/widget", 43, "head-exact", "approved", "review-producer")
	changed, err := store.TransitionJobStatePayloadWithEvent(ctx, "review-producer", "running", "succeeded", final, JobEvent{Kind: "succeeded", Message: "approved"})
	if err != nil || !changed {
		t.Fatalf("TransitionJobStatePayloadWithEvent changed=%v err=%v", changed, err)
	}
	got, err := store.GetAwaitedFact(ctx, fact.ID)
	if err != nil {
		t.Fatalf("GetAwaitedFact: %v", err)
	}
	if got.State != AwaitedFactStateSatisfied {
		t.Fatalf("state = %q, want satisfied", got.State)
	}
	outbox, err := store.ListWakeOutbox(ctx, WakeOutboxStatePending)
	if err != nil {
		t.Fatalf("ListWakeOutbox: %v", err)
	}
	if len(outbox) != 1 || outbox[0].TargetRole != "lane" || outbox[0].CoalesceKey != "fact:lane" {
		t.Fatalf("pending fact outbox = %+v, want one exact lane delivery", outbox)
	}
}

func TestAwaitedFactOldHeadDoesNotSatisfyNewHead(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	initial := awaitedReviewPayload(t, "acme/widget", 44, "old-head", "", "review-old")
	if err := store.CreateJobWithEvent(ctx, Job{ID: "review-old", Agent: "reviewer", Type: "review", State: "running", Payload: initial}, JobEvent{Kind: "running", Message: "started"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	fact := subscribeReviewFact(t, store, "lane", "acme/widget", 44, "new-head")
	final := awaitedReviewPayload(t, "acme/widget", 44, "old-head", "approved", "review-old")
	changed, err := store.TransitionJobStatePayloadWithEvent(ctx, "review-old", "running", "succeeded", final, JobEvent{Kind: "succeeded", Message: "approved old head"})
	if err != nil || !changed {
		t.Fatalf("TransitionJobStatePayloadWithEvent changed=%v err=%v", changed, err)
	}
	got, err := store.GetAwaitedFact(ctx, fact.ID)
	if err != nil {
		t.Fatalf("GetAwaitedFact: %v", err)
	}
	if got.State != AwaitedFactStateWaiting {
		t.Fatalf("new-head subscription state = %q after old-head verdict, want waiting", got.State)
	}
}
