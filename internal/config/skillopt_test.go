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

// TestLoadSkillOptPolicyCanarySampleDefaultsOff proves the #484 canary sample knob
// defaults nil (unset) so CanaryEnabled() is OFF by default — byte-identical to
// #471, and a bare auto_promote_canary (sample unset) is a no-op canary.
func TestLoadSkillOptPolicyCanarySampleDefaultsOff(t *testing.T) {
	policy := DefaultSkillOptPolicy()
	if policy.AutoPromoteCanarySample != nil {
		t.Fatalf("auto_promote_canary_sample must default nil (unset), got %v", policy.AutoPromoteCanarySample)
	}
	if policy.CanaryEnabled() {
		t.Fatalf("CanaryEnabled() must be false by default, got %+v", policy)
	}
	// auto_promote_canary alone (no sample) is NOT enabled (fail-safe).
	if (SkillOptPolicy{AutoPromoteCanary: true}).CanaryEnabled() {
		t.Fatal("CanaryEnabled() must be false when the sample is unset")
	}
}

// TestLoadSkillOptPolicyParsesCanarySample proves auto_promote_canary_sample parses
// into the *float64 and that, with auto_promote_canary, CanaryEnabled() is true.
func TestLoadSkillOptPolicyParsesCanarySample(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_promote_canary = true
auto_promote_canary_sample = 0.25
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if policy.AutoPromoteCanarySample == nil || *policy.AutoPromoteCanarySample != 0.25 {
		t.Fatalf("auto_promote_canary_sample = %v, want 0.25", policy.AutoPromoteCanarySample)
	}
	if !policy.CanaryEnabled() {
		t.Fatalf("CanaryEnabled() must be true with canary + valid sample, got %+v", policy)
	}
}

// TestLoadSkillOptPolicyRejectsBadCanarySample proves an out-of-range sample
// (<=0 or >1) and a garbled value both surface a parse error (fail loud, never a
// silently-broken canary).
func TestLoadSkillOptPolicyRejectsBadCanarySample(t *testing.T) {
	for _, bad := range []string{"0", "-0.1", "1.5", "lots"} {
		paths := PathsForHome(t.TempDir())
		if err := Initialize(paths); err != nil {
			t.Fatalf("Initialize returned error: %v", err)
		}
		if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+"\n[skillopt]\nauto_promote_canary_sample = "+bad+"\n"), 0o600); err != nil {
			t.Fatalf("write config returned error: %v", err)
		}
		if _, err := LoadSkillOptPolicy(paths); err == nil {
			t.Fatalf("expected an error for auto_promote_canary_sample = %q", bad)
		}
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

// TestLoadSkillOptPolicyModeBJudgeDefaultsOff proves the #483 cross-family
// LLM-judge auto-pairwise knob defaults OFF (byte-identical to #473 when unset).
func TestLoadSkillOptPolicyModeBJudgeDefaultsOff(t *testing.T) {
	policy := DefaultSkillOptPolicy()
	if policy.ModeBJudgeEnabled {
		t.Fatalf("mode_b_judge_enabled must default false, got %+v", policy)
	}
}

// TestLoadSkillOptPolicyParsesModeBJudge proves mode_b_judge_enabled round-trips:
// true parses on, false parses off, an absent key leaves the default (off), and a
// non-bool value surfaces a parse error (fail-safe).
func TestLoadSkillOptPolicyParsesModeBJudge(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"true", "[skillopt]\nmode_b_judge_enabled = true\n", true},
		{"false", "[skillopt]\nmode_b_judge_enabled = false\n", false},
		{"absent leaves default off", "[skillopt]\nauto_trace_enabled = true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize returned error: %v", err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+"\n"+tc.body), 0o600); err != nil {
				t.Fatalf("write config returned error: %v", err)
			}
			policy, err := LoadSkillOptPolicy(paths)
			if err != nil {
				t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
			}
			if policy.ModeBJudgeEnabled != tc.want {
				t.Fatalf("ModeBJudgeEnabled = %v, want %v", policy.ModeBJudgeEnabled, tc.want)
			}
		})
	}
}

// TestLoadSkillOptPolicyRejectsBadModeBJudge proves a non-bool mode_b_judge_enabled
// surfaces a parse error rather than silently turning the judge on/off.
func TestLoadSkillOptPolicyRejectsBadModeBJudge(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
mode_b_judge_enabled = sometimes
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadSkillOptPolicy(paths); err == nil {
		t.Fatal("expected an error for a non-bool mode_b_judge_enabled")
	}
}

// TestLoadSkillOptPolicyDeterministicCheckersDefaultsDisabled: the #485 objective
// checker knob is OFF by default and DeterministicCheckersEnabled() is false.
func TestLoadSkillOptPolicyDeterministicCheckersDefaultsDisabled(t *testing.T) {
	policy := DefaultSkillOptPolicy()
	if policy.DeterministicCheckers || policy.DeterministicCheckersEnabled() {
		t.Fatalf("deterministic checkers must default OFF, got %+v", policy)
	}
	if policy.DeterministicCheckerList != nil {
		t.Fatalf("deterministic_checkers list must default nil, got %+v", policy.DeterministicCheckerList)
	}
	// The resolved selector falls back to the safe default set (diff_size only).
	resolved := policy.ResolvedDeterministicCheckers()
	if len(resolved) != 1 || resolved[0] != "diff_size" {
		t.Fatalf("default resolved checkers = %v, want [diff_size]", resolved)
	}
}

