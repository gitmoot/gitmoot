package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openCapTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatalf("openCachedTestStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func capTestReservation(job string, cost float64) ExecBackendAttemptReservation {
	return ExecBackendAttemptReservation{
		ExecBackendAttemptKey: ExecBackendAttemptKey{JobID: job, Attempt: 1, LifecycleGeneration: 0},
		Provider:              "e2b",
		DaemonFencingToken:    "token",
		BootID:                "boot",
		TTLExpiresAt:          time.Now().Add(time.Hour),
		CostReservedUSD:       cost,
	}
}

func capTestPolicy(maxUSD float64, maxConcurrent int) ExecBackendCostCap {
	return ExecBackendCostCap{Configured: true, MaxReservedUSD: maxUSD, MaxConcurrent: maxConcurrent, PerAttemptUSD: 1}
}

// TestExecBackendStatesAreClassified is the corpus guard for this gate. Every
// state constant must be classified as billing or non-billing, so adding a
// state without deciding whether it costs money fails here rather than silently
// leaving that spend uncounted by the cap.
func TestExecBackendStatesAreClassified(t *testing.T) {
	all := []string{
		ExecBackendAttemptStateReserved, ExecBackendAttemptStateProvisioning,
		ExecBackendAttemptStateRunning, ExecBackendAttemptStateCollecting,
		ExecBackendAttemptStateDestroying, ExecBackendAttemptStateDestroyed,
		ExecBackendAttemptStateOrphaned, ExecBackendAttemptStateFailed,
	}
	seen := map[string]int{}
	for _, s := range append(append([]string{}, ExecBackendBillingStates...), ExecBackendNonBillingStates...) {
		seen[s]++
	}
	for _, s := range all {
		if seen[s] != 1 {
			t.Errorf("state %q appears in %d classification lists, want exactly 1", s, seen[s])
		}
	}
	if len(seen) != len(all) {
		t.Errorf("classified %d states, but %d state constants exist", len(seen), len(all))
	}
}

// TestReserveRefusesWithoutConfiguredCap pins no-cap = DENY, which is the
// inverse of every other budget default in this codebase.
func TestReserveRefusesWithoutConfiguredCap(t *testing.T) {
	store := openCapTestStore(t)
	for name, policy := range map[string]ExecBackendCostCap{
		"zero value":        {},
		"configured but $0": {Configured: true, MaxReservedUSD: 0, PerAttemptUSD: 1},
		"no per-attempt":    {Configured: true, MaxReservedUSD: 100, PerAttemptUSD: 0},
	} {
		err := store.ReserveExecBackendAttempt(context.Background(), capTestReservation("job-"+name, 1), policy)
		if err == nil {
			t.Fatalf("%s: reservation admitted, want denial", name)
		}
		if !IsExecBackendCapRefusal(err) {
			t.Fatalf("%s: err = %v, want a cap refusal", name, err)
		}
	}
}

// TestDollarCapRefusesOverBudgetProvision is the acceptance criterion: a
// disposable low cap must refuse the over-budget provision.
func TestDollarCapRefusesOverBudgetProvision(t *testing.T) {
	store := openCapTestStore(t)
	policy := capTestPolicy(10, 0)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := store.ReserveExecBackendAttempt(ctx, capTestReservation(fmt.Sprintf("job-%d", i), 3), policy); err != nil {
			t.Fatalf("reservation %d refused early: %v", i, err)
		}
	}
	err := store.ReserveExecBackendAttempt(ctx, capTestReservation("job-over", 3), policy)
	if err == nil {
		t.Fatal("the 4th $3 reservation was admitted against a $10 cap")
	}
	if !strings.Contains(err.Error(), "$9.0000") || !strings.Contains(err.Error(), "$10.0000") {
		t.Fatalf("refusal does not name its operands: %v", err)
	}
}

// TestOrphanedAttemptsHoldBudget is the load-bearing one. An orphaned sandbox is
// still billing, so it must consume the cap. If this inverts, the meter goes
// blind exactly when spend is unmanaged.
func TestOrphanedAttemptsHoldBudget(t *testing.T) {
	store := openCapTestStore(t)
	ctx := context.Background()
	policy := capTestPolicy(10, 0)
	if err := store.ReserveExecBackendAttempt(ctx, capTestReservation("leaked", 9), policy); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE execbackend_attempts SET state = ? WHERE job_id = ?`,
		ExecBackendAttemptStateOrphaned, "leaked"); err != nil {
		t.Fatalf("orphan: %v", err)
	}
	err := store.ReserveExecBackendAttempt(ctx, capTestReservation("next", 3), policy)
	if err == nil {
		t.Fatal("an orphaned $9 attempt did not hold budget: admitted $3 against a $10 cap")
	}
	if !strings.Contains(err.Error(), "ORPHANED") {
		t.Fatalf("refusal does not surface the orphan that is holding budget: %v", err)
	}

	// A destroyed attempt is confirmed gone and must NOT hold budget, or the cap
	// would never release at all.
	if _, err := store.db.ExecContext(ctx, `UPDATE execbackend_attempts SET state = ? WHERE job_id = ?`,
		ExecBackendAttemptStateDestroyed, "leaked"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := store.ReserveExecBackendAttempt(ctx, capTestReservation("after-destroy", 3), policy); err != nil {
		t.Fatalf("a destroyed attempt still holds budget: %v", err)
	}
}

// TestConcurrencyCapRefusesNPlusOne pins the second clause.
func TestConcurrencyCapRefusesNPlusOne(t *testing.T) {
	store := openCapTestStore(t)
	ctx := context.Background()
	policy := capTestPolicy(1000, 2)
	for i := 0; i < 2; i++ {
		if err := store.ReserveExecBackendAttempt(ctx, capTestReservation(fmt.Sprintf("c-%d", i), 1), policy); err != nil {
			t.Fatalf("reservation %d refused early: %v", i, err)
		}
	}
	err := store.ReserveExecBackendAttempt(ctx, capTestReservation("c-3", 1), policy)
	if err == nil {
		t.Fatal("the 3rd attempt was admitted against a concurrency cap of 2")
	}
	if !strings.Contains(err.Error(), "concurrency cap") {
		t.Fatalf("refusal names the wrong clause: %v", err)
	}
}

// TestUnreadableMeterRefuses pins fail-closed on a meter that cannot be read.
func TestUnreadableMeterRefuses(t *testing.T) {
	store := openCapTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `DROP TABLE execbackend_attempts`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := store.ReserveExecBackendAttempt(ctx, capTestReservation("unreadable", 1), capTestPolicy(10, 0)); err == nil {
		t.Fatal("an unreadable meter admitted a provision")
	}
}

// TestParallelLegsContendOnOneReservation is the race this gate exists for:
// N legs dispatched at once must not all pass the same sum.
func TestParallelLegsContendOnOneReservation(t *testing.T) {
	store := openCapTestStore(t)
	policy := capTestPolicy(10, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := store.ReserveExecBackendAttempt(context.Background(), capTestReservation(fmt.Sprintf("p-%d", i), 3), policy); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	var total float64
	if err := store.db.QueryRow(`SELECT COALESCE(SUM(cost_reserved_usd), 0) FROM execbackend_attempts`).Scan(&total); err != nil {
		t.Fatalf("sum: %v", err)
	}
	if total > 10 {
		t.Fatalf("cap breached under contention: $%.2f reserved against a $10 cap by %d admitted legs", total, admitted)
	}
	if admitted != 3 {
		t.Fatalf("admitted = %d, want exactly 3 ($3 each under a $10 cap)", admitted)
	}
}
