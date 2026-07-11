package cli

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

type dashboardConfigSettings struct {
	memory         config.MemorySettings
	chat           config.ChatSettings
	orchestrate    config.OrchestratePolicy
	github         config.GitHubLimiterPolicy
	skillopt       config.SkillOptPolicy
	memoryPipeline config.MemoryPipelineSettings
	implementBase  string
}

type dashboardConfigProjectionRow struct {
	section      string
	key          string
	kind         string
	doc          string
	value        func(dashboardConfigSettings) any
	defaultValue func(dashboardConfigSettings) any
}

// dashboardConfigProjection is the sole value-bearing allowlist for Config.
// Keep it in section/key order: the dashboard polls this payload and expects a
// byte-stable projection when neither the config nor the agents have changed.
var dashboardConfigProjection = []dashboardConfigProjectionRow{
	{section: "chat", key: "auto_respond", kind: "flag", doc: "Enable automatic replies for enrolled chat agents.", value: func(s dashboardConfigSettings) any { return s.chat.AutoRespond }, defaultValue: func(s dashboardConfigSettings) any { return s.chat.AutoRespond }},
	{section: "github", key: "max_concurrent", kind: "int", doc: "Maximum number of concurrent GitHub calls; zero is unlimited.", value: func(s dashboardConfigSettings) any { return s.github.MaxConcurrent }, defaultValue: func(s dashboardConfigSettings) any { return s.github.MaxConcurrent }},
	{section: "github", key: "min_interval", kind: "duration", doc: "Minimum spacing between GitHub call starts.", value: func(s dashboardConfigSettings) any { return s.github.MinInterval.String() }, defaultValue: func(s dashboardConfigSettings) any { return s.github.MinInterval.String() }},
	{section: "memory", key: "cluster_depth_cap", kind: "int", doc: "Maximum recursive memory-cluster depth.", value: func(s dashboardConfigSettings) any { return s.memory.ClusterDepthCap }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.ClusterDepthCap }},
	{section: "memory", key: "cluster_fanout", kind: "int", doc: "Sibling-count threshold that triggers memory clustering.", value: func(s dashboardConfigSettings) any { return s.memory.ClusterFanout }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.ClusterFanout }},
	{section: "memory", key: "cluster_fanout_keep", kind: "int", doc: "Hysteresis threshold for retaining an existing memory grouping.", value: func(s dashboardConfigSettings) any { return s.memory.ClusterFanoutKeep }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.ClusterFanoutKeep }},
	{section: "memory", key: "disabled", kind: "flag", doc: "Disable persistent memory globally, overriding per-agent enrollment.", value: func(s dashboardConfigSettings) any { return s.memory.Disabled }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.Disabled }},
	{section: "memory", key: "distill_all_jobs", kind: "flag", doc: "Distill outcomes from agents that are not enrolled in memory.", value: func(s dashboardConfigSettings) any { return s.memory.DistillAllJobs }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.DistillAllJobs }},
	{section: "memory", key: "distill_at_terminal", kind: "flag", doc: "Stage deterministic memory observations when jobs terminate.", value: func(s dashboardConfigSettings) any { return s.memory.DistillAtTerminal }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.DistillAtTerminal }},
	{section: "memory", key: "distill_max_per_job", kind: "int", doc: "Maximum observations staged by distillation for one job.", value: func(s dashboardConfigSettings) any { return s.memory.DistillMaxPerJob }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.DistillMaxPerJob }},
	{section: "memory", key: "distill_successes", kind: "flag", doc: "Stage observations from successful promotions and recoveries.", value: func(s dashboardConfigSettings) any { return s.memory.DistillSuccesses }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.DistillSuccesses }},
	{section: "memory", key: "groom_split_llm", kind: "flag", doc: "Enable LLM-guided lossless splitting during memory grooming.", value: func(s dashboardConfigSettings) any { return s.memory.GroomSplitLLM }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.GroomSplitLLM }},
	{section: "memory", key: "groom_split_llm_max_per_run", kind: "int", doc: "Maximum LLM split verdicts requested in one grooming run.", value: func(s dashboardConfigSettings) any { return s.memory.GroomSplitLLMMaxPerRun }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.GroomSplitLLMMaxPerRun }},
	{section: "memory", key: "groom_split_llm_model", kind: "string", doc: "Optional model override for LLM-guided memory splitting.", value: func(s dashboardConfigSettings) any { return s.memory.GroomSplitLLMModel }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.GroomSplitLLMModel }},
	{section: "memory", key: "groom_split_llm_runtime", kind: "string", doc: "Runtime used for LLM-guided memory splitting.", value: func(s dashboardConfigSettings) any { return s.memory.GroomSplitLLMRuntime }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.GroomSplitLLMRuntime }},
	{section: "memory", key: "groom_stale", kind: "flag", doc: "Detect stale operational-status memories during grooming.", value: func(s dashboardConfigSettings) any { return s.memory.GroomStale }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.GroomStale }},
	{section: "memory", key: "groom_stale_age", kind: "duration", doc: "Minimum age before an operational-status memory is stale.", value: func(s dashboardConfigSettings) any { return s.memory.GroomStaleAge.String() }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.GroomStaleAge.String() }},
	{section: "memory", key: "ingest_auto_confirm", kind: "flag", doc: "Immediately confirm newly ingested private memories.", value: func(s dashboardConfigSettings) any { return s.memory.IngestAutoConfirm }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.IngestAutoConfirm }},
	{section: "memory", key: "max_entries", kind: "int", doc: "Maximum confirmed memories considered for context injection.", value: func(s dashboardConfigSettings) any { return s.memory.MaxEntries }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.MaxEntries }},
	{section: "memory", key: "token_budget", kind: "int", doc: "Token budget for injected memory context.", value: func(s dashboardConfigSettings) any { return s.memory.TokenBudget }, defaultValue: func(s dashboardConfigSettings) any { return s.memory.TokenBudget }},
	{section: "memory.pipelines", key: "groom_propose", kind: "duration", doc: "Schedule for automatic memory groom proposal runs.", value: func(s dashboardConfigSettings) any { return s.memoryPipeline.GroomProposeInterval }, defaultValue: func(s dashboardConfigSettings) any { return s.memoryPipeline.GroomProposeInterval }},
	{section: "memory.pipelines", key: "groom_propose_jitter", kind: "duration", doc: "Scheduling jitter applied to memory groom proposals.", value: func(s dashboardConfigSettings) any { return s.memoryPipeline.GroomProposeJitter }, defaultValue: func(s dashboardConfigSettings) any { return s.memoryPipeline.GroomProposeJitter }},
	{section: "memory.pipelines", key: "ingest_sweep", kind: "duration", doc: "Schedule for automatic memory ingest sweeps.", value: func(s dashboardConfigSettings) any { return s.memoryPipeline.IngestSweepInterval }, defaultValue: func(s dashboardConfigSettings) any { return s.memoryPipeline.IngestSweepInterval }},
	{section: "memory.pipelines", key: "ingest_sweep_jitter", kind: "duration", doc: "Scheduling jitter applied to memory ingest sweeps.", value: func(s dashboardConfigSettings) any { return s.memoryPipeline.IngestSweepJitter }, defaultValue: func(s dashboardConfigSettings) any { return s.memoryPipeline.IngestSweepJitter }},
	{section: "orchestrate", key: "blocked_ttl", kind: "duration", doc: "Maximum time a blocked job remains awaiting a human; empty disables expiry.", value: func(s dashboardConfigSettings) any { return s.orchestrate.BlockedTTL }, defaultValue: func(s dashboardConfigSettings) any { return s.orchestrate.BlockedTTL }},
	{section: "skillopt", key: "auto_promote", kind: "flag", doc: "Promote qualifying template candidates automatically.", value: func(s dashboardConfigSettings) any { return s.skillopt.AutoPromote }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.AutoPromote }},
	{section: "skillopt", key: "auto_promote_canary", kind: "flag", doc: "Route automatic promotions through a sampled canary.", value: func(s dashboardConfigSettings) any { return s.skillopt.AutoPromoteCanary }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.AutoPromoteCanary }},
	{section: "skillopt", key: "auto_promote_require_external_ci", kind: "flag", doc: "Require positive external CI evidence before automatic promotion.", value: func(s dashboardConfigSettings) any { return s.skillopt.AutoPromoteRequireExternalCI }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.AutoPromoteRequireExternalCI }},
	{section: "skillopt", key: "auto_promote_require_measured_judge", kind: "flag", doc: "Require calibrated judge evidence before automatic promotion.", value: func(s dashboardConfigSettings) any { return s.skillopt.AutoPromoteRequireMeasuredJudge }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.AutoPromoteRequireMeasuredJudge }},
	{section: "skillopt", key: "auto_trace_enabled", kind: "flag", doc: "Harvest terminal workflow outcomes into SkillOpt evaluations.", value: func(s dashboardConfigSettings) any { return s.skillopt.AutoTraceEnabled }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.AutoTraceEnabled }},
	{section: "skillopt", key: "cross_family_review_enabled", kind: "flag", doc: "Add cross-family review evidence to automatic traces.", value: func(s dashboardConfigSettings) any { return s.skillopt.CrossFamilyReviewEnabled }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.CrossFamilyReviewEnabled }},
	{section: "skillopt", key: "deterministic_checkers_enabled", kind: "flag", doc: "Add deterministic checker evidence to automatic traces.", value: func(s dashboardConfigSettings) any { return s.skillopt.DeterministicCheckers }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.DeterministicCheckers }},
	{section: "skillopt", key: "gate_enabled", kind: "flag", doc: "Require a passing fixed-corpus replay gate before promotion.", value: func(s dashboardConfigSettings) any { return s.skillopt.Gate }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.Gate }},
	{section: "skillopt", key: "hard_verifiers_enabled", kind: "flag", doc: "Run configured deterministic verifier commands for automatic traces.", value: func(s dashboardConfigSettings) any { return s.skillopt.HardVerifiers }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.HardVerifiers }},
	{section: "skillopt", key: "mode_b_judge_enabled", kind: "flag", doc: "Enable cross-family judging for SkillOpt A/B comparisons.", value: func(s dashboardConfigSettings) any { return s.skillopt.ModeBJudgeEnabled }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.ModeBJudgeEnabled }},
	{section: "skillopt", key: "pace_enabled", kind: "flag", doc: "Require the anytime-valid PACE gate before automatic promotion.", value: func(s dashboardConfigSettings) any { return s.skillopt.PaceEnabled }, defaultValue: func(s dashboardConfigSettings) any { return s.skillopt.PaceEnabled }},
	{section: "workflow", key: "implement_base", kind: "string", doc: "Base ref used when dispatching implementation jobs.", value: func(s dashboardConfigSettings) any { return s.implementBase }, defaultValue: func(s dashboardConfigSettings) any { return s.implementBase }},
}

