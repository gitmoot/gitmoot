package issuetrace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

type fakeReader struct {
	mu          sync.Mutex
	issue       github.TraceIssue
	prs         map[int64]github.TracePullRequest
	refs        map[int64][]github.TraceCrossReference
	search      map[string][]github.TracePullRequest
	repository  github.TraceRepository
	comparisons map[string]github.CompareResult
	err         error
	issueCalls  int
	wait        chan struct{}
}

func (f *fakeReader) GetTraceIssue(ctx context.Context, _ github.Repository, _ int64) (github.TraceIssue, error) {
	f.mu.Lock()
	f.issueCalls++
	wait := f.wait
	err := f.err
	issue := f.issue
	f.mu.Unlock()
	if wait != nil {
		select {
		case <-ctx.Done():
			return github.TraceIssue{}, ctx.Err()
		case <-wait:
		}
	}
	return issue, err
}

func (f *fakeReader) GetTracePullRequest(_ context.Context, _ github.Repository, number int64) (github.TracePullRequest, error) {
	if f.err != nil {
		return github.TracePullRequest{}, f.err
	}
	return f.prs[number], nil
}

func (f *fakeReader) ListTraceCrossReferences(_ context.Context, _ github.Repository, number int64) ([]github.TraceCrossReference, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.refs[number], nil
}

func (f *fakeReader) SearchTracePullRequests(_ context.Context, _ github.Repository, text string) ([]github.TracePullRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.search[text], nil
}

func (f *fakeReader) GetTraceRepository(context.Context, github.Repository) (github.TraceRepository, error) {
	if f.err != nil {
		return github.TraceRepository{}, f.err
	}
	return f.repository, nil
}

func (f *fakeReader) CompareTraceCommit(_ context.Context, _ github.Repository, commit, branch string) (github.CompareResult, error) {
	if f.err != nil {
		return github.CompareResult{}, f.err
	}
	return f.comparisons[commit+"..."+branch], nil
}

type fakeLocal struct {
	tasks []db.Task
	prs   []db.PullRequest
	jobs  []db.Job
}

func (f fakeLocal) ListTasksByRepo(context.Context, string) ([]db.Task, error) {
	return f.tasks, nil
}
func (f fakeLocal) ListPullRequests(context.Context, string) ([]db.PullRequest, error) {
	return f.prs, nil
}
func (f fakeLocal) ListJobs(context.Context) ([]db.Job, error) {
	return f.jobs, nil
}

func TestIssue1113MergedCommitRemainsPartialEvidenceGolden(t *testing.T) {
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pr := tracePR(1122, "feat(daemon): shared host tool cache for isolated-worktree jobs (#1113)")
	pr.State = "closed"
	pr.CreatedAt = "2026-07-23T20:01:34Z"
	pr.MergedAt = "2026-07-24T03:14:05Z"
	pr.MergeSHA = "b2524ef"
	pr.Head.Ref = "fix/1113-tool-cache"
	pr.Head.SHA = "reviewed-head"
	remote := &fakeReader{
		issue: github.TraceIssue{
			Number: 1113, Title: "disk footprint has four independent causes", State: "closed",
			URL: "https://github.com/gitmoot/gitmoot/issues/1113",
		},
		prs: map[int64]github.TracePullRequest{1122: pr},
		refs: map[int64][]github.TraceCrossReference{
			1113: {crossReference(repo, pr)},
		},
		search:      map[string][]github.TracePullRequest{},
		repository:  github.TraceRepository{DefaultBranch: "main"},
		comparisons: map[string]github.CompareResult{"b2524ef...main": {Status: "ahead"}},
	}
	trace, err := (&Resolver{Remote: remote}).TraceIssue(context.Background(), repo, 1113)
	if err != nil {
		t.Fatalf("TraceIssue: %v", err)
	}
	if trace.Assessment != "partial_evidence" {
		t.Fatalf("assessment = %q, want partial_evidence", trace.Assessment)
	}
	if trace.Deployment.Status != "unknown" || trace.Deployment.Certainty != CertaintyUnknowable {
		t.Fatalf("deployment = %+v", trace.Deployment)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].MergeSHA != "b2524ef" {
		t.Fatalf("attempts = %+v", trace.Attempts)
	}
	raw, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "issue_1113.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatalf("golden mismatch\n--- got ---\n%s\n--- want ---\n%s", raw, want)
	}
}

