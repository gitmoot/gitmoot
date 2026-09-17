package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// #1519. The merge gate correlates a PR's implement and review rows by TASK
// IDENTITY, but that identity MIGRATES between review rounds: on PR #1518 the
// implement jobs and round-1 reviews carried `adhoc-64daeee4` while round-2
// reviews were issued `review-pr-1518-3f3a1026`, with repo, pull request and
// branch identical throughout. Once the sides diverge nothing can match again,
// so the gate false-blocks for the rest of the PR's life.
//
// Independence is a property of AGENTS - did someone other than the implementer
// approve this commit - and the agents were distinct the whole time. Task
// identity is only a correlation key, so its divergence must not be reported as
// an independence failure.
//
// BOTH call sites in Evaluate's path share that key, which is why a fix confined
// to one of them leaves the defect reachable from the other:
//
//   - the review-collection loop drops rows the key rejects before reviewsAtHead
//     is built, so a migrated ROUND is invisible to quorum;
//   - collectImplementerAttribution drops implement rows the key rejects, which
//     is the direction #1518's table records.
//
// Which side is dropped depends on which identity the MergeRequest carries, so
// the two tests below drive one direction each. Neither asserts anything about
// how identities come to diverge; that is the dispatcher's behaviour and #1519
// leaves it open deliberately.

const (
	migratedImplementTaskID = "adhoc-64daeee4"
	migratedReviewTaskID    = "review-pr-9-3f3a1026"
)

// mergeGateMigratedScenario builds #1518's shape: one implement row and one
// approving review row whose task identities differ while repo, PR and branch
// agree, with distinct agents. requestTaskID selects which side the gate's own
// `current` payload matches, i.e. which side the shared key discards.
// implementPullRequest exists so a caller can place the implement row on a
// DIFFERENT pull request, which is the only way to leave this PR with no
// attribution row at all.
func mergeGateMigratedScenario(t *testing.T, implementTaskID, reviewTaskID, requestTaskID string,
	implementAgent, reviewAgent string, implementPullRequest int) (*db.Store, PolicyMergeGate, MergeRequest) {
	t.Helper()
	store := openEngineStore(t)
	implementBranch := "task-9"
	if implementPullRequest != 9 {
		implementBranch = "other-branch"
	}
	insertCompletedJob(t, store, db.Job{ID: "implement-migrated", Agent: implementAgent, Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: implementPullRequest,
		Branch:      implementBranch,
		TaskID:      implementTaskID,
		Result:      &AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	insertCompletedJob(t, store, db.Job{ID: "review-migrated", Agent: reviewAgent, Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		Branch:      "task-9",
		HeadSHA:     "head123",
		TaskID:      reviewTaskID,
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "approved"},
	})
	mergeable := true
	gate := PolicyMergeGate{AutoMerge: true, Store: store, Git: &fakeMergeGateGit{clean: true}, GitHub: &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}}
	return store, gate, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: requestTaskID}
}

// mergeGateMigratedBranchScenario is mergeGateMigratedScenario with the stored
// implement row's branch and the REQUEST's branch under the caller's control, so
// the conditional branch comparison in sameCorrelatedTask can actually execute.
// #1929's review found that comparison unreachable from every other test here:
// MergeRequest carried no Branch, so deleting the guard left the suite green. A
// measurement (322 pull requests with branches on both sides, zero disagreeing)
// justified the conditional SHAPE; it could not pin it. These three cases do.
func mergeGateMigratedBranchScenario(t *testing.T, implementBranch, requestBranch string) (PolicyMergeGate, MergeRequest) {
	t.Helper()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "implement-branch", Agent: "wave-impl", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		Branch:      implementBranch,
		TaskID:      migratedImplementTaskID,
		Result:      &AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	insertCompletedJob(t, store, db.Job{ID: "review-branch", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		Branch:      "task-9",
		HeadSHA:     "head123",
		TaskID:      migratedReviewTaskID,
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "approved"},
	})
	mergeable := true
	gate := PolicyMergeGate{AutoMerge: true, Store: store, Git: &fakeMergeGateGit{clean: true}, GitHub: &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}}
	return gate, MergeRequest{
		Repo:        "gitmoot/gitmoot",
		Branch:      requestBranch,
		PullRequest: 9,
		TaskID:      migratedReviewTaskID,
	}
}

