package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

func TestPolicyMergeGateMergesPassingPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:    9,
			Title:     "Task 9",
			State:     "open",
			URL:       "https://github.com/jerryfane/gitmoot/pull/9",
			HeadRef:   "task-9",
			BaseRef:   "main",
			HeadSHA:   "head123",
			Mergeable: &mergeable,
		},
		status: github.CombinedStatus{
			State: "success",
			Statuses: []github.CommitStatus{
				{Context: gitmootMergeGateContext, State: "failure"},
			},
		},
		checks:      []github.PullRequestCheck{{Name: gitmootMergeGateContext, Bucket: "fail", State: "FAILURE"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	git := &fakeMergeGateGit{clean: true}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: git, DeleteBranch: true}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || decision.MergeCommitSHA != "merge123" {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 1 || gh.merges[0].Method != "squash" || gh.merges[0].MatchHeadCommit != "head123" || !gh.merges[0].DeleteBranch {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, gitmootNoCIContext, "success") || !hasStatus(gh.statuses, gitmootMergeGateContext, "success") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
	if _, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-9"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("branch lock after merge error = %v, want sql.ErrNoRows", err)
	}
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 9)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.State != "merged" || pr.MergeCommitSHA != "merge123" {
		t.Fatalf("stored pull request = %+v", pr)
	}
	if len(git.updated) != 1 || git.updated[0] != "origin/main" {
		t.Fatalf("updated base calls = %+v", git.updated)
	}
}

func TestPolicyMergeGateDoesNotRecordPreMergeSyntheticSHA(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:    9,
			State:     "open",
			URL:       "https://github.com/jerryfane/gitmoot/pull/9",
			HeadRef:   "task-9",
			BaseRef:   "main",
			HeadSHA:   "head123",
			MergeSHA:  "synthetic-premerge-sha",
			Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success"},
		mergeResult: github.MergeResult{Merged: true},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || decision.MergeCommitSHA != "" {
		t.Fatalf("decision = %+v", decision)
	}
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 9)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.MergeCommitSHA != "" {
		t.Fatalf("stored pull request merge SHA = %q, want empty", pr.MergeCommitSHA)
	}
}

func TestPolicyMergeGateBlocksDirtyWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	gh := &fakeMergeGateGitHub{pr: github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123"}}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: false}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "worktree") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestPolicyMergeGateBlocksFailedExternalCI(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{
			State: "success",
			Statuses: []github.CommitStatus{
				{Context: "gitmoot/review", State: "success"},
			},
		},
		checks: []github.PullRequestCheck{{Name: "ci", Bucket: "fail", State: "COMPLETED"}},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "external CI") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestPolicyMergeGateTruncatesLongStatusDescriptions(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success"},
		checks: []github.PullRequestCheck{{
			Name:   "ci-" + strings.Repeat("very-long-check-name-", 12),
			Bucket: "fail",
			State:  "COMPLETED",
		}},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.statuses) != 1 {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
	if got := len([]rune(gh.statuses[0].Description)); got > 140 {
		t.Fatalf("status description length = %d, want <= 140: %q", got, gh.statuses[0].Description)
	}
}

func TestPolicyMergeGateAllowsSkippedExternalCI(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success"},
		checks:      []github.PullRequestCheck{{Name: "conditional", Bucket: "skipping", State: "SKIPPED"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPolicyMergeGateBlocksStaleBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:      github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:  github.CombinedStatus{State: "success"},
		compare: github.CompareResult{Status: "behind", BehindBy: 1},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "behind base") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "failure") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateKeepsPendingCIReadyToRetry(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success"},
		checks: []github.PullRequestCheck{{Name: "ci", Bucket: "pending", State: "IN_PROGRESS"}},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason, "pending") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "pending") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateKeepsQueuedMergePending(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success"},
		mergeResult: github.MergeResult{Message: "pull request merge is pending"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason, "pending") {
		t.Fatalf("decision = %+v", decision)
	}
	if _, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-9"); err != nil {
		t.Fatalf("branch lock after queued merge error = %v", err)
	}
	if _, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 9); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPullRequest after queued merge error = %v, want sql.ErrNoRows", err)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "pending") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateAllowsReviewOptionalPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success"},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9", ReviewOptional: true})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 1 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestPolicyMergeGateRecordsAlreadyMergedPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:   9,
			State:    "closed",
			Merged:   true,
			URL:      "https://github.com/jerryfane/gitmoot/pull/9",
			HeadRef:  "task-9",
			BaseRef:  "main",
			HeadSHA:  "head123",
			MergeSHA: "merge123",
		},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: false}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || decision.MergeCommitSHA != "merge123" {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if _, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-9"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("branch lock after merge error = %v, want sql.ErrNoRows", err)
	}
}