func TestTraceIssueJoinsLocalRowsWithoutUsingJobPullRequest(t *testing.T) {
	repo := github.Repository{Owner: "o", Name: "r"}
	pr := tracePR(8, "Fix issue")
	pr.Head.Ref = "fix/eight"
	remote := &fakeReader{
		issue:       github.TraceIssue{Number: 7, State: "open"},
		prs:         map[int64]github.TracePullRequest{8: pr},
		refs:        map[int64][]github.TraceCrossReference{7: {crossReference(repo, pr)}},
		search:      map[string][]github.TracePullRequest{},
		repository:  github.TraceRepository{DefaultBranch: "main"},
		comparisons: map[string]github.CompareResult{},
	}
	local := fakeLocal{
		tasks: []db.Task{{ID: "task-8", RepoFullName: "o/r", Branch: "fix/eight", State: "blocked"}},
		prs:   []db.PullRequest{{RepoFullName: "o/r", Number: 8, HeadBranch: "fix/eight", State: "closed"}},
		jobs: []db.Job{
			{ID: "real", PullRequest: 999, Payload: `{"task_id":"task-8"}`, Type: "implement", State: "succeeded"},
			{ID: "conflated", PullRequest: 7, Payload: `{"task_id":"other-task"}`, Type: "ask", State: "succeeded"},
		},
	}
	trace, err := (&Resolver{Remote: remote, Local: local}).TraceIssue(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Tasks) != 1 || trace.Tasks[0].ID != "task-8" || len(trace.Tasks[0].Jobs) != 1 || trace.Tasks[0].Jobs[0].ID != "real" {
		t.Fatalf("local trace = %+v", trace.Tasks)
	}
}

