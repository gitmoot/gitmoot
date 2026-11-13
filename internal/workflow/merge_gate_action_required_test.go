package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// #2605: themartianapp/herdrup PR #673 was blocked by the on-demand check run
// mac-evidence/org-ios-sim-script-v1/ipad-script, whose conclusion was
// action_required although main's branch protection did not require it.
const actionRequiredShard = "mac-evidence/org-ios-sim-script-v1/ipad-script"

func actionRequiredGateClient(checks ...github.PullRequestCheck) *fakeMergeGateGitHub {
	mergeable := true
	return &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success"},
		checks:      checks,
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
}

func evaluateActionRequiredGate(t *testing.T, gh *fakeMergeGateGitHub) MergeDecision {
	t.Helper()
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir()}
	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	return decision
}

func TestMergeGateIgnoresNonRequiredActionRequiredCheck(t *testing.T) {
	gh := actionRequiredGateClient(
		github.PullRequestCheck{Name: "HerdrKit and SwiftTerm (Linux)", State: "success"},
		github.PullRequestCheck{Name: actionRequiredShard, State: "ACTION_REQUIRED"},
	)
	gh.requiredKnown = true
	gh.requiredContexts = []string{"HerdrKit and SwiftTerm (Linux)", "mac-evidence/org-ios-sim-build-v1"}

	decision := evaluateActionRequiredGate(t, gh)

	if strings.Contains(decision.Reason.Render(), actionRequiredShard) {
		t.Fatalf("decision = %+v, a non-required action_required check must not block", decision)
	}
	if !decision.Merged || len(gh.merges) != 1 {
		t.Fatalf("decision = %+v merges = %+v, want the gate to merge", decision, gh.merges)
	}
	if gh.requiredCalls != 1 || len(gh.requiredBranches) != 1 || gh.requiredBranches[0] != "main" {
		t.Fatalf("protection lookups = %d %v, want exactly one for base main", gh.requiredCalls, gh.requiredBranches)
	}
}

func TestMergeGateBlocksRequiredActionRequiredCheck(t *testing.T) {
	gh := actionRequiredGateClient(
		github.PullRequestCheck{Name: "HerdrKit and SwiftTerm (Linux)", State: "success"},
		github.PullRequestCheck{Name: actionRequiredShard, State: "action_required"},
	)
	gh.requiredKnown = true
	gh.requiredContexts = []string{"HerdrKit and SwiftTerm (Linux)", actionRequiredShard}

	decision := evaluateActionRequiredGate(t, gh)

	if decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), actionRequiredShard) {
		t.Fatalf("decision = %+v, want a block on the required action_required check", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestMergeGateBlocksActionRequiredCheckWhenProtectionUnknown(t *testing.T) {
	gh := actionRequiredGateClient(
		github.PullRequestCheck{Name: "HerdrKit and SwiftTerm (Linux)", State: "success"},
		github.PullRequestCheck{Name: actionRequiredShard, State: "action_required"},
	)
	gh.requiredKnown = false

	decision := evaluateActionRequiredGate(t, gh)

	if decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), actionRequiredShard) {
		t.Fatalf("decision = %+v, want a fail-closed block when protection is unknown", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestMergeGateBlocksFailedNonRequiredCheck(t *testing.T) {
	gh := actionRequiredGateClient(
		github.PullRequestCheck{Name: "HerdrKit and SwiftTerm (Linux)", State: "success"},
		github.PullRequestCheck{Name: actionRequiredShard, State: "action_required"},
		github.PullRequestCheck{Name: "optional-lint", State: "failure"},
	)
	gh.requiredKnown = true
	gh.requiredContexts = []string{"HerdrKit and SwiftTerm (Linux)"}

	decision := evaluateActionRequiredGate(t, gh)

	if decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), "optional-lint") {
		t.Fatalf("decision = %+v, want a failed non-required check to block", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestMergeGateSkipsProtectionLookupWithoutActionRequiredCheck(t *testing.T) {
	gh := actionRequiredGateClient(
		github.PullRequestCheck{Name: "HerdrKit and SwiftTerm (Linux)", State: "success"},
		github.PullRequestCheck{Name: "optional-lint", State: "neutral"},
	)
	gh.requiredKnown = true

	decision := evaluateActionRequiredGate(t, gh)

	if !decision.Merged {
		t.Fatalf("decision = %+v, want the passing head to merge", decision)
	}
	if gh.requiredCalls != 0 {
		t.Fatalf("protection lookups = %d, want none without an action_required check", gh.requiredCalls)
	}
}
