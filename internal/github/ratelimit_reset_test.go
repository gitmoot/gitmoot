package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// The 403 gh actually returns when the core bucket is exhausted, captured on
// this host at 2026-09-07T16:12Z with `gh api -i repos/gitmoot/gitmoot`. Reset
// 1788799425 is 2026-09-07T16:43:45Z, and the real call did succeed after it.
//
// NOTE WHAT IS ABSENT: no Retry-After. That is what distinguishes the primary
// class from the secondary/abuse one, and it is why waiting for the reset is
// the only remedy.
const measuredRateLimit403 = `HTTP/2.0 403 Forbidden
X-Github-Request-Id: A4C8:48A5A:12D10FD4:1234D4F7:6A9EE252
X-Ratelimit-Limit: 5000
X-Ratelimit-Remaining: 0
X-Ratelimit-Reset: 1788799425
X-Ratelimit-Resource: core
X-Ratelimit-Used: 5000

{"message":"API rate limit exceeded for user ID 38183696."}
`

// The body-only failure the FAILING call itself produces. `gh api` without -i
// prints no headers, which is the whole reason the reset has to be probed for
// rather than read off the original result.
const measuredRateLimitStderr = `gh: API rate limit exceeded for user ID 38183696.`

func rateLimitTestClient(runner subprocess.Runner) *GhClient {
	return &GhClient{
		Runner:     runner,
		Dir:        ".",
		MaxRetries: 2,
		Limiter:    NewRateLimiter(RateLimiterConfig{}),
		Sleep:      func(context.Context, time.Duration) error { return nil },
	}
}

// TestPrimaryRateLimitRefusalNamesItsReset is #1994's whole point.
//
// Before this, `resolve pull request #N` reached the operator as bare
// `gh: API rate limit exceeded`, and the observed consequence was four dispatch
// attempts against one exhausted bucket by two agents. The reset was in the
// response headers the entire time.
func TestPrimaryRateLimitRefusalNamesItsReset(t *testing.T) {
	runner := &fakeRunner{}
	// Three attempts at the real call (MaxRetries 2), then one -i probe.
	for range 3 {
		runner.results = append(runner.results, subprocess.Result{Stderr: measuredRateLimitStderr})
		runner.errs = append(runner.errs, errors.New("exit status 1"))
	}
	runner.results = append(runner.results, subprocess.Result{Stdout: measuredRateLimit403})
	runner.errs = append(runner.errs, errors.New("exit status 1"))

	client := rateLimitTestClient(runner)
	_, err := client.GetPullRequest(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 1941)
	if err == nil {
		t.Fatal("GetPullRequest succeeded against an exhausted bucket")
	}

	var rateLimit *RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error is %v (%T), want a *RateLimitError carrying the window", err, err)
	}
	message := err.Error()
	// Each of these is a separate decision the operator has to make, and the
	// bare message supported none of them: WHICH bucket, HOW empty, and WHEN it
	// refills. The wait is asserted as a rendered field rather than a value,
	// because it is relative to now.
	for _, want := range []string{
		"core",                 // the resource, so a graphql or search limit is not misread as core
		"2026-09-07T16:43:45Z", // the reset, from the header rather than from /rate_limit
		"0/5000",               // how empty
		"retrying before then cannot succeed",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal %q does not name %q", message, want)
		}
	}
	// The original text survives, because callers and existing tests key on it
	// and the operator should still see what gh said.
	if !strings.Contains(message, "API rate limit exceeded") {
		t.Errorf("refusal %q dropped the original gh error text", message)
	}

	// ONE probe, not one per attempt. Probing inside the retry loop would triple
	// the requests fired at an already-refusing endpoint, which is the behaviour
	// this issue exists to stop rather than to add.
	probes := 0
	for _, call := range runner.calls {
		for _, arg := range call {
			if arg == "-i" {
				probes++
				break
			}
		}
	}
	if probes != 1 {
		t.Fatalf("issued %d header probes, want exactly 1; calls=%v", probes, runner.calls)
	}
	// And the probe asked about the SAME request. Asking a different endpoint is
	// how /rate_limit gets the answer wrong.
	runner.wantArgs(t, 3, "api", "-i", "repos/gitmoot/gitmoot/pulls/1941")
}

