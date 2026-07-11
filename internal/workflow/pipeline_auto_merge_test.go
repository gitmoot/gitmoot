package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/github"
)

func TestPipelineAutoMergerEvaluateCheckStates(t *testing.T) {
	mergeable := true
	tests := []struct {
		name              string
		checks            []github.PullRequestCheck
		requireExternalCI bool
		wantReady         bool
		wantWaiting       bool
		wantBlocked       bool
		wantReason        string
	}{
		{name: "pending waits", checks: []github.PullRequestCheck{{Name: "ci", Bucket: "pending", State: "IN_PROGRESS"}}, wantWaiting: true, wantReason: "pending"},
		{name: "skipped passes", checks: []github.PullRequestCheck{{Name: "ci", Bucket: "skipping", State: "SKIPPED"}}, wantReady: true},
		{name: "neutral passes", checks: []github.PullRequestCheck{{Name: "ci", State: "neutral"}}, wantReady: true},
		{name: "failure blocks", checks: []github.PullRequestCheck{{Name: "ci", Bucket: "fail", State: "FAILURE"}}, wantBlocked: true, wantReason: "not successful"},
		{name: "zero checks blocks policy off", wantBlocked: true, wantReason: "zero external checks"},
		{name: "zero checks blocks policy on", requireExternalCI: true, wantBlocked: true, wantReason: "zero external checks"},
		{name: "green passes", checks: []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}}, wantReady: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := openEngineStore(t)
			gh := &fakeMergeGateGitHub{
				pr:     github.PullRequest{Number: 813, State: "open", HeadSHA: "reviewed-head", Mergeable: &mergeable},
				checks: tc.checks,
			}
			merger := PipelineAutoMerger{Store: store, GitHub: gh, RequireExternalCI: tc.requireExternalCI}
			got, err := merger.Evaluate(context.Background(), PipelineAutoMergeRequest{Repo: "owner/repo", PullRequest: 813, HeadSHA: "reviewed-head"})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got.Ready != tc.wantReady || got.Waiting != tc.wantWaiting || got.Blocked != tc.wantBlocked {
				t.Fatalf("readiness = %+v, want ready=%v waiting=%v blocked=%v", got, tc.wantReady, tc.wantWaiting, tc.wantBlocked)
			}
			if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want substring %q", got.Reason, tc.wantReason)
			}
			if len(gh.statuses) != 0 {
				t.Fatalf("pipeline evaluator synthesized commit statuses: %+v", gh.statuses)
			}
		})
	}
}
