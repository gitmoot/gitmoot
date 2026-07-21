package cli

import (
	"context"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// reloadSignalWriter is an io.Writer that closes done on its first Write. The
// warm-reload handler (reloadDaemonConfig) writes its one-line summary to stdout
// ONLY after live.apply() has finished mutating the live struct, so a Write is an
// edge-triggered, deterministic "the reload completed" signal — no polling, no
// wall-clock sleep. It is the synchronization point for the real-SIGHUP path.
type reloadSignalWriter struct {
	once sync.Once
	done chan struct{}
}

func newReloadSignalWriter() *reloadSignalWriter {
	return &reloadSignalWriter{done: make(chan struct{})}
}

func (w *reloadSignalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.done) })
	return len(p), nil
}

// waitReloaded blocks until the reload handler has written its summary (i.e. the
// live apply completed), failing the test if it does not happen. The time.After is
// only a hung-test backstop; on the happy path the channel is already closed by the
// time the handler returns, so no wall-clock time is spent waiting.
func (w *reloadSignalWriter) waitReloaded(t *testing.T) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		t.Fatal("SIGHUP reload never applied (handler wrote no summary)")
	}
}

// setDispatchLimitObserver installs a recorder on the test-only dispatch seam and
// returns a getter for the last limit the RUNNING dispatch actually read, plus the
// full history. It cleans itself up so it never leaks into a sibling test.
func setDispatchLimitObserver(t *testing.T) (last func() int, seen func() []int) {
	t.Helper()
	var mu sync.Mutex
	var limits []int
	dispatchLimitObserver = func(limit int) {
		mu.Lock()
		limits = append(limits, limit)
		mu.Unlock()
	}
	t.Cleanup(func() { dispatchLimitObserver = nil })
	last = func() int {
		mu.Lock()
		defer mu.Unlock()
		if len(limits) == 0 {
			return -1
		}
		return limits[len(limits)-1]
	}
	seen = func() []int {
		mu.Lock()
		defer mu.Unlock()
		out := make([]int, len(limits))
		copy(out, limits)
		return out
	}
	return last, seen
}

