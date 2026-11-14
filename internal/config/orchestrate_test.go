package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadOrchestratePolicyDefaults(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)

	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.BlockedTTL != "" {
		t.Fatalf("BlockedTTL = %q, want empty (disabled) by default", policy.BlockedTTL)
	}
	if policy.BlockedRoleWakeAfter != 0 {
		t.Fatalf("BlockedRoleWakeAfter = %s, want disabled by default", policy.BlockedRoleWakeAfter)
	}
	if policy.MaxConsecutiveMissedWakes != 0 {
		t.Fatalf("MaxConsecutiveMissedWakes = %d, want disabled by default", policy.MaxConsecutiveMissedWakes)
	}
}

// TestLoadOrchestratePolicyBlockedTTL pins #631: blocked_ttl parses from
// [orchestrate]; it is DISABLED (empty) by default; a positive duration is
// accepted; an explicit zero is accepted (still disabled); and a negative or
// non-duration value is rejected.
func TestLoadOrchestratePolicyBlockedTTL(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	// Disabled by default: an [orchestrate] section without blocked_ttl keeps it empty.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.BlockedTTL != "" {
		t.Fatalf("BlockedTTL = %q, want empty (disabled) by default", policy.BlockedTTL)
	}

	// A positive duration parses.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
blocked_ttl = "48h"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.BlockedTTL != "48h" {
		t.Fatalf("BlockedTTL = %q, want 48h", policy.BlockedTTL)
	}

	// An explicit zero is accepted (it is another spelling of disabled).
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
blocked_ttl = "0s"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadOrchestratePolicy(paths); err != nil {
		t.Fatalf("LoadOrchestratePolicy rejected blocked_ttl = 0s: %v", err)
	}

	// A negative duration is rejected.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
blocked_ttl = "-1h"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadOrchestratePolicy(paths); err == nil {
		t.Fatal("LoadOrchestratePolicy accepted a negative blocked_ttl")
	}

	// A non-duration value is rejected.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
blocked_ttl = "soon"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadOrchestratePolicy(paths); err == nil {
		t.Fatal("LoadOrchestratePolicy accepted an invalid blocked_ttl")
	}
}

func TestLoadOrchestratePolicyBlockedRoleWakeAfter(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy(default) error = %v", err)
	}
	if policy.BlockedRoleWakeAfter != 0 {
		t.Fatalf("BlockedRoleWakeAfter = %s, want disabled", policy.BlockedRoleWakeAfter)
	}

	for _, test := range []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{value: "36h", want: 36 * time.Hour},
		{value: "0s", want: 0},
		{value: "-1m", wantErr: true},
		{value: "soon", wantErr: true},
	} {
		if err := os.WriteFile(paths.ConfigFile, []byte("[orchestrate]\nblocked_role_wake_after = \""+test.value+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		policy, err := LoadOrchestratePolicy(paths)
		if test.wantErr {
			if err == nil {
				t.Fatalf("blocked_role_wake_after %q accepted, want error", test.value)
			}
			continue
		}
		if err != nil {
			t.Fatalf("blocked_role_wake_after %q error = %v", test.value, err)
		}
		if policy.BlockedRoleWakeAfter != test.want {
			t.Fatalf("blocked_role_wake_after %q = %s, want %s", test.value, policy.BlockedRoleWakeAfter, test.want)
		}
	}
}

func TestLoadOrchestratePolicyMaxConsecutiveMissedWakes(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		value   string
		want    int
		wantErr bool
	}{
		{value: "3", want: 3},
		{value: "0", want: 0},
		{value: "-1", wantErr: true},
		{value: "many", wantErr: true},
	} {
		if err := os.WriteFile(paths.ConfigFile, []byte("[orchestrate]\nmax_consecutive_missed_wakes = "+test.value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		policy, err := LoadOrchestratePolicy(paths)
		if test.wantErr {
			if err == nil {
				t.Fatalf("max_consecutive_missed_wakes %q accepted, want error", test.value)
			}
			continue
		}
		if err != nil {
			t.Fatalf("max_consecutive_missed_wakes %q error = %v", test.value, err)
		}
		if policy.MaxConsecutiveMissedWakes != test.want {
			t.Fatalf("max_consecutive_missed_wakes %q = %d, want %d", test.value, policy.MaxConsecutiveMissedWakes, test.want)
		}
	}
}

// #2588 deleted the delegation engine and its [orchestrate] budget, timeout and
// escalation keys. Existing homes still carry them, so a config naming those
// retired keys (including values the old validator would have rejected) must
// load clean and produce the same policy as one without them.
func TestLoadOrchestratePolicyIgnoresRetiredDelegationKeys(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
escalation_handle = "@owner"
escalation_ttl = "banana"
max_delegation_depth = -3
max_delegation_token_budget = lots
max_delegation_cost_usd = -0.5
max_delegation_non_progress_streak = many
max_verify_replan_attempts = -1
inline_artifact_bodies = maybe
inject_upstream_dep_context = maybe
default_delegation_timeout = "banana"
default_review_timeout = "0s"
blocked_ttl = "2h"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("a config still carrying retired delegation keys must load: %v", err)
	}
	want := DefaultOrchestratePolicy()
	want.BlockedTTL = "2h"
	if policy != want {
		t.Fatalf("retired keys changed the policy:\n policy = %+v\n   want = %+v", policy, want)
	}
}

// #1753 deleted the cockpit pane wrapper and its four [orchestrate].cockpit_*
// keys. Every config.toml `gitmoot init` has ever written carries those keys, so
// the deletion is only safe if an EXISTING config still loads. This is the
// migration arm: a config that still names all four keys, including values that
// the old validator would have REJECTED, must load clean and produce the same
// policy as one without them.
func TestLoadOrchestratePolicyIgnoresRetiredCockpitKeys(t *testing.T) {
	withKeys := PathsForHome(t.TempDir())
	if err := Initialize(withKeys); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(withKeys.ConfigFile, []byte(DefaultConfig(withKeys)+`
[orchestrate]
cockpit_mode = "maybe"
cockpit_session = "review-room"
cockpit_max_panes = 0
cockpit_pane_key = "row"
blocked_ttl = "2h"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	legacy, err := LoadOrchestratePolicy(withKeys)
	if err != nil {
		t.Fatalf("a config still carrying cockpit_* keys must load: %v", err)
	}

	fresh := PathsForHome(t.TempDir())
	if err := Initialize(fresh); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(fresh.ConfigFile, []byte(DefaultConfig(fresh)+`
[orchestrate]
blocked_ttl = "2h"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	current, err := LoadOrchestratePolicy(fresh)
	if err != nil {
		t.Fatalf("a freshly initialized config must load: %v", err)
	}
	if legacy != current {
		t.Fatalf("retired keys changed the policy:\n legacy = %+v\ncurrent = %+v", legacy, current)
	}

	// And a fresh `gitmoot init` must not write the retired keys back out.
	for _, key := range []string{"cockpit_mode", "cockpit_session", "cockpit_max_panes", "cockpit_pane_key"} {
		if strings.Contains(DefaultConfig(fresh), key) {
			t.Fatalf("DefaultConfig still writes the retired key %q", key)
		}
	}
}

func TestDefaultConfigIncludesOrchestrateSection(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	content := DefaultConfig(paths)
	if !strings.Contains(content, "[orchestrate]") {
		t.Fatalf("DefaultConfig missing [orchestrate] section:\n%s", content)
	}
}
