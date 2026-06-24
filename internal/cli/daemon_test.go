package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

func TestDaemonStartForwardsRepoAndSessionThroughRestart(t *testing.T) {
	// The detached child argv carries both filters (this is exactly what
	// daemonMeta.Args records).
	childArgs := daemonChildArgs("/tmp/gitmoot-home", "30s", 1, false, false, "barrier", "owner/project", "root-coordinator")
	if !daemonArgsContainFlag(childArgs, "repo", "owner/project") {
		t.Fatalf("child argv missing --repo: %v", childArgs)
	}
	if !daemonArgsContainFlag(childArgs, "session", "root-coordinator") {
		t.Fatalf("child argv missing --session: %v", childArgs)
	}

	meta := daemonMeta{Args: childArgs}

	// A restart with no explicit start flags rebuilds the start args from the
	// saved run args and must preserve both filters.
	restartCfg, code := parseDaemonStartConfig("daemon restart", nil, io.Discard)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig(restart) code = %d", code)
	}
	targetArgs := overlayDaemonStartArgs(daemonStartArgsFromRunArgs(meta.Args), restartCfg)
	parsed, code := parseDaemonStartConfig("daemon restart", targetArgs, io.Discard)
	if code != 0 {
		t.Fatalf("parse restored restart args code = %d, args=%v", code, targetArgs)
	}
	if parsed.RepoFlag != "owner/project" {
		t.Fatalf("restart repo flag = %q, want owner/project; args=%v", parsed.RepoFlag, targetArgs)
	}
	if parsed.Session != "root-coordinator" {
		t.Fatalf("restart session = %q, want root-coordinator; args=%v", parsed.Session, targetArgs)
	}

	// An explicit --session on restart overlays the saved value.
	overrideCfg, code := parseDaemonStartConfig("daemon restart", []string{"--session", "other-root"}, io.Discard)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig(override) code = %d", code)
	}
	overlaid := overlayDaemonStartArgs(daemonStartArgsFromRunArgs(meta.Args), overrideCfg)
	parsedOverride, code := parseDaemonStartConfig("daemon restart", overlaid, io.Discard)
	if code != 0 {
		t.Fatalf("parse overlaid restart args code = %d, args=%v", code, overlaid)
	}
	if parsedOverride.RepoFlag != "owner/project" {
		t.Fatalf("override repo flag = %q, want owner/project; args=%v", parsedOverride.RepoFlag, overlaid)
	}
	if parsedOverride.Session != "other-root" {
		t.Fatalf("override session = %q, want other-root; args=%v", parsedOverride.Session, overlaid)
	}
}

func TestDaemonStartConfigSessionRootAlias(t *testing.T) {
	parsed, code := parseDaemonStartConfig("daemon start", []string{"--root", "root-coordinator"}, io.Discard)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig(--root) code = %d", code)
	}
	if parsed.Session != "root-coordinator" {
		t.Fatalf("--root session = %q, want root-coordinator", parsed.Session)
	}
	if !parsed.ExplicitSession {
		t.Fatalf("--root did not mark the session as explicit")
	}
}

func TestDaemonChildArgsRunAllRepoSupervisor(t *testing.T) {
	args := daemonChildArgs("/tmp/gitmoot-home", "30s", 2, true, false, "barrier", "", "")

	for i, arg := range args {
		if arg == "--repo" || strings.HasPrefix(arg, "--repo=") {
			t.Fatalf("daemon child args include repo at index %d: %v", i, args)
		}
		if arg == "--session" || strings.HasPrefix(arg, "--session=") {
			t.Fatalf("daemon child args include session at index %d: %v", i, args)
		}
	}
	parsed, code := parseDaemonStartConfig("daemon restart", args[2:], io.Discard)
	if code != 0 {
		t.Fatalf("parse child args code = %d, args=%v", code, args)
	}
	if parsed.RepoSet {
		t.Fatalf("daemon child args selected single-repo mode: %v", args)
	}
	if parsed.Session != "" {
		t.Fatalf("daemon child args carried a session filter: %v", args)
	}
	if parsed.Workers != 2 || parsed.Poll != 30*time.Second || !parsed.WatchSkillOptReviews {
		t.Fatalf("parsed child args = %+v", parsed)
	}
}

func TestDaemonChildArgsForwardsRepo(t *testing.T) {
	args := daemonChildArgs("/tmp/gitmoot-home", "30s", 1, false, false, "barrier", "owner/project", "")

	if !daemonArgsContainFlag(args, "repo", "owner/project") {
		t.Fatalf("daemon child args missing --repo owner/project: %v", args)
	}
	parsed, code := parseDaemonStartConfig("daemon restart", args[2:], io.Discard)
	if code != 0 {
		t.Fatalf("parse child args code = %d, args=%v", code, args)
	}
	if !parsed.RepoSet || parsed.RepoFlag != "owner/project" {
		t.Fatalf("parsed child args did not select single-repo mode: %+v args=%v", parsed, args)
	}
	if parsed.Session != "" {
		t.Fatalf("parsed child args carried a session filter: %+v", parsed)
	}
}

func TestDaemonChildArgsForwardsSession(t *testing.T) {
	args := daemonChildArgs("/tmp/gitmoot-home", "30s", 1, false, false, "barrier", "owner/project", "root-coordinator")

	if !daemonArgsContainFlag(args, "repo", "owner/project") {
		t.Fatalf("daemon child args missing --repo owner/project: %v", args)
	}
	if !daemonArgsContainFlag(args, "session", "root-coordinator") {
		t.Fatalf("daemon child args missing --session root-coordinator: %v", args)
	}
	parsed, code := parseDaemonStartConfig("daemon restart", args[2:], io.Discard)
	if code != 0 {
		t.Fatalf("parse child args code = %d, args=%v", code, args)
	}
	if parsed.Session != "root-coordinator" {
		t.Fatalf("parsed child args session = %q, want root-coordinator; args=%v", parsed.Session, args)
	}
}

// daemonArgsContainFlag reports whether argv carries `--name value` as a
// separate flag/value pair.
func daemonArgsContainFlag(args []string, name string, value string) bool {
	flagName := "--" + name
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flagName && args[i+1] == value {
			return true
		}
	}
	return false
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
	poller := defaultRegisteredRepoPoller(store, 2, false, io.Discard, "")
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
	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, "")
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
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, `printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`, []string{"ask"}, "owner/repo")
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

func TestRunQueuedJobsBlocksReadOnlyImplementBeforeRuntime(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskChangesRequested), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-readonly-implement", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	comments := &cliPollFakeGitHub{}
	checkoutCalls := 0
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"should not run","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		checkoutCalls++
		return checkout, nil
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

	if checkoutCalls != 0 {
		t.Fatalf("checkout validator calls = %d, want 0", checkoutCalls)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-readonly-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, string(workflow.JobBlocked)) || !daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, want blocked and permission_blocked", events)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want 1", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "**Decision:** `blocked`") || !strings.Contains(body, agentPermissionBlockedMessage) {
		t.Fatalf("comment body missing permission block:\n%s", body)
	}
}

func TestRunQueuedJobsSkipsReadOnlySideEffectsWhenJobAlreadyMoved(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskChangesRequested), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-readonly-race", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	jobSnapshot, err := store.GetJob(ctx, "job-readonly-race")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if transitioned, err := store.TransitionJobState(ctx, jobSnapshot.ID, string(workflow.JobQueued), string(workflow.JobCancelled)); err != nil || !transitioned {
		t.Fatalf("TransitionJobState returned transitioned=%v err=%v", transitioned, err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		t.Fatal("checkout validator should not run")
		return "", nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := worker.run(ctx, jobSnapshot); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-readonly-race")
	if err != nil {
		t.Fatalf("GetJob after run returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskChangesRequested) {
		t.Fatalf("task state = %q, want changes_requested", task.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, did not want permission_blocked", events)
	}
	if len(comments.posted) != 0 {
		t.Fatalf("posted comments = %+v, want none", comments.posted)
	}
}

func TestRunQueuedJobsAllowsReadOnlyAsk(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-readonly-ask", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"ask ran","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-readonly-ask")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
}

func TestRunQueuedJobsNormalizesRuntimePermissionFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-permission-fail", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	comments := &cliPollFakeGitHub{}
	adapter := &cliWorkerFakeAdapter{err: errors.New("sandbox rejected write: permission denied")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
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

	job, err := store.GetJob(ctx, "job-permission-fail")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, want permission_blocked", events)
	}
	if len(comments.posted) != 1 || !strings.Contains(comments.posted[0].body, agentPermissionBlockedMessage) {
		t.Fatalf("posted comments = %+v, want permission block comment", comments.posted)
	}
}

func TestRunQueuedJobsPreservesGenericRuntimePermissionFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-generic-permission-fail", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	comments := &cliPollFakeGitHub{}
	adapter := &cliWorkerFakeAdapter{err: errors.New("permission denied reading api token")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
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

	job, err := store.GetJob(ctx, "job-generic-permission-fail")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReviewing) {
		t.Fatalf("task state = %q, want reviewing", task.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, did not want permission_blocked", events)
	}
	if len(comments.posted) != 1 || strings.Contains(comments.posted[0].body, agentPermissionBlockedMessage) {
		t.Fatalf("posted comments = %+v, want original failure comment", comments.posted)
	}
}

func TestRunQueuedJobsPreservesAdvanceRetryForPostDeliveryPermissionError(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-advance-permission", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"implemented","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store: store,
			PayloadRefresher: func(context.Context, db.Job, workflow.JobPayload) (workflow.JobPayload, error) {
				return workflow.JobPayload{}, errors.New("permission denied while refreshing implemented head")
			},
		}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-advance-permission")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded result preserved", job.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_retry") {
		t.Fatalf("events = %+v, want advance_retry", events)
	}
	if daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, did not want permission_blocked", events)
	}
}

