package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestPipelineInstallDefaultsIdempotentAndPreservesUserEdits(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	runInstallDefaults(t, home)
	runInstallDefaults(t, home)

	pipes, err := store.ListPipelines(context.Background())
	if err != nil {
		t.Fatalf("ListPipelines: %v", err)
	}
	if len(pipes) != 2 {
		t.Fatalf("default install created %d pipelines, want 2: %+v", len(pipes), pipes)
	}

	const custom = "name: memory-ingest-sweep\nrepo: owner/repo\nstages:\n  - id: custom\n    cmd: printf '%s' '{\"gitmoot_result\":{\"decision\":\"implemented\",\"summary\":\"custom\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[]}}'\n"
	if err := store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{
		Name: "memory-ingest-sweep", Repo: "owner/repo", SpecYAML: custom, SpecHash: pipeline.Hash([]byte(custom)),
	}); err != nil {
		t.Fatalf("customize pipeline: %v", err)
	}

	runInstallDefaults(t, home)
	rec, ok, err := store.GetPipeline(context.Background(), "memory-ingest-sweep")
	if err != nil || !ok {
		t.Fatalf("GetPipeline: ok=%v err=%v", ok, err)
	}
	if rec.SpecHash != pipeline.Hash([]byte(custom)) || !strings.Contains(rec.SpecYAML, "custom") {
		t.Fatalf("install-defaults clobbered user-edited pipeline:\n%s", rec.SpecYAML)
	}
}

func TestPipelineInstallDefaultsSchedulesFromMemoryPipelineSettings(t *testing.T) {
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	appendConfig(t, paths, `
[memory.pipelines]
repo = "owner/repo"
ingest_sweep = "2h"
ingest_sweep_jitter = "10m"
groom_propose = "off"
`)

	runInstallDefaults(t, home)

	ingest, ok, err := store.GetPipeline(context.Background(), "memory-ingest-sweep")
	if err != nil || !ok {
		t.Fatalf("GetPipeline ingest: ok=%v err=%v", ok, err)
	}
	if !ingest.Enabled || ingest.Interval != "2h" || ingest.Jitter != "10m" {
		t.Fatalf("ingest schedule = enabled=%v interval=%q jitter=%q, want enabled 2h/10m", ingest.Enabled, ingest.Interval, ingest.Jitter)
	}
	groom, ok, err := store.GetPipeline(context.Background(), "memory-groom-propose")
	if err != nil || !ok {
		t.Fatalf("GetPipeline groom: ok=%v err=%v", ok, err)
	}
	if groom.Enabled || groom.Interval != "" || groom.Jitter != "" {
		t.Fatalf("groom schedule = enabled=%v interval=%q jitter=%q, want unscheduled manual-only", groom.Enabled, groom.Interval, groom.Jitter)
	}
}

func TestDefaultMemoryIngestSweepNoSourcesSkip(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	t.Setenv(defaultMemoryPipelineBinEnv, buildGitmootTestBinary(t))
	runInstallDefaults(t, home)

	run := runDefaultPipelineToTerminal(t, home, store, "memory-ingest-sweep")
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded (halt=%s)", run.State, run.HaltReason)
	}
	sweep := stageRow(t, store, run.ID, "sweep")
	if sweep.State != pipeline.StageSucceeded || !strings.Contains(sweep.Summary, "no sources configured") {
		t.Fatalf("sweep stage = %+v, want no-sources success", sweep)
	}
	summarize := stageRow(t, store, run.ID, "summarize")
	if summarize.State != pipeline.StageSucceeded || !strings.Contains(summarize.Summary, "no sources configured") {
		t.Fatalf("summary stage = %+v, want no-sources success", summarize)
	}
}

