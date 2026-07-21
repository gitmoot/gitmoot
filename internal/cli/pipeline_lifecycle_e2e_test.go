package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

// TestPipelineRestartRecoversE2E is the daemon-restart E2E (#681): a run driven
// PARTWAY by one advancer/worker instance is completed by a FRESH instance that
// re-opens the same SQLite file, proving the advancer holds NO in-memory run state —
// it recovers everything from the persisted run/stage/job rows, exactly as a real
// daemon restart mid-run must. NO LLM, NO network (shell-runtime stages).
func TestPipelineRestartRecoversE2E(t *testing.T) {
	ctx := context.Background()
	home, paths, store := heartbeatLoopE2EHome(t)

	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	source := pipelineStageResultCmd("approved", "source ok", nil)
	score := pipelineStageResultCmd("approved", "score ok", nil)
	deploy := pipelineStageResultCmd("approved", "deploy ok", nil)
	specYAML := "name: deploy-flow\nrepo: owner/repo\nstages:\n" +
		pipelineE2EStage("source", source, "") +
		pipelineE2EStage("score", score, "source") +
		pipelineE2EStage("deploy", deploy, "score")
	specFile := writeSpec(t, specYAML)

	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "deploy-flow", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())
	if runID == "" {
		t.Fatalf("pipeline run printed no run id (stderr=%s)", errBuf.String())
	}

	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	// --- Phase 1: drive PARTWAY with the first instance -------------------------
	// Stop as soon as the root stage has succeeded: the run is genuinely mid-flight
	// (score enqueued but not run, deploy not started) when we simulate the restart.
	enqueue1 := newPipelineStageEnqueuer(store, home)
	worker1 := defaultJobWorker(store, io.Discard, home)
	for i := 0; i < 8; i++ {
		if err := runEnabledRepoWorkerTicks(ctx, store, worker1, 1, io.Discard, now); err != nil {
			t.Fatalf("phase-1 worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, store, enqueue1, now); err != nil {
			t.Fatalf("phase-1 scan %d: %v", i, err)
		}
		if stageRow(t, store, runID, "source").State == pipeline.StageSucceeded {
			break
		}
	}
	if got := stageRow(t, store, runID, "source"); got.State != pipeline.StageSucceeded {
		t.Fatalf("phase 1 did not land source: %+v", got)
	}
	if mid, _, _ := store.GetPipelineRun(ctx, runID); mid.State != pipeline.RunRunning {
		t.Fatalf("run settled during phase 1 (state=%s); wanted a mid-run restart", mid.State)
	}
	if got := stageRow(t, store, runID, "deploy"); got.State == pipeline.StageSucceeded {
		t.Fatalf("deploy already succeeded before restart: %+v", got)
	}

	// --- Phase 2: RESTART — a fresh instance re-opens the same DB ----------------
	store2, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	enqueue2 := newPipelineStageEnqueuer(store2, home)
	worker2 := defaultJobWorker(store2, io.Discard, home)
	for i := 0; i < 8; i++ {
		if err := runEnabledRepoWorkerTicks(ctx, store2, worker2, 1, io.Discard, now); err != nil {
			t.Fatalf("phase-2 worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, store2, enqueue2, now); err != nil {
			t.Fatalf("phase-2 scan %d: %v", i, err)
		}
		if run, _, _ := store2.GetPipelineRun(ctx, runID); run.State != pipeline.RunRunning {
			break
		}
	}

	run, ok, err := store2.GetPipelineRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetPipelineRun after restart: ok=%v err=%v", ok, err)
	}
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run recovered to state %s, want succeeded", run.State)
	}
	for _, id := range []string{"source", "score", "deploy"} {
		if got := stageRow(t, store2, runID, id); got.State != pipeline.StageSucceeded {
			t.Fatalf("stage %s = %s after restart, want succeeded", id, got.State)
		}
	}
}

