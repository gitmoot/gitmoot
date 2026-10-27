package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #2384: the journal size limit is a per-connection pragma, so it must come
// from the DSN and survive database/sql replacing the store's only connection.
func TestWritableConnectionsBoundJournalSizeAcrossReplacement(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	readLimit := func() int64 {
		t.Helper()
		var limit int64
		if err := store.db.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&limit); err != nil {
			t.Fatalf("PRAGMA journal_size_limit returned error: %v", err)
		}
		return limit
	}
	if got := readLimit(); got != SQLiteJournalSizeLimitBytes {
		t.Fatalf("journal_size_limit = %d, want %d", got, SQLiteJournalSizeLimitBytes)
	}

	// Change the live connection, then make database/sql discard it. The
	// replacement must come back with the DSN limit, not the changed value.
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn returned error: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA journal_size_limit=1`); err != nil {
		t.Fatalf("set journal_size_limit returned error: %v", err)
	}
	if err := conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Raw discard returned %v, want driver.ErrBadConn", err)
	}
	_ = conn.Close()
	if got := readLimit(); got != SQLiteJournalSizeLimitBytes {
		t.Fatalf("replacement connection journal_size_limit = %d, want %d", got, SQLiteJournalSizeLimitBytes)
	}
}

// #2384: the TRUNCATE checkpoint empties the WAL file, never waits out the 15s
// busy timeout behind a reader pinning an old snapshot, and leaves the
// connection's normal busy timeout in place afterwards.
func TestCheckpointWALTruncateEmptiesLogAndDoesNotWaitOnAPinnedReader(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if _, err := store.db.ExecContext(ctx, `CREATE TABLE wal_probe (payload BLOB)`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	write := func(rows int) {
		t.Helper()
		for i := 0; i < rows; i++ {
			if _, err := store.db.ExecContext(ctx, `INSERT INTO wal_probe (payload) VALUES (randomblob(4096))`); err != nil {
				t.Fatalf("insert probe row: %v", err)
			}
		}
	}
	walSize := func() int64 {
		t.Helper()
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatalf("stat WAL: %v", err)
		}
		return info.Size()
	}
	write(200)

	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly returned error: %v", err)
	}
	defer reader.Close()
	tx, err := reader.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin reader transaction: %v", err)
	}
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM wal_probe`).Scan(&rows); err != nil {
		t.Fatalf("pin reader snapshot: %v", err)
	}
	write(50) // frames beyond the pinned snapshot

	started := time.Now()
	pinned, err := store.CheckpointWALTruncate(ctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("checkpoint with pinned reader returned error: %v", err)
	}
	if !pinned.Busy {
		t.Fatalf("checkpoint with pinned reader = %+v, want Busy", pinned)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("checkpoint with pinned reader took %s; it must not wait on the busy timeout", elapsed)
	}
	if size := walSize(); size == 0 {
		t.Fatal("WAL was truncated while a reader still pinned an older snapshot")
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("release reader snapshot: %v", err)
	}
	done, err := store.CheckpointWALTruncate(ctx)
	if err != nil {
		t.Fatalf("checkpoint after reader released returned error: %v", err)
	}
	if done.Busy || done.LogFrames < 0 {
		t.Fatalf("checkpoint after reader released = %+v, want a completed WAL checkpoint", done)
	}
	if size := walSize(); size != 0 {
		t.Fatalf("WAL size after TRUNCATE checkpoint = %d, want 0", size)
	}

	var busyTimeout int
	if err := store.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout returned error: %v", err)
	}
	if busyTimeout != sqliteBusyTimeoutMillis {
		t.Fatalf("busy_timeout after checkpoint = %d, want %d", busyTimeout, sqliteBusyTimeoutMillis)
	}
	write(1) // the store keeps working on the truncated log
}
