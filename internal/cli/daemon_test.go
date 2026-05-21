package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestRunDaemonUsageAndValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"daemon", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("daemon help exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gitmoot daemon start") {
		t.Fatalf("daemon help output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"daemon", "start", "--repo", "not-a-repo"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("daemon start invalid repo exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid repo") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"daemon", "start", "--repo", "jerryfane/gitmoot", "--poll", "0s"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("daemon start invalid poll exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "poll interval must be positive") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"daemon", "run", "--repo", "jerryfane/gitmoot", "--dry-run"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("daemon run single-repo dry-run exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "dry-run") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDaemonStatusRemovesStalePID(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "daemon.pid"), []byte("999999\n"), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("daemon status exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stale pid") {
		t.Fatalf("status output = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(paths.Home, "daemon.pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file after status err = %v, want not exists", err)
	}
}

func TestDaemonStatusRejectsPIDWithoutDaemonMetadata(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "daemon.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("daemon status exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stale pid") {
		t.Fatalf("status output = %q", stdout.String())
	}
}

func TestStopDaemonPIDTreatsMissingProcessAsStopped(t *testing.T) {
	if err := stopDaemonPID(99999999); err != nil {
		t.Fatalf("stopDaemonPID returned error for missing process: %v", err)
	}
}

func TestDaemonRestartRejectsInvalidArgsBeforeStop(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "positional", args: []string{"typo"}},
		{name: "invalid repo", args: []string{"--repo", "not-a-repo"}},
		{name: "invalid poll", args: []string{"--poll", "0s"}},
		{name: "invalid workers", args: []string{"--workers", "0"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatalf("Initialize returned error: %v", err)
			}
			pidFile := filepath.Join(paths.Home, "daemon.pid")
			if err := os.WriteFile(pidFile, []byte("999999\n"), 0o600); err != nil {
				t.Fatalf("write pid: %v", err)
			}

			args := []string{"daemon", "restart", "--home", home}
			args = append(args, tc.args...)
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr)

			if code != 2 {
				t.Fatalf("daemon restart exit code = %d, want 2; stderr=%s", code, stderr.String())
			}
			if _, err := os.Stat(pidFile); err != nil {
				t.Fatalf("pid file was touched before validation: %v", err)
			}
		})
	}
}

func TestDaemonSubcommandHelpDoesNotMutateState(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	pidFile := filepath.Join(paths.Home, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte("999999\n"), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	for _, args := range [][]string{
		{"daemon", "start", "--home", home, "--help"},
		{"daemon", "restart", "--home", home, "--help"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("Run(%v) exit code = %d, stderr=%s", args, code, stderr.String())
		}
		if _, err := os.Stat(pidFile); err != nil {
			t.Fatalf("Run(%v) touched pid file: %v", args, err)
		}
	}
}

func TestDaemonRestartOverlayPreservesSavedArgs(t *testing.T) {
	var stderr bytes.Buffer
	cfg, code := parseDaemonStartConfig("daemon restart", []string{"--poll", "1m", "--workers", "2"}, &stderr)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig code = %d, stderr=%s", code, stderr.String())
	}
	args := overlayDaemonStartArgs([]string{
		"--home", "/tmp/gitmoot-home",
		"--repo", "owner/project",
		"--poll", "10s",
		"--workers", "1",
	}, cfg)

	parsed, code := parseDaemonStartConfig("daemon restart", args, &stderr)
	if code != 0 {
		t.Fatalf("parse overlaid args code = %d, stderr=%s args=%v", code, stderr.String(), args)
	}
	if parsed.RepoFlag != "owner/project" {
		t.Fatalf("repo flag = %q, want owner/project; args=%v", parsed.RepoFlag, args)
	}
	if parsed.Poll != time.Minute {
		t.Fatalf("poll = %s, want 1m; args=%v", parsed.Poll, args)
	}
	if parsed.Workers != 2 {
		t.Fatalf("workers = %d, want 2; args=%v", parsed.Workers, args)
	}
}

