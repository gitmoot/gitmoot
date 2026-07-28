package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OrgRoleUnavailable is one explicit provider-side unavailability incident for
// an organization role. Until, EscalatedAt, and UpdatedAt use
// BlockedEpisodeTimeLayout so comparisons are stable fixed-width UTC strings.
type OrgRoleUnavailable struct {
	Role        string `json:"role"`
	Runtime     string `json:"runtime,omitempty"`
	Reason      string `json:"reason"`
	Until       string `json:"until"`
	EscalatedAt string `json:"escalated_at,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}

// UpsertOrgRoleUnavailable opens or refreshes a role incident. A refresh while
// the current incident is still active preserves its one-shot escalation claim
// and never shortens the declared unavailability window. A stale row starts a
// fresh incident and resets the escalation claim.
func (s *Store) UpsertOrgRoleUnavailable(ctx context.Context, role, reason string, until, now time.Time) error {
	return s.upsertOrgRoleUnavailable(ctx, role, "", reason, until, now)
}

// UpsertOrgRoleUnavailableForRuntime opens or refreshes a role incident
// attributed to the runtime that observed the provider wall.
func (s *Store) UpsertOrgRoleUnavailableForRuntime(ctx context.Context, role, runtimeName, reason string, until, now time.Time) error {
	runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
	if runtimeName == "" {
		return errors.New("org role unavailable runtime is required")
	}
	return s.upsertOrgRoleUnavailable(ctx, role, runtimeName, reason, until, now)
}

func (s *Store) upsertOrgRoleUnavailable(ctx context.Context, role, runtimeName, reason string, until, now time.Time) error {
	role = strings.ToLower(strings.TrimSpace(role))
	runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
	reason = strings.ToLower(strings.TrimSpace(reason))
	if role == "" {
		return errors.New("org role is required")
	}
	if reason == "" {
		return errors.New("org role unavailable reason is required")
	}
	if until.IsZero() {
		return errors.New("org role unavailable until is required")
	}
	nowStamp := now.UTC().Format(BlockedEpisodeTimeLayout)
	untilStamp := until.UTC().Format(BlockedEpisodeTimeLayout)
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_role_unavailable(role, runtime, reason, unavailable_until, escalated_at, updated_at)
		VALUES (?, ?, ?, ?, '', ?)
		ON CONFLICT(role) DO UPDATE SET
			runtime = excluded.runtime,
			reason = excluded.reason,
			unavailable_until = CASE
				WHEN org_role_unavailable.unavailable_until > ?
					AND org_role_unavailable.runtime = excluded.runtime
					AND org_role_unavailable.unavailable_until > excluded.unavailable_until
				THEN org_role_unavailable.unavailable_until
				ELSE excluded.unavailable_until
			END,
			escalated_at = CASE
				WHEN org_role_unavailable.unavailable_until > ?
					AND org_role_unavailable.runtime = excluded.runtime
				THEN org_role_unavailable.escalated_at
				ELSE ''
			END,
			updated_at = excluded.updated_at`,
		role, runtimeName, reason, untilStamp, nowStamp, nowStamp, nowStamp)
	return err
}

// MarkOrgRoleUnavailableEscalated atomically claims the incident's one
// escalation attempt. Mark-before-delivery prevents concurrent quota failures
// from producing a wake storm. A missing/already-claimed row returns false.
func (s *Store) MarkOrgRoleUnavailableEscalated(ctx context.Context, role string, at time.Time) (bool, error) {
	return s.markOrgRoleUnavailableEscalated(ctx, role, "", at, false)
}

// MarkOrgRoleUnavailableEscalatedForRuntime claims an escalation only for the
// incident attributed to runtimeName.
func (s *Store) MarkOrgRoleUnavailableEscalatedForRuntime(ctx context.Context, role, runtimeName string, at time.Time) (bool, error) {
	runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
	if runtimeName == "" {
		return false, errors.New("org role unavailable runtime is required")
	}
	return s.markOrgRoleUnavailableEscalated(ctx, role, runtimeName, at, true)
}

