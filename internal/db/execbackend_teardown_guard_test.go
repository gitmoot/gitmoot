package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// MarkExecBackendAttemptDestroyed admits ONLY `destroying` as a source state,
// and nothing in the suite defended that until now (#2147).
//
// Why it is load-bearing: `destroyed` is where teardown records a completed
// disposal together with its compute cost. Reaching it from `running` or
// `collecting` would mean claiming a sandbox was torn down through phases it
// never entered, and writing a cost for output that was never collected. The
// narrow guard is what makes the state a statement about teardown rather than
// about any terminal outcome.
//
// This exists because a mutant that WIDENED this guard survived the whole
// suite. It was briefly believed to be killed by
// TestExecBackendLedgerTeardownUpdatesEveryPath/startup_reap, but that test
// failed for an unrelated reason - its fixture pinned a provider-confirmed
// destroy as `orphaned`, which was the #2147 defect - so the kill was a
// coincidence and was retracted. A mutant killed for the wrong reason is a
// false kill, and the correct response is the test that was missing.
//
// A reconciler that needs to record a destroy it did not perform must add its
// own transition rather than widen this one; that is the shape #2147 takes.
func TestMarkExecBackendAttemptDestroyedAdmitsOnlyDestroying(t *testing.T) {
	for _, test := range []struct {
		name string
		// advance drives the attempt to the state under test and reports it.
		advance func(must func(bool, error), store *Store, key ExecBackendAttemptKey) string
		want    bool
	}{
		{
			name: "reserved is refused",
			advance: func(func(bool, error), *Store, ExecBackendAttemptKey) string {
				return ExecBackendAttemptStateReserved
			},
		},
		{
			name: "provisioning is refused",
			advance: func(must func(bool, error), store *Store, key ExecBackendAttemptKey) string {
				must(store.MarkExecBackendAttemptProvisioning(context.Background(), key))
				return ExecBackendAttemptStateProvisioning
			},
		},
		{
			name: "running is refused",
			advance: func(must func(bool, error), store *Store, key ExecBackendAttemptKey) string {
				must(store.MarkExecBackendAttemptProvisioning(context.Background(), key))
				must(store.MarkExecBackendAttemptRunning(context.Background(), key, "sandbox-guard"))
				return ExecBackendAttemptStateRunning
			},
		},
		{
			name: "collecting is refused",
			advance: func(must func(bool, error), store *Store, key ExecBackendAttemptKey) string {
				must(store.MarkExecBackendAttemptProvisioning(context.Background(), key))
				must(store.MarkExecBackendAttemptRunning(context.Background(), key, "sandbox-guard"))
				must(store.MarkExecBackendAttemptCollecting(context.Background(), key))
				return ExecBackendAttemptStateCollecting
			},
		},
		{
			// The one permitted source, so the refusals above are a guard and not
			// a broken transition.
			name: "destroying is accepted",
			advance: func(must func(bool, error), store *Store, key ExecBackendAttemptKey) string {
				must(store.MarkExecBackendAttemptProvisioning(context.Background(), key))
				must(store.MarkExecBackendAttemptRunning(context.Background(), key, "sandbox-guard"))
				must(store.MarkExecBackendAttemptCollecting(context.Background(), key))
				must(store.MarkExecBackendAttemptDestroying(context.Background(), key))
				return ExecBackendAttemptStateDestroying
			},
			want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "guard.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			key := ExecBackendAttemptKey{JobID: "job-guard", Attempt: 1, LifecycleGeneration: 1}
			if err := store.ReserveExecBackendAttempt(ctx, ExecBackendAttemptReservation{
				ExecBackendAttemptKey: key, Provider: "e2b",
				DaemonFencingToken: "fence", BootID: "boot",
				TTLExpiresAt: time.Now().Add(time.Minute),
			}, testExecBackendUncappedPolicy()); err != nil {
				t.Fatal(err)
			}
			must := func(changed bool, err error) {
				t.Helper()
				if err != nil || !changed {
					t.Fatalf("lifecycle transition: changed=%v err=%v", changed, err)
				}
			}
			from := test.advance(must, store, key)

			changed, err := store.MarkExecBackendAttemptDestroyed(ctx, key, 0)
			if err != nil {
				t.Fatalf("MarkExecBackendAttemptDestroyed from %s returned error: %v", from, err)
			}
			if changed != test.want {
				t.Fatalf("MarkExecBackendAttemptDestroyed from %s: changed=%v, want %v; teardown's single-source guard defines destroyed as a teardown outcome, not any terminal one", from, changed, test.want)
			}
			attempt, err := store.GetExecBackendAttempt(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			wantState := from
			if test.want {
				wantState = ExecBackendAttemptStateDestroyed
			}
			if attempt.State != wantState {
				t.Fatalf("state after the attempt = %q, want %q", attempt.State, wantState)
			}
		})
	}
}
