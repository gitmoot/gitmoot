package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// TestPolicyMergeGateZeroExternalCIDefersWithinGraceWindow confirms the #596
// grace-window/no-CI-conclusion mechanism is still reachable through the new
// unconditional review-and-CI gate (#1114): a head reporting zero external CI
// defers (mergePending, not a policy miss) rather than immediately escalating,
// exactly as it did before the new gate was added — escalating on the very
// first zero-CI observation would fire on every PR the instant it's opened,
// well before GitHub Actions has had a chance to create a check.
func TestPolicyMergeGateZeroExternalCIDefersWithinGraceWindow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/noted",
		Branch:      "task-11",
		PullRequest: 11,
		HeadSHA:     "head123",
		TaskID:      "task-11",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:    11,
			State:     "open",
			HeadRef:   "task-11",
			BaseRef:   "main",
			HeadSHA:   "head123",
			Mergeable: &mergeable,
		},
		status:   github.CombinedStatus{State: "success"},
		noChecks: true,
	}
	gate := PolicyMergeGate{
		AutoMerge: true,
		Store:     store,
		GitHub:    gh,
		Git:       &fakeMergeGateGit{clean: true},
	}

	for attempt := 0; attempt < 2; attempt++ {
		decision, err := gate.Evaluate(ctx, MergeRequest{
			Repo: "jerryfane/noted", PullRequest: 11, TaskID: "task-11",
		})
		if err != nil {
			t.Fatalf("Evaluate attempt %d: %v", attempt+1, err)
		}
		if decision.LeaveOpen || decision.EscalateMergeGateMiss || !decision.Ready || decision.Merged ||
			!strings.Contains(decision.Reason, "waiting to confirm no external CI") {
			t.Fatalf("decision attempt %d = %+v, want a non-escalating grace-window defer", attempt+1, decision)
		}
	}
	if len(gh.merges) != 0 {
		t.Fatalf("zero-CI gate within the grace window issued a merge: merges=%+v", gh.merges)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "pending") {
		t.Fatalf("statuses = %+v, want a pending gitmoot/merge-gate stamp", gh.statuses)
	}
	if gh.prCheckCalls != 0 || len(gh.checkRefs) != 2 ||
		gh.checkRefs[0] != "head123" || gh.checkRefs[1] != "head123" {
		t.Fatalf("check calls = pr:%d refs:%v; want exact-head checks only", gh.prCheckCalls, gh.checkRefs)
	}
}
