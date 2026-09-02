package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestForeignBootRecoveryLoopIndependentOfSlowPollInterval(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("preseed migrated store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close preseeded store: %v", err)
	}

	const slowPollInterval = 5 * time.Minute
	if slowPollInterval <= foreignBootRecoveryInterval {
		t.Fatalf("test poll interval = %s, want slower than recovery interval %s", slowPollInterval, foreignBootRecoveryInterval)
	}

	ticks := make(chan time.Time, 1)
	tickerStarted := make(chan time.Duration, 1)
	tickerStopped := make(chan struct{})
	originalTicker := newForeignBootRecoveryTicker
	newForeignBootRecoveryTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		tickerStarted <- interval
		return ticks, func() { close(tickerStopped) }
	}
	originalRecovery := recoverForeignBootRunnersForSweep
	recoveryCalls := make(chan struct{}, 3)
	recoverForeignBootRunnersForSweep = func(context.Context, *db.Store, io.Writer) error {
		recoveryCalls <- struct{}{}
		return nil
	}
	t.Cleanup(func() {
		newForeignBootRecoveryTicker = originalTicker
		recoverForeignBootRunnersForSweep = originalRecovery
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runRegisteredRepoSupervisor(ctx, home, newDaemonReloadableConfig(slowPollInterval, 1, false), false, false, "", io.Discard)
	}()

	select {
	case <-recoveryCalls:
		// The eager startup pass is intentionally unchanged.
	case err := <-done:
		t.Fatalf("supervisor stopped before eager startup recovery: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for eager startup recovery")
	}
	select {
	case interval := <-tickerStarted:
		if interval != foreignBootRecoveryInterval {
			t.Fatalf("foreign-boot ticker interval = %s, want %s", interval, foreignBootRecoveryInterval)
		}
	case err := <-done:
		t.Fatalf("supervisor stopped before starting recovery ticker: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for foreign-boot recovery ticker")
	}

	// The supervisor's repo-poll loop is configured to sleep for five minutes,
	// but its independent recovery ticker can fire immediately in the test. The
	// second call therefore proves recovery is no longer waiting on that loop.
	ticks <- time.Now().UTC()
	select {
	case <-recoveryCalls:
	case err := <-done:
		t.Fatalf("supervisor stopped before ticker-driven recovery: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("ticker-driven recovery did not run while poll interval was five minutes")
	}

	select {
	case <-recoveryCalls:
		t.Fatal("unexpected third recovery call without another ticker event")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("supervisor returned %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for supervisor shutdown")
	}
	select {
	case <-tickerStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("foreign-boot recovery ticker was not stopped on shutdown")
	}
}

func TestPollRegisteredReposHonorsPerRepoIntervals(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	slowCheckout, fastCheckout := t.TempDir(), t.TempDir()
	repos := []db.Repo{
		{Owner: "owner", Name: "slow", CheckoutPath: slowCheckout, PollInterval: "1h"},
		{Owner: "owner", Name: "fast", CheckoutPath: fastCheckout, PollInterval: "30s"},
	}
	for _, repo := range repos {
		if err := store.UpsertRepo(ctx, repo); err != nil {
			t.Fatalf("UpsertRepo(%s) returned error: %v", repo.FullName(), err)
		}
	}

	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	nextPoll := map[string]time.Time{}
	schedule := registeredRepoSchedule{NextPoll: nextPoll}
	var stdout bytes.Buffer
	poller := defaultRegisteredRepoPoller(store, 1, true, &stdout, "", "")
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now, 30*time.Second); err != nil {
		t.Fatalf("first pollRegisteredReposWithPoller returned error: %v", err)
	}
	firstSlow, err := store.GetRepo(ctx, "owner/slow")
	if err != nil {
		t.Fatalf("GetRepo slow: %v", err)
	}
	firstFast, err := store.GetRepo(ctx, "owner/fast")
	if err != nil {
		t.Fatalf("GetRepo fast: %v", err)
	}

	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now.Add(31*time.Second), 30*time.Second); err != nil {
		t.Fatalf("second pollRegisteredReposWithPoller returned error: %v", err)
	}
	secondSlow, err := store.GetRepo(ctx, "owner/slow")
	if err != nil {
		t.Fatalf("GetRepo slow after second poll: %v", err)
	}
	secondFast, err := store.GetRepo(ctx, "owner/fast")
	if err != nil {
		t.Fatalf("GetRepo fast after second poll: %v", err)
	}

	if secondSlow.LastPollAt != firstSlow.LastPollAt {
		t.Fatalf("slow repo was polled too soon: first=%s second=%s", firstSlow.LastPollAt, secondSlow.LastPollAt)
	}
	if secondFast.LastPollAt == firstFast.LastPollAt {
		t.Fatalf("fast repo was not polled on its interval: %s", secondFast.LastPollAt)
	}
}

