package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	stallHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	stallHeadB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var stallNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func greenStallFacts(age time.Duration) prStallFacts {
	at := stallNow.Add(-age)
	return prStallFacts{Repo: "owner/repo", PR: github.PullRequestFacts{
		Number: 7, Mergeable: "MERGEABLE", HeadSHA: stallHeadA, BaseRef: "main",
		CreatedAt: at, HeadCommittedAt: at,
		Checks: []github.PullRequestCheckFact{{Name: "ci", Bucket: "pass", StartedAt: at, CompletedAt: at}},
	}}
}

func TestClassifyStalledPR(t *testing.T) {
	policy := config.DefaultPRStalledPolicy()
	with := func(f prStallFacts, edit func(*prStallFacts)) prStallFacts {
		edit(&f)
		return f
	}
	recentGate := stallNow.Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	tests := []struct {
		name      string
		facts     prStallFacts
		wantState string // "" = not stalled
		wantCause string // a fragment the cause must name
	}{
		{name: "conflicting PR with no CI names the conflict", wantState: prStallNoCIOnHead, wantCause: "conflicting",
			facts: with(greenStallFacts(time.Hour), func(f *prStallFacts) { f.PR.Checks = nil; f.PR.Mergeable = "CONFLICTING" })},
		{name: "draft with no CI names the draft", wantState: prStallNoCIOnHead, wantCause: "draft",
			facts: with(greenStallFacts(time.Hour), func(f *prStallFacts) { f.PR.Checks = nil; f.PR.Draft = true })},
		{name: "no CI within the threshold is not yet stalled",
			facts: with(greenStallFacts(10*time.Minute), func(f *prStallFacts) { f.PR.Checks = nil })},
		{name: "required check red for 7h", wantState: prStallCIRed,
			facts: with(greenStallFacts(7*time.Hour), func(f *prStallFacts) { f.PR.Checks[0].Bucket = "fail" })},
		{name: "a failing check that is not required is not red",
			facts: with(greenStallFacts(7*time.Hour), func(f *prStallFacts) {
				f.PR.Checks[0].Bucket = "fail"
				f.Required, f.RequiredKnown = map[string]bool{"other": true}, true
				f.Level = reviewlevel.LevelRequired
				f.ReviewRunning = true
			})},
		{name: "level 3 green with no review at the head", wantState: prStallAwaitingReview,
			facts: with(greenStallFacts(2*time.Hour), func(f *prStallFacts) { f.Level = reviewlevel.LevelRequired })},
		{name: "no recorded level counts as level 3", wantState: prStallAwaitingReview, wantCause: "level unknown",
			facts: greenStallFacts(2 * time.Hour)},
		{name: "level 2 green is green_not_merged, never awaiting_review", wantState: prStallGreenNotMerged,
			facts: with(greenStallFacts(3*time.Hour), func(f *prStallFacts) { f.Level = reviewlevel.LevelBackground })},
		{name: "level 3 green with a review running at the head",
			facts: with(greenStallFacts(5*time.Hour), func(f *prStallFacts) { f.Level = reviewlevel.LevelRequired; f.ReviewRunning = true })},
		{name: "level 3 approved and green for 3h", wantState: prStallGreenNotMerged,
			facts: with(greenStallFacts(5*time.Hour), func(f *prStallFacts) {
				f.Level, f.Verdict, f.VerdictAt = reviewlevel.LevelRequired, "approved", stallNow.Add(-3*time.Hour)
			})},
		{name: "approved an hour ago is within the threshold",
			facts: with(greenStallFacts(5*time.Hour), func(f *prStallFacts) {
				f.Level, f.Verdict, f.VerdictAt = reviewlevel.LevelRequired, "approved", stallNow.Add(-time.Hour)
			})},
		{name: "an armed merge gate making progress",
			facts: with(greenStallFacts(3*time.Hour), func(f *prStallFacts) {
				f.Level = reviewlevel.LevelBackground
				f.MergeGate = &db.MergeGate{State: "pending", UpdatedAt: recentGate}
			})},
		{name: "draft for 13h", wantState: prStallDraftStale,
			facts: with(greenStallFacts(13*time.Hour), func(f *prStallFacts) { f.PR.Draft = true })},
		{name: "a hold at the head silences it",
			facts: with(greenStallFacts(13*time.Hour), func(f *prStallFacts) { f.PR.Draft = true; f.Hold = &prHold{Head: stallHeadA} })},
		{name: "a hold at an older head does not", wantState: prStallDraftStale,
			facts: with(greenStallFacts(13*time.Hour), func(f *prStallFacts) { f.PR.Draft = true; f.Hold = &prHold{Head: stallHeadB} })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stall, ok := classifyStalledPR(test.facts, policy, stallNow)
			if test.wantState == "" {
				if ok {
					t.Fatalf("classified %+v, want not stalled", stall)
				}
				return
			}
			if !ok || stall.State != test.wantState {
				t.Fatalf("classified %+v (stalled=%v), want %s", stall, ok, test.wantState)
			}
			if !strings.Contains(stall.Cause, test.wantCause) {
				t.Fatalf("cause %q does not name %q", stall.Cause, test.wantCause)
			}
		})
	}
}