// Config returns the effective, sanitized dashboard configuration. Every value
// comes from a canonical loader and every default comes from its canonical
// constructor; arbitrary TOML values are never copied into the response.
func (d *webDataSource) Config(ctx context.Context) (dashboard.ConfigSnapshot, error) {
	out := dashboard.ConfigSnapshot{ContractVersion: 1, Sections: []dashboard.ConfigSection{}, Agents: []dashboard.ConfigAgent{}, UnknownKeys: []string{}}
	err := withStoreAndPaths(d.home, func(paths config.Paths, store *db.Store) error {
		out.Path = paths.ConfigFile
		if info, err := os.Stat(paths.ConfigFile); err == nil {
			out.Exists = true
			out.ModifiedAt = info.ModTime().UnixMilli()
		} else if !os.IsNotExist(err) {
			return err
		}

		values, defaults, err := loadDashboardConfigSettings(paths)
		if err != nil {
			return err
		}
		out.Sections = projectDashboardConfig(values, defaults)

		agentTypes, err := config.LoadAgentTypes(paths)
		if err != nil {
			return fmt.Errorf("load agent config: %w", err)
		}
		agents, err := store.ListAgents(ctx)
		if err != nil {
			return fmt.Errorf("list agents: %w", err)
		}
		out.Agents = projectDashboardConfigAgents(agents, agentTypes)
		// Unknown-key discovery re-parses the file with a strict TOML reader,
		// which can reject values gitmoot's lenient loader accepts (e.g. the
		// live box's bare-word list `deterministic_checkers = diff_size,...`).
		// The names are decoration, never worth failing the snapshot: degrade
		// to an empty list on any parse error.
		if unknown, unknownErr := dashboardUnknownConfigKeys(paths.ConfigFile); unknownErr == nil {
			out.UnknownKeys = unknown
		}
		return nil
	})
	if err != nil {
		return dashboard.ConfigSnapshot{}, err
	}
	return out, nil
}