func TestDaemonChildArgsRunAllRepoSupervisor(t *testing.T) {
	args := daemonChildArgs("/tmp/gitmoot-home", "30s", 2)

	for i, arg := range args {
		if arg == "--repo" || strings.HasPrefix(arg, "--repo=") {
			t.Fatalf("daemon child args include repo at index %d: %v", i, args)
		}
	}
	parsed, code := parseDaemonStartConfig("daemon restart", args[2:], io.Discard)
	if code != 0 {
		t.Fatalf("parse child args code = %d, args=%v", code, args)
	}
	if parsed.RepoSet {
		t.Fatalf("daemon child args selected single-repo mode: %v", args)
	}
	if parsed.Workers != 2 || parsed.Poll != 30*time.Second {
		t.Fatalf("parsed child args = %+v", parsed)
	}
}

func TestDaemonProcessArgsMatchRequiresSavedArgs(t *testing.T) {
	meta := daemonMeta{
		Executable: "/usr/local/bin/gitmoot",
		Args:       []string{"daemon", "run", "--poll", "30s", "--workers", "1", "--home", "/tmp/home-a"},
	}
	matching := append([]string{meta.Executable}, meta.Args...)
	if !daemonProcessArgsMatch(matching, meta) {
		t.Fatalf("matching daemon argv was rejected")
	}

	otherHome := []string{meta.Executable, "daemon", "run", "--poll", "30s", "--workers", "1", "--home", "/tmp/home-b"}
	if daemonProcessArgsMatch(otherHome, meta) {
		t.Fatalf("daemon argv for another home was accepted")
	}

	foregroundRepo := []string{meta.Executable, "daemon", "run", "--repo", "owner/repo", "--poll", "30s", "--workers", "1", "--home", "/tmp/home-a"}
	if daemonProcessArgsMatch(foregroundRepo, meta) {
		t.Fatalf("foreground single-repo daemon argv was accepted")
	}

	truncated := []string{meta.Executable, "daemon", "run"}
	if daemonProcessArgsMatch(truncated, meta) {
		t.Fatalf("truncated daemon argv was accepted")
	}
}

func TestDaemonStartRepoPreflightsCheckoutBeforeDaemonizing(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"daemon", "start", "--home", home, "--repo", "jerryfane/gitmoot", "--poll", "1h"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("daemon start exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "daemon started") {
		t.Fatalf("daemon start reported success: %q", stdout.String())
	}
	paths := config.PathsForHome(home)
	if _, err := os.Stat(filepath.Join(paths.Home, "daemon.pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file err = %v, want not exists", err)
	}
}

