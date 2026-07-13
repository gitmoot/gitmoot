package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// Default read-path knobs for agent persistent memory (#626). The token budget
// and max-entries cap are the initial values from the RFC body; they are meant
// to be calibrated empirically by the Phase-1 measurement harness.
const (
	DefaultMemoryTokenBudget = 1500
	DefaultMemoryMaxEntries  = 15
	// DefaultMemoryGroomSplitLLM gates the Phase-2 LLM boundary chooser. Content
	// remains host-sliced and lossless when enabled.
	DefaultMemoryGroomSplitLLM          = false
	DefaultMemoryGroomSplitLLMRuntime   = runtime.CodexRuntime
	DefaultMemoryGroomSplitLLMModel     = ""
	DefaultMemoryGroomSplitLLMMaxPerRun = 5
	DefaultMemoryGroomQuality           = false
	DefaultMemoryGroomQualityMaxPerRun  = 8
	DefaultMemoryGroomQualityMinAge     = 24 * time.Hour
	DefaultMemoryGroomLLMTotalMaxPerRun = 10
	DefaultMemoryGroomStale             = true
	DefaultMemoryGroomStaleAge          = 14 * 24 * time.Hour
	DefaultMemoryClusterFanout          = 12
	DefaultMemoryClusterFanoutKeep      = 9
	DefaultMemoryClusterDepthCap        = 4
	DefaultMemoryHarvestRuntime         = runtime.CodexRuntime
	DefaultMemoryHarvestModel           = ""
	DefaultMemoryHarvestEffort          = "low"
	DefaultMemoryHarvestMaxPerJob       = 2
	DefaultMemoryHarvestMaxJobsPerSweep = 5
	// DefaultMemoryDistillMaxPerJob caps how many pending observations the
	// deterministic distill-at-terminal producers (#737 P4.1) may stage per job.
	// It is only consulted when distill_at_terminal or distill_successes is enabled.
	DefaultMemoryDistillMaxPerJob = 3
)

