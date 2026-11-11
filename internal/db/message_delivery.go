package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeferMessageNotifications records why no input was attempted. It cannot
// reopen an attempted, uncertain or terminal delivery, or spend retry budget.
func (s *Store) DeferMessageNotifications(ctx context.Context, ids []int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE wake_outbox SET last_error=?,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND state='pending' AND last_error!=?`, reason, id, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SendMessageDeferralNotice tells a message's sender that its wake is still
// undelivered (#2574). It stamps the wake row and inserts the system message
// in ONE transaction, so a notice is sent at most once per row and a failed
// send stays owed for the next drain. The stamp is written first and only
// matches a still-pending, not-yet-noticed row whose inbox message was sent by
// sender; any other row returns false and writes nothing.
func (s *Store) SendMessageDeferralNotice(ctx context.Context, wakeID int64, sender, body string, now time.Time) (bool, error) {
	sender = strings.TrimSpace(sender)
	if wakeID <= 0 || sender == "" || sender == MessageSystemSender || strings.TrimSpace(body) == "" {
		return false, errors.New("message deferral notice needs a wake row, a sender and a body")
	}
	writeCtx, cancel := s.durableWriteContext(ctx, 2)
	defer cancel()
	sent := false
	err := retryNotificationWrite(writeCtx, func() error {
		var err error
		sent, err = s.sendMessageDeferralNoticeOnce(writeCtx, wakeID, sender, body, now)
		return err
	})
	return sent, err
}

func (s *Store) sendMessageDeferralNoticeOnce(ctx context.Context, wakeID int64, sender, body string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE wake_outbox SET deferral_notice_at = ?
WHERE id = ? AND state = 'pending' AND deferral_notice_at = ''
	AND EXISTS (SELECT 1 FROM messages m WHERE m.source_kind = wake_outbox.source_kind
		AND m.source_id = wake_outbox.source_id AND m.recipient = wake_outbox.target_role AND m.sender = ?)`,
		now.UTC().Format(time.RFC3339Nano), wakeID, sender)
	if err != nil {
		return false, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return false, err
	}
	message := Message{Sender: MessageSystemSender, Recipient: sender, Body: body, Kind: "message", SourceKind: WakeOutboxSourceWorkflowNote}
	if _, err := insertWorkflowNoteTx(ctx, tx, WorkflowNote{
		Author:          MessageSystemSender,
		Body:            fmt.Sprintf("[message from=%s to=%s]\n%s", MessageSystemSender, sender, body),
		AddressedTarget: sender, Inbox: &message,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// DeferAttemptedWakeOutbox releases a claimed batch only after an explicit
// runtime rejection. Unknown receipts must never take this path.
func (s *Store) DeferAttemptedWakeOutbox(ctx context.Context, ids []int64, reason string, at time.Time) error {
	if len(ids) == 0 {
		return errors.New("notification deferral requires at least one outbox id")
	}
	writeCtx, cancel := s.durableWriteContext(ctx, 2)
	defer cancel()
	return retryNotificationWrite(writeCtx, func() error {
		return s.deferAttemptedWakeOutboxOnce(writeCtx, ids, reason, at)
	})
}

func (s *Store) deferAttemptedWakeOutboxOnce(writeCtx context.Context, ids []int64, reason string, at time.Time) error {
	tx, err := s.db.BeginTx(writeCtx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query, args, err := wakeOutboxIDUpdateStamps(`
UPDATE wake_outbox
SET state = 'pending', coalesced_into = NULL, last_error = ?, attempted_at = NULL, finished_at = NULL,
	attempt_count = attempt_count - 1, updated_at = ?
WHERE state = 'attempted' AND attempt_count > 0 AND id IN (`, ids, at, 1, strings.TrimSpace(reason))
	if err != nil {
		return err
	}
	if err := execWakeOutboxRowUpdate(writeCtx, tx, query, args, len(ids), "defer"); err != nil {
		return err
	}
	return tx.Commit()
}

// Retry only SQLite BUSY outcomes: the statement/transaction did not commit.
// The caller owns one absolute durability budget for all attempts. No transport
// operation belongs inside this callback, and ambiguous I/O/commit errors are
// deliberately not replayed.
func retryNotificationWrite(ctx context.Context, write func() error) error {
	for attempt := 0; ; {
		err := write()
		if err == nil {
			return nil
		}
		var sqliteErr interface{ Code() int }
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != 5 {
			return err
		}
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), err)
		}
		seqConflictBackoff(attempt)
		if attempt < 6 {
			attempt++
		}
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), err)
		}
	}
}