func TestDaemonRestartRepoPreflightsCheckoutBeforeStop(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	pidFile := filepath.Join(paths.Home, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte("999999\n"), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"daemon", "restart", "--home", home, "--repo", "jerryfane/gitmoot", "--poll", "1h"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("daemon restart exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("pid file was touched before preflight completed: %v", err)
	}
}

func TestPollRegisteredReposHonorsPerRepoIntervals(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	repos := []db.Repo{
		{Owner: "owner", Name: "slow", CheckoutPath: "/tmp/slow", PollInterval: "1h"},
		{Owner: "owner", Name: "fast", CheckoutPath: "/tmp/fast", PollInterval: "30s"},
	}
	for _, repo := range repos {
		if err := store.UpsertRepo(ctx, repo); err != nil {
			t.Fatalf("UpsertRepo(%s) returned error: %v", repo.FullName(), err)
		}
	}

	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	nextPoll := map[string]time.Time{}
	var stdout bytes.Buffer
	if _, err := pollRegisteredRepos(ctx, store, 1, true, &stdout, nextPoll, now, 30*time.Second); err != nil {
		t.Fatalf("first pollRegisteredRepos returned error: %v", err)
	}
	firstSlow, err := store.GetRepo(ctx, "owner/slow")
	if err != nil {
		t.Fatalf("GetRepo slow: %v", err)
	}
	firstFast, err := store.GetRepo(ctx, "owner/fast")
	if err != nil {
		t.Fatalf("GetRepo fast: %v", err)
	}

	if _, err := pollRegisteredRepos(ctx, store, 1, true, &stdout, nextPoll, now.Add(31*time.Second), 30*time.Second); err != nil {
		t.Fatalf("second pollRegisteredRepos returned error: %v", err)
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
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	repoA := github.Repository{Owner: "owner", Name: "repo-a"}
	repoB := github.Repository{Owner: "owner", Name: "repo-b"}
	for _, repo := range []db.Repo{
		{Owner: repoA.Owner, Name: repoA.Name, CheckoutPath: "/tmp/repo-a", PollInterval: "30s"},
		{Owner: repoB.Owner, Name: repoB.Name, CheckoutPath: "/tmp/repo-b", PollInterval: "30s"},
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
		"/tmp/repo-a": {
			pulls: []github.PullRequest{{Number: 1, Title: "A", State: "open", HeadRef: "task-a", BaseRef: "main", HeadSHA: "sha-a"}},
			comments: map[int64][]github.IssueComment{
				1: {{ID: 77, Body: "/gitmoot audit review check repo a", Author: "alice"}},
			},
		},
		"/tmp/repo-b": {
			pulls: []github.PullRequest{{Number: 1, Title: "B", State: "open", HeadRef: "task-b", BaseRef: "main", HeadSHA: "sha-b"}},
			comments: map[int64][]github.IssueComment{
				1: {{ID: 77, Body: "/gitmoot audit review check repo b", Author: "alice"}},
			},
		},
	}
	poller := defaultRegisteredRepoPoller(store, 2, false, io.Discard)
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
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	for _, repo := range []db.Repo{
		{Owner: "owner", Name: "failing", CheckoutPath: "/tmp/failing", PollInterval: "30s"},
		{Owner: "owner", Name: "healthy", CheckoutPath: "/tmp/healthy", PollInterval: "30s"},
	} {
		if err := store.UpsertRepo(ctx, repo); err != nil {
			t.Fatalf("UpsertRepo(%s) returned error: %v", repo.FullName(), err)
		}
	}
	failing := &cliPollFakeGitHub{listErr: errors.New("rate limited")}
	healthy := &cliPollFakeGitHub{}
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard)
	poller.GitHubClient = func(checkout string) github.Client {
		if checkout == "/tmp/failing" {
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

func TestRunQueuedJobsExecutesShellAdapterSuccess(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, `printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}'`, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-success", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-success")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) || !strings.Contains(job.Payload, `"summary":"done"`) {
		t.Fatalf("job after worker = %+v", job)
	}
}

func TestRunQueuedJobsMarksShellAdapterFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "exit 7", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-fail", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-fail")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

func TestRunQueuedJobsUsesMailboxRepairForMalformedOutput(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, `if printf '%s' "$1" | grep -q 'Previous raw output'; then printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"repaired","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}'; else printf '%s\n' 'not json'; fi`, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-repair", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-repair")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) || !strings.Contains(job.Payload, `"summary":"repaired"`) {
		t.Fatalf("job after repair = %+v", job)
	}
	events, err := store.ListJobEvents(ctx, "job-repair")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "malformed_output") || !daemonWorkerHasEvent(events, "repair_retry") {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunQueuedJobsPostsAttributedResultComment(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-comment", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 7})
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{
			output: `{"gitmoot_result":{"decision":"approved","summary":"done with token=ghp_abcdefghijklmnopqrstuvwxyz123456","findings":[{"severity":"low","body":"ok"}],"changes_made":["commented"],"tests_run":["go test ./..."],"needs":["none"],"next_agents":["lead"]}}`,
		}, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want 1", comments.posted)
	}
	body := comments.posted[0].body
	for _, want := range []string{
		"> Agent: `audit`",
		"> Runtime: `shell`",
		"> Job: `job-comment`",
		"**Decision:** `approved`",
		"**Summary:** done with token=[REDACTED]",
		"**Findings**",
		"**Changes Made**",
		"**Tests Run**",
		"**Needs**",
		"**Next Agents**",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("comment leaked token:\n%s", body)
	}
	events, err := store.ListJobEvents(ctx, "job-comment")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "comment_posted") {
		t.Fatalf("events = %+v, want comment_posted", events)
	}
}

