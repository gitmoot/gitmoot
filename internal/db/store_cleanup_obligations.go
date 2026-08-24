package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	CleanupObligationPending     = "pending"
	CleanupObligationRetryable   = "retryable"
	CleanupObligationRemoved     = "removed"
	CleanupObligationQuarantined = "quarantined"
)

type CleanupObligation struct {
	ResourceID    string                  `json:"resource_id"`
	ResourceKind  string                  `json:"resource_kind"`
	OwnerJobID    string                  `json:"owner_job_id"`
	ExpectedPath  string                  `json:"expected_path"`
	State         string                  `json:"state"`
	Reason        CleanupObligationReason `json:"reason"`
	AttemptCount  int                     `json:"attempt_count"`
	NextAttemptAt string                  `json:"next_attempt_at"`
	LastError     string                  `json:"last_error"`
	CreatedAt     string                  `json:"created_at"`
	UpdatedAt     string                  `json:"updated_at"`
}

type CleanupObligationReason string

const (
	CleanupReasonPending               CleanupObligationReason = "pending"
	CleanupReasonRemoved               CleanupObligationReason = "removed"
	CleanupReasonOperatorReopened      CleanupObligationReason = "operator_reopened"
	CleanupReasonTerminalDeferred      CleanupObligationReason = "terminal_cleanup_deferred"
	CleanupReasonContextInterrupted    CleanupObligationReason = "context_interrupted"
	CleanupReasonJobLookup             CleanupObligationReason = "job_lookup"
	CleanupReasonRunnerResolution      CleanupObligationReason = "runner_resolution"
	CleanupReasonCheckoutLock          CleanupObligationReason = "checkout_lock"
	CleanupReasonIdentityOrContainment CleanupObligationReason = "identity_or_containment"
	CleanupReasonUnknown               CleanupObligationReason = "unknown"
)

func ClassifyCleanupObligationFailure(phase string, err error) CleanupObligationReason {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CleanupReasonContextInterrupted
	}
	switch strings.TrimSpace(phase) {
	case "get_job":
		return CleanupReasonJobLookup
	case "runner":
		return CleanupReasonRunnerResolution
	case "lock":
		return CleanupReasonCheckoutLock
	case "identity":
		return CleanupReasonIdentityOrContainment
	default:
		return CleanupReasonUnknown
	}
}

func CleanupObligationResourceID(ownerJobID, expectedPath string) string {
	identity := strings.TrimSpace(ownerJobID) + "\x00" + filepath.Clean(strings.TrimSpace(expectedPath))
	sum := sha256.Sum256([]byte(identity))
	return "delegation-worktree:" + hex.EncodeToString(sum[:])
}

func cleanupObligationTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func (s *Store) EnsureCleanupObligation(ctx context.Context, ownerJobID, expectedPath string, now time.Time) (CleanupObligation, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	expectedPath = filepath.Clean(strings.TrimSpace(expectedPath))
	if ownerJobID == "" || expectedPath == "." || expectedPath == "" {
		return CleanupObligation{}, fmt.Errorf("cleanup obligation requires owner job and expected path")
	}
	resourceID := CleanupObligationResourceID(ownerJobID, expectedPath)
	stamp := cleanupObligationTime(now)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cleanup_obligations (
			resource_id, resource_kind, owner_job_id, expected_path, state,
			reason, attempt_count, next_attempt_at, created_at, updated_at
		) VALUES (?, 'delegation_worktree', ?, ?, 'pending', 'pending', 0, ?, ?, ?)
		ON CONFLICT(resource_id) DO UPDATE SET
			owner_job_id=excluded.owner_job_id,
			expected_path=excluded.expected_path,
			updated_at=excluded.updated_at
		WHERE cleanup_obligations.state IN ('pending', 'retryable')`,
		resourceID, ownerJobID, expectedPath, stamp, stamp, stamp)
	if err != nil {
		return CleanupObligation{}, err
	}
	return s.GetCleanupObligation(ctx, resourceID)
}

func (s *Store) RecordCleanupObligationFailure(ctx context.Context, ownerJobID, expectedPath string, reason CleanupObligationReason, cause error, now time.Time, nextAttempt time.Time, budget int) (CleanupObligation, error) {
	if budget <= 0 {
		return CleanupObligation{}, fmt.Errorf("cleanup retry budget must be positive")
	}
	obligation, err := s.EnsureCleanupObligation(ctx, ownerJobID, expectedPath, now)
	if err != nil {
		return CleanupObligation{}, err
	}
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	stamp := cleanupObligationTime(now)
	next := cleanupObligationTime(nextAttempt)
	_, err = s.db.ExecContext(ctx, `
		UPDATE cleanup_obligations
		SET attempt_count=attempt_count+1,
			state=CASE WHEN attempt_count+1 >= ? THEN 'quarantined' ELSE 'retryable' END,
			reason=?, next_attempt_at=?, last_error=?, updated_at=?
		WHERE resource_id=? AND state IN ('pending', 'retryable')`,
		budget, strings.TrimSpace(string(reason)), next, message, stamp, obligation.ResourceID)
	if err != nil {
		return CleanupObligation{}, err
	}
	return s.GetCleanupObligation(ctx, obligation.ResourceID)
}

func (s *Store) DeferCleanupObligation(ctx context.Context, ownerJobID, expectedPath string, reason CleanupObligationReason, now, nextAttempt time.Time) (CleanupObligation, error) {
	obligation, err := s.EnsureCleanupObligation(ctx, ownerJobID, expectedPath, now)
	if err != nil {
		return CleanupObligation{}, err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE cleanup_obligations
		SET state='retryable', reason=?, next_attempt_at=?, updated_at=?
		WHERE resource_id=? AND state IN ('pending', 'retryable')`,
		strings.TrimSpace(string(reason)), cleanupObligationTime(nextAttempt), cleanupObligationTime(now), obligation.ResourceID)
	if err != nil {
		return CleanupObligation{}, err
	}
	return s.GetCleanupObligation(ctx, obligation.ResourceID)
}

