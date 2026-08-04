package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestStableReviewTaskIdentityLetsSecondRoundVerdictReachGate(t *testing.T) {
	repoDir, oldHead, newHead := c1ReviewRepository(t)
	firstTaskID := mintC1ReviewTaskID(t, repoDir, oldHead)
	secondTaskID := mintC1ReviewTaskID(t, repoDir, newHead)

	decision := evaluateC1GateScenario(t, firstTaskID, secondTaskID, oldHead, newHead, firstTaskID, "reviewer", true)
	if !decision.Merged {
		t.Fatalf("second-round verdict did not reach first-round task: first=%q second=%q decision=%+v", firstTaskID, secondTaskID, decision)
	}
}

func TestStableReviewTaskIdentityKeepsHeadRoundIsolation(t *testing.T) {
	const stableTaskID = "review-pr-17-stable"
	decision := evaluateC1GateScenario(t, stableTaskID, stableTaskID, "old-head", "new-head", stableTaskID, "reviewer", false)
	if !decision.LeaveOpen || decision.Merged || !strings.Contains(decision.Reason.Render(), "different head SHA") {
		t.Fatalf("wrong-head verdict was not rejected by the head comparison: %+v", decision)
	}
}

func TestC1MeasurementEngineDispatchedImplementerIdentity(t *testing.T) {
	repoDir, oldHead, newHead := c1ReviewRepository(t)
	if oldHead == newHead {
		t.Fatalf("measurement requires different heads, both were %q", oldHead)
	}
	stableFirst := mintC1ReviewTaskID(t, repoDir, oldHead)
	stableSecond := mintC1ReviewTaskID(t, repoDir, newHead)
	legacyFirst := fmt.Sprintf("review-pr-%d-%s", 17, shortHash("owner/repo\x00"+oldHead))
	legacySecond := fmt.Sprintf("review-pr-%d-%s", 17, shortHash("owner/repo\x00"+newHead))

	for _, tc := range []struct {
		name            string
		implementTaskID string
		reviewTaskID    string
		wantAgents      int
		wantFailClosed  bool
	}{
		{name: "before_C1", implementTaskID: legacyFirst, reviewTaskID: legacySecond, wantAgents: 0, wantFailClosed: true},
		{name: "after_C1", implementTaskID: stableFirst, reviewTaskID: stableSecond, wantAgents: 1, wantFailClosed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := evaluateC1GateScenario(t, tc.implementTaskID, tc.reviewTaskID, oldHead, newHead, tc.reviewTaskID, "reviewer", true)
			implementingAgents, failClosed := observedC1ImplementerResolution(t, decision)
			if implementingAgents != tc.wantAgents || failClosed != tc.wantFailClosed {
				t.Fatalf("implementingAgents=%d failClosed=%v decision=%+v; want agents=%d failClosed=%v", implementingAgents, failClosed, decision, tc.wantAgents, tc.wantFailClosed)
			}
			if tc.wantFailClosed && decision.Merged {
				t.Fatalf("unknown implementer passed independence gate: %+v", decision)
			}
			if !tc.wantFailClosed && !decision.Merged {
				t.Fatalf("independent review did not merge after implementer resolution: %+v", decision)
			}
			t.Logf("old_head=%s new_head=%s implementingAgents=%d independence_fail_closed=%v", oldHead, newHead, implementingAgents, failClosed)
		})
	}
}

func observedC1ImplementerResolution(t *testing.T, decision workflow.MergeDecision) (int, bool) {
	t.Helper()
	switch {
	case decision.Ready && decision.Merged && !decision.LeaveOpen && !decision.Reason.IsGateMiss() && !decision.Deferred:
		return 1, false
	case !decision.Ready && !decision.Merged && decision.LeaveOpen && decision.Reason.IsGateMiss() && !decision.Deferred:
		return 0, true
	default:
		t.Fatalf("gate decision did not expose a structured implementer-resolution outcome: %+v", decision)
		return 0, false
	}
}

func c1ReviewRepository(t *testing.T) (repoDir string, oldHead string, newHead string) {
	t.Helper()
	ctx := context.Background()
	repoDir = t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("round one\n"), 0o644); err != nil {
		t.Fatalf("WriteFile round one: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "round one")
	var err error
	oldHead, err = (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil || oldHead == "" {
		t.Fatalf("round-one head = %q, err=%v", oldHead, err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("round two\n"), 0o644); err != nil {
		t.Fatalf("WriteFile round two: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "round two")
	newHead, err = (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil || newHead == "" || newHead == oldHead {
		t.Fatalf("round heads old=%q new=%q err=%v", oldHead, newHead, err)
	}
	return repoDir, oldHead, newHead
}

func mintC1ReviewTaskID(t *testing.T, repoDir string, headSHA string) string {
	t.Helper()
	_ = repoDir
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() { _ = store.Close() })
	request, err := prepareLocalReviewTask(
		context.Background(),
		store,
		github.Repository{Owner: "owner", Name: "repo"},
		localAgentDispatchRequest{Home: home, PullRequest: 17, HeadSHA: headSHA},
	)
	if err != nil {
		t.Fatalf("prepareLocalReviewTask(%s): %v", headSHA, err)
	}
	return request.TaskID
}

func evaluateC1GateScenario(t *testing.T, implementTaskID string, reviewTaskID string, oldHead string, newHead string, gateTaskID string, reviewerAgent string, includeCurrentReview bool) workflow.MergeDecision {
	t.Helper()
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t, false)
	gh.pr.HeadSHA = newHead
	request.HeadSHA = newHead
	request.TaskID = gateTaskID
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "implement-round", Agent: "implementer", Type: "implement", State: string(workflow.JobSucceeded),
	}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: oldHead, TaskID: implementTaskID,
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "review-round-one", Agent: reviewerAgent, Type: "review", State: string(workflow.JobSucceeded),
	}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: oldHead, TaskID: implementTaskID,
		ReviewRound: "review-1", Result: &workflow.AgentResult{Decision: "approved", Summary: "round one approved"},
	})
	if includeCurrentReview {
		seedDaemonMergeGateJob(t, store, db.Job{
			ID: "review-round-two", Agent: reviewerAgent, Type: "review", State: string(workflow.JobSucceeded),
		}, workflow.JobPayload{
			Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: newHead, TaskID: reviewTaskID,
			ReviewRound: "review-2", Result: &workflow.AgentResult{Decision: "approved", Summary: "round two approved"},
		})
	}
	decision, err := (daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout}).Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return decision
}
