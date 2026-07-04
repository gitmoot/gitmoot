package cli

import (
	"os"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func memoryDaemonHome(t *testing.T, extra string) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if extra != "" {
		if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+extra), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return home, store
}

func TestDaemonMemoryControllerNilWhenNothingEnrolled(t *testing.T) {
	home, store := memoryDaemonHome(t, `
[agents.builder]
runtime = "codex"
`)
	if ctrl := daemonMemoryController(store, home); ctrl != nil {
		t.Fatalf("expected nil controller when no agent is enrolled")
	}
}

func TestDaemonMemoryControllerNilWhenGloballyDisabled(t *testing.T) {
	home, store := memoryDaemonHome(t, `
[memory]
disabled = true

[agents.builder]
runtime = "codex"
memory = true
`)
	if ctrl := daemonMemoryController(store, home); ctrl != nil {
		t.Fatalf("expected nil controller when memory is globally disabled")
	}
}

func TestDaemonMemoryControllerEnablesOnlyEnrolled(t *testing.T) {
	home, store := memoryDaemonHome(t, `
[memory]
token_budget = 900
max_entries = 9

[agents.builder]
runtime = "codex"
memory = true

[agents.planner]
runtime = "codex"
`)
	ctrl := daemonMemoryController(store, home)
	if ctrl == nil {
		t.Fatalf("expected a controller when an agent is enrolled")
	}
	if !ctrl.Enabled("builder") {
		t.Fatalf("enrolled agent builder should be enabled")
	}
	if ctrl.Enabled("planner") {
		t.Fatalf("un-enrolled agent planner should be disabled")
	}
	if ctrl.TokenBudget != 900 || ctrl.MaxEntries != 9 {
		t.Fatalf("controller knobs not sourced from config: budget=%d max=%d", ctrl.TokenBudget, ctrl.MaxEntries)
	}
}