// TestNonRateLimitFailureIsUnchanged is the control that matters most: a
// diagnostic improvement must not alter any other failure, and must not fire a
// probe at an endpoint that was never rate limited.
func TestNonRateLimitFailureIsUnchanged(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "gh: Not Found (HTTP 404)"}},
		errs:    []error{errors.New("exit status 1")},
	}
	client := rateLimitTestClient(runner)
	_, err := client.GetPullRequest(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 1941)
	if err == nil {
		t.Fatal("GetPullRequest succeeded on a 404")
	}
	var rateLimit *RateLimitError
	if errors.As(err, &rateLimit) {
		t.Fatalf("a 404 was wrapped as a rate limit: %v", err)
	}
	if !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("error %q lost the original text", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("a 404 issued %d calls, want 1 (no header probe); calls=%v", len(runner.calls), runner.calls)
	}
}

// TestSecondaryRateLimitIsNotProbed keeps the two classes apart. A secondary
// limit already engages the process-wide backoff and answers to Retry-After;
// adding a request to an abuse window is the opposite of what that path wants.
func TestSecondaryRateLimitIsNotProbed(t *testing.T) {
	const secondary = "gh: You have exceeded a secondary rate limit. Retry-After: 60"
	runner := &fakeRunner{}
	for range 3 {
		runner.results = append(runner.results, subprocess.Result{Stderr: secondary})
		runner.errs = append(runner.errs, errors.New("exit status 1"))
	}
	client := rateLimitTestClient(runner)
	_, err := client.GetPullRequest(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 1941)
	if err == nil {
		t.Fatal("GetPullRequest succeeded against a secondary limit")
	}
	var rateLimit *RateLimitError
	if errors.As(err, &rateLimit) {
		t.Fatalf("a secondary limit was wrapped with a window: %v", err)
	}
	for _, call := range runner.calls {
		for _, arg := range call {
			if arg == "-i" {
				t.Fatalf("a secondary limit was probed for headers; calls=%v", runner.calls)
			}
		}
	}
}

// TestRateLimitProbeWithoutAResetLeavesTheFailureAlone is the fail-open arm. A
// probe that answers without a reset cannot improve the message, and must not
// be able to change whether or how the call failed.
func TestRateLimitProbeWithoutAResetLeavesTheFailureAlone(t *testing.T) {
	runner := &fakeRunner{}
	for range 3 {
		runner.results = append(runner.results, subprocess.Result{Stderr: measuredRateLimitStderr})
		runner.errs = append(runner.errs, errors.New("exit status 1"))
	}
	// Headers present, reset absent: the one field the window requires.
	runner.results = append(runner.results, subprocess.Result{Stdout: "HTTP/2.0 403 Forbidden\nX-Ratelimit-Remaining: 0\n"})
	runner.errs = append(runner.errs, errors.New("exit status 1"))

	client := rateLimitTestClient(runner)
	_, err := client.GetPullRequest(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 1941)
	if err == nil {
		t.Fatal("GetPullRequest succeeded against an exhausted bucket")
	}
	var rateLimit *RateLimitError
	if errors.As(err, &rateLimit) {
		t.Fatalf("a reset-less probe still produced a window: %v", err)
	}
	if !strings.Contains(err.Error(), "API rate limit exceeded") {
		t.Errorf("error %q lost the original text", err)
	}
}

// TestParseRateLimitWindowRequiresAReset pins the parser's own contract,
// because "a window with no reset" is the shape that would render
// "resets at 1970-01-01" and send an operator to the wrong conclusion with
// full confidence.
func TestParseRateLimitWindowRequiresAReset(t *testing.T) {
	if window, ok := parseRateLimitWindow(subprocess.Result{Stdout: measuredRateLimit403}); !ok {
		t.Fatal("the measured 403 did not parse")
	} else {
		if window.Resource != "core" || window.Limit != 5000 || window.Remaining != 0 {
			t.Errorf("window = %+v, want core 0/5000", window)
		}
		if got := window.Reset.UTC().Format(time.RFC3339); got != "2026-09-07T16:43:45Z" {
			t.Errorf("reset = %s, want 2026-09-07T16:43:45Z", got)
		}
	}
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "no headers at all", body: measuredRateLimitStderr},
		{name: "reset absent", body: "X-Ratelimit-Remaining: 0\nX-Ratelimit-Resource: core\n"},
		{name: "reset unparseable", body: "X-Ratelimit-Reset: not-a-number\n"},
		{name: "reset zero", body: "X-Ratelimit-Reset: 0\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := parseRateLimitWindow(subprocess.Result{Stdout: tt.body}); ok {
				t.Fatalf("%q parsed as a usable window", tt.body)
			}
		})
	}
}