func TestRunQueuedJobsUsesMailboxRepairForMalformedOutput(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, `if printf '%s' "$1" | grep -q 'Previous raw output'; then printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"repaired","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'; else printf '%s\n' 'not json'; fi`, []string{"ask"}, "owner/repo")
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
			output: `{"gitmoot_result":{"decision":"approved","summary":"done with token=ghp_abcdefghijklmnopqrstuvwxyz123456","findings":[{"severity":"low","body":"ok"}],"changes_made":["commented"],"tests_run":["go test ./..."],"needs":["none"],"delegations":[{"id":"follow-up","agent":"lead","action":"ask","prompt":"coordinate next steps"}]}}`,
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
		"**Delegations**",
		"- lead",
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
			output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
	if err := retryPendingJobComments(ctx, worker, "owner/repo", ""); err != nil {
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

	if err := retryPendingJobComments(ctx, worker, "owner/repo", ""); err != nil {
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

func TestRunQueuedJobsPostsCommentAfterRetryDespitePriorComment(t *testing.T) {
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
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-comment-after-retry", Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-comment-after-retry",
		Kind:    string(workflow.JobFailed),
		Message: "job failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-comment-after-retry", Kind: "comment_posted", Message: "old result comment"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	if _, err := workflow.RetryJob(ctx, store, "job-comment-after-retry"); err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{
			output: `{"gitmoot_result":{"decision":"approved","summary":"retried","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		}, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want retried result comment", comments.posted)
	}
	if !strings.Contains(comments.posted[0].body, "**Summary:** retried") {
		t.Fatalf("retried comment body = %s", comments.posted[0].body)
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
		output: `{"gitmoot_result":{"decision":"implemented","summary":"opened","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
		output: `{"gitmoot_result":{"decision":"implemented","summary":"opened","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, "")
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
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

func TestRunQueuedJobsSerializesSameRuntimeSessionAcrossRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	content := strings.Replace(config.DefaultConfig(paths), `same_session = "fork_temp_session"`, `same_session = "queue"`, 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
	var mu sync.Mutex
	active := 0
	maxActive := 0
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
	worker := defaultJobWorker(store, io.Discard, home)
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
		t.Fatalf("max concurrent same-runtime deliveries = %d, want 1", maxActive)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

func TestRunQueuedJobsAllowsDifferentRuntimeSessionsAcrossRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit-a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo-a")
	seedDaemonWorkerAgent(t, store, "audit-b", runtime.CodexRuntime, "session-b", []string{"ask"}, "owner/repo-b")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit-a", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit-b", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
	var mu sync.Mutex
	active := 0
	maxActive := 0
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
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

	if maxActive != 2 {
		t.Fatalf("max concurrent different-runtime deliveries = %d, want 2", maxActive)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

func TestRunQueuedJobsLeavesBusyRuntimeSessionQueued(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	content := strings.Replace(config.DefaultConfig(paths), `same_session = "fork_temp_session"`, `same_session = "queue"`, 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapter.calls)
	}
	events, err := store.ListJobEvents(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "runtime_lock_wait") {
		t.Fatalf("events = %+v, want runtime_lock_wait", events)
	}
}

func TestRunQueuedJobsDelegatesBusyRuntimeToTempWorker(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-a",
		Agent:       "audit",
		Action:      "ask",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 7,
		HeadSHA:     "abc123",
		GoalID:      "goal-1",
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
	})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	startAdapter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440111"}
	tempRuntimeLockKey := "runtime:codex:550e8400-e29b-41d4-a716-446655440111"
	var lockObservedMu sync.Mutex
	lockObserved := false
	deliveryAdapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			if _, err := store.GetResourceLock(ctx, tempRuntimeLockKey); err != nil {
				t.Errorf("GetResourceLock during temp delivery returned error: %v", err)
				return
			}
			lockObservedMu.Lock()
			lockObserved = true
			lockObservedMu.Unlock()
		},
	}
	startCheckouts := []string{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(_ string, path string) (runtime.Adapter, error) {
		startCheckouts = append(startCheckouts, path)
		return startAdapter, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	if job.Agent == "audit" || !strings.HasPrefix(job.Agent, "audit-temp-job-a") {
		t.Fatalf("job agent = %q, want temp worker", job.Agent)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.OriginalAgent != "audit" || payload.DelegatedAgent != job.Agent || payload.DelegationReason != "runtime_session_busy" {
		t.Fatalf("delegation payload = %+v, job agent=%s", payload, job.Agent)
	}
	if startAdapter.startCalls != 1 || deliveryAdapter.calls != 1 {
		t.Fatalf("start calls=%d delivery calls=%d, want 1 each", startAdapter.startCalls, deliveryAdapter.calls)
	}
	lockObservedMu.Lock()
	observed := lockObserved
	lockObservedMu.Unlock()
	if !observed {
		t.Fatal("temp worker delivery did not observe runtime session lock")
	}
	if _, err := store.GetResourceLock(ctx, tempRuntimeLockKey); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetResourceLock after temp delivery returned error %v, want no rows", err)
	}
	if len(startCheckouts) != 1 || startCheckouts[0] != checkout {
		t.Fatalf("start checkouts = %+v, want %q", startCheckouts, checkout)
	}
	instance, err := store.GetAgentInstance(ctx, job.Agent)
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.Type != tempWorkerAgentType("audit") || instance.RuntimeRef != "550e8400-e29b-41d4-a716-446655440111" {
		t.Fatalf("temp instance = %+v", instance)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents returned error: %v", err)
	}
	for _, agent := range agents {
		if agent.Name == job.Agent {
			t.Fatalf("temp worker %q was persisted as regular agent: %+v", job.Agent, agents)
		}
	}
	events, err := store.ListJobEvents(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, want := range []string{"temp_worker_eligible", "temp_worker_delegated", "temp_worker_merge_back_queued"} {
		if !daemonWorkerHasEvent(events, want) {
			t.Fatalf("events = %+v, want %s", events, want)
		}
	}
	mergeBack, err := store.GetJob(ctx, "job-a-merge-back")
	if err != nil {
		t.Fatalf("GetJob merge-back returned error: %v", err)
	}
	if mergeBack.Agent != "audit" || mergeBack.Type != "ask" || mergeBack.State != string(workflow.JobQueued) {
		t.Fatalf("merge-back job = %+v, want queued ask for original agent", mergeBack)
	}
	mergePayload, err := daemonJobPayload(mergeBack)
	if err != nil {
		t.Fatalf("daemonJobPayload merge-back returned error: %v", err)
	}
	if mergePayload.Sender != job.Agent || !strings.Contains(mergePayload.Instructions, "Temporary worker") || !strings.Contains(mergePayload.Instructions, "completed job job-a") {
		t.Fatalf("merge-back payload = %+v", mergePayload)
	}
	if mergePayload.Branch != "task-1" || mergePayload.PullRequest != 0 || mergePayload.HeadSHA != "" {
		t.Fatalf("merge-back payload checkout fields = %+v, want branch only", mergePayload)
	}
	if !strings.Contains(mergePayload.Instructions, "Pull request: #7") || !strings.Contains(mergePayload.Instructions, "Head SHA: abc123") {
		t.Fatalf("merge-back instructions missing PR context: %q", mergePayload.Instructions)
	}
	if mergePayload.OriginalAgent != "audit" || mergePayload.DelegatedAgent != job.Agent || mergePayload.DelegationReason != "temp_worker_merge_back" {
		t.Fatalf("merge-back delegation payload = %+v, completed job agent=%s", mergePayload, job.Agent)
	}
	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs merge-back returned error: %v", err)
	}
	mergeBack, err = store.GetJob(ctx, "job-a-merge-back")
	if err != nil {
		t.Fatalf("GetJob merge-back after retry returned error: %v", err)
	}
	if mergeBack.State != string(workflow.JobQueued) {
		t.Fatalf("merge-back state after busy original runtime = %q, want queued", mergeBack.State)
	}
	if startAdapter.startCalls != 1 || deliveryAdapter.calls != 1 {
		t.Fatalf("calls after merge-back retry start=%d delivery=%d, want no recursive temp worker", startAdapter.startCalls, deliveryAdapter.calls)
	}
	mergeEvents, err := store.ListJobEvents(ctx, "job-a-merge-back")
	if err != nil {
		t.Fatalf("ListJobEvents merge-back returned error: %v", err)
	}
	if !daemonWorkerHasEvent(mergeEvents, "runtime_lock_wait") {
		t.Fatalf("merge-back events = %+v, want runtime_lock_wait", mergeEvents)
	}
}

func TestRunQueuedJobsResumesDelegatedTempWorkerAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := config.SaveAgentType(paths, config.AgentType{
		Name:           "planner",
		Runtime:        runtime.CodexRuntime,
		Role:           "planner",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		IdleTimeout:    "17m",
		JobTimeout:     "3m",
	}); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	now := time.Now().UTC()
	if err := store.UpsertAgentInstance(ctx, db.AgentInstance{
		Name:           "audit",
		Type:           "planner",
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     "session-1",
		RepoFullName:   "owner/repo",
		Role:           "planner",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		State:          "running",
		CreatedAt:      now.Format(time.RFC3339Nano),
		LastUsedAt:     now.Format(time.RFC3339Nano),
		ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance original returned error: %v", err)
	}
	if err := store.UpsertAgentInstance(ctx, db.AgentInstance{
		Name:           "audit-temp-job-a",
		Type:           tempWorkerAgentType("audit"),
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     "550e8400-e29b-41d4-a716-446655440333",
		RepoFullName:   "owner/repo",
		Role:           "planner",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		State:          "idle",
		CreatedAt:      now.Format(time.RFC3339Nano),
		LastUsedAt:     now.Format(time.RFC3339Nano),
		ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance temp returned error: %v", err)
	}
	if _, err := store.GetAgent(ctx, "audit-temp-job-a"); err != nil {
		t.Fatalf("GetAgent temp instance fallback returned error: %v", err)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents returned error: %v", err)
	}
	for _, agent := range agents {
		if agent.Name == "audit-temp-job-a" {
			t.Fatalf("temp worker %q was persisted as regular agent: %+v", agent.Name, agents)
		}
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:             "owner/repo",
		Branch:           "main",
		Sender:           "tester",
		Instructions:     "continue",
		OriginalAgent:    "audit",
		DelegatedAgent:   "audit-temp-job-a",
		DelegationReason: "runtime_session_busy",
	})
	if err != nil {
		t.Fatalf("Marshal payload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-a", Agent: "audit-temp-job-a", Type: "ask", State: string(workflow.JobQueued), Payload: string(payload)}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	deliveryAdapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"resumed","findings":[],"changes_made":[],"tests_run":["go test"],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	instance, err := store.GetAgentInstance(ctx, "audit-temp-job-a")
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.State != "idle" {
		t.Fatalf("temp instance state = %q, want idle", instance.State)
	}
}

func TestRunQueuedJobsReturnsTempWorkerIdleAfterDeliveryError(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	startAdapter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440222"}
	deliveryAdapter := &cliWorkerFakeAdapter{err: errors.New("delivery failed")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(string, string) (runtime.Adapter, error) {
		return startAdapter, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	instance, err := store.GetAgentInstance(ctx, job.Agent)
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.State != "idle" {
		t.Fatalf("temp instance state = %q, want idle", instance.State)
	}
}

func TestRunQueuedJobsCleansTempWorkerWhenDelegationRaceLoses(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	startAdapter := &cliWorkerFakeAdapter{
		startRuntimeRef: "550e8400-e29b-41d4-a716-446655440444",
		onStart: func() {
			if _, err := workflow.CancelJob(ctx, store, "job-a"); err != nil {
				t.Fatalf("CancelJob returned error: %v", err)
			}
		},
	}
	deliveryAdapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(string, string) (runtime.Adapter, error) {
		return startAdapter, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	if _, err := store.GetAgentInstance(ctx, "audit-temp-job-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgentInstance temp error = %v, want no rows", err)
	}
	if _, err := store.GetAgent(ctx, "audit-temp-job-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgent temp error = %v, want no rows", err)
	}
}

// TestRunQueuedJobsMaterializesAndDisposesEphemeralWorker proves a queued child
// carrying an inline EphemeralSpec (no pre-registered agent) gets a throwaway
// agent materialized from the spec, runs the job (Start + Deliver invoked), and
// is auto-disposed afterwards — both the agent row and its instance are gone.
func TestRunQueuedJobsMaterializesAndDisposesEphemeralWorker(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	ephemeralName := "reviewer-ephemeral-abc1234567"
	// No agent row is seeded: the worker must be materialized from the spec.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:        "job-eph",
		Agent:     ephemeralName,
		Action:    "ask",
		Repo:      "owner/repo",
		Branch:    "main",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.CodexRuntime, Model: "gpt-5.4"},
	})

	startAdapter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440555"}
	deliveryAdapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	var deliveredModel string
	startCheckouts := []string{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(_ string, path string) (runtime.Adapter, error) {
		startCheckouts = append(startCheckouts, path)
		return startAdapter, nil
	}
	worker.AdapterFactory = func(agent runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		deliveredModel = agent.Model
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-eph")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	if startAdapter.startCalls != 1 || deliveryAdapter.calls != 1 {
		t.Fatalf("start calls=%d delivery calls=%d, want 1 each", startAdapter.startCalls, deliveryAdapter.calls)
	}
	if deliveredModel != "gpt-5.4" {
		t.Fatalf("delivered agent model = %q, want gpt-5.4", deliveredModel)
	}
	if len(startCheckouts) != 1 || startCheckouts[0] != checkout {
		t.Fatalf("start checkouts = %+v, want %q", startCheckouts, checkout)
	}
	// The throwaway agent row and its instance must be gone after the run.
	if _, err := store.GetAgentInstance(ctx, ephemeralName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgentInstance after run error = %v, want no rows", err)
	}
	if _, err := store.GetAgent(ctx, ephemeralName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgent after run error = %v, want no rows", err)
	}
	if !daemonWorkerHasEvent(mustListJobEvents(t, store, "job-eph"), "ephemeral_worker_started") {
		t.Fatalf("missing ephemeral_worker_started event")
	}
}

// TestRunQueuedJobsDisposesEphemeralWorkerOnFailure proves the throwaway agent
// is auto-disposed even when delivery fails: the job reaches a terminal failed
// state and neither the agent row nor its instance survives.
func TestRunQueuedJobsDisposesEphemeralWorkerOnFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	ephemeralName := "impl-ephemeral-def7654321"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:        "job-eph-fail",
		Agent:     ephemeralName,
		Action:    "ask",
		Repo:      "owner/repo",
		Branch:    "main",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.CodexRuntime, Model: "gpt-5.4"},
	})

	startAdapter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440666"}
	deliveryAdapter := &cliWorkerFakeAdapter{err: errors.New("delivery failed")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(string, string) (runtime.Adapter, error) {
		return startAdapter, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-eph-fail")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	if deliveryAdapter.calls != 1 {
		t.Fatalf("delivery calls = %d, want 1", deliveryAdapter.calls)
	}
	// Cleanup must still run on the failure path.
	if _, err := store.GetAgentInstance(ctx, ephemeralName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgentInstance after failed run error = %v, want no rows", err)
	}
	if _, err := store.GetAgent(ctx, ephemeralName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgent after failed run error = %v, want no rows", err)
	}
}

func mustListJobEvents(t *testing.T, store *db.Store, jobID string) []db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	return events
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"late","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			if _, err := workflow.CancelJob(ctx, store, "job-cancel"); err != nil {
				t.Fatalf("CancelJob returned error: %v", err)
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
	events, err := store.ListJobEvents(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "cancel_settled") {
		t.Fatalf("events = %+v, want cancel_settled", events)
	}
}

func TestRunQueuedJobsCancelsActiveDeliveryContext(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-active-cancel", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{
		waitForContextCancel: true,
		onDeliver: func() {
			if _, err := workflow.CancelJob(ctx, store, "job-active-cancel"); err != nil {
				t.Fatalf("CancelJob returned error: %v", err)
			}
		},
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

	if !adapter.observedContextCancel() {
		t.Fatal("adapter did not observe context cancellation")
	}
	job, err := store.GetJob(ctx, "job-active-cancel")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-active-cancel")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "cancel_settled") {
		t.Fatalf("events = %+v, want cancel_settled", events)
	}
}

func TestRunQueuedJobsReleasesRuntimeSessionLockAfterCancellation(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-cancel", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-active-cancel", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{
		waitForContextCancel: true,
		onDeliver: func() {
			if _, err := workflow.CancelJob(ctx, store, "job-active-cancel"); err != nil {
				t.Fatalf("CancelJob returned error: %v", err)
			}
		},
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

	if !adapter.observedContextCancel() {
		t.Fatal("adapter did not observe context cancellation")
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-cancel"); err == nil || err != sql.ErrNoRows {
		t.Fatalf("runtime lock after cancellation error = %v, want no rows", err)
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
		output: `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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

func TestRunQueuedJobsUsesTaskWorktreeForImplement(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-1")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	head, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertAgent(ctx, db.Agent{Name: "reviewer", Role: "reviewer", Runtime: runtime.ShellRuntime, RuntimeRef: "unused", RepoScope: "owner/repo", Capabilities: []string{"review"}, AutonomyPolicy: runtime.AutonomyPolicyAuto, HealthStatus: "ok"}); err != nil {
		t.Fatalf("UpsertAgent reviewer returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-implement-worktree", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, HeadSHA: head, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"implemented","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return workflow.Engine{
			Store:             store,
			RequiredReviewers: []string{"reviewer"},
			PayloadRefresher: func(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
				return refreshDaemonJobPayload(ctx, store, checkout, job, payload)
			},
		}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	wantCheckout, err := filepath.Abs(worktree)
	if err != nil {
		t.Fatalf("Abs returned error: %v", err)
	}
	if adapterCheckout != filepath.Clean(wantCheckout) {
		t.Fatalf("adapter checkout = %q, want %q", adapterCheckout, filepath.Clean(wantCheckout))
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestValidateTargetCheckoutSkipsHeadShaForDelegationWorktreeChild(t *testing.T) {
	// A delegated implement child runs in its own freshly-allocated worktree whose
	// HEAD is the base-branch tip at allocation time; the dispatcher clears the
	// inherited parent HeadSHA. validateTargetCheckout must not reject such a child
	// even when its payload HeadSHA is empty or stale, as long as the worktree is
	// on the job branch and clean. A non-delegation job with a mismatched HeadSHA
	// must still be rejected.
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "task-005")
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	// Delegation child: empty HeadSHA + worktree path -> accepted.
	delegationPayload := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-005",
		DelegationID: "d1",
		ParentJobID:  "parent-job",
		WorktreePath: checkout,
	}
	if err := worker.validateTargetCheckout(ctx, delegationPayload, checkout); err != nil {
		t.Fatalf("validateTargetCheckout rejected delegation worktree child: %v", err)
	}

	// Delegation child with a stale (non-matching) HeadSHA -> still accepted,
	// because the equality check is skipped for delegation worktree children.
	stalePayload := delegationPayload
	stalePayload.HeadSHA = "0000000000000000000000000000000000000000"
	if err := worker.validateTargetCheckout(ctx, stalePayload, checkout); err != nil {
		t.Fatalf("validateTargetCheckout rejected delegation child with stale HeadSHA: %v", err)
	}

	// A non-delegation job with a mismatched HeadSHA must still be rejected so the
	// HeadSHA guard is not weakened for ordinary jobs.
	ordinaryPayload := workflow.JobPayload{
		Repo:    "owner/repo",
		Branch:  "task-005",
		HeadSHA: "0000000000000000000000000000000000000000",
	}
	if err := worker.validateTargetCheckout(ctx, ordinaryPayload, checkout); err == nil {
		t.Fatal("validateTargetCheckout accepted ordinary job with mismatched HeadSHA")
	}
}

func TestValidateTargetCheckoutAcceptsDetachedReadOnlyWorktreeChild(t *testing.T) {
	// A read-only delegation child runs in a *detached* worktree (no branch).
	// CurrentBranch errors on a detached HEAD, so validateTargetCheckout must
	// recognize the delegation worktree child and accept it on the clean-worktree
	// check alone rather than rejecting it on the branch check.
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "task-005")
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	detached := filepath.Join(t.TempDir(), "ro-worktree")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "--detach", detached, "HEAD")

	payload := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-005",
		DelegationID: "d1",
		ParentJobID:  "parent-job",
		WorktreePath: detached,
	}
	if err := worker.validateTargetCheckout(ctx, payload, detached); err != nil {
		t.Fatalf("validateTargetCheckout rejected detached read-only worktree child: %v", err)
	}
}

func TestValidateTargetCheckoutAllowsSharedCheckoutDelegationChildWithoutHeadSHA(t *testing.T) {
	// A read-only delegation child from a local orchestrate (no PR) inherits an
	// empty HeadSHA and runs in the shared checkout (no per-delegation worktree).
	// It must be accepted and run against the current HEAD, not rejected for a
	// missing HeadSHA. A non-delegation job with no HeadSHA is still rejected.
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "task-005")
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	delegationChild := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-005",
		DelegationID: "verify-1",
		ParentJobID:  "parent-job",
		// no WorktreePath (shared checkout), no HeadSHA (PR-less local orchestrate)
	}
	if err := worker.validateTargetCheckout(ctx, delegationChild, checkout); err != nil {
		t.Fatalf("validateTargetCheckout rejected shared-checkout delegation child without HeadSHA: %v", err)
	}

	nonDelegation := workflow.JobPayload{Repo: "owner/repo", Branch: "task-005"}
	if err := worker.validateTargetCheckout(ctx, nonDelegation, checkout); err == nil {
		t.Fatal("validateTargetCheckout accepted a non-delegation job with no HeadSHA")
	}
}

// TestDefaultCheckoutAllowsBranchlessIssueAsk is the regression guard for the
// #389 live bug: an issue `@<agent> ask` job carries the *issue number* in
// PullRequest (>0) but no Branch (the question stands alone). The `ask` case in
// defaultCheckout previously gated its branch validation on PullRequest>0, so an
// issue ask was rejected with "checkout branch is main, not job branch " — the
// job failed instead of answering, and no real reply was ever posted. This test
// drives the real defaultCheckout against a real git checkout that is on `main`
// (not the empty job branch) and asserts the branchless issue ask is accepted.
// It also asserts that a branch-carrying ask (the PR ask) is still validated, so
// the PR ask's checkout guard is not weakened.
func TestDefaultCheckoutAllowsBranchlessIssueAsk(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	issueAsk := workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 7, // the issue number; >0 but carries no branch
		// Branch and HeadSHA intentionally empty (issue ask, no PR context).
	}
	job := db.Job{ID: "issue-comment-regression", Type: "ask"}
	got, err := worker.defaultCheckout(ctx, job, issueAsk, runtime.Agent{})
	if err != nil {
		t.Fatalf("defaultCheckout rejected branchless issue ask: %v", err)
	}
	if got != checkout {
		t.Fatalf("defaultCheckout = %q, want %q", got, checkout)
	}

	// A PR ask carries the PR head branch; when that branch does not match the
	// checkout (here the checkout is on `main`), validation must still fail so the
	// PR ask guard is preserved.
	prAsk := workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 12,
		Branch:      "feature-branch",
	}
	if _, err := worker.defaultCheckout(ctx, job, prAsk, runtime.Agent{}); err == nil {
		t.Fatal("defaultCheckout accepted a PR ask whose branch does not match the checkout")
	}
}

