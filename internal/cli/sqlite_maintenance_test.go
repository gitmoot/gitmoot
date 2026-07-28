package cli

import (
	"context"
	"errors"
	"testing"
	"time"
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
