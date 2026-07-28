package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	WakeOutboxStatePending   = "pending"
	WakeOutboxStateAttempted = "attempted"
	WakeOutboxStateDelivered = "delivered"
	WakeOutboxStateStalled   = "stalled"
	WakeOutboxStateFailed    = "failed"

	WakeOutboxSourceWorkflowNote  = "workflow_note"
	WakeOutboxSourceChatMessage   = "chat_message"
	WakeOutboxReplyCoalescePrefix = "reply:"
)

// WakeOutboxEntry is one durable delivery item. A pending row is positive,
// queryable evidence that no delivery attempt has happened yet.
type WakeOutboxEntry struct {
	ID           int64  `json:"id"`
	SourceKind   string `json:"source_kind"`
	SourceID     string `json:"source_id"`
	TargetRole   string `json:"target_role"`
	CoalesceKey  string `json:"coalesce_key"`
	State        string `json:"state"`
	AttemptCount int    `json:"attempt_count"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    string `json:"created_at"`
	AttemptedAt  string `json:"attempted_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

func insertWorkflowNoteWakeOutboxTx(ctx context.Context, tx *sql.Tx, noteID int64, targetRole string) error {
	if err := insertWakeOutboxTx(
		ctx, tx, WakeOutboxSourceWorkflowNote, strconv.FormatInt(noteID, 10), targetRole,
	); err != nil {
		return fmt.Errorf("insert workflow note wake outbox: %w", err)
	}
	return nil
}

func insertChatMessageWakeOutboxTx(ctx context.Context, tx *sql.Tx, messageID string, targetRoles []string) error {
	seen := make(map[string]struct{}, len(targetRoles))
	for _, targetRole := range targetRoles {
		role := strings.ToLower(strings.TrimSpace(targetRole))
		if role == "" {
			continue
		}
		if _, exists := seen[role]; exists {
			continue
		}
		seen[role] = struct{}{}
		if err := insertWakeOutboxTx(ctx, tx, WakeOutboxSourceChatMessage, messageID, role); err != nil {
			return fmt.Errorf("insert chat message wake outbox: %w", err)
		}
	}
	return nil
}

func insertWakeOutboxTx(ctx context.Context, tx *sql.Tx, sourceKind, sourceID, targetRole string) error {
	sourceKind = strings.TrimSpace(sourceKind)
	if sourceKind == "" {
		return errors.New("wake outbox source kind is required")
	}
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return errors.New("wake outbox source id is required")
	}
	role := strings.ToLower(strings.TrimSpace(targetRole))
	if role == "" {
		return errors.New("wake outbox target role is required")
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key)
VALUES (?, ?, ?, ?)`,
		sourceKind,
		sourceID,
		role,
		WakeOutboxReplyCoalescePrefix+role)
	if err != nil {
		return err
	}
	return nil
}

// ListWakeOutbox returns durable delivery rows in insertion order. An empty
// state returns every row; otherwise it filters by the exact state enum.
func (s *Store) ListWakeOutbox(ctx context.Context, state string) ([]WakeOutboxEntry, error) {
	state = strings.TrimSpace(state)
	query := `
SELECT id, source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, last_error, created_at, COALESCE(attempted_at, ''),
	COALESCE(finished_at, ''), updated_at
FROM wake_outbox`
	args := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WakeOutboxEntry{}
	for rows.Next() {
		var entry WakeOutboxEntry
		if err := rows.Scan(
			&entry.ID, &entry.SourceKind, &entry.SourceID, &entry.TargetRole,
			&entry.CoalesceKey, &entry.State, &entry.AttemptCount,
			&entry.LastError, &entry.CreatedAt, &entry.AttemptedAt,
			&entry.FinishedAt, &entry.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// ClaimWakeOutbox atomically marks an entire coalesced batch attempted. If any
// member is no longer pending, none are claimed; this is the cross-daemon
// mark-before-emit dedup guard.
func (s *Store) ClaimWakeOutbox(ctx context.Context, ids []int64, at time.Time) (bool, error) {
	if len(ids) == 0 {
		return false, errors.New("wake outbox claim requires at least one id")
	}
	query, args, err := wakeOutboxIDUpdate(`
UPDATE wake_outbox
SET state = 'attempted', attempt_count = attempt_count + 1,
	attempted_at = ?, finished_at = NULL, last_error = '', updated_at = ?
WHERE state = 'pending' AND id IN (`, ids, at)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != int64(len(ids)) {
		return false, tx.Rollback()
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// FinishWakeOutbox records the existing event-rule delivery classification for
// every row in one attempted batch.
func (s *Store) FinishWakeOutbox(ctx context.Context, ids []int64, state, detail string, at time.Time) error {
	switch state {
	case WakeOutboxStateDelivered, WakeOutboxStateStalled, WakeOutboxStateFailed:
	default:
		return fmt.Errorf("invalid terminal wake outbox state %q", state)
	}
	query, args, err := wakeOutboxIDUpdate(`
UPDATE wake_outbox
SET state = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE state = 'attempted' AND id IN (`, ids, at, state, strings.TrimSpace(detail))
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != int64(len(ids)) {
		return fmt.Errorf("finish wake outbox updated %d rows, want %d", affected, len(ids))
	}
	return nil
}

func wakeOutboxIDUpdate(prefix string, ids []int64, at time.Time, leading ...string) (string, []any, error) {
	if len(ids) == 0 {
		return "", nil, errors.New("wake outbox update requires at least one id")
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return "", nil, fmt.Errorf("invalid wake outbox id %d", id)
		}
		if _, exists := seen[id]; exists {
			return "", nil, fmt.Errorf("duplicate wake outbox id %d", id)
		}
		seen[id] = struct{}{}
	}
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	args := make([]any, 0, len(leading)+2+len(ids))
	for _, value := range leading {
		args = append(args, value)
	}
	args = append(args, stamp, stamp)
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	return prefix + strings.Join(placeholders, ",") + `)`, args, nil
}
