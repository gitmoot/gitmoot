package github

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func assertNoNULArg(t *testing.T, calls [][]string) {
	t.Helper()
	for _, call := range calls {
		for _, arg := range call {
			if strings.IndexByte(arg, 0) >= 0 {
				t.Fatalf("gh argv element carries a NUL byte: %q (argv %q)", arg, call)
			}
		}
	}
}

// #2688: a result comment whose body held a NUL byte went into argv as
// `-f body=...`, so fork/exec of gh failed with EINVAL on every attempt. The
// body now goes through the temp file, which carries the byte unaltered.
func TestPostIssueCommentWithNULBodyUsesBodyFile(t *testing.T) {
	body := "review failed:\x00 stray NUL from the agent"
	runner := &bodyCapturingRunner{results: []subprocess.Result{{Stdout: `{"id":3}`}}}
	client := GhClient{Runner: runner, MaxRetries: 0}
	if _, err := client.PostIssueComment(context.Background(), Repository{Owner: "o", Name: "r"}, 5, body); err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("gh calls = %d, want 1", len(runner.calls))
	}
	assertNoNULArg(t, runner.calls)
	path := runner.bodyPath[0]
	if path == "" || !hasArgPair(runner.calls[0], "-F", "body=@"+path) {
		t.Fatalf("want -F body=@<path>; got %q", runner.calls[0])
	}
	if runner.bodyRead[0] != body {
		t.Fatalf("body file = %q, want the body byte-identical (NUL included)", runner.bodyRead[0])
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file not cleaned up: %v", err)
	}
}

func TestMergePullRequestWithNULBodyUsesBodyFile(t *testing.T) {
	body := "squash body\x00tail"
	runner := &bodyCapturingRunner{results: []subprocess.Result{{}, {Stdout: `{"merged":true}`}, {}}}
	client := GhClient{Runner: runner, MaxRetries: 0}
	_, _ = client.MergePullRequest(context.Background(), MergePullRequestInput{
		Repo: Repository{Owner: "o", Name: "r"}, Number: 5, Method: "squash", Subject: "sub\x00ject", Body: body,
	})
	if len(runner.calls) == 0 {
		t.Fatal("MergePullRequest ran no gh call")
	}
	assertNoNULArg(t, runner.calls)
	for i, call := range runner.calls {
		if runner.bodyPath[i] != "" && hasArgPair(call, "--body-file", runner.bodyPath[i]) {
			if runner.bodyRead[i] != body {
				t.Fatalf("body file = %q, want %q", runner.bodyRead[i], body)
			}
			if !hasArgPair(call, "--subject", "subject") {
				t.Fatalf("want the NUL dropped from --subject; got %q", call)
			}
			return
		}
	}
	t.Fatalf("no call routed the body through --body-file: %q", runner.calls)
}

// Free-text fields a file cannot carry (titles, subjects, descriptions) lose
// their NUL at the one exec boundary, so no gh argv can carry one.
func TestCreateIssueDropsNULFromTitleArg(t *testing.T) {
	runner := &bodyCapturingRunner{results: []subprocess.Result{{Stdout: `{"number":9}`}}}
	client := GhClient{Runner: runner, MaxRetries: 0}
	if _, err := client.CreateIssue(context.Background(), CreateIssueInput{
		Repo: Repository{Owner: "o", Name: "r"}, Title: "bad\x00title", Body: "body\x00",
	}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	assertNoNULArg(t, runner.calls)
	if !hasArgPair(runner.calls[0], "-f", "title=badtitle") {
		t.Fatalf("want -f title=badtitle; got %q", runner.calls[0])
	}
	if runner.bodyRead[0] != "body\x00" {
		t.Fatalf("body file = %q, want the NUL kept", runner.bodyRead[0])
	}
}

func TestArgvWithoutNULLeavesCleanArgvUncopied(t *testing.T) {
	args := []string{"api", "x", "-f", "body=ok"}
	if got := argvWithoutNUL(args); &got[0] != &args[0] {
		t.Fatal("clean argv was copied")
	}
	dirty := []string{"a\x00", "b", "\x00c\x00"}
	got := argvWithoutNUL(dirty)
	if strings.Join(got, ",") != "a,b,c" || dirty[0] != "a\x00" {
		t.Fatalf("argvWithoutNUL = %q (input now %q), want [a b c] and the input untouched", got, dirty)
	}
}
