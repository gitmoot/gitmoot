package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestListIssueCommentsDedupesByID(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[
				[
					{"id": 11, "body": "first", "html_url": "https://example.com/1", "user": {"login": "alice"}},
					{"id": 11, "body": "duplicate", "html_url": "https://example.com/1", "user": {"login": "alice"}}
				],
				[
					{"id": 12, "body": "second", "html_url": "https://example.com/2", "user": {"login": "bob"}}
				]
			]`,
		}},
	}
	client := GhClient{Runner: runner}

	comments, err := client.ListIssueComments(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListIssueComments returned error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments length = %d, want 2", len(comments))
	}
	if comments[0].Body != "first" || comments[1].Author != "bob" {
		t.Fatalf("comments were not decoded in first-seen order: %+v", comments)
	}
	runner.wantArgs(t, 0, "api", "--paginate", "--slurp", "repos/jerryfane/gitmoot/issues/2/comments")
}

func TestPostIssueCommentUsesIssueCommentsEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"id": 21, "body": "done", "html_url": "https://github.com/jerryfane/gitmoot/pull/2#issuecomment-21", "user": {"login": "gitmoot"}}`,
		}},
	}
	client := GhClient{Runner: runner}

	comment, err := client.PostIssueComment(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2, "done")

	if err != nil {
		t.Fatalf("PostIssueComment returned error: %v", err)
	}
	if comment.ID != 21 || comment.Body != "done" {
		t.Fatalf("comment = %+v", comment)
	}
	runner.wantArgs(t, 0, "api", "repos/jerryfane/gitmoot/issues/2/comments", "-f", "body=done")
}

func TestCreateCommitStatusUsesStatusesEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"id": 31, "state": "success", "context": "gitmoot/task", "description": "ok", "target_url": "https://example.com"}`,
		}},
	}
	client := GhClient{Runner: runner}

	status, err := client.CreateCommitStatus(context.Background(), CommitStatusInput{
		Repo:        Repository{Owner: "jerryfane", Name: "gitmoot"},
		SHA:         "abc123",
		State:       "success",
		Context:     "gitmoot/task",
		Description: "ok",
		TargetURL:   "https://example.com",
	})

	if err != nil {
		t.Fatalf("CreateCommitStatus returned error: %v", err)
	}
	if status.ID != 31 || status.State != "success" {
		t.Fatalf("status = %+v", status)
	}
	runner.wantArgs(t, 0,
		"api",
		"repos/jerryfane/gitmoot/statuses/abc123",
		"-f", "state=success",
		"-f", "context=gitmoot/task",
		"-f", "description=ok",
		"-f", "target_url=https://example.com",
	)
}

func TestListPullRequestChecksUsesGhChecksOutput(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[
				{"name": "test", "state": "SUCCESS", "bucket": "pass", "link": "https://example.com/check", "workflow": "ci"},
				{"name": "lint", "state": "PENDING", "bucket": "pending", "workflow": "ci"}
			]`,
		}},
	}
	client := GhClient{Runner: runner}

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListPullRequestChecks returned error: %v", err)
	}
	if len(checks) != 2 || checks[0].Bucket != "pass" || checks[1].Bucket != "pending" {
		t.Fatalf("checks = %+v", checks)
	}
	runner.wantArgs(t, 0,
		"pr", "checks", "2",
		"--repo", "jerryfane/gitmoot",
		"--json", "name,state,bucket,link,workflow,completedAt",
	)
}

