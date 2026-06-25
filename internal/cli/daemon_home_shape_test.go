package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

// These regression tests pin the #459 daemon home-convention: the daemon resolves
// `home` to two shapes (the RAW --home and the resolved <home>/.gitmoot root) and
// must NEVER create the phantom <home>/.gitmoot/.gitmoot, while keeping default
// behavior byte-identical (TTL values + ArtifactRoot values unchanged). #458
// already fixed the ArtifactRoot-misplacement half by routing the resolved root to
// daemonWorkflowEngine on every caller; these tests lock that in AND prove the
// remaining phantom-dir half is gone.

// phantomDir is <home>/.gitmoot/.gitmoot — the doubled home that initializedPaths
// (-> config.Initialize) would create if a resolved root were re-resolved.
func phantomDir(home string) string {
	return filepath.Join(config.PathsForHome(home).Home, config.DirName)
}

// initHomeWithEscalationTTL writes a real, initialized home with an
// [orchestrate].escalation_ttl set, returning the raw home root.
func initHomeWithEscalationTTL(t *testing.T, ttl string) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// Overwrite the default config.toml with one that sets escalation_ttl so we can
	// assert the value is read identically from both home shapes.
	body := "[orchestrate]\nescalation_ttl = \"" + ttl + "\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return home
}

func assertNoPhantom(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(phantomDir(home)); !os.IsNotExist(err) {
		t.Fatalf("phantom doubled home %s must not exist (stat err=%v)", phantomDir(home), err)
	}
}

// PHANTOM producer #1 + ESCALATION TTL (both shapes): resolveEscalationTTL must
// return the configured TTL for BOTH the raw --home and the already-resolved
// <home>/.gitmoot root, and create NO phantom dir in either case. Before the fix
// the resolved-input case ran initializedPaths(resolved) -> config.Initialize and
// created <home>/.gitmoot/.gitmoot.
func TestResolveEscalationTTLShapeTolerantNoPhantom(t *testing.T) {
	home := initHomeWithEscalationTTL(t, "30m")
	resolved := config.PathsForHome(home).Home

	rawTTL := resolveEscalationTTL(home)
	if rawTTL != 30*time.Minute {
		t.Fatalf("resolveEscalationTTL(raw) = %s, want 30m", rawTTL)
	}
	assertNoPhantom(t, home)

	resolvedTTL := resolveEscalationTTL(resolved)
	if resolvedTTL != 30*time.Minute {
		t.Fatalf("resolveEscalationTTL(resolved) = %s, want 30m", resolvedTTL)
	}
	// The pre-fix bug: resolving the already-resolved root created the phantom.
	assertNoPhantom(t, home)

	if rawTTL != resolvedTTL {
		t.Fatalf("TTL differs across home shapes: raw=%s resolved=%s", rawTTL, resolvedTTL)
	}
}

// ESCALATION TTL table: {raw, resolved, empty} x {config present/absent} returns
// the SAME TTL and writes nothing to the filesystem.
func TestResolveEscalationTTLTable(t *testing.T) {
	// Config present with an explicit TTL.
	present := initHomeWithEscalationTTL(t, "15m")
	presentResolved := config.PathsForHome(present).Home

	// Config absent: an initialized home whose default config leaves escalation_ttl
	// empty, so resolveEscalationTTL falls back to DefaultEscalationTTL (24h).
	absent := t.TempDir()
	if err := config.Initialize(config.PathsForHome(absent)); err != nil {
		t.Fatalf("Initialize absent: %v", err)
	}
	absentResolved := config.PathsForHome(absent).Home

	defaultTTL, err := time.ParseDuration(config.DefaultEscalationTTL)
	if err != nil {
		t.Fatalf("parse DefaultEscalationTTL: %v", err)
	}

	cases := []struct {
		name string
		home string
		want time.Duration
	}{
		{"present-raw", present, 15 * time.Minute},
		{"present-resolved", presentResolved, 15 * time.Minute},
		{"absent-raw", absent, defaultTTL},
		{"absent-resolved", absentResolved, defaultTTL},
		{"empty", "", defaultTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEscalationTTL(tc.home)
			if got != tc.want {
				t.Fatalf("resolveEscalationTTL(%q) = %s, want %s", tc.home, got, tc.want)
			}
		})
	}
	// Zero filesystem side effects: neither home grew a phantom doubled root.
	assertNoPhantom(t, present)
	assertNoPhantom(t, absent)
}

