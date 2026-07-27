package db

import (
	"context"
	"fmt"
)

const (
	SQLiteAutoVacuumNone        = 0
	SQLiteAutoVacuumFull        = 1
	SQLiteAutoVacuumIncremental = 2
)

// SQLiteAutoVacuumMode reports the main database's current auto-vacuum mode.
// It is safe on a read-only Store, which lets diagnostics inspect legacy homes
// without migrating or otherwise mutating them.
func (s *Store) SQLiteAutoVacuumMode(ctx context.Context) (int, error) {
	var mode int
	if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return 0, fmt.Errorf("read sqlite auto-vacuum mode: %w", err)
	}
	return mode, nil
}

// IncrementalVacuum asks SQLite to reclaim at most pages freelist pages. It is
// a no-op for databases that are not already in INCREMENTAL auto-vacuum mode.
// The page count is validated before formatting because SQLite PRAGMA
// arguments do not accept bound parameters.
func (s *Store) IncrementalVacuum(ctx context.Context, pages int) error {
	if pages <= 0 {
		return fmt.Errorf("incremental vacuum pages must be positive")
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, pages)); err != nil {
		return fmt.Errorf("incremental sqlite vacuum: %w", err)
	}
	return nil
}
