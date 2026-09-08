package db

import (
	"context"
	"strings"
	"testing"
)

// TestBlockedReasonSkipsDiagnosticEvents is #1824 review F6. The blocked-jobs
// fallback took the job's NEWEST event message regardless of kind, so the
// phase_profile row emitted after a job's terminal event was displayed to an
// operator as the reason the job is blocked: the dashboard's Needs You card
// showed accounting JSON instead of the actionable blocker.
//
// This is the reader I claimed did not exist. My search required an unaliased
// `ORDER BY id DESC`, and this query writes `ORDER BY e.id DESC`, so the
// instrument I used to clear the design decision could not have found it.
func TestBlockedReasonSkipsDiagnosticEvents(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()

	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "blocked-1", Agent: "reviewer", Type: "review", State: "blocked", Payload: "{}",
	}, JobEvent{Kind: "queued", Message: "job queued"}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	const humanReason = "awaiting a human decision"
	for _, event := range []JobEvent{
		{JobID: "blocked-1", Kind: "blocked", Message: humanReason},
		// The diagnostic row lands LAST, which is exactly the production
		// ordering: the profile is emitted at transcript close.
		{JobID: "blocked-1", Kind: PhaseProfileEventKind, Message: `{"coverage":"decomposed","wall_ms":1234}`},
	} {
		if err := store.AddJobEvent(ctx, event); err != nil {
			t.Fatalf("add %s: %v", event.Kind, err)
		}
	}

	rows, err := store.ListDashboardBlockedJobs(ctx)
	if err != nil {
		t.Fatalf("list blocked: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Reason != humanReason {
		t.Fatalf("reason = %q, want %q - a diagnostic event must never be shown as a blocker", rows[0].Reason, humanReason)
	}
	if strings.Contains(rows[0].Reason, "wall_ms") {
		t.Fatalf("reason leaked accounting JSON: %q", rows[0].Reason)
	}
}
