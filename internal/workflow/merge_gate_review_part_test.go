package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// #2372 review of #2373 (P1): a part of a split review covers a slice of the
// head, so it must never authorize a merge, and a later approval of one part
// must not retire the same reviewer's objection on another part.

func partGateFixture(t *testing.T) (*db.Store, *fakeMergeGateGitHub, PolicyMergeGate, JobPayload) {
	t.Helper()
	store := openEngineStore(t)
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}
	base := JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9"}
	return store, gh, gate, base
}

func TestPolicyMergeGateASinglePartApprovalDoesNotMerge(t *testing.T) {
	ctx := context.Background()
	store, _, gate, base := partGateFixture(t)
	part := base
	part.ReviewPart = "mm-1-mm-3"
	part.Result = &AgentResult{Decision: "approved", Summary: "this slice is fine"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-part", Agent: "reviewer", Type: "review"}, part)

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged {
		t.Fatalf("decision = %+v: one approved part of a split review merged the PR", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "only parts of a split review approved") {
		t.Fatalf("reason = %q, want the part-only refusal", decision.Reason.Render())
	}
}

func TestPolicyMergeGateWholeHeadApprovalStillMergesBesidePartApprovals(t *testing.T) {
	ctx := context.Background()
	store, _, gate, base := partGateFixture(t)
	part := base
	part.ReviewPart = "cross"
	part.Result = &AgentResult{Decision: "approved", Summary: "slice fine"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-part", Agent: "part-reviewer", Type: "review"}, part)
	setMergeGateJobTimestamps(t, store, "review-part", "2026-07-31 12:00:00")
	whole := base
	whole.Result = &AgentResult{Decision: "approved", Summary: "whole head reviewed"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-whole", Agent: "reviewer", Type: "review"}, whole)
	setMergeGateJobTimestamps(t, store, "review-whole", "2026-07-31 12:01:00")

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, want a whole-head approval to merge", decision)
	}
}

func TestPolicyMergeGateLaterPartApprovalDoesNotRetireAnotherPartsObjection(t *testing.T) {
	ctx := context.Background()
	store, _, gate, base := partGateFixture(t)
	objection := base
	objection.ReviewPart = "mm-1-mm-3"
	objection.Result = &AgentResult{Decision: "changes_requested", Summary: "bug in slice A"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-a", Agent: "reviewer", Type: "review"}, objection)
	setMergeGateJobTimestamps(t, store, "review-a", "2026-07-31 12:00:00")
	approval := base
	approval.ReviewPart = "mm-4-mm-6"
	approval.Result = &AgentResult{Decision: "approved", Summary: "slice B fine"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-b", Agent: "reviewer", Type: "review"}, approval)
	setMergeGateJobTimestamps(t, store, "review-b", "2026-07-31 12:01:00")
	whole := base
	whole.Result = &AgentResult{Decision: "approved", Summary: "whole head"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-whole", Agent: "other", Type: "review"}, whole)
	setMergeGateJobTimestamps(t, store, "review-whole", "2026-07-31 12:02:00")

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged {
		t.Fatalf("decision = %+v: slice B's approval retired the same reviewer's objection on slice A", decision)
	}
}
