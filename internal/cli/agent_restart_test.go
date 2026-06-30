package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// agentRestartFakeAdapter is a runtime.Adapter test double for `agent restart`:
// it records whether Start was called and returns a deterministic, configurable
// new runtime_ref (or an error). Runtime-agnostic — the adapter the restart path
// builds is replaced wholesale via runtimeStartAdapterFor so codex/claude/kimi/kimi-cli
// all funnel through this one fake without depending on each adapter's
// runtime-specific ref generation.
type agentRestartFakeAdapter struct {
	mu           sync.Mutex
	name         string
	newRef       string
	startErr     error
	startCalls   int
	lastRequest  runtime.StartRequest
	lastCheckout string
}

func (a *agentRestartFakeAdapter) Name() string {
	if a.name != "" {
		return a.name
	}
	return "fake"
}

func (a *agentRestartFakeAdapter) Start(_ context.Context, request runtime.StartRequest) (runtime.StartResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.startCalls++
	a.lastRequest = request
	if a.startErr != nil {
		return runtime.StartResult{}, a.startErr
	}
	return runtime.StartResult{RuntimeRef: a.newRef}, nil
}

func (a *agentRestartFakeAdapter) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startCalls
}

func (a *agentRestartFakeAdapter) Validate(context.Context, runtime.Agent) error { return nil }

func (a *agentRestartFakeAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	return runtime.Result{}, nil
}

func (a *agentRestartFakeAdapter) Health(context.Context, runtime.Agent) error { return nil }

func (a *agentRestartFakeAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"ask", "review", "implement"}, nil
}

// replaceRuntimeStartAdapter swaps the restart path's adapter builder for one
// that hands back the supplied fake and records the checkout it was asked for.
func replaceRuntimeStartAdapter(t *testing.T, fake *agentRestartFakeAdapter) {
	t.Helper()
	previous := runtimeStartAdapterFor
	runtimeStartAdapterFor = func(_ runtime.Factory, runtimeName string, checkout string) (runtime.Adapter, error) {
		fake.mu.Lock()
		fake.name = runtimeName
		fake.lastCheckout = checkout
		fake.mu.Unlock()
		return fake, nil
	}
	t.Cleanup(func() { runtimeStartAdapterFor = previous })
}

// seedRestartAgent registers a fully-populated agent (every preserved field set
// to a distinctive value) plus its repo, so a restart can resolve the checkout
// and we can assert nothing but runtime_ref + health changes.
func seedRestartAgent(t *testing.T, home, runtimeName, runtimeRef string) (db.Agent, string) {
	t.Helper()
	repoDir := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(context.Background(), db.Repo{
		Owner:         "owner",
		Name:          "repo",
		DefaultBranch: "main",
		CheckoutPath:  repoDir,
		PollInterval:  "30s",
	}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	agent := db.Agent{
		Name:           "rebind-me",
		Role:           "reviewer",
		Runtime:        runtimeName,
		RuntimeRef:     runtimeRef,
		RepoScope:      "owner/repo",
		Model:          "gpt-5.5",
		Capabilities:   []string{"ask", "review"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "failed",
	}
	if err := store.UpsertAgent(context.Background(), agent); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	return agent, repoDir
}

func getRestartAgent(t *testing.T, home, name string) db.Agent {
	t.Helper()
	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), name)
	if err != nil {
		t.Fatalf("GetAgent(%q) returned error: %v", name, err)
	}
	return agent
}

