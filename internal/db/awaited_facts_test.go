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
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
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
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	producerStore, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("Open producer store: %v", err)
	}
	t.Cleanup(func() { _ = producerStore.Close() })
	subscriberStore, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("Open subscriber store: %v", err)
	}
	t.Cleanup(func() { _ = subscriberStore.Close() })
	ctx := context.Background()
	payload := awaitedReviewPayload(t, "acme/widget", 42, "head-race", "approved", "review-race")

	producer, err := producerStore.db.BeginTx(ctx, nil)
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
		fact, subscribeErr := subscriberStore.SubscribeAwaitedFact(ctx, AwaitedFactSubscription{
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
	case <-time.After(150 * time.Millisecond):
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

func TestSubscribeAwaitedFactRecheckRejectsOldHead(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	oldPayload := awaitedReviewPayload(t, "acme/widget", 45, "old-head-a", "approved", "review-already-done")
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "review-already-done", Agent: "reviewer", Type: "review",
		State: "succeeded", Payload: oldPayload,
	}, JobEvent{Kind: "succeeded", Message: "approved old head"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	fact := subscribeReviewFact(t, store, "lane", "acme/widget", 45, "new-head-b")
	if fact.State != AwaitedFactStateWaiting {
		t.Fatalf("new-head subscription state = %q with old verdict already committed; want waiting", fact.State)
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

// TestSucceededReviewVerdictsFiltersCanonicalRows pins the pure read used by
// review-loop admission. It kills mutants that query payload-only repo/PR,
// include unfinished/non-review jobs, accept malformed results, or lose the
// deterministic updated_at DESC, id DESC ordering. The explicit timestamps
// below kill mutants that drop either ordering term.
func TestSucceededReviewVerdictsFiltersCanonicalRows(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	seed := func(id, agent, jobType, state, repo string, pr int, payload string) {
		t.Helper()
		if err := store.CreateJob(ctx, Job{ID: id, Agent: agent, Type: jobType, State: state, Payload: payload}); err != nil {
			t.Fatalf("CreateJob(%s): %v", id, err)
		}
		// Deliberately control the denormalized filter columns independently from
		// payload repo/PR so a payload-only query mutant cannot survive.
		if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET repo = ?, pull_request = ? WHERE id = ?`, repo, pr, id); err != nil {
			t.Fatalf("project job identity(%s): %v", id, err)
		}
	}

	seed("columns-win", "audit-columns", "review", "succeeded", "acme/widget", 52,
		awaitedReviewPayload(t, "other/payload", 999, "head-columns", "approved", "columns-win"))
	seed("valid-a", "audit-a", "review", "succeeded", "Acme/Widget", 52,
		awaitedReviewPayload(t, "Acme/Widget", 52, "HEAD-A", " APPROVED ", "valid-a"))
	seed("valid-z", "audit-z", "review", "succeeded", "acme/widget", 52,
		awaitedReviewPayload(t, "acme/widget", 52, "head-a", "changes_requested", "valid-z"))
	seed("wrong-pr", "audit", "review", "succeeded", "acme/widget", 53,
		awaitedReviewPayload(t, "acme/widget", 53, "head-a", "approved", "wrong-pr"))
	seed("wrong-repo", "audit", "review", "succeeded", "acme/other", 52,
		awaitedReviewPayload(t, "acme/widget", 52, "head-a", "approved", "wrong-repo"))
	seed("running", "audit", "review", "running", "acme/widget", 52,
		awaitedReviewPayload(t, "acme/widget", 52, "head-a", "approved", "running"))
	seed("implement", "audit", "implement", "succeeded", "acme/widget", 52,
		awaitedReviewPayload(t, "acme/widget", 52, "head-a", "approved", "implement"))
	seed("malformed", "audit", "review", "succeeded", "acme/widget", 52, "{")
	seed("no-result", "audit", "review", "succeeded", "acme/widget", 52,
		`{"repo":"acme/widget","pull_request":52,"head_sha":"head-a"}`)

	// Make timestamp order conflict with id order: valid-a is newer despite its
	// lower-sorting id. Keep columns-win and valid-z tied so id DESC remains the
	// required deterministic tie-break (valid-z was inserted after columns-win).
	for _, fixture := range []struct {
		id        string
		updatedAt string
	}{
		{id: "valid-a", updatedAt: "2026-08-02 03:00:00"},
		{id: "columns-win", updatedAt: "2026-08-02 02:00:00"},
		{id: "valid-z", updatedAt: "2026-08-02 02:00:00"},
	} {
		if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, fixture.updatedAt, fixture.id); err != nil {
			t.Fatalf("set updated_at for %s: %v", fixture.id, err)
		}
	}

	got, err := store.SucceededReviewVerdicts(ctx, " ACME/WIDGET ", 52)
	if err != nil {
		t.Fatalf("SucceededReviewVerdicts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("verdicts = %+v, want exactly the three rows selected by denormalized repo/PR", got)
	}
	if got[0].JobID != "valid-a" || got[0].Agent != "audit-a" || got[0].HeadSHA != "head-a" || got[0].Decision != "approved" {
		t.Fatalf("newest verdict = %+v", got[0])
	}
	if got[1].JobID != "valid-z" || got[1].HeadSHA != "head-a" || got[1].Decision != "changes_requested" {
		t.Fatalf("equal-timestamp id-desc verdict = %+v", got[1])
	}
	if got[2].JobID != "columns-win" || got[2].HeadSHA != "head-columns" || got[2].Decision != "approved" {
		t.Fatalf("denormalized-column verdict = %+v", got[2])
	}
}
