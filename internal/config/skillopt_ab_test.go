package config

import (
	"os"
	"strings"
	"testing"
)

// TestLoadSkillOptABPolicyDefaultsOff: with no [skillopt] section (every existing
// home + DefaultConfig), the resolved live-A/B policy is OFF — rate 0.0 — so the
// interceptor is a no-op and the foreground ask path stays byte-identical.
func TestLoadSkillOptABPolicyDefaultsOff(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	policy, err := LoadSkillOptABPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptABPolicy: %v", err)
	}
	if policy.LiveABSampleRate != 0.0 {
		t.Fatalf("default LiveABSampleRate = %v, want 0.0 (off)", policy.LiveABSampleRate)
	}
	if policy.BanditMinSamples != DefaultLiveABBanditMinSamples {
		t.Fatalf("default BanditMinSamples = %d, want %d", policy.BanditMinSamples, DefaultLiveABBanditMinSamples)
	}
}

// TestLoadSkillOptABPolicyDefaultConfigHasNoLiveSkillOptSection guards the byte-
// identical invariant at its source: DefaultConfig must NOT emit an UNCOMMENTED
// [skillopt] section (the only one in the stub is a # comment), so a freshly-
// initialized home resolves rate 0.0.
func TestLoadSkillOptABPolicyDefaultConfigHasNoLiveSkillOptSection(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	for _, line := range strings.Split(DefaultConfig(paths), "\n") {
		if strings.TrimSpace(stripConfigComment(line)) == "[skillopt]" {
			t.Fatal("DefaultConfig must not emit a live [skillopt] section (would change the off-by-default path)")
		}
	}
}

// TestLoadSkillOptABPolicyParsesExplicit: an explicit live_ab_sample_rate +
// bandit_min_samples are parsed from the shared [skillopt] section.
func TestLoadSkillOptABPolicyParsesExplicit(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
live_ab_sample_rate = 0.25
bandit_min_samples = 40
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	policy, err := LoadSkillOptABPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptABPolicy: %v", err)
	}
	if policy.LiveABSampleRate != 0.25 {
		t.Fatalf("LiveABSampleRate = %v, want 0.25", policy.LiveABSampleRate)
	}
	if policy.BanditMinSamples != 40 {
		t.Fatalf("BanditMinSamples = %d, want 40", policy.BanditMinSamples)
	}
}

// TestLoadSkillOptABPolicyRateWithoutFloorUsesDefaultFloor: setting only the rate
// inherits the conservative default floor (so a rate without an explicit floor is
// never accidentally floor-less).
func TestLoadSkillOptABPolicyRateWithoutFloorUsesDefaultFloor(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
live_ab_sample_rate = 0.5
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	policy, err := LoadSkillOptABPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptABPolicy: %v", err)
	}
	if policy.BanditMinSamples != DefaultLiveABBanditMinSamples {
		t.Fatalf("BanditMinSamples = %d, want default %d", policy.BanditMinSamples, DefaultLiveABBanditMinSamples)
	}
}

func TestLoadSkillOptABPolicyRejectsOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"negative", "live_ab_sample_rate = -0.1"},
		{"above one", "live_ab_sample_rate = 1.5"},
		{"nan", "live_ab_sample_rate = nan"},
		{"inf", "live_ab_sample_rate = inf"},
		{"zero floor", "live_ab_sample_rate = 0.5\nbandit_min_samples = 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+"\n[skillopt]\n"+tc.body+"\n"), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := LoadSkillOptABPolicy(paths); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestSkillOptABRoundTripsThroughSetConfigScalar: writing a valid rate via
// SetConfigScalar (which re-runs validateConfigFile, where LoadSkillOptABPolicy is
// now registered) succeeds and parses back; an out-of-range write is rejected and
// reverted, leaving the prior value intact.
func TestSkillOptABRoundTripsThroughSetConfigScalar(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// SetConfigScalar only edits EXISTING keys, so seed the section first.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
live_ab_sample_rate = 0.0
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	validRate := 0.3
	if err := SetConfigScalar(paths, []string{"skillopt", "live_ab_sample_rate"}, ConfigScalar{float: &validRate}); err != nil {
		t.Fatalf("SetConfigScalar valid: %v", err)
	}
	policy, err := LoadSkillOptABPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptABPolicy: %v", err)
	}
	if policy.LiveABSampleRate != 0.3 {
		t.Fatalf("after set, LiveABSampleRate = %v, want 0.3", policy.LiveABSampleRate)
	}

	// An out-of-range write must be rejected by validateConfigFile and reverted.
	invalidRate := 2.0
	if err := SetConfigScalar(paths, []string{"skillopt", "live_ab_sample_rate"}, ConfigScalar{float: &invalidRate}); err == nil {
		t.Fatal("expected SetConfigScalar to reject out-of-range rate 2.0")
	}
	reverted, err := LoadSkillOptABPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptABPolicy after revert: %v", err)
	}
	if reverted.LiveABSampleRate != 0.3 {
		t.Fatalf("after rejected write, LiveABSampleRate = %v, want preserved 0.3", reverted.LiveABSampleRate)
	}
}