func TestPollRegisteredReposRoutesEachRepoWithOwnGitHubClient(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	repoA := github.Repository{Owner: "owner", Name: "repo-a"}
	repoB := github.Repository{Owner: "owner", Name: "repo-b"}
	checkoutA, checkoutB := t.TempDir(), t.TempDir()
	for _, repo := range []db.Repo{
		{Owner: repoA.Owner, Name: repoA.Name, CheckoutPath: checkoutA, PollInterval: "30s"},
		{Owner: repoB.Owner, Name: repoB.Name, CheckoutPath: checkoutB, PollInterval: "30s"},
	} {
		if err := store.UpsertRepo(ctx, repo); err != nil {
			t.Fatalf("UpsertRepo(%s) returned error: %v", repo.FullName(), err)
		}
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repoA.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	if err := store.AllowAgentRepo(ctx, "audit", repoB.FullName()); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}

	clients := map[string]*cliPollFakeGitHub{
		checkoutA: {
			pulls: []github.PullRequest{{Number: 1, Title: "A", State: "open", HeadRef: "task-a", BaseRef: "main", HeadSHA: "sha-a"}},
			comments: map[int64][]github.IssueComment{
				1: {{ID: 77, Body: "/gitmoot audit review check repo a", Author: "alice"}},
			},
		},
		checkoutB: {
			pulls: []github.PullRequest{{Number: 1, Title: "B", State: "open", HeadRef: "task-b", BaseRef: "main", HeadSHA: "sha-b"}},
			comments: map[int64][]github.IssueComment{
				1: {{ID: 77, Body: "/gitmoot audit review check repo b", Author: "alice"}},
			},
		},
	}
	poller := defaultRegisteredRepoPoller(store, 2, false, io.Discard, "", "")
	poller.GitHubClient = func(checkout string) github.Client { return clients[checkout] }
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }

	if _, err := pollRegisteredReposWithPoller(ctx, poller, registeredRepoSchedule{}, time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC), 30*time.Second); err != nil {
		t.Fatalf("pollRegisteredReposWithPoller returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v, want two repo-scoped jobs", jobs)
	}
	seenRepos := map[string]bool{}
	for _, job := range jobs {
		var payload workflow.JobPayload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			t.Fatalf("unmarshal job payload %s: %v", job.ID, err)
		}
		seenRepos[payload.Repo] = true
		if payload.Repo == repoA.FullName() && (payload.Branch != "task-a" || payload.Instructions != "check repo a") {
			t.Fatalf("repo A payload = %+v", payload)
		}
		if payload.Repo == repoB.FullName() && (payload.Branch != "task-b" || payload.Instructions != "check repo b") {
			t.Fatalf("repo B payload = %+v", payload)
		}
	}
	if !seenRepos[repoA.FullName()] || !seenRepos[repoB.FullName()] {
		t.Fatalf("job payload repos = %+v, want both repos", seenRepos)
	}
	for path, client := range clients {
		if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "queued `review` job") {
			t.Fatalf("posted acknowledgements for %s = %+v", path, client.posted)
		}
	}
	for _, repo := range []github.Repository{repoA, repoB} {
		seen, err := store.HasCommentSeen(ctx, repo.FullName(), 77)
		if err != nil {
			t.Fatalf("HasCommentSeen(%s) returned error: %v", repo.FullName(), err)
		}
		if !seen {
			t.Fatalf("comment 77 was not marked seen for %s", repo.FullName())
		}
	}
}

