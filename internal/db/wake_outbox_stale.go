package db

import (
	"context"
	"fmt"
	"time"
)

// ListStaleNotifications returns, in Pending, the pending obligations created
// at or before staleBefore (#2303); the other projection fields are left empty.
// Only `pending` rows count: delivered, superseded, failed, stalled and
// delivery-unknown rows have an outcome, and an `attempted` row is in flight
// right now.
//
// It narrows the same obligation projection the daemon's drain classifies, so a
// caller can set aside the rows the daemon leaves pending on purpose (a muted
// route, or no delivery rule at all, #2309) with the daemon's own rules. It is
// read-only: nothing here changes a delivery state or schedules a retry.
//
// created_at is compared as an instant, not as text, so a row stamped by SQLite
// (millisecond layout) and a cutoff formatted by Go agree whatever their
// fractional-second widths.
func (s *Store) ListStaleNotifications(ctx context.Context, staleBefore time.Time) (WakeOutboxObligationProjection, error) {
	projection, err := s.ListWakeOutboxObligations(ctx, staleBefore)
	if err != nil {
		return WakeOutboxObligationProjection{}, err
	}
	cutoff := staleBefore.UTC()
	stale := WakeOutboxObligationProjection{Pending: []WakeOutboxObligation{}}
	for _, obligation := range projection.Pending {
		created, err := time.Parse(time.RFC3339Nano, obligation.CreatedAt)
		if err != nil {
			return WakeOutboxObligationProjection{}, fmt.Errorf("parse stale notification created_at %q for row %d: %w", obligation.CreatedAt, obligation.ID, err)
		}
		if created.After(cutoff) {
			continue
		}
		stale.Pending = append(stale.Pending, obligation)
	}
	return stale, nil
}
