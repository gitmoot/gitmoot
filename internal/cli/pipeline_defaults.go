package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	yaml "gopkg.in/yaml.v3"
)

const (
	defaultMemoryIngestSweepPipeline  = "memory-ingest-sweep"
	defaultMemoryGroomProposePipeline = "memory-groom-propose"
	defaultMemoryPipelineBinEnv       = "GITMOOT_PIPELINE_BIN"
)

type defaultPipelineInstallResult struct {
	Installed []string
	Skipped   []string
}

type defaultPipelineDefinition struct {
	name    string
	spec    pipeline.Spec
	enabled bool
}

func installDefaultMemoryPipelines(ctx context.Context, store *db.Store, paths config.Paths, rawHome string) (defaultPipelineInstallResult, error) {
	settings, err := config.LoadMemoryPipelineSettings(paths)
	if err != nil {
		return defaultPipelineInstallResult{}, err
	}
	repo, err := defaultMemoryPipelineRepo(ctx, store, settings)
	if err != nil {
		return defaultPipelineInstallResult{}, err
	}
	definitions := []defaultPipelineDefinition{
		renderMemoryIngestSweepPipeline(settings, paths, rawHome, repo),
		renderMemoryGroomProposePipeline(settings, paths, rawHome, repo),
	}
	var result defaultPipelineInstallResult
	for _, def := range definitions {
		_, found, err := store.GetPipeline(ctx, def.name)
		if err != nil {
			return result, err
		}
		if found {
			result.Skipped = append(result.Skipped, def.name)
			continue
		}
		raw, err := yaml.Marshal(def.spec)
		if err != nil {
			return result, fmt.Errorf("render default pipeline %s: %w", def.name, err)
		}
		loaded, err := pipeline.Load(raw)
		if err != nil {
			return result, fmt.Errorf("validate default pipeline %s: %w", def.name, err)
		}
		record := db.Pipeline{
			Name:     loaded.Name,
			Repo:     loaded.Repo,
			SpecYAML: string(raw),
			SpecHash: pipeline.Hash(raw),
			Enabled:  def.enabled,
		}
		if loaded.Schedule != nil {
			record.Interval = loaded.Schedule.Interval
			record.Jitter = loaded.Schedule.Jitter
		}
		if err := store.CreateOrUpdatePipeline(ctx, record); err != nil {
			return result, err
		}
		if err := store.UpsertAgent(ctx, pipelineRunnerAgent(pipelineRunnerAgentName(loaded.Name), loaded.Repo)); err != nil {
			return result, err
		}
		result.Installed = append(result.Installed, loaded.Name)
	}
	return result, nil
}

func installDefaultMemoryPipelinesForDaemon(ctx context.Context, store *db.Store, paths config.Paths, rawHome string, stdout io.Writer) {
	result, err := installDefaultMemoryPipelines(ctx, store, paths, rawHome)
	if err != nil {
		writeLine(stdout, "default memory pipeline install error: %s", err)
		return
	}
	for _, name := range result.Installed {
		writeLine(stdout, "installed default memory pipeline %s", name)
	}
}

func defaultMemoryPipelineRepo(ctx context.Context, store *db.Store, settings config.MemoryPipelineSettings) (string, error) {
	if repo := strings.TrimSpace(settings.Repo); repo != "" {
		parsed, err := daemon.ParseRepository(repo)
		if err != nil {
			return "", fmt.Errorf("memory.pipelines.repo: %w", err)
		}
		return parsed.FullName(), nil
	}
	for _, source := range settings.IngestSources {
		if repo := strings.TrimSpace(source.Repo); repo != "" {
			parsed, err := daemon.ParseRepository(repo)
			if err != nil {
				return "", fmt.Errorf("memory.ingest repo %q: %w", repo, err)
			}
			return parsed.FullName(), nil
		}
	}
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return "", err
	}
	for _, repo := range repos {
		if repo.Enabled && strings.TrimSpace(repo.CheckoutPath) != "" {
			return repo.FullName(), nil
		}
	}
	return "", nil
}

