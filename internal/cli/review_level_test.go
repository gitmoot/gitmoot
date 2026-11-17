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
	"time"

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
	// Runs without --home use the user's home: keep them off the real one,
	// where each decision is now appended to review-levels.jsonl.
	t.Setenv("HOME", t.TempDir())
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
	defaultHome, _ := os.UserHomeDir()
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
	if _, err := os.Stat(filepath.Join(config.PathsForHome(defaultHome).Home, reviewLevelLogName)); err == nil {
		t.Fatal("a run with --home also wrote to the default home")
	}
	if info, _ := os.Stat(filepath.Join(config.PathsForHome(home).Home, reviewLevelLogName)); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

const reviewLevelEarlierHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// reviewLevelLevel2Judge judges every complete diff level 2 (risk p=0.12).
type reviewLevelLevel2Judge struct{ calls int }

func (j *reviewLevelLevel2Judge) Evaluate(context.Context, jev.Request) (jev.Exchange, error) {
	j.calls++
	risk := 0.12
	return jev.Exchange{Response: jev.Response{Answers: map[string]jev.Answer{
		"fixed_category": {Type: "noul", Noul: &risk},
		"level": {Type: "choice", Choice: reviewlevel.LevelBackground, Probabilities: map[string]float64{
			reviewlevel.LevelBackground: 0.9, reviewlevel.LevelRequired: 0.1,
		}},
	}}}, nil
}

type reviewLevelSeed struct {
	repo   string
	pr     int
	head   string
	level  string
	source string
}

func seedReviewLevelLog(t *testing.T, home string, seeds ...reviewLevelSeed) {
	t.Helper()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for _, seed := range seeds {
		decision := reviewlevel.Decision{Level: seed.level, Source: seed.source, Reason: "earlier " + seed.source, HeadSHA: seed.head}
		if err := appendReviewLevelLog(home, seed.repo, seed.pr, decision, at); err != nil {
			t.Fatal(err)
		}
	}
}

