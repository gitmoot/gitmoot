package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/subprocess"
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
