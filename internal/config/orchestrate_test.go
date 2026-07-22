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
	if policy.CockpitMode != CockpitModeAuto {
		t.Fatalf("CockpitMode = %q, want %q", policy.CockpitMode, CockpitModeAuto)
	}
	if policy.CockpitSession != "" {
		t.Fatalf("CockpitSession = %q, want empty", policy.CockpitSession)
	}
	if policy.CockpitMaxPanes != 4 {
		t.Fatalf("CockpitMaxPanes = %d, want 4", policy.CockpitMaxPanes)
	}
	if policy.CockpitPaneKey != CockpitPaneKeyJob {
		t.Fatalf("CockpitPaneKey = %q, want %q", policy.CockpitPaneKey, CockpitPaneKeyJob)
	}
	if policy.InlineArtifactBodies {
		t.Fatalf("InlineArtifactBodies = true, want false by default")
	}
	if policy.InlineArtifactMaxBytes != 0 {
		t.Fatalf("InlineArtifactMaxBytes = %d, want 0 by default", policy.InlineArtifactMaxBytes)
	}
	if policy.InjectUpstreamDepContext {
		t.Fatalf("InjectUpstreamDepContext = true, want false by default")
	}
	if policy.MaxDelegationTokenBudget != 0 {
		t.Fatalf("MaxDelegationTokenBudget = %d, want 0 (unlimited) by default", policy.MaxDelegationTokenBudget)
	}
	if policy.MaxDelegationCostUSD != 0 {
		t.Fatalf("MaxDelegationCostUSD = %v, want 0 (unlimited) by default", policy.MaxDelegationCostUSD)
	}
	if policy.MaxDelegationNonProgressStreak != 0 {
		t.Fatalf("MaxDelegationNonProgressStreak = %d, want 0 (engine default) by default", policy.MaxDelegationNonProgressStreak)
	}
	if policy.MaxVerifyReplanAttempts != 0 {
		t.Fatalf("MaxVerifyReplanAttempts = %d, want 0 (engine default) by default", policy.MaxVerifyReplanAttempts)
	}
	if policy.BlockedTTL != "" {
		t.Fatalf("BlockedTTL = %q, want empty (disabled) by default", policy.BlockedTTL)
	}
	if policy.MaxConsecutiveMissedWakes != 0 {
		t.Fatalf("MaxConsecutiveMissedWakes = %d, want disabled by default", policy.MaxConsecutiveMissedWakes)
	}
	if policy.DefaultDelegationTimeout != "" || policy.DefaultPlanTimeout != "" || policy.DefaultImplementTimeout != "" || policy.DefaultReviewTimeout != "" || policy.DefaultGateTimeout != "" || policy.DefaultRepairTimeout != "" {
		t.Fatalf("delegation timeout defaults = %+v, want all empty by default", policy)
	}
}

func TestLoadOrchestratePolicyDelegationTimeoutDefaults(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
default_delegation_timeout = "45m"
default_plan_timeout = "15m"
default_implement_timeout = "75m"
default_review_timeout = "25m"
default_gate_timeout = "10m"
default_repair_timeout = "5m"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.DefaultDelegationTimeout != "45m" || policy.DefaultPlanTimeout != "15m" || policy.DefaultImplementTimeout != "75m" || policy.DefaultReviewTimeout != "25m" || policy.DefaultGateTimeout != "10m" || policy.DefaultRepairTimeout != "5m" {
		t.Fatalf("delegation timeout defaults = %+v", policy)
	}

	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
default_review_timeout = ""
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.DefaultReviewTimeout != "" {
		t.Fatalf("empty default_review_timeout = %q, want empty/unbounded", policy.DefaultReviewTimeout)
	}
}

// TestLoadOrchestratePolicyMaxDelegationNonProgressStreak pins #339: the
// result-aware non-progress streak threshold parses from [orchestrate] and an
// absent key keeps the 0 (engine default) value.
func TestLoadOrchestratePolicyMaxDelegationNonProgressStreak(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
max_delegation_non_progress_streak = 3
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.MaxDelegationNonProgressStreak != 3 {
		t.Fatalf("MaxDelegationNonProgressStreak = %d, want 3", policy.MaxDelegationNonProgressStreak)
	}

	// Absent key keeps the engine-default (0) even with the section present.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "auto"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.MaxDelegationNonProgressStreak != 0 {
		t.Fatalf("absent max_delegation_non_progress_streak should default 0, got %d", policy.MaxDelegationNonProgressStreak)
	}
}

