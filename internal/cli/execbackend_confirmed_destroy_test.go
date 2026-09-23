package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

// A provider-confirmed deletion releases the reservation; an inventory that
// merely omits the sandbox does not.
func TestReapInventoryConfirmedDestroyReleasesAndInconclusiveDoesNot(t *testing.T) {
	for _, test := range []struct {
		name      string
		report    func(sandboxID string) execbackend.ReapReport
		wantState string
	}{
		{
			name: "provider confirmed the destroy",
			report: func(sandboxID string) execbackend.ReapReport {
				return execbackend.ReapReport{
					InventoryObserved: true,
					Inventory: []execbackend.ProviderInstance{{
						ID: sandboxID, JobID: "job-confirmed", Attempt: 1,
						LifecycleGeneration: 3, DaemonFencingToken: "fence-current",
						BootID: "boot-current", Reapable: true,
					}},
				}
			},
			wantState: db.ExecBackendAttemptStateDestroyed,
		},
		{
			name: "sandbox absent from a complete inventory",
			report: func(string) execbackend.ReapReport {
				return execbackend.ReapReport{InventoryObserved: true, InventoryComplete: true}
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
			attempt := execBackendAttemptForTest(t, store, key)
			if attempt.State != test.wantState {
				t.Fatalf("attempt state = %q, want %q", attempt.State, test.wantState)
			}
			if attempt.CostActualUSD != nil {
				t.Fatalf("cost_actual_usd = %v, want NULL because provider reported no dollar cost", *attempt.CostActualUSD)
			}
		})
	}
}
