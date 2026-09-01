package pipeline

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/proof"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1685 P2. The public service proof is built from a SANITIZED copy of each
// result, and that copy is rebuilt field by field. In the production pipeline
// shape Mailbox.Run has already stripped Delegations and only the FanOut
// classification survives, so a copy that omits it hands the projector an
// ordinary approval — and the rendered public manifest reports an approved
// review that nobody gave.
//
// Driven through the real chain: sanitizePipelineProofJobs -> proof.Project ->
// proof.RenderTree. The projector-only assertion is not enough here; the earlier
// projector fix was already in place while the rendered output was still wrong.
func TestPublicServiceProofKeepsFanOutClassification(t *testing.T) {
	root := db.Job{
		ID: "pipeline-run", RootID: "pipeline-run", Type: "pipeline", State: "succeeded",
		CreatedAt: "2026-09-01 10:00:00", UpdatedAt: "2026-09-01 10:05:00",
	}
	reviewJob := db.Job{
		ID: "stage-review", RootID: "pipeline-run", ParentJobID: "pipeline-run",
		Agent: "panel-coordinator", Type: "review", State: "succeeded",
		CreatedAt: "2026-09-01 10:01:00", UpdatedAt: "2026-09-01 10:04:00",
	}
	// Exactly what the mailbox persists for a pipeline review fan-out: no
	// delegations left, classification retained.
	results := map[string]*workflow.AgentResult{
		reviewJob.ID: {Decision: "approved", Summary: "Convening a panel", FanOut: true},
	}

	publicJobs, publicResults := sanitizePipelineProofJobs([]db.Job{root, reviewJob}, results)

	safe := publicResults[reviewJob.ID]
	if safe == nil {
		t.Fatal("sanitization dropped the review result entirely")
	}
	if !safe.FanOut {
		t.Fatal("sanitization dropped the fan-out classification")
	}
	if len(safe.Delegations) != 0 {
		t.Fatalf("sanitized copy leaked delegation payloads: %+v", safe.Delegations)
	}
	if !workflow.ResultIsFanOut(safe) {
		t.Fatal("the sanitized copy is not classified as a fan-out")
	}

	manifest := proof.Project(root, publicJobs, publicResults, nil, nil)
	var rendered strings.Builder
	if err := proof.RenderTree(&rendered, manifest); err != nil {
		t.Fatalf("RenderTree: %v", err)
	}
	out := rendered.String()
	if !strings.Contains(out, "no verdict") || !strings.Contains(out, "fan-out") {
		t.Fatalf("public proof hides the fan-out classification:\n%s", out)
	}
	if strings.Contains(out, "1 approved") {
		t.Fatalf("public proof counted a fan-out as an approved review:\n%s", out)
	}
}

// ACCEPTANCE: a real leaf approval still projects and renders as approved
// through the same public path, so the guard cannot be read as "pipeline reviews
// never count".
func TestPublicServiceProofStillCountsRealApproval(t *testing.T) {
	root := db.Job{
		ID: "pipeline-run", RootID: "pipeline-run", Type: "pipeline", State: "succeeded",
		CreatedAt: "2026-09-01 10:00:00", UpdatedAt: "2026-09-01 10:05:00",
	}
	reviewJob := db.Job{
		ID: "stage-review", RootID: "pipeline-run", ParentJobID: "pipeline-run",
		Agent: "reviewer", Type: "review", State: "succeeded",
		CreatedAt: "2026-09-01 10:01:00", UpdatedAt: "2026-09-01 10:04:00",
	}
	results := map[string]*workflow.AgentResult{
		reviewJob.ID: {Decision: "approved", Summary: "reviewed the diff at this head"},
	}

	publicJobs, publicResults := sanitizePipelineProofJobs([]db.Job{root, reviewJob}, results)
	manifest := proof.Project(root, publicJobs, publicResults, nil, nil)
	var rendered strings.Builder
	if err := proof.RenderTree(&rendered, manifest); err != nil {
		t.Fatalf("RenderTree: %v", err)
	}
	if out := rendered.String(); strings.Contains(out, "no verdict") {
		t.Fatalf("a real approval was rendered as no verdict:\n%s", out)
	}
}