func loadDashboardConfigSettings(paths config.Paths) (dashboardConfigSettings, dashboardConfigSettings, error) {
	values := dashboardConfigSettings{}
	defaults := dashboardConfigSettings{
		memory:      config.DefaultMemorySettings(),
		chat:        config.DefaultChatSettings(),
		orchestrate: config.DefaultOrchestratePolicy(),
		github:      config.DefaultGitHubLimiterPolicy(),
		skillopt:    config.DefaultSkillOptPolicy(),
	}
	var err error
	if values.memory, err = config.LoadMemorySettings(paths); err != nil {
		return values, defaults, fmt.Errorf("load memory config: %w", err)
	}
	if values.chat, err = config.LoadChatSettings(paths); err != nil {
		return values, defaults, fmt.Errorf("load chat config: %w", err)
	}
	if values.orchestrate, err = config.LoadOrchestratePolicy(paths); err != nil {
		return values, defaults, fmt.Errorf("load orchestrate config: %w", err)
	}
	if values.github, err = config.LoadGitHubLimiterPolicy(paths); err != nil {
		return values, defaults, fmt.Errorf("load github config: %w", err)
	}
	if values.skillopt, err = config.LoadSkillOptPolicy(paths); err != nil {
		return values, defaults, fmt.Errorf("load skillopt config: %w", err)
	}
	if values.memoryPipeline, err = config.LoadMemoryPipelineSettings(paths); err != nil {
		return values, defaults, fmt.Errorf("load memory pipeline config: %w", err)
	}
	if values.implementBase, err = config.LoadImplementBase(paths); err != nil {
		return values, defaults, fmt.Errorf("load workflow config: %w", err)
	}
	return values, defaults, nil
}