// MemorySettings is the resolved, off-by-default global knob set for agent
// persistent memory, parsed from the optional [memory] section. Enrollment is
// per-agent ([agents.<name>] memory = true); this section only carries the
// shared read-path knobs plus a global kill switch. A config with no [memory]
// section resolves to the documented defaults, and — crucially — no agent is
// enrolled unless it opts in, so the whole feature is off and behavior is
// byte-identical.
type MemorySettings struct {
	// Disabled is the global kill switch. When true it overrides every per-agent
	// memory=true enrollment, disabling both the read and shadow-write paths.
	// Default false (absent section == not globally disabled), so enrollment alone
	// governs; an operator can flip this to turn the feature off box-wide without
	// editing every agent block.
	Disabled bool
	// DefaultEnroll makes manual `agent start` enroll newly-created agents unless
	// the command explicitly supplies --memory=false. It never affects managed,
	// pipeline, or ephemeral construction paths because they do not call agent
	// start. Default false preserves the existing opt-in behavior.
	DefaultEnroll bool
	// TokenBudget caps the total estimated tokens of the injected learnings block.
	TokenBudget int
	// MaxEntries caps how many confirmed memories are considered for injection.
	MaxEntries int
	// DistillAtTerminal is the master switch for the deterministic
	// distill-at-terminal WRITE producers (#737 P4.1). Default false: with it off
	// the terminal path is byte-identical (no observation rows staged from job
	// outcomes). When true, at each terminal Gitmoot stages a bounded number of
	// PENDING observations (trust_mark=low, provenance "distill:<job-id>") derived
	// deterministically from the result — never confirmed memory (the owner's
	// `memory confirm` gate stays the only promotion path).
	DistillAtTerminal bool
	// DistillSuccesses enables deterministic success-side memory producers (#781).
	// Default false: no SkillOpt promotion observations and no recovered-failure
	// observations are staged. When true, those producers still write only pending
	// low-trust observations; they never confirm memory directly.
	DistillSuccesses bool
	// DistillMaxPerJob is the hard per-job cap on distill writes (default 3). Only
	// consulted when DistillAtTerminal or DistillSuccesses is true; a value <= 0
	// falls back to the default so the producers can never write an unbounded number
	// of rows.
	DistillMaxPerJob int
	// DistillAllJobs widens distill to EVERY job, not only memory-enrolled agents.
	// Default false: distill (like the rest of memory) runs only for agents with
	// [agents.<name>].memory = true. When true it also runs for un-enrolled agents
	// — useful to harvest failure signal box-wide — while the READ/injection and
	// the confirmed mechanical producers stay enrolled-only.
	DistillAllJobs bool
	// IngestAutoConfirm immediately promotes memory ingest and chat remember
	// observations into the authoring agent's private pool. Default false keeps the
	// pending human gate. Shared memory remains explicit through confirm/promote
	// commands even when this is enabled.
	IngestAutoConfirm bool
	// HarvestEnabled gates the daemon-owned post-terminal insight sweep. Harvest
	// stages low-trust shared observations only; it never confirms memory.
	HarvestEnabled         bool
	HarvestRuntime         string
	HarvestModel           string
	HarvestEffort          string
	HarvestMaxPerJob       int
	HarvestMaxJobsPerSweep int
	// GroomSplitLLM enables the Phase-2 one-shot LLM boundary chooser after the
	// deterministic lossless pass. Runtime/model select the isolated one-shot
	// adapter; MaxPerRun caps billable candidate calls.
	GroomSplitLLM          bool
	GroomSplitLLMRuntime   string
	GroomSplitLLMModel     string
	GroomSplitLLMMaxPerRun int
	// GroomQuality controls mutation only: false runs the general audit in
	// shadow mode, while true permits corroborated useless verdicts to retire.
	// Runtime/model are shared with the split classifier.
	GroomQuality           bool
	GroomQualityMaxPerRun  int
	GroomQualityMinAge     time.Duration
	GroomLLMTotalMaxPerRun int
	// GroomStale enables deterministic operational-status detection. Automatic
	// retirement remains additionally gated by GroomSplitLLM; StaleAge is the
	// minimum age of the newest in-content date.
	GroomStale    bool
	GroomStaleAge time.Duration
	// ClusterFanout bounds rendered sibling entries per repo scope. FanoutKeep is
	// the strict hysteresis boundary below which a prior grouping dissolves, and
	// ClusterDepthCap bounds recursive grouping/splitting.
	ClusterFanout     int
	ClusterFanoutKeep int
	ClusterDepthCap   int
}

// DefaultMemorySettings returns the off-by-default resolved settings.
func DefaultMemorySettings() MemorySettings {
	return MemorySettings{
		Disabled:               false,
		DefaultEnroll:          false,
		TokenBudget:            DefaultMemoryTokenBudget,
		MaxEntries:             DefaultMemoryMaxEntries,
		DistillAtTerminal:      false,
		DistillSuccesses:       false,
		DistillMaxPerJob:       DefaultMemoryDistillMaxPerJob,
		DistillAllJobs:         false,
		IngestAutoConfirm:      false,
		HarvestEnabled:         false,
		HarvestRuntime:         DefaultMemoryHarvestRuntime,
		HarvestModel:           DefaultMemoryHarvestModel,
		HarvestEffort:          DefaultMemoryHarvestEffort,
		HarvestMaxPerJob:       DefaultMemoryHarvestMaxPerJob,
		HarvestMaxJobsPerSweep: DefaultMemoryHarvestMaxJobsPerSweep,
		GroomSplitLLM:          DefaultMemoryGroomSplitLLM,
		GroomSplitLLMRuntime:   DefaultMemoryGroomSplitLLMRuntime,
		GroomSplitLLMModel:     DefaultMemoryGroomSplitLLMModel,
		GroomSplitLLMMaxPerRun: DefaultMemoryGroomSplitLLMMaxPerRun,
		GroomQuality:           DefaultMemoryGroomQuality,
		GroomQualityMaxPerRun:  DefaultMemoryGroomQualityMaxPerRun,
		GroomQualityMinAge:     DefaultMemoryGroomQualityMinAge,
		GroomLLMTotalMaxPerRun: DefaultMemoryGroomLLMTotalMaxPerRun,
		GroomStale:             DefaultMemoryGroomStale,
		GroomStaleAge:          DefaultMemoryGroomStaleAge,
		ClusterFanout:          DefaultMemoryClusterFanout,
		ClusterFanoutKeep:      DefaultMemoryClusterFanoutKeep,
		ClusterDepthCap:        DefaultMemoryClusterDepthCap,
	}
}

