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
autonomy_policy = " workspace-write "
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
	if planner.Runtime != "codex" || planner.Template != "planner" || planner.Role != "planner" || planner.AutonomyPolicy != "workspace-write" || planner.MaxBackground != 2 || planner.IdleTimeout != "15m" || strings.Join(planner.Capabilities, ",") != "ask,review" {
		t.Fatalf("planner type = %+v", planner)
	}

	planner.MaxBackground = 3
	planner.Capabilities = []string{"ask"}
	planner.AutonomyPolicy = "read-only"
	if err := SaveAgentType(paths, planner); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	updated, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes after save returned error: %v", err)
	}
	if updated["planner"].MaxBackground != 3 || updated["planner"].AutonomyPolicy != "read-only" || strings.Join(updated["planner"].Capabilities, ",") != "ask" {
		t.Fatalf("updated planner type = %+v", updated["planner"])
	}
}

func TestLoadAndSaveAgentTypeModel(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.x]
runtime = "claude"
model = "claude-opus"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	if got := types["x"].Model; got != "claude-opus" {
		t.Fatalf("model = %q, want claude-opus", got)
	}

	// writeAgentTypeBlock round-trips the model.
	var builder strings.Builder
	writeAgentTypeBlock(&builder, types["x"])
	if !strings.Contains(builder.String(), `model = "claude-opus"`) {
		t.Fatalf("writeAgentTypeBlock did not write model:\n%s", builder.String())
	}

	// SaveAgentType round-trips the model through the config file.
	if err := SaveAgentType(paths, types["x"]); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	reloaded, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes after save returned error: %v", err)
	}
	if got := reloaded["x"].Model; got != "claude-opus" {
		t.Fatalf("reloaded model = %q, want claude-opus", got)
	}

	// An empty model omits the model line entirely.
	empty := reloaded["x"]
	empty.Model = ""
	var emptyBuilder strings.Builder
	writeAgentTypeBlock(&emptyBuilder, empty)
	if strings.Contains(emptyBuilder.String(), "model =") {
		t.Fatalf("writeAgentTypeBlock wrote model for empty value:\n%s", emptyBuilder.String())
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

func TestLoadAgentTypesRejectsInvalidAutonomyPolicy(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.planner]
runtime = "codex"
autonomy_policy = "read_only"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	_, err := LoadAgentTypes(paths)
	if err == nil || !strings.Contains(err.Error(), "unsupported autonomy policy") {
		t.Fatalf("LoadAgentTypes error = %v, want unsupported autonomy policy", err)
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