func (s *Store) markOrgRoleUnavailableEscalated(ctx context.Context, role, runtimeName string, at time.Time, scoped bool) (bool, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return false, errors.New("org role is required")
	}
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	query := `UPDATE org_role_unavailable
		SET escalated_at = ?, updated_at = ?
		WHERE role = ? AND escalated_at = '' AND unavailable_until > ?`
	args := []any{stamp, stamp, role, stamp}
	if scoped {
		query += ` AND runtime = ?`
		args = append(args, runtimeName)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// GetActiveOrgRoleUnavailable returns the role's active incident. An expired
// row is removed eagerly so dispatch cannot be refused on stale state even if
// the periodic sweep has not run yet.
func (s *Store) GetActiveOrgRoleUnavailable(ctx context.Context, role string, now time.Time) (OrgRoleUnavailable, bool, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return OrgRoleUnavailable{}, false, nil
	}
	var unavailable OrgRoleUnavailable
	err := s.db.QueryRowContext(ctx, `SELECT role, runtime, reason, unavailable_until, escalated_at, updated_at
		FROM org_role_unavailable WHERE role = ?`, role).
		Scan(&unavailable.Role, &unavailable.Runtime, &unavailable.Reason, &unavailable.Until, &unavailable.EscalatedAt, &unavailable.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OrgRoleUnavailable{}, false, nil
	}
	if err != nil {
		return OrgRoleUnavailable{}, false, err
	}
	until, err := time.Parse(BlockedEpisodeTimeLayout, unavailable.Until)
	if err != nil {
		return OrgRoleUnavailable{}, false, fmt.Errorf("parse org role %q unavailable until %q: %w", role, unavailable.Until, err)
	}
	if !now.UTC().Before(until) {
		if clearErr := s.ClearOrgRoleUnavailable(ctx, role); clearErr != nil {
			return OrgRoleUnavailable{}, false, clearErr
		}
		return OrgRoleUnavailable{}, false, nil
	}
	return unavailable, true, nil
}

// ListActiveOrgRolesUnavailable returns active incidents in stable role order.
// It is deliberately read-only because the shared org projection is also used
// by the read-only dashboard store. The periodic sweep and eager single-role
// dispatch read own physical deletion; this query excludes stale rows without
// mutating them.
func (s *Store) ListActiveOrgRolesUnavailable(ctx context.Context, now time.Time) ([]OrgRoleUnavailable, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, runtime, reason, unavailable_until, escalated_at, updated_at
		FROM org_role_unavailable ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []OrgRoleUnavailable{}
	for rows.Next() {
		var unavailable OrgRoleUnavailable
		if err := rows.Scan(&unavailable.Role, &unavailable.Runtime, &unavailable.Reason, &unavailable.Until, &unavailable.EscalatedAt, &unavailable.UpdatedAt); err != nil {
			return nil, err
		}
		until, err := time.Parse(BlockedEpisodeTimeLayout, unavailable.Until)
		if err != nil {
			return nil, fmt.Errorf("parse org role %q unavailable until %q: %w", unavailable.Role, unavailable.Until, err)
		}
		if now.UTC().Before(until) {
			result = append(result, unavailable)
		}
	}
	return result, rows.Err()
}

// ClearExpiredOrgRolesUnavailable closes every incident whose declared reset
// has arrived. It returns the number removed for deterministic sweep tests. A
// malformed boundary aborts the sweep and preserves every row: an indeterminate
// quota state must never be converted into confident availability.
func (s *Store) ClearExpiredOrgRolesUnavailable(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT role, unavailable_until FROM org_role_unavailable ORDER BY role`)
	if err != nil {
		return 0, err
	}
	var expired []string
	for rows.Next() {
		var role, untilStamp string
		if err := rows.Scan(&role, &untilStamp); err != nil {
			rows.Close()
			return 0, err
		}
		until, err := time.Parse(BlockedEpisodeTimeLayout, untilStamp)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("parse org role %q unavailable until %q: %w", role, untilStamp, err)
		}
		if !now.UTC().Before(until) {
			expired = append(expired, role)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var cleared int64
	for _, role := range expired {
		result, err := tx.ExecContext(ctx, `DELETE FROM org_role_unavailable WHERE role = ?`, role)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		cleared += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return cleared, nil
}

// ClearOrgRoleUnavailable closes a role incident after a definitive successful
// job (or an eager expiry read). Deleting a missing row is idempotent.
func (s *Store) ClearOrgRoleUnavailable(ctx context.Context, role string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_role_unavailable WHERE role = ?`,
		strings.ToLower(strings.TrimSpace(role)))
	return err
}

// ClearOrgRoleUnavailableForRuntime closes an incident only when runtimeName is
// the same provider identity that opened it. Unknown legacy attribution is
// deliberately not cleared by a successful job.
func (s *Store) ClearOrgRoleUnavailableForRuntime(ctx context.Context, role, runtimeName string) error {
	role = strings.ToLower(strings.TrimSpace(role))
	runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
	if role == "" {
		return errors.New("org role is required")
	}
	if runtimeName == "" {
		return errors.New("org role unavailable runtime is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_role_unavailable WHERE role = ? AND runtime = ?`,
		role, runtimeName)
	return err
}
