package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestUnscopableFollowUpReanchorsInsteadOfWedging pins the recovery path an
// unscopable range used to lack. A force-pushed head cannot be compared against the
// head the reviewer last saw, so the round degrades to a recorded FULL review at that
// head. The point is what happens NEXT: that review is the new anchor, so the round
// after it is scoped again from it. Blocking instead left the anchor frozen at the
// pre-force-push head, and every later head re-compared the same unscopable range.
func TestUnscopableFollowUpReanchorsInsteadOfWedging(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Fix the boundary check.",
	})
	var ranges []string
	engine.ReviewChangedFiles = func(_ context.Context, _ string, _ int, previousHead, currentHead string) ([]string, error) {
		ranges = append(ranges, previousHead+".."+currentHead)
		if previousHead == "head-one" {
			return nil, ReviewScopeUnavailableError{Reason: `review scope compare is "diverged", not a direct follow-up`}
		}
		return []string{"internal/boundary.go"}, nil
	}

	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened at unscopable head: %v", err)
	}
	assertTaskState(t, store, "task-1678", TaskReviewing)
	degraded := mustJob(t, store, "review-audit-task-1678-review-2")
	degradedPayload, err := unmarshalPayload(degraded.Payload)
	if err != nil {
		t.Fatalf("unmarshal degraded payload: %v", err)
	}
	if degradedPayload.ReviewScope != nil {
		t.Fatalf("degraded review carries a scope = %+v, want a full review", degradedPayload.ReviewScope)
	}
	if strings.Contains(degradedPayload.Instructions, "Do not re-review the full PR-to-base diff") {
		t.Fatalf("degraded review kept scoped instructions: %q", degradedPayload.Instructions)
	}
	if !hasReviewScopeUnavailableEvent(t, store, "head-two") {
		t.Fatal("the unscoped head left no review_scope_unavailable record")
	}

	// The degraded review returns a verdict, which is what re-anchors the loop.
	completeQueuedReview(t, store, degraded, degradedPayload, AgentResult{
		Decision: "changes_requested",
		Summary:  "Still broken.",
		Findings: []json.RawMessage{json.RawMessage(`{"id":"F-2","summary":"Still broken."}`)},
	})

	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-three")); err != nil {
		t.Fatalf("HandlePullRequestOpened after re-anchor: %v", err)
	}
	if len(ranges) != 2 || ranges[1] != "head-two..head-three" {
		t.Fatalf("compare ranges = %#v, want the second scoped from head-two", ranges)
	}
	next := mustJob(t, store, "review-audit-task-1678-review-3")
	nextPayload, err := unmarshalPayload(next.Payload)
	if err != nil {
		t.Fatalf("unmarshal re-anchored payload: %v", err)
	}
	if nextPayload.ReviewScope == nil || nextPayload.ReviewScope.PreviousHeadSHA != "head-two" {
		t.Fatalf("re-anchored scope = %+v, want previous head head-two", nextPayload.ReviewScope)
	}
	if len(nextPayload.ReviewScope.Findings) != 1 || !strings.Contains(nextPayload.ReviewScope.Findings[0], `"id":"F-2"`) {
		t.Fatalf("re-anchored findings = %#v, want F-2 carried forward", nextPayload.ReviewScope.Findings)
	}
}

func hasReviewScopeUnavailableEvent(t *testing.T, store *db.Store, head string) bool {
	t.Helper()
	events, err := store.ListTaskEvents(context.Background(), "task-1678")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	seen := 0
	for _, event := range events {
		if event.Kind == "review_scope_unavailable" &&
			strings.Contains(event.Reason, "head_sha="+head) &&
			strings.Contains(event.Reason, ReviewScopeUnavailableMarker) {
			seen++
		}
	}
	if seen > 1 {
		t.Fatalf("review_scope_unavailable records for %s = %d, want at most 1", head, seen)
	}
	return seen == 1
}
