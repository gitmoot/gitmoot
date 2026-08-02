package workflow

import (
	"context"
	"testing"
)

func TestPolicyMergeGateUnknownDraftLeavesOpenBeforeForgeAccess(t *testing.T) {
	gh := &fakeMergeGateGitHub{}
	gate := PolicyMergeGate{AutoMerge: true, GitHub: gh}

	decision, err := gate.Evaluate(context.Background(), MergeRequest{
		Repo:                    "owner/repo",
		PullRequest:             17,
		PullRequestDraftUnknown: true,
	})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || decision.Reason != "pull request draft state is unknown" {
		t.Fatalf("decision = %+v, want unknown-draft leave-open", decision)
	}
	if gh.getCalls != 0 {
		t.Fatalf("GetPullRequest calls = %d, want 0 while draft state is unknown", gh.getCalls)
	}
}
