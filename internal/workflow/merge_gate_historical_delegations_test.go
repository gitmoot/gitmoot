package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestPolicyMergeGateHistoricalDelegationsWithoutVerdictNeverApprove pins the
// safety property #2588 PR 2 relies on when the gate stopped classifying
// fan-outs itself: a stored #1685 coordinator announcement is not a verdict,
// whatever decision it recorded. Coordinators announced with decision
// "approved", in two stored shapes: the declared panel still visible, or
// stripped to `fan_out: true`. Each must hold the merge as an abstaining
// reviewer, never satisfy approval. The rows are written as raw stored JSON
// because no current result type can produce them.
func TestPolicyMergeGateHistoricalDelegationsWithoutVerdictNeverApprove(t *testing.T) {
	const declaredPanel = `,"delegations":[{"id":"lens-a","agent":"lens-a","action":"review","prompt":"look"},{"id":"lens-b","agent":"lens-b","action":"review","prompt":"look"}]`
	const strippedPanel = `,"fan_out":true`
	for _, panel := range []struct{ name, json string }{{"declared", declaredPanel}, {"stripped", strippedPanel}} {
		for _, decision := range []string{"approved", "changes_requested", "", "skipped", "fan-out"} {
			t.Run(panel.name+"/decision="+decision, func(t *testing.T) {
				store, gh, gate, request := newMergeGateQuorumScenario(t)
				seedMergeGateFixtureAgent(t, store, "audit")
				encoded := fmt.Sprintf(`{"repo":"gitmoot/gitmoot","pull_request":9,"head_sha":"head123","task_id":"task-9","review_round":"review-1","result":{"decision":%q,"summary":"historical panel announcement"%s}}`, decision, panel.json)
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
					t.Fatalf("decision = %+v merges = %+v; a historical coordinator announcement satisfied approval", got, gh.merges)
				}
				if !got.LeaveOpen || !got.Reason.IsGateMiss() {
					t.Fatalf("decision = %+v, want the announcement to hold the merge", got)
				}
				if reason := got.Reason.Render(); !strings.Contains(reason, "abstaining reviewer audit") {
					t.Fatalf("reason = %q, want the row reported as an abstaining reviewer", reason)
				}
			})
		}
	}
}

// TestDecodeJobPayloadMarksOnlyReviewAnnouncements is the control for the
// boundary above. Only a REVIEW row carrying the announcement marks loses its
// decision: an ask or implement result with delegations is a real answer and
// keeps it (round-2 P2), and re-encoding a marked row writes back the stored
// decision, so a read-modify-write never persists the rewrite.
func TestDecodeJobPayloadMarksOnlyReviewAnnouncements(t *testing.T) {
	for _, tc := range []struct {
		name, jobType, panel, decision, want string
	}{
		{"ordinary approved", "review", ``, "approved", "approved"},
		{"ordinary changes_requested", "review", ``, "changes_requested", "changes_requested"},
		{"empty delegations", "review", `,"delegations":[]`, "approved", "approved"},
		{"fan_out false", "review", `,"fan_out":false`, "approved", "approved"},
		{"declared panel", "review", `,"delegations":[{"id":"a"}]`, "approved", HistoricalFanOutDecision},
		{"stripped panel", "review", `,"fan_out":true`, "changes_requested", HistoricalFanOutDecision},
		{"announcement blocked", "review", `,"fan_out":true`, "blocked", "blocked"},
		{"ask with delegations", "ask", `,"delegations":[{"id":"a"}]`, "approved", "approved"},
		{"implement with delegations", "implement", `,"delegations":[{"id":"a"}]`, "implemented", "implemented"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := fmt.Sprintf(`{"result":{"decision":%q,"summary":"s"%s}}`, tc.decision, tc.panel)
			payload, err := DecodeJobPayload(db.Job{ID: "j", Type: tc.jobType, Payload: stored})
			if err != nil {
				t.Fatalf("DecodeJobPayload returned error: %v", err)
			}
			if payload.Result == nil || payload.Result.Decision != tc.want {
				t.Fatalf("decision = %+v, want %q", payload.Result, tc.want)
			}
			encoded, err := marshalPayload(payload)
			if err != nil {
				t.Fatalf("marshalPayload returned error: %v", err)
			}
			if !strings.Contains(encoded, fmt.Sprintf(`"decision":%q`, tc.decision)) {
				t.Fatalf("re-encoded payload %s does not keep the stored decision %q", encoded, tc.decision)
			}
		})
	}
}