func TestPollRegisteredReposBacksOffFailedRepoWithoutStoppingOthers(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	failingCheckout, healthyCheckout := t.TempDir(), t.TempDir()
	for _, repo := range []db.Repo{
		{Owner: "owner", Name: "failing", CheckoutPath: failingCheckout, PollInterval: "30s"},
		{Owner: "owner", Name: "healthy", CheckoutPath: healthyCheckout, PollInterval: "30s"},
	} {
		if err := store.UpsertRepo(ctx, repo); err != nil {
			t.Fatalf("UpsertRepo(%s) returned error: %v", repo.FullName(), err)
		}
	}
	failing := &cliPollFakeGitHub{listErr: errors.New("rate limited")}
	healthy := &cliPollFakeGitHub{}
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "", "")
	poller.GitHubClient = func(checkout string) github.Client {
		if checkout == failingCheckout {
			return failing
		}
		return healthy
	}
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }
	schedule := registeredRepoSchedule{
		NextPoll:    map[string]time.Time{},
		ErrorStreak: map[string]int{},
	}
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	wait, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now, 30*time.Second)
	if err != nil {
		t.Fatalf("first pollRegisteredReposWithPoller returned error: %v", err)
	}
	if wait != 30*time.Second {
		t.Fatalf("wait = %s, want healthy repo interval 30s", wait)
	}
	if got := schedule.NextPoll["owner/failing"].Sub(now); got != time.Minute {
		t.Fatalf("failing repo next poll = %s, want 1m backoff", got)
	}
	if got := schedule.NextPoll["owner/healthy"].Sub(now); got != 30*time.Second {
		t.Fatalf("healthy repo next poll = %s, want 30s", got)
	}
	failingRepo, err := store.GetRepo(ctx, "owner/failing")
	if err != nil {
		t.Fatalf("GetRepo failing returned error: %v", err)
	}
	healthyRepo, err := store.GetRepo(ctx, "owner/healthy")
	if err != nil {
		t.Fatalf("GetRepo healthy returned error: %v", err)
	}
	if !strings.Contains(failingRepo.LastError, "rate limited") {
		t.Fatalf("failing repo last_error = %q", failingRepo.LastError)
	}
	if healthyRepo.LastError != "" {
		t.Fatalf("healthy repo last_error = %q, want empty", healthyRepo.LastError)
	}

	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now.Add(31*time.Second), 30*time.Second); err != nil {
		t.Fatalf("second pollRegisteredReposWithPoller returned error: %v", err)
	}
	if failing.listPullRequestsCalls != 1 {
		t.Fatalf("failing ListPullRequests calls = %d, want still backed off at 1", failing.listPullRequestsCalls)
	}
	if healthy.listPullRequestsCalls != 2 {
		t.Fatalf("healthy ListPullRequests calls = %d, want 2", healthy.listPullRequestsCalls)
	}
}

func TestPollRegisteredReposAdaptiveIdleCadence(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "idle", CheckoutPath: t.TempDir(), PollInterval: "1s"}); err != nil {
		t.Fatal(err)
	}
	stats := []github.ConditionalRequestStats{
		{Calls: 1},
		{Calls: 1},
		{Calls: 1},
		{Calls: 1},
		{Calls: 1, Misses: 1},
	}
	index := 0
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "", "")
	poller.GitHubClient = func(string) github.Client {
		client := &cliPollFakeGitHub{conditionalStats: stats[index]}
		index++
		return client
	}
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }
	schedule := registeredRepoSchedule{NextPoll: map[string]time.Time{}, ErrorStreak: map[string]int{}, IdleStreak: map[string]int{}}
	start := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	steps := []struct {
		at         time.Duration
		wantStreak int
		wantNext   time.Duration
	}{
		{at: 0, wantStreak: 1, wantNext: time.Second},
		{at: time.Second, wantStreak: 2, wantNext: time.Second},
		{at: 2 * time.Second, wantStreak: 3, wantNext: 2 * time.Second},
		{at: 4 * time.Second, wantStreak: 4, wantNext: 4 * time.Second},
		{at: 8 * time.Second, wantStreak: 0, wantNext: time.Second},
	}
	for _, step := range steps {
		now := start.Add(step.at)
		wait, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now, time.Second)
		if err != nil {
			t.Fatalf("poll at %s: %v", step.at, err)
		}
		if got := schedule.IdleStreak["owner/idle"]; got != step.wantStreak {
			t.Fatalf("at %s streak=%d, want %d", step.at, got, step.wantStreak)
		}
		if got := schedule.NextPoll["owner/idle"].Sub(now); got != step.wantNext {
			t.Fatalf("at %s next=%s, want %s", step.at, got, step.wantNext)
		}
		if wait != step.wantNext {
			t.Fatalf("at %s wait=%s, want %s", step.at, wait, step.wantNext)
		}
	}
}

