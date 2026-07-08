package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadMemorySettingsDefaults(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// No [memory] section -> documented defaults, feature globally not disabled.
	settings, err := LoadMemorySettings(paths)
	if err != nil {
		t.Fatalf("LoadMemorySettings: %v", err)
	}
	if settings.Disabled {
		t.Fatalf("default settings should not be globally disabled")
	}
	if settings.TokenBudget != DefaultMemoryTokenBudget || settings.MaxEntries != DefaultMemoryMaxEntries {
		t.Fatalf("defaults = %+v", settings)
	}
	// Distill is off by default with a bounded per-job cap.
	if settings.DistillAtTerminal || settings.DistillAllJobs {
		t.Fatalf("distill must be off by default, got %+v", settings)
	}
	if settings.DistillMaxPerJob != DefaultMemoryDistillMaxPerJob {
		t.Fatalf("distill_max_per_job default = %d, want %d", settings.DistillMaxPerJob, DefaultMemoryDistillMaxPerJob)
	}
}

func TestLoadMemorySettingsParsesDistillKnobs(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[memory]
distill_at_terminal = true
distill_max_per_job = 5
distill_all_jobs = true
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	settings, err := LoadMemorySettings(paths)
	if err != nil {
		t.Fatalf("LoadMemorySettings: %v", err)
	}
	if !settings.DistillAtTerminal || !settings.DistillAllJobs || settings.DistillMaxPerJob != 5 {
		t.Fatalf("parsed distill knobs = %+v", settings)
	}
}

func TestLoadMemorySettingsRejectsNegativeDistillCap(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[memory]
distill_max_per_job = -1
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadMemorySettings(paths); err == nil {
		t.Fatalf("expected negative distill_max_per_job to be rejected")
	}
}

func TestLoadMemorySettingsParsesKnobs(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[memory]
disabled = true
token_budget = 800
max_entries = 7
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	settings, err := LoadMemorySettings(paths)
	if err != nil {
		t.Fatalf("LoadMemorySettings: %v", err)
	}
	if !settings.Disabled || settings.TokenBudget != 800 || settings.MaxEntries != 7 {
		t.Fatalf("parsed = %+v", settings)
	}
}

func TestLoadMemorySettingsRejectsNegative(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[memory]
token_budget = -5
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadMemorySettings(paths); err == nil {
		t.Fatalf("expected negative token_budget to be rejected")
	}
}

func TestAgentTypeMemoryFlagRoundTrip(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.builder]
runtime = "codex"
memory = true
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes: %v", err)
	}
	if !types["builder"].Memory {
		t.Fatalf("builder should be enrolled in memory, got %+v", types["builder"])
	}

	// Round-trip: saving preserves the flag, and an unenrolled agent omits the key.
	builder := types["builder"]
	if err := SaveAgentType(paths, builder); err != nil {
		t.Fatalf("SaveAgentType: %v", err)
	}
	reloaded, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded["builder"].Memory {
		t.Fatalf("memory flag lost on save/reload")
	}

	unenrolled := AgentType{Name: "planner", Runtime: "codex"}
	if err := SaveAgentType(paths, unenrolled); err != nil {
		t.Fatalf("SaveAgentType unenrolled: %v", err)
	}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	// The planner block must not carry a memory key (default off, omitted).
	plannerBlock := extractAgentBlock(string(content), "planner")
	if strings.Contains(plannerBlock, "memory") {
		t.Fatalf("unenrolled agent should omit the memory key:\n%s", plannerBlock)
	}
}

func TestAgentTypeMemoryFlagRejectsBadValue(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.builder]
runtime = "codex"
memory = "yes"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadAgentTypes(paths); err == nil {
		t.Fatalf("expected a non-boolean memory value to be rejected")
	}
}

// extractAgentBlock returns the lines of the [agents.<name>] block for assertion.
func extractAgentBlock(content, name string) string {
	lines := strings.Split(content, "\n")
	var out []string
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inBlock = trimmed == "[agents."+name+"]"
			if inBlock {
				out = append(out, line)
			}
			continue
		}
		if inBlock {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