func TestDefaultMemoryIngestSweepTwoSourcesE2E(t *testing.T) {
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	t.Setenv(defaultMemoryPipelineBinEnv, buildGitmootTestBinary(t))
	runInstallDefaults(t, home)
	assertDefaultIngestPipelineShape(t, store)

	srcA := t.TempDir()
	srcB := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcA, "alpha.md"), []byte("The pipeline ingest sweep records alpha release notes for owner repo.\n"), 0o644); err != nil {
		t.Fatalf("write source A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcB, "beta.md"), []byte("The pipeline ingest sweep records beta verification notes for owner repo.\n"), 0o644); err != nil {
		t.Fatalf("write source B: %v", err)
	}
	appendConfig(t, paths, `
[[memory.ingest]]
path = "`+filepath.ToSlash(srcA)+`"
agent = "lead"
repo = "owner/repo"
tier = "repo"

[[memory.ingest]]
path = "`+filepath.ToSlash(srcB)+`"
agent = "lead"
repo = "owner/repo"
tier = "repo"
`)

	run := runDefaultPipelineToTerminal(t, home, store, "memory-ingest-sweep")
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded (halt=%s)", run.State, run.HaltReason)
	}
	sweep := stageRow(t, store, run.ID, "sweep")
	if sweep.State != pipeline.StageSucceeded || !strings.Contains(sweep.Summary, "failed_sources=0") {
		t.Fatalf("sweep stage = %+v, want succeeded with zero failed sources", sweep)
	}
	stage := stageRow(t, store, run.ID, "summarize")
	if !strings.Contains(stage.Summary, "staged 2 observation(s)") {
		t.Fatalf("summary = %q, want two inserted observations", stage.Summary)
	}
	obs, err := store.ListMemoryObservations(context.Background(), "lead", "owner/repo")
	if err != nil {
		t.Fatalf("ListMemoryObservations: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("observations = %d, want 2: %+v", len(obs), obs)
	}
}

func TestDefaultMemoryIngestSweepAllSourcesFailIsLoud(t *testing.T) {
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	t.Setenv(defaultMemoryPipelineBinEnv, buildGitmootTestBinary(t))
	missing := filepath.Join(t.TempDir(), "missing")
	appendConfig(t, paths, `
[[memory.ingest]]
path = "`+filepath.ToSlash(missing)+`"
agent = "lead"
repo = "owner/repo"
tier = "repo"
`)
	runInstallDefaults(t, home)

	run := runDefaultPipelineToTerminal(t, home, store, "memory-ingest-sweep")
	if run.State != pipeline.RunFailed || run.HaltStage != "sweep" {
		t.Fatalf("run = state %s halt %q reason %q, want failed at sweep", run.State, run.HaltStage, run.HaltReason)
	}
	sweep := stageRow(t, store, run.ID, "sweep")
	if sweep.State != pipeline.StageFailed || !strings.Contains(sweep.Summary, "memory ingest sweep failed") {
		t.Fatalf("sweep stage = %+v, want loud failure", sweep)
	}
	summarize := stageRow(t, store, run.ID, "summarize")
	if summarize.State != pipeline.StageSkipped {
		t.Fatalf("summarize stage = %+v, want skipped after sweep failure", summarize)
	}
	job, err := store.GetJob(context.Background(), sweep.JobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", sweep.JobID, err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if len(payload.RawOutputs) == 0 || !strings.Contains(payload.RawOutputs[0], missing) || !strings.Contains(payload.RawOutputs[0], `"error"`) {
		t.Fatalf("sweep raw output should include per-source error for %s:\n%v", missing, payload.RawOutputs)
	}
}

func TestDefaultMemoryGroomProposeE2E(t *testing.T) {
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	t.Setenv(defaultMemoryPipelineBinEnv, buildGitmootTestBinary(t))
	appendConfig(t, paths, `
[memory.pipelines]
repo = "owner/repo"
	`)
	groomSeed(t, store)
	parentID := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "owner/repo", "repo", "pipeline-brick",
		"**First pipeline story**\nThe first pipeline story has substantive implementation detail.\n\n**Second pipeline story**\nThe second pipeline story has substantive implementation detail.")
	runInstallDefaults(t, home)
	rec, ok, err := store.GetPipeline(context.Background(), "memory-groom-propose")
	if err != nil || !ok {
		t.Fatalf("GetPipeline memory-groom-propose: ok=%v err=%v", ok, err)
	}
	spec, err := pipeline.Load([]byte(rec.SpecYAML))
	if err != nil {
		t.Fatalf("load groom pipeline: %v", err)
	}
	if len(spec.Stages) != 3 || spec.Stages[0].ID != "split" || spec.Stages[1].ID != "propose" || spec.Stages[2].ID != "summarize" ||
		len(spec.Stages[1].Needs) != 1 || spec.Stages[1].Needs[0] != "split" {
		t.Fatalf("groom stages = %+v, want split -> propose -> summarize", spec.Stages)
	}

	run := runDefaultPipelineToTerminal(t, home, store, "memory-groom-propose")
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded (halt=%s)", run.State, run.HaltReason)
	}
	stage := stageRow(t, store, run.ID, "summarize")
	if !strings.Contains(stage.Summary, "split 1 brick(s)") || !strings.Contains(stage.Summary, "proposed 3 retirement(s)") || !strings.Contains(stage.Summary, "1 rewrite flag") {
		t.Fatalf("summary = %q, want groom proposal counts", stage.Summary)
	}
	splitStage := stageRow(t, store, run.ID, "split")
	if splitStage.State != pipeline.StageSucceeded || !strings.Contains(splitStage.Summary, "split 1 brick(s)") {
		t.Fatalf("split stage = %+v", splitStage)
	}
	active, err := store.ListConfirmedMemoriesForVault(context.Background(), "")
	if err != nil {
		t.Fatalf("list active after pipeline: %v", err)
	}
	for _, row := range active {
		if row.ID == parentID {
			t.Fatal("pipeline proposal ran without first superseding the split parent")
		}
	}
	plan := filepath.Join(paths.Home, "evals", "memory-pipelines", run.ID, "groom-plan.json")
	if _, err := readGroomPlan(plan); err != nil {
		t.Fatalf("read groom plan: %v\nfiles: %v", err, listRelativeFiles(t, filepath.Join(paths.Home, "evals", "memory-pipelines")))
	}
}

