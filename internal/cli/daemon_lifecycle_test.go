package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/presence"
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
	code = Run([]string{"daemon", "start", "--repo", "gitmoot/gitmoot", "--poll", "0s"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("daemon start invalid poll exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "poll interval must be positive") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"daemon", "run", "--repo", "gitmoot/gitmoot", "--dry-run"}, &stdout, &stderr)
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

func TestCurrentDaemonPIDOnlyDeletesConfidentlyStalePID(t *testing.T) {
	state := daemonState{PIDFile: t.TempDir() + "/daemon.pid", MetaFile: t.TempDir() + "/daemon.json"}
	if err := os.WriteFile(state.PIDFile, []byte("42\n"), 0o600); err != nil {
		t.Fatalf("write stale pid: %v", err)
	}

	pid, stale, err := currentDaemonPIDWithProbe(state, func(int, string) (string, error) {
		return presence.DaemonStopped, nil
	})
	if err != nil || pid != 0 || !stale {
		t.Fatalf("stale pid result = pid=%d stale=%v err=%v", pid, stale, err)
	}
	if _, err := os.Stat(state.PIDFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confidently stale pidfile still exists: %v", err)
	}

	if err := os.WriteFile(state.PIDFile, []byte("42\n"), 0o600); err != nil {
		t.Fatalf("rewrite pid: %v", err)
	}
	probeErr := errors.New("kill permission denied")
	pid, stale, err = currentDaemonPIDWithProbe(state, func(int, string) (string, error) {
		return presence.DaemonUnknown, probeErr
	})
	if !errors.Is(err, probeErr) || pid != 0 || stale {
		t.Fatalf("unknown pid result = pid=%d stale=%v err=%v", pid, stale, err)
	}
	if _, err := os.Stat(state.PIDFile); err != nil {
		t.Fatalf("unknown probe removed pidfile: %v", err)
	}
}

func TestDaemonStartRepoPreflightsCheckoutBeforeDaemonizing(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"daemon", "start", "--home", home, "--repo", "gitmoot/gitmoot", "--poll", "1h"}, &stdout, &stderr)

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
	code := Run([]string{"daemon", "restart", "--home", home, "--repo", "gitmoot/gitmoot", "--poll", "1h"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("daemon restart exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("pid file was touched before preflight completed: %v", err)
	}
}

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

func TestAutoSelectScheduler(t *testing.T) {
	cases := []struct {
		name              string
		scheduler         string
		workers           int
		explicitScheduler bool
		want              string
	}{
		{"single worker stays barrier", "barrier", 1, false, "barrier"},
		{"multi worker auto-pools", "barrier", 4, false, "pool"},
		{"explicit barrier is honored", "barrier", 4, true, "barrier"},
		{"explicit pool is honored", "pool", 1, true, "pool"},
		{"multi worker already pool", "pool", 4, false, "pool"},
		{"default empty single", "", 1, false, ""},
		{"default empty multi auto-pools", "", 3, false, "pool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoSelectScheduler(tc.scheduler, tc.workers, tc.explicitScheduler); got != tc.want {
				t.Fatalf("autoSelectScheduler(%q, %d, %t) = %q, want %q", tc.scheduler, tc.workers, tc.explicitScheduler, got, tc.want)
			}
		})
	}
}

func TestParseDaemonStartConfigAutoPoolsWithWorkers(t *testing.T) {
	parsed, code := parseDaemonStartConfig("daemon start", []string{"--workers", "4"}, io.Discard)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig code = %d", code)
	}
	if parsed.Scheduler != "pool" {
		t.Fatalf("scheduler = %q, want pool (auto-selected for --workers 4)", parsed.Scheduler)
	}
	if parsed.ExplicitScheduler {
		t.Fatalf("ExplicitScheduler = true, want false (auto-selection is not explicit)")
	}
	// The resolved child args must carry --scheduler pool so the daemon child runs it.
	childArgs := daemonChildArgs("", "30s", parsed.Workers, false, false, parsed.Scheduler, "", "")
	if !daemonArgsContainFlag(childArgs, "scheduler", "pool") {
		t.Fatalf("child argv missing --scheduler pool: %v", childArgs)
	}
}

func TestParseDaemonStartConfigExplicitBarrierWithWorkersIsHonored(t *testing.T) {
	parsed, code := parseDaemonStartConfig("daemon start", []string{"--workers", "4", "--scheduler", "barrier"}, io.Discard)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig code = %d", code)
	}
	if parsed.Scheduler != "barrier" {
		t.Fatalf("scheduler = %q, want barrier (explicit opt-out preserved)", parsed.Scheduler)
	}
	if !parsed.ExplicitScheduler {
		t.Fatalf("ExplicitScheduler = false, want true")
	}
}

