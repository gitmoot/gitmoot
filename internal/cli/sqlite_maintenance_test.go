package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestSQLiteIncrementalVacuumRunsAtSlowCadence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	state := &sqliteMaintenanceState{}
	calls := 0
	vacuum := func(_ context.Context, pages int) error {
		calls++
		if pages != sqliteIncrementalVacuumPages {
			t.Fatalf("pages = %d, want %d", pages, sqliteIncrementalVacuumPages)
		}
		return nil
	}

	if err := runSQLiteIncrementalVacuumAtCadence(ctx, now, state, vacuum); err != nil {
		t.Fatalf("first pass returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("first-pass calls = %d, want 1", calls)
	}
	if err := runSQLiteIncrementalVacuumAtCadence(ctx, now.Add(time.Second), state, vacuum); err != nil {
		t.Fatalf("worker-tick-adjacent pass returned error: %v", err)
	}
	if err := runSQLiteIncrementalVacuumAtCadence(ctx, now.Add(sqliteIncrementalVacuumInterval-time.Nanosecond), state, vacuum); err != nil {
		t.Fatalf("pre-interval pass returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls within slow interval = %d, want 1", calls)
	}
	if err := runSQLiteIncrementalVacuumAtCadence(ctx, now.Add(sqliteIncrementalVacuumInterval), state, vacuum); err != nil {
		t.Fatalf("due pass returned error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls after interval = %d, want 2", calls)
	}
}

func TestSQLiteIncrementalVacuumFailureDoesNotAdvanceCadence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	state := &sqliteMaintenanceState{}
	calls := 0
	vacuum := func(context.Context, int) error {
		calls++
		if calls == 1 {
			return errors.New("database busy")
		}
		return nil
	}

	if err := runSQLiteIncrementalVacuumAtCadence(ctx, now, state, vacuum); err == nil {
		t.Fatal("first pass returned nil, want injected error")
	}
	if !state.incrementalVacuumAt.IsZero() {
		t.Fatalf("failed pass advanced timestamp to %s", state.incrementalVacuumAt)
	}
	if err := runSQLiteIncrementalVacuumAtCadence(ctx, now, state, vacuum); err != nil {
		t.Fatalf("immediate retry returned error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls after immediate retry = %d, want 2", calls)
	}
}

// #2384: a completed TRUNCATE checkpoint waits the slow interval, a Busy one
// (reader pinned an old snapshot) retries after a minute, and an error leaves
// the schedule untouched so the next supervisor sweep retries at once.
func TestSQLiteWALCheckpointCadenceSeparatesBusyFromCompletedAndFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	state := &sqliteMaintenanceState{}
	var outcomes []func() (db.WALCheckpoint, error)
	calls := 0
	checkpoint := func(context.Context) (db.WALCheckpoint, error) {
		next := outcomes[calls]
		calls++
		return next()
	}
	busy := func() (db.WALCheckpoint, error) { return db.WALCheckpoint{Busy: true}, nil }
	done := func() (db.WALCheckpoint, error) { return db.WALCheckpoint{}, nil }
	fail := func() (db.WALCheckpoint, error) { return db.WALCheckpoint{}, errors.New("disk I/O error") }
	outcomes = []func() (db.WALCheckpoint, error){busy, done, fail, done}

	step := func(at time.Time, wantCalls int, wantErr bool) {
		t.Helper()
		err := runSQLiteWALCheckpointAtCadence(ctx, at, state, checkpoint)
		if (err != nil) != wantErr {
			t.Fatalf("at %s: err = %v, wantErr %v", at.Sub(now), err, wantErr)
		}
		if calls != wantCalls {
			t.Fatalf("at %s: calls = %d, want %d", at.Sub(now), calls, wantCalls)
		}
	}
	step(now, 1, false)                                                   // first pass: Busy
	step(now.Add(sqliteWALCheckpointBusyRetry-time.Nanosecond), 1, false) // still waiting
	step(now.Add(sqliteWALCheckpointBusyRetry), 2, false)                 // retried: completes
	completedAt := now.Add(sqliteWALCheckpointBusyRetry)
	step(completedAt.Add(sqliteWALCheckpointInterval-time.Nanosecond), 2, false)
	step(completedAt.Add(sqliteWALCheckpointInterval), 3, true)  // due: fails
	step(completedAt.Add(sqliteWALCheckpointInterval), 4, false) // immediate retry after an error
}