// TestWarmReloadE2EChangesRunningDispatch is the full-chain warm-reload proof for
// #577. It goes end to end WITHOUT any teardown or env re-inheritance:
//
//  1. Launch: a live daemonReloadableConfig is seeded from an on-disk [daemon]
//     section, with poll set from an EXPLICIT launch flag (flag wins over the file
//     at start via applyStart).
//  2. Reload: the config file is EDITED (workers 1->4, scheduler barrier->pool;
//     poll left absent) and a real SIGHUP is delivered to this process through the
//     production installDaemonReloadHandler wiring. Synchronization is
//     deterministic (an edge-triggered writer signal), not a sleep.
//  3. Assert: the LIVE snapshot reflects the new workers/scheduler; the very next
//     dispatch pass (the real runDaemonWorkerTickTracked -> runQueuedJobsForRepo path)
//     reads the NEW limit; the process is not torn down; the launch env token is
//     untouched (no re-inherit); and the explicit poll flag survives the reload
//     (the file omitted poll, so the flag override is preserved).
//
// Mutation proof (test-only): neutering daemonReloadableConfig.apply to a no-op
// makes both the "live workers = 4 after reload" and the "post-reload dispatch
// limit = 4" assertions go RED — see the PR body / task report.
func TestWarmReloadE2EChangesRunningDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Launch: an on-disk [daemon] section + a live struct seeded from it. ---
	// The file's start value for poll is intentionally NOT what we launch with: we
	// pass an explicit --poll flag, which must win at launch (applyStart) and then
	// survive every later reload (apply never touches a key the file omits).
	paths := reloadFixture(t, "\n[daemon]\npoll = \"99s\"\nworkers = 1\nscheduler = \"barrier\"\n")

	const flagPoll = 5 * time.Second
	live := newDaemonReloadableConfig(30*time.Second, 1, false)
	startCfg, err := config.LoadDaemonRuntimeConfig(paths)
	if err != nil {
		t.Fatalf("LoadDaemonRuntimeConfig(start): %v", err)
	}
	// Simulate the daemon's launch merge: workers/scheduler come from the file, but
	// poll was given as an explicit flag so the file's poll=99s is ignored.
	live.poll = flagPoll
	_ = live.applyStart(startCfg, true /*explicitPoll*/, false, false)
	if p, w, u := live.snapshot(); p != flagPoll || w != 1 || u {
		t.Fatalf("post-launch snapshot = poll:%v workers:%d pool:%v, want 5s/1/barrier", p, w, u)
	}

	// A runtime-auth-shaped token that ONLY exists in the launching env. A warm
	// reload must never re-read the shell, so mutating it after launch must not
	// change the reload outcome, and the reload must not rewrite it (#559 guard).
	t.Setenv("GITMOOT_TEST_RUNTIME_TOKEN", "sk-live-inflight")
	launchToken := os.Getenv("GITMOOT_TEST_RUNTIME_TOKEN")

	// --- A real dispatch pass reads the live limit BEFORE the reload. ---
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	worker := poolSchedulerWorker(t, store, adapter, false)

	lastLimit, seenLimits := setDispatchLimitObserver(t)

	// dispatchOnce mirrors the supervisor tick closure (runSingleRepoSupervisor):
	// read the warm-reloadable snapshot, then drive the REAL production dispatch.
	dispatchOnce := func() {
		_, workers, usePool := live.snapshot()
		w := worker
		w.UsePool = usePool
		if err := runDaemonWorkerTickTracked(ctx, store, w, workers, false, "owner/repo", "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
			t.Fatalf("runDaemonWorkerTickTracked: %v", err)
		}
	}

	dispatchOnce()
	if got := lastLimit(); got != 1 {
		t.Fatalf("pre-reload dispatch limit = %d, want 1 (initial worker count)", got)
	}

	// --- Reload: EDIT the file, then deliver a real SIGHUP through production wiring. ---
	// The new file flips workers 1->4 and scheduler barrier->pool. It OMITS poll, so
	// the explicit launch-flag poll must be preserved across the reload.
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+"\n[daemon]\nworkers = 4\nscheduler = \"pool\"\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	// Corrupt the env AFTER launch, exactly as a fresh shell on `daemon restart`
	// would. A warm reload must ignore it entirely.
	os.Setenv("GITMOOT_TEST_RUNTIME_TOKEN", "")

	sig := newReloadSignalWriter()
	installDaemonReloadHandler(ctx, paths, live, sig)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	sig.waitReloaded(t)

	// --- Assert: the LIVE snapshot reflects the new values. ---
	poll, workers, usePool := live.snapshot()
	if workers != 4 {
		t.Fatalf("live workers = %d after reload, want 4 (warm reload did not change the running setting)", workers)
	}
	if !usePool {
		t.Fatalf("live scheduler still barrier after reload, want pool")
	}
	// The explicit launch-flag poll is preserved because the reload file omitted it.
	if poll != flagPoll {
		t.Fatalf("live poll = %v after reload, want %v (explicit launch flag must override the file key across reloads)", poll, flagPoll)
	}

	// --- Assert: the NEXT real dispatch pass reads the new live limit. ---
	dispatchOnce()
	if got := lastLimit(); got != 4 {
		t.Fatalf("post-reload dispatch limit = %d, want 4 (running dispatch did not pick up the reloaded worker count); seen=%v", got, seenLimits())
	}

	// --- Assert: no teardown, no env re-inherit. ---
	// The same ctx is still live: had a restart been used, this ctx would be
	// cancelled and the running supervision torn down.
	if err := ctx.Err(); err != nil {
		t.Fatalf("context cancelled: the warm reload tore down the running loop (%v)", err)
	}
	// The reload changed workers from the file alone even though the env token was
	// blanked before the reload — proof the reload is env-independent — and it never
	// rewrote the environment either (#559 guard).
	if launchToken != "sk-live-inflight" {
		t.Fatalf("launch token = %q, want sk-live-inflight (env should be captured at launch)", launchToken)
	}
	if got := os.Getenv("GITMOOT_TEST_RUNTIME_TOKEN"); got != "" {
		t.Fatalf("reload rewrote the environment: GITMOOT_TEST_RUNTIME_TOKEN = %q, want empty (reload must not touch env)", got)
	}
}
