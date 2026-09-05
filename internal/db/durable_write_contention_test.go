package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// #1836's MECHANISM, pinned behaviourally at the boundary that owns it.
//
// WHAT THE DEADLINE ACTUALLY CONTROLS, and it is not what the name suggests: a
// caller's context does NOT shorten the wait for a contended write. The driver
// ignores cancellation while backing off, so the call returns only when the lock
// frees. What the deadline decides is whether a write that ALREADY OBTAINED THE
// LOCK is accepted or discarded for being late.
//
// That is exactly the production symptom this issue records: the wake was
// delivered, the lock was eventually obtained, and the success record was thrown
// away because the caller had budgeted 5s against a store that waits 15s. The
// row stayed `attempted`, the age-out sweep relabelled it `delivery_unknown`
// with policy=expire_without_retry, and a delivered wake became an unknown that
// is never retried.
//
// Both arms below hold the write lock for the SAME duration from a second
// connection. Only the caller's budget differs, so the budget is the only thing
// the assertions can be about.
func TestShortCallerBudgetDiscardsAWriteItAlreadyWon(t *testing.T) {
	const hold = 8 * time.Second

	run := func(t *testing.T, budget time.Duration) (time.Duration, error) {
		t.Helper()
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "contended.db")
		store, err := openRealTestStore(t, path)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer store.Close()
		if _, err := store.db.ExecContext(ctx,
			`CREATE TABLE probe(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO probe(id, v) VALUES (1, 'a')`); err != nil {
			t.Fatalf("seed: %v", err)
		}

		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("second connection: %v", err)
		}
		defer raw.Close()
		blocker, err := raw.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin blocking transaction: %v", err)
		}
		if _, err := blocker.ExecContext(ctx, `UPDATE probe SET v='b' WHERE id=1`); err != nil {
			t.Fatalf("take the write lock: %v", err)
		}
		go func() {
			time.Sleep(hold)
			_ = blocker.Rollback()
		}()

		writeCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		start := time.Now()
		_, execErr := store.db.ExecContext(writeCtx, `UPDATE probe SET v='c' WHERE id=1`)
		return time.Since(start), execErr
	}

	// THE DEFECT: a 5s budget, the value #1836 found on the wake-outbox terminal
	// write. The write waits out the whole hold and is then thrown away.
	t.Run("short budget discards the write", func(t *testing.T) {
		waited, err := run(t, 5*time.Second)
		if err == nil {
			t.Fatalf("a 5s budget accepted a write that waited %s; if the driver now honours cancellation early this test's premise has changed and #1836's mechanism needs re-reading", waited)
		}
		if waited < hold {
			t.Fatalf("the write returned after %s, before the %s hold elapsed; then the deadline shortened the WAIT and this test is not about what it claims", waited, hold)
		}
	})

	// THE FIX: the store's own derived budget accepts the same write after the
	// same wait. Same contention, same lock, only the budget differs.
	t.Run("derived budget accepts the write", func(t *testing.T) {
		waited, err := run(t, DurableWriteBudget)
		if err != nil {
			t.Fatalf("DurableWriteBudget (%s) discarded a write that obtained the lock after %s: %v", DurableWriteBudget, waited, err)
		}
		if waited < hold {
			t.Fatalf("the write returned after %s, before the %s hold elapsed; the arms are not comparable", waited, hold)
		}
	})
}