func TestRefreshDaemonJobPayloadPreservesTaskWorktreeHeadForFinalizer(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	checkout := filepath.Join(home, "repo")
	worktree := filepath.Join(home, "task-1")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, checkout)
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "push", "-u", "origin", "main")
	runGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	oldHead, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "owner/repo", Number: 12, URL: "https://github.com/owner/repo/pull/12", HeadBranch: "task-1", BaseBranch: "main", HeadSHA: oldHead, State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("self committed\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit(t, worktree, "add", "feature.txt")
	runGit(t, worktree, "commit", "-m", "agent self commit")
	newHead, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if newHead == oldHead {
		t.Fatal("worker did not create a new commit")
	}
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "task-1", PullRequest: 12, HeadSHA: oldHead, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1", Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}}
	refreshed, err := refreshDaemonJobPayload(ctx, store, worktree, db.Job{ID: "job-implement-worktree", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("refreshDaemonJobPayload returned error: %v", err)
	}
	if refreshed.HeadSHA != oldHead {
		t.Fatalf("refreshed head = %q, want original %q", refreshed.HeadSHA, oldHead)
	}
	finalized, err := (daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}).FinalizeImplementation(ctx, db.Job{ID: "job-implement-worktree", Agent: "lead", Type: "implement"}, refreshed)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	if finalized.HeadSHA != newHead {
		t.Fatalf("finalized head = %q, want %q", finalized.HeadSHA, newHead)
	}
	remoteHead := strings.TrimSpace(runGitOutput(t, checkout, "ls-remote", "origin", "refs/heads/task-1"))
	if !strings.Contains(remoteHead, newHead) {
		t.Fatalf("remote task branch %q does not contain local head %q", remoteHead, newHead)
	}
}