// T1 (LOAD-BEARING) — restart rebinds runtime_ref + resets health to "unknown"
// while preserving role/repo_scope/model/capabilities/autonomy verbatim. This
// fails against an implementation that rebuilds the agent from flags (it would
// blank the metadata) or that skips the rebind (ref would stay R1).
func TestRunAgentRestartPreservesMetadataAndRebindsSession(t *testing.T) {
	home := t.TempDir()
	const r1 = "550e8400-e29b-41d4-a716-446655440001"
	const r2 = "550e8400-e29b-41d4-a716-446655440002"
	seedRestartAgent(t, home, "codex", r1)

	fake := &agentRestartFakeAdapter{newRef: r2}
	replaceRuntimeStartAdapter(t, fake)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "restart", "rebind-me", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("restart exit code = %d, stderr=%s", code, stderr.String())
	}
	if fake.calls() != 1 {
		t.Fatalf("adapter.Start calls = %d, want 1", fake.calls())
	}
	if !strings.Contains(stdout.String(), "restarted rebind-me (codex); session: "+r2) {
		t.Fatalf("restart output = %q", stdout.String())
	}

	got := getRestartAgent(t, home, "rebind-me")
	if got.RuntimeRef != r2 {
		t.Fatalf("RuntimeRef = %q, want %q (not rebound)", got.RuntimeRef, r2)
	}
	if got.HealthStatus != "unknown" {
		t.Fatalf("HealthStatus = %q, want unknown", got.HealthStatus)
	}
	if got.Role != "reviewer" || got.RepoScope != "owner/repo" || got.Model != "gpt-5.5" ||
		got.AutonomyPolicy != "workspace-write" || strings.Join(got.Capabilities, ",") != "ask,review" {
		t.Fatalf("preserved metadata changed: %+v", got)
	}
}

// T2 — a queued/running job blocks the restart: exit non-zero, the busy message
// (ErrAgentHasActiveJobs), runtime_ref UNCHANGED, adapter.Start NOT called. This
// is load-bearing for the busy guard: an implementation that skips it would
// start a session and rebind.
func TestRunAgentRestartRejectsBusyAgent(t *testing.T) {
	home := t.TempDir()
	const r1 = "550e8400-e29b-41d4-a716-446655440011"
	seedRestartAgent(t, home, "codex", r1)
	func() {
		store := openCLIJobStore(t, home)
		defer store.Close()
		if err := store.CreateJob(context.Background(), db.Job{ID: "busy-job", Agent: "rebind-me", Type: "ask", State: "running", Payload: "{}"}); err != nil {
			t.Fatalf("CreateJob returned error: %v", err)
		}
	}()

	fake := &agentRestartFakeAdapter{newRef: "550e8400-e29b-41d4-a716-446655440012"}
	replaceRuntimeStartAdapter(t, fake)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "restart", "rebind-me", "--home", home}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("restart exit code = 0, want non-zero; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "cancel them first") {
		t.Fatalf("stderr = %q, want busy message", stderr.String())
	}
	if fake.calls() != 0 {
		t.Fatalf("adapter.Start calls = %d, want 0 (busy agent must not start a session)", fake.calls())
	}
	if got := getRestartAgent(t, home, "rebind-me"); got.RuntimeRef != r1 {
		t.Fatalf("RuntimeRef = %q, want unchanged %q", got.RuntimeRef, r1)
	}
}

// T3 — restarting a missing agent fails with the start-to-create hint.
func TestRunAgentRestartRejectsMissingAgent(t *testing.T) {
	home := t.TempDir()
	fake := &agentRestartFakeAdapter{newRef: "x"}
	replaceRuntimeStartAdapter(t, fake)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "restart", "ghost", "--home", home}, &stdout, &stderr); code != 1 {
		t.Fatalf("restart exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not found; use agent start to create it") {
		t.Fatalf("stderr = %q, want not-found hint", stderr.String())
	}
	if fake.calls() != 0 {
		t.Fatalf("adapter.Start calls = %d, want 0", fake.calls())
	}
}