// fakeStalledGitHub serves one repository's open pull requests and counts the
// GraphQL queries.
type fakeStalledGitHub struct {
	pulls   []github.PullRequestFacts
	queries int
}

func (f *fakeStalledGitHub) OpenPullRequestFacts(context.Context, github.Repository) (github.OpenPullRequestFacts, error) {
	f.queries++
	return github.OpenPullRequestFacts{PullRequests: append([]github.PullRequestFacts(nil), f.pulls...), Total: len(f.pulls), Cost: 2}, nil
}

func (f *fakeStalledGitHub) RequiredStatusCheckContexts(context.Context, github.Repository, string) ([]string, bool, error) {
	return nil, false, nil
}

func (f *fakeStalledGitHub) GetPullRequest(_ context.Context, _ github.Repository, number int64) (github.PullRequest, error) {
	for _, pull := range f.pulls {
		if int64(pull.Number) == number {
			return github.PullRequest{Number: number, State: "open", HeadSHA: pull.HeadSHA}, nil
		}
	}
	return github.PullRequest{}, io.EOF
}

func noCIPull(number int, head string, committed time.Time) github.PullRequestFacts {
	return github.PullRequestFacts{Number: number, Mergeable: "CONFLICTING", HeadSHA: head, BaseRef: "main",
		CreatedAt: committed, HeadCommittedAt: committed}
}

// stalledSweepFixture is blockedTaskRoutingFixture ("lane", child of "owner",
// is the most specific owner of gitmoot/gitmoot) with PR 7 implemented by
// "owner".
func stalledSweepFixture(t *testing.T) (string, *db.Store, *fakeStalledGitHub) {
	t.Helper()
	home, store := blockedTaskRoutingFixture(t)
	if err := store.CreateJob(context.Background(), db.Job{
		ID: "impl-7", Agent: "coder", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 7, ActingOrgRole: "owner"}),
	}); err != nil {
		t.Fatal(err)
	}
	return home, store, &fakeStalledGitHub{}
}

func runStalledSweep(t *testing.T, home string, store *db.Store, gh *fakeStalledGitHub, now time.Time) {
	t.Helper()
	ctx := context.Background()
	policy := config.DefaultPRStalledPolicy()
	policy.Enabled = true
	sweep := &prStalledSweep{newEnv: func() *prStalledEnv {
		return newPRStalledEnv(ctx, store, home, gh, newPRRequiredCheckCache())
	}}
	sink := &eventRuleSink{store: store, home: home}
	if err := sweepStalledPullRequests(ctx, store, sink, sweep, "gitmoot/gitmoot", policy, io.Discard, now); err != nil {
		t.Fatalf("sweepStalledPullRequests: %v", err)
	}
}

func TestStalledPRSweepAlertsTheAttributedSeat(t *testing.T) {
	home, store, gh := stalledSweepFixture(t)
	gh.pulls = []github.PullRequestFacts{noCIPull(7, stallHeadA, stallNow.Add(-time.Hour))}

	runStalledSweep(t, home, store, gh, stallNow)
	// owner implemented PR 7; lane is only the repo's most specific owner.
	if got, want := pendingWakeTargetRoles(t, store), []string{"jarvis", "owner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("wake targets = %v, want %v", got, want)
	}
	// Within the interval: no second query, so no second alert.
	runStalledSweep(t, home, store, gh, stallNow.Add(10*time.Minute))
	if gh.queries != 1 {
		t.Fatalf("GraphQL queries = %d within one interval, want 1", gh.queries)
	}
	if got := len(pendingWakeTargetRoles(t, store)); got != 2 {
		t.Fatalf("wakes after a second sweep within the interval = %d, want 2", got)
	}
}

func TestStalledPRSweepNudgesAtMostThreeTimesPerEpisode(t *testing.T) {
	home, store, gh := stalledSweepFixture(t)
	gh.pulls = []github.PullRequestFacts{noCIPull(7, stallHeadA, stallNow.Add(-time.Hour))}
	for i := range 6 {
		runStalledSweep(t, home, store, gh, stallNow.Add(time.Duration(i)*31*time.Minute))
	}
	if gh.queries != 6 {
		t.Fatalf("GraphQL queries = %d, want one per interval (6)", gh.queries)
	}
	if got := ownerWakes(t, store); got != blockedTaskMaxNudges {
		t.Fatalf("alerts to owner = %d over 6 intervals, want %d", got, blockedTaskMaxNudges)
	}

	// A new head is a new episode with its own ladder.
	gh.pulls = []github.PullRequestFacts{noCIPull(7, stallHeadB, stallNow.Add(3*time.Hour))}
	runStalledSweep(t, home, store, gh, stallNow.Add(4*time.Hour))
	if got := ownerWakes(t, store); got != blockedTaskMaxNudges+1 {
		t.Fatalf("alerts to owner after the head moved = %d, want %d", got, blockedTaskMaxNudges+1)
	}
	episodes, err := store.ListBlockedEpisodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, episode := range episodes {
		if strings.HasPrefix(episode.Subject, prStallEpisodePrefix) {
			subjects = append(subjects, episode.Subject)
		}
	}
	want := []string{prStallEpisodeSubject("gitmoot/gitmoot", 7, stallHeadB, prStallNoCIOnHead)}
	if !reflect.DeepEqual(subjects, want) {
		t.Fatalf("episodes = %v, want only the new head's %v", subjects, want)
	}
}