func TestRunQueuedJobsPostsMalformedOutputDiagnosticComment(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-malformed", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 7})
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{output: "not valid json with token=ghp_abcdefghijklmnopqrstuvwxyz123456"}, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want 1", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "> Agent: `audit`") || !strings.Contains(body, "**Decision:** `failed`") || !strings.Contains(body, "**Diagnostics:**") {
		t.Fatalf("comment body missing failure diagnostics:\n%s", body)
	}
	if strings.Contains(body, "not valid json") || strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("comment leaked raw output:\n%s", body)
	}
	if !strings.Contains(body, "Raw runtime output was retained in local Gitmoot state") {
		t.Fatalf("comment did not mention local raw output retention:\n%s", body)
	}
}

func TestRunQueuedJobsPostsCheckoutDiagnosticWithoutCheckoutCwd(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", "/missing/gitmoot-checkout")
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-checkout-comment", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 7})
	comments := &cliPollFakeGitHub{}
	commenterDir := "unset"
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return "", errors.New("checkout path is missing")
	}
	worker.CommenterFactory = func(dir string) github.Client {
		commenterDir = dir
		return comments
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if commenterDir != "" {
		t.Fatalf("commenter dir = %q, want empty cwd for PR comment posting", commenterDir)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want checkout diagnostic comment", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "**Diagnostics:** checkout path is missing") {
		t.Fatalf("comment body lost checkout diagnostic:\n%s", body)
	}
}

func TestDaemonWorkerTickRetriesFailedResultCommentPost(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-comment-retry", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 7})
	comments := &cliPollFakeGitHub{postErr: errors.New("temporary github error")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{
			output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
		}, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if len(comments.posted) != 0 {
		t.Fatalf("posted comments = %+v, want failed post only", comments.posted)
	}
	events, err := store.ListJobEvents(ctx, "job-comment-retry")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "comment_post_failed") {
		t.Fatalf("events = %+v, want comment_post_failed", events)
	}

	comments.postErr = nil
	if err := retryPendingJobComments(ctx, worker, "owner/repo"); err != nil {
		t.Fatalf("retryPendingJobComments returned error: %v", err)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want retry post", comments.posted)
	}
	events, err = store.ListJobEvents(ctx, "job-comment-retry")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "comment_posted") {
		t.Fatalf("events = %+v, want comment_posted", events)
	}
}

func TestRetryPendingJobCommentsPreservesStoredFailureDiagnostic(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-comment-diagnostic-retry", Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-comment-diagnostic-retry",
		Kind:    string(workflow.JobFailed),
		Message: "checkout validation failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-comment-diagnostic-retry", Kind: "comment_post_failed", Message: "temporary github error"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := retryPendingJobComments(ctx, worker, "owner/repo"); err != nil {
		t.Fatalf("retryPendingJobComments returned error: %v", err)
	}

	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want retry post", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "**Diagnostics:** checkout validation failed") {
		t.Fatalf("comment body lost stored failure diagnostic:\n%s", body)
	}
}

func TestRunQueuedJobsDrainsBeyondWorkerLimit(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	for _, id := range []string{"job-1", "job-2", "job-3"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 2); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 3 {
		t.Fatalf("adapter calls = %d, want 3", adapter.calls)
	}
	for _, id := range []string{"job-1", "job-2", "job-3"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob(%s) returned error: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded", id, job.State)
		}
	}
}

func TestRunQueuedJobsDefersJobsEnqueuedByCurrentSnapshot(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-implement",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
		LeadAgent:   "lead",
		HeadSHA:     strings.Repeat("a", 40),
	})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"implemented","summary":"opened","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, RequiredReviewers: []string{"reviewer"}}
	}

	if err := runQueuedJobs(ctx, worker, 2); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want only initial implementation job", adapter.calls)
	}
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Agent != "reviewer" || jobs[0].Type != "review" {
		t.Fatalf("queued jobs = %+v, want deferred reviewer job", jobs)
	}
}