// T4 — a shell-runtime agent has no startable session and is rejected.
func TestRunAgentRestartRejectsShellRuntime(t *testing.T) {
	home := t.TempDir()
	func() {
		store := openCLIJobStore(t, home)
		defer store.Close()
		if err := store.UpsertAgent(context.Background(), db.Agent{
			Name:       "sheller",
			Runtime:    runtime.ShellRuntime,
			RuntimeRef: "echo hi",
		}); err != nil {
			t.Fatalf("UpsertAgent returned error: %v", err)
		}
	}()
	fake := &agentRestartFakeAdapter{newRef: "x"}
	replaceRuntimeStartAdapter(t, fake)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "restart", "sheller", "--home", home}, &stdout, &stderr); code != 1 {
		t.Fatalf("restart exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "shell runtime") {
		t.Fatalf("stderr = %q, want shell rejection", stderr.String())
	}
	if fake.calls() != 0 {
		t.Fatalf("adapter.Start calls = %d, want 0", fake.calls())
	}
}

// T5 — runtime-agnostic: T1's preserve+rebind holds for claude and kimi too.
func TestRunAgentRestartRuntimeAgnostic(t *testing.T) {
	for _, runtimeName := range []string{runtime.ClaudeRuntime, runtime.KimiRuntime} {
		t.Run(runtimeName, func(t *testing.T) {
			home := t.TempDir()
			const r1 = "550e8400-e29b-41d4-a716-446655440021"
			const r2 = "550e8400-e29b-41d4-a716-446655440022"
			seedRestartAgent(t, home, runtimeName, r1)

			fake := &agentRestartFakeAdapter{newRef: r2}
			replaceRuntimeStartAdapter(t, fake)

			var stdout, stderr bytes.Buffer
			if code := Run([]string{"agent", "restart", "rebind-me", "--home", home}, &stdout, &stderr); code != 0 {
				t.Fatalf("restart %s exit code = %d, stderr=%s", runtimeName, code, stderr.String())
			}
			if fake.calls() != 1 {
				t.Fatalf("adapter.Start calls = %d, want 1", fake.calls())
			}
			got := getRestartAgent(t, home, "rebind-me")
			if got.RuntimeRef != r2 || got.Runtime != runtimeName || got.HealthStatus != "unknown" {
				t.Fatalf("agent = %+v, want runtime=%s ref=%s health=unknown", got, runtimeName, r2)
			}
			if got.Role != "reviewer" || got.Model != "gpt-5.5" || strings.Join(got.Capabilities, ",") != "ask,review" {
				t.Fatalf("preserved metadata changed: %+v", got)
			}
		})
	}
}

// T6 — adapter.Start failing writes NOTHING: runtime_ref AND health stay exactly
// as loaded (no partial update). Load-bearing for the "write nothing on error"
// invariant.
func TestRunAgentRestartStartFailsLeavesNoPartialWrite(t *testing.T) {
	home := t.TempDir()
	const r1 = "550e8400-e29b-41d4-a716-446655440031"
	seedRestartAgent(t, home, "codex", r1)

	fake := &agentRestartFakeAdapter{startErr: errors.New("session backend exploded")}
	replaceRuntimeStartAdapter(t, fake)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "restart", "rebind-me", "--home", home}, &stdout, &stderr); code != 1 {
		t.Fatalf("restart exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if fake.calls() != 1 {
		t.Fatalf("adapter.Start calls = %d, want 1", fake.calls())
	}
	got := getRestartAgent(t, home, "rebind-me")
	if got.RuntimeRef != r1 {
		t.Fatalf("RuntimeRef = %q, want unchanged %q (no partial write)", got.RuntimeRef, r1)
	}
	if got.HealthStatus != "failed" {
		t.Fatalf("HealthStatus = %q, want unchanged failed (no partial write)", got.HealthStatus)
	}
}

// runtimeAgentForRef builds the runtime.Agent whose runtime:<rt>:<ref> session
// key a seeded "rebind-me" agent maps to — used to drive the REAL
// acquireRuntimeSessionLock at the exact key the restart guard inspects.
func runtimeAgentForRef(runtimeName, runtimeRef string) runtime.Agent {
	return runtime.Agent{Name: "rebind-me", Runtime: runtimeName, RuntimeRef: runtimeRef}
}