func renderMemoryIngestSweepPipeline(settings config.MemoryPipelineSettings, paths config.Paths, rawHome string, repo string) defaultPipelineDefinition {
	spec := pipeline.Spec{
		Name: defaultMemoryIngestSweepPipeline,
		Repo: repo,
		Stages: []pipeline.Stage{
			{ID: "sweep", Cmd: memoryIngestSweepStageCommand(paths, rawHome)},
			{ID: "summarize", Cmd: memoryIngestSummaryStageCommand(paths), Needs: []string{"sweep"}},
		},
	}
	enabled := settings.IngestSweepInterval != ""
	if enabled {
		spec.Schedule = &pipeline.Schedule{Interval: settings.IngestSweepInterval, Jitter: settings.IngestSweepJitter}
	}
	return defaultPipelineDefinition{name: spec.Name, spec: spec, enabled: enabled}
}

func renderMemoryGroomProposePipeline(settings config.MemoryPipelineSettings, paths config.Paths, rawHome string, repo string) defaultPipelineDefinition {
	spec := pipeline.Spec{
		Name: defaultMemoryGroomProposePipeline,
		Repo: repo,
		Stages: []pipeline.Stage{
			{ID: "split", Cmd: memoryGroomSplitStageCommand(paths, rawHome)},
			{ID: "propose", Cmd: memoryGroomProposeStageCommand(paths, rawHome), Needs: []string{"split"}},
			{ID: "summarize", Cmd: memoryGroomSummaryStageCommand(paths), Needs: []string{"propose"}},
		},
	}
	enabled := settings.GroomProposeInterval != ""
	if enabled {
		spec.Schedule = &pipeline.Schedule{Interval: settings.GroomProposeInterval, Jitter: settings.GroomProposeJitter}
	}
	return defaultPipelineDefinition{name: spec.Name, spec: spec, enabled: enabled}
}

func memoryGroomSplitStageCommand(paths config.Paths, rawHome string) string {
	homeArgs := memoryPipelineHomeArgs(rawHome)
	return strings.Join([]string{
		"set -eu",
		memoryPipelineRunDirScript(paths),
		"summary_file=\"$run_dir/groom-split.json\"",
		"err_file=\"$run_dir/groom-split.err\"",
		"if " + memoryPipelineShellQuote(defaultPipelineGitmootBinary()) + " memory groom" + homeArgs + " --split --json > \"$summary_file\" 2> \"$err_file\"; then",
		"  applied=$(json_num \"$summary_file\" applied)",
		"  printf '%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"memory groom split '" + "\"$applied\"" + "' brick(s)\",\"findings\":[],\"changes_made\":[\"auto-applied lossless memory splits\"],\"tests_run\":[\"gitmoot memory groom --split --json\"],\"needs\":[],\"delegations\":[]}}'",
		"else",
		"  if [ -s \"$err_file\" ]; then cat \"$err_file\"; fi",
		"  printf '\\n%s' '{\"gitmoot_result\":{\"decision\":\"failed\",\"summary\":\"memory groom split failed; see run-scoped stderr\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"gitmoot memory groom --split --json\"],\"needs\":[],\"delegations\":[]}}'",
		"fi",
	}, "\n")
}

