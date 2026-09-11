package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

// A provider-CONFIRMED destroy must land in `destroyed`; an INCONCLUSIVE
// observation must stay `orphaned` (#2147).
//
// This runs the production path - backend.ReapInventory - rather than calling
// the reconciler directly, because the defect was in which store transition the
// reconciler chose and a direct call would let the test pick the transition it
// wants to see.
//
// Why the STATE is the right observable for "releases the reservation": the
// budget consequence is keyed on the attempt state, so `destroyed` is
// non-billing and `orphaned` holds its reservation. On current main no cap query
// exists (that arrives with #2129), so asserting the state is asserting exactly
// the property the cap will read - and it is the property the old code got
// backwards.
func TestReapInventoryConfirmedDestroyReleasesAndInconclusiveDoesNot(t *testing.T) {
	for _, test := range []struct {
		name      string
		report    func(sandboxID string) execbackend.ReapReport
		wantState string
	}{
		{
			// Positive evidence: the provider told us this sandbox is gone.
			name: "provider confirmed the destroy",
			report: func(sandboxID string) execbackend.ReapReport {
				return execbackend.ReapReport{
					InventoryObserved: true,
					InventoryComplete: true,
					Destroyed:         []string{sandboxID},
				}
			},
			wantState: db.ExecBackendAttemptStateDestroyed,
		},
		{
			// Absence of evidence: a complete inventory that simply does not list
			// the sandbox. Potentially-live is the safe reading, so this one must
			// NOT be released.
			name: "sandbox absent from a complete inventory",
			report: func(string) execbackend.ReapReport {
				return execbackend.ReapReport{
					InventoryObserved: true,
					InventoryComplete: true,
				}
			},
			wantState: db.ExecBackendAttemptStateOrphaned,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openExecBackendLedgerTestStore(t)
			const sandboxID = "sandbox-confirmed"
			key := seedRunningExecBackendAttempt(t, store, "job-confirmed", sandboxID, "fence-current", "boot-current")
			inner := &ledgerTestBackend{report: test.report(sandboxID)}
			var output bytes.Buffer
			backend := newExecBackendLedgerForTest(t, store, inner, &output, "fence-current", "boot-current")

			if _, err := backend.ReapInventory(context.Background()); err != nil {
				t.Fatalf("ReapInventory returned error: %v", err)
			}
			if got := execBackendAttemptForTest(t, store, key).State; got != test.wantState {
				t.Fatalf("attempt state = %q, want %q", got, test.wantState)
			}
		})
	}
}

// The first write is the only write, so recording the wrong state is permanent.
//
// transitionExecBackendAttemptToTerminal accepts only reserved, provisioning,
// running, collecting and destroying as SOURCE states, so an attempt written as
// `orphaned` can never be corrected to `destroyed` afterwards - not by the next
// reconciliation pass, not by a manual reap. This pins that, because it is the
// reason the fix has to be right on the first write rather than eventually
// consistent, and a future change that made orphaned repairable would make the
// whole urgency of #2147 obsolete and should have to update this test.
func TestOrphanedExecBackendAttemptHasNoRepairPath(t *testing.T) {
	ctx := context.Background()
	store := openExecBackendLedgerTestStore(t)
	key := seedRunningExecBackendAttempt(t, store, "job-orphan", "sandbox-orphan", "fence", "boot")

	if changed, err := store.MarkExecBackendAttemptOrphaned(ctx, key); err != nil || !changed {
		t.Fatalf("mark orphaned: changed=%v err=%v", changed, err)
	}
	changed, err := store.MarkExecBackendAttemptDestroyed(ctx, key, 0)
	if err != nil {
		t.Fatalf("mark destroyed after orphaned returned error: %v", err)
	}
	if changed {
		t.Fatal("an orphaned attempt was transitioned to destroyed; orphaned is documented as terminal-unreachable and #2147's first-write requirement depends on that")
	}
	if got := execBackendAttemptForTest(t, store, key).State; got != db.ExecBackendAttemptStateOrphaned {
		t.Fatalf("attempt state = %q, want orphaned to be unchanged", got)
	}
}
