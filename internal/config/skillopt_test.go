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

// TestLoadSkillOptPolicyReviewDefaultsDisabled: the cross-family review knob is OFF
// by default and ReviewEnabled() is false.
func TestLoadSkillOptPolicyReviewDefaultsDisabled(t *testing.T) {
	policy := DefaultSkillOptPolicy()
	if policy.CrossFamilyReviewEnabled || policy.ReviewEnabled() {
		t.Fatalf("cross-family review must default OFF, got %+v", policy)
	}
}

// TestLoadSkillOptPolicyReviewRequiresAutoTrace: cross_family_review_enabled alone
// (without auto_trace_enabled) is OFF — ReviewEnabled() requires BOTH.
func TestLoadSkillOptPolicyReviewRequiresAutoTrace(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
cross_family_review_enabled = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.CrossFamilyReviewEnabled {
		t.Fatal("cross_family_review_enabled = true should parse")
	}
	if policy.ReviewEnabled() {
		t.Fatal("ReviewEnabled() must be false without auto_trace_enabled (requires BOTH)")
	}
}

// TestLoadSkillOptPolicyReviewEnabledWithBoth: both knobs on => ReviewEnabled().
func TestLoadSkillOptPolicyReviewEnabledWithBoth(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_trace_enabled = true
cross_family_review_enabled = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.ReviewEnabled() {
		t.Fatalf("ReviewEnabled() must be true with both knobs on, got %+v", policy)
	}
}

// TestLoadSkillOptPolicyAutoPromoteDefaultsOff proves auto_promote and its
// thresholds default OFF/unset (#471): the manual flow, byte-identical. An unset
// threshold is nil (a hard do-not-promote), never 0.
func TestLoadSkillOptPolicyAutoPromoteDefaultsOff(t *testing.T) {
	policy := DefaultSkillOptPolicy()
	if policy.AutoPromote {
		t.Fatalf("auto_promote must default false, got %+v", policy)
	}
	if policy.AutoPromoteMinSamples != nil || policy.AutoPromoteMinScore != nil {
		t.Fatalf("auto_promote thresholds must default nil (unset), got %+v", policy)
	}
	if policy.AutoPromoteRequireExternalCI || policy.AutoPromoteRequireMeasuredJudge || policy.AutoPromoteCanary {
		t.Fatalf("auto_promote guardrail flags must default false, got %+v", policy)
	}
	// #473 Mode B additive keys default nil (off): byte-identical when unset.
	if policy.AutoPromoteMinConfidence != nil || policy.BanditMinSamples != nil {
		t.Fatalf("auto_promote_min_confidence and bandit_min_samples must default nil (unset), got %+v", policy)
	}
}

// TestLoadSkillOptPolicyParsesModeBKeys proves the #473 auto_promote_min_confidence
// and bandit_min_samples keys parse into their pointers, and an absent section
// leaves them nil (the off-by-default contract).
func TestLoadSkillOptPolicyParsesModeBKeys(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_promote_min_confidence = 0.95
bandit_min_samples = 30
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if policy.AutoPromoteMinConfidence == nil || *policy.AutoPromoteMinConfidence != 0.95 {
		t.Fatalf("auto_promote_min_confidence = %v, want 0.95", policy.AutoPromoteMinConfidence)
	}
	if policy.BanditMinSamples == nil || *policy.BanditMinSamples != 30 {
		t.Fatalf("bandit_min_samples = %v, want 30", policy.BanditMinSamples)
	}
}

// TestLoadSkillOptPolicyParsesAutoPromote proves every new auto_promote_* key
// parses into the policy (including the deferred-but-parsed knobs).
func TestLoadSkillOptPolicyParsesAutoPromote(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_promote = true
auto_promote_min_samples = 5
auto_promote_min_score = 0.9
auto_promote_require_external_ci = true
auto_promote_require_measured_judge = true
auto_promote_canary = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.AutoPromote {
		t.Fatal("auto_promote = true should parse")
	}
	if policy.AutoPromoteMinSamples == nil || *policy.AutoPromoteMinSamples != 5 {
		t.Fatalf("auto_promote_min_samples = %v, want 5", policy.AutoPromoteMinSamples)
	}
	if policy.AutoPromoteMinScore == nil || *policy.AutoPromoteMinScore != 0.9 {
		t.Fatalf("auto_promote_min_score = %v, want 0.9", policy.AutoPromoteMinScore)
	}
	if !policy.AutoPromoteRequireExternalCI || !policy.AutoPromoteRequireMeasuredJudge || !policy.AutoPromoteCanary {
		t.Fatalf("deferred/external-ci guardrail flags should parse, got %+v", policy)
	}
}

// TestLoadSkillOptPolicyRejectsBadAutoPromoteThreshold proves a garbled numeric
// threshold surfaces a parse error (fail-safe: the policy is not silently kept).
func TestLoadSkillOptPolicyRejectsBadAutoPromoteThreshold(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_promote = true
auto_promote_min_samples = lots
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadSkillOptPolicy(paths); err == nil {
		t.Fatal("expected an error for a non-integer auto_promote_min_samples")
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
