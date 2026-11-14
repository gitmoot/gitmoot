package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/github"
)

// These regression tests pin the #459 daemon home-convention: the daemon resolves
// `home` to two shapes (the RAW --home and the resolved <home>/.gitmoot root) and
// must NEVER create the phantom <home>/.gitmoot/.gitmoot, while keeping default
// behavior byte-identical. #458 already fixed the misplacement half by routing the
// resolved root to daemonWorkflowEngine on every caller; these tests lock that in
// AND prove the remaining phantom-dir half is gone.

// phantomDir is <home>/.gitmoot/.gitmoot — the doubled home that initializedPaths
// (-> config.Initialize) would create if a resolved root were re-resolved.
func phantomDir(home string) string {
	return filepath.Join(config.PathsForHome(home).Home, config.DirName)
}

// initHomeWithBlockedTTL writes a real, initialized home with an
// [orchestrate].blocked_ttl set, returning the raw home root. It writes the
// config directly (bypassing LoadOrchestratePolicy validation) so a negative or
// unparseable value can be pinned for the off-by-default resolveBlockedTTL cases.
func initHomeWithBlockedTTL(t *testing.T, ttl string) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	body := "[orchestrate]\nblocked_ttl = \"" + ttl + "\"\n"
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

// BLOCKED TTL (#631): resolveBlockedTTL uses a shape-tolerant, side-effect-free
// config resolution with NO default fallback — the sweep is OFF unless the
// operator opted in with a positive duration. This pins that contract
// (positive -> duration; unset/zero/negative/unparseable -> 0) AND the home-shape
// invariant the #446/#459 seam shares: a raw --home and the already-resolved
// <home>/.gitmoot root resolve identically with no phantom doubled home.
func TestResolveBlockedTTLShapeTolerantNoPhantom(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := config.DefaultPaths(); err != nil {
		t.Fatal(err)
	}

	// A positive TTL resolves identically for BOTH the raw --home and the resolved
	// <home>/.gitmoot root, creating no phantom in either case.
	positive := initHomeWithBlockedTTL(t, "48h")
	positiveResolved := config.PathsForHome(positive).Home

	rawTTL := resolveBlockedTTL(positive)
	if rawTTL != 48*time.Hour {
		t.Fatalf("resolveBlockedTTL(raw) = %s, want 48h", rawTTL)
	}
	assertNoPhantom(t, positive)

	resolvedTTL := resolveBlockedTTL(positiveResolved)
	if resolvedTTL != 48*time.Hour {
		t.Fatalf("resolveBlockedTTL(resolved) = %s, want 48h", resolvedTTL)
	}
	assertNoPhantom(t, positive)
	if rawTTL != resolvedTTL {
		t.Fatalf("blocked_ttl differs across home shapes: raw=%s resolved=%s", rawTTL, resolvedTTL)
	}

	// Off-by-default: an unset value, an explicit zero, a negative (rejected by
	// LoadOrchestratePolicy, leaving the empty default), and an unparseable duration
	// all resolve to 0 so the sweep stays disabled.
	unset := t.TempDir()
	if err := config.Initialize(config.PathsForHome(unset)); err != nil {
		t.Fatalf("Initialize unset: %v", err)
	}
	cases := []struct {
		name string
		home string
	}{
		{"unset-raw", unset},
		{"unset-resolved", config.PathsForHome(unset).Home},
		{"zero", initHomeWithBlockedTTL(t, "0s")},
		{"negative", initHomeWithBlockedTTL(t, "-1h")},
		{"unparseable", initHomeWithBlockedTTL(t, "soon")},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBlockedTTL(tc.home); got != 0 {
				t.Fatalf("resolveBlockedTTL(%q) = %s, want 0 (sweep disabled)", tc.home, got)
			}
		})
	}
	assertNoPhantom(t, unset)
}

