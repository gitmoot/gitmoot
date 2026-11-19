package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// TestOpenPullRequestFactsToleratesMissingWorkflowsDir pins the #2701
// follow-up: GitHub answers Commit.file(path: ".github/workflows") on a commit
// without that directory with a null field plus a NOT_FOUND error, and gh then
// exits non-zero. That repo has no workflows; it is not an unreadable repo.
// Any other GraphQL error still fails the read.
func TestOpenPullRequestFactsToleratesMissingWorkflowsDir(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}
	const data = `"data":{"rateLimit":{"cost":1},"repository":{"viewerPermission":"ADMIN",
"defaultBranchRef":{"target":{"oid":"d1","file":null}},
"pullRequests":{"totalCount":1,"nodes":[{"number":4,"headRefOid":"h1",
 "baseRef":{"branchProtectionRule":null,"rules":{"pageInfo":{"hasNextPage":false},"nodes":[]},"target":{"oid":"b1","file":null}},
 "commits":{"nodes":[{"commit":{"oid":"h1","file":null}}]}}]}}}`
	notFound := func(path string) string {
		return `{"type":"NOT_FOUND","path":[` + path + `],"message":"Could not resolve file for path '.github/workflows'."}`
	}
	ghFailed := errors.New("exit status 1")

	t.Run("missing dir on every commit reads as no workflows", func(t *testing.T) {
		body := `{` + data + `,"errors":[` +
			notFound(`"repository","defaultBranchRef","target","file"`) + `,` +
			notFound(`"repository","pullRequests","nodes",0,"baseRef","target","file"`) + `,` +
			notFound(`"repository","pullRequests","nodes",0,"commits","nodes",0,"commit","file"`) + `]}`
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: body}, {Stdout: body}, {Stdout: body}}, errs: []error{ghFailed, ghFailed, ghFailed}}
		client := GhClient{Runner: runner}
		facts, err := client.OpenPullRequestFacts(context.Background(), repo)
		if err != nil {
			t.Fatalf("OpenPullRequestFacts: %v", err)
		}
		if len(facts.PullRequests) != 1 {
			t.Fatalf("pull requests = %d, want 1", len(facts.PullRequests))
		}
		pr := facts.PullRequests[0]
		for name, dir := range map[string]WorkflowDir{"base": pr.BaseWorkflows, "head": pr.HeadWorkflows, "default": pr.DefaultBranchWorkflows} {
			if !dir.Known || dir.Files != 0 {
				t.Fatalf("%s workflows = %+v, want known with 0 files", name, dir)
			}
		}
	})

	t.Run("any other error still fails", func(t *testing.T) {
		body := `{` + data + `,"errors":[` + notFound(`"repository","defaultBranchRef","target","file"`) +
			`,{"type":"FORBIDDEN","path":["repository","pullRequests"],"message":"Resource not accessible by integration"}]}`
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: body}, {Stdout: body}, {Stdout: body}}, errs: []error{ghFailed, ghFailed, ghFailed}}
		client := GhClient{Runner: runner}
		if _, err := client.OpenPullRequestFacts(context.Background(), repo); err == nil {
			t.Fatal("a FORBIDDEN error was tolerated")
		}
		if _, err := decodeOpenPullRequestFacts([]byte(body)); err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
			t.Fatalf("decode err = %v, want the FORBIDDEN message", err)
		}
	})

	t.Run("a NOT_FOUND elsewhere still fails", func(t *testing.T) {
		body := `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository"}]}`
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: body}, {Stdout: body}, {Stdout: body}}, errs: []error{ghFailed, ghFailed, ghFailed}}
		client := GhClient{Runner: runner}
		if _, err := client.OpenPullRequestFacts(context.Background(), repo); err == nil {
			t.Fatal("a missing repository was tolerated")
		}
	})

	t.Run("gh failing with no body keeps its error", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: ""}, {Stdout: ""}, {Stdout: ""}}, errs: []error{ghFailed, ghFailed, ghFailed}}
		client := GhClient{Runner: runner}
		if _, err := client.OpenPullRequestFacts(context.Background(), repo); err == nil {
			t.Fatal("an empty failed response was tolerated")
		}
	})

	t.Run("a cancellation while waiting to retry is not traded for the stale body", func(t *testing.T) {
		// "rate limit" in a PR title trips run's retry classifier, so run waits
		// to retry and returns the earlier body alongside the wait's error.
		body := `{` + strings.Replace(data, `"number":4,`, `"number":4,"title":"handle rate limit",`, 1) +
			`,"errors":[` + notFound(`"repository","defaultBranchRef","target","file"`) + `]}`
		ctx, cancel := context.WithCancel(context.Background())
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: body}, {Stdout: body}, {Stdout: body}}, errs: []error{ghFailed, ghFailed, ghFailed}}
		client := GhClient{Runner: runner, Sleep: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		}}
		if _, err := client.OpenPullRequestFacts(ctx, repo); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if len(runner.calls) != 1 {
			t.Fatalf("gh calls = %d, want 1 before the cancelled wait", len(runner.calls))
		}
	})
}
