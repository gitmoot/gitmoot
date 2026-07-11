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
	if settings.DistillAtTerminal || settings.DistillSuccesses || settings.DistillAllJobs {
		t.Fatalf("distill must be off by default, got %+v", settings)
	}
	if settings.DistillMaxPerJob != DefaultMemoryDistillMaxPerJob {
		t.Fatalf("distill_max_per_job default = %d, want %d", settings.DistillMaxPerJob, DefaultMemoryDistillMaxPerJob)
	}
	if settings.IngestAutoConfirm {
		t.Fatalf("ingest_auto_confirm must default false")
	}
	if settings.GroomSplitLLM != DefaultMemoryGroomSplitLLM {
		t.Fatalf("groom_split_llm default = %v", settings.GroomSplitLLM)
	}
	if settings.GroomSplitLLMRuntime != "codex" || settings.GroomSplitLLMModel != "" || settings.GroomSplitLLMMaxPerRun != 5 {
		t.Fatalf("groom LLM defaults = %+v", settings)
	}
	if settings.ClusterFanout != 12 || settings.ClusterFanoutKeep != 9 || settings.ClusterDepthCap != 4 {
		t.Fatalf("cluster hierarchy defaults = %+v", settings)
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
distill_successes = true
distill_max_per_job = 5
distill_all_jobs = true
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	settings, err := LoadMemorySettings(paths)
	if err != nil {
		t.Fatalf("LoadMemorySettings: %v", err)
	}
	if !settings.DistillAtTerminal || !settings.DistillSuccesses || !settings.DistillAllJobs || settings.DistillMaxPerJob != 5 {
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
ingest_auto_confirm = true
groom_split_llm = true
groom_split_llm_runtime = "claude"
groom_split_llm_model = "sonnet"
groom_split_llm_max_per_run = 3
cluster_fanout = 10
cluster_fanout_keep = 7
cluster_depth_cap = 3
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	settings, err := LoadMemorySettings(paths)
	if err != nil {
		t.Fatalf("LoadMemorySettings: %v", err)
	}
	if !settings.Disabled || settings.TokenBudget != 800 || settings.MaxEntries != 7 || !settings.IngestAutoConfirm || !settings.GroomSplitLLM || settings.GroomSplitLLMRuntime != "claude" || settings.GroomSplitLLMModel != "sonnet" || settings.GroomSplitLLMMaxPerRun != 3 || settings.ClusterFanout != 10 || settings.ClusterFanoutKeep != 7 || settings.ClusterDepthCap != 3 {
		t.Fatalf("parsed = %+v", settings)
	}
}

func TestLoadMemorySettingsRejectsInvalidGroomLLMKnobs(t *testing.T) {
	tests := []string{
		"groom_split_llm_runtime = \"shell\"",
		"groom_split_llm_runtime = \"unknown\"",
		"groom_split_llm_max_per_run = 0",
	}
	for _, setting := range tests {
		t.Run(setting, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+"\n[memory]\n"+setting+"\n"), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := LoadMemorySettings(paths); err == nil {
				t.Fatalf("expected %q to be rejected", setting)
			}
		})
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

func TestLoadMemoryPipelineSettingsDefaults(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	settings, err := LoadMemoryPipelineSettings(paths)
	if err != nil {
		t.Fatalf("LoadMemoryPipelineSettings: %v", err)
	}
	if len(settings.IngestSources) != 0 || settings.IngestSweepInterval != "" || settings.GroomProposeInterval != "" {
		t.Fatalf("default memory pipeline settings should be inert, got %+v", settings)
	}
}

func TestLoadMemoryPipelineSettingsParsesSourcesAndSchedules(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[[memory.ingest]]
path = "/notes/a"
agent = "lead"
repo = "owner/repo"
tier = "repo"

[[memory.ingest]]
path = "/notes/global"
agent = "lead"
tier = "general"

[memory.pipelines]
repo = "owner/repo"
ingest_sweep = "nightly"
ingest_sweep_jitter = "15m"
groom_propose = "48h"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	settings, err := LoadMemoryPipelineSettings(paths)
	if err != nil {
		t.Fatalf("LoadMemoryPipelineSettings: %v", err)
	}
	if len(settings.IngestSources) != 2 {
		t.Fatalf("sources = %+v", settings.IngestSources)
	}
	if settings.IngestSources[0].Repo != "owner/repo" || settings.IngestSources[1].Tier != "general" {
		t.Fatalf("parsed sources = %+v", settings.IngestSources)
	}
	if settings.Repo != "owner/repo" || settings.IngestSweepInterval != "24h" || settings.IngestSweepJitter != "15m" || settings.GroomProposeInterval != "48h" {
		t.Fatalf("parsed pipeline settings = %+v", settings)
	}
}

func TestLoadMemoryPipelineSettingsRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing source path",
			body: `
[[memory.ingest]]
agent = "lead"
`,
			want: "path is required",
		},
		{
			name: "bad tier",
			body: `
[[memory.ingest]]
path = "/notes"
agent = "lead"
tier = "team"
`,
			want: "tier must be repo or general",
		},
		{
			name: "bad interval",
			body: `
[memory.pipelines]
ingest_sweep = "every night"
`,
			want: "expected a Go duration",
		},
		{
			name: "general with repo",
			body: `
[[memory.ingest]]
path = "/notes"
agent = "lead"
repo = "owner/repo"
tier = "general"
`,
			want: "tier general cannot set repo",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+tc.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := LoadMemoryPipelineSettings(paths)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadMemoryPipelineSettings error = %v, want %q", err, tc.want)
			}
		})
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
