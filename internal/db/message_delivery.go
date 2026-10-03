package db

import "context"

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