func ownerWakes(t *testing.T, store *db.Store) int {
	t.Helper()
	count := 0
	for _, role := range pendingWakeTargetRoles(t, store) {
		if role == "owner" {
			count++
		}
	}
	return count
}

func TestStalledPRDigestGoesToTheParentOncePerDay(t *testing.T) {
	home, store, gh := stalledSweepFixture(t)
	// PR 9 is lane's own; lane's parent is owner.
	if err := store.CreateJob(context.Background(), db.Job{
		ID: "impl-9", Agent: "coder", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 9, ActingOrgRole: "lane"}),
	}); err != nil {
		t.Fatal(err)
	}
	gh.pulls = []github.PullRequestFacts{noCIPull(9, stallHeadA, stallNow.Add(-time.Hour))}
	digestsTo := func(role string) int {
		t.Helper()
		messages, err := store.ListMessages(context.Background(), role, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, message := range messages {
			if message.Kind == "message" && message.Sender == db.MessageSystemSender && strings.Contains(message.Body, "gitmoot/gitmoot#9") {
				count++
			}
		}
		return count
	}
	digests := func() int { return digestsTo("owner") }
	steps := []struct {
		at   time.Duration
		want int
	}{
		{0, 0},                // first sight: every repo is swept before the first digest
		{31 * time.Minute, 1}, // the digest
		{62 * time.Minute, 1}, // within 24h
		{(24*60 + 32) * time.Minute, 2},
	}
	for _, step := range steps {
		runStalledSweep(t, home, store, gh, stallNow.Add(step.at))
		if got := digests(); got != step.want {
			t.Fatalf("digests to owner at +%s = %d, want %d", step.at, got, step.want)
		}
	}
	if got := digestsTo("lane"); got != 0 {
		t.Fatalf("digests to lane = %d, want none (the digest goes to the parent)", got)
	}
}

func TestPRStalledJSONAndHold(t *testing.T) {
	home, store, gh := stalledSweepFixture(t)
	at := time.Now().UTC().Add(-time.Hour)
	gh.pulls = []github.PullRequestFacts{noCIPull(7, stallHeadA, at)}
	previous := prStalledGitHubClient
	prStalledGitHubClient = func() prStalledGitHub { return gh }
	t.Cleanup(func() { prStalledGitHubClient = previous })
	_ = store.Close()
	t.Setenv("GITMOOT_ORG_ROLE", "owner")

	stalled := func() []prStalledReport {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"pr", "stalled", "--home", home, "--repo", "gitmoot/gitmoot", "--json"}, &stdout, &stderr); code != 0 {
			t.Fatalf("pr stalled exited %d: %s", code, stderr.String())
		}
		var reports []prStalledReport
		if err := json.Unmarshal(stdout.Bytes(), &reports); err != nil {
			t.Fatalf("decode %q: %v", stdout.String(), err)
		}
		if len(reports) != 1 {
			t.Fatalf("reports = %+v, want one", reports)
		}
		return reports
	}
	report := stalled()[0]
	if len(report.Stalled) != 1 || report.Stalled[0].PullRequest != 7 || report.Stalled[0].State != prStallNoCIOnHead ||
		report.Stalled[0].Owner != "owner" || report.Stalled[0].Next == "" || len(report.Held) != 0 {
		t.Fatalf("report = %+v, want #7 no_ci_on_head owned by owner", report)
	}

	hold := func(args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := Run(append([]string{"pr", "hold", "--home", home}, args...), &stdout, &stderr); code != 0 {
			t.Fatalf("pr hold %v exited %d: %s", args, code, stderr.String())
		}
	}
	hold("gitmoot/gitmoot#7", "--reason", "waiting on the vendor")
	report = stalled()[0]
	if len(report.Stalled) != 0 || len(report.Held) != 1 || report.Held[0].By != "owner" || report.Held[0].Head != stallHeadA {
		t.Fatalf("report after hold = %+v, want #7 held by owner at its head", report)
	}

	// The head moves: the hold no longer applies.
	gh.pulls = []github.PullRequestFacts{noCIPull(7, stallHeadB, at)}
	if report = stalled()[0]; len(report.Stalled) != 1 || len(report.Held) != 0 {
		t.Fatalf("report after the head moved = %+v, want #7 stalled again", report)
	}

	gh.pulls = []github.PullRequestFacts{noCIPull(7, stallHeadA, at)}
	hold("--clear", "gitmoot/gitmoot#7")
	if report = stalled()[0]; len(report.Stalled) != 1 || len(report.Held) != 0 {
		t.Fatalf("report after --clear = %+v, want #7 stalled", report)
	}
}