func TestRunQueuedJobsResumesDelegatedImplementWithOriginalBranchLock(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-1")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	head, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead-temp-job-implement", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:               "job-implement",
		Agent:            "lead-temp-job-implement",
		Action:           "implement",
		Repo:             "owner/repo",
		Branch:           "task-1",
		HeadSHA:          head,
		GoalID:           "goal-1",
		TaskID:           "task-1",
		TaskTitle:        "Task 1",
		LeadAgent:        "lead",
		OriginalAgent:    "lead",
		DelegatedAgent:   "lead-temp-job-implement",
		DelegationReason: "runtime_session_busy",
	})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"implemented","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return workflow.Engine{
			Store: store,
			PayloadRefresher: func(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
				return refreshDaemonJobPayload(ctx, store, checkout, job, payload)
			},
		}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	wantCheckout := filepath.Clean(worktree)
	if adapterCheckout != wantCheckout {
		t.Fatalf("adapter checkout = %q, want %q", adapterCheckout, wantCheckout)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunQueuedJobsUsesTaskWorktreeForReview(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-1")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	head, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review-worktree", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, HeadSHA: head, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	wantCheckout, err := filepath.Abs(worktree)
	if err != nil {
		t.Fatalf("Abs returned error: %v", err)
	}
	if adapterCheckout != filepath.Clean(wantCheckout) {
		t.Fatalf("adapter checkout = %q, want %q", adapterCheckout, filepath.Clean(wantCheckout))
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestDaemonImplementationFinalizerCommitsBeforeReusingExistingPullRequest(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "owner/repo", Number: 12, URL: "https://github.com/owner/repo/pull/12", HeadBranch: "task-1", BaseBranch: "main", HeadSHA: "old", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		GoalID:    "goal-1",
		TaskID:    "task-1",
		TaskTitle: "Task 1",
		LeadAgent: "lead",
		Result:    &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}

	if finalized.PullRequest != 12 {
		t.Fatalf("pull request = %d, want 12", finalized.PullRequest)
	}
	clean, err := (gitutil.Client{Dir: repoDir}).WorktreeClean(ctx)
	if err != nil {
		t.Fatalf("WorktreeClean returned error: %v", err)
	}
	if !clean {
		t.Fatal("worktree still has uncommitted changes")
	}
	localHead, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if finalized.HeadSHA != localHead {
		t.Fatalf("finalized head = %q, want %q", finalized.HeadSHA, localHead)
	}
	remoteHead := strings.TrimSpace(runGitOutput(t, repoDir, "ls-remote", "origin", "refs/heads/task-1"))
	if !strings.Contains(remoteHead, localHead) {
		t.Fatalf("remote task branch %q does not contain local head %q", remoteHead, localHead)
	}
}

// stubEnsureGitHub adopts a pre-existing open PR via EnsurePullRequest, the way
// the real GhClient does when GetOpenPullRequestByHead finds one. It records
// whether CreatePullRequest was called so the test can prove the finalizer took
// the idempotent adopt path instead of erroring on a 422 race.
type stubEnsureGitHub struct {
	github.NoopClient
	existing    github.PullRequest
	ensureCalls int
	createCalls int
	ensureErr   error
}

func (s *stubEnsureGitHub) EnsurePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	s.ensureCalls++
	if s.ensureErr != nil {
		return github.PullRequest{}, s.ensureErr
	}
	return s.existing, nil
}

func (s *stubEnsureGitHub) CreatePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	s.createCalls++
	return github.PullRequest{}, errors.New("CreatePullRequest should not be called when EnsurePullRequest adopts")
}

// With no local PR record, the finalizer must adopt an out-of-band/concurrent
// open PR via EnsurePullRequest instead of erroring with a BlockedError — the
// #387 idempotency fix.
func TestDaemonImplementationFinalizerAdoptsExistingPullRequestViaEnsure(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	// No local PR record: the local fast-path misses, so the github-side ensure runs.
	gh := &stubEnsureGitHub{existing: github.PullRequest{
		Number:  99,
		URL:     "https://github.com/owner/repo/pull/99",
		State:   "open",
		HeadRef: "task-1",
		BaseRef: "main",
	}}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: gh}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		GoalID:    "goal-1",
		TaskID:    "task-1",
		TaskTitle: "Task 1",
		LeadAgent: "lead",
		Result:    &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	var blocked workflow.BlockedError
	if errors.As(err, &blocked) {
		t.Fatalf("FinalizeImplementation returned BlockedError on an existing PR: %v", err)
	}
	if finalized.PullRequest != 99 {
		t.Fatalf("finalized pull request = %d, want 99 (adopted)", finalized.PullRequest)
	}
	if gh.ensureCalls != 1 {
		t.Fatalf("EnsurePullRequest calls = %d, want 1", gh.ensureCalls)
	}
	if gh.createCalls != 0 {
		t.Fatalf("CreatePullRequest calls = %d, want 0 (adopt, not create)", gh.createCalls)
	}
	// The adopted PR is persisted locally.
	stored, err := store.GetPullRequest(ctx, "owner/repo", 99)
	if err != nil {
		t.Fatalf("GetPullRequest(99) returned error: %v", err)
	}
	if stored.HeadBranch != "task-1" || !strings.EqualFold(stored.State, "open") {
		t.Fatalf("stored PR = %+v", stored)
	}
}

// stubSkipFlagAtOpenGitHub records whether the branch lock's skip flag was
// already persisted at the moment the PR is opened — the #390 invariant: the
// flag must be durable BEFORE the daemon-watched PR becomes observable, or the
// PR-watcher can fan out native reviews in the gap.
type stubSkipFlagAtOpenGitHub struct {
	github.NoopClient
	store      *db.Store
	repo       string
	branch     string
	pr         github.PullRequest
	opened     bool
	skipAtOpen bool
}

func (s *stubSkipFlagAtOpenGitHub) EnsurePullRequest(ctx context.Context, _ github.CreatePullRequestInput) (github.PullRequest, error) {
	s.opened = true
	if lock, err := s.store.GetBranchLock(ctx, s.repo, s.branch); err == nil {
		s.skipAtOpen = lock.SkipNativeReviewFanout
	}
	return s.pr, nil
}

// #390: with --skip-native-review-fanout, the finalizer must persist the skip
// flag onto the branch lock BEFORE it opens the PR, so there is no TOCTOU window
// in which the daemon's PR-watcher sees the fresh PR with the flag unset.
func TestDaemonImplementationFinalizerPersistsSkipFanoutBeforeOpeningPR(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	// The branch lock exists from job start; the finalizer flips its flag.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	gh := &stubSkipFlagAtOpenGitHub{store: store, repo: "owner/repo", branch: "task-1", pr: github.PullRequest{
		Number:  7,
		URL:     "https://github.com/owner/repo/pull/7",
		State:   "open",
		HeadRef: "task-1",
		BaseRef: "main",
	}}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: gh}
	payload := workflow.JobPayload{
		Repo:                   "owner/repo",
		Branch:                 "task-1",
		GoalID:                 "goal-1",
		TaskID:                 "task-1",
		TaskTitle:              "Task 1",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
		Result:                 &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	if _, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload); err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	if !gh.opened {
		t.Fatal("EnsurePullRequest was never called; cannot assert ordering")
	}
	if !gh.skipAtOpen {
		t.Fatal("#390 regression: branch-lock skip flag was NOT persisted before the PR was opened")
	}
	// And the flag is durably set after finalize.
	lock, err := store.GetBranchLock(ctx, "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if !lock.SkipNativeReviewFanout {
		t.Fatalf("expected lock.SkipNativeReviewFanout = true after finalize, got %+v", lock)
	}
}

// #390 edge case: a re-finalize with no new changes and a PR already attached
// takes the early "no changes" return BEFORE any PR-open path. The skip flag must
// still be persisted on that path (it is written-ahead as soon as the branch is
// confirmed), or a retry could leave the flag unset and reintroduce the TOCTOU.
func TestDaemonImplementationFinalizerPersistsSkipFanoutOnNoChangeReFinalize(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit(t, repoDir, "add", "feature.txt")
	runGit(t, repoDir, "commit", "-m", "work")
	head, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	// Clean worktree + PR already attached + head unchanged ⇒ the no-changes early
	// return. NoopClient: no PR-open call is expected on this path.
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:                   "owner/repo",
		Branch:                 "task-1",
		PullRequest:            7,
		HeadSHA:                head,
		GoalID:                 "goal-1",
		TaskID:                 "task-1",
		TaskTitle:              "Task 1",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
		Result:                 &workflow.AgentResult{Decision: "implemented", Summary: "no new changes"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	if finalized.PullRequest != 7 {
		t.Fatalf("finalized.PullRequest = %d, want 7 (unchanged on no-change path)", finalized.PullRequest)
	}
	lock, err := store.GetBranchLock(ctx, "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if !lock.SkipNativeReviewFanout {
		t.Fatal("#390 regression: skip flag NOT persisted on the no-changes early-return path")
	}
}

func TestDaemonImplementationFinalizerPushesAlreadyCommittedWork(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	oldHead, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("already committed\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit(t, repoDir, "add", "feature.txt")
	runGit(t, repoDir, "commit", "-m", "agent commit")

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "owner/repo", Number: 12, URL: "https://github.com/owner/repo/pull/12", HeadBranch: "task-1", BaseBranch: "main", HeadSHA: oldHead, State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		HeadSHA:   oldHead,
		GoalID:    "goal-1",
		TaskID:    "task-1",
		TaskTitle: "Task 1",
		LeadAgent: "lead",
		Result:    &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	localHead, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if finalized.HeadSHA != localHead || finalized.HeadSHA == oldHead {
		t.Fatalf("finalized head = %q, local=%q old=%q", finalized.HeadSHA, localHead, oldHead)
	}
	remoteHead := strings.TrimSpace(runGitOutput(t, repoDir, "ls-remote", "origin", "refs/heads/task-1"))
	if !strings.Contains(remoteHead, localHead) {
		t.Fatalf("remote task branch %q does not contain local head %q", remoteHead, localHead)
	}
}

func TestDaemonImplementationFinalizerBlocksWrongBranch(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "wrong-branch")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("wrong branch work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		GoalID:    "goal-1",
		TaskID:    "task-1",
		TaskTitle: "Task 1",
		LeadAgent: "lead",
		Result:    &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	_, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	var blocked workflow.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("FinalizeImplementation error = %v, want BlockedError", err)
	}
	if !strings.Contains(blocked.Reason, "not task-1") {
		t.Fatalf("blocked reason = %q", blocked.Reason)
	}
}

func TestDaemonImplementationFinalizerAllowsAlreadyFinalizedPullRequest(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	head, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 12,
		HeadSHA:     head,
		GoalID:      "goal-1",
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
		LeadAgent:   "lead",
		Result:      &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	if finalized.HeadSHA != head || finalized.PullRequest != 12 || finalized.Branch != "task-1" {
		t.Fatalf("finalized payload = %+v", finalized)
	}
}

func TestRunQueuedJobsKeepsReviewOnRegisteredCheckoutWithoutTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "task-1")
	head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review-main-checkout", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, HeadSHA: head, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapterCheckout != checkout {
		t.Fatalf("adapter checkout = %q, want registered checkout %q", adapterCheckout, checkout)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestMergeGateCheckoutUsesRegisteredCheckoutOverTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-1")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	got, err := mergeGateCheckout(ctx, store, "owner/repo", worktree)
	if err != nil {
		t.Fatalf("mergeGateCheckout returned error: %v", err)
	}
	if got != checkout {
		t.Fatalf("merge gate checkout = %q, want registered checkout %q", got, checkout)
	}
}