func TestPollRegisteredReposIdleErrorBackoffTakesPrecedence(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: t.TempDir(), PollInterval: "1s"}); err != nil {
		t.Fatal(err)
	}
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "", "")
	poller.GitHubClient = func(string) github.Client {
		return &cliPollFakeGitHub{listErr: errors.New("rate limited"), conditionalStats: github.ConditionalRequestStats{Calls: 1, Misses: 1}}
	}
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }
	schedule := registeredRepoSchedule{
		NextPoll:    map[string]time.Time{},
		ErrorStreak: map[string]int{},
		IdleStreak:  map[string]int{"owner/repo": 8},
	}
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := schedule.NextPoll["owner/repo"].Sub(now); got != 2*time.Second {
		t.Fatalf("error next interval=%s, want 2s backoff without idle multiplier", got)
	}
	if got := schedule.IdleStreak["owner/repo"]; got != 0 {
		t.Fatalf("idle streak after error=%d, want reset", got)
	}
}

func TestPollRegisteredReposQueuedJobPromotesImmediately(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: t.TempDir(), PollInterval: "1s"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "queued", Agent: "agent", Type: "ask", State: string(workflow.JobQueued), Payload: `{"repo":"owner/repo"}`}); err != nil {
		t.Fatal(err)
	}
	client := &cliPollFakeGitHub{conditionalStats: github.ConditionalRequestStats{Calls: 1}}
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "", "")
	poller.GitHubClient = func(string) github.Client { return client }
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }
	start := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	schedule := registeredRepoSchedule{
		NextPoll:    map[string]time.Time{"owner/repo": start.Add(4 * time.Second)},
		ErrorStreak: map[string]int{},
		IdleStreak:  map[string]int{"owner/repo": 6},
	}
	now := start.Add(time.Second)
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now, time.Second); err != nil {
		t.Fatal(err)
	}
	if client.listPullRequestsCalls != 1 {
		t.Fatalf("poll calls=%d, want immediate poll", client.listPullRequestsCalls)
	}
	if got := schedule.IdleStreak["owner/repo"]; got != 0 {
		t.Fatalf("idle streak=%d, want reset", got)
	}
	if got := schedule.NextPoll["owner/repo"].Sub(now); got != time.Second {
		t.Fatalf("next interval=%s, want base 1s", got)
	}
}

// A queued job must never override ERROR backoff: promotion is an exit from
// idle decay only. Otherwise queued local work (which commonly piles up during
// GitHub outages) would hammer the failing API at base cadence forever.
func TestPollRegisteredReposQueuedJobDoesNotOverrideErrorBackoff(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: t.TempDir(), PollInterval: "1s"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "queued", Agent: "agent", Type: "ask", State: string(workflow.JobQueued), Payload: `{"repo":"owner/repo"}`}); err != nil {
		t.Fatal(err)
	}
	client := &cliPollFakeGitHub{conditionalStats: github.ConditionalRequestStats{Calls: 1}}
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "", "")
	poller.GitHubClient = func(string) github.Client { return client }
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }
	start := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	backoffUntil := start.Add(4 * time.Second)
	schedule := registeredRepoSchedule{
		NextPoll:    map[string]time.Time{"owner/repo": backoffUntil},
		ErrorStreak: map[string]int{"owner/repo": 2},
		IdleStreak:  map[string]int{},
	}
	now := start.Add(time.Second)
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now, time.Second); err != nil {
		t.Fatal(err)
	}
	if client.listPullRequestsCalls != 0 {
		t.Fatalf("poll calls=%d, want 0: queued job must not override error backoff", client.listPullRequestsCalls)
	}
	if got := schedule.NextPoll["owner/repo"]; !got.Equal(backoffUntil) {
		t.Fatalf("next poll=%s, want untouched backoff %s", got, backoffUntil)
	}
}

