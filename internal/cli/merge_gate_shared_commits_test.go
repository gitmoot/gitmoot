package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// The #2755 incident through the daemon's native low-risk gate: PR #17 of
// owner/repo is approved at its head and classified level 1, and carries the
// commit PR #16's review rejected.
func TestDaemonLowRiskGateHoldsPullRequestCarryingAnotherPullRequestsChangesRequestedCommit(t *testing.T) {
	const rejected = "3d1ea615b599f7d1e6b75341bf13459e1f80d629"
	setup := func(t *testing.T) (*db.Store, string, *activeJobMergeGateGitHub, workflow.MergeRequest, string) {
		t.Helper()
		store, checkout, gh, request := daemonMergeGateActiveJobFixture(t)
		home := daemonMergeGateLiveOrgHome(t)
		configFile := filepath.Join(home, "config.toml")
		raw, err := os.ReadFile(configFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configFile, append(raw, []byte("\n[repos.\"owner/repo\".merge_gate]\nauto_merge = \"low_risk\"\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := appendReviewLevelLog(rawHomeFromResolved(home), "owner/repo", 17,
			reviewlevel.Decision{HeadSHA: "head123", Level: reviewlevel.LevelNoReview, Source: "jev"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		t.Setenv(reviewLevelAPIKeyName, "")
		gh.commits = []string{rejected, "head123"}
		seedSharedCommitVerdict(t, store, "review-pr16-rejected", rejected, "changes_requested", "2026-11-25 10:21:03")
		return store, checkout, gh, request, home
	}
	evaluate := func(t *testing.T, store *db.Store, checkout string, gh *activeJobMergeGateGitHub, request workflow.MergeRequest, home string) workflow.MergeDecision {
		t.Helper()
		decision, err := newDaemonMergeGate(store, gh, checkout, home, subprocess.ExecRunner{}).Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		return decision
	}

	t.Run("held naming the other pull request, its job and the commit", func(t *testing.T) {
		store, checkout, gh, request, home := setup(t)
		decision := evaluate(t, store, checkout, gh, request, home)
		reason := decision.Reason.Render()
		if decision.Merged || !decision.LeaveOpen || len(gh.mergeInputs) != 0 {
			t.Fatalf("decision = %+v (reason %q) merges = %+v; want the low-risk gate to hold", decision, reason, gh.mergeInputs)
		}
		for _, want := range []string{workflow.SharedCommitReviewGate, "PR #16", "review-pr16-rejected", rejected} {
			if !strings.Contains(reason, want) {
				t.Fatalf("reason %q does not name %q", reason, want)
			}
		}
	})
	t.Run("merges once PR #16 is approved at that commit", func(t *testing.T) {
		store, checkout, gh, request, home := setup(t)
		seedSharedCommitVerdict(t, store, "review-pr16-approved", rejected, "approved", "2026-11-25 10:40:00")
		decision := evaluate(t, store, checkout, gh, request, home)
		if !decision.Merged || decision.ReviewLevel != reviewlevel.LevelNoReview || len(gh.mergeInputs) != 1 {
			t.Fatalf("decision = %+v (reason %q) merges = %+v; want a low-risk merge", decision, decision.Reason.Render(), gh.mergeInputs)
		}
	})
	t.Run("held when the commit list cannot be read", func(t *testing.T) {
		store, checkout, gh, request, home := setup(t)
		gh.commitsErr = errors.New("github: 502 bad gateway")
		decision := evaluate(t, store, checkout, gh, request, home)
		if decision.Merged || len(gh.mergeInputs) != 0 || !strings.Contains(decision.Reason.Render(), "cannot list the commits of pull request #17") {
			t.Fatalf("decision = %+v (reason %q); want a held merge naming the listing failure", decision, decision.Reason.Render())
		}
	})
}

func seedSharedCommitVerdict(t *testing.T, store *db.Store, id, head, decision, recorded string) {
	t.Helper()
	seedDaemonMergeGateJob(t, store, db.Job{ID: id, Agent: "reviewer-" + id, Type: "review", State: string(workflow.JobSucceeded)}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "other-fix", PullRequest: 16, HeadSHA: head, TaskID: "task-1016",
		ReviewRound: "review-1", Result: &workflow.AgentResult{Decision: decision, Summary: "fixture verdict"},
	})
	if err := store.ExecForTest(context.Background(), `UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`, recorded, recorded, id); err != nil {
		t.Fatal(err)
	}
}
