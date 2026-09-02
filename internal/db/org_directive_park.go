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

const malformedOrgDirectiveParkReason = "malformed directive marker"

// ParkOpenOrgDirectivesForRole parks every OPEN directive obligation addressed
// to targetRole that is not already parked. Done and cancelled directives are
// untouched (they carry no ladder to suspend). Returns how many rows parked.
//
// The EXISTS clause on org_role_archived makes a stale-snapshot park
// SELF-INVALIDATE AT THE WRITE (#1643 round 5, kimi's finding): a park racing
// an unarchive transition would otherwise re-park directives whose mirror row
// the atomic transition just deleted — and with the row gone, no later tick
// could ever unpark them, because the mirror row is the unpark retry key.
// Requiring the row to exist AT THE MOMENT OF THE WRITE closes that regardless
// of caller topology or scheduling. Before this clause the invariant held only
// because the #556 daemon.lock flock serialises writers — correctness by
// scheduling, which is not a design property and fires the moment concurrency
// assumptions change.
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
	AND EXISTS (SELECT 1 FROM org_role_archived WHERE role = ?)
	AND NOT EXISTS (
		SELECT 1 FROM workflow_notes r
		WHERE r.workflow_id = workflow_notes.workflow_id AND (
			substr(r.body, 1, length('[org:directive-cancel id=' || workflow_notes.id || ' ')) = '[org:directive-cancel id=' || workflow_notes.id || ' '
			OR substr(r.body, 1, length('[org:directive-done id=' || workflow_notes.id || ' ')) = '[org:directive-done id=' || workflow_notes.id || ' '
		)
	)`,
		stamp, strings.TrimSpace(reason), targetRole, targetRole, targetRole)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ParkMalformedOrgDirective removes a syntactically invalid directive from the
// live TTL sweep without claiming that its obligation completed.
func (s *Store) ParkMalformedOrgDirective(ctx context.Context, id int64, at time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE workflow_notes
SET directive_parked_at = ?, directive_parked_reason = ?
WHERE id = ?
	AND TRIM(directive_parked_at) = ''
	AND substr(body, 1, length('[org:directive ')) = '[org:directive '
	AND NOT EXISTS (
		SELECT 1 FROM workflow_notes r
		WHERE r.workflow_id = workflow_notes.workflow_id AND (
			substr(r.body, 1, length('[org:directive-cancel id=' || workflow_notes.id || ' ')) = '[org:directive-cancel id=' || workflow_notes.id || ' '
			OR substr(r.body, 1, length('[org:directive-done id=' || workflow_notes.id || ' ')) = '[org:directive-done id=' || workflow_notes.id || ' '
		)
	)`,
		at.UTC().Format(time.RFC3339Nano), malformedOrgDirectiveParkReason, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
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
	AND TRIM(directive_parked_at) <> ''
	AND directive_parked_reason <> ?`,
		stamp, targetRole, targetRole, malformedOrgDirectiveParkReason)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// orgUnarchiveTransitionTestHook, when set, runs INSIDE the transition
// transaction just before commit. Tests use it to prove the transaction is
// genuinely atomic: a hook error must roll back BOTH the unpark and the
// mirror-row delete, leaving the archived state fully intact.
var orgUnarchiveTransitionTestHook func() error

// UnarchiveOrgSeatTransition completes an observed archived->active
// transition ATOMICALLY: unpark the role's directives (nudge anchors reset to
// the transition time) and delete its mirror row in ONE transaction. Atomicity
// is the whole point (#1643 round 3): a partial transition — unparked
// directives with the row gone, or the reverse — is an inconsistent state that
// a later list OMISSION would preserve forever, because retries are driven by
// the durable mirror row. All-or-nothing means a failure leaves the archived
// state fully in force and the next positive-evidence tick retries the whole
// transition.
func (s *Store) UnarchiveOrgSeatTransition(ctx context.Context, targetRole string, at time.Time) (int64, error) {
	targetRole = strings.ToLower(strings.TrimSpace(targetRole))
	if targetRole == "" {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stamp := at.UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `
UPDATE workflow_notes
SET directive_parked_at = '', directive_parked_reason = '', directive_last_nudged_at = ?
WHERE substr(body, 1, length('[org:directive to=' || ? || ' ')) = '[org:directive to=' || ? || ' '
	AND TRIM(directive_parked_at) <> ''
	AND directive_parked_reason <> ?`,
		stamp, targetRole, targetRole, malformedOrgDirectiveParkReason)
	if err != nil {
		return 0, err
	}
	unparked, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM org_role_archived WHERE role = ?`, targetRole); err != nil {
		return 0, err
	}
	// The pending row dies IN THE SAME TRANSACTION (#1643 round 7): the
	// supersede of a stale observation is atomic with the transition that
	// consumes it, so a contradicted pending row can never outlive the
	// positive evidence that contradicted it and be re-applied by a later
	// omission tick. Round 6 discarded contradicted rows with a separate
	// DELETE — a write whose failure left the contradiction living only in
	// tick memory, which is adversary 4 one level up.
	if _, err := tx.ExecContext(ctx, `DELETE FROM org_archive_pending WHERE role = ?`, targetRole); err != nil {
		return 0, err
	}
	if orgUnarchiveTransitionTestHook != nil {
		if err := orgUnarchiveTransitionTestHook(); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return unparked, nil
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