func TestListPullRequestChecksAcceptsPendingExitWithJSON(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[{"name": "test", "state": "PENDING", "bucket": "pending"}]`,
		}},
		errs: []error{errors.New("exit status 8")},
	}
	client := GhClient{Runner: runner}

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListPullRequestChecks returned error: %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != "pending" {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestListPullRequestChecksTreatsNoChecksAsEmpty(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stderr: "no checks reported on the 'task' branch",
		}},
		errs: []error{errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListPullRequestChecks returned error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("checks = %+v, want empty", checks)
	}
}

func TestMergePullRequestUsesSafeHeadMatch(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "merged"}}}
	client := GhClient{Runner: runner}

	result, err := client.MergePullRequest(context.Background(), MergePullRequestInput{
		Repo:            Repository{Owner: "jerryfane", Name: "gitmoot"},
		Number:          2,
		Method:          "squash",
		Subject:         "feat: task",
		MatchHeadCommit: "abc123",
		DeleteBranch:    true,
	})

	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if !result.Merged {
		t.Fatalf("merge result = %+v", result)
	}
	runner.wantArgs(t, 0,
		"pr", "merge", "2",
		"--repo", "jerryfane/gitmoot",
		"--squash",
		"--subject", "feat: task",
		"--match-head-commit", "abc123",
		"--delete-branch",
	)
}

func TestRateLimitBackoffRetries(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "HTTP 429: secondary rate limit"},
			{Stdout: `{"id": 42, "state": "pending", "context": "gitmoot/task"}`},
		},
		errs: []error{errors.New("exit 1"), nil},
	}
	var sleeps []time.Duration
	client := GhClient{
		Runner: runner,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
		MaxRetries: 1,
	}

	status, err := client.CreateCommitStatus(context.Background(), CommitStatusInput{
		Repo:    Repository{Owner: "jerryfane", Name: "gitmoot"},
		SHA:     "abc123",
		State:   "pending",
		Context: "gitmoot/task",
	})

	if err != nil {
		t.Fatalf("CreateCommitStatus returned error: %v", err)
	}
	if status.ID != 42 {
		t.Fatalf("status = %+v", status)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{time.Second}) {
		t.Fatalf("sleeps = %v, want [1s]", sleeps)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %d, want 2", len(runner.calls))
	}
}

func TestCreatePullRequestFetchesCreatedPR(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "https://github.com/jerryfane/gitmoot/pull/7\n"},
			{Stdout: `{"number": 7, "title": "Task 7", "state": "open", "html_url": "https://github.com/jerryfane/gitmoot/pull/7", "head": {"ref": "task-7", "sha": "abc123"}, "base": {"ref": "main"}}`},
		},
	}
	client := GhClient{Runner: runner}

	pr, err := client.CreatePullRequest(context.Background(), CreatePullRequestInput{
		Repo:  Repository{Owner: "jerryfane", Name: "gitmoot"},
		Title: "Task 7",
		Body:  "body",
		Head:  "task-7",
		Base:  "main",
	})

	if err != nil {
		t.Fatalf("CreatePullRequest returned error: %v", err)
	}
	if pr.Number != 7 || pr.HeadSHA != "abc123" {
		t.Fatalf("pr = %+v", pr)
	}
	runner.wantArgs(t, 0,
		"pr", "create",
		"--repo", "jerryfane/gitmoot",
		"--title", "Task 7",
		"--body", "body",
		"--head", "task-7",
		"--base", "main",
	)
	runner.wantArgs(t, 1, "api", "repos/jerryfane/gitmoot/pulls/7")
}

type fakeRunner struct {
	results []subprocess.Result
	errs    []error
	calls   [][]string
}

func (f *fakeRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	call := append([]string{command}, args...)
	f.calls = append(f.calls, call)
	index := len(f.calls) - 1
	if command != "gh" {
		return subprocess.Result{}, errors.New("unexpected command: " + command)
	}
	result := subprocess.Result{Command: command, Args: args}
	if index < len(f.results) {
		result = f.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return result, err
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	if file != "gh" {
		return "", errors.New("not found")
	}
	return "/usr/bin/gh", nil
}

func (f *fakeRunner) wantArgs(t *testing.T, index int, want ...string) {
	t.Helper()
	if index >= len(f.calls) {
		t.Fatalf("missing call %d; calls=%v", index, f.calls)
	}
	got := f.calls[index]
	if !reflect.DeepEqual(got, append([]string{"gh"}, want...)) {
		t.Fatalf("call %d = %s\nwant %s", index, strings.Join(got, " "), strings.Join(append([]string{"gh"}, want...), " "))
	}
}
