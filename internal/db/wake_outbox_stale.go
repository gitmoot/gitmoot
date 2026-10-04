package db

import (
	"context"
	"fmt"
	"time"
)

// StaleNotificationRole summarizes one role's notifications that have stayed
// pending at least as long as the stale threshold (#2303). It is a read-only
// projection: nothing here changes a delivery state or schedules a retry.
type StaleNotificationRole struct {
	Role  string
	Count int
	// OldestCreatedAt is when the oldest stale notification for the role was
	// created; its age is how long the role has gone without being told.
	OldestCreatedAt time.Time
	// LastError is the most recently recorded reason a stale row for the role
	// was not delivered, or "" when no attempt or deferral was ever recorded.
	LastError string
}

// ListStaleNotifications groups pending notification rows created at or
// before staleBefore by target role, oldest role first. Only `pending` rows
// count: delivered, superseded, failed, stalled and delivery-unknown rows have
// an outcome, and an `attempted` row is in flight right now.
//
// Times are compared through julianday so a row stamped by SQLite
// (millisecond layout) and a cutoff formatted by Go compare as instants, not as
// strings whose fractional-second widths differ.
func (s *Store) ListStaleNotifications(ctx context.Context, staleBefore time.Time) ([]StaleNotificationRole, error) {
	cutoff := staleBefore.UTC().Format(BlockedEpisodeTimeLayout)
	rows, err := s.db.QueryContext(ctx, `
SELECT stale.target_role, COUNT(*), MIN(stale.created_at),
	COALESCE((
		SELECT reason.last_error FROM wake_outbox reason
		WHERE reason.state = 'pending'
			AND reason.target_role = stale.target_role
			AND reason.last_error != ''
			AND julianday(reason.created_at) <= julianday(?)
		ORDER BY julianday(reason.updated_at) DESC, reason.id DESC
		LIMIT 1
	), '')
FROM wake_outbox stale
WHERE stale.state = 'pending' AND julianday(stale.created_at) <= julianday(?)
GROUP BY stale.target_role
ORDER BY MIN(stale.created_at), stale.target_role`, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StaleNotificationRole{}
	for rows.Next() {
		var row StaleNotificationRole
		var oldest string
		if err := rows.Scan(&row.Role, &row.Count, &oldest, &row.LastError); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, oldest)
		if err != nil {
			return nil, fmt.Errorf("parse stale notification created_at %q for role %q: %w", oldest, row.Role, err)
		}
		row.OldestCreatedAt = parsed.UTC()
		out = append(out, row)
	}
	return out, rows.Err()
}
