package config

import (
	"os"
	"testing"
	"time"
)

func TestDefaultMergeGatePolicyOff(t *testing.T) {
	policy := DefaultMergeGatePolicy()
	if policy.RequireExternalCI {
		t.Fatalf("default require_external_ci = true, want false (off)")
	}
	if policy.AutoMerge {
		t.Fatalf("default auto_merge = true, want false")
	}
	if policy.MinCIWait != DefaultMinCIWait {
		t.Fatalf("default MinCIWait = %v, want %v", policy.MinCIWait, DefaultMinCIWait)
	}
	if policy.MaxCIWait != DefaultMaxCIWait {
		t.Fatalf("default MaxCIWait = %v, want %v", policy.MaxCIWait, DefaultMaxCIWait)
	}
}

func TestLoadMergeGatePolicyAbsentKeepsDefaults(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	cfg, err := LoadMergeGatePolicy(paths)
	if err != nil {
		t.Fatalf("LoadMergeGatePolicy returned error: %v", err)
	}
	got := cfg.For("jerryfane/noted")
	if got != DefaultMergeGatePolicy() {
		t.Fatalf("absent [merge_gate] For() = %+v, want defaults %+v", got, DefaultMergeGatePolicy())
	}
}

func TestLoadMergeGatePolicyGlobalSection(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[merge_gate]
auto_merge = true
require_external_ci = true
min_ci_wait = "90s"
max_ci_wait = "5m"
`), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	cfg, err := LoadMergeGatePolicy(paths)
	if err != nil {
		t.Fatalf("LoadMergeGatePolicy returned error: %v", err)
	}
	got := cfg.For("jerryfane/noted")
	if !got.RequireExternalCI {
		t.Fatalf("global require_external_ci = false, want true")
	}
	if !got.AutoMerge {
		t.Fatalf("global auto_merge = false, want true")
	}
	if got.MinCIWait != 90*time.Second {
		t.Fatalf("global MinCIWait = %v, want 90s", got.MinCIWait)
	}
	if got.MaxCIWait != 5*time.Minute {
		t.Fatalf("global MaxCIWait = %v, want 5m", got.MaxCIWait)
	}
}

func TestLoadMergeGatePolicyPerRepoOverride(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[merge_gate]
auto_merge = true
min_ci_wait = "30s"

[repos."jerryfane/noted".merge_gate]
auto_merge = false
require_external_ci = true
max_ci_wait = "3m"
`), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	cfg, err := LoadMergeGatePolicy(paths)
	if err != nil {
		t.Fatalf("LoadMergeGatePolicy returned error: %v", err)
	}

	// The override repo inherits the global min_ci_wait, flips require_external_ci,
	// and sets its own max_ci_wait.
	noted := cfg.For("jerryfane/noted")
	if !noted.RequireExternalCI {
		t.Fatalf("override require_external_ci = false, want true")
	}
	if noted.AutoMerge {
		t.Fatalf("override auto_merge = true, want false")
	}
	if noted.MinCIWait != 30*time.Second {
		t.Fatalf("override MinCIWait = %v, want inherited 30s", noted.MinCIWait)
	}
	if noted.MaxCIWait != 3*time.Minute {
		t.Fatalf("override MaxCIWait = %v, want 3m", noted.MaxCIWait)
	}

	// A repo with no override keeps the global default (require_external_ci off) and
	// the default max_ci_wait.
	other := cfg.For("gitmoot/gitmoot")
	if other.RequireExternalCI {
		t.Fatalf("non-override repo require_external_ci = true, want false")
	}
	if !other.AutoMerge {
		t.Fatalf("non-override repo auto_merge = false, want inherited true")
	}
	if other.MinCIWait != 30*time.Second {
		t.Fatalf("non-override repo MinCIWait = %v, want 30s", other.MinCIWait)
	}
	if other.MaxCIWait != DefaultMaxCIWait {
		t.Fatalf("non-override repo MaxCIWait = %v, want default %v", other.MaxCIWait, DefaultMaxCIWait)
	}
}

func TestLoadMergeGatePolicyRejectsBadValues(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[merge_gate]
require_external_ci = maybe
`), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if _, err := LoadMergeGatePolicy(paths); err == nil {
		t.Fatal("LoadMergeGatePolicy accepted a non-bool require_external_ci")
	}
}