func memoryIngestSweepStageCommand(paths config.Paths, rawHome string) string {
	homeArgs := memoryPipelineHomeArgs(rawHome)
	return strings.Join([]string{
		"set -eu",
		memoryPipelineRunDirScript(paths),
		"out_file=\"$run_dir/ingest-sweep.json\"",
		"err_file=\"$run_dir/ingest-sweep.err\"",
		"if " + memoryPipelineShellQuote(defaultPipelineGitmootBinary()) + " memory ingest sweep" + homeArgs + " --json > \"$out_file\" 2> \"$err_file\"; then",
		"  cat \"$out_file\"",
		"  sources=$(json_num \"$out_file\" sources)",
		"  inserted=$(json_num \"$out_file\" inserted)",
		"  deduped=$(json_num \"$out_file\" deduped)",
		"  rejected=$(json_num \"$out_file\" rejected)",
		"  failed=$(json_num \"$out_file\" failed)",
		"  if [ \"$sources\" -eq 0 ]; then",
		"    summary=\"memory ingest sweep skipped: no sources configured\"",
		"  else",
		"    summary=\"memory ingest sweep staged ${inserted} observation(s) from ${sources} source(s); deduped=${deduped} rejected=${rejected} failed_sources=${failed}\"",
		"  fi",
		"  printf '\\n%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"'\"$summary\"'\",\"findings\":[],\"changes_made\":[\"wrote run-scoped ingest sweep JSON\"],\"tests_run\":[\"gitmoot memory ingest sweep --json\"],\"needs\":[],\"delegations\":[]}}'",
		"else",
		"  if [ -s \"$out_file\" ]; then cat \"$out_file\"; fi",
		"  if [ -s \"$err_file\" ]; then cat \"$err_file\"; fi",
		"  sources=$(json_num \"$out_file\" sources)",
		"  inserted=$(json_num \"$out_file\" inserted)",
		"  failed=$(json_num \"$out_file\" failed)",
		"  printf '\\n%s' '{\"gitmoot_result\":{\"decision\":\"failed\",\"summary\":\"memory ingest sweep failed; sources='\"$sources\"' failed_sources='\"$failed\"' inserted='\"$inserted\"'; see run-scoped JSON/stderr\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"gitmoot memory ingest sweep --json\"],\"needs\":[],\"delegations\":[]}}'",
		"fi",
	}, "\n")
}

func memoryIngestSummaryStageCommand(paths config.Paths) string {
	return strings.Join([]string{
		"set -eu",
		memoryPipelineRunDirScript(paths),
		"summary_file=\"$run_dir/ingest-sweep.json\"",
		"if [ -s \"$summary_file\" ]; then cat \"$summary_file\"; fi",
		"sources=$(json_num \"$summary_file\" sources)",
		"succeeded=$(json_num \"$summary_file\" succeeded)",
		"failed=$(json_num \"$summary_file\" failed)",
		"files=$(json_num \"$summary_file\" files)",
		"chunks=$(json_num \"$summary_file\" chunks)",
		"inserted=$(json_num \"$summary_file\" inserted)",
		"deduped=$(json_num \"$summary_file\" deduped)",
		"rejected=$(json_num \"$summary_file\" rejected)",
		"if [ \"$sources\" -eq 0 ]; then",
		"  summary=\"memory ingest sweep skipped: no sources configured\"",
		"else",
		"  summary=\"memory ingest sweep staged ${inserted} observation(s) from ${succeeded}/${sources} successful source(s), ${files} file(s), ${chunks} chunk(s); deduped=${deduped} rejected=${rejected} failed_sources=${failed}\"",
		"fi",
		"printf '\\n%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"'\"$summary\"'\",\"findings\":[],\"changes_made\":[\"aggregated memory ingest sweep counts\"],\"tests_run\":[\"gitmoot memory ingest sweep --json\"],\"needs\":[],\"delegations\":[]}}'",
	}, "\n")
}

func memoryGroomProposeStageCommand(paths config.Paths, rawHome string) string {
	homeArgs := memoryPipelineHomeArgs(rawHome)
	return strings.Join([]string{
		"set -eu",
		memoryPipelineRunDirScript(paths),
		"plan_file=\"$run_dir/groom-plan.json\"",
		"summary_file=\"$run_dir/groom-propose.json\"",
		"err_file=\"$run_dir/groom-propose.err\"",
		"if " + memoryPipelineShellQuote(defaultPipelineGitmootBinary()) + " memory groom" + homeArgs + " --propose --out \"$plan_file\" --json > \"$summary_file\" 2> \"$err_file\"; then",
		"  printf '%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"memory groom proposal written\",\"findings\":[],\"changes_made\":[\"wrote run-scoped groom proposal\"],\"tests_run\":[\"gitmoot memory groom --propose --json\"],\"needs\":[],\"delegations\":[]}}'",
		"else",
		"  if [ -s \"$err_file\" ]; then cat \"$err_file\"; fi",
		"  printf '\\n%s' '{\"gitmoot_result\":{\"decision\":\"failed\",\"summary\":\"memory groom proposal failed; see run-scoped stderr\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"gitmoot memory groom --propose --json\"],\"needs\":[],\"delegations\":[]}}'",
		"fi",
	}, "\n")
}

