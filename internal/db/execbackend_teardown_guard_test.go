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

// TestExecBackendLifecycleTransitionsRefuseNonAdjacentSources generalises the
// guard above to EVERY single-source transition in the lifecycle.
//
// The #2129 review found that the gap fixed for MarkExecBackendAttemptDestroyed
// was shared by its four siblings: their production guards are correctly narrow,
// but their tests only assert that a REPEATED call from the already-reached
// target state is rejected. That catches nothing, because a widened guard still
// refuses the target state - it is the source state that stops being checked.
// The reviewer reproduced it on MarkExecBackendAttemptCollecting.
//
// The table asserts both directions per transition: refusal from every
// non-adjacent live source, AND acceptance from the one legal source. The
// acceptance arm is what makes the refusals mean something; without it a
// transition that refused everything would pass identically.
func TestExecBackendLifecycleTransitionsRefuseNonAdjacentSources(t *testing.T) {
	ctx := context.Background()
	// walk advances a fresh row to the requested state through the legal path.
	walk := func(t *testing.T, store *Store, key ExecBackendAttemptKey, to string) {
		t.Helper()
		steps := []struct {
			state string
			call  func() (bool, error)
		}{
			{ExecBackendAttemptStateProvisioning, func() (bool, error) { return store.MarkExecBackendAttemptProvisioning(ctx, key) }},
			{ExecBackendAttemptStateRunning, func() (bool, error) {
				return store.MarkExecBackendAttemptRunning(ctx, key, "sbx-"+key.JobID)
			}},
			{ExecBackendAttemptStateCollecting, func() (bool, error) { return store.MarkExecBackendAttemptCollecting(ctx, key) }},
			{ExecBackendAttemptStateDestroying, func() (bool, error) { return store.MarkExecBackendAttemptDestroying(ctx, key) }},
		}
		for _, step := range steps {
			if to == ExecBackendAttemptStateReserved {
				return
			}
			changed, err := step.call()
			if err != nil || !changed {
				t.Fatalf("walking to %s: step %s changed=%v err=%v", to, step.state, changed, err)
			}
			if step.state == to {
				return
			}
		}
	}

	transitions := []struct {
		name      string
		legalFrom string
		call      func(store *Store, key ExecBackendAttemptKey) (bool, error)
	}{
		{"Provisioning", ExecBackendAttemptStateReserved, func(s *Store, k ExecBackendAttemptKey) (bool, error) {
			return s.MarkExecBackendAttemptProvisioning(ctx, k)
		}},
		{"Running", ExecBackendAttemptStateProvisioning, func(s *Store, k ExecBackendAttemptKey) (bool, error) {
			return s.MarkExecBackendAttemptRunning(ctx, k, "sbx-late")
		}},
		{"Collecting", ExecBackendAttemptStateRunning, func(s *Store, k ExecBackendAttemptKey) (bool, error) {
			return s.MarkExecBackendAttemptCollecting(ctx, k)
		}},
		{"Destroying", ExecBackendAttemptStateCollecting, func(s *Store, k ExecBackendAttemptKey) (bool, error) {
			return s.MarkExecBackendAttemptDestroying(ctx, k)
		}},
	}
	liveStates := []string{
		ExecBackendAttemptStateReserved, ExecBackendAttemptStateProvisioning,
		ExecBackendAttemptStateRunning, ExecBackendAttemptStateCollecting,
		ExecBackendAttemptStateDestroying,
	}

	for _, transition := range transitions {
		for _, from := range liveStates {
			if from == transition.legalFrom {
				continue
			}
			t.Run(transition.name+"_refuses_"+from, func(t *testing.T) {
				store := openCapTestStore(t)
				key := ExecBackendAttemptKey{JobID: "job-" + transition.name + "-" + from, Attempt: 1, LifecycleGeneration: 1}
				if err := store.ReserveExecBackendAttempt(ctx, ExecBackendAttemptReservation{
					ExecBackendAttemptKey: key, Provider: "e2b", DaemonFencingToken: "fence", BootID: "boot",
					TTLExpiresAt: time.Now().Add(time.Minute),
				}, testExecBackendUncappedPolicy()); err != nil {
					t.Fatal(err)
				}
				walk(t, store, key, from)
				changed, err := transition.call(store, key)
				if err != nil {
					t.Fatalf("Mark...%s from %s: err=%v", transition.name, from, err)
				}
				if changed {
					t.Fatalf("Mark...%s from %s: changed=true, want false; this transition admits only %s, and a widened guard would let the lifecycle skip phases",
						transition.name, from, transition.legalFrom)
				}
			})
		}
		t.Run(transition.name+"_accepts_"+transition.legalFrom, func(t *testing.T) {
			store := openCapTestStore(t)
			key := ExecBackendAttemptKey{JobID: "job-ok-" + transition.name, Attempt: 1, LifecycleGeneration: 1}
			if err := store.ReserveExecBackendAttempt(ctx, ExecBackendAttemptReservation{
				ExecBackendAttemptKey: key, Provider: "e2b", DaemonFencingToken: "fence", BootID: "boot",
				TTLExpiresAt: time.Now().Add(time.Minute),
			}, testExecBackendUncappedPolicy()); err != nil {
				t.Fatal(err)
			}
			walk(t, store, key, transition.legalFrom)
			changed, err := transition.call(store, key)
			if err != nil || !changed {
				t.Fatalf("Mark...%s from its legal source %s: changed=%v err=%v; the refusal arms above prove nothing if this cannot pass",
					transition.name, transition.legalFrom, changed, err)
			}
		})
	}
}