// LoadMemorySettings resolves the [memory] section knobs. An absent section (or
// an absent key) yields the documented default for that knob. Out-of-range or
// malformed values are rejected so `gitmoot config set` surfaces the error.
func LoadMemorySettings(paths Paths) (MemorySettings, error) {
	settings := DefaultMemorySettings()
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return MemorySettings{}, err
	}
	current := ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if current != "memory" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "disabled":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].disabled: %w", err)
			}
			settings.Disabled = parsed
		case "default_enroll":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].default_enroll: %w", err)
			}
			settings.DefaultEnroll = parsed
		case "token_budget":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].token_budget: %w", err)
			}
			settings.TokenBudget = parsed
		case "max_entries":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].max_entries: %w", err)
			}
			settings.MaxEntries = parsed
		case "distill_at_terminal":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].distill_at_terminal: %w", err)
			}
			settings.DistillAtTerminal = parsed
		case "distill_successes":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].distill_successes: %w", err)
			}
			settings.DistillSuccesses = parsed
		case "distill_max_per_job":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].distill_max_per_job: %w", err)
			}
			settings.DistillMaxPerJob = parsed
		case "distill_all_jobs":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].distill_all_jobs: %w", err)
			}
			settings.DistillAllJobs = parsed
		case "ingest_auto_confirm":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].ingest_auto_confirm: %w", err)
			}
			settings.IngestAutoConfirm = parsed
		case "harvest_enabled":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].harvest_enabled: %w", err)
			}
			settings.HarvestEnabled = parsed
		case "harvest_runtime":
			parsed, err := parseConfigString(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].harvest_runtime: %w", err)
			}
			settings.HarvestRuntime = strings.TrimSpace(parsed)
		case "harvest_model":
			parsed, err := parseConfigString(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].harvest_model: %w", err)
			}
			settings.HarvestModel = strings.TrimSpace(parsed)
		case "harvest_effort":
			parsed, err := parseConfigString(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].harvest_effort: %w", err)
			}
			settings.HarvestEffort = strings.TrimSpace(parsed)
		case "harvest_max_per_job":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].harvest_max_per_job: %w", err)
			}
			settings.HarvestMaxPerJob = parsed
		case "harvest_max_jobs_per_sweep":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].harvest_max_jobs_per_sweep: %w", err)
			}
			settings.HarvestMaxJobsPerSweep = parsed
		case "groom_split_llm":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_split_llm: %w", err)
			}
			settings.GroomSplitLLM = parsed
		case "groom_split_llm_runtime":
			parsed, err := parseConfigString(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_split_llm_runtime: %w", err)
			}
			settings.GroomSplitLLMRuntime = strings.TrimSpace(parsed)
		case "groom_split_llm_model":
			parsed, err := parseConfigString(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_split_llm_model: %w", err)
			}
			settings.GroomSplitLLMModel = strings.TrimSpace(parsed)
		case "groom_split_llm_max_per_run":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_split_llm_max_per_run: %w", err)
			}
			settings.GroomSplitLLMMaxPerRun = parsed
		case "groom_quality":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_quality: %w", err)
			}
			settings.GroomQuality = parsed
		case "groom_quality_max_per_run":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_quality_max_per_run: %w", err)
			}
			settings.GroomQualityMaxPerRun = parsed
		case "groom_quality_min_age":
			parsed, err := parseConfigString(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_quality_min_age: %w", err)
			}
			settings.GroomQualityMinAge, err = time.ParseDuration(strings.TrimSpace(parsed))
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_quality_min_age: %w", err)
			}
		case "groom_llm_total_max_per_run":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_llm_total_max_per_run: %w", err)
			}
			settings.GroomLLMTotalMaxPerRun = parsed
		case "groom_stale":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_stale: %w", err)
			}
			settings.GroomStale = parsed
		case "groom_stale_age":
			parsed, err := parseConfigString(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_stale_age: %w", err)
			}
			settings.GroomStaleAge, err = time.ParseDuration(strings.TrimSpace(parsed))
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].groom_stale_age: %w", err)
			}
		case "cluster_fanout":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].cluster_fanout: %w", err)
			}
			settings.ClusterFanout = parsed
		case "cluster_fanout_keep":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].cluster_fanout_keep: %w", err)
			}
			settings.ClusterFanoutKeep = parsed
		case "cluster_depth_cap":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].cluster_depth_cap: %w", err)
			}
			settings.ClusterDepthCap = parsed
		}
	}
	if err := validateMemorySettings(settings); err != nil {
		return MemorySettings{}, err
	}
	return settings, nil
}