// PHANTOM producer #2 (durable hardening): the read-only policy loaders must
// NEVER create a phantom doubled home, even when handed the RESOLVED root
// (mimicking the pre-fix buggy supervisor call site). Pre-fix they ran
// initializedPaths -> config.Initialize and created <home>/.gitmoot/.gitmoot. The
// resolved-root input is a CALLER MISTAKE: degrading to an error is fine; creating
// a phantom is not. The raw home (the production ConfigHome invariant) must read
// cleanly with no phantom.
func TestPolicyLoadersNoPhantomDefenseInDepth(t *testing.T) {
	home := initHomeWithBlockedTTL(t, "1h")
	resolved := config.PathsForHome(home).Home

	resolvedWorker := defaultJobWorker(nil, io.Discard, resolved)
	// Each loader, given the resolved root, must create NO phantom regardless of
	// whether the (now non-existent doubled) config reads.
	_, _ = resolvedWorker.parallelSessionPolicy()
	assertNoPhantom(t, home)
	_, _ = resolvedWorker.admissionPolicy()
	assertNoPhantom(t, home)

	// The raw home (the production invariant) reads cleanly AND creates no phantom.
	rawWorker := defaultJobWorker(nil, io.Discard, home)
	if _, err := rawWorker.parallelSessionPolicy(); err != nil {
		t.Fatalf("parallelSessionPolicy(raw): %v", err)
	}
	if _, err := rawWorker.admissionPolicy(); err != nil {
		t.Fatalf("admissionPolicy(raw): %v", err)
	}
	assertNoPhantom(t, home)
}

// HOME (jobWorker path): the engine the per-tick worker builds is rooted at the
// resolved <home>/.gitmoot root (NOT the raw --home, NOT a doubled root) when a
// checkout is present.
func TestJobWorkerWorkflowHomeResolved(t *testing.T) {
	home := initHomeWithBlockedTTL(t, "5m")
	resolvedRoot := config.PathsForHome(home).Home
	checkout := t.TempDir()

	w := defaultJobWorker(nil, io.Discard, home)
	engine := w.defaultWorkflow(checkout)

	if engine.Home != resolvedRoot {
		t.Fatalf("jobWorker engine.Home = %q, want resolved root %q", engine.Home, resolvedRoot)
	}
	assertNoPhantom(t, home)
}

// HOME (supervisor path) + PHANTOM (integration): the registered-repo
// supervisor's poller WorkflowFactory must give the engine the resolved
// <home>/.gitmoot root, and driving it once must leave NO phantom doubled home.
// This wires the poller exactly as runRegisteredRepoSupervisor does: rawHome for
// policy, paths.Home for the engine.
func TestSupervisorPollerWorkflowHomeResolvedNoPhantom(t *testing.T) {
	home := initHomeWithBlockedTTL(t, "45m")
	paths := config.PathsForHome(home)
	resolvedRoot := paths.Home

	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	poller := defaultRegisteredRepoPoller(store, 1, false, io.Discard, home, resolvedRoot)
	assertNoPhantom(t, home)

	checkout := t.TempDir()
	engine := poller.WorkflowFactory(store, github.NewClient(checkout), checkout)
	if engine == nil {
		t.Fatal("WorkflowFactory returned nil engine")
	}
	if engine.Home != resolvedRoot {
		t.Fatalf("supervisor engine.Home = %q, want resolved root %q (not raw --home, not doubled)", engine.Home, resolvedRoot)
	}
	assertNoPhantom(t, home)
}

// The direct test poller construction (pollRegisteredReposWithPoller passes "","") stays a
// no-op: nil engine wiring and no filesystem writes.
func TestEmptyHomePollerIsNoOp(t *testing.T) {
	poller := defaultRegisteredRepoPoller(nil, 1, false, io.Discard, "", "")
	engine := poller.WorkflowFactory(nil, github.NewClient(""), "")
	if engine == nil {
		t.Fatal("WorkflowFactory returned nil engine")
	}
	// Empty home => daemonWorkflowEngine leaves Home unset.
	if engine.Home != "" {
		t.Fatalf("empty-home engine.Home = %q, want empty", engine.Home)
	}
}
