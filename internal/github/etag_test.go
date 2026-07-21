package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func resetConditionalState() {
	conditionalRequests.Lock()
	conditionalRequests.enabled = true
	conditionalRequests.entries = map[string]conditionalCacheEntry{}
	conditionalRequests.Unlock()
}

func TestConditionalRequestKeyIncludesLiteralArguments(t *testing.T) {
	t.Setenv("GH_HOST", "github.example.com")
	repo := Repository{Owner: "owner", Name: "repo"}
	base := conditionalRequestKey(repo, []string{"api", "-X", "GET", "repos/owner/repo/issues", "-f", "state=open", "-f", "per_page=100"})
	for name, args := range map[string][]string{
		"state":    {"api", "-X", "GET", "repos/owner/repo/issues", "-f", "state=closed", "-f", "per_page=100"},
		"since":    {"api", "-X", "GET", "repos/owner/repo/issues", "-f", "state=open", "-f", "per_page=100", "-f", "since=2026-07-14T00:00:00Z"},
		"per_page": {"api", "-X", "GET", "repos/owner/repo/issues", "-f", "state=open", "-f", "per_page=50"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := conditionalRequestKey(repo, args); got == base {
				t.Fatalf("key did not distinguish %s: %q", name, got)
			}
		})
	}
	if !strings.HasPrefix(base, "github.example.com|owner/repo|") {
		t.Fatalf("key = %q, want host and repo prefix", base)
	}
}

