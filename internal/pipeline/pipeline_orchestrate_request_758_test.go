package pipeline

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestPipelineOrchestrateStageJobRequestShape pins the #758 dispatch deviation: an
// orchestrate stage builds a #757-shaped AGENT request (named agent on its own
// runtime, no shell override, pipeline sender, upstream-context-prepended prompt)
// with exactly two deliberate differences — the OrchestrateStage authorization flag
// is set, and RootJobID is the stage job's OWN id (NOT the run id) so the sub-tree
// is a true tree root. A plain #757 agent stage (control) keeps OrchestrateStage
// false and RootJobID = run.ID, byte-identically.
func TestPipelineOrchestrateStageJobRequestShape(t *testing.T) {
	rec := db.Pipeline{Name: "orch", Repo: "owner/repo"}
	run := db.PipelineRun{ID: "prun-orch-abc", Pipeline: "orch"}

	orch := Stage{ID: "decompose", Agent: "coordinator", Prompt: "Fan out.", Action: "ask", Orchestrate: true, Timeout: "30m"}
	req := PipelineStageJobRequest(rec, orch, run, 0, "UPSTREAM\n", PipelineStagePRBinding{}, false)

	wantID := pipelineStageJobID(run.ID, orch.ID, 0)
	if req.ID != wantID {
		t.Fatalf("orchestrate request ID = %q, want %q", req.ID, wantID)
	}
	if !req.OrchestrateStage {
		t.Fatalf("orchestrate request must set OrchestrateStage=true to authorize fan-out")
	}
	if req.RootJobID != wantID {
		t.Fatalf("orchestrate request RootJobID = %q, want its OWN job id %q (the sub-tree root, not run.ID=%q)", req.RootJobID, wantID, run.ID)
	}
	if req.Agent != "coordinator" || req.Action != "ask" {
		t.Fatalf("orchestrate request agent/action = %q/%q, want coordinator/ask", req.Agent, req.Action)
	}
	if req.Sender != workflow.PipelineJobSender {
		t.Fatalf("orchestrate request sender = %q, want the pipeline sender", req.Sender)
	}
	if req.RuntimeOverride != "" {
		t.Fatalf("orchestrate request must NOT carry a shell runtime override (it runs on the agent's own runtime); got %q", req.RuntimeOverride)
	}
	if req.Instructions != "UPSTREAM\nFan out." {
		t.Fatalf("orchestrate request instructions = %q, want upstream context prepended to the prompt", req.Instructions)
	}
	if req.ParentJobID != "" {
		t.Fatalf("orchestrate request ParentJobID = %q, want empty (the coordinator is the tree root, not a delegation child)", req.ParentJobID)
	}
	if req.JobTimeout != "30m" {
		t.Fatalf("orchestrate request JobTimeout = %q, want 30m", req.JobTimeout)
	}

	// Control: a plain #757 agent stage — no OrchestrateStage flag, RootJobID = run.ID.
	leaf := Stage{ID: "review", Agent: "reviewer", Prompt: "Review.", Action: "review"}
	leafReq := PipelineStageJobRequest(rec, leaf, run, 0, "", PipelineStagePRBinding{}, false)
	if leafReq.OrchestrateStage {
		t.Fatalf("a plain #757 agent stage must NOT set OrchestrateStage")
	}
	if leafReq.RootJobID != run.ID {
		t.Fatalf("plain agent stage RootJobID = %q, want run.ID %q (byte-identical #757)", leafReq.RootJobID, run.ID)
	}
}