// PHANTOM producer #2 (durable hardening): the read-only policy loaders must
// NEVER create a phantom doubled home, even when handed the RESOLVED root
// (mimicking the pre-fix buggy supervisor call site). Pre-fix they ran
// initializedPaths -> config.Initialize and created <home>/.gitmoot/.gitmoot. The
// resolved-root input is a CALLER MISTAKE: degrading to an error is fine; creating
// a phantom is not. The raw home (the production ConfigHome invariant) must read
// cleanly with no phantom.
func TestPolicyLoadersNoPhantomDefenseInDepth(t *testing.T) {
	home := initHomeWithEscalationTTL(t, "1h")
	resolved := config.PathsForHome(home).Home

	resolvedWorker := defaultJobWorker(nil, io.Discard, resolved)
	// Each loader, given the resolved root, must create NO phantom regardless of
	// whether the (now non-existent doubled) config reads.
	_, _ = resolvedWorker.orchestratePolicy()
	assertNoPhantom(t, home)
	_, _ = resolvedWorker.parallelSessionPolicy()
	assertNoPhantom(t, home)
	_, _ = resolvedWorker.admissionPolicy()
	assertNoPhantom(t, home)

	// The raw home (the production invariant) reads cleanly AND creates no phantom.
	rawWorker := defaultJobWorker(nil, io.Discard, home)
	if _, err := rawWorker.orchestratePolicy(); err != nil {
		t.Fatalf("orchestratePolicy(raw): %v", err)
	}
	if _, err := rawWorker.parallelSessionPolicy(); err != nil {
		t.Fatalf("parallelSessionPolicy(raw): %v", err)
	}
	if _, err := rawWorker.admissionPolicy(); err != nil {
		t.Fatalf("admissionPolicy(raw): %v", err)
	}
	assertNoPhantom(t, home)
}

// ARTIFACTROOT (jobWorker path): the engine the per-tick worker builds roots
// artifacts at the resolved <home>/.gitmoot root (NOT the raw --home, NOT a
// doubled root), and engine.Home matches when a checkout is present.
func TestJobWorkerWorkflowArtifactRootResolved(t *testing.T) {
	home := initHomeWithEscalationTTL(t, "5m")
	resolvedRoot := config.PathsForHome(home).Home
	checkout := t.TempDir()

	w := defaultJobWorker(nil, io.Discard, home)
	engine := w.defaultWorkflow(checkout)

	if engine.ArtifactRoot != resolvedRoot {
		t.Fatalf("jobWorker engine.ArtifactRoot = %q, want resolved root %q", engine.ArtifactRoot, resolvedRoot)
	}
	if engine.Home != resolvedRoot {
		t.Fatalf("jobWorker engine.Home = %q, want resolved root %q", engine.Home, resolvedRoot)
	}
	assertNoPhantom(t, home)
}

// ARTIFACTROOT (supervisor path) + PHANTOM (integration): the registered-repo
// supervisor's poller WorkflowFactory must root artifacts at the resolved
// <home>/.gitmoot root, and driving it once (exercising both resolveEscalationTTL
// at construction and the orchestratePolicy notifier-handle read inside the
// factory) must leave NO phantom doubled home. This wires the poller exactly as
// runRegisteredRepoSupervisor does: rawHome for policy/TTL, paths.Home for the
// engine.
func TestSupervisorPollerWorkflowArtifactRootResolvedNoPhantom(t *testing.T) {
	home := initHomeWithEscalationTTL(t, "45m")
	paths := config.PathsForHome(home)
	resolvedRoot := paths.Home

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, home, resolvedRoot)

	// resolveEscalationTTL ran at construction; it must have read the real config
	// (45m) and created no phantom.
	if poller.EscalationTTL != 45*time.Minute {
		t.Fatalf("poller.EscalationTTL = %s, want 45m", poller.EscalationTTL)
	}
	assertNoPhantom(t, home)

	checkout := t.TempDir()
	engine := poller.WorkflowFactory(store, github.NewClient(checkout), checkout)
	if engine == nil {
		t.Fatal("WorkflowFactory returned nil engine")
	}
	if engine.ArtifactRoot != resolvedRoot {
		t.Fatalf("supervisor engine.ArtifactRoot = %q, want resolved root %q (not raw --home, not doubled)", engine.ArtifactRoot, resolvedRoot)
	}
	if engine.Home != resolvedRoot {
		t.Fatalf("supervisor engine.Home = %q, want resolved root %q", engine.Home, resolvedRoot)
	}
	// The factory's orchestratePolicy notifier-handle read used the raw home, so no
	// phantom can appear even though the engine got the resolved root.
	assertNoPhantom(t, home)
}

// The legacy/test poller construction (pollRegisteredRepos passes "","") stays a
// no-op: nil engine wiring, default TTL, and no filesystem writes.
func TestEmptyHomePollerIsNoOp(t *testing.T) {
	poller := defaultRegisteredRepoPoller(nil, 1, false, io.Discard, "", "")
	defaultTTL, err := time.ParseDuration(config.DefaultEscalationTTL)
	if err != nil {
		t.Fatalf("parse DefaultEscalationTTL: %v", err)
	}
	if poller.EscalationTTL != defaultTTL {
		t.Fatalf("empty-home poller.EscalationTTL = %s, want %s", poller.EscalationTTL, defaultTTL)
	}
	engine := poller.WorkflowFactory(nil, github.NewClient(""), "")
	if engine == nil {
		t.Fatal("WorkflowFactory returned nil engine")
	}
	// Empty home => daemonWorkflowEngine leaves ArtifactRoot/Home unset.
	if engine.ArtifactRoot != "" {
		t.Fatalf("empty-home engine.ArtifactRoot = %q, want empty", engine.ArtifactRoot)
	}
	if engine.Home != "" {
		t.Fatalf("empty-home engine.Home = %q, want empty", engine.Home)
	}
}
