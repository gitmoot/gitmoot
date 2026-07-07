package github

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// bodyCapturingRunner records every gh argv and, for a call that references a
// body temp file (`--body-file <path>` or a `body=@<path>` field), reads the
// file's content DURING the run — before the caller's deferred cleanup removes
// it — so a test can assert the content is byte-identical to the intended body.
type bodyCapturingRunner struct {
	results  []subprocess.Result
	calls    [][]string
	bodyPath []string // temp-file path referenced by each call, or "" for inline
	bodyRead []string // file content read during each call, or "" for inline
}

func (r *bodyCapturingRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	idx := len(r.calls) - 1

	path := bodyFilePathFromArgs(args)
	r.bodyPath = append(r.bodyPath, path)
	content := ""
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return subprocess.Result{}, err
		}
		content = string(b)
	}
	r.bodyRead = append(r.bodyRead, content)

	var result subprocess.Result
	if idx < len(r.results) {
		result = r.results[idx]
	}
	result.Command = command
	result.Args = args
	return result, nil
}

func (r *bodyCapturingRunner) LookPath(string) (string, error) { return "/usr/bin/gh", nil }

// bodyFilePathFromArgs extracts the temp-file path a call routed the body
// through, whether via the gh-verb `--body-file <path>` flag or the `gh api`
// `-F body=@<path>` field.
func bodyFilePathFromArgs(args []string) string {
	for i, a := range args {
		if a == "--body-file" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "body=@") {
			return strings.TrimPrefix(a, "body=@")
		}
	}
	return ""
}

func hasArgPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func hasArg(args []string, val string) bool {
	for _, a := range args {
		if a == val {
			return true
		}
	}
	return false
}

func largeBody() string {
	return strings.Repeat("A", bodyFileThreshold) + "\n-tail-marker-\n"
}

// TestBodyFlagArgsBoundary pins the `--body`/`--body-file` verb helper: inline at
// the threshold, temp-file above it, content byte-identical, file removed on
// cleanup.
func TestBodyFlagArgsBoundary(t *testing.T) {
	t.Run("at threshold stays inline", func(t *testing.T) {
		body := strings.Repeat("x", bodyFileThreshold)
		args, cleanup, err := bodyFlagArgs(body)
		if err != nil {
			t.Fatalf("bodyFlagArgs: %v", err)
		}
		defer cleanup()
		want := []string{"--body", body}
		if len(args) != 2 || args[0] != want[0] || args[1] != want[1] {
			t.Fatalf("args[0]=%q len=%d; want inline --body", args[0], len(args))
		}
	})

	t.Run("above threshold routes through a temp file", func(t *testing.T) {
		body := strings.Repeat("x", bodyFileThreshold+1)
		args, cleanup, err := bodyFlagArgs(body)
		if err != nil {
			t.Fatalf("bodyFlagArgs: %v", err)
		}
		if len(args) != 2 || args[0] != "--body-file" {
			t.Fatalf("args=%v; want --body-file <path>", args)
		}
		path := args[1]
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read temp body file: %v", err)
		}
		if string(got) != body {
			t.Fatalf("temp file content mismatch: got %d bytes, want %d bytes", len(got), len(body))
		}
		cleanup()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("temp file not cleaned up: stat err=%v", err)
		}
	})
}

// TestAPIBodyFieldArgsBoundary pins the `gh api` field helper: inline `-f body=`
// at the threshold, `-F body=@<file>` above it, content byte-identical, cleanup.
func TestAPIBodyFieldArgsBoundary(t *testing.T) {
	t.Run("at threshold stays inline -f", func(t *testing.T) {
		body := strings.Repeat("y", bodyFileThreshold)
		args, cleanup, err := apiBodyFieldArgs(body)
		if err != nil {
			t.Fatalf("apiBodyFieldArgs: %v", err)
		}
		defer cleanup()
		if len(args) != 2 || args[0] != "-f" || args[1] != "body="+body {
			t.Fatalf("args[0]=%q len=%d; want inline -f body=", args[0], len(args))
		}
	})

	t.Run("above threshold routes through -F @file", func(t *testing.T) {
		body := strings.Repeat("y", bodyFileThreshold+1)
		args, cleanup, err := apiBodyFieldArgs(body)
		if err != nil {
			t.Fatalf("apiBodyFieldArgs: %v", err)
		}
		if len(args) != 2 || args[0] != "-F" || !strings.HasPrefix(args[1], "body=@") {
			t.Fatalf("args=%v; want -F body=@<path>", args)
		}
		path := strings.TrimPrefix(args[1], "body=@")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read temp body file: %v", err)
		}
		if string(got) != body {
			t.Fatalf("temp file content mismatch: got %d bytes, want %d bytes", len(got), len(body))
		}
		cleanup()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("temp file not cleaned up: stat err=%v", err)
		}
	})
}

