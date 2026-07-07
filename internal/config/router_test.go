package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRouterSettingsDefaultsWhenAbsent(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	settings, err := LoadRouterSettings(paths)
	if err != nil {
		t.Fatalf("LoadRouterSettings: %v", err)
	}
	if settings.ContextEnabled {
		t.Fatalf("expected context_enabled off by default")
	}
}

func TestLoadRouterSettingsReadsContextEnabled(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	existing, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, append(existing, []byte("\n[router]\ncontext_enabled = true\n")...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	settings, err := LoadRouterSettings(paths)
	if err != nil {
		t.Fatalf("LoadRouterSettings: %v", err)
	}
	if !settings.ContextEnabled {
		t.Fatalf("expected context_enabled true")
	}
}

func TestLoadRouterSettingsRejectsMalformed(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[router]\ncontext_enabled = notabool\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadRouterSettings(paths); err == nil {
		t.Fatalf("expected error on malformed context_enabled")
	}
}
