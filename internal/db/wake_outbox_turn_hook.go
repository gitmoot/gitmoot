package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// WakeOutboxTurnHookReceiptPrefix marks a wake row delivered by a runtime turn
// hook (#2302). The full receipt is turn-hook:<runtime>:<event>; it is stored in
// last_error, the detail column every delivered row already carries.
const WakeOutboxTurnHookReceiptPrefix = "turn-hook:"

// HasPendingInboxWakeOutbox reports whether any role has a pending inbox
// notification, including one re-pended after a deferred attempt. Turn hooks run on every tool call, so this one indexed read lets
// them skip resolving the acting role when there is nothing to deliver.
func (s *Store) HasPendingInboxWakeOutbox(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM wake_outbox
JOIN messages inbox ON inbox.source_kind = wake_outbox.source_kind
	AND inbox.source_id = wake_outbox.source_id AND inbox.recipient = wake_outbox.target_role
WHERE wake_outbox.state = ?)`, WakeOutboxStatePending).Scan(&exists)
	return exists, err
}

// ClaimWakeOutboxForTurnHook moves the given rows from pending to delivered for
// one role and returns exactly the ids this call transitioned, ascending.
//
// It is the turn hook's mark-before-emit guard. The UPDATE takes the write lock
// first (#2028) and its state/role predicate is the whole dedup: a row the
// daemon claimed, another hook claimed, or that belongs to another role is
// simply not returned, so the caller prints only what it alone delivered. A row
// is never returned to pending here; an uncertain outcome is not replayed.
func (s *Store) ClaimWakeOutboxForTurnHook(ctx context.Context, role string, ids []int64, receipt string, at time.Time) ([]int64, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	receipt = strings.TrimSpace(receipt)
	if role == "" {
		return nil, errors.New("turn hook claim requires a role")
	}
	if !strings.HasPrefix(receipt, WakeOutboxTurnHookReceiptPrefix) {
		return nil, fmt.Errorf("turn hook receipt %q must start with %q", receipt, WakeOutboxTurnHookReceiptPrefix)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+5)
	args = append(args, receipt, stamp, stamp, WakeOutboxStatePending, role)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `
UPDATE wake_outbox
SET state = 'delivered', last_error = ?, finished_at = ?, updated_at = ?
WHERE state = ? AND target_role = ? AND id IN (`+placeholders+`)
RETURNING id`, args...)
	if err != nil {
		return nil, err
	}
	var claimed []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		claimed = append(claimed, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	sort.Slice(claimed, func(i, j int) bool { return claimed[i] < claimed[j] })
	return claimed, nil
}