func TestRunQueuedJobsRefreshesImplementedHeadBeforeReviewDispatch(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	oldHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-implement",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
		LeadAgent:   "lead",
		HeadSHA:     oldHead,
	})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"implemented","summary":"opened","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
		onDeliver: func() {
			if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("implemented\n"), 0o644); err != nil {
				t.Fatalf("WriteFile returned error: %v", err)
			}
			runGit(t, checkout, "add", "README.md")
			runGit(t, checkout, "commit", "-m", "implement")
		},
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout)
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	newHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if newHead == oldHead {
		t.Fatal("new HEAD did not change")
	}
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Agent != "reviewer" || jobs[0].Type != "review" {
		t.Fatalf("queued jobs = %+v, want reviewer job", jobs)
	}
	payload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.HeadSHA != newHead {
		t.Fatalf("review payload head = %q, want %q", payload.HeadSHA, newHead)
	}
}

func TestRunQueuedJobsSerializesSameRepoCheckout(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	for _, id := range []string{"job-a", "job-b"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	}
	var mu sync.Mutex
	active := 0
	maxActive := 0
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
		onDeliver: func() {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
		},
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 2); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if maxActive != 1 {
		t.Fatalf("max concurrent same-repo deliveries = %d, want 1", maxActive)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

func TestRunQueuedJobsPreservesCreationOrderForSameRepo(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	for _, id := range []string{"job-z", "job-a"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	want := []string{"job-z", "job-a"}
	if !reflect.DeepEqual(adapter.delivered, want) {
		t.Fatalf("delivered jobs = %v, want %v", adapter.delivered, want)
	}
}

func TestRunQueuedJobsPreservesCancellationRace(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-cancel", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"late","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
		onDeliver: func() {
			if err := store.UpdateJobState(ctx, "job-cancel", string(workflow.JobCancelled)); err != nil {
				t.Fatalf("UpdateJobState returned error: %v", err)
			}
		},
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	if len(comments.posted) != 0 {
		t.Fatalf("posted comments = %+v, want no comment for cancelled job", comments.posted)
	}
}

func TestRunQueuedJobsUsesConfiguredMergeGate(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-review",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
	})
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true}}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if gate.calls != 1 {
		t.Fatalf("merge gate calls = %d, want 1", gate.calls)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskMerged) {
		t.Fatalf("task state = %q, want merged", task.State)
	}
}

func TestRunQueuedJobsFailsImplementWithoutBranchLockBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-implement", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, TaskID: "task-1"})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want preflight to stop delivery", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

func TestRunQueuedJobsFailsReviewOnWrongCheckoutBranchBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, HeadSHA: head})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want preflight to stop delivery", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-review")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

func TestRunQueuedJobsFailsReviewOnWrongCheckoutHeadBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, HeadSHA: strings.Repeat("0", 40)})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want preflight to stop delivery", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-review")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

func TestRunQueuedJobsFailsPRScopedAskOnWrongCheckoutHeadBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-ask", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, HeadSHA: strings.Repeat("0", 40)})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want preflight to stop delivery", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-ask")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

