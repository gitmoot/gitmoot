package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
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

func TestReviewLevelKeepsEveryDecision(t *testing.T) {
	gh := &reviewLevelFakeGitHub{files: []github.PullRequestFile{{Filename: "README.md", Patch: "+typo"}}}
	stubReviewLevel(t, gh, &reviewLevelCountingJudge{})
	home := t.TempDir()
	for _, pr := range []string{"7", "8"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"review", "level", "--repo", "gitmoot/gitmoot", "--pr", pr, "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
		}
	}
	raw, err := os.ReadFile(filepath.Join(config.PathsForHome(home).Home, reviewLevelLogName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want one per decision:\n%s", len(lines), raw)
	}
	var entry reviewLevelLogEntry
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Repo != "gitmoot/gitmoot" || entry.PR != 8 || entry.Level != "level3_required" || entry.Source != "repo" ||
		entry.HeadSHA != "0123456789abcdef0123456789abcdef01234567" || entry.Time == "" {
		t.Fatalf("entry = %+v", entry)
	}
	if info, _ := os.Stat(filepath.Join(config.PathsForHome(home).Home, reviewLevelLogName)); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}
