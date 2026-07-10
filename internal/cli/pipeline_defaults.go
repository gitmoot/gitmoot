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
	stages := make([]pipeline.Stage, 0, len(settings.IngestSources)+1)
	needs := make([]string, 0, len(settings.IngestSources))
	for i, source := range settings.IngestSources {
		id := fmt.Sprintf("ingest-%d", i+1)
		stages = append(stages, pipeline.Stage{
			ID:  id,
			Cmd: memoryIngestStageCommand(paths, rawHome, source, i+1),
		})
		needs = append(needs, id)
	}
	stages = append(stages, pipeline.Stage{
		ID:    "summarize",
		Cmd:   memoryIngestSummaryStageCommand(paths, len(settings.IngestSources)),
		Needs: needs,
	})
	spec := pipeline.Spec{
		Name:   defaultMemoryIngestSweepPipeline,
		Repo:   repo,
		Stages: stages,
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
			{ID: "propose", Cmd: memoryGroomProposeStageCommand(paths, rawHome)},
			{ID: "summarize", Cmd: memoryGroomSummaryStageCommand(paths), Needs: []string{"propose"}},
		},
	}
	enabled := settings.GroomProposeInterval != ""
	if enabled {
		spec.Schedule = &pipeline.Schedule{Interval: settings.GroomProposeInterval, Jitter: settings.GroomProposeJitter}
	}
	return defaultPipelineDefinition{name: spec.Name, spec: spec, enabled: enabled}
}

func memoryIngestStageCommand(paths config.Paths, rawHome string, source config.MemoryIngestSource, index int) string {
	homeArgs := memoryPipelineHomeArgs(rawHome)
	repoArgs := ""
	if strings.TrimSpace(source.Repo) != "" {
		repoArgs = " --repo " + memoryPipelineShellQuote(source.Repo)
	}
	return strings.Join([]string{
		"set -eu",
		memoryPipelineRunDirScript(paths),
		"out_file=\"$run_dir/ingest-" + fmt.Sprint(index) + ".json\"",
		"err_file=\"$run_dir/ingest-" + fmt.Sprint(index) + ".err\"",
		"if " + memoryPipelineShellQuote(defaultPipelineGitmootBinary()) + " memory ingest" + homeArgs + " --agent " + memoryPipelineShellQuote(source.Agent) + " --tier " + memoryPipelineShellQuote(source.Tier) + repoArgs + " --json " + memoryPipelineShellQuote(source.Path) + " > \"$out_file\" 2> \"$err_file\"; then",
		"  inserted=$(json_num \"$out_file\" inserted)",
		"  inserted=${inserted:-0}",
		"  printf '%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"memory ingest source " + fmt.Sprint(index) + " staged '\"$inserted\"' observation(s)\",\"findings\":[],\"changes_made\":[\"wrote run-scoped ingest summary\"],\"tests_run\":[\"gitmoot memory ingest --json\"],\"needs\":[],\"delegations\":[]}}'",
		"else",
		"  printf '%s' '{\"gitmoot_result\":{\"decision\":\"failed\",\"summary\":\"memory ingest source " + fmt.Sprint(index) + " failed; see run-scoped stderr\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"gitmoot memory ingest --json\"],\"needs\":[],\"delegations\":[]}}'",
		"fi",
	}, "\n")
}

func memoryIngestSummaryStageCommand(paths config.Paths, sources int) string {
	if sources == 0 {
		return strings.Join([]string{
			"set -eu",
			memoryPipelineRunDirScript(paths),
			"printf '%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"memory ingest sweep skipped: no sources configured\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[]}}'",
		}, "\n")
	}
	return strings.Join([]string{
		"set -eu",
		memoryPipelineRunDirScript(paths),
		"files=0",
		"chunks=0",
		"inserted=0",
		"deduped=0",
		"rejected=0",
		"seen=0",
		"for f in \"$run_dir\"/ingest-*.json; do",
		"  [ -f \"$f\" ] || continue",
		"  seen=$((seen + 1))",
		"  files=$((files + $(json_num \"$f\" files)))",
		"  chunks=$((chunks + $(json_num \"$f\" chunks)))",
		"  inserted=$((inserted + $(json_num \"$f\" inserted)))",
		"  deduped=$((deduped + $(json_num \"$f\" deduped)))",
		"  rejected=$((rejected + $(json_num \"$f\" rejected)))",
		"done",
		"printf '%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"memory ingest sweep staged '\"$inserted\"' observation(s) from '\"$seen\"' source(s), '\"$files\"' file(s), '\"$chunks\"' chunk(s); deduped='\"$deduped\"' rejected='\"$rejected\"'\",\"findings\":[],\"changes_made\":[\"aggregated memory ingest sweep counts\"],\"tests_run\":[\"gitmoot memory ingest --json\"],\"needs\":[],\"delegations\":[]}}'",
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
		"  printf '%s' '{\"gitmoot_result\":{\"decision\":\"failed\",\"summary\":\"memory groom proposal failed; see run-scoped stderr\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"gitmoot memory groom --propose --json\"],\"needs\":[],\"delegations\":[]}}'",
		"fi",
	}, "\n")
}

func memoryGroomSummaryStageCommand(paths config.Paths) string {
	return strings.Join([]string{
		"set -eu",
		memoryPipelineRunDirScript(paths),
		"summary_file=\"$run_dir/groom-propose.json\"",
		"plan_file=\"$run_dir/groom-plan.json\"",
		"proposals=$(json_stat \"$summary_file\" proposed_retirements)",
		"rewrites=$(json_stat \"$summary_file\" rewrite_flags)",
		"total=$(json_stat \"$summary_file\" total_memories)",
		"proposals=${proposals:-0}",
		"rewrites=${rewrites:-0}",
		"total=${total:-0}",
		"printf '%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"memory groom proposed '\"$proposals\"' retirement(s) and '\"$rewrites\"' rewrite flag(s) across '\"$total\"' confirmed memory item(s)\",\"findings\":[],\"changes_made\":[\"review plan at '\"$plan_file\"'\"],\"tests_run\":[\"gitmoot memory groom --propose --json\"],\"needs\":[],\"delegations\":[]}}'",
	}, "\n")
}

func memoryPipelineRunDirScript(paths config.Paths) string {
	return strings.Join([]string{
		"json_num() { awk -v key=\"\\\"$2\\\"\" 'index($0,key) { value=$0; sub(/^.*: */, \"\", value); sub(/,.*/, \"\", value); gsub(/[^0-9-]/, \"\", value); found=1; print value; exit } END { if (!found) print 0 }' \"$1\"; }",
		"json_stat() { awk -v key=\"\\\"$2\\\"\" '/\"stats\"[[:space:]]*:/ { in_stats=1; next } in_stats && /}/ { exit } in_stats && index($0,key) { value=$0; sub(/^.*: */, \"\", value); sub(/,.*/, \"\", value); gsub(/[^0-9-]/, \"\", value); found=1; print value; exit } END { if (!found) print 0 }' \"$1\"; }",
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