func TestCreateIssueBodyRouting(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}
	ctx := context.Background()

	t.Run("small body stays inline -f", func(t *testing.T) {
		runner := &bodyCapturingRunner{results: []subprocess.Result{{Stdout: `{"number":7}`}}}
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.CreateIssue(ctx, CreateIssueInput{Repo: repo, Title: "t", Body: "small"}); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		call := runner.calls[0]
		if !hasArgPair(call, "-f", "body=small") {
			t.Fatalf("want inline -f body=small; got %v", call)
		}
		if hasArg(call, "--body-file") || hasArg(call, "-F") {
			t.Fatalf("small body should not use a file; got %v", call)
		}
	})

	t.Run("large body routes through -F @file and cleans up", func(t *testing.T) {
		body := largeBody()
		runner := &bodyCapturingRunner{results: []subprocess.Result{{Stdout: `{"number":7}`}}}
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.CreateIssue(ctx, CreateIssueInput{Repo: repo, Title: "t", Body: body}); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		call := runner.calls[0]
		path := runner.bodyPath[0]
		if path == "" || !hasArgPair(call, "-F", "body=@"+path) {
			t.Fatalf("want -F body=@<path>; got %v", call)
		}
		if hasArg(call, "-f") && strings.Contains(strings.Join(call, " "), "body=A") {
			t.Fatalf("large body must not be an inline -f arg; got %v", call)
		}
		if runner.bodyRead[0] != body {
			t.Fatalf("temp file content not byte-identical to body")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("temp file not cleaned up after CreateIssue: %v", err)
		}
	})
}

func TestPostIssueCommentBodyRouting(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}
	ctx := context.Background()

	t.Run("small body stays inline -f", func(t *testing.T) {
		runner := &bodyCapturingRunner{results: []subprocess.Result{{Stdout: `{"id":3}`}}}
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.PostIssueComment(ctx, repo, 5, "small"); err != nil {
			t.Fatalf("PostIssueComment: %v", err)
		}
		if !hasArgPair(runner.calls[0], "-f", "body=small") {
			t.Fatalf("want inline -f body=small; got %v", runner.calls[0])
		}
	})

	t.Run("large body routes through -F @file and cleans up", func(t *testing.T) {
		body := largeBody()
		runner := &bodyCapturingRunner{results: []subprocess.Result{{Stdout: `{"id":3}`}}}
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.PostIssueComment(ctx, repo, 5, body); err != nil {
			t.Fatalf("PostIssueComment: %v", err)
		}
		path := runner.bodyPath[0]
		if path == "" || !hasArgPair(runner.calls[0], "-F", "body=@"+path) {
			t.Fatalf("want -F body=@<path>; got %v", runner.calls[0])
		}
		if runner.bodyRead[0] != body {
			t.Fatalf("temp file content not byte-identical to body")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("temp file not cleaned up after PostIssueComment: %v", err)
		}
	})
}

func TestCreatePullRequestBodyRouting(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}
	ctx := context.Background()
	// call 0: pr create -> URL on stdout; call 1: getPullRequest -> PR JSON.
	makeRunner := func() *bodyCapturingRunner {
		return &bodyCapturingRunner{results: []subprocess.Result{
			{Stdout: "https://github.com/o/r/pull/12\n"},
			{Stdout: `{"number":12,"html_url":"https://github.com/o/r/pull/12"}`},
		}}
	}

	t.Run("small body stays inline --body", func(t *testing.T) {
		runner := makeRunner()
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.CreatePullRequest(ctx, CreatePullRequestInput{Repo: repo, Title: "t", Body: "small", Head: "h", Base: "main"}); err != nil {
			t.Fatalf("CreatePullRequest: %v", err)
		}
		if !hasArgPair(runner.calls[0], "--body", "small") {
			t.Fatalf("want inline --body small; got %v", runner.calls[0])
		}
		if hasArg(runner.calls[0], "--body-file") {
			t.Fatalf("small body should not use --body-file; got %v", runner.calls[0])
		}
	})

	t.Run("large body routes through --body-file and cleans up", func(t *testing.T) {
		body := largeBody()
		runner := makeRunner()
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.CreatePullRequest(ctx, CreatePullRequestInput{Repo: repo, Title: "t", Body: body, Head: "h", Base: "main"}); err != nil {
			t.Fatalf("CreatePullRequest: %v", err)
		}
		path := runner.bodyPath[0]
		if path == "" || !hasArgPair(runner.calls[0], "--body-file", path) {
			t.Fatalf("want --body-file <path>; got %v", runner.calls[0])
		}
		if hasArg(runner.calls[0], "--body") {
			t.Fatalf("large body must not also pass inline --body; got %v", runner.calls[0])
		}
		if runner.bodyRead[0] != body {
			t.Fatalf("temp file content not byte-identical to body")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("temp file not cleaned up after CreatePullRequest: %v", err)
		}
	})
}

func TestMergePullRequestBodyRouting(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}
	ctx := context.Background()
	makeRunner := func() *bodyCapturingRunner {
		return &bodyCapturingRunner{results: []subprocess.Result{
			{},
			{Stdout: `{"number":5,"merged":true,"state":"merged"}`},
		}}
	}

	t.Run("small body stays inline --body", func(t *testing.T) {
		runner := makeRunner()
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.MergePullRequest(ctx, MergePullRequestInput{Repo: repo, Number: 5, Method: "squash", Body: "small"}); err != nil {
			t.Fatalf("MergePullRequest: %v", err)
		}
		if !hasArgPair(runner.calls[0], "--body", "small") {
			t.Fatalf("want inline --body small; got %v", runner.calls[0])
		}
	})

	t.Run("large body routes through --body-file and cleans up", func(t *testing.T) {
		body := largeBody()
		runner := makeRunner()
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.MergePullRequest(ctx, MergePullRequestInput{Repo: repo, Number: 5, Method: "squash", Body: body}); err != nil {
			t.Fatalf("MergePullRequest: %v", err)
		}
		path := runner.bodyPath[0]
		if path == "" || !hasArgPair(runner.calls[0], "--body-file", path) {
			t.Fatalf("want --body-file <path>; got %v", runner.calls[0])
		}
		if runner.bodyRead[0] != body {
			t.Fatalf("temp file content not byte-identical to body")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("temp file not cleaned up after MergePullRequest: %v", err)
		}
	})
}