func assertDefaultIngestPipelineShape(t *testing.T, store *db.Store) {
	t.Helper()
	rec, ok, err := store.GetPipeline(context.Background(), "memory-ingest-sweep")
	if err != nil || !ok {
		t.Fatalf("GetPipeline memory-ingest-sweep: ok=%v err=%v", ok, err)
	}
	spec, err := pipeline.Load([]byte(rec.SpecYAML))
	if err != nil {
		t.Fatalf("load default ingest spec: %v", err)
	}
	if len(spec.Stages) != 2 || spec.Stages[0].ID != "sweep" || spec.Stages[1].ID != "summarize" {
		t.Fatalf("default ingest stages = %+v, want sweep -> summarize", spec.Stages)
	}
	if len(spec.Stages[1].Needs) != 1 || spec.Stages[1].Needs[0] != "sweep" {
		t.Fatalf("summary needs = %+v, want [sweep]", spec.Stages[1].Needs)
	}
	if strings.Contains(rec.SpecYAML, "ingest-1") || strings.Contains(rec.SpecYAML, "memory ingest --agent") {
		t.Fatalf("default ingest spec still freezes configured sources:\n%s", rec.SpecYAML)
	}
}

func TestPipelineInstallDefaultsReportsConfigValidationErrors(t *testing.T) {
	home, paths, _ := heartbeatLoopE2EHome(t)
	appendConfig(t, paths, `
[[memory.ingest]]
path = "/notes"
agent = "lead"
tier = "bogus"
`)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"pipeline", "install-defaults", "--home", home}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("install-defaults exit = %d, want 1 (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "tier must be repo or general") {
		t.Fatalf("stderr missing validation detail:\n%s", stderr.String())
	}
}

func runInstallDefaults(t *testing.T, home string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "install-defaults", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("install-defaults exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
}

func runDefaultPipelineToTerminal(t *testing.T, home string, store *db.Store, name string) db.PipelineRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "run", name, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline run %s exit=%d stderr=%s", name, code, stderr.String())
	}
	runID := strings.TrimSpace(stdout.String())
	worker := defaultJobWorker(store, io.Discard, home)
	enqueue := newPipelineStageEnqueuer(store, home)
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		if err := runEnabledRepoWorkerTicks(context.Background(), store, worker, 1, io.Discard, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(context.Background(), store, enqueue, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		run, ok, err := store.GetPipelineRun(context.Background(), runID)
		if err != nil || !ok {
			t.Fatalf("GetPipelineRun(%s): ok=%v err=%v", runID, ok, err)
		}
		if run.State != pipeline.RunRunning {
			return run
		}
	}
	run, _, _ := store.GetPipelineRun(context.Background(), runID)
	return run
}

func appendConfig(t *testing.T, paths config.Paths, body string) {
	t.Helper()
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func buildGitmootTestBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "gitmoot")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "./cmd/gitmoot")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gitmoot test binary: %v\n%s", err, string(output))
	}
	return bin
}

func listRelativeFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files
}
