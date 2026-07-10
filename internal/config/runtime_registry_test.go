package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeRuntimeRegistryConfig(t *testing.T, body string) Paths {
	t.Helper()
	dir := t.TempDir()
	paths := Paths{ConfigFile: filepath.Join(dir, "config.toml")}
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return paths
}

// TestLoadRuntimeOverridesNoSection proves a config with no [runtimes.*] section
// yields no overrides — the byte-identical default path.
func TestLoadRuntimeOverridesNoSection(t *testing.T) {
	paths := writeRuntimeRegistryConfig(t, "[paths]\ndatabase = \"x\"\n")
	overrides, err := LoadRuntimeOverrides(paths)
	if err != nil {
		t.Fatalf("LoadRuntimeOverrides error: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("expected no overrides, got %v", overrides)
	}
}

// TestLoadRuntimeOverridesParsesKeys proves each key is parsed and its *Set flag
// is only true when the key was present.
func TestLoadRuntimeOverridesParsesKeys(t *testing.T) {
	paths := writeRuntimeRegistryConfig(t, `
[runtimes.codex]
default_model = "gpt-5.5-codex"
default_effort = "high"
models = ["gpt-5.5-codex", "gpt-5.4-codex"]

[runtimes.claude]
capabilities = ["review", "ask"]
usage_source = "custom"
`)
	overrides, err := LoadRuntimeOverrides(paths)
	if err != nil {
		t.Fatalf("LoadRuntimeOverrides error: %v", err)
	}
	if len(overrides) != 2 {
		t.Fatalf("expected 2 overrides, got %d: %+v", len(overrides), overrides)
	}
	codex := overrides[0]
	if codex.Name != "codex" {
		t.Fatalf("order/name wrong: %+v", codex)
	}
	if !codex.DefaultModelSet || codex.DefaultModel != "gpt-5.5-codex" {
		t.Fatalf("default_model not parsed: %+v", codex)
	}
	if !codex.DefaultEffortSet || codex.DefaultEffort != "high" {
		t.Fatalf("default_effort not parsed: %+v", codex)
	}
	if !codex.ModelsSet || !reflect.DeepEqual(codex.Models, []string{"gpt-5.5-codex", "gpt-5.4-codex"}) {
		t.Fatalf("models not parsed: %+v", codex)
	}
	// codex did not set capabilities/usage_source.
	if codex.CapabilitiesSet || codex.UsageSourceSet {
		t.Fatalf("unset keys must have *Set false: %+v", codex)
	}
	claude := overrides[1]
	if !claude.CapabilitiesSet || !reflect.DeepEqual(claude.Capabilities, []string{"review", "ask"}) {
		t.Fatalf("capabilities not parsed: %+v", claude)
	}
	if !claude.UsageSourceSet || claude.UsageSource != "custom" {
		t.Fatalf("usage_source not parsed: %+v", claude)
	}
	if claude.DefaultModelSet || claude.DefaultEffortSet || claude.ModelsSet {
		t.Fatalf("unset keys must have *Set false: %+v", claude)
	}
}

// TestLoadRuntimeOverridesDuplicateMerges proves a repeated [runtimes.<name>]
// merges into one override (last key wins) rather than producing two entries.
func TestLoadRuntimeOverridesDuplicateMerges(t *testing.T) {
	paths := writeRuntimeRegistryConfig(t, `
[runtimes.codex]
default_model = "a"

[runtimes.codex]
default_model = "b"
`)
	overrides, err := LoadRuntimeOverrides(paths)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(overrides) != 1 {
		t.Fatalf("expected 1 merged override, got %d", len(overrides))
	}
	if overrides[0].DefaultModel != "b" {
		t.Fatalf("last key should win: %q", overrides[0].DefaultModel)
	}
}

// TestLoadRuntimeOverridesIgnoresSubSubsection proves a deeper section under a
// runtime name is ignored rather than mis-parsed as the runtime.
func TestLoadRuntimeOverridesIgnoresSubSubsection(t *testing.T) {
	paths := writeRuntimeRegistryConfig(t, `
[runtimes.codex.extra]
default_model = "a"
`)
	overrides, err := LoadRuntimeOverrides(paths)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("sub-subsection must be ignored, got %v", overrides)
	}
}

// TestLoadRuntimeOverridesMissingFile surfaces os.ErrNotExist so the caller can
// treat a fresh box as "no overrides" via errors.Is.
func TestLoadRuntimeOverridesMissingFile(t *testing.T) {
	paths := Paths{ConfigFile: filepath.Join(t.TempDir(), "does-not-exist.toml")}
	if _, err := LoadRuntimeOverrides(paths); !os.IsNotExist(err) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}