func TestPolicyMergeGateBlocksClosedUnmergedPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:  9,
			State:   "closed",
			Merged:  false,
			HeadRef: "task-9",
			BaseRef: "main",
			HeadSHA: "head123",
		},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "closed") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "failure") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateUsesLatestNumericReviewRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-nine", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		HeadSHA:     "old123",
		TaskID:      "task-9",
		ReviewRound: "review-9",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "old change"},
	})
	insertCompletedJob(t, store, db.Job{ID: "review-ten", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-10",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success"},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPolicyMergeGateBlocksReviewForStaleHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		HeadSHA:     "old123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "different head SHA") {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPolicyMergeGateBlocksLegacyReviewWithoutHeadSHA(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "jerryfane/gitmoot",
		Number:       9,
		URL:          "https://github.com/jerryfane/gitmoot/pull/9",
		HeadBranch:   "task-9",
		BaseBranch:   "main",
		HeadSHA:      "head123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		PullRequest: 9,
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success"},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "does not record a head SHA") {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPolicyMergeGateBlocksMissingFinalReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "gitmoot/review", State: "success"}}},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "jerryfane/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "review") {
		t.Fatalf("decision = %+v", decision)
	}
}

type fakeMergeGateGitHub struct {
	pr          github.PullRequest
	status      github.CombinedStatus
	compare     github.CompareResult
	checks      []github.PullRequestCheck
	mergeResult github.MergeResult
	statuses    []github.CommitStatusInput
	merges      []github.MergePullRequestInput
}

func (f *fakeMergeGateGitHub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	return f.pr, nil
}

func (f *fakeMergeGateGitHub) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	return f.status, nil
}

func (f *fakeMergeGateGitHub) CompareCommits(context.Context, github.Repository, string, string) (github.CompareResult, error) {
	if f.compare.Status == "" && f.compare.AheadBy == 0 && f.compare.BehindBy == 0 {
		return github.CompareResult{Status: "ahead", AheadBy: 1}, nil
	}
	return f.compare, nil
}

func (f *fakeMergeGateGitHub) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	return f.checks, nil
}

func (f *fakeMergeGateGitHub) CreateCommitStatus(_ context.Context, input github.CommitStatusInput) (github.CommitStatus, error) {
	f.statuses = append(f.statuses, input)
	return github.CommitStatus{State: input.State, Context: input.Context}, nil
}

func (f *fakeMergeGateGitHub) MergePullRequest(_ context.Context, input github.MergePullRequestInput) (github.MergeResult, error) {
	f.merges = append(f.merges, input)
	return f.mergeResult, nil
}

type fakeMergeGateGit struct {
	clean   bool
	updated []string
}

func (f *fakeMergeGateGit) WorktreeClean(context.Context) (bool, error) {
	return f.clean, nil
}

func (f *fakeMergeGateGit) UpdateBase(_ context.Context, remote string, branch string) error {
	f.updated = append(f.updated, remote+"/"+branch)
	return nil
}

func hasStatus(statuses []github.CommitStatusInput, context string, state string) bool {
	for _, status := range statuses {
		if status.Context == context && status.State == state {
			return true
		}
	}
	return false
}
