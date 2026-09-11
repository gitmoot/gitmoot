package workflow

import (
	"context"
	"strings"
	"testing"
)

// THE NORMAL ORDERING (#2150): a review at head N completes before the repo poll
// refreshes the observed head from N-1.
//
// This is the production-path regression. It reproduces the exact sequence
// measured twice in a row on gitmoot/gitmoot#2146 - round three approved
// 946b49de while the observed row still held b5317e68, round four approved
// 1c1e0793 while it held 946b49de - and asserts the properties the fix must
// have. Under the previous behaviour the first advancement SETTLED, writing
// advance_completed, and since retryPendingJobAdvancements only re-fires jobs
// whose latest marker is advance_retry, nothing ever re-evaluated: the task
// stayed changes_requested across two approvals and the pull request could not
// be merged.
func TestStaleObservedHeadHoldsRetryablyThenClearsWhenCacheCatchesUp(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedWedgedTask(t, store)
	seedImplementAttribution(t, store)
	engine, gh := wedgeEngine(t, store)

	// The observed row is one head behind: the poll has not run since the push.
	seedObservedPullRequest(t, store, "head-previous")
	seedReviewJob(t, store, "review-current", "auditor", "head-current", "approved", JobSucceeded)

	// PROPERTY 1: the first advancement holds, and holds RETRYABLY - surfaced as
	// an error with no settled review_advance_held row, because the caller's
	// recordAdvanceRetryOnce carries the message on a single deduped marker.
	firstErr := engine.AdvanceJob(ctx, "review-current")
	if firstErr == nil {
		t.Fatal("first AdvanceJob returned nil: a stale observed head was settled, which is the wedge #2150 describes")
	}
	if !strings.Contains(firstErr.Error(), "review approval held") {
		t.Fatalf("first hold error = %v, want a held-approval error", firstErr)
	}
	if reason := heldReason(t, store, "review-current"); reason != "" {
		t.Fatalf("review_advance_held = %q, want none: settling a transient hold is what prevented any retry", reason)
	}
	assertTaskState(t, store, "task-9", TaskChangesRequested)

	// PROPERTY 5a: repeated attempts before the cache catches up are idempotent -
	// same decision, still no settled row, no state change.
	if err := engine.AdvanceJob(ctx, "review-current"); err == nil {
		t.Fatal("second AdvanceJob returned nil while the observed head was still stale")
	}
	if reason := heldReason(t, store, "review-current"); reason != "" {
		t.Fatalf("review_advance_held = %q after a repeat attempt, want none", reason)
	}
	assertTaskState(t, store, "task-9", TaskChangesRequested)

	// PROPERTY 2: the poll catches up. NO new commit and NO new review.
	seedObservedPullRequest(t, store, "head-current")

	// PROPERTY 3: the same job, re-advanced by the existing machinery, now clears
	// changes_requested at the exact approved head.
	if err := engine.AdvanceJob(ctx, "review-current"); err != nil {
		t.Fatalf("AdvanceJob after the observed head caught up returned error: %v", err)
	}
	if state := taskState(t, store, "task-9"); state == string(TaskChangesRequested) {
		t.Fatalf("task state = %q, want changes_requested cleared once the approval's head was confirmed current", state)
	}
	_ = gh
}

// PROPERTY 4: a GENUINELY superseded approval must never admit, however many
// times it is re-evaluated.
//
// This is the safety half of the same change. Retrying must not become a way for
// an old approval to eventually slip through: the head comparison runs again on
// every attempt, so an approval whose head is not the observed one keeps being
// refused for as long as that remains true.
func TestGenuinelySupersededApprovalNeverAdmitsAcrossRetries(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedWedgedTask(t, store)
	seedImplementAttribution(t, store)
	engine, gh := wedgeEngine(t, store)

	// The observed head is genuinely newer, and it stays that way.
	seedObservedPullRequest(t, store, "head-new")
	seedReviewJob(t, store, "review-stale", "auditor", "head-old", "approved", JobSucceeded)

	for attempt := 1; attempt <= 5; attempt++ {
		err := engine.AdvanceJob(ctx, "review-stale")
		if err == nil {
			t.Fatalf("attempt %d admitted a superseded approval", attempt)
		}
		if !strings.Contains(err.Error(), "superseded head") {
			t.Fatalf("attempt %d error = %v, want it to name the superseded head", attempt, err)
		}
		assertTaskState(t, store, "task-9", TaskChangesRequested)
		if len(gh.merges) != 0 {
			t.Fatalf("attempt %d merge calls = %d, want 0", attempt, len(gh.merges))
		}
	}

	// PROPERTY 5b: bounded in the event log. Repeated retries must not append a
	// row per attempt; the caller keeps one deduped advance_retry marker, and this
	// path writes no settled row at all. A per-tick row is what grew job_events to
	// ~1.8M rows once before.
	events, err := store.ListJobEvents(ctx, "review-stale")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	held := 0
	for _, event := range events {
		if event.Kind == "review_advance_held" {
			held++
		}
	}
	if held != 0 {
		t.Fatalf("review_advance_held rows = %d after 5 attempts, want 0 from this path", held)
	}
}
