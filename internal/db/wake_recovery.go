package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (s *Store) GetWakeOutbox(ctx context.Context, id int64) (WakeOutboxEntry, error) {
	var row WakeOutboxEntry
	err := s.db.QueryRowContext(ctx, `SELECT id, source_kind, source_id, target_role, coalesce_key, state,
 attempt_count, last_error, created_at, COALESCE(attempted_at, ''), COALESCE(finished_at, ''), updated_at
 FROM wake_outbox WHERE id = ?`, id).Scan(&row.ID, &row.SourceKind, &row.SourceID, &row.TargetRole,
		&row.CoalesceKey, &row.State, &row.AttemptCount, &row.LastError, &row.CreatedAt, &row.AttemptedAt, &row.FinishedAt, &row.UpdatedAt)
	return row, err
}

// WakeProvenUnsent recognizes only recorded pre-write refusals. Legacy stalls,
// unknown delivery and prose containing an error name are not retry evidence.
func WakeProvenUnsent(row WakeOutboxEntry) bool {
	if row.State != WakeOutboxStateFailed && row.State != WakeOutboxStateStalled {
		return false
	}
	if row.LastError == "role pane binding unresolved" || row.LastError == "herdr unavailable" {
		return true
	}
	for _, code := range []string{"agent_not_found", "agent_blocked", "agent_input_pending"} {
		if strings.HasPrefix(row.LastError, "agent prompt "+code+":") {
			return true
		}
		if strings.HasPrefix(row.LastError, "agent prompt receipt ") && strings.Contains(row.LastError, " code=\""+code+"\" delivery=\"\"") {
			return true
		}
	}
	return false
}

// RecoverWakeOutbox requires an operator reason and fences the inspected row.
// Retrying unknown delivery is intentionally unsupported, including after a
// crash between transport submission and recording its receipt.
func (s *Store) RecoverWakeOutbox(ctx context.Context, expected WakeOutboxEntry, retry bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("wake recovery requires a reason")
	}
	if retry && !WakeProvenUnsent(expected) {
		return fmt.Errorf("wake %d is not proven unsent; reconcile it instead of retrying", expected.ID)
	}
	if expected.State == WakeOutboxStateAttempted || expected.State == WakeOutboxStateDelivered || expected.State == WakeOutboxStateSuperseded {
		return fmt.Errorf("wake %d cannot be recovered from state %s", expected.ID, expected.State)
	}
	ctx, cancel := s.durableWriteContext(ctx, 2)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state := WakeOutboxStateSuperseded
	kind := "wake_recovery_superseded"
	if retry {
		state = WakeOutboxStatePending
		kind = "wake_recovery_retry"
	}
	now := time.Now().UTC().Format(BlockedEpisodeTimeLayout)
	result, err := tx.ExecContext(ctx, `UPDATE wake_outbox SET state = ?, last_error = ?,
 attempted_at = NULL, finished_at = CASE WHEN ? = 'pending' THEN NULL ELSE ? END, updated_at = ?
 WHERE id = ? AND state = ? AND last_error = ? AND updated_at = ?`,
		state, "operator recovery: "+reason, state, now, now, expected.ID, expected.State, expected.LastError, expected.UpdatedAt)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("wake %d changed since inspection; no recovery applied", expected.ID)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,message,runtime,provider) VALUES (?,?,?,'','')`,
		fmt.Sprintf("wake-outbox:%d", expected.ID), kind,
		fmt.Sprintf("role=%s from=%s prior_error=%q reason=%s", expected.TargetRole, expected.State, expected.LastError, reason))
	if err != nil {
		return err
	}
	return tx.Commit()
}
