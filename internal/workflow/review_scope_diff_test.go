package workflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// testScopeDiff is a resolver answer for tests that only need a follow-up
// scope to exist: the files moved, and they are the pull request's own.
func testScopeDiff(files ...string) ReviewScopeDiff {
	return ReviewScopeDiff{ChangedFiles: files, MergeBase: "merge-base", OwnFiles: files}
}

// #2682 (d) at the engine. A prior finding in a file outside the pull
// request's own diff is not carried as a named finding: it moves to
// OutsidePRFindings, and the brief lists it as not to be re-raised as blocking.
// A finding in an own file and a finding that names no file stay.
func TestFollowUpScopeDropsPriorFindingsOutsideThePullRequest(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "gitmoot/gitmoot", Number: 1678, HeadBranch: "task-1678", BaseBranch: "release", HeadSHA: "head-one", State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	outside := `{"severity":"P2","title":"base code is wrong","file":"base/x.go","line":3}`
	own := `{"severity":"P2","title":"own code is wrong","file":"own/y.go","line":2}`
	plain := "F3: the summary names no file"
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Three findings.",
		Findings: []json.RawMessage{json.RawMessage(outside), json.RawMessage(own), mustJSONString(t, plain)},
	})
	var gotBase string
	engine.ReviewChangedFiles = func(_ context.Context, _ string, _ int, baseBranch, _, _ string) (ReviewScopeDiff, error) {
		gotBase = baseBranch
		return ReviewScopeDiff{
			ChangedFiles:  []string{"own/y.go"},
			MergeBase:     "mb-two",
			OwnFiles:      []string{"own/y.go"},
			PriorOwnLines: map[string][]ReviewLineRange{"own/y.go": {{Start: 1, End: 4}}},
		}, nil
	}

	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	if gotBase != "release" {
		t.Fatalf("resolver base branch = %q, want the cached PR base %q", gotBase, "release")
	}
	payload, err := unmarshalPayload(mustJob(t, store, "review-audit-task-1678-review-2").Payload)
	if err != nil {
		t.Fatal(err)
	}
	scope := payload.ReviewScope
	if scope == nil {
		t.Fatal("follow-up review carries no scope")
	}
	if scope.MergeBase != "mb-two" {
		t.Fatalf("scope merge base = %q, want mb-two", scope.MergeBase)
	}
	if !slices.Equal(scope.OutsidePRFindings, []string{outside}) {
		t.Fatalf("outside-PR findings = %q, want only the base-file finding", scope.OutsidePRFindings)
	}
	if !slices.Equal(scope.Findings, []string{own, plain}) {
		t.Fatalf("carried findings = %q, want the own-file finding and the fileless one", scope.Findings)
	}
	named, rest, ok := strings.Cut(payload.Instructions, "Named findings still in scope:\n")
	if !ok {
		t.Fatalf("brief has no named-findings list: %q", payload.Instructions)
	}
	carried, listed, ok := strings.Cut(rest, "Previously reported outside this PR: do not re-raise as blocking:\n")
	if !ok {
		t.Fatalf("brief does not list the outside-PR finding as non-blocking: %q", payload.Instructions)
	}
	if strings.Contains(named+carried, "base code is wrong") {
		t.Fatalf("the outside-PR finding is still carried as a named finding: %q", payload.Instructions)
	}
	if !strings.Contains(listed, "base code is wrong") || !strings.Contains(carried, "own code is wrong") {
		t.Fatalf("brief lists the findings under the wrong headings: %q", payload.Instructions)
	}
	if !strings.Contains(payload.Instructions, "mb-two") {
		t.Fatalf("brief does not name the merge base: %q", payload.Instructions)
	}
}

// A resolver that names no merge base cannot tell the reviewer where the pull
// request's own change starts, so the round degrades to a recorded full review
// rather than a scope whose diff base is blank.
func TestFollowUpScopeWithoutAMergeBaseDegradesToAFullReview(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{Decision: "approved"})
	engine.ReviewChangedFiles = func(context.Context, string, int, string, string, string) (ReviewScopeDiff, error) {
		return ReviewScopeDiff{ChangedFiles: []string{"own/y.go"}}, nil
	}
	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	payload, err := unmarshalPayload(mustJob(t, store, "review-audit-task-1678-review-2").Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ReviewScope != nil {
		t.Fatalf("scope = %+v, want a full review", payload.ReviewScope)
	}
	if !hasReviewScopeUnavailableEvent(t, store, "head-two") {
		t.Fatal("the unscoped head left no review_scope_unavailable record")
	}
}