func projectDashboardConfig(values, defaults dashboardConfigSettings) []dashboard.ConfigSection {
	sections := make([]dashboard.ConfigSection, 0)
	for _, row := range dashboardConfigProjection {
		if len(sections) == 0 || sections[len(sections)-1].Name != row.section {
			sections = append(sections, dashboard.ConfigSection{Name: row.section, Knobs: []dashboard.ConfigKnob{}})
		}
		value, defaultValue := row.value(values), row.defaultValue(defaults)
		sections[len(sections)-1].Knobs = append(sections[len(sections)-1].Knobs, dashboard.ConfigKnob{
			Key: row.key, Value: value, Default: defaultValue, IsDefault: reflect.DeepEqual(value, defaultValue), Kind: row.kind, Doc: row.doc,
		})
	}
	return sections
}

func projectDashboardConfigAgents(registered []db.Agent, configured map[string]config.AgentType) []dashboard.ConfigAgent {
	byName := make(map[string]dashboard.ConfigAgent, len(registered)+len(configured))
	for _, agent := range registered {
		byName[agent.Name] = dashboard.ConfigAgent{
			Name: agent.Name, Runtime: strings.TrimSpace(agent.Runtime), Model: strings.TrimSpace(agent.Model),
			Capabilities: append([]string(nil), agent.Capabilities...), AutonomyPolicy: strings.TrimSpace(agent.AutonomyPolicy),
		}
	}
	for name, agentType := range configured {
		row := byName[name]
		row.Name = name
		if runtime := strings.TrimSpace(agentType.Runtime); runtime != "" {
			row.Runtime = runtime
		}
		if model := strings.TrimSpace(agentType.Model); model != "" {
			row.Model = model
		}
		row.Memory = agentType.Memory
		row.ChatAutorespond = agentType.ChatAutoRespond
		row.Capabilities = append([]string(nil), agentType.Capabilities...)
		row.AutonomyPolicy = strings.TrimSpace(agentType.AutonomyPolicy)
		row.MaxBackground = agentType.MaxBackground
		byName[name] = row
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]dashboard.ConfigAgent, 0, len(names))
	for _, name := range names {
		row := byName[name]
		if row.Capabilities == nil {
			row.Capabilities = []string{}
		}
		out = append(out, row)
	}
	return out
}

func dashboardUnknownConfigKeys(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	doc, err := tomledit.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse config key names: %w", err)
	}

	known := make(map[string]struct{}, len(dashboardConfigProjection))
	for _, row := range dashboardConfigProjection {
		known[row.section+"."+row.key] = struct{}{}
	}
	agentKeys := map[string]struct{}{
		"runtime": {}, "model": {}, "memory": {}, "chat_autorespond": {}, "capabilities": {}, "autonomy_policy": {}, "max_background": {},
	}
	unknown := map[string]struct{}{}
	doc.Scan(func(key parser.Key, entry *tomledit.Entry) bool {
		if entry.KeyValue == nil {
			return true
		}
		plain := strings.Join(key, ".")
		if _, ok := known[plain]; ok {
			return true
		}
		if len(key) == 3 && key[0] == "agents" {
			if _, ok := agentKeys[key[2]]; ok {
				return true
			}
		}
		unknown[plain] = struct{}{}
		return true
	})
	out := make([]string, 0, len(unknown))
	for key := range unknown {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}
