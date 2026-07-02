package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
)

// spawnFakeDaemon starts a real, long-lived child process and records it in the
// daemon meta/pid files so it passes the SAME liveness check the singleton guard
// uses (currentDaemonPID -> processLooksLikeDaemon: a live pid whose /proc
// cmdline matches the recorded executable+args). We exec `sleep` with an explicit
// argv[0] of "sleep" so /proc/<pid>/cmdline == {"sleep", "<dur>"} matches the
// meta we write. The child is killed on test cleanup.
func spawnFakeDaemon(t *testing.T, state daemonState) (pid int) {
	t.Helper()
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	cmd := &exec.Cmd{Path: sleepPath, Args: []string{"sleep", "120"}}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pid = cmd.Process.Pid
	meta := daemonMeta{
		PID:        pid,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		Args:       []string{"120"},
		LogFile:    state.LogFile,
		Executable: "sleep",
	}
	if err := writeDaemonState(state, meta); err != nil {
		t.Fatalf("writeDaemonState: %v", err)
	}
	// Sanity: the recorded process must register as a live daemon.
	if got, stale, err := currentDaemonPID(state); err != nil || got != pid || stale {
		t.Fatalf("fake daemon not seen as live: pid=%d stale=%v err=%v want pid=%d", got, stale, err, pid)
	}
	return pid
}

// TestGuardDaemonRunSingleton is the #550 regression: `daemon run` must refuse to
// start a SECOND daemon on a home that already has a LIVE registered daemon, while
// a stale pidfile from a dead daemon (and a clean home) must NOT block a fresh
// start. Before the fix `daemon run` had no singleton guard at all — it would
// enter its loop regardless, racing the existing daemon on the shared SQLite store
// and clobbering daemon.json so the prior daemon ran untracked.
func TestGuardDaemonRunSingleton(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(t *testing.T, state daemonState)
		wantCode int
		wantErr  string
	}{
		{
			name:     "clean home allows start",
			setup:    func(t *testing.T, state daemonState) {},
			wantCode: 0,
		},
		{
			name: "stale pidfile from dead daemon allows start",
			setup: func(t *testing.T, state daemonState) {
				// A pid that is guaranteed not running: spawn `true`, reap it.
				cmd := exec.Command("true")
				if err := cmd.Run(); err != nil {
					t.Fatalf("run true: %v", err)
				}
				deadPID := cmd.Process.Pid
				if err := os.WriteFile(state.PIDFile, []byte(strconv.Itoa(deadPID)+"\n"), 0o600); err != nil {
					t.Fatalf("write stale pidfile: %v", err)
				}
			},
			wantCode: 0,
		},
		{
			name: "live registered daemon refuses start",
			setup: func(t *testing.T, state daemonState) {
				spawnFakeDaemon(t, state)
			},
			wantCode: 1,
			wantErr:  "daemon already running with pid",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			state := daemonProcessState(paths)
			tt.setup(t, state)

			var stderr bytes.Buffer
			code := guardDaemonRunSingleton(home, &stderr)
			if code != tt.wantCode {
				t.Fatalf("guardDaemonRunSingleton code = %d, want %d (stderr=%q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}

			// The stale-pidfile case must also have been cleared, so a fresh start
			// is unobstructed (currentDaemonPID reports not-running).
			if tt.name == "stale pidfile from dead daemon allows start" {
				if pid, _, err := currentDaemonPID(state); err != nil || pid != 0 {
					t.Fatalf("stale pidfile not cleared: pid=%d err=%v", pid, err)
				}
			}
		})
	}
}

// TestAcquireDaemonRunLock covers the in-process seams of the #556 flock
// backstop: a second acquire on the same home must refuse with the "daemon
// already running" UX while the first holds the lock (flock conflicts across
// separate open file descriptions even within one process), and releasing must
// free it for the next acquire — including when the lockfile already exists
// (no stale-lockfile unlink dance is ever needed). The process-scoped kill -9
// semantics are covered by TestDaemonRunFlockSingletonE2E.
//
// Deliberately NOT t.Parallel(), and the post-release re-acquire retries
// briefly: flock lives on the open file description, and a concurrently
// fork+exec'ing test (several in this package launch real child processes)
// briefly shares every fd between fork and exec — during that window the lock
// survives the parent's close, so an instant re-acquire can transiently see
// EWOULDBLOCK. The kernel guarantees release once no reference remains; only
// the test's timing needs the slack, not the daemon (whose children exec with
// CLOEXEC and whose lock dies with the process).
func TestAcquireDaemonRunLock(t *testing.T) {
	home := t.TempDir()

	var stderr bytes.Buffer
	release, code := acquireDaemonRunLock(home, &stderr)
	if code != 0 {
		t.Fatalf("first acquire code = %d, stderr=%q", code, stderr.String())
	}

	var stderr2 bytes.Buffer
	release2, code2 := acquireDaemonRunLock(home, &stderr2)
	if code2 == 0 {
		release2()
		t.Fatalf("second acquire succeeded while the lock was held")
	}
	if !strings.Contains(stderr2.String(), "daemon already running") {
		t.Fatalf("second acquire stderr = %q, want 'daemon already running'", stderr2.String())
	}

	release()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var stderr3 bytes.Buffer
		release3, code3 := acquireDaemonRunLock(home, &stderr3)
		if code3 == 0 {
			release3()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("re-acquire after release code = %d, stderr=%q", code3, stderr3.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestDaemonRunCommandRefusesSecondDaemon binds the regression to the real command
// surface: with a live daemon already registered, `gitmoot daemon run` must return
// promptly with a non-zero exit and a clear message instead of entering its
// supervisor loop. Without the guard this call would block forever (the loop), so
// the test would time out — exactly the #550 behavior we are preventing.
func TestDaemonRunCommandRefusesSecondDaemon(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	state := daemonProcessState(paths)
	livePID := spawnFakeDaemon(t, state)

	type result struct {
		code   int
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"daemon", "run", "--home", home}, &stdout, &stderr)
		done <- result{code: code, stderr: stderr.String()}
	}()

	select {
	case res := <-done:
		if res.code == 0 {
			t.Fatalf("daemon run exit = 0, want non-zero refusal (stderr=%q)", res.stderr)
		}
		if !strings.Contains(res.stderr, "daemon already running with pid "+strconv.Itoa(livePID)) {
			t.Fatalf("stderr = %q, want 'daemon already running with pid %d'", res.stderr, livePID)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("daemon run did not refuse and is blocking (missing singleton guard)")
	}
}
