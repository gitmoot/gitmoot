package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestAgedSweepHonoursDestinationEvidence is #1958 acceptance item 3.
//
// THE DEFECT. ExpireAgedWakeOutbox writes `delivery_unknown` on any attempted
// row that aged out, without ever asking whether the wake demonstrably LANDED.
// #1911 measured the consequence directly: directive 126123's row sat
// `attempted` while its acknowledgment `[org:directive-ack id=126123 ...]` had
// been recorded about two minutes after the row was created and 78 minutes
// before the row was flagged as undelivered. An instrument built on the row's
// own columns then reports delivered work as an outstanding obligation, and the
// older the row the more confident the false positive looks.
//
// THE SIBLING SEAM. The store ALREADY knows how to read that evidence:
// wakeOutboxObligationQuery, the function immediately above the sweep, joins
// workflow_notes to find `[org:directive-ack ...]` and
// `[org:directive-delivered ...]` markers to pick a directive's phase. The
// sweep never consults it. One function apart, and only one of them asks.
func TestAgedSweepHonoursDestinationEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		evidence  string
		wantState string
	}{
		{name: "acknowledged", evidence: "[org:directive-ack id=%d by=worker]", wantState: WakeOutboxStateDelivered},
		{name: "delivered-marker", evidence: "[org:directive-delivered id=%d to=worker]", wantState: WakeOutboxStateDelivered},
		{name: "no-evidence", evidence: "", wantState: WakeOutboxStateDeliveryUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()

			directive, err := store.InsertWorkflowNote(ctx, WorkflowNote{
				WorkflowID: "release/directives", Author: "coordinator",
				Body: "[org:directive to=worker] do the thing",
			})
			if err != nil {
				t.Fatalf("seed directive: %v", err)
			}
			if test.evidence != "" {
				if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
					WorkflowID: "release/directives", Author: "worker",
					Body: fmt.Sprintf(test.evidence, directive.ID),
				}); err != nil {
					t.Fatalf("seed evidence: %v", err)
				}
			}

			attemptedAt := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
			stamp := attemptedAt.Format(BlockedEpisodeTimeLayout)
			if _, err := store.db.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, created_at, updated_at, attempted_at)
VALUES ('workflow_note', ?, 'worker', 'directive:worker', 'attempted', 1, ?, ?, ?)`,
				fmt.Sprint(directive.ID), stamp, stamp, stamp); err != nil {
				t.Fatalf("seed wake: %v", err)
			}

			if _, err := store.ExpireAgedWakeOutbox(ctx, attemptedAt.Add(time.Hour), attemptedAt.Add(2*time.Hour)); err != nil {
				t.Fatalf("ExpireAgedWakeOutbox: %v", err)
			}

			rows, err := store.ListWakeOutbox(ctx, test.wantState)
			if err != nil {
				t.Fatalf("ListWakeOutbox: %v", err)
			}
			if len(rows) != 1 {
				all, _ := store.ListWakeOutbox(ctx, "")
				t.Fatalf("rows in %s = %d, want 1; every row = %+v", test.wantState, len(rows), all)
			}
			if test.wantState == WakeOutboxStateDelivered && rows[0].LastError == "" {
				t.Fatalf("a row resolved by destination evidence records no reason: %+v", rows[0])
			}

			// THE EVENT STREAM MUST NOT CALL A PROVEN DELIVERY UNKNOWN. An
			// operator reads the stream, not the row, and a resolved obligation
			// recorded as `wake_delivery_unknown` reproduces this defect
			// downstream of the fix.
			events, err := store.ListJobEvents(ctx, fmt.Sprintf("wake-outbox:%d", rows[0].ID))
			if err != nil {
				t.Fatalf("ListJobEvents: %v", err)
			}
			wantKind := WakeOutboxDeliveryUnknownEventKind
			if test.wantState == WakeOutboxStateDelivered {
				wantKind = WakeOutboxDeliveredEventKind
			}
			var kinds []string
			for _, event := range events {
				kinds = append(kinds, event.Kind)
			}
			if len(events) != 1 || events[0].Kind != wantKind {
				t.Fatalf("job events = %v, want exactly one %q", kinds, wantKind)
			}
		})
	}
}

// TestAgedSweepDoesNotLaunderAReplyRowWithADirectiveAck pins the guard that
// mutant-survived without it.
//
// The evidence subquery joins on `source_id` cast to a note id, and a REPLY
// row's source is also a note. So without the directive-class restriction, a
// reply wake whose source note happens to share a workflow with some
// directive's acknowledgment would be marked delivered on evidence that is
// about a different obligation entirely. A reply has no acknowledgment marker
// of its own, so its unknown outcome is the honest one.
func TestAgedSweepDoesNotLaunderAReplyRowWithADirectiveAck(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()

	// One workflow holding BOTH a reply-class source note and a directive with
	// its own acknowledgment - the shape that makes an unguarded join wrong.
	source, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "release/mixed", Author: "worker", Body: "a plain reply-worthy note",
	})
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "release/mixed", Author: "worker",
		Body: fmt.Sprintf("[org:directive-ack id=%d by=worker]", source.ID),
	}); err != nil {
		t.Fatalf("seed ack: %v", err)
	}

	attemptedAt := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	stamp := attemptedAt.Format(BlockedEpisodeTimeLayout)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, created_at, updated_at, attempted_at)
VALUES ('workflow_note', ?, 'owner', 'reply:owner', 'attempted', 1, ?, ?, ?)`,
		fmt.Sprint(source.ID), stamp, stamp, stamp); err != nil {
		t.Fatalf("seed reply wake: %v", err)
	}

	if _, err := store.ExpireAgedWakeOutbox(ctx, attemptedAt.Add(time.Hour), attemptedAt.Add(2*time.Hour)); err != nil {
		t.Fatalf("ExpireAgedWakeOutbox: %v", err)
	}

	unknown, err := store.ListWakeOutbox(ctx, WakeOutboxStateDeliveryUnknown)
	if err != nil {
		t.Fatalf("ListWakeOutbox: %v", err)
	}
	if len(unknown) != 1 {
		delivered, _ := store.ListWakeOutbox(ctx, WakeOutboxStateDelivered)
		t.Fatalf("reply row in delivery_unknown = %d, want 1; delivered = %+v", len(unknown), delivered)
	}
}

