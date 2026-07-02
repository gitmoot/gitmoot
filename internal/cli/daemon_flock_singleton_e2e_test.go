package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
)

// daemonRunChildHomeEnv, when set, flips the re-exec'd test binary into a REAL
// `gitmoot daemon run --home <value>` process (see TestMain). flock(2) is
// process-scoped — two goroutines cannot exercise it — so the #556 singleton
// E2E must launch genuine child processes, and re-exec'ing the already-built
// test binary is how it gets them without shelling out to `go build`.
const daemonRunChildHomeEnv = "GITMOOT_TEST_DAEMON_RUN_CHILD_HOME"

func TestMain(m *testing.M) {
	if home := os.Getenv(daemonRunChildHomeEnv); home != "" {
		os.Exit(Run([]string{"daemon", "run", "--home", home}, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// daemonRunProc is one real `daemon run` child process launched by the E2E.
// stdout/stderr buffers must only be read AFTER done has delivered (cmd.Wait
// has returned), or the exec copier goroutines race the reader.
type daemonRunProc struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan error
	reaped bool
}

// startDaemonRunProc launches the test binary as a real `daemon run` on home,
// with the #556 widening seam enabled so BOTH simultaneous children are
// guaranteed past the friendly liveness guard before either self-registers —
// making the TOCTOU window deterministic instead of microseconds wide.
//
// argv is cosmetic: TestMain re-execs on the env var alone and ignores the
// arguments, but tests of the untracked-holder kill-guard (#597 review) pass
// "daemon run ..." so the child's /proc cmdline looks like a real daemon run.
func startDaemonRunProc(t *testing.T, exe, home string, argv ...string) *daemonRunProc {
	t.Helper()
	p := &daemonRunProc{done: make(chan error, 1)}
	p.cmd = exec.Command(exe, argv...)
	env := os.Environ()
	scrubbed := env[:0]
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GITMOOT_HOME="),
			strings.HasPrefix(kv, "HERDR_ENV="),
			strings.HasPrefix(kv, "HERDR_SOCKET_PATH="),
			strings.HasPrefix(kv, daemonRunChildHomeEnv+"="),
			strings.HasPrefix(kv, daemonRunWidenGuardWindowEnv+"="):
			continue
		}
		scrubbed = append(scrubbed, kv)
	}
	p.cmd.Env = append(scrubbed,
		daemonRunChildHomeEnv+"="+home,
		daemonRunWidenGuardWindowEnv+"=500ms",
	)
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start daemon run child: %v", err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() { p.kill(t) })
	return p
}

// kill SIGKILLs the child (if still running) and reaps it. Idempotent.
func (p *daemonRunProc) kill(t *testing.T) {
	t.Helper()
	if p.reaped {
		return
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon run child pid %d did not reap after SIGKILL", p.cmd.Process.Pid)
	}
	p.reaped = true
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// waitForDaemonRegistration polls until <home>/.gitmoot/daemon.pid records
// wantPID — the winner's self-registration, proof it survived the race and
// entered its startup path rather than merely not having exited yet.
func waitForDaemonRegistration(t *testing.T, home string, wantPID int, timeout time.Duration) {
	t.Helper()
	state := daemonProcessState(config.PathsForHome(home))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(state.PIDFile); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid == wantPID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("winner pid %d never self-registered in %s", wantPID, state.PIDFile)
}

// TestDaemonRunFlockSingletonE2E is the #556 regression, mutation-proven: the
// residual #550 TOCTOU where two `daemon run` launched SIMULTANEOUSLY on a
// clean home both pass the liveness guard before either self-registers, so
// both enter the supervisor loop and race the shared SQLite store.
//
// Each round launches two REAL child processes (flock is process-scoped) on
// one shared temp home, with the widening seam holding both past the friendly
// guard so the race window is deterministic, and asserts:
//   - EXACTLY ONE child exits, code 1, printing the "daemon already running"
//     refusal (the flock loser);
//   - the other child stays alive and self-registers (the flock winner);
//   - after the winner is SIGKILLed — the harshest death, no cleanup runs —
//     the next round's children acquire immediately: the kernel released the
//     flock with the process, so a stale lockfile can never cause a lockout.
//
// Mutation: removing the acquireDaemonRunLock call in runDaemonRun makes both
// children survive every round — neither exits — and the round times out red.
func TestDaemonRunFlockSingletonE2E(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	home := t.TempDir()

	const rounds = 20
	for round := 0; round < rounds; round++ {
		a := startDaemonRunProc(t, exe, home)
		b := startDaemonRunProc(t, exe, home)

		var winner, loser *daemonRunProc
		var loserErr error
		select {
		case loserErr = <-a.done:
			loser, winner = a, b
		case loserErr = <-b.done:
			loser, winner = b, a
		case <-time.After(60 * time.Second):
			t.Fatalf("round %d: NEITHER simultaneous daemon run refused — double-run (flock backstop missing or inert)", round)
		}
		loser.reaped = true

		if code := exitCodeOf(loserErr); code != 1 {
			t.Fatalf("round %d: loser exit = %d (err=%v), want 1; stderr=%q stdout=%q",
				round, code, loserErr, loser.stderr.String(), loser.stdout.String())
		}
		if !strings.Contains(loser.stderr.String(), "daemon already running") {
			t.Fatalf("round %d: loser stderr = %q, want the 'daemon already running' refusal", round, loser.stderr.String())
		}

		// The winner must be alive AND self-registered — not merely slow to lose.
		waitForDaemonRegistration(t, home, winner.cmd.Process.Pid, 30*time.Second)
		select {
		case winnerErr := <-winner.done:
			winner.reaped = true
			t.Fatalf("round %d: BOTH runs exited — no daemon survived (winner err=%v stderr=%q)",
				round, winnerErr, winner.stderr.String())
		default:
		}

		// SIGKILL the winner; the next round (and the final check below) proves
		// the flock was released by the kernel with no stale-lock lockout.
		winner.kill(t)
	}

	// Explicit stale-lock check after the final SIGKILL: a lone fresh run must
	// acquire immediately and register — the lockfile left behind by 20 dead
	// winners never blocks a new daemon.
	fresh := startDaemonRunProc(t, exe, home)
	waitForDaemonRegistration(t, home, fresh.cmd.Process.Pid, 30*time.Second)
	select {
	case freshErr := <-fresh.done:
		fresh.reaped = true
		t.Fatalf("fresh run after SIGKILLed winner exited (err=%v stderr=%q) — stale-lock lockout", freshErr, fresh.stderr.String())
	default:
	}
	fresh.kill(t)
}
