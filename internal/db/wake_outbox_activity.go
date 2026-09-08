package db

import (
	"context"
	"strings"
	"time"
)

// WakeOutboxActivity is one wake row reduced to the fields an interrupt-rate
// report needs (#1983). The report aggregates in Go rather than in SQL because
// a median gap needs the ordered arrival series per role, not a scalar, and
// because the rows are already bounded by the window.
type WakeOutboxActivity struct {
	ID          int64
	TargetRole  string
	SourceKind  string
	CoalesceKey string
	State       string
	CreatedAt   string
	// Collapsed marks a row that another row's wake carried (#1978). It is the
	// per-row form of the coalescing saving, so a report can state how many
	// interrupts were avoided rather than only how many happened.
	Collapsed bool
}

// ListWakeOutboxActivity returns every wake row created at or after `since`,
// oldest first. An empty `since` window returns the whole table.
func (s *Store) ListWakeOutboxActivity(ctx context.Context, since time.Time) ([]WakeOutboxActivity, error) {
	query := `
SELECT id, target_role, source_kind, coalesce_key, state, created_at, last_error
FROM wake_outbox`
	var args []any
	if !since.IsZero() {
		query += "\nWHERE created_at >= ?"
		args = append(args, since.UTC().Format(BlockedEpisodeTimeLayout))
	}
	query += "\nORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	activity := []WakeOutboxActivity{}
	for rows.Next() {
		var entry WakeOutboxActivity
		var lastError string
		if err := rows.Scan(
			&entry.ID, &entry.TargetRole, &entry.SourceKind,
			&entry.CoalesceKey, &entry.State, &entry.CreatedAt, &lastError,
		); err != nil {
			return nil, err
		}
		entry.Collapsed = strings.HasPrefix(lastError, wakeOutboxCoalescedPrefix)
		activity = append(activity, entry)
	}
	return activity, rows.Err()
}

// OrgDirectiveNudgeRow carries the persisted nudge ladders for one directive.
// The target role lives in the body marker, so the caller parses it with the
// same parser the evaluator uses instead of a second SQL implementation.
type OrgDirectiveNudgeRow struct {
	ID               int64
	Body             string
	AckNudges        int
	CompletionNudges int
	CreatedAt        string
}

// ListOrgDirectiveNudges returns directives created at or after `since` with
// their persisted nudge counts, so a report can state how many completion nags
// a seat was actually sent (#1979's population) without hand-written SQL.
func (s *Store) ListOrgDirectiveNudges(ctx context.Context, since time.Time) ([]OrgDirectiveNudgeRow, error) {
	query := `
SELECT id, body, directive_nudge_count, directive_done_nudge_count, created_at
FROM workflow_notes
WHERE substr(body, 1, length('[org:directive ')) = '[org:directive '`
	var args []any
	if !since.IsZero() {
		query += "\n\tAND created_at >= ?"
		args = append(args, since.UTC().Format(BlockedEpisodeTimeLayout))
	}
	query += "\nORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrgDirectiveNudgeRow{}
	for rows.Next() {
		var row OrgDirectiveNudgeRow
		if err := rows.Scan(&row.ID, &row.Body, &row.AckNudges, &row.CompletionNudges, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