// T7 (LOAD-BEARING) — a LIVE same-host owner holding the runtime-session lock
// blocks the restart. The lock is acquired through the REAL
// acquireRuntimeSessionLock, which now records this test process's os.Getpid()
// (live) and os.Hostname() (this host). The guard must REJECT: exit non-zero,
// runtime_ref UNCHANGED, adapter.Start NOT called.
//
// This is the test PR2's removed guard could not pass: that guard fired only on
// lock.OwnerPID > 0, but acquireRuntimeSessionLock recorded no PID (OwnerPID=0),
// so it never fired. Revert EITHER the OwnerPID recording in
// acquireRuntimeSessionLock OR the runtimeSessionHeldByLiveOwner call in
// runAgentRestart and this test fails (restart would proceed and rebind) —
// proving both halves are load-bearing.
func TestRunAgentRestartRejectsLiveSessionOwner(t *testing.T) {
	home := t.TempDir()
	const r1 = "550e8400-e29b-41d4-a716-446655440041"
	agent, _ := seedRestartAgent(t, home, "codex", r1)

	// Acquire the session lock for runtime:codex:<r1> via the real path.
	store := openCLIJobStore(t, home)
	defer store.Close()
	release, acquired, key, _, err := acquireRuntimeSessionLock(
		context.Background(), store, "live-ask-job",
		runtimeAgentForRef(agent.Runtime, agent.RuntimeRef),
		time.Now().UTC(), 30*time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("acquireRuntimeSessionLock: acquired=%v err=%v", acquired, err)
	}
	t.Cleanup(func() { _ = release(context.Background()) })
	if key != "runtime:codex:"+r1 {
		t.Fatalf("session key = %q, want runtime:codex:%s", key, r1)
	}

	fake := &agentRestartFakeAdapter{newRef: "550e8400-e29b-41d4-a716-446655440042"}
	replaceRuntimeStartAdapter(t, fake)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "restart", "rebind-me", "--home", home}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("restart exit code = 0, want non-zero (live session must block); stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "foreground runtime session for rebind-me is in use") {
		t.Fatalf("stderr = %q, want live-session refusal", stderr.String())
	}
	if fake.calls() != 0 {
		t.Fatalf("adapter.Start calls = %d, want 0 (live session must not start a new one)", fake.calls())
	}
	if got := getRestartAgent(t, home, "rebind-me"); got.RuntimeRef != r1 {
		t.Fatalf("RuntimeRef = %q, want unchanged %q", got.RuntimeRef, r1)
	}
}

// seedRuntimeSessionLock writes a raw runtime:<rt>:<ref> resource lock with an
// explicit owner PID / hostname / lease, bypassing acquireRuntimeSessionLock so
// a test can model a stranded (dead-owner) or expired session precisely.
func seedRuntimeSessionLock(t *testing.T, home, runtimeName, runtimeRef string, ownerPID int64, hostname string, expiresAt time.Time) {
	t.Helper()
	store := openCLIJobStore(t, home)
	defer store.Close()
	key, ok := runtimeSessionResourceKey(runtimeAgentForRef(runtimeName, runtimeRef))
	if !ok {
		t.Fatalf("runtimeSessionResourceKey(%s,%s) not resumable", runtimeName, runtimeRef)
	}
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey:   key,
		OwnerJobID:    "seed-session-owner",
		OwnerToken:    "seed-token",
		OwnerPID:      ownerPID,
		OwnerHostname: hostname,
		ExpiresAt:     expiresAt.UTC().Format(time.RFC3339Nano),
	}, time.Now().UTC())
	if err != nil || !acquired {
		t.Fatalf("seed AcquireResourceLock: acquired=%v err=%v", acquired, err)
	}
}

