package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

// openHarvestStore opens a throwaway SQLite store for the harvester-wiring tests.
func openHarvestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// writeHarvestConfig writes a config.toml under <home>/.gitmoot and returns the
// resolved home ROOT (what daemonWorkflowEngine passes to daemonOutcomeHarvester).
func writeHarvestConfig(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	root := config.PathsForHome(home).Home // <home>/.gitmoot
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestDaemonOutcomeHarvesterDisabledByDefault proves the off-by-default guarantee
// (#465): with no [skillopt] section (or no config at all), no harvester is
// constructed so daemon behavior and every human-run TrainingPackage are
// byte-identical. Mirrors event_sink_resolve_test.go's disabled assertions.
func TestDaemonOutcomeHarvesterDisabledByDefault(t *testing.T) {
	store := openHarvestStore(t)

	// No config file at all (empty home).
	if h := daemonOutcomeHarvester(store, github.NoopClient{}, t.TempDir()); h != nil {
		t.Fatal("expected nil harvester with no config (off by default)")
	}

	// A config with no [skillopt] section.
	root := writeHarvestConfig(t, "[orchestrate]\n")
	if h := daemonOutcomeHarvester(store, github.NoopClient{}, root); h != nil {
		t.Fatal("expected nil harvester when [skillopt] is absent")
	}

	// An explicit auto_trace_enabled = false.
	rootFalse := writeHarvestConfig(t, "[skillopt]\nauto_trace_enabled = false\n")
	if h := daemonOutcomeHarvester(store, github.NoopClient{}, rootFalse); h != nil {
		t.Fatal("expected nil harvester when auto_trace_enabled = false")
	}
}

// TestDaemonOutcomeHarvesterEnabled proves a non-nil harvester is wired only when
// [skillopt].auto_trace_enabled = true.
func TestDaemonOutcomeHarvesterEnabled(t *testing.T) {
	store := openHarvestStore(t)
	root := writeHarvestConfig(t, "[skillopt]\nauto_trace_enabled = true\n")
	h := daemonOutcomeHarvester(store, github.NoopClient{}, root)
	if h == nil {
		t.Fatal("expected a non-nil harvester when auto_trace_enabled = true")
	}
}

// TestDaemonOutcomeHarvesterNilStore proves a nil store never yields a harvester
// (defensive: the harvester has nothing to write to).
func TestDaemonOutcomeHarvesterNilStore(t *testing.T) {
	root := writeHarvestConfig(t, "[skillopt]\nauto_trace_enabled = true\n")
	if h := daemonOutcomeHarvester(nil, github.NoopClient{}, root); h != nil {
		t.Fatal("expected nil harvester with a nil store")
	}
}

// TestDaemonOutcomeHarvesterFailSafeOnBadConfig proves a malformed [skillopt]
// value fails safe to disabled (nil harvester) rather than erroring the daemon —
// mirroring the #446 fail-safe-to-disabled pattern.
func TestDaemonOutcomeHarvesterFailSafeOnBadConfig(t *testing.T) {
	store := openHarvestStore(t)
	root := writeHarvestConfig(t, "[skillopt]\nauto_trace_enabled = not-a-bool\n")
	if h := daemonOutcomeHarvester(store, github.NoopClient{}, root); h != nil {
		t.Fatal("expected nil harvester when the [skillopt] config is malformed (fail-safe to disabled)")
	}
}

// TestDaemonWorkflowEngineNilHarvesterByDefault proves the full engine wiring
// leaves OutcomeHarvester nil with no [skillopt] config, so the engine constructs
// no Outcome and harvests nothing (byte-identical default).
func TestDaemonWorkflowEngineNilHarvesterByDefault(t *testing.T) {
	store := openHarvestStore(t)
	engine := daemonWorkflowEngine(store, github.NoopClient{}, "/tmp/gitmoot-checkout", "")
	if engine.OutcomeHarvester != nil {
		t.Fatal("daemonWorkflowEngine must wire a nil OutcomeHarvester with no [skillopt] config")
	}
}