func TestNewDaemonPolicyMergeGateIncludesWorktreeCleaner(t *testing.T) {
	gate := newDaemonPolicyMergeGate(nil, github.NoopClient{}, "/tmp/gitmoot-checkout")

	if gate.Worktrees == nil {
		t.Fatal("daemon merge gate missing worktree cleaner")
	}
	if gate.CheckoutPath != "/tmp/gitmoot-checkout" {
		t.Fatalf("checkout path = %q", gate.CheckoutPath)
	}
	if !gate.DeleteBranch {
		t.Fatal("daemon merge gate should delete merged branches")
	}
}

func TestDaemonMergeGatePreservesInjectedGitHubClient(t *testing.T) {
	fake := github.NoopClient{}
	gate := daemonMergeGate{GitHub: fake}

	if got := gate.githubClient("/tmp/checkout"); got != fake {
		t.Fatalf("github client = %#v, want injected fake %#v", got, fake)
	}
}

func TestSelectRunnableQueuedJobsAllowsSeparateTaskWorktrees(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "lead-a", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead-b", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: "/tmp/gitmoot/task-a"}); err != nil {
		t.Fatalf("UpsertTask task-a returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-b", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-b", WorktreePath: "/tmp/gitmoot/task-b"}); err != nil {
		t.Fatalf("UpsertTask task-b returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead-a", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "lead-b", Action: "implement", Repo: "owner/repo", Branch: "task-b", TaskID: "task-b"})
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}

	selected, remaining := selectRunnableQueuedJobs(ctx, store, jobs, 2)

	if len(selected) != 2 || len(remaining) != 0 {
		t.Fatalf("selected=%d remaining=%d, want two selected separate worktrees", len(selected), len(remaining))
	}
}

func TestSelectRunnableQueuedJobsKeepsSameRuntimeSerializedAcrossWorktrees(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "lead-a", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead-b", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: "/tmp/gitmoot/task-a"}); err != nil {
		t.Fatalf("UpsertTask task-a returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-b", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-b", WorktreePath: "/tmp/gitmoot/task-b"}); err != nil {
		t.Fatalf("UpsertTask task-b returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead-a", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "lead-b", Action: "implement", Repo: "owner/repo", Branch: "task-b", TaskID: "task-b"})
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}

	selected, remaining := selectRunnableQueuedJobs(ctx, store, jobs, 2)

	if len(selected) != 1 || len(remaining) != 1 {
		t.Fatalf("selected=%d remaining=%d, want same runtime session serialized", len(selected), len(remaining))
	}
}

func TestSelectRunnableQueuedJobsAllowsForkEligibleSameRuntime(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead-a", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead-b", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	worktreeA := t.TempDir()
	worktreeB := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: worktreeA}); err != nil {
		t.Fatalf("UpsertTask task-a returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-b", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-b", WorktreePath: worktreeB}); err != nil {
		t.Fatalf("UpsertTask task-b returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead-a", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "lead-b", Action: "implement", Repo: "owner/repo", Branch: "task-b", TaskID: "task-b"})
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}

	selected, remaining := selectRunnableQueuedJobsWithPolicy(ctx, store, jobs, 2, config.DefaultParallelSessionPolicy())

	if len(selected) != 2 || len(remaining) != 0 {
		t.Fatalf("selected=%d remaining=%d, want fork-eligible same runtime selected", len(selected), len(remaining))
	}
}

func TestSelectRunnableQueuedJobsCountsExternallyBusyRuntimeAgainstTempCap(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	worktreeA := t.TempDir()
	worktreeB := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: worktreeA}); err != nil {
		t.Fatalf("UpsertTask task-a returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-b", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-b", WorktreePath: worktreeB}); err != nil {
		t.Fatalf("UpsertTask task-b returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-b", TaskID: "task-b"})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	policy := config.DefaultParallelSessionPolicy()
	policy.MaxTempSessionsPerAgent = 1

	selected, remaining := selectRunnableQueuedJobsWithPolicy(ctx, store, jobs, 2, policy)

	if len(selected) != 1 || len(remaining) != 1 {
		t.Fatalf("selected=%d remaining=%d, want external busy runtime counted against temp cap", len(selected), len(remaining))
	}
}

func TestTempWorkerEligibleAllowsWritableImplementWithTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := t.TempDir()
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	got := tempWorkerEligible(ctx, store, job, payload, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}, config.DefaultParallelSessionPolicy(), time.Now().UTC())

	if !got.Eligible {
		t.Fatalf("tempWorkerEligible = %+v, want eligible", got)
	}
}

func TestTempWorkerEligibleRejectsQueuePolicy(t *testing.T) {
	policy := config.DefaultParallelSessionPolicy()
	policy.SameSession = config.ParallelSessionQueue

	got := tempWorkerEligible(context.Background(), nil, db.Job{ID: "job-a", Type: "ask"}, workflow.JobPayload{}, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime}, policy, time.Now().UTC())

	if got.Eligible || !strings.Contains(got.Reason, "same_session is queue") {
		t.Fatalf("tempWorkerEligible = %+v, want queue policy rejection", got)
	}
}

func TestTempWorkerEligibleRejectsAlreadyDelegatedJob(t *testing.T) {
	got := tempWorkerEligible(context.Background(), nil, db.Job{ID: "job-a", Type: "ask"}, workflow.JobPayload{DelegationReason: "runtime_session_busy"}, runtime.Agent{Name: "lead-temp-job-a", Runtime: runtime.CodexRuntime}, config.DefaultParallelSessionPolicy(), time.Now().UTC())

	if got.Eligible || !strings.Contains(got.Reason, "delegated temp worker waits") {
		t.Fatalf("tempWorkerEligible = %+v, want delegated job rejection", got)
	}
}

func TestJobWorkerParallelSessionPolicyUsesDefaultWhenHomeNotExplicit(t *testing.T) {
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	got, err := worker.parallelSessionPolicy()
	if err != nil {
		t.Fatalf("parallelSessionPolicy returned error: %v", err)
	}

	if got.SameSession != config.ParallelSessionForkTempSession {
		t.Fatalf("same_session = %q, want default fork_temp_session", got.SameSession)
	}
}

func TestJobWorkerParallelSessionPolicyLoadsExplicitHome(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	content := strings.Replace(config.DefaultConfig(paths), `same_session = "fork_temp_session"`, `same_session = "queue"`, 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)

	got, err := worker.parallelSessionPolicy()
	if err != nil {
		t.Fatalf("parallelSessionPolicy returned error: %v", err)
	}

	if got.SameSession != config.ParallelSessionQueue {
		t.Fatalf("same_session = %q, want explicit queue config", got.SameSession)
	}
}

func TestTempWorkerEligibleRejectsReadOnlyImplementation(t *testing.T) {
	got := tempWorkerEligible(context.Background(), nil, db.Job{ID: "job-a", Type: "implement"}, workflow.JobPayload{}, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyReadOnly}, config.DefaultParallelSessionPolicy(), time.Now().UTC())

	if got.Eligible || !strings.Contains(got.Reason, "writable agent policy") {
		t.Fatalf("tempWorkerEligible = %+v, want read-only implementation rejection", got)
	}
}

func TestTempWorkerEligibleRejectsImplementationWithoutTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	got := tempWorkerEligible(ctx, store, db.Job{ID: "job-a", Type: "implement"}, workflow.JobPayload{Repo: "owner/repo", TaskID: "task-a"}, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}, config.DefaultParallelSessionPolicy(), time.Now().UTC())

	if got.Eligible || !strings.Contains(got.Reason, "task worktree") {
		t.Fatalf("tempWorkerEligible = %+v, want missing worktree rejection", got)
	}
}

func TestTempWorkerEligibleRejectsMaxTempWorkers(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		if err := store.UpsertAgentInstance(ctx, db.AgentInstance{
			Name:           fmt.Sprintf("lead-temp-%d", i),
			Type:           tempWorkerAgentType("lead"),
			Runtime:        runtime.CodexRuntime,
			RuntimeRef:     fmt.Sprintf("session-%d", i),
			RepoFullName:   "owner/repo",
			Role:           "worker",
			AutonomyPolicy: runtime.AutonomyPolicyAuto,
			State:          "idle",
			CreatedAt:      now.Format(time.RFC3339),
			LastUsedAt:     now.Format(time.RFC3339),
			ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("UpsertAgentInstance %d returned error: %v", i, err)
		}
	}

	got := tempWorkerEligible(ctx, store, db.Job{ID: "job-a", Type: "ask"}, workflow.JobPayload{}, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyAuto}, config.DefaultParallelSessionPolicy(), now)

	if got.Eligible || !strings.Contains(got.Reason, "max temp workers reached") {
		t.Fatalf("tempWorkerEligible = %+v, want cap rejection", got)
	}
}

