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