// A pull request's level never drops below an earlier real judgement of it
// (#2674); fail-closed results and other pull requests do not hold it, and an
// unreadable record fails closed.
func TestReviewLevelRatchet(t *testing.T) {
	const current = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name       string
		seeds      []reviewLevelSeed
		unreadable bool
		wantLevel  string
		wantSource string
		wantReason string
		wantStatus string
	}{
		{"earlier level 3 at another head holds",
			[]reviewLevelSeed{{"jerryfane/joltra", 7, reviewLevelEarlierHead, reviewlevel.LevelRequired, "risk"}}, false,
			reviewlevel.LevelRequired, "ratchet", "head aaaaaaaaaaaa", "ratchet: held by risk at aaaaaaaa"},
		{"same head re-judged lower stays",
			[]reviewLevelSeed{{"jerryfane/joltra", 7, current, reviewlevel.LevelRequired, "jev"}}, false,
			reviewlevel.LevelRequired, "ratchet", "head 0123456789ab", "ratchet: held by jev at 01234567"},
		{"classifier error does not hold",
			[]reviewLevelSeed{{"jerryfane/joltra", 7, reviewLevelEarlierHead, reviewlevel.LevelRequired, "classifier_error"}}, false,
			reviewlevel.LevelBackground, "jev", "", "L2 background review"},
		{"other pull requests do not hold",
			[]reviewLevelSeed{
				{"jerryfane/joltra", 8, reviewLevelEarlierHead, reviewlevel.LevelRequired, "risk"},
				{"jerryfane/other", 7, reviewLevelEarlierHead, reviewlevel.LevelRequired, "risk"},
			}, false,
			reviewlevel.LevelBackground, "jev", "", "L2 background review"},
		{"unreadable history fails closed", nil, true,
			reviewlevel.LevelRequired, "history_unreadable", "could not be read", "L3 review before merge (history_unreadable)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := &reviewLevelFakeGitHub{files: []github.PullRequestFile{{Filename: "README.md", Patch: "+typo"}}}
			judge := &reviewLevelLevel2Judge{}
			prevGH, prevJudge := newReviewLevelGitHubClient, newReviewLevelJudge
			newReviewLevelGitHubClient = func() reviewLevelGitHubClient { return gh }
			newReviewLevelJudge = func(string) reviewlevel.Judge { return judge }
			t.Cleanup(func() { newReviewLevelGitHubClient, newReviewLevelJudge = prevGH, prevJudge })
			t.Setenv(reviewLevelAPIKeyName, "test-key")
			home := t.TempDir()
			seedReviewLevelLog(t, home, tc.seeds...)
			logPath := filepath.Join(config.PathsForHome(home).Home, reviewLevelLogName)
			if len(tc.seeds) > 0 {
				// A malformed line is skipped, not fatal.
				f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.WriteString("{not json\n")
				_ = f.Close()
			}
			if tc.unreadable {
				if err := os.MkdirAll(logPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			var stdout, stderr bytes.Buffer
			if code := Run([]string{"review", "level", "--repo", "jerryfane/joltra", "--pr", "7", "--json", "--home", home}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
			var got map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("%v: %s", err, stdout.String())
			}
			reason, _ := got["reason"].(string)
			if got["level"] != tc.wantLevel || got["source"] != tc.wantSource || !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("decision = %s/%s (%s), want %s/%s naming %q", got["level"], got["source"], reason, tc.wantLevel, tc.wantSource, tc.wantReason)
			}
			if judge.calls != 1 {
				t.Fatalf("judge calls = %d, want the head judged once", judge.calls)
			}
			if len(gh.statuses) != 1 || !strings.Contains(gh.statuses[0].Description, tc.wantStatus) {
				t.Fatalf("statuses = %+v, want one naming %q", gh.statuses, tc.wantStatus)
			}
			if tc.wantSource != "ratchet" {
				return
			}
			// The fresh judgement stays visible, and the log records the held level.
			if judged, _ := got["judged"].(map[string]any); judged["level"] != reviewlevel.LevelBackground || judged["source"] != "jev" {
				t.Fatalf("judged = %v, want the fresh level 2 jev judgement", got["judged"])
			}
			if got["jev_choice"] != reviewlevel.LevelBackground {
				t.Fatalf("jev_choice = %v, want the fresh JEV choice", got["jev_choice"])
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			var last reviewLevelLogEntry
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
				t.Fatal(err)
			}
			if last.Level != reviewlevel.LevelRequired || last.Source != "ratchet" || last.HeadSHA != current || !strings.Contains(last.Reason, tc.wantReason) {
				t.Fatalf("logged %+v, want the held level 3", last)
			}
		})
	}
}

// reviewLevelRacingJudge is another writer racing the classification: while
// judging, it records a real level 3 for the same pull request, then judges
// level 2.
type reviewLevelRacingJudge struct {
	t    *testing.T
	home string
}

func (j *reviewLevelRacingJudge) Evaluate(ctx context.Context, request jev.Request) (jev.Exchange, error) {
	seedReviewLevelLog(j.t, j.home, reviewLevelSeed{"jerryfane/joltra", 7, reviewLevelEarlierHead, reviewlevel.LevelRequired, "risk"})
	return (&reviewLevelLevel2Judge{}).Evaluate(ctx, request)
}

