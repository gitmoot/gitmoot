package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
)

// TestDaemonRunSelfRegistrationSurfacesRunningDaemon is the #505 gap-3 regression:
// a `daemon run` launched directly (the form a `systemd --user` unit uses as its
// ExecStart) must be recognized as running by `daemon status` / the dashboard.
//
// Before the fix only `daemon start` (the forking parent) wrote daemon.json, so a
// systemd-managed `daemon run` left no pid/meta and currentDaemonPID — and thus
// `daemon status` — falsely reported "stopped". registerDaemonRunState lets the
// daemon-run process record ITSELF; this test drives that boundary using the test
// process as the stand-in daemon (its argv matches /proc/<pid>/cmdline, so it
// passes processLooksLikeDaemon exactly as a real `gitmoot daemon run` would).
func TestDaemonRunSelfRegistrationSurfacesRunningDaemon(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	state := daemonProcessState(paths)

	// systemd scenario: a `daemon run` has NOT self-registered yet — no daemon.json
	// exists, so the daemon is invisible (the reported bug).
	if pid, _, err := currentDaemonPID(state); err != nil || pid != 0 {
		t.Fatalf("pre-registration currentDaemonPID = (%d, err=%v), want (0, nil)", pid, err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon status exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped") {
		t.Fatalf("pre-registration status = %q, want stopped", stdout.String())
	}

	// The fix: the daemon-run process self-registers with its own argv.
	wd, _ := os.Getwd()
	ok, err := registerDaemonRunState(state, os.Args, wd)
	if err != nil || !ok {
		t.Fatalf("registerDaemonRunState ok=%v err=%v, want true nil", ok, err)
	}

	if pid, stale, err := currentDaemonPID(state); err != nil || pid != os.Getpid() || stale {
		t.Fatalf("post-registration currentDaemonPID = (%d, stale=%v, err=%v), want (%d, false, nil)", pid, stale, err, os.Getpid())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon status (registered) exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "running pid") {
		t.Fatalf("post-registration status = %q, want running pid", stdout.String())
	}

	// Shutdown cleanup removes our own state but is restricted to our pid.
	deregisterDaemonRunState(state)
	if pid, _, err := currentDaemonPID(state); err != nil || pid != 0 {
		t.Fatalf("post-deregister currentDaemonPID = (%d, err=%v), want (0, nil)", pid, err)
	}
}

// TestDaemonRunStartupReconcile binds the #505 regression to the INTEGRATION
// point, not just the leaf helpers: the single startup seam runDaemonRun defers
// (daemonRunStartupReconcile) must self-register this process so `daemon
// status` flips from "stopped" to "running pid" (gap 3). Exercising the seam
// (rather than registerDaemonRunState in isolation) makes a dropped call
// detectable. The returned cleanup deregisters our marker.
func TestDaemonRunStartupReconcile(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	// Before startup: no daemon.json → status reports stopped (the systemd bug).
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon status exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped") {
		t.Fatalf("pre-startup status = %q, want stopped", stdout.String())
	}

	// The integration seam (test process stands in for the daemon-run process;
	// its argv passes processLooksLikeDaemon).
	var startupOut bytes.Buffer
	cleanup := daemonRunStartupReconcile(context.Background(), home, os.Args, &startupOut)

	// (a) self-registered: status now reports the live daemon.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon status (registered) exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "running pid") {
		t.Fatalf("post-startup status = %q, want running pid", stdout.String())
	}
	// cleanup deregisters our marker → status back to stopped, but the meta is kept
	// so `daemon restart` can still recover the prior daemon's args/workdir.
	cleanup()
	if _, err := os.Stat(daemonProcessState(paths).MetaFile); err != nil {
		t.Fatalf("deregister must preserve daemon.json for restart recovery: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon status (deregistered) exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped") {
		t.Fatalf("post-cleanup status = %q, want stopped", stdout.String())
	}
}

// TestDeregisterDaemonRunStateOnlyRemovesOwnState confirms shutdown cleanup never
// clobbers state a restarted daemon recorded under a different pid.
func TestDeregisterDaemonRunStateOnlyRemovesOwnState(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	state := daemonProcessState(paths)

	// A restarted daemon recorded a foreign pid in the meta.
	foreign := daemonMeta{PID: os.Getpid() + 1, Args: []string{"daemon", "run"}, LogFile: state.LogFile}
	if err := writeDaemonState(state, foreign); err != nil {
		t.Fatalf("writeDaemonState returned error: %v", err)
	}
	// Our shutdown must leave the foreign daemon's state intact.
	deregisterDaemonRunState(state)
	if _, err := os.Stat(state.MetaFile); err != nil {
		t.Fatalf("deregister removed foreign daemon meta: %v", err)
	}
	if _, err := os.Stat(state.PIDFile); err != nil {
		t.Fatalf("deregister removed foreign daemon pid: %v", err)
	}
}

// TestDeregisterDaemonRunStatePreservesMetaForRestart is the #505-review
// regression: a clean shutdown must remove ONLY daemon.pid and keep daemon.json,
// mirroring `daemon stop`. currentDaemonPID already reports "stopped" once the pid
// is gone, and `daemon restart` reads daemon.json (readDaemonMeta) to recover the
// prior daemon's --repo/--watch-issues/--workers/workdir — deleting the meta on
// exit silently broke that restart-recovery path that worked on main.
func TestDeregisterDaemonRunStatePreservesMetaForRestart(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	state := daemonProcessState(paths)

	// This process self-registers, then exits cleanly.
	ok, err := registerDaemonRunState(state, []string{"gitmoot", "daemon", "run", "--repo", "owner/repo", "--watch-issues"}, home)
	if err != nil || !ok {
		t.Fatalf("registerDaemonRunState ok=%v err=%v", ok, err)
	}
	deregisterDaemonRunState(state)

	// pid removed → status sees a stopped daemon.
	if _, err := os.Stat(state.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("deregister must remove daemon.pid, stat err=%v", err)
	}
	// meta retained → restart can still recover the recorded args/workdir.
	meta, err := readDaemonMeta(state)
	if err != nil {
		t.Fatalf("deregister must preserve daemon.json: %v", err)
	}
	joined := strings.Join(meta.Args, " ")
	if !strings.Contains(joined, "--repo owner/repo") || !strings.Contains(joined, "--watch-issues") {
		t.Fatalf("preserved meta lost restart args: %q", joined)
	}
	if meta.WorkingDir != home {
		t.Fatalf("preserved meta working dir = %q, want %q", meta.WorkingDir, home)
	}
}
