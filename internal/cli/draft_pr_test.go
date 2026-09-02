package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/githubtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type draftRecordingGitHub struct {
	githubtest.NoopClient
	input github.CreatePullRequestInput
	pr    github.PullRequest
}

func (c *draftRecordingGitHub) EnsurePullRequest(_ context.Context, input github.CreatePullRequestInput) (github.PullRequest, error) {
	c.input = input
	c.pr = github.PullRequest{
		Number: 21, URL: "https://github.com/owner/repo/pull/21", State: "open",
		HeadRef: input.Head, BaseRef: input.Base, Draft: input.Draft,
	}
	return c.pr, nil
}

func TestAgentRunPullRequestModeDefaultsToDraftWithReadyOptOut(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantReady bool
	}{
		{name: "default draft", args: []string{"builder", "implement the change"}},
		{name: "explicit draft", args: []string{"builder", "implement the change", "--draft"}},
		{name: "explicit ready", args: []string{"builder", "implement the change", "--ready"}, wantReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			options, ok := parseAgentRunOptions("run", tc.args, &stderr)
			if !ok {
				t.Fatalf("parseAgentRunOptions failed: %s", stderr.String())
			}
			if options.pullRequestReady != tc.wantReady {
				t.Fatalf("pullRequestReady = %v, want %v", options.pullRequestReady, tc.wantReady)
			}
			request := localAgentDispatchRequestFromOptions(options, "implement", "test", "agent_run")
			if request.PullRequestReady != tc.wantReady {
				t.Fatalf("dispatch request PullRequestReady = %v, want %v", request.PullRequestReady, tc.wantReady)
			}
		})
	}
	var stderr bytes.Buffer
	if _, ok := parseAgentRunOptions("run", []string{"builder", "implement", "--draft", "--ready"}, &stderr); ok {
		t.Fatal("parseAgentRunOptions accepted mutually exclusive --draft and --ready")
	}
}

func TestImplementationFinalizerOpensForgePullRequestInRequestedMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantDraft bool
	}{
		{name: "default dispatch opens draft", args: []string{"builder", "implement the change"}, wantDraft: true},
		{name: "explicit draft opens draft", args: []string{"builder", "implement the change", "--draft"}, wantDraft: true},
		{name: "ready opt-out opens ready", args: []string{"builder", "implement the change", "--ready"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			options, ok := parseAgentRunOptions("run", tc.args, &stderr)
			if !ok {
				t.Fatalf("parseAgentRunOptions failed: %s", stderr.String())
			}
			request := localAgentDispatchRequestFromOptions(options, "implement", "test", "agent_run")
			fixture := newFinalizerFixPassFixture(t)
			writeFile(t, filepath.Join(fixture.checkout, "feature.txt"), tc.name+"\n")
			gh := &draftRecordingGitHub{}
			payload := workflow.JobPayload{
				Repo: "owner/repo", Branch: fixture.task.Branch, TaskID: fixture.task.ID,
				TaskTitle: fixture.task.Title, LeadAgent: "lead", PullRequestReady: request.PullRequestReady,
				Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"},
			}
			finalized, err := (newHostDaemonImplementationFinalizer(fixture.store, gh)).FinalizeImplementation(
				context.Background(), db.Job{ID: "implement", Agent: "lead", Type: "implement"}, payload,
			)
			if err != nil {
				t.Fatalf("FinalizeImplementation: %v", err)
			}
			if gh.pr.Draft != tc.wantDraft {
				t.Fatalf("forge PR draft = %v, want %v; create input = %+v", gh.pr.Draft, tc.wantDraft, gh.input)
			}
			if finalized.PullRequestDraft != gh.pr.Draft {
				t.Fatalf("finalized forge state = %v, want %v", finalized.PullRequestDraft, gh.pr.Draft)
			}
		})
	}
}