// TestAgedSweepRequiresEvidenceForItsOwnDirective pins that the evidence is
// keyed to THE ROW'S directive, not to the existence of any acknowledgment.
//
// This is the case the no-evidence subtest cannot reach: with no markers in the
// store at all, a predicate that ignores the id still finds nothing and looks
// correct. Put ANOTHER directive's acknowledgment in the same workflow and the
// difference becomes observable - an unacknowledged directive must stay
// unknown even while its neighbour is acknowledged.
func TestAgedSweepRequiresEvidenceForItsOwnDirective(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()

	unacked, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "release/directives", Author: "coordinator",
		Body: "[org:directive to=worker] the one nobody answered",
	})
	if err != nil {
		t.Fatalf("seed unacked directive: %v", err)
	}
	neighbour, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "release/directives", Author: "coordinator",
		Body: "[org:directive to=worker] the one that was answered",
	})
	if err != nil {
		t.Fatalf("seed neighbour directive: %v", err)
	}
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "release/directives", Author: "worker",
		Body: fmt.Sprintf("[org:directive-ack id=%d by=worker]", neighbour.ID),
	}); err != nil {
		t.Fatalf("seed neighbour ack: %v", err)
	}

	attemptedAt := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	stamp := attemptedAt.Format(BlockedEpisodeTimeLayout)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, created_at, updated_at, attempted_at)
VALUES ('workflow_note', ?, 'worker', 'directive:worker', 'attempted', 1, ?, ?, ?)`,
		fmt.Sprint(unacked.ID), stamp, stamp, stamp); err != nil {
		t.Fatalf("seed wake: %v", err)
	}

	if _, err := store.ExpireAgedWakeOutbox(ctx, attemptedAt.Add(time.Hour), attemptedAt.Add(2*time.Hour)); err != nil {
		t.Fatalf("ExpireAgedWakeOutbox: %v", err)
	}

	unknown, err := store.ListWakeOutbox(ctx, WakeOutboxStateDeliveryUnknown)
	if err != nil {
		t.Fatalf("ListWakeOutbox: %v", err)
	}
	if len(unknown) != 1 {
		delivered, _ := store.ListWakeOutbox(ctx, WakeOutboxStateDelivered)
		t.Fatalf("unacked row in delivery_unknown = %d, want 1: it was resolved by ANOTHER directive's acknowledgment; delivered = %+v",
			len(unknown), delivered)
	}
}

// TestAgedSweepRequiresEvidenceInTheDirectivesOwnWorkflow pins what the
// `d.id = CAST(source_id)` condition actually buys, which is not what it looks
// like it buys.
//
// The marker comparison already keys on `wake_outbox.source_id`, so an ack
// naming a DIFFERENT directive cannot match regardless of that condition -
// which is why a mutant replacing it with `1=1` survived the neighbour test.
// What it really constrains is WHERE the acknowledgment lives: it must be in
// the directive's own workflow. An ack naming this row's directive id from an
// unrelated workflow is not evidence about this obligation, and this test is
// the only thing that says so.
func TestAgedSweepRequiresEvidenceInTheDirectivesOwnWorkflow(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()

	directive, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "release/directives", Author: "coordinator",
		Body: "[org:directive to=worker] do the thing",
	})
	if err != nil {
		t.Fatalf("seed directive: %v", err)
	}
	// The ack names the right id and lives in the WRONG workflow.
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "some/other-workflow", Author: "worker",
		Body: fmt.Sprintf("[org:directive-ack id=%d by=worker]", directive.ID),
	}); err != nil {
		t.Fatalf("seed foreign ack: %v", err)
	}

	attemptedAt := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	stamp := attemptedAt.Format(BlockedEpisodeTimeLayout)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, created_at, updated_at, attempted_at)
VALUES ('workflow_note', ?, 'worker', 'directive:worker', 'attempted', 1, ?, ?, ?)`,
		fmt.Sprint(directive.ID), stamp, stamp, stamp); err != nil {
		t.Fatalf("seed wake: %v", err)
	}

	if _, err := store.ExpireAgedWakeOutbox(ctx, attemptedAt.Add(time.Hour), attemptedAt.Add(2*time.Hour)); err != nil {
		t.Fatalf("ExpireAgedWakeOutbox: %v", err)
	}

	unknown, err := store.ListWakeOutbox(ctx, WakeOutboxStateDeliveryUnknown)
	if err != nil {
		t.Fatalf("ListWakeOutbox: %v", err)
	}
	if len(unknown) != 1 {
		delivered, _ := store.ListWakeOutbox(ctx, WakeOutboxStateDelivered)
		t.Fatalf("row in delivery_unknown = %d, want 1: an ack from a foreign workflow was accepted as evidence; delivered = %+v",
			len(unknown), delivered)
	}
}
