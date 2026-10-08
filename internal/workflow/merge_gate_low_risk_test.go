package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
)

const lowRiskTestRepo = "gitmoot/test-check"

// lowRiskGateFixture is an exact-head-approved pull request with green external
// CI in a repo that opted into auto_merge = "low_risk".
func lowRiskGateFixture(t *testing.T, repo string) (PolicyMergeGate, *fakeMergeGateGitHub) {
	t.Helper()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        repo,
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "head123",
		TaskID:      "task-7",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 7, State: "open", HeadRef: "task-7", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success"},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{
		AutoMerge: true, LowRiskOnly: true, Store: store, GitHub: gh,
		Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir(),
	}
	return gate, gh
}

func lowRiskLevelAt(level, head string, err error) func(context.Context, string, int, string) (reviewlevel.Decision, error) {
	return func(context.Context, string, int, string) (reviewlevel.Decision, error) {
		if err != nil {
			return reviewlevel.Decision{}, err
		}
		return reviewlevel.Decision{HeadSHA: head, Level: level, Source: "jev", Reason: "fixture"}, nil
	}
}

func lowRiskRequest(repo string) MergeRequest {
	return MergeRequest{Repo: repo, PullRequest: 7, TaskID: "task-7", Reviewer: "audit"}
}

func assertLowRiskLeftOpen(t *testing.T, decision MergeDecision, gh *fakeMergeGateGitHub, want string) {
	t.Helper()
	reason := decision.Reason.Render()
	if !decision.LeaveOpen || decision.Merged || !strings.HasPrefix(reason, LowRiskAutoMergeLeaveOpenPrefix) || !strings.Contains(reason, want) {
		t.Fatalf("decision = %+v reason %q; want low-risk leave-open naming %q", decision, reason, want)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("low-risk leave-open issued a merge: %+v", gh.merges)
	}
}

func TestLowRiskAutoMergeMergesLevelOneHead(t *testing.T) {
	for _, level := range []string{reviewlevel.LevelNoReview, reviewlevel.LevelBackground} {
		t.Run(level, func(t *testing.T) {
			gate, gh := lowRiskGateFixture(t, lowRiskTestRepo)
			gate.ReviewLevel = lowRiskLevelAt(level, "head123", nil)
			decision, err := gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if !decision.Merged || decision.ReviewLevel != level || len(gh.merges) != 1 || gh.merges[0].MatchHeadCommit != "head123" {
				t.Fatalf("decision = %+v merges = %+v; want an exact-head merge at %s", decision, gh.merges, level)
			}
		})
	}
}

func TestLowRiskAutoMergeLeavesLevelThreeHeadOpen(t *testing.T) {
	gate, gh := lowRiskGateFixture(t, lowRiskTestRepo)
	gate.ReviewLevel = lowRiskLevelAt(reviewlevel.LevelRequired, "head123", nil)
	decision, err := gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	assertLowRiskLeftOpen(t, decision, gh, "is level 3")
}

func TestLowRiskAutoMergeLeavesClassifierErrorOpen(t *testing.T) {
	gate, gh := lowRiskGateFixture(t, lowRiskTestRepo)
	gate.ReviewLevel = lowRiskLevelAt("", "", errors.New("jev unreachable"))
	decision, err := gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	assertLowRiskLeftOpen(t, decision, gh, "review level unavailable")

	gate, gh = lowRiskGateFixture(t, lowRiskTestRepo)
	decision, err = gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo))
	if err != nil {
		t.Fatalf("Evaluate without classifier: %v", err)
	}
	assertLowRiskLeftOpen(t, decision, gh, "classifier is not configured")
}

func TestLowRiskAutoMergeLeavesHeadMovedAfterClassificationOpen(t *testing.T) {
	gate, gh := lowRiskGateFixture(t, lowRiskTestRepo)
	gate.ReviewLevel = lowRiskLevelAt(reviewlevel.LevelNoReview, "oldhead", nil)
	decision, err := gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	assertLowRiskLeftOpen(t, decision, gh, "not the current head head123")
}

func TestLowRiskAutoMergeLeavesHoldLabelOpen(t *testing.T) {
	gate, gh := lowRiskGateFixture(t, lowRiskTestRepo)
	gh.pr.Labels = []github.PullRequestLabel{{Name: "do-not-merge"}}
	gate.ReviewLevel = lowRiskLevelAt(reviewlevel.LevelNoReview, "head123", nil)
	decision, err := gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	assertLowRiskLeftOpen(t, decision, gh, `hold label "do-not-merge"`)
}

func TestLowRiskAutoMergeRefusesGitmootGitmoot(t *testing.T) {
	gate, gh := lowRiskGateFixture(t, "gitmoot/gitmoot")
	gate.ReviewLevel = lowRiskLevelAt(reviewlevel.LevelNoReview, "head123", nil)
	decision, err := gate.Evaluate(context.Background(), lowRiskRequest("gitmoot/gitmoot"))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	assertLowRiskLeftOpen(t, decision, gh, "gitmoot/gitmoot always requires review")
}

func TestLowRiskAutoMergeKeepsNoCIHeadManual(t *testing.T) {
	gate, gh := lowRiskGateFixture(t, lowRiskTestRepo)
	gh.checks, gh.noChecks = nil, true
	gate.ReviewLevel = lowRiskLevelAt(reviewlevel.LevelNoReview, "head123", nil)
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	now := start
	gate.Clock = func() time.Time { return now }
	if _, err := gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo)); err != nil {
		t.Fatalf("Evaluate first zero observation: %v", err)
	}
	now = start.Add(time.Hour)
	decision, err := gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo))
	if err != nil {
		t.Fatalf("Evaluate after grace: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 || hasStatus(gh.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("decision = %+v statuses = %+v; a no-CI low-risk head must not merge or get synthetic CI", decision, gh.statuses)
	}
	if !strings.Contains(decision.Reason.Render(), "low-risk auto-merge requires real green CI") {
		t.Fatalf("reason = %q, want the no-CI low-risk condition named", decision.Reason.Render())
	}
}

func TestNonLowRiskRepoMergesWithoutReviewLevel(t *testing.T) {
	gate, gh := lowRiskGateFixture(t, lowRiskTestRepo)
	gate.LowRiskOnly = false
	decision, err := gate.Evaluate(context.Background(), lowRiskRequest(lowRiskTestRepo))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.Merged || decision.ReviewLevel != "" || len(gh.merges) != 1 {
		t.Fatalf("decision = %+v; a plain auto_merge = true repo must merge unchanged", decision)
	}
}