// TestLoadOrchestratePolicyMaxVerifyReplanAttempts pins #439: the verify→replan
// attempt cap parses from [orchestrate] and an absent key keeps the 0 (engine
// default) value, mirroring the non-progress-streak test above.
func TestLoadOrchestratePolicyMaxVerifyReplanAttempts(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
max_verify_replan_attempts = 3
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.MaxVerifyReplanAttempts != 3 {
		t.Fatalf("MaxVerifyReplanAttempts = %d, want 3", policy.MaxVerifyReplanAttempts)
	}

	// Absent key keeps the engine-default (0) even with the section present.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "auto"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.MaxVerifyReplanAttempts != 0 {
		t.Fatalf("absent max_verify_replan_attempts should default 0, got %d", policy.MaxVerifyReplanAttempts)
	}
}

// TestLoadOrchestratePolicyMaxDelegationCostUSD pins #380: the dollar-cost budget
// parses from [orchestrate] and an absent key keeps the 0 (unlimited) default,
// mirroring the token-budget test above.
func TestLoadOrchestratePolicyMaxDelegationCostUSD(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
max_delegation_cost_usd = 2.50
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.MaxDelegationCostUSD != 2.50 {
		t.Fatalf("MaxDelegationCostUSD = %v, want 2.50", policy.MaxDelegationCostUSD)
	}

	// Absent key keeps the unlimited default even with the section present.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "auto"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.MaxDelegationCostUSD != 0 {
		t.Fatalf("absent max_delegation_cost_usd should default 0, got %v", policy.MaxDelegationCostUSD)
	}
}

// TestLoadOrchestratePolicyMaxDelegationTokenBudget pins #338 Part B: the token
// budget parses from [orchestrate] and an absent key keeps the 0 (unlimited)
// default.
func TestLoadOrchestratePolicyMaxDelegationTokenBudget(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
max_delegation_token_budget = 500000
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.MaxDelegationTokenBudget != 500000 {
		t.Fatalf("MaxDelegationTokenBudget = %d, want 500000", policy.MaxDelegationTokenBudget)
	}

	// Absent key keeps the unlimited default even with the section present.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "auto"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.MaxDelegationTokenBudget != 0 {
		t.Fatalf("absent max_delegation_token_budget should default 0, got %d", policy.MaxDelegationTokenBudget)
	}
}

// TestLoadOrchestratePolicyEscalationKeys pins #340: escalation_handle and
// escalation_ttl parse from [orchestrate]; absent keys keep the empty defaults;
// the handle drops a leading @; and a non-duration ttl is rejected.
func TestLoadOrchestratePolicyEscalationKeys(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
escalation_handle = "@octocat"
escalation_ttl = "36h"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.EscalationHandle != "octocat" {
		t.Fatalf("EscalationHandle = %q, want octocat (leading @ stripped)", policy.EscalationHandle)
	}
	if policy.EscalationTTL != "36h" {
		t.Fatalf("EscalationTTL = %q, want 36h", policy.EscalationTTL)
	}

	// Absent keys keep the empty defaults even with the section present.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "auto"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.EscalationHandle != "" || policy.EscalationTTL != "" {
		t.Fatalf("absent escalation keys should default empty, got handle=%q ttl=%q", policy.EscalationHandle, policy.EscalationTTL)
	}

	// A non-duration ttl is rejected.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
escalation_ttl = "soon"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadOrchestratePolicy(paths); err == nil {
		t.Fatal("LoadOrchestratePolicy accepted an invalid escalation_ttl")
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
cockpit_mode = "auto"
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

// TestLoadOrchestratePolicyInlineArtifactKeys pins #368: both inline-artifact keys
// parse from [orchestrate], and absent keys default off.
func TestLoadOrchestratePolicyInlineArtifactKeys(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
inline_artifact_bodies = true
inline_artifact_max_bytes = 4096
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if !policy.InlineArtifactBodies {
		t.Fatalf("InlineArtifactBodies = false, want true")
	}
	if policy.InlineArtifactMaxBytes != 4096 {
		t.Fatalf("InlineArtifactMaxBytes = %d, want 4096", policy.InlineArtifactMaxBytes)
	}

	// Absent keys keep the off default even when the section is otherwise present.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "auto"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.InlineArtifactBodies || policy.InlineArtifactMaxBytes != 0 {
		t.Fatalf("absent inline keys should default off, got %+v", policy)
	}
}