// Promotion is a one-shot exit from decay: a repo already at base cadence with
// queued/busy work keeps its configured interval instead of being re-polled on
// every supervisor tick (which would multiply API calls while jobs run).
func TestPollRegisteredReposBusyRepoAtBaseCadenceKeepsInterval(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: t.TempDir(), PollInterval: "1s"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "queued", Agent: "agent", Type: "ask", State: string(workflow.JobQueued), Payload: `{"repo":"owner/repo"}`}); err != nil {
		t.Fatal(err)
	}
	client := &cliPollFakeGitHub{conditionalStats: github.ConditionalRequestStats{Calls: 1}}
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "", "")
	poller.GitHubClient = func(string) github.Client { return client }
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }
	start := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	dueAt := start.Add(900 * time.Millisecond)
	schedule := registeredRepoSchedule{
		NextPoll:    map[string]time.Time{"owner/repo": dueAt},
		ErrorStreak: map[string]int{},
		IdleStreak:  map[string]int{},
	}
	now := start.Add(500 * time.Millisecond)
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, now, time.Second); err != nil {
		t.Fatal(err)
	}
	if client.listPullRequestsCalls != 0 {
		t.Fatalf("poll calls=%d, want 0: base-cadence repo with queued work polls at its interval, not every tick", client.listPullRequestsCalls)
	}
	if got := schedule.NextPoll["owner/repo"]; !got.Equal(dueAt) {
		t.Fatalf("next poll=%s, want untouched %s", got, dueAt)
	}
}

func TestPollRegisteredReposTreats304AsSuccess(t *testing.T) {
	github.ConfigureConditional(true)
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "etag304", Name: "repo", CheckoutPath: t.TempDir(), PollInterval: "1s"}); err != nil {
		t.Fatal(err)
	}
	runner := &cliETagRunner{}
	limiter := github.NewRateLimiter(github.RateLimiterConfig{})
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "", "")
	poller.GitHubClient = func(string) github.Client { return &github.GhClient{Runner: runner, Limiter: limiter} }
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }
	schedule := registeredRepoSchedule{NextPoll: map[string]time.Time{}, ErrorStreak: map[string]int{}, IdleStreak: map[string]int{}}
	start := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, start, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, start.Add(time.Second), time.Second); err != nil {
		t.Fatal(err)
	}
	if len(schedule.ErrorStreak) != 0 {
		t.Fatalf("304 populated ErrorStreak: %+v", schedule.ErrorStreak)
	}
	repo, err := store.GetRepo(ctx, "etag304/repo")
	if err != nil {
		t.Fatal(err)
	}
	if repo.LastError != "" {
		t.Fatalf("304 last_error=%q, want empty", repo.LastError)
	}
	if got := limiter.Snapshot().CallsInLastHour; got != 2 {
		t.Fatalf("limiter calls=%d, want 2", got)
	}
}

func TestRepoIdleMultiplier(t *testing.T) {
	for _, tc := range []struct {
		streak, grace, max, want int
	}{
		{0, 3, 4, 1}, {2, 3, 4, 1}, {3, 3, 4, 2}, {4, 3, 4, 4},
		{20, 3, 4, 4}, {20, 3, 1, 1}, {5, 4, 3, 3},
	} {
		if got := repoIdleMultiplier(tc.streak, tc.grace, tc.max); got != tc.want {
			t.Fatalf("repoIdleMultiplier(%d,%d,%d)=%d, want %d", tc.streak, tc.grace, tc.max, got, tc.want)
		}
	}
}

func TestStartSupervisorWorkerLoopRecoveringRetriesAfterRunError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	var mu sync.Mutex
	errCh := startSupervisorWorkerLoopRecovering(ctx, time.Millisecond, io.Discard, func(time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		cancel()
		return nil
	})

	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			t.Fatalf("recovering worker loop reported error = %v, want silent retry", err)
		}
	case <-time.After(time.Second):
		t.Fatal("recovering worker loop did not retry after the run error")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("recovering worker loop calls = %d, want at least 2", calls)
	}
}

