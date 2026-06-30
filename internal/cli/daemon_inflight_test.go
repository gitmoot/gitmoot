package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
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
