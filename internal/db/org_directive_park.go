package db

import (
	"context"
	"strings"
	"time"
)

// Directive parking (#1635, herdrup#173). Parking SUSPENDS a directive's nudge
// ladder while its target seat is out of rotation. It is deliberately neither
// `done` (which asserts a deliverable exists) nor `cancel` (which discards the
// obligation): a parked directive stays open and queryable, its ladder frozen,
// and returns to the live sweep on unpark. That preserves #1418's exhaustion
// discrimination — an archived seat's directives must not exhaust into the
// background rate.
//
// The park stamp lives in ladder columns (like directive_exhausted_at), not in
// a done/cancel-style marker note, because it records LADDER state rather than
// obligation completion.

// ParkOpenOrgDirectivesForRole parks every OPEN directive obligation addressed
// to targetRole that is not already parked. Done and cancelled directives are
// untouched (they carry no ladder to suspend). Returns how many rows parked.
func (s *Store) ParkOpenOrgDirectivesForRole(ctx context.Context, targetRole string, at time.Time, reason string) (int64, error) {
	targetRole = strings.ToLower(strings.TrimSpace(targetRole))
	if targetRole == "" {
		return 0, nil
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
UPDATE workflow_notes
SET directive_parked_at = ?, directive_parked_reason = ?
WHERE substr(body, 1, length('[org:directive to=' || ? || ' ')) = '[org:directive to=' || ? || ' '
	AND TRIM(directive_parked_at) = ''
	AND NOT EXISTS (
		SELECT 1 FROM workflow_notes r
		WHERE r.workflow_id = workflow_notes.workflow_id AND (
			substr(r.body, 1, length('[org:directive-cancel id=' || workflow_notes.id || ' ')) = '[org:directive-cancel id=' || workflow_notes.id || ' '
			OR substr(r.body, 1, length('[org:directive-done id=' || workflow_notes.id || ' ')) = '[org:directive-done id=' || workflow_notes.id || ' '
		)
	)`,
		stamp, strings.TrimSpace(reason), targetRole, targetRole)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// UnparkOrgDirectivesForRole returns targetRole's parked directives to the live
// sweep. The nudge anchor (directive_last_nudged_at) is reset to the unpark
// time: a directive whose TTL elapsed while its seat was archived must get a
// full fresh TTL from unpark, not nag immediately (#1635 ruling).
func (s *Store) UnparkOrgDirectivesForRole(ctx context.Context, targetRole string, at time.Time) (int64, error) {
	targetRole = strings.ToLower(strings.TrimSpace(targetRole))
	if targetRole == "" {
		return 0, nil
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
UPDATE workflow_notes
SET directive_parked_at = '', directive_parked_reason = '', directive_last_nudged_at = ?
WHERE substr(body, 1, length('[org:directive to=' || ? || ' ')) = '[org:directive to=' || ? || ' '
	AND TRIM(directive_parked_at) <> ''`,
		stamp, targetRole, targetRole)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ListParkedOrgDirectives returns parked directive obligations oldest-first,
// for the archived-seat backlog view. An empty targetRole lists every parked
// directive.
func (s *Store) ListParkedOrgDirectives(ctx context.Context, targetRole string) ([]OrgDirectiveObligation, error) {
	targetRole = strings.ToLower(strings.TrimSpace(targetRole))
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id, d.workflow_id, d.author, d.body, d.repo, d.memory_observation_id, d.created_at,
	d.directive_nudge_count, d.directive_last_nudged_at, d.directive_done_ttl_seconds,
	d.directive_done_nudge_count, d.directive_exhausted_at,
	d.directive_parked_at, d.directive_parked_reason,
	COALESCE((
		SELECT MIN(a.created_at) FROM workflow_notes a
		WHERE a.workflow_id = d.workflow_id
			AND substr(a.body, 1, length('[org:directive-ack id=' || d.id || ' ')) = '[org:directive-ack id=' || d.id || ' '
	), '')
FROM workflow_notes d
WHERE substr(d.body, 1, length('[org:directive ')) = '[org:directive '
	AND TRIM(d.directive_parked_at) <> ''
	AND (? = '' OR substr(d.body, 1, length('[org:directive to=' || ? || ' ')) = '[org:directive to=' || ? || ' ')
ORDER BY d.created_at ASC, d.id ASC`, targetRole, targetRole, targetRole)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	obligations := make([]OrgDirectiveObligation, 0)
	for rows.Next() {
		var item OrgDirectiveObligation
		if err := rows.Scan(
			&item.ID, &item.WorkflowID, &item.Author, &item.Body, &item.Repo,
			&item.MemoryObservationID, &item.CreatedAt, &item.NudgeCount,
			&item.LastNudgedAt, &item.DoneTTLOverrideSeconds,
			&item.DoneNudgeCount, &item.ExhaustedAt,
			&item.ParkedAt, &item.ParkedReason, &item.AckedAt,
		); err != nil {
			return nil, err
		}
		obligations = append(obligations, item)
	}
	return obligations, rows.Err()
}
