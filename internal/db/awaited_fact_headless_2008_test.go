package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedAwaitedReview persists a succeeded review verdict for owner/repo#7.
func openHeadlessTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("openCachedTestStore: %v", err)
	}
	return store
}

func seedAwaitedReview(t *testing.T, store *Store, jobID, headSHA string, externallyDriven bool) {
	t.Helper()
	ctx := context.Background()
	payload := `{"repo":"owner/repo","pull_request":7,"head_sha":"` + headSHA +
		`","result":{"decision":"approved","summary":"seeded"}}`
	job := Job{ID: jobID, Agent: "reviewer", Type: "review", State: "succeeded", Payload: payload}
	event := JobEvent{Kind: "succeeded", Message: "approved"}
	if externallyDriven {
		if err := store.CreateExternallyDrivenJobWithEvent(ctx, job, event); err != nil {
			t.Fatalf("CreateExternallyDrivenJobWithEvent(%s): %v", jobID, err)
		}
		return
	}
	if err := store.CreateJobWithEvent(ctx, job, event); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", jobID, err)
	}
}

func subscribeAtHead(t *testing.T, store *Store, head string) (AwaitedFact, []HeadlessReviewSkip) {
	t.Helper()
	key, err := ReviewVerdictSubjectKey("owner/repo", 7, head)
	if err != nil {
		t.Fatalf("ReviewVerdictSubjectKey: %v", err)
	}
	fact, skipped, err := store.SubscribeAwaitedFact(context.Background(), AwaitedFactSubscription{
		WaiterRole: "gitmoot", SubjectKind: AwaitedFactSubjectReviewVerdict,
		SubjectKey: key, Deadline: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("SubscribeAwaitedFact: %v", err)
	}
	return fact, skipped
}

// TestSubscribeAwaitedFactReportsAHeadlessReviewItPassedOver is #2008's
// awaited-fact consumer.
//
// A headless review still cannot satisfy a review_verdict fact - it names no
// head to satisfy it at - and that is unchanged. What changes is that the row no
// longer vanishes: the store reports it so the caller, which can reach workflow
// as this package cannot, can state the exclusion.
//
// The report is RAW on purpose. This package must not classify the row, because
// the reason vocabulary lives in workflow and a copy here would be the second
// definition the campaign exists to remove.
func TestSubscribeAwaitedFactReportsAHeadlessReviewItPassedOver(t *testing.T) {
	store := openHeadlessTestStore(t)
	head := strings.Repeat("a", 40)
	seedAwaitedReview(t, store, "session-review", "", true)

	fact, skipped := subscribeAtHead(t, store, head)

	if fact.State == AwaitedFactStateSatisfied {
		t.Fatal("a headless review satisfied a head-bound awaited fact: the exclusion became a head binding")
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want exactly the headless row", skipped)
	}
	if skipped[0].JobID != "session-review" {
		t.Errorf("skipped job = %q, want session-review", skipped[0].JobID)
	}
	if !skipped[0].ExternallyDriven {
		t.Error("skipped row lost externally_driven: the caller cannot tell a session row from a merely headless one, so it would state the wrong reason")
	}
}

// TestSubscribeAwaitedFactDoesNotReportAReviewAtAnotherHead is the control. A
// row WITH an engine-observed head that simply is not this one was never this
// class, and reporting it would bury the rows that are.
func TestSubscribeAwaitedFactDoesNotReportAReviewAtAnotherHead(t *testing.T) {
	store := openHeadlessTestStore(t)
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	seedAwaitedReview(t, store, "other-head-review", headB, false)

	_, skipped := subscribeAtHead(t, store, headA)

	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none: this row has an engine-observed head", skipped)
	}
}

// TestSubscribeAwaitedFactStillSatisfiesAtTheHead keeps the two above from
// passing under a rule that stops satisfying facts at all.
func TestSubscribeAwaitedFactStillSatisfiesAtTheHead(t *testing.T) {
	store := openHeadlessTestStore(t)
	head := strings.Repeat("a", 40)
	seedAwaitedReview(t, store, "at-head-review", head, false)

	fact, skipped := subscribeAtHead(t, store, head)

	if fact.State != AwaitedFactStateSatisfied {
		t.Fatalf("fact state = %q, want satisfied: a review AT this head must still satisfy it", fact.State)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", skipped)
	}
}
