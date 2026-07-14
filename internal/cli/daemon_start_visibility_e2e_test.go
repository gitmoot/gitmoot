package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
)

// These tests bind the #597 review findings: the #556 flock refusal must be
// VISIBLE through the composed `daemon start`/`daemon restart`/`daemon stop`
// surfaces, not only on the raw `daemon run` child's stderr (which start
// redirects to daemon.log). Before the fixes, a start against an untracked
// flock-holding daemon printed "daemon started pid N" and exited 0 while the
// child died instantly, writeDaemonState registered the corpse, and stop/status
// then reported "not running" — a stable silent lie in unattended automation.

// isolateDaemonChildEnv pins the env the re-exec'd test binary children inherit:
// the TestMain re-exec home, no widening seam, a throwaway herdr socket (so a
// prod HERDR_ENV inherited from the outer environment can never leak panes),
// and GITMOOT_HOME matching the test home.
func isolateDaemonChildEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv(daemonRunChildHomeEnv, home)
	t.Setenv(daemonRunWidenGuardWindowEnv, "")
	t.Setenv("GITMOOT_HOME", home)
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "herdr.sock"))
}

// holdDaemonRunLock takes <home>/daemon.lock in-process (flock conflicts across
// open file descriptions even within one process) and releases it on cleanup,
// standing in for the live-but-untracked daemon of the #597 scenario.
func holdDaemonRunLock(t *testing.T, home string) {
	t.Helper()
	var stderr bytes.Buffer
	release, code := acquireDaemonRunLock(home, &stderr)
	if code != 0 {
		t.Fatalf("acquireDaemonRunLock code = %d, stderr=%q", code, stderr.String())
	}
	t.Cleanup(release)
}

// TestDaemonStartRefusesWhenDaemonRunLockHeld is the #597 pre-spawn probe
// regression: with <home>/daemon.lock flock-held by an UNTRACKED daemon (no
// pidfile — exactly what the start path's pidfile check cannot see), `daemon
// start` must refuse up front with the "daemon already running" UX naming the
// holder, WITHOUT spawning a child and WITHOUT registering any pid.
//
// Mutation: removing the refuseWhenDaemonRunLockHeld call in
// runDaemonStartWithWorkDirRestart makes this spawn (spawned=true) and print
// "daemon started" — red.
func TestDaemonStartRefusesWhenDaemonRunLockHeld(t *testing.T) {
	home := t.TempDir()
	holdDaemonRunLock(t, home)

	spawned := false
	prev := startDaemonChildFn
	startDaemonChildFn = func(h, poll string, workers int, wsor, wi bool, scheduler, repo, session string, state daemonState, workDir string) (daemonMeta, error) {
		spawned = true
		return daemonMeta{PID: 424242, LogFile: filepath.Join(h, "daemon.log")}, nil
	}
	t.Cleanup(func() { startDaemonChildFn = prev })

	var stdout, stderr bytes.Buffer
	code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("daemon start code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if spawned {
		t.Fatalf("daemon start spawned a child despite the held daemon.lock")
	}
	if !strings.Contains(stderr.String(), "daemon already running") || !strings.Contains(stderr.String(), "flock-held") {
		t.Fatalf("stderr = %q, want the 'daemon already running ... flock-held' refusal", stderr.String())
	}
	// The refusal must name the holder (this process wrote its pid on acquire).
	if !strings.Contains(stderr.String(), fmt.Sprintf("with pid %d", os.Getpid())) {
		t.Fatalf("stderr = %q, want the holder pid %d", stderr.String(), os.Getpid())
	}
	if strings.Contains(stdout.String(), "daemon started") {
		t.Fatalf("stdout = %q claims success despite the refusal", stdout.String())
	}
	state := daemonProcessState(config.PathsForHome(home))
	if _, err := os.Stat(state.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("daemon.pid exists after a refused start (stat err=%v)", err)
	}
}

// TestStartDaemonChildSurfacesInstantFlockLoser drives the REAL startDaemonChild
// (the same function `daemon start`/`daemon restart` use behind the seam)
// against a home whose daemon.lock is already held: the spawned child is a real
// `daemon run` (via the TestMain re-exec) that loses the flock and exits 1
// within milliseconds. startDaemonChild must report that death as an error —
// carrying the child's refusal from the daemon.log tail — instead of returning
// a corpse pid for the caller to register and celebrate.
//
// Mutation: removing the confirmDaemonRunChildSurvived call in startDaemonChild
// makes this return nil error with the dead child's pid — red.
func TestStartDaemonChildSurfacesInstantFlockLoser(t *testing.T) {
	home := t.TempDir()
	isolateDaemonChildEnv(t, home)
	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatalf("initializedPaths: %v", err)
	}
	state := daemonProcessState(paths)
	holdDaemonRunLock(t, home)

	meta, err := startDaemonChild(home, "30s", 1, false, false, "", "", "", state, t.TempDir())
	if err == nil {
		_ = syscall.Kill(meta.PID, syscall.SIGKILL)
		reapChildProcess(t, meta.PID)
		t.Fatalf("startDaemonChild succeeded (pid %d) despite the held daemon.lock", meta.PID)
	}
	if !strings.Contains(err.Error(), "exited immediately") {
		t.Fatalf("err = %q, want the instant-death diagnosis", err)
	}
	if !strings.Contains(err.Error(), "daemon already running") {
		t.Fatalf("err = %q, want the child's 'daemon already running' refusal surfaced from the log tail", err)
	}
	if _, statErr := os.Stat(state.PIDFile); !os.IsNotExist(statErr) {
		t.Fatalf("daemon.pid exists after a failed start (stat err=%v)", statErr)
	}
}