func TestTraceIssueRemoteFailureIsExplicit(t *testing.T) {
	local := fakeLocal{
		tasks: []db.Task{{ID: "task-local", RepoFullName: "o/r", Branch: "fix/local", State: "blocked"}},
		prs:   []db.PullRequest{{RepoFullName: "o/r", Number: 10, HeadBranch: "fix/local", State: "closed"}},
	}
	trace, err := (&Resolver{Remote: &fakeReader{err: errors.New("authentication failed")}, Local: local}).
		TraceIssue(context.Background(), github.Repository{Owner: "o", Name: "r"}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !trace.RemoteUnavailable || !strings.Contains(trace.RefreshError, "authentication failed") {
		t.Fatalf("trace did not expose remote failure: %+v", trace)
	}
	if trace.Assessment != "partial_evidence" {
		t.Fatalf("assessment = %q", trace.Assessment)
	}
	if len(trace.UnlinkedLocal) != 2 {
		t.Fatalf("unlinked local evidence = %+v", trace.UnlinkedLocal)
	}
}

func TestTracePullRequestPrefersTrailingTitleIssueOverIncidentalBodyKeyword(t *testing.T) {
	repo := github.Repository{Owner: "o", Name: "r"}
	pr := tracePR(12, "Implement one part (#1113)")
	pr.Body = "This fixes #1 in the test harness."
	remote := &fakeReader{
		issue:       github.TraceIssue{Number: 1113, State: "open"},
		prs:         map[int64]github.TracePullRequest{12: pr},
		refs:        map[int64][]github.TraceCrossReference{1113: {crossReference(repo, pr)}},
		search:      map[string][]github.TracePullRequest{},
		repository:  github.TraceRepository{DefaultBranch: "main"},
		comparisons: map[string]github.CompareResult{},
	}
	trace, err := (&Resolver{Remote: remote}).TracePullRequest(context.Background(), repo, 12)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Issue.Number != 1113 {
		t.Fatalf("issue = #%d, want #1113", trace.Issue.Number)
	}
}

func TestTraceIssueTraversesSuccessorThatOnlyReferencesPredecessor(t *testing.T) {
	repo := github.Repository{Owner: "o", Name: "r"}
	first := tracePR(10, "First attempt")
	first.CreatedAt = "2026-01-01T00:00:00Z"
	successor := tracePR(12, "SUPERSEDE PR #10: replacement")
	successor.CreatedAt = "2026-01-02T00:00:00Z"
	remote := &fakeReader{
		issue: github.TraceIssue{Number: 9, State: "open"},
		prs: map[int64]github.TracePullRequest{
			10: first,
			12: successor,
		},
		refs: map[int64][]github.TraceCrossReference{
			9: {crossReference(repo, first)},
		},
		search: map[string][]github.TracePullRequest{
			`"SUPERSEDE PR #10"`: {successor},
		},
		repository:  github.TraceRepository{DefaultBranch: "main"},
		comparisons: map[string]github.CompareResult{},
	}
	trace, err := (&Resolver{Remote: remote}).TraceIssue(context.Background(), repo, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Attempts) != 2 || trace.Attempts[0].Number != 10 || trace.Attempts[1].Number != 12 {
		t.Fatalf("attempts = %+v", trace.Attempts)
	}
	if len(trace.Successors) != 1 || trace.Successors[0].From != 10 || trace.Successors[0].To != 12 ||
		trace.Successors[0].Certainty != CertaintyDeclared {
		t.Fatalf("successors = %+v", trace.Successors)
	}
}

func TestTraceIssuePreservesSuccessorForkAndWarnsOnCycle(t *testing.T) {
	repo := github.Repository{Owner: "o", Name: "r"}
	first := tracePR(10, "SUPERSEDE PR #12: cycle back")
	second := tracePR(12, "SUPERSEDE PR #10: replacement")
	fork := tracePR(13, "SUPERSEDE PR #10: alternate")
	remote := &fakeReader{
		issue: github.TraceIssue{Number: 9, State: "open"},
		prs: map[int64]github.TracePullRequest{
			10: first,
			12: second,
			13: fork,
		},
		refs: map[int64][]github.TraceCrossReference{
			9: {crossReference(repo, first)},
		},
		search: map[string][]github.TracePullRequest{
			`"SUPERSEDE PR #10"`: {second, fork},
			`"SUPERSEDE PR #12"`: {first},
		},
		repository:  github.TraceRepository{DefaultBranch: "main"},
		comparisons: map[string]github.CompareResult{},
	}
	trace, err := (&Resolver{Remote: remote}).TraceIssue(context.Background(), repo, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Successors) != 3 {
		t.Fatalf("successors = %+v", trace.Successors)
	}
	codes := map[string]bool{}
	for _, warning := range trace.Warnings {
		codes[warning.Code] = true
	}
	if !codes["successor_fork"] || !codes["successor_cycle"] {
		t.Fatalf("warnings = %+v", trace.Warnings)
	}
}

func TestTraceIssueCacheAndSingleflight(t *testing.T) {
	wait := make(chan struct{})
	remote := &fakeReader{
		issue:       github.TraceIssue{Number: 4},
		prs:         map[int64]github.TracePullRequest{},
		refs:        map[int64][]github.TraceCrossReference{},
		search:      map[string][]github.TracePullRequest{},
		repository:  github.TraceRepository{DefaultBranch: "main"},
		comparisons: map[string]github.CompareResult{},
		wait:        wait,
	}
	resolver := &Resolver{Remote: remote, TTL: time.Hour}
	repo := github.Repository{Owner: "o", Name: "r"}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := resolver.TraceIssue(context.Background(), repo, 4)
			errs <- err
		}()
	}
	deadline := time.Now().Add(time.Second)
	for {
		remote.mu.Lock()
		calls := remote.issueCalls
		remote.mu.Unlock()
		if calls == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("issue calls = %d, want 1 in flight", calls)
		}
		time.Sleep(time.Millisecond)
	}
	close(wait)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := resolver.TraceIssue(context.Background(), repo, 4); err != nil {
		t.Fatal(err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.issueCalls != 1 {
		t.Fatalf("issue calls = %d, want one coalesced and cached call", remote.issueCalls)
	}
}

func tracePR(number int64, title string) github.TracePullRequest {
	return github.TracePullRequest{
		Number: number, Title: title, State: "open",
		URL: "https://github.com/gitmoot/gitmoot/pull/" + strconv.FormatInt(number, 10),
	}
}

func crossReference(repo github.Repository, pr github.TracePullRequest) github.TraceCrossReference {
	var ref github.TraceCrossReference
	ref.Source.Issue.Number = pr.Number
	ref.Source.Issue.Title = pr.Title
	ref.Source.Issue.Body = pr.Body
	ref.Source.Issue.URL = pr.URL
	ref.Source.Issue.RepositoryURL = "https://api.github.com/repos/" + repo.FullName()
	ref.Source.Issue.PullRequest = json.RawMessage(`{}`)
	return ref
}
