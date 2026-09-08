package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// holdWriteLock takes this store's write lock from a SECOND connection and holds
// it for `hold`, then releases. It returns a wait function.
//
// It PROVES THE LOCK IS HELD before returning rather than assuming a BEGIN
// IMMEDIATE won it: an instrument that silently failed to take the lock would
// make every test below pass for the wrong reason (#1911's own lesson, and the
// fleet rule about asserting your instrument's shape before comparing).
func holdWriteLock(t *testing.T, path string, hold time.Duration) func() {
	t.Helper()
	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	tx, err := holder.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO org_role_missed_wakes(role, consecutive, updated_at) VALUES ('lock-holder', 1, '')`,
	); err != nil {
		t.Fatalf("holder write: %v", err)
	}

	// Positive control: a competing write with a deadline far below busy_timeout
	// MUST fail right now. If it succeeds the lock is not held and no result
	// from this harness means anything.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer probeCancel()
	if _, err := holder.ExecContext(probeCtx, `SELECT 1`); err == nil {
		// A read is allowed in WAL; only prove the WRITE path is blocked.
		_ = err
	}
	probe, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer probe.Close()
	if _, err := probe.ExecContext(context.Background(),
		`INSERT INTO org_role_missed_wakes(role, consecutive, updated_at) VALUES ('probe', 1, '')`,
	); err == nil {
		t.Fatal("instrument failure: the write lock is NOT held, so nothing measured here is about contention")
	}

	done := make(chan struct{})
	go func() {
		time.Sleep(hold)
		_ = tx.Commit()
		_ = holder.Close()
		close(done)
	}()
	return func() { <-done }
}

// TestBookkeepingCounterWriteOutlastsAShortCallerDeadline is #1911 mechanism (a).
//
// Three call sites bounded these SQLite writes at eventRuleProbeTimeout (5s), a
// constant documented for herdr SUBPROCESS PROBES, while this store's driver
// waits sqliteBusyTimeoutMillis (15s) for the write lock. Under contention
// longer than the caller's deadline the write was abandoned and the missed-wake
// counter silently kept a stale value - which is what a coordinator reads to
// decide whether a seat is alive.
//
// The budget belongs AT THE WRITE, not at each caller: #1836 fixed one caller
// and its neighbours in the same file were never fixed.
func TestBookkeepingCounterWriteOutlastsAShortCallerDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Both durations are DERIVED FROM THE BUDGET, never literals: a literal
	// margin does not scale when someone widens the busy timeout (#2017).
	hold := DurableWriteBudget / 18
	callerBudget := hold / 5
	wait := holdWriteLock(t, path, hold)
	defer wait()

	ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()
	if err := store.IncrementRoleMissedWake(ctx, "worker", time.Now().UTC()); err != nil {
		t.Fatalf("IncrementRoleMissedWake under %s contention with a %s caller deadline: %v",
			hold, callerBudget, err)
	}

	// Destination evidence, not a nil error: the row must actually be there.
	missed, err := store.ListRoleMissedWakes(context.Background())
	if err != nil {
		t.Fatalf("ListRoleMissedWakes: %v", err)
	}
	var found bool
	for _, row := range missed {
		if row.Role == "worker" && row.Consecutive == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("counter rows = %+v, want worker at 1", missed)
	}
}

// TestBookkeepingCounterResetOutlastsAShortCallerDeadline covers the reset
// neighbour, which is a SEPARATE call site (event_rule_sink.go:362) and would
// stay broken if only the increment were given a floor.
func TestBookkeepingCounterResetOutlastsAShortCallerDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.IncrementRoleMissedWake(context.Background(), "worker", time.Now().UTC()); err != nil {
		t.Fatalf("seed IncrementRoleMissedWake: %v", err)
	}

	hold := DurableWriteBudget / 18
	callerBudget := hold / 5
	wait := holdWriteLock(t, path, hold)
	defer wait()

	ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()
	if err := store.ResetRoleMissedWake(ctx, "worker"); err != nil {
		t.Fatalf("ResetRoleMissedWake under %s contention with a %s caller deadline: %v",
			hold, callerBudget, err)
	}
	missed, err := store.ListRoleMissedWakes(context.Background())
	if err != nil {
		t.Fatalf("ListRoleMissedWakes: %v", err)
	}
	for _, row := range missed {
		if row.Role == "worker" {
			t.Fatalf("worker counter survived the reset: %+v", missed)
		}
	}
}

// TestDurableWriteBudgetCoversEveryLockAcquisition is the invariant that would
// have caught my own #1978/#1982 regression.
//
// TestDurableWriteBudgetExceedsBusyTimeout pins the two CONSTANTS' order. It
// does not pin that the budget covers the NUMBER OF TIMES the write actually
// takes the lock, and #1978 made the delivered finish issue two independent
// statements under a budget derived for one, so a legitimately contended write
// could need 30s against an 18s ceiling.
func TestDurableWriteBudgetCoversEveryLockAcquisition(t *testing.T) {
	busy := time.Duration(sqliteBusyTimeoutMillis) * time.Millisecond
	for acquisitions := 1; acquisitions <= 4; acquisitions++ {
		got := DurableWriteBudgetFor(acquisitions)
		floor := time.Duration(acquisitions) * busy
		if got <= floor {
			t.Fatalf("DurableWriteBudgetFor(%d) = %s, want more than %d full busy waits (%s)",
				acquisitions, got, acquisitions, floor)
		}
	}
	if DurableWriteBudgetFor(1) != DurableWriteBudget {
		t.Fatalf("DurableWriteBudgetFor(1) = %s, want the single-acquisition budget %s",
			DurableWriteBudgetFor(1), DurableWriteBudget)
	}
}

// TestDeliveredFinishNeverLeavesCollapsedRowsSupersededAlone is #1911 mechanism
// (b), and it is a defect in my own #1978.
//
// The delivered path supersedes the collapsed siblings and then delivers the
// survivor in TWO separate statements. If the survivor's write does not land,
// the siblings stay `superseded` with no delivered row carrying their
// obligation - the exact outcome #1978 promised could not happen ("no wake is
// silently dropped"). Two statements are also two lock acquisitions, which is
// how the same bug produces mechanism (b)'s timeout.
func TestDeliveredFinishNeverLeavesCollapsedRowsSupersededAlone(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	var ids []int64
	for index, state := range []string{"pending", "attempted", "attempted"} {
		res, err := store.db.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, created_at, updated_at)
VALUES ('workflow_note', ?, 'worker', 'reply:worker', ?, 1, ?, ?)`,
			fmt.Sprint(index+1), state,
			now.Format(BlockedEpisodeTimeLayout), now.Format(BlockedEpisodeTimeLayout))
		if err != nil {
			t.Fatalf("seed row %d: %v", index, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("seed id %d: %v", index, err)
		}
		ids = append(ids, id)
	}

	// The survivor (ids[0]) is `pending`, so the second statement cannot update
	// it and the whole finish must fail.
	if err := store.FinishWakeOutbox(ctx, ids, WakeOutboxStateDelivered, "", now); err == nil {
		t.Fatal("FinishWakeOutbox succeeded with a survivor that was not attempted")
	}

	rows, err := store.ListWakeOutbox(ctx, WakeOutboxStateSuperseded)
	if err != nil {
		t.Fatalf("ListWakeOutbox: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("collapsed rows were superseded by a finish that failed: %+v", rows)
	}
}
