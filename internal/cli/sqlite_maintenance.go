package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

const (
	sqliteIncrementalVacuumInterval = 15 * time.Minute
	sqliteIncrementalVacuumPages    = 256
	// A completed TRUNCATE checkpoint is repeated at the vacuum cadence; one
	// that a pinned reader kept from completing is retried sooner, because it
	// never blocks and costs one non-waiting PRAGMA.
	sqliteWALCheckpointInterval  = 15 * time.Minute
	sqliteWALCheckpointBusyRetry = time.Minute
)

type sqliteMaintenanceState struct {
	incrementalVacuumAt time.Time
	// walCheckpointDue is when the next TRUNCATE checkpoint may run; zero
	// means now.
	walCheckpointDue time.Time
}

type sqliteWALCheckpointFunc func(context.Context) (db.WALCheckpoint, error)

// runSQLiteWALCheckpointOnce truncates the write-ahead log at the supervisor
// cadence (#2384), so a burst of writes cannot leave a WAL file at its peak
// size on disk. It never waits on a reader: a Busy result is retried after
// sqliteWALCheckpointBusyRetry, and an error leaves the due time unchanged so
// the next supervisor sweep retries, like the incremental vacuum.
func runSQLiteWALCheckpointOnce(ctx context.Context, store *db.Store, now time.Time, state *sqliteMaintenanceState) error {
	if store == nil {
		return fmt.Errorf("sqlite WAL checkpoint requires a store")
	}
	return runSQLiteWALCheckpointAtCadence(ctx, now, state, store.CheckpointWALTruncate)
}

func runSQLiteWALCheckpointAtCadence(ctx context.Context, now time.Time, state *sqliteMaintenanceState, checkpoint sqliteWALCheckpointFunc) error {
	if state == nil {
		return fmt.Errorf("sqlite WAL checkpoint requires maintenance state")
	}
	if checkpoint == nil {
		return fmt.Errorf("sqlite WAL checkpoint requires an executor")
	}
	if !state.walCheckpointDue.IsZero() && now.Before(state.walCheckpointDue) {
		return nil
	}
	result, err := checkpoint(ctx)
	if err != nil {
		return err
	}
	if result.Busy {
		state.walCheckpointDue = now.Add(sqliteWALCheckpointBusyRetry)
		return nil
	}
	state.walCheckpointDue = now.Add(sqliteWALCheckpointInterval)
	return nil
}

type sqliteIncrementalVacuumFunc func(context.Context, int) error

// runSQLiteIncrementalVacuumOnce throttles the bounded database reclaim pass at
// the outer supervisor cadence. It is deliberately not part of the ~1-second
// worker tick. A failed pass does not advance the timestamp, so a transient
// SQLite error can retry on the next supervisor sweep.
func runSQLiteIncrementalVacuumOnce(ctx context.Context, store *db.Store, now time.Time, state *sqliteMaintenanceState) error {
	if store == nil {
		return fmt.Errorf("incremental sqlite vacuum requires a store")
	}
	return runSQLiteIncrementalVacuumAtCadence(ctx, now, state, store.IncrementalVacuum)
}

func runSQLiteIncrementalVacuumAtCadence(ctx context.Context, now time.Time, state *sqliteMaintenanceState, vacuum sqliteIncrementalVacuumFunc) error {
	if state == nil {
		return fmt.Errorf("incremental sqlite vacuum requires maintenance state")
	}
	if vacuum == nil {
		return fmt.Errorf("incremental sqlite vacuum requires an executor")
	}
	if !state.incrementalVacuumAt.IsZero() && now.Sub(state.incrementalVacuumAt) < sqliteIncrementalVacuumInterval {
		return nil
	}
	if err := vacuum(ctx, sqliteIncrementalVacuumPages); err != nil {
		return err
	}
	state.incrementalVacuumAt = now
	return nil
}