func TestParseDaemonStartConfigParallelFlag(t *testing.T) {
	parsed, code := parseDaemonStartConfig("daemon start", []string{"--parallel", "5"}, io.Discard)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig code = %d", code)
	}
	if parsed.Workers != 5 {
		t.Fatalf("workers = %d, want 5", parsed.Workers)
	}
	if parsed.Scheduler != "pool" {
		t.Fatalf("scheduler = %q, want pool", parsed.Scheduler)
	}
	if !parsed.ExplicitWorkers || !parsed.ExplicitScheduler {
		t.Fatalf("--parallel must mark workers+scheduler explicit so they persist through restart (got workers=%t scheduler=%t)", parsed.ExplicitWorkers, parsed.ExplicitScheduler)
	}
}

func TestParseDaemonStartConfigParallelConflictsWithWorkers(t *testing.T) {
	var stderr bytes.Buffer
	_, code := parseDaemonStartConfig("daemon start", []string{"--parallel", "5", "--workers", "2"}, &stderr)
	if code == 0 {
		t.Fatalf("parseDaemonStartConfig accepted --parallel with --workers; want rejection")
	}
	if !strings.Contains(stderr.String(), "--parallel cannot be combined") {
		t.Fatalf("stderr = %q, want conflict message", stderr.String())
	}
}

func TestParseDaemonStartConfigParallelRejectsNonPositive(t *testing.T) {
	var stderr bytes.Buffer
	_, code := parseDaemonStartConfig("daemon start", []string{"--parallel", "0"}, &stderr)
	if code == 0 {
		t.Fatalf("parseDaemonStartConfig accepted --parallel 0; want rejection")
	}
}

func TestDaemonSchedulerStatusLine(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"defaults", []string{"daemon", "run", "--poll", "30s", "--workers", "1"}, "scheduler: barrier, workers: 1"},
		{"pool multi", []string{"daemon", "run", "--workers", "4", "--scheduler", "pool"}, "scheduler: pool, workers: 4"},
		{"equals form", []string{"daemon", "run", "--workers=3", "--scheduler=pool"}, "scheduler: pool, workers: 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonSchedulerStatusLine(tc.args); got != tc.want {
				t.Fatalf("daemonSchedulerStatusLine(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestDaemonSchedulerStatusLineWarnsBarrierWithWorkers(t *testing.T) {
	got := daemonSchedulerStatusLine([]string{"daemon", "run", "--workers", "4"})
	if !strings.HasPrefix(got, "scheduler: barrier, workers: 4") {
		t.Fatalf("line = %q, want barrier/4 prefix", got)
	}
	if !strings.Contains(got, "--scheduler pool") {
		t.Fatalf("line = %q, want a relaunch hint pointing at --scheduler pool", got)
	}
}

func TestDaemonAutoPoolSurvivesChildArgsAndRestart(t *testing.T) {
	// daemon start --workers 4 auto-selects pool; the persisted child argv must
	// carry --scheduler pool, and a restart that re-parses those args must keep it.
	cfg, code := parseDaemonStartConfig("daemon start", []string{"--workers", "4"}, io.Discard)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig code = %d", code)
	}
	childArgs := daemonChildArgs(cfg.Home, cfg.Poll.String(), cfg.Workers, cfg.WatchSkillOptReviews, cfg.WatchIssues, cfg.Scheduler, cfg.RepoFlag, cfg.Session)
	if !daemonArgsContainFlag(childArgs, "scheduler", "pool") {
		t.Fatalf("persisted child argv lost auto-selected pool: %v", childArgs)
	}
	meta := daemonMeta{Args: childArgs}

	restartCfg, code := parseDaemonStartConfig("daemon restart", nil, io.Discard)
	if code != 0 {
		t.Fatalf("parseDaemonStartConfig(restart) code = %d", code)
	}
	targetArgs := overlayDaemonStartArgs(daemonStartArgsFromRunArgs(meta.Args), restartCfg)
	parsed, code := parseDaemonStartConfig("daemon restart", targetArgs, io.Discard)
	if code != 0 {
		t.Fatalf("parse restored restart args code = %d, args=%v", code, targetArgs)
	}
	if parsed.Scheduler != "pool" || parsed.Workers != 4 {
		t.Fatalf("restart lost config: scheduler=%q workers=%d, want pool/4; args=%v", parsed.Scheduler, parsed.Workers, targetArgs)
	}
}
