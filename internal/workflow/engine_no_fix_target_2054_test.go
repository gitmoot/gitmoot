package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestChangesRequestedHonoursNoFixTarget is review finding F1 on #2064, and it
// is the half that made the flag a TRAP rather than merely incomplete.
//
// --no-fix-target declares that a review has no implementer and the dispatching
// operator owns the follow-up. Clearing the lead at dispatch was not enough:
// enqueue restored the reviewer through firstNonEmpty, and advancement had no
// field to read the declaration from. So a changes_requested verdict would have
// dispatched its fix leg to an implementer the operator had declined to name -
// and where the fallback resolved to the reviewer, to the very agent that
// produced the verdict, which is the independence violation the lead
// requirement exists to prevent.
//
// The declaration therefore has to live on the PAYLOAD and be honoured HERE,
// where the leg is dispatched, not only at the CLI seam.
func TestChangesRequestedHonoursNoFixTarget(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	enableAutoFix(t, store, 1678)

	// A review dispatched --no-fix-target: no lead, and the declaration carried
	// on its own payload.
	// Mirrors insertPriorReviewResult exactly, EXCEPT no LeadAgent and
	// NoFixTarget set: the difference is the subject.
	result := AgentResult{
		Decision: "changes_requested",
		Summary:  "A finding with nobody assigned to fix it.",
		Findings: []json.RawMessage{
			json.RawMessage(`{"id":"F-1","file":"internal/boundary.go","summary":"Unguarded on the failure path."}`),
		},
	}
	insertCompletedJob(t, store, db.Job{ID: "review-only-verdict", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		HeadSHA:     "head-one",
		TaskID:      "task-1678",
		TaskTitle:   "Bound native review fanout",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		NoFixTarget: true,
		Result:      &result,
	})
	if err := engine.AdvanceJob(ctx, "review-only-verdict"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}

	if _, err := store.GetJob(ctx, "implement-lead-task-1678-review-1"); err == nil {
		t.Fatal("a fix leg was dispatched for a review declared --no-fix-target; " +
			"the operator declined to name an implementer and the fallback is the reviewer itself")
	}

	// THE SKIP MUST BE ON THE RECORD, for the same reason the active-leg skip is:
	// an operator reads "no fix leg" as "nothing to fix" unless the reason is
	// findable.
	events, err := store.ListJobEvents(ctx, "review-only-verdict")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var skip db.JobEvent
	for _, event := range events {
		if event.Kind == "auto_fix_skipped_no_fix_target" {
			skip = event
		}
	}
	if skip.Kind == "" {
		t.Fatalf("no auto_fix_skipped_no_fix_target event; the skip is invisible. events = %+v", events)
	}
	for _, want := range []string{"no-fix-target", "findings stay open"} {
		if !strings.Contains(skip.Message, want) {
			t.Fatalf("skip message %q does not name %q", skip.Message, want)
		}
	}
}