func TestParseConditionalResponseHeaderBlocks(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		status int
		etag   string
		body   string
	}{
		{
			name:   "http1 crlf",
			input:  "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nETag: W/\"abc\"\r\n\r\n[{\"number\":1}]",
			status: 200,
			etag:   `W/"abc"`,
			body:   `[{"number":1}]`,
		},
		{
			name:   "http2 case insensitive",
			input:  "HTTP/2.0 304 Not Modified\neTaG: \"quoted\"\n\n",
			status: 304,
			etag:   `"quoted"`,
		},
		{
			name:   "multiple blocks",
			input:  "HTTP/1.1 100 Continue\r\nX-One: yes\r\n\r\nHTTP/2 200 OK\r\netag: \"final\"\r\n\r\n[]",
			status: 200,
			etag:   `"final"`,
			body:   `[]`,
		},
		{
			name:   "missing etag",
			input:  "HTTP/2 200 OK\nContent-Type: application/json\n\n[]",
			status: 200,
			body:   `[]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseConditionalResponse([]byte(tc.input))
			if !got.headers || got.status != tc.status || got.etag != tc.etag || string(got.body) != tc.body {
				t.Fatalf("parsed = %+v body=%q", got, got.body)
			}
		})
	}
}

func TestConditionalRunStoresAndReplays304(t *testing.T) {
	resetConditionalState()
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "HTTP/2.0 200 OK\r\nETag: W/\"etag-1\"\r\n\r\n[{\"number\":7}]"},
			{Stdout: "HTTP/2.0 304 Not Modified\r\n\r\n"},
		},
		errs: []error{nil, errors.New("exit status 1")},
	}
	repo := Repository{Owner: "owner", Name: "repo"}
	first := GhClient{Runner: runner}
	if pulls, err := first.ListPullRequests(context.Background(), repo, "open"); err != nil || len(pulls) != 1 || pulls[0].Number != 7 {
		t.Fatalf("first pulls=%+v err=%v", pulls, err)
	}
	second := GhClient{Runner: runner}
	pulls, err := second.ListPullRequests(context.Background(), repo, "open")
	if err != nil || len(pulls) != 1 || pulls[0].Number != 7 {
		t.Fatalf("304 replay pulls=%+v err=%v", pulls, err)
	}
	runner.wantArgs(t, 1, "api", "-i", "-H", `If-None-Match: W/"etag-1"`, "-X", "GET", "repos/owner/repo/pulls", "-f", "state=open", "-f", "per_page=100")
	if got := second.ConditionalRequestStats(); got.Calls != 1 || got.Misses != 0 {
		t.Fatalf("304 stats = %+v, want one hit", got)
	}
}

func TestConditionalFullPageFallsBackToUnconditionalPagination(t *testing.T) {
	resetConditionalState()
	page := make([]PullRequest, 100)
	for i := range page {
		page[i].Number = int64(i + 1)
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []subprocess.Result{
		{Stdout: "HTTP/2 200 OK\nETag: \"full\"\n\n" + string(body)},
		{Stdout: string(body)},
	}}
	client := GhClient{Runner: runner}
	repo := Repository{Owner: "owner", Name: "repo"}
	pulls, err := client.ListPullRequests(context.Background(), repo, "open")
	if err != nil || len(pulls) != 100 {
		t.Fatalf("pulls=%d err=%v", len(pulls), err)
	}
	runner.wantArgs(t, 1, "api", "--paginate", "-X", "GET", "repos/owner/repo/pulls", "-f", "state=open")
	key := conditionalRequestKey(repo, []string{"api", "-X", "GET", "repos/owner/repo/pulls", "-f", "state=open", "-f", "per_page=100"})
	if _, ok := loadConditionalEntry(key); ok {
		t.Fatal("full first page remained cached")
	}
}

func TestConditional304CorruptCacheEvictsAndRetriesOnce(t *testing.T) {
	resetConditionalState()
	repo := Repository{Owner: "owner", Name: "repo"}
	args := []string{"api", "-X", "GET", "repos/owner/repo/pulls", "-f", "state=open", "-f", "per_page=100"}
	key := conditionalRequestKey(repo, args)
	conditionalRequests.Lock()
	conditionalRequests.entries[key] = conditionalCacheEntry{etag: `"bad"`, body: []byte("not-json")}
	conditionalRequests.Unlock()
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "HTTP/2.0 304 Not Modified\r\n\r\n"},
			{Stdout: "HTTP/2.0 200 OK\r\nETag: \"good\"\r\n\r\n[]"},
		},
		errs: []error{errors.New("exit status 1"), nil},
	}
	client := GhClient{Runner: runner}
	if pulls, err := client.ListPullRequests(context.Background(), repo, "open"); err != nil || len(pulls) != 0 {
		t.Fatalf("pulls=%+v err=%v", pulls, err)
	}
	runner.wantArgs(t, 0, "api", "-i", "-H", `If-None-Match: "bad"`, "-X", "GET", "repos/owner/repo/pulls", "-f", "state=open", "-f", "per_page=100")
	runner.wantArgs(t, 1, "api", "-i", "-X", "GET", "repos/owner/repo/pulls", "-f", "state=open", "-f", "per_page=100")
	if got := client.ConditionalRequestStats(); got.Calls != 2 || got.Misses != 2 {
		t.Fatalf("corrupt retry stats = %+v", got)
	}
}

func TestConditionalSkipsBodiesOverOneMiBAndMissingETag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		body   string
	}{
		{name: "too large", header: "ETag: \"large\"\n", body: `["` + strings.Repeat("x", maxConditionalBodyBytes) + `"]`},
		{name: "missing etag", body: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetConditionalState()
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: "HTTP/2 200 OK\n" + tc.header + "\n" + tc.body}}}
			client := GhClient{Runner: runner}
			repo := Repository{Owner: "owner", Name: "repo"}
			_, key, err := client.conditionalRun(context.Background(), repo, "-X", "GET", "repos/owner/repo/items")
			if err != nil {
				t.Fatalf("conditionalRun: %v", err)
			}
			if _, ok := loadConditionalEntry(key); ok {
				t.Fatal("response was cached")
			}
		})
	}
}

func TestConditional304NotesLimiterSuccess(t *testing.T) {
	resetConditionalState()
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    time.Minute,
		MaxBackoff:     4 * time.Minute,
		Now:            func() time.Time { return now },
	})
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "HTTP/2 200 OK\nETag: \"v1\"\n\n[]"},
			{Stdout: "HTTP/2.0 304 Not Modified\n\n"},
		},
		errs: []error{nil, errors.New("exit status 1")},
	}
	repo := Repository{Owner: "owner", Name: "repo"}
	client := GhClient{Runner: runner, Limiter: limiter}
	if _, err := client.ListPullRequests(context.Background(), repo, "open"); err != nil {
		t.Fatal(err)
	}
	limiter.NoteSecondaryLimit(0)
	now = now.Add(2 * time.Minute)
	if _, err := client.ListPullRequests(context.Background(), repo, "open"); err != nil {
		t.Fatalf("304 returned error: %v", err)
	}
	limiter.NoteSecondaryLimit(0)
	state := limiter.Snapshot()
	if state.BackoffRemaining != time.Minute {
		t.Fatalf("backoff after 304 success = %s, want reset base 1m", state.BackoffRemaining)
	}
	if state.CallsInLastHour != 2 {
		t.Fatalf("CallsInLastHour = %d, want 2", state.CallsInLastHour)
	}
}
