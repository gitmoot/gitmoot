package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// #1994. A PRIMARY rate-limit refusal used to reach the operator as bare
// `gh: API rate limit exceeded`, dropping the one fact that decides what to do
// next: when the bucket refills. Two agents on this host then burned four
// dispatch attempts against the same exhausted bucket before anyone read the
// response headers, where the reset had been the whole time.
//
// THE RESET IS ONLY AVAILABLE FROM A REAL REQUEST'S HEADERS, and that is
// measured rather than assumed. `GET /rate_limit` is not a usable instrument:
// on one credential, one host, seconds apart, it reported core 4999/5000
// used=1 while a real core request returned 403 with remaining=0 used=5000,
// and their resets were thirteen minutes apart. Five minutes later it reported
// a completely FRESH window, 5000/5000 used=0, while real calls were still
// refused; the 403 header's own reset was accurate throughout. So this probes
// the SAME request that failed, whose 403 carries authoritative headers,
// instead of asking a different endpoint about a bucket it does not own.
var (
	rateLimitResetPattern     = regexp.MustCompile(`(?i)x-ratelimit-reset:\s*(\d+)`)
	rateLimitRemainingPattern = regexp.MustCompile(`(?i)x-ratelimit-remaining:\s*(\d+)`)
	rateLimitLimitPattern     = regexp.MustCompile(`(?i)x-ratelimit-limit:\s*(\d+)`)
	rateLimitResourcePattern  = regexp.MustCompile(`(?i)x-ratelimit-resource:\s*(\S+)`)
)

// RateLimitWindow is what a refused request's own headers say about the bucket
// it was charged against.
type RateLimitWindow struct {
	Resource  string
	Limit     int
	Remaining int
	Reset     time.Time
}

// parseRateLimitWindow reads the headers of a `gh api -i` response. It requires
// a RESET, because a window with no reset cannot answer the only question this
// exists for; every other field is decoration on the message.
func parseRateLimitWindow(result subprocess.Result) (RateLimitWindow, bool) {
	text := result.Stdout + "\n" + result.Stderr
	match := rateLimitResetPattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return RateLimitWindow{}, false
	}
	seconds, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || seconds <= 0 {
		return RateLimitWindow{}, false
	}
	window := RateLimitWindow{Reset: time.Unix(seconds, 0).UTC()}
	if m := rateLimitResourcePattern.FindStringSubmatch(text); len(m) >= 2 {
		window.Resource = strings.TrimSpace(m[1])
	}
	if m := rateLimitLimitPattern.FindStringSubmatch(text); len(m) >= 2 {
		if value, convErr := strconv.Atoi(m[1]); convErr == nil {
			window.Limit = value
		}
	}
	if m := rateLimitRemainingPattern.FindStringSubmatch(text); len(m) >= 2 {
		if value, convErr := strconv.Atoi(m[1]); convErr == nil {
			window.Remaining = value
		}
	}
	return window, true
}

// RateLimitError carries a primary rate-limit refusal together with the window
// its own response reported, so every caller's message names the reset without
// each one having to parse headers.
//
// It wraps rather than replaces: the original error text is preserved verbatim,
// because callers and existing tests key on it and because the operator should
// still see exactly what gh said.
type RateLimitError struct {
	Window RateLimitWindow
	Err    error
}

func (e *RateLimitError) Error() string {
	resource := strings.TrimSpace(e.Window.Resource)
	if resource == "" {
		resource = "GitHub API"
	}
	// The WAIT is rendered as well as the timestamp. An absolute reset alone
	// still leaves an operator computing a subtraction under time pressure, and
	// that subtraction is exactly what nobody did today.
	wait := time.Until(e.Window.Reset).Round(time.Second)
	if wait < 0 {
		wait = 0
	}
	return fmt.Sprintf("%v; %s rate limit exhausted (%d/%d remaining), resets at %s (in %s) - waiting is the only remedy, retrying before then cannot succeed",
		e.Err, resource, e.Window.Remaining, e.Window.Limit,
		e.Window.Reset.Format(time.RFC3339), wait)
}

func (e *RateLimitError) Unwrap() error { return e.Err }

// rateLimitWindowForRequest re-issues the SAME request with `-i` and reads the
// rate-limit headers off its response.
//
// It runs only on a request that has already failed with a primary rate limit,
// and it costs one refused request. A refused request consumes no further
// quota - there is none left to consume - and a 403 carries the same
// `X-Ratelimit-*` headers as a 200, which is what makes this work.
//
// It is BEST EFFORT and silent on failure. If the probe cannot run, or answers
// without a reset, the caller keeps the original unwrapped error. A diagnostic
// improvement must never be able to change whether a call failed.
func (c *GhClient) rateLimitWindowForRequest(ctx context.Context, runner subprocess.Runner, args []string) (RateLimitWindow, bool) {
	if len(args) == 0 || args[0] != "api" {
		return RateLimitWindow{}, false
	}
	probe := make([]string, 0, len(args)+1)
	probe = append(probe, "api", "-i")
	probe = append(probe, args[1:]...)
	result, err := runner.Run(ctx, c.Dir, "gh", probe...)
	if err != nil && strings.TrimSpace(result.Stdout+result.Stderr) == "" {
		return RateLimitWindow{}, false
	}
	return parseRateLimitWindow(result)
}