func TestDaemonReviewersIgnoresTempWorkerInstances(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"review"}, "owner/repo")
	now := time.Now().UTC()
	if err := store.UpsertAgentInstance(ctx, db.AgentInstance{
		Name:           "audit-temp-job-a",
		Type:           tempWorkerAgentType("audit"),
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     "550e8400-e29b-41d4-a716-446655440555",
		RepoFullName:   "owner/repo",
		Role:           "reviewer",
		Capabilities:   []string{"review"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		State:          "idle",
		CreatedAt:      now.Format(time.RFC3339),
		LastUsedAt:     now.Format(time.RFC3339),
		ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}

	reviewers, err := daemonReviewers(ctx, store, "owner/repo")
	if err != nil {
		t.Fatalf("daemonReviewers returned error: %v", err)
	}

	if !reflect.DeepEqual(reviewers, []string{"audit"}) {
		t.Fatalf("reviewers = %+v, want only regular configured reviewer", reviewers)
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, HeadSHA: strings.Repeat("0", 40), TaskID: "task-1"})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "owner/repo-a", ""); err != nil {
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

// --- #394 PR1: opt-in continuous worker-pool scheduler (--scheduler=pool) ---

const poolSchedulerAskResult = `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

func TestParseSchedulerMode(t *testing.T) {
	cases := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"barrier", false, false},
		{"pool", true, false},
		{"  pool  ", true, false},
		{"bogus", false, true},
	}
	for _, tc := range cases {
		got, err := parseSchedulerMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSchedulerMode(%q) err = nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSchedulerMode(%q) err = %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("parseSchedulerMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDaemonChildArgsForwardsPoolScheduler(t *testing.T) {
	pool := daemonChildArgs("/tmp/home", "30s", 1, false, false, "pool", "", "")
	if !daemonArgsContainFlag(pool, "scheduler", "pool") {
		t.Fatalf("pool child args missing --scheduler pool: %v", pool)
	}
	parsed, code := parseDaemonStartConfig("daemon restart", pool[2:], io.Discard)
	if code != 0 {
		t.Fatalf("parse pool child args code = %d: %v", code, pool)
	}
	if parsed.Scheduler != "pool" || !parsed.ExplicitScheduler {
		t.Fatalf("parsed pool scheduler = %q explicit=%v", parsed.Scheduler, parsed.ExplicitScheduler)
	}
	// barrier (the default) appends no --scheduler flag, keeping the child argv
	// byte-identical to before this change.
	barrier := daemonChildArgs("/tmp/home", "30s", 1, false, false, "barrier", "", "")
	for _, a := range barrier {
		if a == "--scheduler" {
			t.Fatalf("barrier child args should not carry --scheduler: %v", barrier)
		}
	}
	if _, code := parseDaemonStartConfig("daemon start", []string{"--scheduler", "bogus"}, io.Discard); code != 2 {
		t.Fatalf("invalid --scheduler code = %d, want 2", code)
	}
}

func poolSchedulerWorker(t *testing.T, store *db.Store, adapter workflow.DeliveryAdapter, usePool bool) jobWorker {
	t.Helper()
	worker := defaultJobWorker(store, io.Discard)
	worker.UsePool = usePool
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	return worker
}

func TestRunQueuedJobsPoolDrainsIndependentJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	worker := poolSchedulerWorker(t, store, adapter, true)

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("pool runQueuedJobsForRepo: %v", err)
	}
	for _, id := range []string{"job-a", "job-b"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded", id, job.State)
		}
	}
}

// runMidFlightEnqueueScenario runs job-a, which enqueues job-b (a different repo,
// so a distinct checkout key) once dispatch has begun, then returns job-b's final
// state. The pool re-queries and runs it; the barrier never re-queries, so it
// stays queued — the #394 layer-1 behavior the pool fixes.
func runMidFlightEnqueueScenario(t *testing.T, usePool bool) string {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})

	var once sync.Once
	var enqErr error
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = func() {
		once.Do(func() {
			_, enqErr = (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
		})
	}
	worker := poolSchedulerWorker(t, store, adapter, usePool)

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo(usePool=%v): %v", usePool, err)
	}
	if enqErr != nil {
		t.Fatalf("mid-flight enqueue of job-b failed: %v", enqErr)
	}
	job, err := store.GetJob(ctx, "job-b")
	if err != nil {
		t.Fatalf("GetJob job-b: %v", err)
	}
	return job.State
}

func TestRunQueuedJobsPoolPicksUpMidFlightJob(t *testing.T) {
	if got := runMidFlightEnqueueScenario(t, true); got != string(workflow.JobSucceeded) {
		t.Fatalf("pool: job-b state = %q, want succeeded (mid-flight job not picked up)", got)
	}
	if got := runMidFlightEnqueueScenario(t, false); got != string(workflow.JobQueued) {
		t.Fatalf("barrier: job-b state = %q, want queued (barrier must not re-query mid-tick)", got)
	}
}

func TestRunQueuedJobsPoolStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	// The worker blocks until its context is cancelled; the pool must drain it and
	// return rather than hang.
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult, waitForContextCancel: true}
	worker := poolSchedulerWorker(t, store, adapter, true)

	errCh := make(chan error, 1)
	go func() { errCh <- runQueuedJobsForRepo(ctx, worker, 2, "", "") }()
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not return after context cancellation (hang)")
	}
}

type poolConcurrencyTracker struct {
	mu     sync.Mutex
	active int
	max    int
}

func (c *poolConcurrencyTracker) span() {
	c.mu.Lock()
	c.active++
	if c.active > c.max {
		c.max = c.active
	}
	c.mu.Unlock()
	time.Sleep(25 * time.Millisecond)
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
}

func (c *poolConcurrencyTracker) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.max
}

func runPoolConcurrencyScenario(t *testing.T, repoB string) int {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if repoB != "owner/repo-a" {
		seedDaemonWorkerRepo(t, store, repoB, t.TempDir())
		if err := store.AllowAgentRepo(ctx, "audit", repoB); err != nil {
			t.Fatalf("AllowAgentRepo: %v", err)
		}
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-1", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-2", Agent: "audit", Action: "ask", Repo: repoB, Branch: "main", PullRequest: 2})

	tracker := &poolConcurrencyTracker{}
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = tracker.span
	worker := poolSchedulerWorker(t, store, adapter, true)

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo: %v", err)
	}
	return tracker.peak()
}

func TestRunQueuedJobsPoolHonorsCheckoutSafety(t *testing.T) {
	// Same repo, no worktree ⇒ same checkout key ⇒ live accounting must serialize
	// them even at --workers 2 (working-tree safety, #394 layer 2).
	if peak := runPoolConcurrencyScenario(t, "owner/repo-a"); peak != 1 {
		t.Fatalf("same-repo peak concurrency = %d, want 1 (must serialize same checkout key)", peak)
	}
	// Distinct repos ⇒ distinct checkout keys ⇒ the pool runs them in parallel.
	if peak := runPoolConcurrencyScenario(t, "owner/repo-b"); peak != 2 {
		t.Fatalf("distinct-repo peak concurrency = %d, want 2 (distinct keys must parallelize)", peak)
	}
}

func TestRunQueuedJobsPoolIsolatesContendedReadJob(t *testing.T) {
	// Two same-repo read (ask) jobs under the pool with isolation enabled
	// (ConfigHome + a real checkout): one runs in the shared checkout, the other is
	// auto-isolated into a detached worktree so it runs beside it (#394 part 2)
	// instead of serializing/deadlocking, and the worktree is disposed afterward.
	ctx := context.Background()
	home := t.TempDir()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-1", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-2", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	worker := poolSchedulerWorker(t, store, adapter, true)
	worker.ConfigHome = home // enable isolation

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("pool run: %v", err)
	}

	isolated := 0
	for _, id := range []string{"job-1", "job-2"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded (contended read job must run, not stay queued)", id, job.State)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("payload %s: %v", id, err)
		}
		if payload.WorktreePath != "" {
			isolated++
			if _, statErr := os.Stat(payload.WorktreePath); !os.IsNotExist(statErr) {
				t.Fatalf("%s isolation worktree %s was not cleaned up", id, payload.WorktreePath)
			}
		}
	}
	if isolated != 1 {
		t.Fatalf("isolated read jobs = %d, want exactly 1 (the contended one)", isolated)
	}
}

func TestRunQueuedJobsPoolRecoversWorkerPanicAndCleansWorktree(t *testing.T) {
	// A panicking worker must not hang the pool or crash the daemon, and any
	// isolation worktree allocated for the contended job must still be disposed
	// (the always-send-to-done invariant keeps reap's cleanup intact).
	ctx := context.Background()
	home := t.TempDir()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-1", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-2", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = func() { panic("boom") }
	worker := poolSchedulerWorker(t, store, adapter, true)
	worker.ConfigHome = home

	resultCh := make(chan error, 1)
	go func() { resultCh <- runQueuedJobsForRepo(ctx, worker, 2, "", "") }()
	select {
	case err := <-resultCh:
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("err = %v, want a recovered panic error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pool hung after a worker panic (recovery did not send to done)")
	}
	for _, id := range []string{"job-1", "job-2"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("payload %s: %v", id, err)
		}
		if payload.WorktreePath != "" {
			if _, statErr := os.Stat(payload.WorktreePath); !os.IsNotExist(statErr) {
				t.Fatalf("%s isolation worktree %s leaked after panic", id, payload.WorktreePath)
			}
		}
	}
}

func TestRunQueuedJobsForRepoSkipsOtherSessions(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	// The root coordinator job (its own id is the root) and a child carrying the
	// root id both belong to session "root-coordinator"; a job from a different
	// root does not.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "root-coordinator", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "child-of-root", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 2, RootJobID: "root-coordinator"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "other-root-job", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 3, RootJobID: "other-root"})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 3, "", "root-coordinator"); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}

	root, err := store.GetJob(ctx, "root-coordinator")
	if err != nil {
		t.Fatalf("GetJob root-coordinator returned error: %v", err)
	}
	child, err := store.GetJob(ctx, "child-of-root")
	if err != nil {
		t.Fatalf("GetJob child-of-root returned error: %v", err)
	}
	other, err := store.GetJob(ctx, "other-root-job")
	if err != nil {
		t.Fatalf("GetJob other-root-job returned error: %v", err)
	}
	// The root coordinator (job.ID == session) ran.
	if root.State != string(workflow.JobSucceeded) {
		t.Fatalf("root-coordinator state = %q, want succeeded", root.State)
	}
	// The child (payload.RootJobID == session) ran.
	if child.State != string(workflow.JobSucceeded) {
		t.Fatalf("child-of-root state = %q, want succeeded", child.State)
	}
	// The non-matching root stayed queued.
	if other.State != string(workflow.JobQueued) {
		t.Fatalf("other-root-job state = %q, want queued", other.State)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

func TestRunQueuedJobsForRepoAppliesRepoAndSessionAndMatch(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	// Only the job matching BOTH repo AND session should run.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "match", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1, RootJobID: "root-coordinator"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "wrong-repo", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2, RootJobID: "root-coordinator"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "wrong-session", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 3, RootJobID: "other-root"})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 3, "owner/repo-a", "root-coordinator"); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}

	match, err := store.GetJob(ctx, "match")
	if err != nil {
		t.Fatalf("GetJob match returned error: %v", err)
	}
	wrongRepo, err := store.GetJob(ctx, "wrong-repo")
	if err != nil {
		t.Fatalf("GetJob wrong-repo returned error: %v", err)
	}
	wrongSession, err := store.GetJob(ctx, "wrong-session")
	if err != nil {
		t.Fatalf("GetJob wrong-session returned error: %v", err)
	}
	if match.State != string(workflow.JobSucceeded) {
		t.Fatalf("match state = %q, want succeeded", match.State)
	}
	if wrongRepo.State != string(workflow.JobQueued) {
		t.Fatalf("wrong-repo state = %q, want queued", wrongRepo.State)
	}
	if wrongSession.State != string(workflow.JobQueued) {
		t.Fatalf("wrong-session state = %q, want queued", wrongSession.State)
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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
	if err := retryPendingJobAdvancements(ctx, worker, "", ""); err != nil {
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

	if err := retryPendingJobAdvancements(ctx, worker, "", ""); err != nil {
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

	if err := retryPendingJobAdvancements(ctx, worker, "", ""); err != nil {
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

func TestJobNeedsAdvanceRetryResetsAfterJobRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-retry-reset", Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: `{"repo":"owner/repo"}`}, db.JobEvent{
		JobID:   "job-retry-reset",
		Kind:    string(workflow.JobFailed),
		Message: "failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-retry-reset", Kind: "advance_retry", Message: "old advance retry"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	if _, err := workflow.RetryJob(ctx, store, "job-retry-reset"); err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	needsRetry, err := worker.jobNeedsAdvanceRetry(ctx, "job-retry-reset")
	if err != nil {
		t.Fatalf("jobNeedsAdvanceRetry returned error: %v", err)
	}
	if needsRetry {
		t.Fatal("jobNeedsAdvanceRetry returned true after retry_queued reset")
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
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, "")
	}

	if err := retryPendingJobAdvancements(ctx, worker, "", ""); err != nil {
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
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
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

func TestRecoverCancelledRunningJobsSettlesAbandonedCancellation(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-cancelled", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-cancelled", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	if _, err := workflow.CancelJob(ctx, store, "job-cancelled"); err != nil {
		t.Fatalf("CancelJob returned error: %v", err)
	}

	if err := recoverCancelledRunningJobsForRepo(ctx, store, io.Discard, "owner/repo", ""); err != nil {
		t.Fatalf("recoverCancelledRunningJobsForRepo returned error: %v", err)
	}

	events, err := store.ListJobEvents(ctx, "job-cancelled")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "cancel_settled") {
		t.Fatalf("events = %+v, want cancel_settled", events)
	}
	if _, err := workflow.RetryJob(ctx, store, "job-cancelled"); err != nil {
		t.Fatalf("RetryJob after cancelled recovery returned error: %v", err)
	}
}

func TestRecoverCancelledRunningJobsForRepoSkipsOtherRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 1})
	for _, id := range []string{"job-a", "job-b"} {
		if err := store.UpdateJobState(ctx, id, string(workflow.JobRunning)); err != nil {
			t.Fatalf("UpdateJobState(%s) returned error: %v", id, err)
		}
		if _, err := workflow.CancelJob(ctx, store, id); err != nil {
			t.Fatalf("CancelJob(%s) returned error: %v", id, err)
		}
	}

	if err := recoverCancelledRunningJobsForRepo(ctx, store, io.Discard, "owner/repo-a", ""); err != nil {
		t.Fatalf("recoverCancelledRunningJobsForRepo returned error: %v", err)
	}

	eventsA, err := store.ListJobEvents(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListJobEvents job-a returned error: %v", err)
	}
	eventsB, err := store.ListJobEvents(ctx, "job-b")
	if err != nil {
		t.Fatalf("ListJobEvents job-b returned error: %v", err)
	}
	if !daemonWorkerHasEvent(eventsA, "cancel_settled") {
		t.Fatalf("eventsA = %+v, want cancel_settled", eventsA)
	}
	if daemonWorkerHasEvent(eventsB, "cancel_settled") {
		t.Fatalf("eventsB = %+v, want no cancel_settled", eventsB)
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

	if err := runDaemonWorkerTick(ctx, store, worker, 0, false, "", "", io.Discard, now); err != nil {
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

	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, time.Now().UTC().Add(time.Second), "owner/repo-a", ""); err != nil {
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

func (f *cliPollFakeGitHub) ListIssues(context.Context, github.Repository, string) ([]github.Issue, error) {
	return nil, nil
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
	mu                   sync.Mutex
	output               string
	err                  error
	startRuntimeRef      string
	startErr             error
	startCalls           int
	startCheckouts       []string
	calls                int
	delivered            []string
	onStart              func()
	onDeliver            func()
	waitForContextCancel bool
	contextCancelled     bool
}

func (f *cliWorkerFakeAdapter) Name() string {
	return "fake"
}

func (f *cliWorkerFakeAdapter) Start(_ context.Context, _ runtime.StartRequest) (runtime.StartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.startErr != nil {
		return runtime.StartResult{}, f.startErr
	}
	onStart := f.onStart
	ref := strings.TrimSpace(f.startRuntimeRef)
	if ref == "" {
		ref = "550e8400-e29b-41d4-a716-446655440000"
	}
	if onStart != nil {
		onStart()
	}
	return runtime.StartResult{RuntimeRef: ref}, nil
}

func (f *cliWorkerFakeAdapter) Validate(context.Context, runtime.Agent) error {
	return nil
}

func (f *cliWorkerFakeAdapter) Deliver(ctx context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	f.mu.Lock()
	f.calls++
	f.delivered = append(f.delivered, job.ID)
	onDeliver := f.onDeliver
	waitForContextCancel := f.waitForContextCancel
	f.mu.Unlock()
	if onDeliver != nil {
		onDeliver()
	}
	if waitForContextCancel {
		<-ctx.Done()
		f.mu.Lock()
		f.contextCancelled = true
		f.mu.Unlock()
		return runtime.Result{}, ctx.Err()
	}
	if f.err != nil {
		return runtime.Result{}, f.err
	}
	return runtime.Result{Raw: f.output}, nil
}

func (f *cliWorkerFakeAdapter) Health(context.Context, runtime.Agent) error {
	return nil
}

func (f *cliWorkerFakeAdapter) Capabilities(context.Context) ([]string, error) {
	return nil, nil
}

func (f *cliWorkerFakeAdapter) observedContextCancel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contextCancelled
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

func createDaemonWorkerGitCheckout(t *testing.T, branch string) string {
	t.Helper()
	checkout := t.TempDir()
	runDaemonWorkerGit(t, checkout, "init", "-b", branch)
	runDaemonWorkerGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runDaemonWorkerGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "README.md")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "initial")
	runDaemonWorkerGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	return checkout
}

func runDaemonWorkerGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
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
	seedDaemonWorkerAgentWithPolicy(t, store, name, runtimeName, runtimeRef, capabilities, repo, runtime.AutonomyPolicyAuto)
}

func seedDaemonWorkerAgentWithPolicy(t *testing.T, store *db.Store, name string, runtimeName string, runtimeRef string, capabilities []string, repo string, policy string) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           name,
		Role:           "worker",
		Runtime:        runtimeName,
		RuntimeRef:     runtimeRef,
		RepoScope:      repo,
		Capabilities:   capabilities,
		AutonomyPolicy: policy,
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

// TestRunQueuedJobsForRepoSkipsChildrenOfKilledRoot pins the #341 daemon half of
// the operator kill switch: once a tree's root is marked killed, a queued CHILD
// of that root is skipped by runQueuedJobsForRepo (never delivered, stays
// queued), while a queued child of an un-killed root still runs.
func TestRunQueuedJobsForRepoSkipsChildrenOfKilledRoot(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "w", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")

	killedChildPayload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", Sender: "w", RootJobID: "killed-root", DelegationID: "d1"})
	liveChildPayload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", Sender: "w", RootJobID: "live-root", DelegationID: "d2"})
	// A continuation of the killed root carries NO DelegationID and MUST still run
	// so the engine routes the tree through the graceful #305 finalize.
	killedContinuationPayload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", Sender: "w", RootJobID: "killed-root", DelegationFinalize: true})
	for _, j := range []db.Job{
		{ID: "killed-root", Agent: "w", Type: "ask", State: string(workflow.JobSucceeded), Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Sender: "w"})},
		{ID: "live-root", Agent: "w", Type: "ask", State: string(workflow.JobSucceeded), Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Sender: "w"})},
		{ID: "killed-child", Agent: "w", Type: "ask", State: string(workflow.JobQueued), Payload: killedChildPayload},
		{ID: "killed-root/continuation", Agent: "w", Type: "ask", State: string(workflow.JobQueued), Payload: killedContinuationPayload},
		{ID: "live-child", Agent: "w", Type: "ask", State: string(workflow.JobQueued), Payload: liveChildPayload},
	} {
		if err := store.CreateJobWithEvent(ctx, j, db.JobEvent{Kind: j.State, Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent(%s) returned error: %v", j.ID, err)
		}
	}

	// Operator kills only the first tree.
	if err := store.SetRootJobKilled(ctx, "killed-root"); err != nil {
		t.Fatalf("SetRootJobKilled returned error: %v", err)
	}

	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 4, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}

	for _, id := range adapter.delivered {
		if id == "killed-child" {
			t.Fatalf("a queued child of a killed root must not be delivered; delivered=%v", adapter.delivered)
		}
	}
	liveDelivered := false
	for _, id := range adapter.delivered {
		if id == "live-child" {
			liveDelivered = true
		}
	}
	if !liveDelivered {
		t.Fatalf("a queued child of an un-killed root must still run; delivered=%v", adapter.delivered)
	}
	continuationDelivered := false
	for _, id := range adapter.delivered {
		if id == "killed-root/continuation" {
			continuationDelivered = true
		}
	}
	if !continuationDelivered {
		t.Fatalf("the continuation of a killed root must run so the graceful finalize executes; delivered=%v", adapter.delivered)
	}

	killedChild, err := store.GetJob(ctx, "killed-child")
	if err != nil {
		t.Fatalf("GetJob(killed-child) returned error: %v", err)
	}
	if killedChild.State != string(workflow.JobQueued) {
		t.Fatalf("killed-child state = %q, want still queued", killedChild.State)
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

func TestEffectiveJobTimeout(t *testing.T) {
	managed := managedJobRuntimeConfig{OK: true, JobTimeout: 10 * time.Minute}

	// A valid per-delegation timeout overrides the agent-type timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "30s"}, managed); got != 30*time.Second {
		t.Fatalf("effectiveJobTimeout(payload override) = %v, want 30s", got)
	}

	// An empty payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{}, managed); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(empty payload) = %v, want 10m", got)
	}

	// An unparseable payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "banana"}, managed); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(invalid payload) = %v, want 10m", got)
	}

	// A non-positive payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "0s"}, managed); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(zero payload) = %v, want 10m", got)
	}

	// With no managed config, an empty payload timeout yields zero (no deadline).
	if got := effectiveJobTimeout(workflow.JobPayload{}, managedJobRuntimeConfig{}); got != 0 {
		t.Fatalf("effectiveJobTimeout(unmanaged, empty) = %v, want 0", got)
	}

	// With no managed config, a valid payload timeout still applies.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "45s"}, managedJobRuntimeConfig{}); got != 45*time.Second {
		t.Fatalf("effectiveJobTimeout(unmanaged, payload) = %v, want 45s", got)
	}
}

// seedDelegationCoordinator inserts a completed coordinator parent job whose
// result carries the given delegations and advances it so the engine dispatches
// the top-level delegation children. It returns the worker wired to the same
// store. The coordinator runs an "ask" job; delegations use "review" so dispatch
// stays on the shared-checkout path (no per-delegation worktree allocation).
func seedDelegationCoordinator(t *testing.T, store *db.Store, parentID string, delegations []workflow.Delegation) jobWorker {
	t.Helper()
	ctx := context.Background()
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "coord", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	seenAgents := map[string]bool{"coord": true}
	for _, d := range delegations {
		if seenAgents[d.Agent] {
			continue
		}
		seenAgents[d.Agent] = true
		seedDaemonWorkerAgent(t, store, d.Agent, runtime.ShellRuntime, "unused", []string{d.Action}, "owner/repo")
	}

	if err := store.CreateJob(ctx, db.Job{ID: parentID, Agent: "coord", Type: "ask", State: string(workflow.JobRunning)}); err != nil {
		t.Fatalf("CreateJob(parent) returned error: %v", err)
	}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		TaskID:    "task-1",
		TaskTitle: "Coordinator",
		Sender:    "coord",
		Result: &workflow.AgentResult{
			Decision:    "approved",
			Summary:     "delegated",
			Delegations: delegations,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal parent payload returned error: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, parentID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload(parent) returned error: %v", err)
	}
	if err := store.UpdateJobState(ctx, parentID, string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("UpdateJobState(parent) returned error: %v", err)
	}

	worker := defaultJobWorker(store, io.Discard)
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, "")
	}
	engine := worker.WorkflowFactory("")
	if err := engine.AdvanceJob(ctx, parentID); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	return worker
}

// markDelegationChildTimedOut simulates a per-delegation timeout kill: the child
// is left JobRunning with no stored result, exactly as Mailbox.Run leaves it when
// the run context deadline fires mid-delivery (the cancelled context aborts its
// own fail write).
func markDelegationChildTimedOut(t *testing.T, store *db.Store, childID string) {
	t.Helper()
	if err := store.UpdateJobState(context.Background(), childID, string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState(%s, running) returned error: %v", childID, err)
	}
}

func TestHandleRunJobErrorTimedOutDelegationChildTriggersRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := seedDelegationCoordinator(t, store, "parent-job", []workflow.Delegation{
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api", Retry: 1},
	})

	childID := "parent-job/delegation/api"
	markDelegationChildTimedOut(t, store, childID)
	child, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(child) returned error: %v", err)
	}

	// The daemon's run-error path must turn the timeout kill into a terminal
	// failed child AND drive the parent DAG so the delegation's retry fires.
	if err := worker.handleRunJobError(ctx, childID, context.DeadlineExceeded); err != nil {
		t.Fatalf("handleRunJobError returned error: %v", err)
	}

	// The timed-out child is now terminal failed (not stranded in running).
	finalized, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(child after) returned error: %v", err)
	}
	if finalized.State != string(workflow.JobFailed) {
		t.Fatalf("timed-out child state = %q, want failed", finalized.State)
	}

	// Retry budget (Retry:1) is consumed by the timeout: a retry job is enqueued.
	retry, err := store.GetJob(ctx, "parent-job/delegation/api/retry/1")
	if err != nil {
		t.Fatalf("retry job not enqueued after timeout: %v", err)
	}
	if retry.State != string(workflow.JobQueued) || retry.DelegationID != "api" {
		t.Fatalf("retry job = %+v, want queued review of delegation api", retry)
	}
	events, err := store.ListJobEvents(ctx, "parent-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "delegation_retry") {
		t.Fatalf("parent events = %+v, want delegation_retry", events)
	}
	_ = child
}

func TestHandleRunJobErrorTimedOutDelegationChildBlocksParentWhenNoRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := seedDelegationCoordinator(t, store, "parent-job", []workflow.Delegation{
		// Retry:0 + default block_parent failure_policy: a timeout must block the
		// shared parent task, not strand the child.
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
	})

	childID := "parent-job/delegation/api"
	markDelegationChildTimedOut(t, store, childID)

	// handleRunJobError swallows the BlockedError (the child is finalized and the
	// DAG advanced), returning nil for a clean terminal outcome.
	if err := worker.handleRunJobError(ctx, childID, context.DeadlineExceeded); err != nil {
		t.Fatalf("handleRunJobError returned error: %v", err)
	}

	finalized, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(child) returned error: %v", err)
	}
	if finalized.State != string(workflow.JobFailed) {
		t.Fatalf("timed-out child state = %q, want failed", finalized.State)
	}
	// No retry: the failure_policy (block_parent) fired on the shared task.
	if _, err := store.GetJob(ctx, "parent-job/delegation/api/retry/1"); err == nil {
		t.Fatal("no retry job should be enqueued when Retry=0")
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked by block_parent failure_policy", task.State)
	}
}

func TestHandleRunJobErrorTimedOutDelegationChildEnqueuesContinuationOnContinuePolicy(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := seedDelegationCoordinator(t, store, "parent-job", []workflow.Delegation{
		// continue failure_policy: a timed-out child must resolve the delegation so
		// the coordinator continuation is enqueued once every sibling is terminal.
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "continue"},
	})

	childID := "parent-job/delegation/api"
	markDelegationChildTimedOut(t, store, childID)

	if err := worker.handleRunJobError(ctx, childID, context.DeadlineExceeded); err != nil {
		t.Fatalf("handleRunJobError returned error: %v", err)
	}

	// The coordinator continuation job is enqueued (the timeout failure resolved
	// the only delegation under the continue policy).
	if _, err := store.GetJob(ctx, "parent-job/continuation"); err != nil {
		t.Fatalf("continuation job not enqueued after timed-out child under continue policy: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "parent-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "delegation_continuation_enqueued") {
		t.Fatalf("parent events = %+v, want delegation_continuation_enqueued", events)
	}
}

func TestTaskWorktreeCheckoutPrefersDelegationPayloadWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := defaultJobWorker(store, io.Discard)

	// The task table records a DIFFERENT worktree; a delegated implement child must
	// run in its own payload.WorktreePath, never the shared task checkout.
	taskCheckout := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", Branch: "task-1", WorktreePath: taskCheckout}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	delegationCheckout := t.TempDir()
	payload := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "gitmoot-delegation-api",
		TaskID:       "task-1",
		DelegationID: "api",
		ParentJobID:  "parent-job",
		WorktreePath: delegationCheckout,
	}

	checkout, ok, err := worker.taskWorktreeCheckout(ctx, payload)
	if err != nil {
		t.Fatalf("taskWorktreeCheckout returned error: %v", err)
	}
	wantCheckout, err := normalizeTaskWorktreePath(delegationCheckout)
	if err != nil {
		t.Fatalf("normalizeTaskWorktreePath returned error: %v", err)
	}
	if !ok || checkout != wantCheckout {
		t.Fatalf("taskWorktreeCheckout = (%q,%v), want (%q,true) from payload worktree, not task table", checkout, ok, wantCheckout)
	}
	if checkout == taskCheckout {
		t.Fatal("checkout resolved to the shared task worktree instead of the delegation worktree")
	}

	// queuedJobTaskWorktreePath keys the scheduler off the same delegation path so
	// siblings sharing a task id get distinct checkout keys and run in parallel.
	path, keyed := queuedJobTaskWorktreePath(ctx, store, payload)
	if !keyed || path != wantCheckout {
		t.Fatalf("queuedJobTaskWorktreePath = (%q,%v), want (%q,true) from payload worktree", path, keyed, wantCheckout)
	}
}

func TestRunQueuedJobsFansOutDelegationsAndEnqueuesContinuation(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "coord", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "api", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "ui", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:     "coordinator",
		Agent:  "coord",
		Action: "ask",
		Repo:   "owner/repo",
		Branch: "task-1",
		TaskID: "task-1",
		Sender: "coord",
	})

	// Acceptance criterion #1: a coordinator returning delegations fans out one
	// child per delegation. Each subsequent reviewer approves, and the last sibling
	// to finish triggers the auto-created continuation (acceptance criterion #4).
	outputs := map[string]string{
		"coordinator": `{"gitmoot_result":{"decision":"approved","summary":"split the work","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"api","agent":"api","action":"review","prompt":"review the api"},{"id":"ui","agent":"ui","action":"review","prompt":"review the ui"}]}}`,
	}
	defaultOutput := `{"gitmoot_result":{"decision":"approved","summary":"looks good","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	adapter := &cliWorkerDelegationAdapter{outputs: outputs, defaultOutput: defaultOutput}

	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, "")
	}

	// Drain the DAG: coordinator -> two reviewer children -> continuation.
	for range 5 {
		if err := runQueuedJobs(ctx, worker, 2); err != nil {
			t.Fatalf("runQueuedJobs returned error: %v", err)
		}
	}

	// Both delegation children were fanned out and succeeded.
	for _, id := range []string{"coordinator/delegation/api", "coordinator/delegation/ui"} {
		child, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("delegation child %s not created: %v", id, err)
		}
		if child.State != string(workflow.JobSucceeded) {
			t.Fatalf("child %s state = %q, want succeeded", id, child.State)
		}
		if child.ParentJobID != "coordinator" {
			t.Fatalf("child %s parent = %q, want coordinator", id, child.ParentJobID)
		}
	}

	// The continuation job was auto-created after the children finished.
	cont, err := store.GetJob(ctx, "coordinator/continuation")
	if err != nil {
		t.Fatalf("continuation job not auto-created: %v", err)
	}
	if cont.ParentJobID != "coordinator" || strings.TrimSpace(cont.DelegationID) != "" {
		t.Fatalf("continuation job = %+v, want parent=coordinator and empty delegation id", cont)
	}
	events, err := store.ListJobEvents(ctx, "coordinator")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "delegation_continuation_enqueued") {
		t.Fatalf("coordinator events = %+v, want delegation_continuation_enqueued", events)
	}
}

// cliWorkerDelegationAdapter returns a per-job-id canned gitmoot_result, so a
// fan-out test can give the coordinator a delegating result and each child a
// plain approval.
type cliWorkerDelegationAdapter struct {
	mu            sync.Mutex
	outputs       map[string]string
	defaultOutput string
	delivered     []string
}

func (a *cliWorkerDelegationAdapter) Name() string { return "fake-delegation" }

func (a *cliWorkerDelegationAdapter) Validate(context.Context, runtime.Agent) error { return nil }

func (a *cliWorkerDelegationAdapter) Deliver(_ context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.delivered = append(a.delivered, job.ID)
	if out, ok := a.outputs[job.ID]; ok {
		return runtime.Result{Raw: out}, nil
	}
	return runtime.Result{Raw: a.defaultOutput}, nil
}

func (a *cliWorkerDelegationAdapter) Health(context.Context, runtime.Agent) error { return nil }

func (a *cliWorkerDelegationAdapter) Capabilities(context.Context) ([]string, error) {
	return nil, nil
}

// TestResolveDaemonStartRepoUsesRegisteredCheckout is the #202 regression: when
// the target repo is already registered with a checkout, `daemon start --repo
// owner/repo` resolves against the REGISTERED checkout regardless of the current
// working directory, instead of failing because cwd's origin does not match.
func TestResolveDaemonStartRepoUsesRegisteredCheckout(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	// The registered checkout for owner/repo.
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// A non-matching working directory: a different git repo whose origin points
	// at some other repo. Resolving from here previously errored with
	// "current checkout origin is owner/other, not owner/repo".
	otherDir := t.TempDir()
	runGit(t, otherDir, "init")
	runGit(t, otherDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, otherDir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(otherDir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, otherDir, "add", "f.txt")
	runGit(t, otherDir, "commit", "-m", "init")
	runGit(t, otherDir, "remote", "add", "origin", "https://github.com/owner/other.git")

	repo, err := daemon.ParseRepository("owner/repo")
	if err != nil {
		t.Fatalf("ParseRepository returned error: %v", err)
	}
	record, err := resolveDaemonStartRepo(ctx, store, repo, otherDir)
	if err != nil {
		t.Fatalf("resolveDaemonStartRepo from non-matching cwd returned error: %v", err)
	}
	wantRoot := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "--show-toplevel"))
	if record.CheckoutPath != wantRoot {
		t.Fatalf("resolved checkout = %q, want registered checkout %q", record.CheckoutPath, wantRoot)
	}
	if record.Owner != "owner" || record.Name != "repo" {
		t.Fatalf("resolved repo = %s/%s, want owner/repo", record.Owner, record.Name)
	}
}

// TestResolveDaemonStartRepoBootstrapsUnregistered pins that an unregistered repo
// still resolves from the current checkout (first-time setup path), and that an
// origin mismatch there still fails (origin protection intact).
func TestResolveDaemonStartRepoBootstrapsUnregistered(t *testing.T) {
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
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")

	repo, err := daemon.ParseRepository("owner/repo")
	if err != nil {
		t.Fatalf("ParseRepository returned error: %v", err)
	}
	// Bootstraps from the matching cwd.
	record, err := resolveDaemonStartRepo(ctx, store, repo, checkout)
	if err != nil {
		t.Fatalf("resolveDaemonStartRepo bootstrap returned error: %v", err)
	}
	if record.Owner != "owner" || record.Name != "repo" {
		t.Fatalf("bootstrapped repo = %s/%s, want owner/repo", record.Owner, record.Name)
	}
	// Origin protection: a non-matching cwd for an unregistered repo still fails.
	other, err := daemon.ParseRepository("owner/elsewhere")
	if err != nil {
		t.Fatalf("ParseRepository returned error: %v", err)
	}
	if _, err := resolveDaemonStartRepo(ctx, store, other, checkout); err == nil {
		t.Fatal("expected origin mismatch error for unregistered repo from non-matching cwd")
	}
}

// TestBuildEscalationCommentIsNotParsedAsCommand pins the #340 regression where
// the escalation notification, posted by the daemon on its own PR, led a line
// with "@<handle> Gitmoot paused…" and so was parsed by ParseCommand as a
// "@<agent> <action=Gitmoot>" command — making the daemon reply with a spurious
// "unsupported command action 'Gitmoot'" ack. The body must @-mention the human
// (so GitHub still notifies) without any line that ParseCommand treats as a
// command.
func TestBuildEscalationCommentIsNotParsedAsCommand(t *testing.T) {
	req := workflow.EscalationRequest{
		CoordinatorJobID: "coord-123",
		DelegationID:     "failing-leg",
		Reason:           "child returned failed",
		Question:         "how should we proceed?",
	}
	body := buildEscalationComment("jerryfane", req)

	// The human is still mentioned (GitHub notification preserved)...
	if !strings.Contains(body, "@jerryfane") {
		t.Fatalf("escalation comment dropped the @-mention; body:\n%s", body)
	}
	// ...but never as the first token of a line (which ParseCommand would treat
	// as a `@<agent> <action>` command), and no line starts with a bare /gitmoot.
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "@") {
			t.Fatalf("escalation comment line starts with an @-mention (parses as a command): %q", line)
		}
		if fields[0] == "/gitmoot" {
			t.Fatalf("escalation comment line starts with a bare /gitmoot (parses as a command): %q", line)
		}
	}

	// And end-to-end: the daemon's own parser yields no command at all for the
	// notification body, so the daemon never acks it as an (un)routable command.
	if cmds := daemon.ParseCommands(body); len(cmds) != 0 {
		t.Fatalf("escalation comment parsed into %d command(s); want 0: %+v\nbody:\n%s", len(cmds), cmds, body)
	}
}
