package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/jev"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const lowRiskCLIHead = "0123456789abcdef0123456789abcdef01234567"

type lowRiskFailingJudge struct{ calls int }

func (j *lowRiskFailingJudge) Evaluate(context.Context, jev.Request) (jev.Exchange, error) {
	j.calls++
	return jev.Exchange{}, errors.New("jev unreachable")
}

// A classifier failure fails closed (level 3) and is reused for the backoff
// window instead of calling the classifier again on every poll.
func TestLowRiskReviewLevelCachesClassifierFailurePerHead(t *testing.T) {
	home := t.TempDir()
	gh := &reviewLevelFakeGitHub{files: []github.PullRequestFile{{Filename: "README.md", Patch: "+typo"}}}
	judge := &lowRiskFailingJudge{}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	classify := lowRiskReviewLevel(home, gh, func() reviewlevel.Judge { return judge }, func() time.Time { return now })
	ctx := context.Background()

	for poll := range 3 {
		decision, err := classify(ctx, "gitmoot/test-check", 7, lowRiskCLIHead)
		if err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		if decision.Level != reviewlevel.LevelRequired || decision.Source != "classifier_error" {
			t.Fatalf("poll %d decision = %+v, want a fail-closed classifier_error", poll, decision)
		}
	}
	if judge.calls == 0 || judge.calls > jev.DefaultMaxRetries+1 {
		t.Fatalf("judge calls = %d, want one classification for three polls", judge.calls)
	}
	first := judge.calls
	now = now.Add(lowRiskClassifierRetryAfter)
	if _, err := classify(ctx, "gitmoot/test-check", 7, lowRiskCLIHead); err != nil {
		t.Fatal(err)
	}
	if judge.calls == first {
		t.Fatal("classifier not retried after the backoff window")
	}
}

// A head that moved while classifying is an error (leave open), not a level.
func TestLowRiskReviewLevelRefusesMovedHead(t *testing.T) {
	gh := &reviewLevelFakeGitHub{}
	classify := lowRiskReviewLevel(t.TempDir(), gh, func() reviewlevel.Judge { return &lowRiskFailingJudge{} }, time.Now)
	if _, err := classify(context.Background(), "gitmoot/test-check", 7, "ffffffffffffffffffffffffffffffffffffffff"); err == nil {
		t.Fatal("classification of a head that is not the pull request head succeeded")
	}
}

// A recorded exact-head decision is used without classifying again.
func TestLowRiskReviewLevelUsesRecordedExactHeadDecision(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if err := appendReviewLevelLog(home, "gitmoot/test-check", 7, reviewlevel.Decision{HeadSHA: lowRiskCLIHead, Level: reviewlevel.LevelNoReview, Source: "jev"}, now); err != nil {
		t.Fatal(err)
	}
	judge := &lowRiskFailingJudge{}
	classify := lowRiskReviewLevel(home, &reviewLevelFakeGitHub{}, func() reviewlevel.Judge { return judge }, func() time.Time { return now })
	decision, err := classify(context.Background(), "gitmoot/test-check", 7, lowRiskCLIHead)
	if err != nil || decision.Level != reviewlevel.LevelNoReview || judge.calls != 0 {
		t.Fatalf("decision = %+v err = %v calls = %d; want the recorded level 1", decision, err, judge.calls)
	}
	raw, _ := os.ReadFile(filepath.Join(config.PathsForHome(home).Home, reviewLevelLogName))
	var entry reviewLevelLogEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
}

// A level 2 auto-merge requests the post-merge review once, however often the
// daemon replays the merged decision.
func TestLowRiskPostMergeReviewRequestedOnce(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-12", RepoFullName: "owner/repo", Title: "t", State: "merged", Branch: "feature/review"}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", PullRequest: 12, HeadSHA: head, Branch: "feature/review", ActingOrgRole: "joltra"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "pre-merge-review", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded), Repo: "owner/repo", PullRequest: 12, Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return &reviewRoutingFixtureClient{head: head} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })
	gate := daemonMergeGate{Store: store, Home: home}
	request := workflow.MergeRequest{Repo: "owner/repo", PullRequest: 12, HeadSHA: head, TaskID: "task-12"}
	for range 2 {
		gate.requestLowRiskPostMergeReview(ctx, request, workflow.MergeDecision{Merged: true, ReviewLevel: reviewlevel.LevelBackground})
	}
	jobs, err := store.ListReviewJobsForPullRequest(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	postMerge := 0
	for _, job := range jobs {
		if parsed, err := workflow.ParseJobPayload(job.Payload); err == nil && parsed.PostMergeReview {
			postMerge++
		}
	}
	events, err := store.ListTaskEvents(ctx, "task-12")
	if err != nil {
		t.Fatal(err)
	}
	requested := 0
	for _, event := range events {
		if event.Kind == lowRiskPostMergeReviewEventKind {
			requested++
		}
		if event.Kind == lowRiskPostMergeReviewOwedEventKind {
			t.Fatalf("post-merge review recorded as owed: %s", event.Reason)
		}
	}
	if postMerge != 1 || requested != 1 {
		t.Fatalf("post-merge review jobs = %d, request events = %d; want exactly one of each", postMerge, requested)
	}
}

// The per-repo low_risk value reaches the daemon's gate as LowRiskOnly, and a
// plain repo keeps the global kill switch.
func TestResolvedMergeGatePolicyCarriesLowRisk(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+`
[merge_gate]
auto_merge = false

[repos."gitmoot/test-check".merge_gate]
auto_merge = "low_risk"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, ok := resolvedMergeGatePolicy(home, "gitmoot/test-check")
	var gate workflow.PolicyMergeGate
	applyResolvedMergeGatePolicy(&gate, policy)
	if !ok || !gate.AutoMerge || !gate.LowRiskOnly {
		t.Fatalf("gate = %+v ok = %v, want low-risk auto-merge", gate, ok)
	}
	if plain, ok := resolvedMergeGatePolicy(home, "jerryfane/noted"); !ok || plain.AutoMerge || plain.LowRiskOnly {
		t.Fatalf("plain repo policy = %+v, want the global kill switch", plain)
	}
	if !autoMergeEnabledResolver(home)("gitmoot/test-check") || autoMergeEnabledResolver(home)("jerryfane/noted") {
		t.Fatal("auto-merge resolver does not follow the per-repo low_risk opt-in")
	}
}