func memoryGroomSummaryStageCommand(paths config.Paths) string {
	return strings.Join([]string{
		"set -eu",
		memoryPipelineRunDirScript(paths),
		"summary_file=\"$run_dir/groom-propose.json\"",
		"split_file=\"$run_dir/groom-split.json\"",
		"plan_file=\"$run_dir/groom-plan.json\"",
		"splits=$(json_num \"$split_file\" applied)",
		"proposals=$(json_stat \"$summary_file\" proposed_retirements)",
		"rewrites=$(json_stat \"$summary_file\" rewrite_flags)",
		"total=$(json_stat \"$summary_file\" total_memories)",
		"splits=${splits:-0}",
		"proposals=${proposals:-0}",
		"rewrites=${rewrites:-0}",
		"total=${total:-0}",
		"printf '%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"memory groom split '\"$splits\"' brick(s), then proposed '\"$proposals\"' retirement(s) and '\"$rewrites\"' rewrite flag(s) across '\"$total\"' confirmed memory item(s)\",\"findings\":[],\"changes_made\":[\"review plan at '\"$plan_file\"'\"],\"tests_run\":[\"gitmoot memory groom --split --json\",\"gitmoot memory groom --propose --json\"],\"needs\":[],\"delegations\":[]}}'",
	}, "\n")
}

func memoryPipelineRunDirScript(paths config.Paths) string {
	return strings.Join([]string{
		"json_num() { if [ ! -f \"$1\" ]; then printf '0\\n'; return 0; fi; awk -v key=\"\\\"$2\\\"\" 'index($0,key) { value=$0; sub(/^.*: */, \"\", value); sub(/,.*/, \"\", value); gsub(/[^0-9-]/, \"\", value); found=1; print value; exit } END { if (!found) print 0 }' \"$1\"; }",
		"json_stat() { if [ ! -f \"$1\" ]; then printf '0\\n'; return 0; fi; awk -v key=\"\\\"$2\\\"\" '/\"stats\"[[:space:]]*:/ { in_stats=1; next } in_stats && /}/ { exit } in_stats && index($0,key) { value=$0; sub(/^.*: */, \"\", value); sub(/,.*/, \"\", value); gsub(/[^0-9-]/, \"\", value); found=1; print value; exit } END { if (!found) print 0 }' \"$1\"; }",
		"prompt=${1:-}",
		"run_id=$(printf '%s\\n' \"$prompt\" | sed -n 's/.*pipeline [^[:space:]]* run \\([^[:space:]]*\\) stage .*/\\1/p' | head -n 1)",
		"if [ -z \"$run_id\" ]; then run_id=manual; fi",
		"run_dir=" + memoryPipelineShellQuote(filepath.Join(paths.Home, "evals", "memory-pipelines")) + "/$run_id",
		"mkdir -p \"$run_dir\"",
	}, "\n")
}

func memoryPipelineHomeArgs(rawHome string) string {
	rawHome = strings.TrimSpace(rawHome)
	if rawHome == "" {
		return ""
	}
	return " --home " + memoryPipelineShellQuote(rawHome)
}

func defaultPipelineGitmootBinary() string {
	if path := strings.TrimSpace(os.Getenv(defaultMemoryPipelineBinEnv)); path != "" {
		return path
	}
	if exe, err := os.Executable(); err == nil {
		base := strings.TrimSpace(exe)
		if base != "" && !strings.HasSuffix(base, ".test") {
			return base
		}
	}
	return "gitmoot"
}

func memoryPipelineShellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func installDefaultsEnabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "manual-only"
}