func validateMemorySettings(s MemorySettings) error {
	if s.TokenBudget < 0 {
		return fmt.Errorf("memory.token_budget must be >= 0, got %d", s.TokenBudget)
	}
	if s.MaxEntries < 0 {
		return fmt.Errorf("memory.max_entries must be >= 0, got %d", s.MaxEntries)
	}
	if s.DistillMaxPerJob < 0 {
		return fmt.Errorf("memory.distill_max_per_job must be >= 0, got %d", s.DistillMaxPerJob)
	}
	if !configMemoryLLMRuntimeSupported(s.HarvestRuntime) {
		return fmt.Errorf("memory.harvest_runtime must be one of codex, claude, kimi, got %q", s.HarvestRuntime)
	}
	if strings.TrimSpace(s.HarvestEffort) == "" {
		return fmt.Errorf("memory.harvest_effort must not be empty")
	}
	if s.HarvestMaxPerJob < 1 {
		return fmt.Errorf("memory.harvest_max_per_job must be >= 1, got %d", s.HarvestMaxPerJob)
	}
	if s.HarvestMaxJobsPerSweep < 1 {
		return fmt.Errorf("memory.harvest_max_jobs_per_sweep must be >= 1, got %d", s.HarvestMaxJobsPerSweep)
	}
	if !configMemoryLLMRuntimeSupported(s.GroomSplitLLMRuntime) {
		return fmt.Errorf("memory.groom_split_llm_runtime must be one of codex, claude, kimi, got %q", s.GroomSplitLLMRuntime)
	}
	if s.GroomSplitLLMMaxPerRun < 1 {
		return fmt.Errorf("memory.groom_split_llm_max_per_run must be >= 1, got %d", s.GroomSplitLLMMaxPerRun)
	}
	if s.GroomQualityMaxPerRun < 1 {
		return fmt.Errorf("memory.groom_quality_max_per_run must be >= 1, got %d", s.GroomQualityMaxPerRun)
	}
	if s.GroomQualityMinAge <= 0 {
		return fmt.Errorf("memory.groom_quality_min_age must be > 0, got %s", s.GroomQualityMinAge)
	}
	if s.GroomLLMTotalMaxPerRun < 1 {
		return fmt.Errorf("memory.groom_llm_total_max_per_run must be >= 1, got %d", s.GroomLLMTotalMaxPerRun)
	}
	if s.GroomStaleAge <= 0 {
		return fmt.Errorf("memory.groom_stale_age must be > 0, got %s", s.GroomStaleAge)
	}
	if s.ClusterFanout < 2 {
		return fmt.Errorf("memory.cluster_fanout must be >= 2, got %d", s.ClusterFanout)
	}
	if s.ClusterFanoutKeep < 1 || s.ClusterFanoutKeep >= s.ClusterFanout {
		return fmt.Errorf("memory.cluster_fanout_keep must be >= 1 and < cluster_fanout, got %d", s.ClusterFanoutKeep)
	}
	if s.ClusterDepthCap < 1 || s.ClusterDepthCap > DefaultMemoryClusterDepthCap {
		return fmt.Errorf("memory.cluster_depth_cap must be between 1 and %d, got %d", DefaultMemoryClusterDepthCap, s.ClusterDepthCap)
	}
	return nil
}

func configMemoryLLMRuntimeSupported(name string) bool {
	for _, supported := range []string{runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime} {
		if name == supported {
			return true
		}
	}
	return false
}