// T8 — a STRANDED (dead-owner) same-host session lock does NOT block the restart:
// restart is exactly the recovery for an abandoned foreground ask. Dead PID
// 2147480000 is far above any live PID; the lease is non-expired and the host is
// this host, so only the PID-liveness gate decides — and a dead PID proceeds.
func TestRunAgentRestartProceedsStrandedDeadOwner(t *testing.T) {
	home := t.TempDir()
	const r1 = "550e8400-e29b-41d4-a716-446655440051"
	const r2 = "550e8400-e29b-41d4-a716-446655440052"
	seedRestartAgent(t, home, "codex", r1)
	thisHost, _ := os.Hostname()
	seedRuntimeSessionLock(t, home, "codex", r1, 2147480000, thisHost, time.Now().UTC().Add(30*time.Minute))

	fake := &agentRestartFakeAdapter{newRef: r2}
	replaceRuntimeStartAdapter(t, fake)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "restart", "rebind-me", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("restart exit code = %d, want 0 (stranded session must be recoverable); stderr=%s", code, stderr.String())
	}
	if fake.calls() != 1 {
		t.Fatalf("adapter.Start calls = %d, want 1", fake.calls())
	}
	if got := getRestartAgent(t, home, "rebind-me"); got.RuntimeRef != r2 {
		t.Fatalf("RuntimeRef = %q, want rebound %q", got.RuntimeRef, r2)
	}
}

// T9 — an EXPIRED lease does NOT block, even with a live owner PID: the lock is
// stale and self-clears on the next acquire, so restart proceeds.
func TestRunAgentRestartProceedsExpiredLease(t *testing.T) {
	home := t.TempDir()
	const r1 = "550e8400-e29b-41d4-a716-446655440061"
	const r2 = "550e8400-e29b-41d4-a716-446655440062"
	seedRestartAgent(t, home, "codex", r1)
	thisHost, _ := os.Hostname()
	// Live PID (this process) but a lease that expired in the past.
	seedRuntimeSessionLock(t, home, "codex", r1, int64(os.Getpid()), thisHost, time.Now().UTC().Add(-time.Minute))

	fake := &agentRestartFakeAdapter{newRef: r2}
	replaceRuntimeStartAdapter(t, fake)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "restart", "rebind-me", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("restart exit code = %d, want 0 (expired lease must not block); stderr=%s", code, stderr.String())
	}
	if fake.calls() != 1 {
		t.Fatalf("adapter.Start calls = %d, want 1", fake.calls())
	}
	if got := getRestartAgent(t, home, "rebind-me"); got.RuntimeRef != r2 {
		t.Fatalf("RuntimeRef = %q, want rebound %q", got.RuntimeRef, r2)
	}
}

// T10 — acquisition records identity: acquireRuntimeSessionLock then
// GetResourceLock shows OwnerPID == os.Getpid() and OwnerHostname ==
// os.Hostname() (trimmed). This is the additive metadata that makes the live
// guard above functional at all.
func TestAcquireRuntimeSessionLockRecordsOwnerIdentity(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	agent := runtimeAgentForRef("codex", "550e8400-e29b-41d4-a716-446655440071")
	release, acquired, key, _, err := acquireRuntimeSessionLock(
		context.Background(), store, "identity-job", agent, time.Now().UTC(), time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("acquireRuntimeSessionLock: acquired=%v err=%v", acquired, err)
	}
	defer func() { _ = release(context.Background()) }()

	lock, err := store.GetResourceLock(context.Background(), key)
	if err != nil {
		t.Fatalf("GetResourceLock(%q) returned error: %v", key, err)
	}
	if lock.OwnerPID != int64(os.Getpid()) {
		t.Fatalf("lock.OwnerPID = %d, want os.Getpid() = %d", lock.OwnerPID, os.Getpid())
	}
	wantHost, _ := os.Hostname()
	if lock.OwnerHostname != strings.TrimSpace(wantHost) {
		t.Fatalf("lock.OwnerHostname = %q, want os.Hostname() = %q", lock.OwnerHostname, strings.TrimSpace(wantHost))
	}
}
