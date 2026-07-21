package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
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
		if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, now, nil, nil); err != nil {
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

// TestPipelineAgentStageUpstreamContextE2E proves #757 UPSTREAM CONTEXT INJECTION:
// a downstream AGENT stage receives the RESULTS of the stages it needs, prepended
// to its prompt as a clearly-delimited, labeled block. It drives the real chain to
// completion, then inspects the downstream stage job's enqueued payload: its
// Instructions must carry the "Upstream stage results" block, the upstream stage id,
// the upstream stage's result summary, AND the stage's own prompt — proving the
// extract → triage dataflow reaches the runtime the agent runs.
func TestPipelineAgentStageUpstreamContextE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)

	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// The extractor approves with a DISTINCTIVE summary the downstream triager must
	// see verbatim in its prompt; the triager approves so the run completes.
	const extractSummary = "extracted 3 records: alpha, beta, gamma"
	seedDaemonWorkerAgentWithPolicy(t, store, "extractor", runtime.ShellRuntime,
		pipelineStageResultCmd("approved", extractSummary, nil),
		[]string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "triager", runtime.ShellRuntime,
		pipelineStageResultCmd("approved", "triaged", nil),
		[]string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	const triagePrompt = "Triage the extracted records and flag anomalies."
	specYAML := "name: context-flow\nrepo: owner/repo\nstages:\n" +
		"  - id: extract\n    agent: extractor\n    prompt: Extract the records.\n" +
		"  - id: triage\n    agent: triager\n    prompt: " + triagePrompt + "\n    needs: [extract]\n"
	specFile := writeSpec(t, specYAML)

	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "context-flow", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())

	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, now, nil, nil); err != nil {
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
		t.Fatalf("run state = %s, want succeeded", run.State)
	}

	// The downstream triage stage's ENQUEUED job carries the upstream context.
	triage := stageRow(t, store, runID, "triage")
	if triage.JobID == "" {
		t.Fatalf("triage stage has no job id; it never enqueued")
	}
	triageJob, err := store.GetJob(ctx, triage.JobID)
	if err != nil {
		t.Fatalf("GetJob(triage): %v", err)
	}
	payload, err := workflow.ParseJobPayload(triageJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(triage): %v", err)
	}
	instr := payload.Instructions
	if !strings.Contains(instr, "Upstream stage results") {
		t.Fatalf("triage prompt missing upstream-context header:\n%s", instr)
	}
	if !strings.Contains(instr, `stage "extract"`) {
		t.Fatalf("triage prompt missing upstream stage label:\n%s", instr)
	}
	if !strings.Contains(instr, extractSummary) {
		t.Fatalf("triage prompt missing upstream extract summary %q:\n%s", extractSummary, instr)
	}
	if !strings.Contains(instr, triagePrompt) {
		t.Fatalf("triage prompt lost its own task prompt:\n%s", instr)
	}
	// The upstream block precedes the stage's own prompt (prepended, per #757).
	if strings.Index(instr, "Upstream stage results") > strings.Index(instr, triagePrompt) {
		t.Fatalf("upstream context must be PREPENDED before the stage prompt:\n%s", instr)
	}

	// A ROOT agent stage (no needs) gets NO upstream block — its prompt is bare.
	extract := stageRow(t, store, runID, "extract")
	extractJob, err := store.GetJob(ctx, extract.JobID)
	if err != nil {
		t.Fatalf("GetJob(extract): %v", err)
	}
	extractPayload, err := workflow.ParseJobPayload(extractJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(extract): %v", err)
	}
	if strings.Contains(extractPayload.Instructions, "Upstream stage results") {
		t.Fatalf("root extract stage must have NO upstream block:\n%s", extractPayload.Instructions)
	}
}