func (s *Store) MarkCleanupObligationRemoved(ctx context.Context, ownerJobID, expectedPath string, now time.Time) (CleanupObligation, error) {
	obligation, err := s.EnsureCleanupObligation(ctx, ownerJobID, expectedPath, now)
	if err != nil {
		return CleanupObligation{}, err
	}
	stamp := cleanupObligationTime(now)
	_, err = s.db.ExecContext(ctx, `
		UPDATE cleanup_obligations
		SET state='removed', reason='removed', next_attempt_at=?, last_error='', updated_at=?
		WHERE resource_id=? AND state <> 'quarantined'`, stamp, stamp, obligation.ResourceID)
	if err != nil {
		return CleanupObligation{}, err
	}
	return s.GetCleanupObligation(ctx, obligation.ResourceID)
}

func (s *Store) GetCleanupObligation(ctx context.Context, resourceID string) (CleanupObligation, error) {
	var obligation CleanupObligation
	err := s.db.QueryRowContext(ctx, `
		SELECT resource_id, resource_kind, owner_job_id, expected_path, state,
		       reason, attempt_count, next_attempt_at, last_error, created_at, updated_at
		FROM cleanup_obligations WHERE resource_id=?`, strings.TrimSpace(resourceID)).Scan(
		&obligation.ResourceID, &obligation.ResourceKind, &obligation.OwnerJobID,
		&obligation.ExpectedPath, &obligation.State, &obligation.Reason,
		&obligation.AttemptCount, &obligation.NextAttemptAt, &obligation.LastError,
		&obligation.CreatedAt, &obligation.UpdatedAt)
	return obligation, err
}

func (s *Store) ListCleanupObligations(ctx context.Context, state string) ([]CleanupObligation, error) {
	query := `SELECT resource_id, resource_kind, owner_job_id, expected_path, state,
	                 reason, attempt_count, next_attempt_at, last_error, created_at, updated_at
	          FROM cleanup_obligations`
	args := []any{}
	if state = strings.TrimSpace(state); state != "" {
		query += ` WHERE state=?`
		args = append(args, state)
	}
	query += ` ORDER BY owner_job_id, expected_path`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var obligations []CleanupObligation
	for rows.Next() {
		var obligation CleanupObligation
		if err := rows.Scan(&obligation.ResourceID, &obligation.ResourceKind, &obligation.OwnerJobID,
			&obligation.ExpectedPath, &obligation.State, &obligation.Reason,
			&obligation.AttemptCount, &obligation.NextAttemptAt, &obligation.LastError,
			&obligation.CreatedAt, &obligation.UpdatedAt); err != nil {
			return nil, err
		}
		obligations = append(obligations, obligation)
	}
	return obligations, rows.Err()
}

func (s *Store) ReopenCleanupObligation(ctx context.Context, resourceID string, now time.Time) (CleanupObligation, error) {
	stamp := cleanupObligationTime(now)
	result, err := s.db.ExecContext(ctx, `
		UPDATE cleanup_obligations
		SET state='pending', reason='operator_reopened', attempt_count=0,
			next_attempt_at=?, last_error='', updated_at=?
		WHERE resource_id=? AND state='quarantined'`, stamp, stamp, strings.TrimSpace(resourceID))
	if err != nil {
		return CleanupObligation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return CleanupObligation{}, err
	}
	if changed == 0 {
		return CleanupObligation{}, sql.ErrNoRows
	}
	return s.GetCleanupObligation(ctx, resourceID)
}

func (s *Store) CountQuarantinedCleanupObligations(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cleanup_obligations WHERE state='quarantined'`).Scan(&count)
	return count, err
}
