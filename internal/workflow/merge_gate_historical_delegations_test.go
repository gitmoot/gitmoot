package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestPolicyMergeGateHistoricalDelegationsWithoutVerdictNeverApprove pins the
// safety property #2588 PR 2 relies on when the gate stopped treating review
// rows that carry `delegations` as fan-outs: such a row is judged on its own
// recorded decision, so a row with no decision or an unrecognized one is an
// abstention at the evaluated head and must hold the merge, never satisfy
// approval. Historical rows of this shape remain in stored payloads.
func TestPolicyMergeGateHistoricalDelegationsWithoutVerdictNeverApprove(t *testing.T) {
	for _, decision := range []string{"", "skipped", "fan-out"} {
		t.Run("decision="+decision, func(t *testing.T) {
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			seedMergeGateFixtureAgent(t, store, "audit")
			encoded, err := marshalPayload(JobPayload{
				Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123",
				TaskID: "task-9", ReviewRound: "review-1",
				Result: &AgentResult{
					Decision: decision,
					Summary:  "historical panel announcement",
					Delegations: []Delegation{
						{ID: "lens-a", Agent: "lens-a", Action: "review", Prompt: "look"},
						{ID: "lens-b", Agent: "lens-b", Action: "review", Prompt: "look"},
					},
				},
			})
			if err != nil {
				t.Fatalf("marshalPayload returned error: %v", err)
			}
			if err := store.CreateJobWithEvent(context.Background(), db.Job{
				ID: "review-historical-panel", Agent: "audit", Type: "review",
				State: string(JobSucceeded), Payload: encoded,
			}, db.JobEvent{Kind: string(JobSucceeded), Message: "historical"}); err != nil {
				t.Fatalf("CreateJobWithEvent returned error: %v", err)
			}

			got, err := gate.Evaluate(context.Background(), request)
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if got.Merged || len(gh.merges) != 0 {
				t.Fatalf("decision = %+v merges = %+v; a review row with delegations but no verdict satisfied approval", got, gh.merges)
			}
			if !got.LeaveOpen || !got.Reason.IsGateMiss() {
				t.Fatalf("decision = %+v, want the abstaining row to hold the merge", got)
			}
			if reason := got.Reason.Render(); !strings.Contains(reason, "abstaining reviewer audit") {
				t.Fatalf("reason = %q, want the row reported as an abstaining reviewer", reason)
			}
		})
	}
}