// A registered repo whose checkout directory has vanished must be reported and
// SKIPPED (#1758): before the guard it still paid for engine construction, DB
// passes, a config parse and a `gh` exec that could only fail on chdir, on every
// tick forever. The factory counter is the load-bearing assertion — a version
// that logs the problem and then polls anyway would pass a log-only check.
func TestPollRepoSkipsRepoWhoseCheckoutIsMissing(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "went-away")
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "gone", CheckoutPath: missing, PollInterval: "1s"}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	engines, clients := 0, 0
	poller := defaultRegisteredRepoPoller(store, 1, false, &stdout, "", "")
	poller.GitHubClient = func(string) github.Client { clients++; return &cliPollFakeGitHub{} }
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine {
		engines++
		return nil
	}
	schedule := registeredRepoSchedule{NextPoll: map[string]time.Time{}, ErrorStreak: map[string]int{}, IdleStreak: map[string]int{}}
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, time.Now().UTC(), time.Second); err != nil {
		t.Fatalf("poll pass: %v", err)
	}
	if engines != 0 || clients != 0 {
		t.Fatalf("missing checkout still built %d engines and %d github clients, want 0 and 0", engines, clients)
	}
	if !strings.Contains(stdout.String(), "owner/gone: registered repo checkout path is unavailable") {
		t.Fatalf("poll log = %q, want the unavailable-checkout report", stdout.String())
	}
	if strings.Contains(stdout.String(), "polling owner/gone") {
		t.Fatalf("missing checkout was polled anyway: %q", stdout.String())
	}
	repos, err := store.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || !strings.Contains(repos[0].LastError, "checkout path is unavailable") {
		t.Fatalf("repos = %+v, want last_error naming the unavailable checkout", repos)
	}
}

// The poll line echoed a fleet-wide --workers flag as if it were a per-repo pool
// (#1758). No such pool exists, so the number is removed rather than corrected.
func TestPollRepoLogDoesNotClaimAPerRepoWorkerPool(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "live", CheckoutPath: t.TempDir(), PollInterval: "1s"}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	poller := defaultRegisteredRepoPoller(store, 32, false, &stdout, "", "")
	poller.GitHubClient = func(string) github.Client { return &cliPollFakeGitHub{} }
	poller.WorkflowFactory = func(*db.Store, github.Client, string) *workflow.Engine { return nil }
	schedule := registeredRepoSchedule{NextPoll: map[string]time.Time{}, ErrorStreak: map[string]int{}, IdleStreak: map[string]int{}}
	if _, err := pollRegisteredReposWithPoller(ctx, poller, schedule, time.Now().UTC(), time.Second); err != nil {
		t.Fatalf("poll pass: %v", err)
	}
	if !strings.Contains(stdout.String(), "polling owner/live dry_run=false") {
		t.Fatalf("poll log = %q, want the worker-free poll line", stdout.String())
	}
	if strings.Contains(stdout.String(), "workers") {
		t.Fatalf("poll log still advertises a per-repo worker pool: %q", stdout.String())
	}
}

