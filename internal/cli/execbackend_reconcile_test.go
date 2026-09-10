package cli

import (
	"testing"
	"time"
)

// TestExecBackendReconcileCadenceAdvancesOnSuccess pins the cadence contract
// #1539 asks for. Before this change reconciliation ran ONCE PER PROCESS at
// backend construction, so there was no cadence to get wrong; the failure this
// guards is a provider outage turning into a per-tick hot loop, or a recovered
// provider serving out a penalty it no longer deserves.
func TestExecBackendReconcileCadenceAdvancesOnSuccess(t *testing.T) {
	cadence := &execBackendReconcileCadence{nextAt: map[string]time.Time{}, streak: map[string]int{}}
	now := time.Date(2026, 9, 10, 5, 0, 0, 0, time.UTC)
	const key = "remote"

	// An unseen backend is due immediately: a freshly started daemon must
	// reconcile on its first tick rather than waiting out a full interval.
	if !cadence.due(key, now) {
		t.Fatal("an unseen backend is not due; a restarted daemon would wait a full interval before reconciling")
	}

	cadence.succeeded(key, now)
	if cadence.due(key, now.Add(execBackendReconcileBaseInterval-time.Second)) {
		t.Fatal("backend is due before the base interval elapsed; the pass would run on consecutive ticks")
	}
	if !cadence.due(key, now.Add(execBackendReconcileBaseInterval)) {
		t.Fatal("backend is not due at the base interval; the cadence would never fire again")
	}

	// A failure must push the next attempt strictly FURTHER OUT than the steady
	// cadence, or a persistently failing provider is polled every tick.
	failedAt := now.Add(execBackendReconcileBaseInterval)
	first := cadence.failed(key, failedAt)
	if first <= execBackendReconcileBaseInterval {
		t.Fatalf("first failure interval %s is not longer than the base %s; a failing provider would be polled at the steady rate", first, execBackendReconcileBaseInterval)
	}
	second := cadence.failed(key, failedAt)
	if second < first {
		t.Fatalf("second failure interval %s is shorter than the first %s; the back-off is not monotonic", second, first)
	}

	// ADVANCE ON SUCCESS: one success clears the whole penalty rather than
	// decaying it, so a provider that recovers returns to the steady cadence.
	cadence.succeeded(key, failedAt)
	if cadence.due(key, failedAt.Add(execBackendReconcileBaseInterval-time.Second)) {
		t.Fatal("due before the base interval after recovery")
	}
	if !cadence.due(key, failedAt.Add(execBackendReconcileBaseInterval)) {
		t.Fatalf("after a success the next attempt is still backed off past the base interval %s; the provider is serving a penalty it no longer deserves", execBackendReconcileBaseInterval)
	}

	// THE STREAK ITSELF MUST BE ZEROED, not decayed, and that is only observable
	// on the NEXT failure: succeeded() sets nextAt unconditionally, so asserting
	// on due() alone cannot tell a reset from a decrement. A mutant replacing the
	// reset with `streak--` survived until this assertion existed.
	afterRecovery := cadence.failed(key, failedAt)
	if afterRecovery != first {
		t.Fatalf("first failure after a success backs off %s, want the fresh-failure interval %s; the success decayed the streak instead of clearing it, so a flapping provider ratchets", afterRecovery, first)
	}
}

// TestExecBackendReconcileCadenceIsPerBackend keeps one failing backend from
// suppressing another's reconciliation - the cadence is keyed, and a shared
// timer would silently couple them.
func TestExecBackendReconcileCadenceIsPerBackend(t *testing.T) {
	cadence := &execBackendReconcileCadence{nextAt: map[string]time.Time{}, streak: map[string]int{}}
	now := time.Date(2026, 9, 10, 5, 0, 0, 0, time.UTC)

	cadence.failed("remote", now)
	if !cadence.due("local", now) {
		t.Fatal("a failure on one backend made another backend not due; the cadence is not keyed per backend")
	}
	if cadence.due("remote", now) {
		t.Fatal("the failing backend is still due immediately; its back-off was not recorded")
	}
}