// reviewLevelDeciders are both writers of review-levels.jsonl, deciding
// jerryfane/joltra#7 at lowRiskCLIHead.
var reviewLevelDeciders = []struct {
	name   string
	decide func(t *testing.T, home string, gh *reviewLevelFakeGitHub, judge reviewlevel.Judge) reviewlevel.Decision
}{
	{"review level", func(t *testing.T, home string, gh *reviewLevelFakeGitHub, judge reviewlevel.Judge) reviewlevel.Decision {
		prevGH, prevJudge := newReviewLevelGitHubClient, newReviewLevelJudge
		newReviewLevelGitHubClient = func() reviewLevelGitHubClient { return gh }
		newReviewLevelJudge = func(string) reviewlevel.Judge { return judge }
		t.Cleanup(func() { newReviewLevelGitHubClient, newReviewLevelJudge = prevGH, prevJudge })
		t.Setenv(reviewLevelAPIKeyName, "test-key")
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"review", "level", "--repo", "jerryfane/joltra", "--pr", "7", "--json", "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
		}
		var decision reviewlevel.Decision
		if err := json.Unmarshal(stdout.Bytes(), &decision); err != nil {
			t.Fatalf("%v: %s", err, stdout.String())
		}
		return decision
	}},
	{"low-risk merge gate", func(t *testing.T, home string, gh *reviewLevelFakeGitHub, judge reviewlevel.Judge) reviewlevel.Decision {
		classify := lowRiskReviewLevel(home, gh, func() reviewlevel.Judge { return judge }, time.Now)
		decision, err := classify(context.Background(), "jerryfane/joltra", 7, lowRiskCLIHead)
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}},
}

// A real level 3 recorded by another writer while this head was being
// classified holds the level: both writers re-read the record under its lock
// after judging, so neither returns nor logs the fresh level 2.
func TestReviewLevelSettlesAgainstLevelRecordedDuringClassification(t *testing.T) {
	for _, decider := range reviewLevelDeciders {
		t.Run(decider.name, func(t *testing.T) {
			home := t.TempDir()
			gh := &reviewLevelFakeGitHub{files: []github.PullRequestFile{{Filename: "README.md", Patch: "+typo"}}}
			decision := decider.decide(t, home, gh, &reviewLevelRacingJudge{t: t, home: home})
			if decision.Level != reviewlevel.LevelRequired || decision.Source != reviewlevel.SourceRatchet || !strings.Contains(decision.Reason, "head aaaaaaaaaaaa") {
				t.Fatalf("decision = %s/%s (%s), want level 3 held by the racing record", decision.Level, decision.Source, decision.Reason)
			}
			if len(gh.statuses) != 1 || !strings.Contains(gh.statuses[0].Description, "ratchet: held by risk at aaaaaaaa") {
				t.Fatalf("statuses = %+v, want one ratchet status", gh.statuses)
			}
			raw, err := os.ReadFile(filepath.Join(config.PathsForHome(home).Home, reviewLevelLogName))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			var last reviewLevelLogEntry
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
				t.Fatal(err)
			}
			if len(lines) != 2 || last.Level != reviewlevel.LevelRequired || last.Source != reviewlevel.SourceRatchet || last.HeadSHA != lowRiskCLIHead {
				t.Fatalf("log =\n%s\nwant the racing level 3, then this head held at level 3", raw)
			}
		})
	}
}

// While another writer holds the record lock past the bounded wait, a writer
// fails closed to level 3 and appends nothing.
func TestReviewLevelLockedRecordFailsClosed(t *testing.T) {
	prevWait := reviewLevelLockWait
	reviewLevelLockWait = 100 * time.Millisecond
	t.Cleanup(func() { reviewLevelLockWait = prevWait })
	for _, decider := range reviewLevelDeciders {
		t.Run(decider.name, func(t *testing.T) {
			home := t.TempDir()
			unlock, err := lockReviewLevelLog(home)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			gh := &reviewLevelFakeGitHub{files: []github.PullRequestFile{{Filename: "README.md", Patch: "+typo"}}}
			decision := decider.decide(t, home, gh, &reviewLevelLevel2Judge{})
			if decision.Level != reviewlevel.LevelRequired || decision.Source != reviewlevel.SourceHistoryLocked || !strings.Contains(decision.Reason, "could not be locked") {
				t.Fatalf("decision = %s/%s (%s), want a fail-closed history_locked level 3", decision.Level, decision.Source, decision.Reason)
			}
			if len(gh.statuses) != 1 || !strings.Contains(gh.statuses[0].Description, "history_locked") {
				t.Fatalf("statuses = %+v, want one history_locked status", gh.statuses)
			}
			if _, err := os.Stat(filepath.Join(config.PathsForHome(home).Home, reviewLevelLogName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("log stat err = %v, want nothing appended", err)
			}
		})
	}
}
