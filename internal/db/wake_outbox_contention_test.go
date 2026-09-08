package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// This file's contention helper was collapsed into durable_write_test.go's
// holdWriteLock when #2023 landed, as its own note said it would be: the
// duplication was deliberate and temporary, and forking a second convention
// permanently was never the plan.

func seedAttemptedWakeRows(t *testing.T, store *Store, count int, attemptedAt time.Time) []int64 {
	t.Helper()
	stamp := attemptedAt.UTC().Format(BlockedEpisodeTimeLayout)
	var ids []int64
	for index := 0; index < count; index++ {
		res, err := store.db.ExecContext(context.Background(), `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, created_at, updated_at, attempted_at)
VALUES ('workflow_note', ?, 'worker', 'reply:worker', 'attempted', 1, ?, ?, ?)`,
			index+1, stamp, stamp, stamp)
		if err != nil {
			t.Fatalf("seed row %d: %v", index, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("seed id %d: %v", index, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestWakeTransactionsWaitForTheWriteLockInsteadOfBeingRefused is #2028.
//
// MEASURED, with another connection holding the write lock and a caller
// deadline far above the store's busy timeout so the caller is never the limit:
//
//	write-first transaction                  waits 14.50s then SQLITE_BUSY
//	read-then-write transaction              refused at 0s
//	first statement UPDATE ... RETURNING     waits 14.22s then SQLITE_BUSY
//
// SQLite will not run the busy handler for a write attempted inside a
// transaction that already holds a read lock, because waiting there could
// deadlock. So a transaction that reads before it writes forfeits the wait
// entirely, and NO context budget can help it - which is why #1911's budget
// floor is correct but inert for these two writers.
//
// Both of these transactions must therefore acquire the write lock with their
// FIRST statement. The test asserts the observable consequence: under a lock
// held for less than the store's budget, the call WAITS and then succeeds,
// rather than failing immediately.
func TestWakeTransactionsWaitForTheWriteLockInsteadOfBeingRefused(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(store *Store, ctx context.Context, ids []int64, at time.Time) error
	}{
		{name: "aged-sweep", call: func(store *Store, ctx context.Context, ids []int64, at time.Time) error {
			_, err := store.ExpireAgedWakeOutbox(ctx, at.Add(time.Hour), at)
			return err
		}},
		{name: "finish-or-retry", call: func(store *Store, ctx context.Context, ids []int64, at time.Time) error {
			_, _, err := store.FinishOrRetryWakeOutbox(ctx, ids, WakeOutboxStateFailed, "transport down", 3, at)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gitmoot.db")
			store, err := openCachedTestStore(t, path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			now := time.Now().UTC()
			ids := seedAttemptedWakeRows(t, store, 2, now)

			// The hold is a fraction of the store's own budget rather than a
			// literal, so it still means "less than the store will wait" if
			// anyone widens the busy timeout.
			hold := DurableWriteBudget / 18
			wait := holdWriteLock(t, path, hold)
			defer wait()

			started := time.Now()
			if err := test.call(store, context.Background(), ids, now); err != nil {
				t.Fatalf("%s under a %s held write lock: %v", test.name, hold, err)
			}
			// A call that returned before the lock was released did not wait for
			// it, so a pass would not mean what the test claims.
			if waited := time.Since(started); waited < hold/2 {
				t.Fatalf("%s returned after %s, less than half the %s hold: it did not wait for the write lock",
					test.name, waited, hold)
			}
		})
	}
}
