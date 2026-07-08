package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// TestPipelineAgentStageAdvanceAndParkE2E is the full-chain, NO-LLM, deterministic
// E2E for #757 AGENT stages: a pipeline stage that runs a NAMED managed agent (not
// the hidden shell runner) as a read-only LEAF. It drives the real chain a daemon
// iteration runs — `pipeline add` -> `pipeline run` -> the real worker tick claims
// + runs each stage job through the agent's OWN runtime -> runPipelineScanOnce
// folds each settled stage by decision and advances.
//
// The agents are bound to the SHELL runtime as deterministic stand-ins for real
// LLM agents (each emits a fixed gitmoot_result), since the point is the STAGE
// machinery — that an agent stage ADVANCES on an approved decision and PARKS the
// run on a blocked decision — not the model. The first agent stage approves (so
// its dependent is enqueued), the second blocks with needs (so the run parks
// blocked with the needs persisted at the run level).
func TestPipelineAgentStageAdvanceAndParkE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)

	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// Two NAMED managed agents on the SHELL runtime: the stage binds to these by
	// name and each runs on its OWN registered runtime (no per-job override). Their
	// RuntimeRef is the deterministic result-emitting command a real LLM agent's
	// decision stands in for.
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime,
		pipelineStageResultCmd("approved", "review ok", nil),
		[]string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "auditor", runtime.ShellRuntime,
		pipelineStageResultCmd("blocked", "audit needs prod creds", []string{"prod creds"}),
		[]string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	specYAML := "name: agent-flow\nrepo: owner/repo\nstages:\n" +
		"  - id: review\n    agent: reviewer\n    prompt: Review the change.\n" +
		"  - id: audit\n    agent: auditor\n    prompt: Audit the dependencies.\n    needs: [review]\n"
	specFile := writeSpec(t, specYAML)

	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "agent-flow", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())
	if runID == "" {
		t.Fatalf("pipeline run printed no run id (stderr=%s)", errBuf.String())
	}

	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
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
	if run.HaltStage != "audit" {
		t.Fatalf("halt_stage = %q, want audit", run.HaltStage)
	}
	if got := decodePipelineNeeds(run.NeedsJSON); len(got) != 1 || got[0] != "prod creds" {
		t.Fatalf("run needs = %v, want [prod creds]", got)
	}

	// The approved AGENT stage advanced: it succeeded AND its dependent was enqueued
	// and run (it could only run because review reached succeeded).
	rev := stageRow(t, store, runID, "review")
	if rev.State != pipeline.StageSucceeded {
		t.Fatalf("stage review = %s, want succeeded", rev.State)
	}
	if rev.JobID == "" {
		t.Fatalf("stage review has no job id; it never ran")
	}
	// The stage bound its job to the NAMED agent, not the hidden shell runner.
	revJob, err := store.GetJob(ctx, rev.JobID)
	if err != nil {
		t.Fatalf("GetJob(review): %v", err)
	}
	if revJob.Agent != "reviewer" {
		t.Fatalf("review stage job agent = %q, want reviewer (named agent, not runner)", revJob.Agent)
	}

	aud := stageRow(t, store, runID, "audit")
	if aud.State != pipeline.StageBlocked {
		t.Fatalf("stage audit = %s, want blocked", aud.State)
	}
	if got := decodePipelineNeeds(aud.NeedsJSON); len(got) != 1 || got[0] != "prod creds" {
		t.Fatalf("stage audit needs = %v, want [prod creds]", got)
	}

	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "show", runID, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline show run exit=%d stderr=%s", code, errBuf.String())
	}
	funnel := out.String()
	if !strings.Contains(funnel, "review OK -> audit BLOCKED (needs: prod creds)") {
		t.Fatalf("funnel missing expected line:\n%s", funnel)
	}
}

// TestPipelineAddRejectsMissingAgentStageAgent proves the #757 add-time guard: a
// spec whose agent stage names an agent that does not exist is rejected at
// `pipeline add`, not left as a stage job the worker can never resolve.
func TestPipelineAddRejectsMissingAgentStageAgent(t *testing.T) {
	home, _, _ := heartbeatLoopE2EHome(t)

	specYAML := "name: ghost-flow\nrepo: owner/repo\nstages:\n" +
		"  - id: review\n    agent: nonexistent\n    prompt: Review.\n"
	specFile := writeSpec(t, specYAML)

	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code == 0 {
		t.Fatalf("pipeline add succeeded, want rejection for missing agent")
	}
	if !strings.Contains(errBuf.String(), `agent "nonexistent" which does not exist`) {
		t.Fatalf("stderr missing missing-agent error:\n%s", errBuf.String())
	}
}
