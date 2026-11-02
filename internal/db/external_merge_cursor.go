package db

import (
	"context"
	"database/sql"
	"errors"
)

// ExternalMergeReconcileCursor returns where the next externally-merged
// reconcile window starts for repo (#2493); 0 when none is recorded.
func (s *Store) ExternalMergeReconcileCursor(ctx context.Context, repo string) (int64, error) {
	var offset int64
	err := s.db.QueryRowContext(ctx,
		`SELECT next_offset FROM external_merge_reconcile_cursors WHERE repo_full_name = ?`, repo).Scan(&offset)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return offset, err
}

// SetExternalMergeReconcileCursor records where the next window starts.
func (s *Store) SetExternalMergeReconcileCursor(ctx context.Context, repo string, offset int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO external_merge_reconcile_cursors (repo_full_name, next_offset, updated_at)
VALUES (?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(repo_full_name) DO UPDATE SET next_offset = excluded.next_offset, updated_at = CURRENT_TIMESTAMP`,
		repo, offset)
	return err
}
