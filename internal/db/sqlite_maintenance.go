package db

import (
	"context"
	"database/sql/driver"
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

// WALCheckpoint is the result row of PRAGMA wal_checkpoint.
type WALCheckpoint struct {
	// Busy reports that a reader pinned to an older snapshot, a concurrent
	// writer or another checkpoint kept SQLite from completing and truncating
	// the log. It is a normal outcome, not an error: retry later.
	Busy bool
	// LogFrames is the WAL frame count SQLite reports for this checkpoint, or
	// -1 when the database is not in WAL mode.
	LogFrames int
	// CheckpointedFrames is the number of frames copied into the database.
	CheckpointedFrames int
}

// CheckpointWALTruncate copies the write-ahead log into the database and
// truncates the WAL file to zero bytes (#2384) WITHOUT WAITING: the busy
// timeout is set to zero for the duration, so a reader pinning an old snapshot
// makes SQLite fall back to a passive checkpoint and report Busy instead of
// holding the store's only connection for the full 15s busy timeout. The
// connection's normal busy timeout is restored afterwards; if that restore
// fails, the connection is discarded so database/sql reopens it from the DSN,
// which reapplies every per-connection pragma.
func (s *Store) CheckpointWALTruncate(ctx context.Context) (WALCheckpoint, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return WALCheckpoint{}, fmt.Errorf("sqlite WAL checkpoint: acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return WALCheckpoint{}, fmt.Errorf("sqlite WAL checkpoint: disable busy wait: %w", err)
	}
	var result WALCheckpoint
	var busy int
	checkpointErr := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &result.LogFrames, &result.CheckpointedFrames)
	result.Busy = busy != 0
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA busy_timeout=%d`, sqliteBusyTimeoutMillis)); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return result, fmt.Errorf("sqlite WAL checkpoint: restore busy timeout (connection discarded): %w", err)
	}
	if checkpointErr != nil {
		return WALCheckpoint{}, fmt.Errorf("sqlite WAL checkpoint: %w", checkpointErr)
	}
	return result, nil
}
