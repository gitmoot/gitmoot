package pipeline

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1990. #1967 resolved the dispatcher at the enqueue chokepoint and then also
// set an explicit, more specific value at each construction site that knew one.
// I got the pipeline site WRONG: PipelineStageJobRequest has five per-kind
// JobRequest literals and I set DispatchedBy in one of them, the read-only agent
// branch.
//
// FOUND IN PRODUCTION, not by reading. After the deploy, five real prun-* stage
// jobs recorded dispatched_by "pipeline" - the Sender channel - instead of the
// pipeline that ordered them. The chokepoint did its job: the column was
// non-empty, which is what #1967 was for. But four stage kinds out of five never
// got the specific value, and nothing would have told me.
//
// So this asserts EVERY stage kind, and the fix stamps the value at the
// function's single exit rather than in its branches, so a sixth kind inherits
// it without its author knowing the rule exists. A per-branch test would pass on
// a per-branch fix and then rot the moment a kind is added; this table is the
// shape that cannot.
func TestEveryPipelineStageKindNamesTheDispatchingPipeline(t *testing.T) {
	rec := db.Pipeline{Name: "nightly-audit", Repo: "owner/repo"}
	run := db.PipelineRun{ID: "prun-nightly-audit-abc", Pipeline: "nightly-audit"}
	const want = "pipeline:nightly-audit"

	for _, tc := range []struct {
		kind  string
		stage Stage
	}{
		{"orchestrate", Stage{ID: "decompose", Agent: "coordinator", Prompt: "Fan out.", Action: "ask", Orchestrate: true}},
		{"agent produce", Stage{ID: "collect", Agent: "producer", Prompt: "Produce.", Action: "produce", Writes: []string{"out.json"}}},
		{"agent implement", Stage{ID: "fix", Agent: "implementer", Prompt: "Fix.", Action: "implement"}},
		{"agent read-only", Stage{ID: "review", Agent: "reviewer", Prompt: "Review.", Action: "review"}},
		{"shell", Stage{ID: "deliver", Cmd: "echo delivered"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			request := PipelineStageJobRequest(rec, tc.stage, run, 0, "", PipelineStagePRBinding{}, false)
			if request.DispatchedBy != want {
				t.Fatalf("%s stage dispatched_by = %q, want %q; without it the row falls back to the %q channel and a finding cannot be traced to the pipeline that ordered the review",
					tc.kind, request.DispatchedBy, want, "pipeline")
			}
		})
	}
}