func TestRunQueuedJobsForRepoSkipsOtherRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "owner/repo-a"); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}

	jobA, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob job-a returned error: %v", err)
	}
	jobB, err := store.GetJob(ctx, "job-b")
	if err != nil {
		t.Fatalf("GetJob job-b returned error: %v", err)
	}
	if jobA.State != string(workflow.JobSucceeded) {
		t.Fatalf("job-a state = %q, want succeeded", jobA.State)
	}
	if jobB.State != string(workflow.JobQueued) {
		t.Fatalf("job-b state = %q, want queued", jobB.State)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunEnabledRepoWorkerTicksSkipsDisabledRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/enabled", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/disabled", t.TempDir())
	if err := store.SetRepoEnabled(ctx, "owner/disabled", false); err != nil {
		t.Fatalf("SetRepoEnabled returned error: %v", err)
	}
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/enabled")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/disabled"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-enabled", Agent: "audit", Action: "ask", Repo: "owner/enabled", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-disabled", Agent: "audit", Action: "ask", Repo: "owner/disabled", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runEnabledRepoWorkerTicks(ctx, store, worker, 2, io.Discard, time.Now().UTC()); err != nil {
		t.Fatalf("runEnabledRepoWorkerTicks returned error: %v", err)
	}

	enabledJob, err := store.GetJob(ctx, "job-enabled")
	if err != nil {
		t.Fatalf("GetJob job-enabled returned error: %v", err)
	}
	disabledJob, err := store.GetJob(ctx, "job-disabled")
	if err != nil {
		t.Fatalf("GetJob job-disabled returned error: %v", err)
	}
	if enabledJob.State != string(workflow.JobSucceeded) {
		t.Fatalf("enabled job state = %q, want succeeded", enabledJob.State)
	}
	if disabledJob.State != string(workflow.JobQueued) {
		t.Fatalf("disabled job state = %q, want queued", disabledJob.State)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunQueuedJobsRecordsPostDeliveryWorkflowErrorForRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-review",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		HeadSHA:     strings.Repeat("a", 40),
	})
	gate := &cliWorkerFakeMergeGate{err: errors.New("github unavailable")}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	job, getErr := store.GetJob(ctx, "job-review")
	if getErr != nil {
		t.Fatalf("GetJob returned error: %v", getErr)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded result preserved", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_retry") {
		t.Fatalf("events = %+v, want advance retry event", events)
	}
	gate.err = nil
	gate.decision = workflow.MergeDecision{Ready: true}
	if err := retryPendingJobAdvancements(ctx, worker, ""); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want no redelivery during advance retry", adapter.calls)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want ready_to_merge", task.State)
	}
	events, err = store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_retried") {
		t.Fatalf("events = %+v, want advance retried event", events)
	}
}

func TestRetryPendingJobAdvancementsRecoversStartedAdvancement(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	payload := workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		Result:      &workflow.AgentResult{Decision: "approved", Summary: "approved"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-review", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-review",
		Kind:    string(workflow.JobSucceeded),
		Message: "job succeeded",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-review", Kind: "advance_started", Message: "workflow advancement started"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: true}}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}

	if err := retryPendingJobAdvancements(ctx, worker, ""); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want ready_to_merge", task.State)
	}
	events, err := store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_retried") {
		t.Fatalf("events = %+v, want advance retried event", events)
	}
}

func TestRetryPendingJobAdvancementsAdvancesFailedStoredResult(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	payload := workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		Result:      &workflow.AgentResult{Decision: "failed", Summary: "tests failed"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-review", Agent: "reviewer", Type: "review", State: string(workflow.JobFailed), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-review",
		Kind:    string(workflow.JobFailed),
		Message: "job failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-review", Kind: "advance_retry", Message: "transient workflow failure"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := retryPendingJobAdvancements(ctx, worker, ""); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
	events, err := store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_blocked") {
		t.Fatalf("events = %+v, want advance blocked event", events)
	}
}

func TestRetryPendingJobAdvancementsRefreshesImplementedHeadBeforePreflight(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	oldHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("implemented\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "implement")
	newHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	payload := workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		HeadSHA:     oldHead,
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
		LeadAgent:   "lead",
		Result:      &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-implement", Agent: "lead", Type: "implement", State: string(workflow.JobSucceeded), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-implement",
		Kind:    string(workflow.JobSucceeded),
		Message: "job succeeded",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-implement", Kind: "advance_retry", Message: "transient refresh failure"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout)
	}

	if err := retryPendingJobAdvancements(ctx, worker, ""); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}

	implementJob, err := store.GetJob(ctx, "job-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	implementPayload, err := daemonJobPayload(implementJob)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if implementPayload.HeadSHA != newHead {
		t.Fatalf("implement payload head = %q, want %q", implementPayload.HeadSHA, newHead)
	}
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Agent != "reviewer" || jobs[0].Type != "review" {
		t.Fatalf("queued jobs = %+v, want reviewer job", jobs)
	}
	reviewPayload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if reviewPayload.HeadSHA != newHead {
		t.Fatalf("review payload head = %q, want %q", reviewPayload.HeadSHA, newHead)
	}
}