func TestSplitOutsidePRFindings(t *testing.T) {
	diff := ReviewScopeDiff{
		// renamed.go is added.go after the pull request renamed the file it added.
		OwnFiles: []string{"own/y.go", "own/bin.png", "own/new.go", "own/renamed.go"},
		PriorOwnLines: map[string][]ReviewLineRange{
			"own/y.go":     {{Start: 10, End: 20}, {Start: 40, End: 41}},
			"own/bin.png":  nil,
			"own/added.go": {{Start: 1, End: 30}},
		},
	}
	for _, tc := range []struct {
		name    string
		finding string
		outside bool
	}{
		{"plain text names no file", "F1: guard has no test", false},
		{"json without a file", `{"title":"no locator"}`, false},
		{"file outside the pull request", `{"file":"base/x.go","line":3}`, true},
		{"file outside, no line", `{"file":"base/x.go"}`, true},
		{"own file, line in a hunk", `{"file":"own/y.go","line":15}`, false},
		{"own file, line outside every hunk", `{"file":"own/y.go","line":30}`, true},
		{"own file, no line", `{"file":"own/y.go"}`, false},
		{"own file, line as text range overlapping", `{"file":"own/y.go","line":"38-40"}`, false},
		{"own binary file", `{"file":"own/bin.png","line":99}`, false},
		{"file the pull request touched only after the finding", `{"file":"own/new.go","line":7}`, true},
		{"file the pull request added, renamed since", `{"file":"own/added.go","line":7}`, false},
		{"absolute path", `{"file":"/usr/lib/x.go","line":3}`, false},
		{"dot-slash path outside", `{"file":"./base/x.go","line":3}`, true},
		{"location outside", `{"location":"base/x.go:711-718"}`, true},
		{"location inside", `{"location":"own/y.go#L12-L13"}`, false},
		{"location naming one own hunk among others", `{"location":"base/x.go:1; own/y.go:41"}`, false},
		{"location naming only outside lines", `{"location":"base/x.go:1; own/y.go:74-87,115-200"}`, true},
		{"location that is not a path", `{"location":"the retry loop"}`, false},
		{"file wins, location supplies its line", `{"file":"own/y.go","location":"own/y.go:30"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, outside := SplitOutsidePRFindings([]string{tc.finding}, diff)
			if got := len(outside) == 1; got != tc.outside {
				t.Fatalf("outside = %v (kept %q, outside %q), want %v", got, kept, outside, tc.outside)
			}
			if len(kept)+len(outside) != 1 {
				t.Fatalf("finding lost or duplicated: kept %q, outside %q", kept, outside)
			}
		})
	}
	if kept, outside := SplitOutsidePRFindings([]string{`{"file":"base/x.go","line":3}`}, ReviewScopeDiff{OwnFiles: []string{"own/y.go"}}); len(kept) != 1 || len(outside) != 0 {
		t.Fatalf("an unknown prior own diff dropped a finding: kept %q, outside %q", kept, outside)
	}
}

// #2682 round 2. A base merge whose round proves no own change moved is not an
// empty scope to approve unread: the engine records the loss of scope and
// re-reads the pull request against its merge base.
func TestFollowUpScopeEmptyAfterABaseMergeIsAFullReview(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{Decision: "approved"})
	engine.ReviewChangedFiles = func(context.Context, string, int, string, string, string) (ReviewScopeDiff, error) {
		return ReviewScopeDiff{MergeBase: "mb-two", BaseMoved: true, OwnFiles: []string{"own/y.go"}, PriorOwnLines: map[string][]ReviewLineRange{}}, nil
	}
	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	payload, err := unmarshalPayload(mustJob(t, store, "review-audit-task-1678-review-2").Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ReviewScope != nil || strings.Contains(payload.Instructions, "approve without rereading") {
		t.Fatalf("scope = %+v, instructions %q: want a full review", payload.ReviewScope, payload.Instructions)
	}
	if !hasReviewScopeUnavailableEvent(t, store, "head-two") {
		t.Fatal("the unscoped head left no review_scope_unavailable record")
	}
}

func mustJSONString(t *testing.T, value string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