// The per-tick config memo must read config.toml ONCE per tick and re-read it on
// the next one: rewriting the file between two calls on the same cache must be
// invisible, and visible immediately after reset(). That is the same measurement
// as counting opens, without depending on strace.
func TestTickConfigCacheReadsConfigOncePerTick(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	write := func(ttl string) {
		if err := os.WriteFile(paths.ConfigFile, []byte("[orchestrate]\nblocked_ttl = \""+ttl+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("48h")
	cache := &tickConfigCache{}
	if got := cache.blockedJobTTL(paths.Home); got != 48*time.Hour {
		t.Fatalf("blockedJobTTL = %s, want 48h", got)
	}
	write("1h")
	if got := cache.blockedJobTTL(paths.Home); got != 48*time.Hour {
		t.Fatalf("blockedJobTTL re-read config.toml mid-tick: got %s, want the memoized 48h", got)
	}
	// A different home must never be served another home's policy.
	other := t.TempDir()
	otherPaths := config.PathsForHome(other)
	if err := config.Initialize(otherPaths); err != nil {
		t.Fatal(err)
	}
	if got := cache.blockedJobTTL(otherPaths.Home); got != 0 {
		t.Fatalf("a second home got %s, want its own (unset ⇒ 0) policy", got)
	}
	cache.reset()
	if got := cache.blockedJobTTL(paths.Home); got != time.Hour {
		t.Fatalf("blockedJobTTL after reset = %s, want the re-read 1h", got)
	}

	// A nil cache is the un-memoized path and must see every write immediately.
	var uncached *tickConfigCache
	write("2h")
	if got := uncached.blockedJobTTL(paths.Home); got != 2*time.Hour {
		t.Fatalf("nil cache blockedJobTTL = %s, want 2h", got)
	}

	// Every other memoized resolver must behave the same way, or the hoist only
	// covers whichever one the test happened to exercise.
	writePolicy := func(wake, retention string) {
		body := "[orchestrate]\nblocked_role_wake_after = \"" + wake + "\"\n[workflow]\ndelegation_worktree_ttl = \"" + retention + "\"\n"
		if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writePolicy("3h", "24h")
	fresh := &tickConfigCache{}
	if got := fresh.blockedRoleWakeAfter(paths.Home); got != 3*time.Hour {
		t.Fatalf("blockedRoleWakeAfter = %s, want 3h", got)
	}
	retention, err := fresh.delegationWorktreeTTL(paths.Home)
	if err != nil || retention != 24*time.Hour {
		t.Fatalf("delegationWorktreeTTL = %s err=%v, want 24h", retention, err)
	}
	writePolicy("9h", "72h")
	if got := fresh.blockedRoleWakeAfter(paths.Home); got != 3*time.Hour {
		t.Fatalf("blockedRoleWakeAfter re-read config.toml mid-tick: got %s, want 3h", got)
	}
	if got, err := fresh.delegationWorktreeTTL(paths.Home); err != nil || got != 24*time.Hour {
		t.Fatalf("delegationWorktreeTTL re-read config.toml mid-tick: got %s err=%v, want 24h", got, err)
	}
	fresh.reset()
	if got := fresh.blockedRoleWakeAfter(paths.Home); got != 9*time.Hour {
		t.Fatalf("blockedRoleWakeAfter after reset = %s, want 9h", got)
	}
	if got, err := fresh.delegationWorktreeTTL(paths.Home); err != nil || got != 72*time.Hour {
		t.Fatalf("delegationWorktreeTTL after reset = %s err=%v, want 72h", got, err)
	}
}

// The memory controller is the loader that cost the most for the least: two full
// config loads per repo per tick to return nil. Memoize the RESULT, nil included,
// and still re-resolve on the next tick.
func TestTickConfigCacheMemoizesMemoryController(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	write := func(body string) {
		if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("[agents.solo]\nmemory = true\n")
	cache := &tickConfigCache{}
	if cache.memoryController(store, paths.Home) == nil {
		t.Fatal("an enrolled agent must produce a controller")
	}
	write("[memory]\ndisabled = true\n")
	if cache.memoryController(store, paths.Home) == nil {
		t.Fatal("the controller was re-resolved mid-tick; the memo did not hold")
	}
	cache.reset()
	if cache.memoryController(store, paths.Home) != nil {
		t.Fatal("after reset the kill switch must take effect")
	}
	// The nil result is memoized too — reaching it costs the same two loads.
	write("[agents.solo]\nmemory = true\n")
	if cache.memoryController(store, paths.Home) != nil {
		t.Fatal("the nil result was not memoized")
	}
}

// Every poll pass must start from a cold config memo, otherwise the poller would
// read config.toml once per PROCESS rather than once per tick and stop being
// live-tunable.
func TestPollPassResetsConfigCache(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "", "")
	if poller.ConfigCache == nil {
		t.Fatal("the default poller must carry a config cache")
	}
	poller.ConfigCache.memoryDone = true
	poller.ConfigCache.memoryHome = "stale"
	// No enabled repos: the pass does nothing except its own setup, so a cleared
	// cache can only have come from the reset.
	if _, err := pollRegisteredReposWithPoller(context.Background(), poller, registeredRepoSchedule{}, time.Now().UTC(), time.Second); err != nil {
		t.Fatalf("poll pass: %v", err)
	}
	if poller.ConfigCache.memoryDone || poller.ConfigCache.memoryHome != "" {
		t.Fatalf("config cache survived a poll pass: %+v", poller.ConfigCache)
	}
}