func TestRunQueuedJobsSwallowsPostDeliveryBlockedWorkflow(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-review",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		HeadSHA:     strings.Repeat("a", 40),
	})
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: false, Reason: "ci pending"}}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-review")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded result preserved", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
}

func TestRecoverRunningJobsRequeuesStaleRunningJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}

	if err := recoverRunningJobsBefore(ctx, store, io.Discard, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatalf("recoverRunningJobsBefore returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-running")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, string(workflow.JobQueued)) {
		t.Fatalf("events = %+v, want queued recovery event", events)
	}
}

func TestRecoverRunningJobsKeepsRecentRunningJobsOnStartup(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}

	if err := recoverRunningJobs(ctx, store, io.Discard); err != nil {
		t.Fatalf("recoverRunningJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("job state = %q, want running", job.State)
	}
}

func TestDaemonWorkerTickRechecksStaleRunningJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	now := time.Now().UTC().Add(daemonRunningJobStaleAfter + time.Second)

	if err := runDaemonWorkerTick(ctx, store, worker, 0, false, "", io.Discard, now); err != nil {
		t.Fatalf("runDaemonWorkerTick returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
}

func TestRecoverRunningJobsForRepoSkipsOtherRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 1})
	for _, id := range []string{"job-a", "job-b"} {
		if err := store.UpdateJobState(ctx, id, string(workflow.JobRunning)); err != nil {
			t.Fatalf("UpdateJobState(%s) returned error: %v", id, err)
		}
	}

	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, time.Now().UTC().Add(time.Second), "owner/repo-a"); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}

	jobA, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob job-a returned error: %v", err)
	}
	jobB, err := store.GetJob(ctx, "job-b")
	if err != nil {
		t.Fatalf("GetJob job-b returned error: %v", err)
	}
	if jobA.State != string(workflow.JobQueued) {
		t.Fatalf("job-a state = %q, want queued", jobA.State)
	}
	if jobB.State != string(workflow.JobRunning) {
		t.Fatalf("job-b state = %q, want running", jobB.State)
	}
}

func TestDaemonLogsEmptyWhenMissing(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"daemon", "logs", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("daemon logs exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "daemon log is empty") {
		t.Fatalf("logs output = %q", stdout.String())
	}
}

func TestResolveDaemonCheckoutRequiresMatchingOrigin(t *testing.T) {
	runner := &daemonGitRunner{results: []subprocess.Result{
		{Stdout: "/repo/gitmoot\n"},
		{Stdout: "https://github.com/jerryfane/gitmoot.git\n"},
	}}

	root, err := resolveDaemonCheckout(context.Background(), github.Repository{Owner: "jerryfane", Name: "gitmoot"}, gitutil.Client{Runner: runner, Dir: "."})

	if err != nil {
		t.Fatalf("resolveDaemonCheckout returned error: %v", err)
	}
	if root != "/repo/gitmoot" {
		t.Fatalf("root = %q, want /repo/gitmoot", root)
	}
	runner.wantArgs(t, 0, "git", "rev-parse", "--show-toplevel")
	runner.wantArgs(t, 1, "git", "remote", "get-url", "origin")
}

func TestResolveDaemonCheckoutRejectsWrongOrigin(t *testing.T) {
	runner := &daemonGitRunner{results: []subprocess.Result{
		{Stdout: "/repo/other\n"},
		{Stdout: "https://github.com/jerryfane/other.git\n"},
	}}

	_, err := resolveDaemonCheckout(context.Background(), github.Repository{Owner: "jerryfane", Name: "gitmoot"}, gitutil.Client{Runner: runner, Dir: "."})

	if err == nil || !strings.Contains(err.Error(), "not jerryfane/gitmoot") {
		t.Fatalf("error = %v, want wrong-origin error", err)
	}
}

type daemonGitRunner struct {
	results []subprocess.Result
	errs    []error
	calls   [][]string
}