// TestLoadOrchestratePolicyInjectUpstreamDepContext pins #419: the
// inject_upstream_dep_context toggle parses from [orchestrate], and an absent key
// keeps the off default, mirroring the inline-artifact keys above.
func TestLoadOrchestratePolicyInjectUpstreamDepContext(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
inject_upstream_dep_context = true
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if !policy.InjectUpstreamDepContext {
		t.Fatalf("InjectUpstreamDepContext = false, want true")
	}

	// Absent key keeps the off default even when the section is otherwise present.
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "auto"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err = LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.InjectUpstreamDepContext {
		t.Fatalf("absent inject_upstream_dep_context should default off, got %+v", policy)
	}
}

func TestLoadOrchestratePolicyOverrides(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "on"
cockpit_session = "review-room"
cockpit_max_panes = 8
cockpit_pane_key = "seat"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadOrchestratePolicy(paths)

	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if policy.CockpitMode != CockpitModeOn || policy.CockpitSession != "review-room" || policy.CockpitMaxPanes != 8 || policy.CockpitPaneKey != CockpitPaneKeySeat {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestLoadOrchestratePolicyRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "cockpit_mode",
			body: `
[orchestrate]
cockpit_mode = "maybe"
`,
			wantErr: "unsupported orchestrate.cockpit_mode",
		},
		{
			name: "cockpit_max_panes",
			body: `
[orchestrate]
cockpit_max_panes = 0
`,
			wantErr: "cockpit_max_panes must be positive",
		},
		{
			name: "cockpit_pane_key",
			body: `
[orchestrate]
cockpit_pane_key = "row"
`,
			wantErr: "unsupported orchestrate.cockpit_pane_key",
		},
		{
			name: "max_delegation_token_budget_non_int",
			body: `
[orchestrate]
max_delegation_token_budget = lots
`,
			wantErr: "parse [orchestrate].max_delegation_token_budget",
		},
		{
			name: "max_delegation_token_budget_negative",
			body: `
[orchestrate]
max_delegation_token_budget = -1
`,
			wantErr: "max_delegation_token_budget must be 0 (unlimited) or positive",
		},
		{
			name: "max_delegation_cost_usd_non_float",
			body: `
[orchestrate]
max_delegation_cost_usd = cheap
`,
			wantErr: "parse [orchestrate].max_delegation_cost_usd",
		},
		{
			name: "max_delegation_cost_usd_negative",
			body: `
[orchestrate]
max_delegation_cost_usd = -0.5
`,
			wantErr: "max_delegation_cost_usd must be 0 (unlimited) or positive",
		},
		{
			name: "inject_upstream_dep_context_non_bool",
			body: `
[orchestrate]
inject_upstream_dep_context = maybe
`,
			wantErr: "parse [orchestrate].inject_upstream_dep_context",
		},
		{
			name: "max_delegation_non_progress_streak_non_int",
			body: `
[orchestrate]
max_delegation_non_progress_streak = many
`,
			wantErr: "parse [orchestrate].max_delegation_non_progress_streak",
		},
		{
			name: "max_delegation_non_progress_streak_negative",
			body: `
[orchestrate]
max_delegation_non_progress_streak = -1
`,
			wantErr: "max_delegation_non_progress_streak must be 0 (engine default) or positive",
		},
		{
			name: "max_verify_replan_attempts_non_int",
			body: `
[orchestrate]
max_verify_replan_attempts = lots
`,
			wantErr: "parse [orchestrate].max_verify_replan_attempts",
		},
		{
			name: "max_verify_replan_attempts_negative",
			body: `
[orchestrate]
max_verify_replan_attempts = -1
`,
			wantErr: "max_verify_replan_attempts must be 0 (engine default) or positive",
		},
		{
			name: "default_delegation_timeout_invalid",
			body: `
[orchestrate]
default_delegation_timeout = "banana"
`,
			wantErr: "orchestrate.default_delegation_timeout",
		},
		{
			name: "default_review_timeout_zero",
			body: `
[orchestrate]
default_review_timeout = "0s"
`,
			wantErr: "orchestrate.default_review_timeout must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize returned error: %v", err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+tt.body), 0o600); err != nil {
				t.Fatalf("write config returned error: %v", err)
			}

			_, err := LoadOrchestratePolicy(paths)

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadOrchestratePolicy error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultConfigIncludesOrchestrateSection(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	content := DefaultConfig(paths)
	if !strings.Contains(content, "[orchestrate]") {
		t.Fatalf("DefaultConfig missing [orchestrate] section:\n%s", content)
	}
}
