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

// TestRoutineRoundInheritsLensScopesForOneReviewer pins the lens -> routine
// transition. One reviewer runs several lenses on the high-risk path, so its
// candidates are keyed by lens id. A following ROUTINE round has no lens id: looking
// up the bare reviewer alone missed every lens entry and silently issued an unscoped
// full-PR re-review, spending the compare call anyway.
func TestRoutineRoundInheritsLensScopesForOneReviewer(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	insertPriorLensReview(t, store, "prior-correctness", LensCorrectness, JobSucceeded, "changes_requested",
		json.RawMessage(`{"lens":"correctness","id":"C-1","summary":"correctness finding"}`))
	insertPriorLensReview(t, store, "prior-security", LensSecurity, JobSucceeded, "changes_requested",
		json.RawMessage(`{"lens":"security","id":"S-9","summary":"security finding"}`))

	engine := testEngine(store)
	// The prior round was high-risk; this round classifies routine (label removed,
	// or the tier flag flipped off).
	engine.RiskTiersEnabled = false
	compareCalls := 0
	engine.ReviewChangedFiles = func(_ context.Context, _ string, _ int, previousHead, currentHead string) ([]string, error) {
		compareCalls++
		if previousHead != "head-one" || currentHead != "head-two" {
			t.Fatalf("compare = %s..%s, want head-one..head-two", previousHead, currentHead)
		}
		return []string{"internal/auth/session.go"}, nil
	}
	event := highRiskEvent()
	event.HeadSHA = "head-two"
	event.RequiredReviewers = []string{"audit"}
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	if compareCalls != 1 {
		t.Fatalf("compare calls = %d, want 1", compareCalls)
	}
	job := mustJob(t, store, "review-audit-task-7-review-2")
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.ReviewScope == nil {
		t.Fatal("routine round after a lens round lost the scope and re-reviewed the full PR")
	}
	if payload.ReviewScope.PreviousHeadSHA != "head-one" {
		t.Fatalf("scope previous head = %q, want head-one", payload.ReviewScope.PreviousHeadSHA)
	}
	joined := strings.Join(payload.ReviewScope.Findings, " ")
	if !strings.Contains(joined, `"id":"C-1"`) || !strings.Contains(joined, `"id":"S-9"`) {
		t.Fatalf("routine scope findings = %#v, want both lenses' findings", payload.ReviewScope.Findings)
	}
	if len(payload.ReviewScope.ChangedFiles) != 1 || payload.ReviewScope.ChangedFiles[0] != "internal/auth/session.go" {
		t.Fatalf("routine scope files = %#v", payload.ReviewScope.ChangedFiles)
	}
	if !strings.Contains(payload.Instructions, "Do not re-review the full PR-to-base diff") {
		t.Fatalf("routine round issued unscoped instructions: %q", payload.Instructions)
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