func (f *daemonGitRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	call := append([]string{command}, args...)
	f.calls = append(f.calls, call)
	index := len(f.calls) - 1
	result := subprocess.Result{Command: command, Args: args}
	if index < len(f.results) {
		result = f.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return result, err
}

func (f *daemonGitRunner) LookPath(string) (string, error) {
	return "", errors.New("not implemented")
}

func (f *daemonGitRunner) wantArgs(t *testing.T, index int, want ...string) {
	t.Helper()
	if index >= len(f.calls) {
		t.Fatalf("missing call %d; calls=%v", index, f.calls)
	}
	if !reflect.DeepEqual(f.calls[index], want) {
		t.Fatalf("call %d = %v, want %v", index, f.calls[index], want)
	}
}

type cliPollFakeGitHub struct {
	github.NoopClient
	pulls                 []github.PullRequest
	comments              map[int64][]github.IssueComment
	listErr               error
	postErr               error
	listPullRequestsCalls int
	posted                []cliPollPostedComment
}

type cliPollPostedComment struct {
	issueNumber int64
	body        string
}

func (f *cliPollFakeGitHub) ListPullRequests(context.Context, github.Repository, string) ([]github.PullRequest, error) {
	f.listPullRequestsCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]github.PullRequest(nil), f.pulls...), nil
}

func (f *cliPollFakeGitHub) ListIssueComments(_ context.Context, _ github.Repository, issueNumber int64) ([]github.IssueComment, error) {
	return append([]github.IssueComment(nil), f.comments[issueNumber]...), nil
}

func (f *cliPollFakeGitHub) PostIssueComment(_ context.Context, _ github.Repository, issueNumber int64, body string) (github.IssueComment, error) {
	if f.postErr != nil {
		return github.IssueComment{}, f.postErr
	}
	f.posted = append(f.posted, cliPollPostedComment{issueNumber: issueNumber, body: body})
	return github.IssueComment{ID: int64(len(f.posted)), Body: body}, nil
}

func (f *cliPollFakeGitHub) GetUserPermission(context.Context, github.Repository, string) (github.UserPermission, error) {
	return github.UserPermission{Permission: "write", RoleName: "write"}, nil
}

type cliWorkerFakeAdapter struct {
	mu        sync.Mutex
	output    string
	calls     int
	delivered []string
	onDeliver func()
}

func (f *cliWorkerFakeAdapter) Deliver(_ context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	f.mu.Lock()
	f.calls++
	f.delivered = append(f.delivered, job.ID)
	onDeliver := f.onDeliver
	f.mu.Unlock()
	if onDeliver != nil {
		onDeliver()
	}
	return runtime.Result{Raw: f.output}, nil
}

type cliWorkerFakeMergeGate struct {
	calls    int
	decision workflow.MergeDecision
	err      error
}

func (f *cliWorkerFakeMergeGate) Evaluate(context.Context, workflow.MergeRequest) (workflow.MergeDecision, error) {
	f.calls++
	if f.err != nil {
		return workflow.MergeDecision{}, f.err
	}
	return f.decision, nil
}

func daemonWorkerStore(t *testing.T) *db.Store {
	t.Helper()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func seedDaemonWorkerRepo(t *testing.T, store *db.Store, fullName string, checkout string) {
	t.Helper()
	repo, err := daemon.ParseRepository(fullName)
	if err != nil {
		t.Fatalf("ParseRepository returned error: %v", err)
	}
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: repo.Owner, Name: repo.Name, CheckoutPath: checkout, PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
}

func seedDaemonWorkerAgent(t *testing.T, store *db.Store, name string, runtimeName string, runtimeRef string, capabilities []string, repo string) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           name,
		Role:           "worker",
		Runtime:        runtimeName,
		RuntimeRef:     runtimeRef,
		RepoScope:      repo,
		Capabilities:   capabilities,
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
}

func enqueueDaemonWorkerJob(t *testing.T, store *db.Store, request workflow.JobRequest) {
	t.Helper()
	if _, err := (workflow.Mailbox{Store: store}).Enqueue(context.Background(), request); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
}

func daemonWorkerHasEvent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}
