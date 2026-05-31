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
template = "planner"
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
	if planner.Runtime != "codex" || planner.Template != "planner" || planner.Role != "planner" || planner.MaxBackground != 2 || planner.IdleTimeout != "15m" || strings.Join(planner.Capabilities, ",") != "ask,review" {
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

func TestLoadAgentTypesIgnoresRetiredTemplateAlias(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	retiredAlias := "pre" + "set"
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.planner]
runtime = "codex"
`+retiredAlias+` = "planner"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	if got := types["planner"].Template; got != "" {
		t.Fatalf("retired template alias loaded template %q, want empty", got)
	}

	if err := SaveAgentType(paths, types["planner"]); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config returned error: %v", err)
	}
	if strings.Contains(string(content), retiredAlias+" =") {
		t.Fatalf("SaveAgentType preserved retired template alias:\n%s", string(content))
	}
	if strings.Contains(string(content), `template = "planner"`) {
		t.Fatalf("SaveAgentType wrote template from retired alias:\n%s", string(content))
	}
}

func TestLoadDefaultFeedbackRepo(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[feedback]
repo = "owner/reviews"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	repo, err := LoadDefaultFeedbackRepo(paths)

	if err != nil {
		t.Fatalf("LoadDefaultFeedbackRepo returned error: %v", err)
	}
	if repo != "owner/reviews" {
		t.Fatalf("repo = %q, want owner/reviews", repo)
	}
}