// Equal branches on both sides: the divergent identity still correlates.
func TestPolicyMergeGateCorrelatesDivergentIdentityWhenBranchesAgree(t *testing.T) {
	gate, request := mergeGateMigratedBranchScenario(t, "task-9", "task-9")

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, reason = %q; want the merge admitted: repo, PR and branch all agree", decision, decision.Reason.Render())
	}
}

// DIFFERING branches refuse to correlate, so the implement row cannot attribute
// this pull request. This is the case that fails if the guard is deleted.
func TestPolicyMergeGateRefusesCorrelationWhenBranchesDiffer(t *testing.T) {
	gate, request := mergeGateMigratedBranchScenario(t, "task-9-other", "task-9")

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Merged || !decision.LeaveOpen {
		t.Fatalf("decision = %+v; want the merge refused: the only implement row records branch task-9-other, not task-9", decision)
	}
}

// One side missing a branch admits: a stored row is not required to carry one,
// and treating absence as a mismatch would re-block exactly the rows #1519
// exists to admit.
func TestPolicyMergeGateCorrelatesWhenOnlyOneSideRecordsABranch(t *testing.T) {
	gate, request := mergeGateMigratedBranchScenario(t, "", "task-9")

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, reason = %q; want the merge admitted: an absent branch is no evidence, not a mismatch", decision, decision.Reason.Render())
	}
}

// The exact #1518 repro: the request carries the MIGRATED REVIEW identity, so
// the implement row is the side discarded and the gate reports the
// stable-task-identity anomaly against an approval that was genuinely
// independent.
func TestPolicyMergeGateAdmitsIndependenceWhenImplementTaskIdentityDiverges(t *testing.T) {
	_, gate, request := mergeGateMigratedScenario(t,
		migratedImplementTaskID, migratedReviewTaskID, migratedReviewTaskID, "wave-impl", "g7-review", 9)

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, reason = %q; want the merge admitted: repo, PR and branch agree and the implementer (wave-impl) is not the reviewer (g7-review), so a divergent task identity is a correlation-key difference rather than an independence failure",
			decision, decision.Reason.Render())
	}
}

// The direction #1519's report did not measure: the request carries the ORIGINAL
// implement identity, so the migrated round-2 REVIEW row is the side discarded -
// before reviewsAtHead exists - and the gate cannot see the approval at all.
func TestPolicyMergeGateSeesReviewRoundWhoseTaskIdentityDiverges(t *testing.T) {
	_, gate, request := mergeGateMigratedScenario(t,
		migratedImplementTaskID, migratedReviewTaskID, migratedImplementTaskID, "wave-impl", "g7-review", 9)

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, reason = %q; want the migrated review round visible to the gate: the approving row carries this PR's repo, number and branch",
			decision, decision.Reason.Render())
	}
}

// A REAL hollow approval must still block after the fix. Same divergence, but
// the approver IS the implementer, so the positional correlation now succeeds and
// the self-approval refusal is what has to fire.
func TestPolicyMergeGateStillBlocksSelfApprovalAcrossDivergentTaskIdentities(t *testing.T) {
	_, gate, request := mergeGateMigratedScenario(t,
		migratedImplementTaskID, migratedReviewTaskID, migratedReviewTaskID, "wave-impl", "wave-impl", 9)

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Merged || !decision.LeaveOpen {
		t.Fatalf("decision = %+v; want the merge refused: the only approval is the implementer's own", decision)
	}
	if reason := decision.Reason.Render(); !strings.Contains(reason, "implement") {
		t.Fatalf("reason = %q; want it to name the implementer/approver collision", reason)
	}
}

// Cross-PR rows must never correlate, whatever their task identities: the
// positional fallback is keyed on repo AND pull request, so a row from another
// PR cannot supply attribution here.
