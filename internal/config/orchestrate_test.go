package config

import (
	"os"
	"strings"
	"testing"
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
	if policy.MaxDelegationTokenBudget != 0 {
		t.Fatalf("MaxDelegationTokenBudget = %d, want 0 (unlimited) by default", policy.MaxDelegationTokenBudget)
	}
	if policy.MaxDelegationCostUSD != 0 {
		t.Fatalf("MaxDelegationCostUSD = %v, want 0 (unlimited) by default", policy.MaxDelegationCostUSD)
	}
	if policy.MaxDelegationNonProgressStreak != 0 {
		t.Fatalf("MaxDelegationNonProgressStreak = %d, want 0 (engine default) by default", policy.MaxDelegationNonProgressStreak)
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
