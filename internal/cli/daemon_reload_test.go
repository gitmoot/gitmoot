package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
)

// reloadFixture initializes a home + config file with the given [daemon] body and
// returns the resolved paths. It mirrors the other cli daemon tests (DefaultConfig +
// body) so the reload path is exercised against a real on-disk config, not a stub.
func reloadFixture(t *testing.T, body string) config.Paths {
	t.Helper()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return paths
}

// TestReloadDaemonConfigAppliesLiveSettings drives the warm-reload path directly
// (deliverables 1-3): a re-read [daemon] section is applied to the LIVE supervisor
// settings, the poll/worker/scheduler snapshot updates without any restart, and a
// concise summary of what was re-read and what changed is logged.
func TestReloadDaemonConfigAppliesLiveSettings(t *testing.T) {
	paths := reloadFixture(t, "\n[daemon]\npoll = \"45s\"\nworkers = 4\nscheduler = \"pool\"\n")
	live := newDaemonReloadableConfig(30*time.Second, 1, false)

	var buf bytes.Buffer
	reloadDaemonConfig(paths, live, &buf)

	poll, workers, usePool := live.snapshot()
	if poll != 45*time.Second {
		t.Fatalf("poll = %v after reload, want 45s", poll)
	}
	if workers != 4 {
		t.Fatalf("workers = %d after reload, want 4", workers)
	}
	if !usePool {
		t.Fatalf("usePool = false after reload, want true (scheduler=pool)")
	}
	out := buf.String()
	for _, want := range []string{"poll 30s->45s", "workers 1->4", "scheduler barrier->pool"} {
		if !strings.Contains(out, want) {
			t.Fatalf("reload summary %q missing %q", strings.TrimSpace(out), want)
		}
	}
}

// TestReloadDaemonConfigPreservesFlagValues asserts a CLI-flag value (a live value the
// file does NOT set) survives a reload untouched, and an absent/empty [daemon] section
// reloads to a no-op — so a warm reload never silently drops in-flight-configured knobs.
func TestReloadDaemonConfigPreservesFlagValues(t *testing.T) {
	// Only poll is in the file; workers/scheduler were "set from a flag" at launch.
	paths := reloadFixture(t, "\n[daemon]\npoll = \"10s\"\n")
	live := newDaemonReloadableConfig(30*time.Second, 8, true)

	var buf bytes.Buffer
	reloadDaemonConfig(paths, live, &buf)

	poll, workers, usePool := live.snapshot()
	if poll != 10*time.Second {
		t.Fatalf("poll = %v, want 10s", poll)
	}
	if workers != 8 || !usePool {
		t.Fatalf("flag-set workers/scheduler changed: workers=%d usePool=%v, want 8/true", workers, usePool)
	}

	// A second reload with no [daemon] keys must be a no-op that preserves everything.
	noop := reloadFixture(t, "\n")
	// Point the live config at the empty-section home by reloading from it.
	buf.Reset()
	reloadDaemonConfig(noop, live, &buf)
	if p, w, u := live.snapshot(); p != 10*time.Second || w != 8 || !u {
		t.Fatalf("no-op reload changed live settings: poll=%v workers=%d usePool=%v", p, w, u)
	}
	if !strings.Contains(buf.String(), "nothing to reload") {
		t.Fatalf("empty [daemon] reload summary = %q, want a no-op notice", strings.TrimSpace(buf.String()))
	}
}

// TestReloadDaemonConfigBadEditKeepsCurrent asserts a broken [daemon] edit never
// disrupts a healthy daemon: the live settings are kept and the error is logged.
func TestReloadDaemonConfigBadEditKeepsCurrent(t *testing.T) {
	paths := reloadFixture(t, "\n[daemon]\npoll = \"not-a-duration\"\n")
	live := newDaemonReloadableConfig(30*time.Second, 2, false)

	var buf bytes.Buffer
	reloadDaemonConfig(paths, live, &buf)

	if p, w, u := live.snapshot(); p != 30*time.Second || w != 2 || u {
		t.Fatalf("bad edit mutated live settings: poll=%v workers=%d usePool=%v", p, w, u)
	}
	if !strings.Contains(buf.String(), "keeping current settings") {
		t.Fatalf("bad-edit reload summary = %q, want a keep-current notice", strings.TrimSpace(buf.String()))
	}
}

// TestReloadDaemonConfigDoesNotReinheritEnv is the #559 regression guard: a warm reload
// re-reads ONLY the config file + the live struct, never the process environment. It
// proves that mutating a runtime-auth-shaped env var between start and reload has NO
// effect on the reload outcome (the reload does not re-inherit the launching shell's
// env) and that the reload itself does not clear or rewrite the env.
func TestReloadDaemonConfigDoesNotReinheritEnv(t *testing.T) {
	// A stand-in for the runtime auth token a `daemon restart` would re-inherit.
	t.Setenv("GITMOOT_TEST_RUNTIME_TOKEN", "sk-live-inflight")
	paths := reloadFixture(t, "\n[daemon]\nworkers = 5\n")
	live := newDaemonReloadableConfig(30*time.Second, 1, false)

	// Corrupt the env AFTER start, exactly as a fresh shell on restart would. A warm
	// reload must ignore it entirely.
	os.Setenv("GITMOOT_TEST_RUNTIME_TOKEN", "")

	var buf bytes.Buffer
	reloadDaemonConfig(paths, live, &buf)

	if _, workers, _ := live.snapshot(); workers != 5 {
		t.Fatalf("workers = %d after reload, want 5 (reload is env-independent)", workers)
	}
	// The reload never writes the environment either.
	if got := os.Getenv("GITMOOT_TEST_RUNTIME_TOKEN"); got != "" {
		t.Fatalf("reload rewrote env: GITMOOT_TEST_RUNTIME_TOKEN = %q", got)
	}
}

// TestInstallDaemonReloadHandlerAppliesOnSIGHUP exercises the real SIGHUP wiring
// (deliverable 1) against an in-test supervisor config: signal.Notify is registered
// synchronously by installDaemonReloadHandler before it returns, so delivering SIGHUP
// to our own process is caught (never the default terminate) and drives a live apply.
// The handler stops when the context is cancelled, so it never outlives the run loop.
func TestInstallDaemonReloadHandlerAppliesOnSIGHUP(t *testing.T) {
	paths := reloadFixture(t, "\n[daemon]\nworkers = 6\n")
	live := newDaemonReloadableConfig(30*time.Second, 1, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installDaemonReloadHandler(ctx, paths, live, &bytes.Buffer{})

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if _, workers, _ := live.snapshot(); workers == 6 {
			return
		}
		select {
		case <-deadline:
			_, workers, _ := live.snapshot()
			t.Fatalf("SIGHUP did not apply reload in time: workers=%d, want 6", workers)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
