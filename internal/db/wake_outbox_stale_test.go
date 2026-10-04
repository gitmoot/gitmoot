package db

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestListStaleNotificationsListsPendingRowsPastThreshold pins the stale
// projection the dashboard, doctor and daemon status share (#2303): a row
// exactly at the threshold is stale and one a millisecond younger is not; only
// pending rows count; rows come oldest first and carry their recorded reason,
// so the caller can group them after classifying each against the delivery
// rules (#2309).
func TestListStaleNotificationsListsPendingRowsPastThreshold(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	cutoff := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	stamp := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }
	seed := func(role, state, lastError string, created, updated time.Time) {
		t.Helper()
		note, err := store.InsertWorkflowNote(ctx, WorkflowNote{
			WorkflowID: "stale/" + role, Author: "owner", Body: "notice for " + role,
			AddressedTarget: role,
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.db.ExecContext(ctx, `
UPDATE wake_outbox SET state = ?, last_error = ?, created_at = ?, updated_at = ?
WHERE source_kind = 'workflow_note' AND source_id = ? AND target_role = ?`,
			state, lastError, stamp(created), stamp(updated), note.ID, role)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			t.Fatalf("seed %s/%s updated %d rows, want 1", role, state, n)
		}
	}

	// alpha: one row exactly at the threshold, one an hour older whose reason
	// was recorded later, and one a millisecond too young whose newer reason
	// must not leak into the stale summary.
	seed("alpha", WakeOutboxStatePending, "recipient offline", cutoff, cutoff.Add(time.Minute))
	seed("alpha", WakeOutboxStatePending, "operator active", cutoff.Add(-time.Hour), cutoff.Add(2*time.Minute))
	seed("alpha", WakeOutboxStatePending, "too young to count", cutoff.Add(time.Millisecond), cutoff.Add(3*time.Minute))
	// beta: old rows that already have an outcome or are in flight.
	seed("beta", WakeOutboxStateDelivered, "", cutoff.Add(-3*time.Hour), cutoff)
	seed("beta", WakeOutboxStateSuperseded, "coalesced into wake outbox row 1", cutoff.Add(-3*time.Hour), cutoff)
	seed("beta", WakeOutboxStateAttempted, "", cutoff.Add(-3*time.Hour), cutoff)
	seed("beta", WakeOutboxStateDeliveryUnknown, "receipt lost", cutoff.Add(-3*time.Hour), cutoff)
	// gamma: stale, never deferred or attempted.
	seed("gamma", WakeOutboxStatePending, "", cutoff.Add(-5*time.Minute), cutoff.Add(-5*time.Minute))
	// delta: only a fresh row.
	seed("delta", WakeOutboxStatePending, "recipient offline", cutoff.Add(time.Second), cutoff.Add(time.Second))

	got, err := store.ListStaleNotifications(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	type staleRow struct{ Role, LastError, CreatedAt, UpdatedAt string }
	rows := make([]staleRow, 0, len(got.Pending))
	for _, row := range got.Pending {
		rows = append(rows, staleRow{row.TargetRole, row.LastError, row.CreatedAt, row.UpdatedAt})
	}
	want := []staleRow{
		{"alpha", "operator active", stamp(cutoff.Add(-time.Hour)), stamp(cutoff.Add(2 * time.Minute))},
		{"gamma", "", stamp(cutoff.Add(-5 * time.Minute)), stamp(cutoff.Add(-5 * time.Minute))},
		{"alpha", "recipient offline", stamp(cutoff), stamp(cutoff.Add(time.Minute))},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ListStaleNotifications() =\n%+v\nwant\n%+v", rows, want)
	}

	// Delivering every stale row clears it from the listing.
	if _, err := store.db.ExecContext(ctx, `UPDATE wake_outbox SET state = 'delivered' WHERE target_role IN ('alpha', 'gamma') AND state = 'pending' AND created_at <= ?`, stamp(cutoff)); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListStaleNotifications(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pending) != 0 {
		t.Fatalf("after delivery ListStaleNotifications() = %+v, want none", got.Pending)
	}
}
