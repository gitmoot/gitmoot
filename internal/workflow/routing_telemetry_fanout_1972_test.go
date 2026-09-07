package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #1972. routing_telemetry recorded a fan-out COORDINATOR's decision as if it
// were a verdict about code, and the fleet's one instrument for comparing
// reviewer configurations was therefore mixing announcements with judgements.
//
// THIS IS NOT HYPOTHETICAL: it produced a filed conclusion. #1972 reported that
// the review-panel template moves codex from 81% changes_requested to 17%.
// Measured on this box: of 93 review-panel rows, 76 were approvals and 70 of
// those declared delegations - coordinator dispatch records, not answers.
// Excluding them leaves 22 real verdicts at 72.7%, and the panel's own lens
// children at 77.8%, neither distinguishable from the untemplated 81.5% on
// those sample sizes. The entire effect was the artifact.
//
// The engine already draws this line. ResultIsFanOut exists because "a FAN-OUT
// is a coordinator's dispatch record, not an answer about the code", and the
// merge gate uses it to keep such rows out of its verdict population. The
// telemetry did not, so the gate and the instrument disagreed about what counts
// as an answer. This pins that they now agree.
func TestRoutingTelemetryMarksAFanOutAsAnAnnouncementNotAVerdict(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))

	for _, tc := range []struct {
		name   string
		jobID  string
		result AgentResult
		want   bool
	}{
		{
			name:  "a coordinator announcing a fan-out is not a verdict",
			jobID: "coordinator-row",
			result: AgentResult{
				Decision: "approved",
				Summary:  "dispatched three refutation lenses",
				Delegations: []Delegation{
					{ID: "lens-a", Agent: "reviewer", Action: "review", Prompt: "disprove a"},
					{ID: "lens-b", Agent: "reviewer", Action: "review", Prompt: "disprove b"},
				},
			},
			want: true,
		},
		{
			name:   "an ordinary review verdict is a verdict",
			jobID:  "verdict-row",
			result: AgentResult{Decision: "changes_requested", Summary: "found a defect", Severity: "P2"},
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := db.Job{ID: tc.jobID, Agent: "g6-review-sol", Type: "review"}
			mailbox.recordRoutingTelemetry(ctx, job, runtime.Agent{Runtime: "codex"},
				JobPayload{Repo: "gitmoot/gitmoot", TemplateID: "review-panel"},
				tc.result, JobSucceeded, time.Second)

			rows, err := store.ListRoutingTelemetry(ctx, db.RoutingTelemetryFilter{})
			if err != nil {
				t.Fatalf("ListRoutingTelemetry: %v", err)
			}
			var found *db.RoutingTelemetry
			for i := range rows {
				if rows[i].JobID == tc.jobID {
					found = &rows[i]
				}
			}
			if found == nil {
				t.Fatalf("no telemetry row for %q", tc.jobID)
			}
			if found.FanOut != tc.want {
				t.Fatalf("fan_out = %v, want %v; a verdict-rate query that cannot tell an announcement from a judgement produced #1972's 81%%-to-17%% finding out of 70 announcements",
					found.FanOut, tc.want)
			}
			// The decision itself is preserved either way: the row still describes
			// itself, exactly as review_threshold.go returns the raw decision for a
			// fan-out rather than suppressing it. Marking is not filtering.
			if found.Decision != tc.result.Decision {
				t.Fatalf("decision = %q, want %q; marking a row must not rewrite what it recorded", found.Decision, tc.result.Decision)
			}
		})
	}
}

// The instrument and the gate must not be able to drift apart. If ResultIsFanOut
// ever changes what counts as a coordinator record, the telemetry has to move
// with it rather than keeping a second, stale copy of the rule.
func TestRoutingTelemetryFanOutUsesTheGatesOwnPredicate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))

	result := AgentResult{
		Decision:    "approved",
		Summary:     "fan-out",
		Delegations: []Delegation{{ID: "lens", Agent: "reviewer", Action: "review", Prompt: "disprove"}},
	}
	if !ResultIsFanOut(&result) {
		t.Fatal("premise broken: the gate no longer classifies this as a fan-out, so this test is asserting nothing")
	}
	mailbox.recordRoutingTelemetry(ctx, db.Job{ID: "row", Agent: "a", Type: "review"},
		runtime.Agent{Runtime: "codex"}, JobPayload{Repo: "r"}, result, JobSucceeded, time.Second)

	rows, err := store.ListRoutingTelemetry(ctx, db.RoutingTelemetryFilter{})
	if err != nil {
		t.Fatalf("ListRoutingTelemetry: %v", err)
	}
	if len(rows) != 1 || !rows[0].FanOut {
		t.Fatalf("rows=%d fan_out=%v; the telemetry must agree with ResultIsFanOut, not keep its own copy of the rule",
			len(rows), len(rows) == 1 && rows[0].FanOut)
	}
}