// TestPipelineReviewStagesConcurrentWorktreesE2E proves #757 READ-ONLY WORKTREE
// ISOLATION: two same-repo review AGENT stages fanning out of a shared upstream
// stage are each born with their OWN detached read-only worktree, so they key
// worktree:<path> (never the shared repo:<repo> live checkout) and run CONCURRENTLY,
// and each worktree is DISPOSED on terminal (the #739 acceptance shape).
//
// CONCURRENCY PROOF: each review seat's shell body blocks on a 2-of-2 filesystem
// rendezvous — it emits `approved` only after BOTH seats' start markers exist. Both
// can reach `approved` ONLY if they were live SIMULTANEOUSLY. If isolation regressed
// (both keyed repo:owner/repo and serialized), the first seat waits out the
// rendezvous, emits `failed`, and the both-succeeded assertion flips RED.
func TestPipelineReviewStagesConcurrentWorktreesE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)

	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	stateDir := t.TempDir()
	// Root extractor approves instantly (no rendezvous), fanning out to two reviews.
	seedDaemonWorkerAgentWithPolicy(t, store, "extractor", runtime.ShellRuntime,
		pipelineStageResultCmd("approved", "extracted", nil),
		[]string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	// Two REVIEW agent stages (need the `review` capability) that rendezvous 2-of-2.
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewa", runtime.ShellRuntime,
		rendezvousSeatScript(stateDir, "reviewa", 2, rendezvousResult("approved", "reviewa ran beside reviewb"), ""),
		[]string{"ask", "review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewb", runtime.ShellRuntime,
		rendezvousSeatScript(stateDir, "reviewb", 2, rendezvousResult("approved", "reviewb ran beside reviewa"), ""),
		[]string{"ask", "review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	specYAML := "name: review-fan\nrepo: owner/repo\nstages:\n" +
		"  - id: extract\n    agent: extractor\n    prompt: Extract.\n" +
		"  - id: reva\n    agent: reviewa\n    action: review\n    prompt: Review A.\n    needs: [extract]\n" +
		"  - id: revb\n    agent: reviewb\n    action: review\n    prompt: Review B.\n    needs: [extract]\n"
	specFile := writeSpec(t, specYAML)

	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "review-fan", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())

	enqueue := newPipelineStageEnqueuer(store, home)
	// Phase 1: run the root extract stage and ENQUEUE the two reviews — but do NOT
	// run them yet (workers=1, break the moment both review rows are queued). This
	// lets us assert each review is born with its own distinct worktree BEFORE the
	// concurrent pool run.
	tickWorker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	var reva, revb db.PipelineRunStage
	enqueued := false
	for i := 0; i < 8 && !enqueued; i++ {
		if err := runEnabledRepoWorkerTicksTracked(ctx, store, tickWorker, 1, "", io.Discard, now, nil, nil); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		reva = stageRow(t, store, runID, "reva")
		revb = stageRow(t, store, runID, "revb")
		if reva.State == pipeline.StageQueued && revb.State == pipeline.StageQueued {
			enqueued = true
		}
	}
	if !enqueued {
		t.Fatalf("review stages never both reached queued (reva=%s revb=%s)", reva.State, revb.State)
	}

	// Each review stage job is born with its own detached read-only worktree.
	jobA, err := store.GetJob(ctx, reva.JobID)
	if err != nil {
		t.Fatalf("GetJob(reva): %v", err)
	}
	jobB, err := store.GetJob(ctx, revb.JobID)
	if err != nil {
		t.Fatalf("GetJob(revb): %v", err)
	}
	payloadA, err := workflow.ParseJobPayload(jobA.Payload)
	if err != nil {
		t.Fatalf("payload reva: %v", err)
	}
	payloadB, err := workflow.ParseJobPayload(jobB.Payload)
	if err != nil {
		t.Fatalf("payload revb: %v", err)
	}
	for name, p := range map[string]workflow.JobPayload{"reva": payloadA, "revb": payloadB} {
		if strings.TrimSpace(p.WorktreePath) == "" {
			t.Fatalf("review stage %s has no read-only worktree (#757/#739)", name)
		}
		if !p.ReadOnlyWorktree {
			t.Fatalf("review stage %s ReadOnlyWorktree = false, want true (disposal marker)", name)
		}
	}
	keyA := queuedJobCheckoutKey(ctx, store, jobA)
	keyB := queuedJobCheckoutKey(ctx, store, jobB)
	if !strings.HasPrefix(keyA, "worktree:") || !strings.HasPrefix(keyB, "worktree:") {
		t.Fatalf("review stages must key worktree:<path>, got %q and %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("review stages share checkout key %q — they would serialize (want distinct)", keyA)
	}
	for _, id := range []string{reva.JobID, revb.JobID} {
		if n := countCLIJobEvents(t, store, id, "readonly_worktree_allocated"); n != 1 {
			t.Fatalf("job %s readonly_worktree_allocated events = %d, want 1", id, n)
		}
	}

	// Phase 2: drive both reviews concurrently on the pool worker. Both must be live
	// simultaneously to clear the 2-of-2 rendezvous.
	bothRunning := drivePoolConcurrently(t, ctx, readonlyPoolWorker(store, home), store, []string{reva.JobID, revb.JobID})
	if !bothRunning {
		t.Fatal("state sampler never observed both review stages in `running` at once (serialized)")
	}
	for _, id := range []string{reva.JobID, revb.JobID} {
		job, decision := terminalJobDecision(t, ctx, store, id)
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("review stage %s state = %q, want succeeded (a serialized seat times out the rendezvous)", id, job.State)
		}
		if decision != "approved" {
			t.Fatalf("review stage %s decision = %q, want approved (ran concurrently)", id, decision)
		}
	}

	// Each detached worktree is DISPOSED on terminal (dir gone + removal event).
	for name, id := range map[string]string{"reva": reva.JobID, "revb": revb.JobID} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		p, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatalf("payload %s: %v", id, err)
		}
		if _, statErr := os.Stat(p.WorktreePath); !os.IsNotExist(statErr) {
			t.Fatalf("review stage %s worktree %s not disposed on terminal (stat err=%v)", name, p.WorktreePath, statErr)
		}
		if n := countCLIJobEvents(t, store, id, "delegation_worktree_removed"); n != 1 {
			t.Fatalf("review stage %s delegation_worktree_removed events = %d, want 1", name, n)
		}
	}

	// Phase 3: a final scan folds both approved reviews and the run SUCCEEDS.
	if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
		t.Fatalf("final pipeline scan: %v", err)
	}
	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetPipelineRun(%s): ok=%v err=%v", runID, ok, err)
	}
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded", run.State)
	}
}

// TestPipelineAddWarnsMissingAgentStageAgent proves the #757 add-time behavior: a
// spec whose agent stage names an agent that does not exist yet is WARNED about
// but NOT blocked at `pipeline add` — a spec may legitimately be added before its
// agents are provisioned (bundled/shareable pipelines, scripted setup). The
// genuinely-missing agent still fails loudly when the stage runs.
func TestPipelineAddWarnsMissingAgentStageAgent(t *testing.T) {
	home, _, _ := heartbeatLoopE2EHome(t)

	specYAML := "name: ghost-flow\nrepo: owner/repo\nstages:\n" +
		"  - id: review\n    agent: nonexistent\n    prompt: Review.\n"
	specFile := writeSpec(t, specYAML)

	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add failed (code %d), want success with a warning:\n%s", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `agent "nonexistent" which does not exist yet`) {
		t.Fatalf("stderr missing missing-agent warning:\n%s", errBuf.String())
	}
}
