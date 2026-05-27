package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadAndSaveAgentTypes(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.planner]
runtime = "codex"
template = "planner-here"
role = "planner"
capabilities = ["ask", "review"]
max_background = 2
idle_timeout = "15m"
job_timeout = "5m"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	planner := types["planner"]
	if planner.Runtime != "codex" || planner.Template != "planner-here" || planner.Role != "planner" || planner.MaxBackground != 2 || planner.IdleTimeout != "15m" || strings.Join(planner.Capabilities, ",") != "ask,review" {
		t.Fatalf("planner type = %+v", planner)
	}

	planner.MaxBackground = 3
	planner.Capabilities = []string{"ask"}
	if err := SaveAgentType(paths, planner); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	updated, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes after save returned error: %v", err)
	}
	if updated["planner"].MaxBackground != 3 || strings.Join(updated["planner"].Capabilities, ",") != "ask" {
		t.Fatalf("updated planner type = %+v", updated["planner"])
	}
}

func TestLoadAgentTypesAcceptsLegacyPresetKey(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.planner]
runtime = "codex"
preset = "planner-here"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	if got := types["planner"].Template; got != "planner-here" {
		t.Fatalf("legacy preset key loaded template %q, want planner-here", got)
	}

	if err := SaveAgentType(paths, types["planner"]); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config returned error: %v", err)
	}
	if strings.Contains(string(content), "preset =") {
		t.Fatalf("SaveAgentType preserved legacy preset key:\n%s", string(content))
	}
	if !strings.Contains(string(content), `template = "planner-here"`) {
		t.Fatalf("SaveAgentType did not write template key:\n%s", string(content))
	}
}
