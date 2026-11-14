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
			// resaved runs the row through a production read-modify-write
			// first: decode, re-encode, save. A save must not turn the
			// announcement back into an approval (round-3 P1).
			for _, resaved := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/decision=%s/resaved=%t", panel.name, decision, resaved), func(t *testing.T) {
					store, gh, gate, request := newMergeGateQuorumScenario(t)
					seedMergeGateFixtureAgent(t, store, "audit")
					encoded := fmt.Sprintf(`{"repo":"gitmoot/gitmoot","pull_request":9,"head_sha":"head123","task_id":"task-9","review_round":"review-1","result":{"decision":%q,"summary":"historical panel announcement"%s}}`, decision, panel.json)
					if err := store.CreateJobWithEvent(context.Background(), db.Job{
						ID: "review-historical-panel", Agent: "audit", Type: "review",
						State: string(JobSucceeded), Payload: encoded,
					}, db.JobEvent{Kind: string(JobSucceeded), Message: "historical"}); err != nil {
						t.Fatalf("CreateJobWithEvent returned error: %v", err)
					}
					if resaved {
						job, err := store.GetJob(context.Background(), "review-historical-panel")
						if err != nil {
							t.Fatalf("GetJob returned error: %v", err)
						}
						payload, err := DecodeJobPayload(job)
						if err != nil {
							t.Fatalf("DecodeJobPayload returned error: %v", err)
						}
						if err := (Mailbox{store: store}).savePayload(context.Background(), job.ID, payload); err != nil {
							t.Fatalf("savePayload returned error: %v", err)
						}
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
}

// TestDecodeJobPayloadMarksOnlyReviewAnnouncements is the control for the
// boundary above. Only a REVIEW row carrying the announcement marks loses its
// decision: an ask or implement result with delegations is a real answer and
// keeps it (round-2 P2). Re-encoding writes back the stored result, marks
// included, so a read-modify-write neither persists the rewrite nor drops the
// mark that makes the row an announcement (round-3 P1).
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
			if tc.want == HistoricalFanOutDecision && !strings.Contains(encoded, stored[len(`{`):len(stored)-1]) {
				t.Fatalf("re-encoded announcement %s does not carry its stored result verbatim (%s)", encoded, stored)
			}
			again, err := DecodeJobPayload(db.Job{ID: "j", Type: tc.jobType, Payload: encoded})
			if err != nil {
				t.Fatalf("DecodeJobPayload(re-encoded) returned error: %v", err)
			}
			if again.Result == nil || again.Result.Decision != tc.want {
				t.Fatalf("decision after round trip = %+v, want %q", again.Result, tc.want)
			}
		})
	}
}

// TestHistoricalAnnouncementRetryStoresTheFreshVerdict pins round-4 P2: the
// verbatim write-back belongs to the decoded announcement only. A manual retry
// clears the result, and the retried run's fresh verdict must be what gets
// stored, not the old announcement it replaced.
func TestHistoricalAnnouncementRetryStoresTheFreshVerdict(t *testing.T) {
	for _, panel := range []struct{ name, json string }{
		{"declared", `,"delegations":[{"id":"lens-a","agent":"lens-a","action":"review","prompt":"look"}]`},
		{"stripped", `,"fan_out":true`},
	} {
		t.Run(panel.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			stored := fmt.Sprintf(`{"repo":"gitmoot/gitmoot","pull_request":9,"head_sha":"head123","result":{"decision":"approved","summary":"stale"%s}}`, panel.json)
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID: "review-historical", Agent: "audit", Type: "review", State: string(JobFailed), Payload: stored,
			}, db.JobEvent{Kind: string(JobFailed), Message: "failed"}); err != nil {
				t.Fatalf("CreateJobWithEvent returned error: %v", err)
			}

			job, err := RetryJob(ctx, store, "review-historical")
			if err != nil {
				t.Fatalf("RetryJob returned error: %v", err)
			}
			if strings.Contains(job.Payload, `"result"`) {
				t.Fatalf("payload after retry = %s, want the historical result cleared", job.Payload)
			}

			// The retried run decodes the queued row and stores its fresh
			// result through the same encoder the mailbox uses.
			payload, err := DecodeJobPayload(job)
			if err != nil {
				t.Fatalf("DecodeJobPayload returned error: %v", err)
			}
			payload.Result = &AgentResult{Decision: "changes_requested", Summary: "fresh review"}
			if err := (Mailbox{store: store}).savePayload(ctx, job.ID, payload); err != nil {
				t.Fatalf("savePayload returned error: %v", err)
			}
			after, err := store.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if !strings.Contains(after.Payload, `"decision":"changes_requested"`) || strings.Contains(after.Payload, "stale") {
				t.Fatalf("stored payload = %s, want the fresh verdict", after.Payload)
			}

			// And a fresh verdict assigned over a still-decoded announcement
			// (no retry in between) is stored too.
			original, err := DecodeJobPayload(db.Job{ID: "j", Type: "review", Payload: stored})
			if err != nil {
				t.Fatalf("DecodeJobPayload returned error: %v", err)
			}
			original.Result = &AgentResult{Decision: "changes_requested", Summary: "fresh review"}
			encoded, err := marshalPayload(original)
			if err != nil {
				t.Fatalf("marshalPayload returned error: %v", err)
			}
			if !strings.Contains(encoded, `"decision":"changes_requested"`) || strings.Contains(encoded, "stale") {
				t.Fatalf("encoded = %s, want the fresh verdict over the decoded announcement", encoded)
			}
		})
	}
}
