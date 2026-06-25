package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
)

// Regression for #446: the daemon resolves the [events] policy from the ALREADY-
// resolved home ROOT (config.Paths.Home) that the engine factory passes in
// (daemonWorkflowEngine / jobWorker.workflowHome both yield paths.Home). A prior
// bug re-appended ".gitmoot" via pathsFromFlag/initializedPaths, reading (and
// initializing) a phantom .gitmoot/.gitmoot/config.toml that has no [events]
// section, so the webhook sink was never built even when [events].webhook_url was
// set — the event stream was silently always-off. These tests exercise the REAL
// home resolution (no EventSinkOverride), which the prior unit tests bypassed.
func TestBuildDaemonEventSinkFromResolvedHomeRoot(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home // <home>/.gitmoot
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, config.ConfigName)
	if err := os.WriteFile(cfg, []byte("[events]\nwebhook_url = \"http://127.0.0.1:9/\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := buildDaemonEventSink(nil, root)
	if sink == nil {
		t.Fatalf("expected a non-nil sink for a configured [events].webhook_url at %s", cfg)
	}
	// The buggy double-resolution would have created (and read) this phantom dir.
	if _, err := os.Stat(filepath.Join(root, config.DirName)); err == nil {
		t.Fatalf("phantom doubled home %s must not be created", filepath.Join(root, config.DirName))
	}
}

func TestBuildDaemonEventSinkDisabledWithoutEventsSection(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte("[orchestrate]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sink := buildDaemonEventSink(nil, root); sink != nil {
		t.Fatal("expected nil sink when [events] is absent (off by default)")
	}
}

// On main the registered-repo supervisor passes paths.Home (the already-resolved
// <home>/.gitmoot root), not the raw --home, into daemonWorkflowEngine ->
// daemonEventSink (#459). The resolver must nonetheless also build the sink
// correctly from a RAW --home — kept as defense in depth so a caller mistake can
// never re-introduce the #446 silent-off bug — without creating a phantom doubled
// home. This test exercises that raw-home tolerance directly.
func TestBuildDaemonEventSinkFromRawHome(t *testing.T) {
	home := t.TempDir() // a raw --home value
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte("[events]\nwebhook_url = \"http://127.0.0.1:9/\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := buildDaemonEventSink(nil, home)
	if sink == nil {
		t.Fatal("expected a non-nil sink when passed the raw --home")
	}
	if _, err := os.Stat(filepath.Join(root, config.DirName)); err == nil {
		t.Fatalf("phantom doubled home %s must not be created", filepath.Join(root, config.DirName))
	}
}
