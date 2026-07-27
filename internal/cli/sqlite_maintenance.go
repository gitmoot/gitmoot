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
)

type sqliteMaintenanceState struct {
	incrementalVacuumAt time.Time
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
