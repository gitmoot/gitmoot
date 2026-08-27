package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The #1635 archived-seat mirror. herdr owns archive state; these rows cache
// the last successful observation so roster exclusions survive a herdr outage
// (preserved exclusion — the deliberate fail direction). One write site: the
// daemon one-minute org lane, after a successful `herdr agent list` read.

// OrgRoleArchived is one observed archived seat.
type OrgRoleArchived struct {
	Role       string
	ArchivedAt string
	ArchivedBy string
	Reason     string
	// ParkedWork is the verbatim JSON array herdr stored at archive time
	// (structural round-trip per the confirmed contract). Opaque here; the
	// `org seats archived` view renders it.
	ParkedWork string
	ObservedAt string
}

// UpsertOrgRoleArchived records or refreshes one observed archived seat.
func (s *Store) UpsertOrgRoleArchived(ctx context.Context, row OrgRoleArchived) error {
	role := strings.ToLower(strings.TrimSpace(row.Role))
	if role == "" {
		return errors.New("archived seat role must not be empty")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO org_role_archived (role, archived_at, archived_by, reason, parked_work, observed_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(role) DO UPDATE SET
	archived_at = excluded.archived_at,
	archived_by = excluded.archived_by,
	reason = excluded.reason,
	parked_work = excluded.parked_work,
	observed_at = excluded.observed_at`,
		role, row.ArchivedAt, row.ArchivedBy, row.Reason, row.ParkedWork, row.ObservedAt)
	return err
}

// DeleteOrgRoleArchived removes a mirror row after an observed unarchive.
// Reports whether a row existed (the archived->active transition signal).
func (s *Store) DeleteOrgRoleArchived(ctx context.Context, role string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM org_role_archived WHERE role = ?`,
		strings.ToLower(strings.TrimSpace(role)))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// ListOrgRolesArchived returns every observed archived seat, role-ordered.
func (s *Store) ListOrgRolesArchived(ctx context.Context) ([]OrgRoleArchived, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT role, archived_at, archived_by, reason, parked_work, observed_at
FROM org_role_archived ORDER BY role ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OrgRoleArchived, 0)
	for rows.Next() {
		var row OrgRoleArchived
		if err := rows.Scan(&row.Role, &row.ArchivedAt, &row.ArchivedBy, &row.Reason, &row.ParkedWork, &row.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// MergeOrgArchivePending records observed archived seats into the pending
// ledger (#1643 round 4) — the tick's FIRST write, in one transaction, so the
// observation is durable before any mirror write can fail. Existing pending
// rows for the same role are refreshed; rows for roles NOT in this batch are
// left alone (they are prior observations still awaiting application — a
// REPLACE here would re-create the exact loss this table closes).
func (s *Store) MergeOrgArchivePending(ctx context.Context, rows []OrgRoleArchived) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range rows {
		role := strings.ToLower(strings.TrimSpace(row.Role))
		if role == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO org_archive_pending (role, archived_at, archived_by, reason, parked_work, observed_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(role) DO UPDATE SET
	archived_at = excluded.archived_at,
	archived_by = excluded.archived_by,
	reason = excluded.reason,
	parked_work = excluded.parked_work,
	observed_at = excluded.observed_at`,
			role, row.ArchivedAt, row.ArchivedBy, row.Reason, row.ParkedWork, row.ObservedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListOrgArchivePending returns every observation still awaiting application
// to the mirror, role-ordered.
func (s *Store) ListOrgArchivePending(ctx context.Context) ([]OrgRoleArchived, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT role, archived_at, archived_by, reason, parked_work, observed_at
FROM org_archive_pending ORDER BY role ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OrgRoleArchived, 0)
	for rows.Next() {
		var row OrgRoleArchived
		if err := rows.Scan(&row.Role, &row.ArchivedAt, &row.ArchivedBy, &row.Reason, &row.ParkedWork, &row.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// DeleteOrgArchivePending removes one applied pending observation.
func (s *Store) DeleteOrgArchivePending(ctx context.Context, role string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_archive_pending WHERE role = ?`,
		strings.ToLower(strings.TrimSpace(role)))
	return err
}

// RecordOrgArchivePollSuccess stamps the last successful herdr agent-list
// read. Written ONLY on success: its age is the staleness signal.
func (s *Store) RecordOrgArchivePollSuccess(ctx context.Context, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO org_archive_poll (id, last_success_at) VALUES (1, ?)
ON CONFLICT(id) DO UPDATE SET last_success_at = excluded.last_success_at`,
		at.UTC().Format(time.RFC3339Nano))
	return err
}

// OrgArchivePollLastSuccess returns the last successful poll time. ok=false
// means no successful poll has ever been recorded — callers must treat that
// as unknown, never as fresh.
func (s *Store) OrgArchivePollLastSuccess(ctx context.Context) (time.Time, bool, error) {
	var stamp string
	err := s.db.QueryRowContext(ctx, `SELECT last_success_at FROM org_archive_poll WHERE id = 1`).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(stamp))
	if err != nil {
		return time.Time{}, false, nil
	}
	return parsed, true, nil
}