// pipelineSecretGatedCmd is a shell stage body that gates its decision on an
// external file: if the file EXISTS it returns approved, else it returns blocked
// with the given need. It models the real resume story — a stage blocks on a missing
// secret, an operator provisions it (creates the file), and the SAME command
// succeeds on resume because the environment changed. Fully deterministic offline.
func pipelineSecretGatedCmd(secretPath, need string) string {
	ok := `printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"secret present","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`
	blocked := `printf '%s' '{"gitmoot_result":{"decision":"blocked","summary":"secret missing","findings":[],"changes_made":[],"tests_run":[],"needs":["` + need + `"],"delegations":[]}}'`
	return "if [ -f " + secretPath + " ]; then " + ok + "; else " + blocked + "; fi"
}

// TestPipelineResumeE2E is the resume E2E (#681): a run parks blocked on a missing
// secret; the operator provisions the secret and pipeline.ResumePipelineRun re-runs the
// halted stage and its dependents, which now succeed, driving the run to succeeded.
// The already-succeeded upstream stage is NOT re-run. NO LLM, NO network.
func TestPipelineResumeE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)

	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	secretPath := filepath.Join(t.TempDir(), "r2-token")
	source := pipelineStageResultCmd("approved", "source ok", nil)
	score := pipelineSecretGatedCmd(secretPath, "R2 token")
	deploy := pipelineStageResultCmd("approved", "deploy ok", nil)
	specYAML := "name: deploy-flow\nrepo: owner/repo\nstages:\n" +
		pipelineE2EStage("source", source, "") +
		pipelineE2EStage("score", score, "source") +
		pipelineE2EStage("deploy", deploy, "score")
	specFile := writeSpec(t, specYAML)

	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "deploy-flow", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())

	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	drive := func(label string) db.PipelineRun {
		t.Helper()
		for i := 0; i < 8; i++ {
			if err := runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, now); err != nil {
				t.Fatalf("%s worker tick %d: %v", label, i, err)
			}
			if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
				t.Fatalf("%s scan %d: %v", label, i, err)
			}
			if run, _, _ := store.GetPipelineRun(ctx, runID); run.State != pipeline.RunRunning {
				break
			}
		}
		run, _, _ := store.GetPipelineRun(ctx, runID)
		return run
	}

	// --- Run 1: parks blocked (secret absent) -----------------------------------
	run := drive("phase-1")
	if run.State != pipeline.RunBlocked {
		t.Fatalf("run state = %s, want blocked (secret absent)", run.State)
	}
	if run.HaltStage != "score" {
		t.Fatalf("halt_stage = %q, want score", run.HaltStage)
	}
	sourceJobBefore := stageRow(t, store, runID, "source").JobID
	if stageRow(t, store, runID, "source").State != pipeline.StageSucceeded {
		t.Fatalf("source must be succeeded before resume")
	}

	// --- Operator provisions the secret, then resumes ---------------------------
	if err := os.WriteFile(secretPath, []byte("present"), 0o644); err != nil {
		t.Fatalf("provision secret: %v", err)
	}
	resumed, err := pipeline.ResumePipelineRun(ctx, store, runID, "")
	if err != nil {
		t.Fatalf("pipeline.ResumePipelineRun: %v", err)
	}
	if resumed.State != pipeline.RunRunning {
		t.Fatalf("resumed run = %s, want running", resumed.State)
	}
	if got := stageRow(t, store, runID, "score"); got.State != pipeline.StagePending || got.Attempt != 1 {
		t.Fatalf("score after resume = %+v, want pending attempt 1", got)
	}

	// --- Run 2: now succeeds; source is NOT re-run ------------------------------
	run = drive("phase-2")
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run after resume = %s, want succeeded", run.State)
	}
	for _, id := range []string{"source", "score", "deploy"} {
		if got := stageRow(t, store, runID, id); got.State != pipeline.StageSucceeded {
			t.Fatalf("stage %s = %s after resume, want succeeded", id, got.State)
		}
	}
	// The succeeded upstream source stage kept its original job — it was never re-run.
	if got := stageRow(t, store, runID, "source"); got.JobID != sourceJobBefore || got.Attempt != 0 {
		t.Fatalf("source re-ran on resume: job %q (was %q) attempt %d", got.JobID, sourceJobBefore, got.Attempt)
	}
}