// TestStartDaemonChildConfirmsHealthySurvivor is the no-regression half of the
// #597 survival check: on a free home the REAL startDaemonChild must still
// succeed, returning only after the child proved it survived its startup guards
// by self-registering its pid — not merely "hasn't died yet".
func TestStartDaemonChildConfirmsHealthySurvivor(t *testing.T) {
	home := t.TempDir()
	isolateDaemonChildEnv(t, home)
	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatalf("initializedPaths: %v", err)
	}
	state := daemonProcessState(paths)

	meta, err := startDaemonChild(home, "30s", 1, false, false, "", "", "", state, t.TempDir())
	if err != nil {
		t.Fatalf("startDaemonChild: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(meta.PID, syscall.SIGKILL)
		reapChildProcess(t, meta.PID)
	})
	// The survival confirmation normally returns only after self-registration
	// (its early-out); the poll below covers the deadline fallback path so a
	// slow-but-healthy child never flakes this test.
	waitForDaemonRegistration(t, home, meta.PID, 30*time.Second)
}

// reapChildProcess waits (blocking, EINTR-tolerant) for a direct child spawned
// by the REAL startDaemonChild, so no zombie outlives the test.
func reapChildProcess(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var status syscall.WaitStatus
		reaped, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || reaped == pid {
			return // reaped, or already gone (ECHILD)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d did not exit for reaping", pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDaemonStopStopsUntrackedFlockHolder: with a LIVE `daemon run` holding the
// flock but its registration lost (the #597 aftermath state), `daemon stop`
// must find the holder via daemon.lock, verify it, and actually stop it —
// instead of reporting "daemon not running" and leaving the daemon immortal to
// the CLI (which also made `daemon restart` a silent no-op).
func TestDaemonStopStopsUntrackedFlockHolder(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	home := t.TempDir()
	// Cosmetic "daemon run" argv so the kill-guard can verify the holder by its
	// cmdline, as it would a production `gitmoot daemon run`.
	p := startDaemonRunProc(t, exe, home, "daemon", "run", "--home", home)
	waitForDaemonRegistration(t, home, p.cmd.Process.Pid, 30*time.Second)

	// Lose the registration out from under the live daemon (what the clobber
	// race + stale-pid cleanup produce organically).
	state := daemonProcessState(config.PathsForHome(home))
	if err := os.Remove(state.PIDFile); err != nil {
		t.Fatalf("remove pidfile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"daemon", "stop", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("daemon stop code = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), fmt.Sprintf("daemon stopped pid %d", p.cmd.Process.Pid)) || !strings.Contains(stdout.String(), "untracked") {
		t.Fatalf("stdout = %q, want 'daemon stopped pid %d (untracked ...)'", stdout.String(), p.cmd.Process.Pid)
	}
	select {
	case <-p.done:
		p.reaped = true
	case <-time.After(30 * time.Second):
		t.Fatalf("untracked daemon pid %d still alive after daemon stop", p.cmd.Process.Pid)
	}
	// The flock must now be free: a fresh acquire succeeds immediately.
	release, lockCode := acquireDaemonRunLock(home, io.Discard)
	if lockCode != 0 {
		t.Fatalf("daemon.lock still held after stopping the untracked holder")
	}
	release()
}

// TestDaemonStopRefusesUnverifiedUntrackedHolder: when the flock is held but
// the recorded holder cannot be positively verified as a gitmoot `daemon run`
// (here: the lock is held by the TEST process, whose cmdline has no `daemon
// run`), `daemon stop` must NOT signal the pid — but it must also NOT lie
// "daemon not running": it reports the untracked holder and exits non-zero.
func TestDaemonStopRefusesUnverifiedUntrackedHolder(t *testing.T) {
	home := t.TempDir()
	holdDaemonRunLock(t, home)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"daemon", "stop", "--home", home}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("daemon stop code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "daemon not running") {
		t.Fatalf("stdout = %q lies 'daemon not running' while the flock is held", stdout.String())
	}
	if !strings.Contains(stderr.String(), "UNTRACKED") {
		t.Fatalf("stderr = %q, want the untracked-holder report", stderr.String())
	}
}
