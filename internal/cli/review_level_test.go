package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/jev"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
)

type reviewLevelFakeGitHub struct {
	pullErr  error
	files    []github.PullRequestFile
	statuses []github.CommitStatusInput
}

func (f *reviewLevelFakeGitHub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	if f.pullErr != nil {
		return github.PullRequest{}, f.pullErr
	}
	return github.PullRequest{Title: "Change", HeadSHA: "0123456789abcdef0123456789abcdef01234567"}, nil
}

func (f *reviewLevelFakeGitHub) ListPullRequestFiles(context.Context, github.Repository, int64) ([]github.PullRequestFile, error) {
	return f.files, nil
}

func (f *reviewLevelFakeGitHub) CreateCommitStatus(_ context.Context, input github.CommitStatusInput) (github.CommitStatus, error) {
	f.statuses = append(f.statuses, input)
	return github.CommitStatus{}, nil
}

type reviewLevelCountingJudge struct{ calls int }

func (j *reviewLevelCountingJudge) Evaluate(context.Context, jev.Request) (jev.Exchange, error) {
	j.calls++
	return jev.Exchange{}, errors.New("not expected")
}

func stubReviewLevel(t *testing.T, gh *reviewLevelFakeGitHub, judge *reviewLevelCountingJudge) {
	t.Helper()
	prevGH, prevJudge := newReviewLevelGitHubClient, newReviewLevelJudge
	newReviewLevelGitHubClient = func() reviewLevelGitHubClient { return gh }
	newReviewLevelJudge = func(string) reviewlevel.Judge { return judge }
	t.Setenv(reviewLevelAPIKeyName, "test-key")
	t.Cleanup(func() { newReviewLevelGitHubClient, newReviewLevelJudge = prevGH, prevJudge })
}

func TestReviewLevelGitmootAlwaysRequiresReviewAndPostsStatus(t *testing.T) {
	gh := &reviewLevelFakeGitHub{files: []github.PullRequestFile{{Filename: "README.md", Patch: "+typo"}}}
	judge := &reviewLevelCountingJudge{}
	stubReviewLevel(t, gh, judge)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"review", "level", "--repo", "gitmoot/gitmoot", "--pr", "7"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "level: level3_required") || !strings.Contains(out, "reason: repo:") {
		t.Fatalf("stdout = %q", out)
	}
	if judge.calls != 0 {
		t.Fatalf("JEV called %d times for gitmoot/gitmoot", judge.calls)
	}
	if len(gh.statuses) != 1 || gh.statuses[0].Context != reviewLevelStatusContext ||
		gh.statuses[0].SHA != "0123456789abcdef0123456789abcdef01234567" ||
		!strings.HasPrefix(gh.statuses[0].Description, "L3 review before merge") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestReviewLevelGitHubReadFailurePrintsNoDecision(t *testing.T) {
	gh := &reviewLevelFakeGitHub{pullErr: errors.New("HTTP 404")}
	stubReviewLevel(t, gh, &reviewLevelCountingJudge{})
	var stdout, stderr bytes.Buffer
	code := Run([]string{"review", "level", "--repo", "jerryfane/joltra", "--pr", "7"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "review level: HTTP 404") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	if len(gh.statuses) != 0 {
		t.Fatalf("status posted without a decision: %+v", gh.statuses)
	}
}