// TestLoadSkillOptPolicyDeterministicCheckersRequiresAutoTrace:
// deterministic_checkers_enabled alone (without auto_trace_enabled) is OFF —
// DeterministicCheckersEnabled() requires BOTH, mirroring ReviewEnabled().
func TestLoadSkillOptPolicyDeterministicCheckersRequiresAutoTrace(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
deterministic_checkers_enabled = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.DeterministicCheckers {
		t.Fatal("deterministic_checkers_enabled = true should parse")
	}
	if policy.DeterministicCheckersEnabled() {
		t.Fatal("DeterministicCheckersEnabled() must be false without auto_trace_enabled (requires BOTH)")
	}
}

// TestLoadSkillOptPolicyDeterministicCheckersEnabledWithBoth: both knobs on =>
// DeterministicCheckersEnabled().
func TestLoadSkillOptPolicyDeterministicCheckersEnabledWithBoth(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_trace_enabled = true
deterministic_checkers_enabled = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.DeterministicCheckersEnabled() {
		t.Fatalf("DeterministicCheckersEnabled() must be true with both knobs on, got %+v", policy)
	}
}

// TestLoadSkillOptPolicyParsesDeterministicCheckerList: the comma list selector
// parses into a trimmed slice and ResolvedDeterministicCheckers returns it verbatim.
func TestLoadSkillOptPolicyParsesDeterministicCheckerList(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_trace_enabled = true
deterministic_checkers_enabled = true
deterministic_checkers = diff_size, duplication ,lint
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	want := []string{"diff_size", "duplication", "lint"}
	got := policy.ResolvedDeterministicCheckers()
	if len(got) != len(want) {
		t.Fatalf("resolved checkers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolved checkers[%d] = %q, want %q (got %v)", i, got[i], want[i], got)
		}
	}
}

// TestLoadSkillOptPolicyRejectsBadDeterministicCheckersBool: a non-bool
// deterministic_checkers_enabled surfaces a parse error (fail-safe, consistent with
// applySkillOptPolicyField).
func TestLoadSkillOptPolicyRejectsBadDeterministicCheckersBool(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
deterministic_checkers_enabled = perhaps
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadSkillOptPolicy(paths); err == nil {
		t.Fatal("expected an error for a non-bool deterministic_checkers_enabled")
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

// TestRevertDetectionDefaultsOnWithAutoTrace: the #467 sub-knob is UNSET by default
// (nil) and rides AutoTraceEnabled — RevertDetectionEnabled() is true whenever the
// harvester is on with no extra config, but false when auto_trace is off.
func TestRevertDetectionDefaultsOnWithAutoTrace(t *testing.T) {
	policy := DefaultSkillOptPolicy()
	if policy.RevertDetection != nil {
		t.Fatalf("revert_detection must default UNSET (nil), got %+v", policy.RevertDetection)
	}
	// auto_trace off (the default) => detection off, byte-identical.
	if policy.RevertDetectionEnabled() {
		t.Fatal("RevertDetectionEnabled() must be false with auto_trace off")
	}
	// auto_trace on, knob unset => detection ON (nil = on-when-harvester-on).
	policy.AutoTraceEnabled = true
	if !policy.RevertDetectionEnabled() {
		t.Fatal("RevertDetectionEnabled() must be true with auto_trace on and the knob unset")
	}
}

// TestRevertDetectionRequiresAutoTrace: revert_detection_enabled = true alone
// (without auto_trace) is OFF — RevertDetectionEnabled() requires the harvester.
func TestRevertDetectionRequiresAutoTrace(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
revert_detection_enabled = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if policy.RevertDetection == nil || !*policy.RevertDetection {
		t.Fatalf("revert_detection_enabled = true should parse to *true, got %+v", policy.RevertDetection)
	}
	if policy.RevertDetectionEnabled() {
		t.Fatal("RevertDetectionEnabled() must be false without auto_trace_enabled (requires the harvester)")
	}
}

// TestRevertDetectionOptOutWithAutoTrace: auto_trace on but
// revert_detection_enabled = false keeps the harvester on while turning the
// corrective revert overwrites OFF.
func TestRevertDetectionOptOutWithAutoTrace(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_trace_enabled = true
revert_detection_enabled = false
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.Enabled() {
		t.Fatal("auto_trace_enabled = true should keep the harvester on")
	}
	if policy.RevertDetection == nil || *policy.RevertDetection {
		t.Fatalf("revert_detection_enabled = false should parse to *false, got %+v", policy.RevertDetection)
	}
	if policy.RevertDetectionEnabled() {
		t.Fatal("RevertDetectionEnabled() must be false when explicitly opted out")
	}
}

// TestRevertDetectionEnabledWithAutoTraceExplicitTrue: both auto_trace and
// revert_detection_enabled = true => RevertDetectionEnabled().
func TestRevertDetectionEnabledWithAutoTraceExplicitTrue(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
auto_trace_enabled = true
revert_detection_enabled = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.RevertDetectionEnabled() {
		t.Fatalf("RevertDetectionEnabled() must be true with both knobs on, got %+v", policy)
	}
}

// TestRevertDetectionRejectsBadBool: a non-bool revert_detection_enabled is a load
// error (the daemon fail-safes to disabled around it).
func TestRevertDetectionRejectsBadBool(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
revert_detection_enabled = maybe
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadSkillOptPolicy(paths); err == nil {
		t.Fatal("expected an error for a non-bool revert_detection_enabled")
	}
}
