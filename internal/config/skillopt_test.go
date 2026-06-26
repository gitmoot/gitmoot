package config

import (
	"os"
	"testing"
)

func TestLoadSkillOptPolicyDefaultsDisabled(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	// With no [skillopt] section the trace-harvester is OFF.
	if policy.Enabled() || policy.AutoTraceEnabled {
		t.Fatalf("default SkillOptPolicy must be disabled, got %+v", policy)
	}
}

func TestLoadSkillOptPolicyParsesAutoTraceEnabled(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_trace_enabled = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.Enabled() || !policy.AutoTraceEnabled {
		t.Fatalf("SkillOptPolicy with auto_trace_enabled = true should be enabled, got %+v", policy)
	}
}

func TestLoadSkillOptPolicyRejectsBadBool(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_trace_enabled = maybe
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadSkillOptPolicy(paths); err == nil {
		t.Fatal("expected an error for a non-bool auto_trace_enabled")
	}
}

// TestLoadSkillOptPolicyIgnoresOtherSections proves a config that only sets
// [events]/[orchestrate] leaves the trace-harvester at its disabled default.
func TestLoadSkillOptPolicyIgnoresOtherSections(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[events]
webhook_url = "https://example.test/hook"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if policy.Enabled() {
		t.Fatalf("SkillOptPolicy must stay disabled when only [events] is set, got %+v", policy)
	}
}
