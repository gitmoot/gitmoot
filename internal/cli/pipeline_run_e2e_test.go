package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/pipeline"
)

// pipelineStageResultCmd is a shell command that ignores its input and echoes a
// valid gitmoot_result with the given decision (and, for a block, the given
// needs). It is the SHELL-runtime session body a stage job runs as
// `sh -c <cmd> gitmoot <prompt>`, so the whole pipeline chain runs with NO LLM and
// NO network — fully deterministic offline (the #446/#533 shell-runtime idiom).
func pipelineStageResultCmd(decision, summary string, needs []string) string {
	needsJSON := "[]"
	if len(needs) > 0 {
		quoted := make([]string, 0, len(needs))
		for _, n := range needs {
			quoted = append(quoted, `"`+n+`"`)
		}
		needsJSON = "[" + strings.Join(quoted, ",") + "]"
	}
	return `printf '%s' '{"gitmoot_result":{"decision":"` + decision + `","summary":"` + summary +
		`","findings":[],"changes_made":[],"tests_run":[],"needs":` + needsJSON + `,"delegations":[]}}'`
}

// TestPipelineBlockedParkE2E is the full-chain, NO-LLM, deterministic blocked-park
// E2E (#681). It drives the REAL chain a daemon iteration runs, end to end:
//
//	`pipeline add` (writes the registry row + the hidden shell runner agent)
//	  -> `pipeline run` (creates the run, enqueues the ready root stage)
//	    -> the REAL worker tick CLAIMS + RUNS each queued stage job through the
//	       REAL shell adapter to its terminal decision
//	      -> runPipelineScanOnce FOLDS each settled stage by DECISION and advances
//
// The middle stage returns a blocked result with needs: the run must PARK blocked
// with the needs persisted at the run level, the blocked stage marked blocked, and
// the third stage NEVER enqueued (no stage job, marked skipped). It goes red if any
// link breaks: not-enqueued, worker-didn't-run, decision-misfolded, or the third
// stage leaking a job.
func TestPipelineBlockedParkE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)

	// A real managed (enabled + checked-out) repo so the worker's checkout
	// validation passes and it will claim the stage jobs.
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	source := pipelineStageResultCmd("approved", "source ok", nil)
	score := pipelineStageResultCmd("blocked", "score needs a secret", []string{"R2 token"})
	deploy := pipelineStageResultCmd("approved", "deploy ok", nil)
	specYAML := "name: deploy-flow\nrepo: owner/repo\nstages:\n" +
		pipelineE2EStage("source", source, "") +
		pipelineE2EStage("score", score, "source") +
		pipelineE2EStage("deploy", deploy, "score")
	specFile := writeSpec(t, specYAML)

	// `pipeline add` creates the registry row + the hidden pipeline-<name>-runner
	// shell agent the worker resolves for each stage job.
	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}

	// `pipeline run` creates the run and enqueues the root stage; it prints the run id.
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "deploy-flow", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())
	if runID == "" {
		t.Fatalf("pipeline run printed no run id (stderr=%s)", errBuf.String())
	}

	// Drive the real worker tick + advancer scan until the run leaves running.
	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		if err := runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, now); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		run, _, err := store.GetPipelineRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetPipelineRun: %v", err)
		}
		if run.State != pipeline.RunRunning {
			break
		}
	}

	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetPipelineRun(%s): ok=%v err=%v", runID, ok, err)
	}
	if run.State != pipeline.RunBlocked {
		t.Fatalf("run state = %s, want blocked", run.State)
	}
	if run.HaltStage != "score" {
		t.Fatalf("halt_stage = %q, want score", run.HaltStage)
	}
	if got := decodePipelineNeeds(run.NeedsJSON); len(got) != 1 || got[0] != "R2 token" {
		t.Fatalf("run needs = %v, want [R2 token]", got)
	}

	src := stageRow(t, store, runID, "source")
	if src.State != pipeline.StageSucceeded {
		t.Fatalf("stage source = %s, want succeeded", src.State)
	}
	scr := stageRow(t, store, runID, "score")
	if scr.State != pipeline.StageBlocked {
		t.Fatalf("stage score = %s, want blocked", scr.State)
	}
	if got := decodePipelineNeeds(scr.NeedsJSON); len(got) != 1 || got[0] != "R2 token" {
		t.Fatalf("stage score needs = %v, want [R2 token]", got)
	}
	dep := stageRow(t, store, runID, "deploy")
	if dep.JobID != "" {
		t.Fatalf("stage deploy job = %q, want NEVER enqueued", dep.JobID)
	}
	if dep.State != pipeline.StageSkipped {
		t.Fatalf("stage deploy = %s, want skipped", dep.State)
	}

	// The funnel renders the parked run with the blocking needs inline.
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "show", runID, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline show run exit=%d stderr=%s", code, errBuf.String())
	}
	funnel := out.String()
	if !strings.Contains(funnel, "source OK -> score BLOCKED (needs: R2 token) -> deploy SKIPPED") {
		t.Fatalf("funnel missing expected line:\n%s", funnel)
	}
	if !strings.Contains(funnel, "state: blocked") {
		t.Fatalf("funnel missing state: blocked:\n%s", funnel)
	}
}

// TestPipelineSkippedAdvancesE2E drives the real shell worker and pipeline scan
// through a no-work stage. skipped folds to the existing succeeded stage state,
// carries the trusted marker, and allows the downstream shell stage to run.
func TestPipelineSkippedAdvancesE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	scan := pipelineStageResultCmd("skipped", "no new replies today", nil)
	notify := pipelineStageResultCmd("approved", "downstream ran", nil)
	specYAML := "name: skipped-flow\nrepo: owner/repo\nstages:\n" +
		pipelineE2EStage("scan", scan, "") +
		pipelineE2EStage("notify", notify, "scan")
	specFile := writeSpec(t, specYAML)

	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "skipped-flow", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())

	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		if err := runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, now); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		run, _, err := store.GetPipelineRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetPipelineRun: %v", err)
		}
		if run.State != pipeline.RunRunning {
			break
		}
	}

	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetPipelineRun(%s): ok=%v err=%v", runID, ok, err)
	}
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run = %s, want succeeded", run.State)
	}
	if got := stageRow(t, store, runID, "scan"); got.State != pipeline.StageSucceeded || got.Summary != "[skipped: no work] no new replies today" {
		t.Fatalf("scan stage = %+v", got)
	}
	if got := stageRow(t, store, runID, "notify"); got.State != pipeline.StageSucceeded || got.Summary != "downstream ran" {
		t.Fatalf("notify stage = %+v", got)
	}
}

// pipelineE2EStage renders one stage block with the shell cmd as a literal YAML
// block scalar (so the JSON-bearing single-quoted command survives YAML parsing).
func pipelineE2EStage(id, cmd, needs string) string {
	block := "  - id: " + id + "\n    cmd: |\n      " + cmd + "\n"
	if needs != "" {
		block += "    needs: [" + needs + "]\n"
	}
	return block
}
