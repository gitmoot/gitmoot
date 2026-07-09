package workflow

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// #758 CORE: an orchestrate pipeline stage IS a bounded agent sub-tree ROOT. This
// file proves the three engine-seam properties the CORE guarantees:
//   1. the pipeline-sender leaf strip is RELAXED for a stage carrying the explicit
//      OrchestrateStage payload flag, so the coordinator's delegations[] survive;
//   2. AdvanceJob then fans them out as children OWNED by the stage job
//      (ParentJobID = the stage job) via the unchanged dispatchDelegations;
//   3. the child's RootJobID is the stage job's OWN id (the sub-tree root), NOT the
//      run id — the one deliberate #757 deviation that gives the sub-tree its own
//      per-root budget/kill scope.
// The control (a normal pipeline agent stage WITHOUT the flag) keeps the strip
// byte-identically, so the relaxation is gated strictly on the flag and never on
// sender-sniffing.

const orchestrateStageRepo = "jerryfane/gitmoot"

// orchestrateDelegatingResult is the coordinator's shell stand-in output: a healthy
// approved result that fans out ONE named-agent review child. No real LLM.
const orchestrateDelegatingResult = `{"gitmoot_result":{"decision":"approved","summary":"fan out the sub-tree","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"leg1","agent":"orch-child","action":"review","prompt":"inspect the module"}]}}`

// TestOrchestrateStageFansOutUnderStageJob is the #758 CORE proof: a pipeline
// orchestrate stage job (Sender = pipeline, OrchestrateStage flag set) whose shell
// stand-in returns delegations[] keeps them (strip relaxed), and AdvanceJob fans a
// child out UNDER the stage job with RootJobID = the stage job id.
func TestPipelineOrchestrateStageFansOutUnderStageJob(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	// The fan-out target must be a known, review-capable agent in the repo scope or
	// dispatchDelegations' preflight rejects the set before enqueuing any child.
	seedAgent(t, store, "orch-child", []string{"review"}, orchestrateStageRepo)

	mailbox := Mailbox{Store: store}
	const stageID = "prun-orch-1-stage-a0"
	// Enqueue the orchestrate stage job exactly as pipelineStageJobRequest does: the
	// pipeline sender, the OrchestrateStage authorization flag, and RootJobID = its
	// OWN id (the sub-tree root).
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:               stageID,
		Agent:            "orch-coord",
		Action:           "ask",
		Repo:             orchestrateStageRepo,
		Sender:           PipelineJobSender,
		RootJobID:        stageID,
		OrchestrateStage: true,
	}); err != nil {
		t.Fatalf("Enqueue orchestrate stage: %v", err)
	}

	coordAgent := runtime.Agent{Name: "orch-coord", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: orchestrateStageRepo, Role: "coordinator"}
	adapter := &fakeDelivery{outputs: []string{orchestrateDelegatingResult}}
	if _, err := mailbox.Run(ctx, stageID, coordAgent, adapter); err != nil {
		t.Fatalf("Run orchestrate stage: %v", err)
	}

	// (1) Strip relaxed: the coordinator's delegations[] survive in the stored result.
	stageJob, err := store.GetJob(ctx, stageID)
	if err != nil {
		t.Fatalf("GetJob(stage): %v", err)
	}
	stagePayload, err := ParseJobPayload(stageJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(stage): %v", err)
	}
	if stagePayload.Result == nil || len(stagePayload.Result.Delegations) != 1 {
		t.Fatalf("orchestrate stage delegations = %+v, want preserved (1) — the leaf strip must be relaxed for the OrchestrateStage flag", stagePayload.Result)
	}
	// (3a) The stage job itself is the sub-tree root.
	if stagePayload.RootJobID != stageID {
		t.Fatalf("orchestrate stage RootJobID = %q, want its own id %q (the sub-tree root)", stagePayload.RootJobID, stageID)
	}

	// (2) AdvanceJob fans the delegation out as a child owned by the stage job.
	if err := engine.AdvanceJob(ctx, stageID); err != nil {
		t.Fatalf("AdvanceJob(stage) fan-out: %v", err)
	}
	childID := stageID + "/delegation/leg1"
	if !jobExists(t, store, childID) {
		t.Fatalf("orchestrate child %s was not dispatched — dispatchDelegations should own the fan-out", childID)
	}
	childJob, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(child): %v", err)
	}
	// (2) ParentJobID = the stage job (owned, not orphaned).
	if childJob.ParentJobID != stageID {
		t.Fatalf("child ParentJobID = %q, want the stage job %q (fan-out must be OWNED)", childJob.ParentJobID, stageID)
	}
	childPayload, err := ParseJobPayload(childJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(child): %v", err)
	}
	if childPayload.ParentJobID != stageID {
		t.Fatalf("child payload ParentJobID = %q, want the stage job %q", childPayload.ParentJobID, stageID)
	}
	// (3b) The child's RootJobID is the stage job's own id — the sub-tree root, so
	// every per-root bound (countRootDelegationJobs / rootWallClockExceeded /
	// IsRootJobKilled) scopes to THIS stage's tree, not the pipeline run.
	if childPayload.RootJobID != stageID {
		t.Fatalf("child RootJobID = %q, want the stage job id %q (per-root budget/kill scope)", childPayload.RootJobID, stageID)
	}
}

// TestNonOrchestratePipelineStageStillStripsDelegations is the CONTROL: a normal
// pipeline agent stage (Sender = pipeline, OrchestrateStage NOT set) returning the
// same delegations[] has them stripped byte-identically, and AdvanceJob dispatches
// NO child. This proves the strip relaxation is gated strictly on the explicit flag.
func TestNonOrchestratePipelineStageStillStripsDelegations(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedAgent(t, store, "orch-child", []string{"review"}, orchestrateStageRepo)

	mailbox := Mailbox{Store: store}
	const stageID = "prun-leaf-1-stage-a0"
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:        stageID,
		Agent:     "leaf-coord",
		Action:    "ask",
		Repo:      orchestrateStageRepo,
		Sender:    PipelineJobSender,
		RootJobID: "prun-leaf-1", // a normal #757 agent stage carries RootJobID = run id
		// OrchestrateStage deliberately unset.
	}); err != nil {
		t.Fatalf("Enqueue leaf stage: %v", err)
	}

	agent := runtime.Agent{Name: "leaf-coord", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: orchestrateStageRepo, Role: "pipeline-runner"}
	adapter := &fakeDelivery{outputs: []string{orchestrateDelegatingResult}}
	if _, err := mailbox.Run(ctx, stageID, agent, adapter); err != nil {
		t.Fatalf("Run leaf stage: %v", err)
	}

	stageJob, err := store.GetJob(ctx, stageID)
	if err != nil {
		t.Fatalf("GetJob(stage): %v", err)
	}
	stagePayload, err := ParseJobPayload(stageJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(stage): %v", err)
	}
	if stagePayload.Result == nil {
		t.Fatalf("leaf stage has no result")
	}
	if len(stagePayload.Result.Delegations) != 0 {
		t.Fatalf("non-orchestrate pipeline stage delegations = %+v, want STRIPPED (leaf) — the strip must stay byte-identical without the flag", stagePayload.Result.Delegations)
	}

	// AdvanceJob dispatches nothing: with delegations stripped there is no fan-out.
	if err := engine.AdvanceJob(ctx, stageID); err != nil {
		t.Fatalf("AdvanceJob(leaf stage): %v", err)
	}
	if jobExists(t, store, stageID+"/delegation/leg1") {
		t.Fatalf("a non-orchestrate pipeline stage must never spawn a delegation child")
	}
}
