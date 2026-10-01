package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	remoteexec "github.com/gitmoot/gitmoot/internal/execbackend/remote"
)

// fakeE2BControlPlane serves E2B's account-wide List. Any other request,
// including a DELETE, is recorded and fails the test: settling must not depend
// on a per-ID response, which E2B also returns for an inaccessible live sandbox.
type fakeE2BControlPlane struct {
	t          *testing.T
	listStatus int
	listed     []string

	mu    sync.Mutex
	other []string
}

func (f *fakeE2BControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/v2/sandboxes" {
		f.mu.Lock()
		f.other = append(f.other, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		f.t.Errorf("unexpected E2B request: %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if f.listStatus != http.StatusOK {
		w.WriteHeader(f.listStatus)
		_, _ = w.Write([]byte(`{"code":503,"message":"unavailable"}`))
		return
	}
	items := make([]string, 0, len(f.listed))
	for _, id := range f.listed {
		items = append(items, fmt.Sprintf(`{"templateID":"template-a","sandboxID":%q,"clientID":"client","startedAt":"2026-10-01T05:00:00Z","cpuCount":2,"memoryMB":512,"diskSizeMB":1024,"endAt":"2026-10-01T06:00:00Z","state":"running","envdVersion":"1.0"}`, id))
	}
	_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
}

// TestReconcileSettlesDestroyingAttemptWhoseSandboxIsGone is #2282. A teardown
// whose DELETE came back as an inconclusive 404 left the attempt `destroying`,
// a billing state, so it held a cost/concurrency cap slot forever while every
// reconcile logged that the sandbox was not in provider inventory.
//
// Each case drives the daemon's periodic reconcile against a destroying row
// (generation 2) plus a live re-provision of the SAME job (generation 3,
// running, same TTL, its sandbox also unlisted), with the concurrency cap filled
// by those two rows. Settling must free exactly the destroying row's slot, and
// only when the sandbox provably cannot exist: absent from a complete inventory,
// or absent from E2B's list after E2B's own timeout has killed it.
func TestReconcileSettlesDestroyingAttemptWhoseSandboxIsGone(t *testing.T) {
	const (
		jobID          = "local-review-review-router-18da4e7e7043ba72-1"
		staleSandbox   = "in9do8wv4ygc0hruoosht"
		reprovisionSbx = "sandbox-reprovision-live"
	)
	ttlExpiresAt := time.Date(2026, 10, 1, 8, 2, 18, 310302746, time.UTC)
	// A complete inventory lists the live re-provision; leaving it out would
	// (correctly, and outside this change) orphan that running row.
	liveListed := []execbackend.ProviderInstance{{
		ID: reprovisionSbx, JobID: jobID, Attempt: 1, LifecycleGeneration: 3,
		DaemonFencingToken: "fence-current", BootID: "boot-current",
	}}
	type e2bCase struct {
		listStatus int
		listed     []string
	}
	tests := []struct {
		name string
		// Exactly one of e2b/fake is set.
		e2b  *e2bCase
		fake *execbackend.ReapReport
		// now returns the reconcile clock from the attempt TTL and the inner
		// backend's provider-enforcement grace (zero when it has none).
		now         func(ttl time.Time, grace time.Duration) time.Time
		wantSettled bool
	}{
		{
			name:        "unlisted after E2B's own timeout has killed it",
			e2b:         &e2bCase{listStatus: http.StatusOK},
			now:         func(ttl time.Time, grace time.Duration) time.Time { return ttl.Add(grace + time.Second) },
			wantSettled: true,
		},
		{
			name: "unlisted but E2B may not have enforced the timeout yet",
			e2b:  &e2bCase{listStatus: http.StatusOK},
			now:  func(ttl time.Time, grace time.Duration) time.Time { return ttl.Add(grace - time.Second) },
		},
		{
			name: "past the timeout but still listed",
			e2b:  &e2bCase{listStatus: http.StatusOK, listed: []string{staleSandbox}},
			now:  func(ttl time.Time, grace time.Duration) time.Time { return ttl.Add(grace + time.Hour) },
		},
		{
			name: "past the timeout but the inventory failed",
			e2b:  &e2bCase{listStatus: http.StatusServiceUnavailable},
			now:  func(ttl time.Time, grace time.Duration) time.Time { return ttl.Add(grace + time.Hour) },
		},
		{
			name: "complete inventory without the sandbox, before its TTL",
			fake: &execbackend.ReapReport{InventoryObserved: true, InventoryComplete: true, Inventory: liveListed},
			now:  func(ttl time.Time, _ time.Duration) time.Time { return ttl.Add(-time.Hour) },
			// A complete inventory is proof on its own; no TTL is needed.
			wantSettled: true,
		},
		{
			name: "partial inventory from a provider that enforces no timeout",
			fake: &execbackend.ReapReport{InventoryObserved: true},
			now:  func(ttl time.Time, _ time.Duration) time.Time { return ttl.Add(24 * time.Hour) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openExecBackendLedgerTestStore(t)
			stale := db.ExecBackendAttemptKey{JobID: jobID, Attempt: 1, LifecycleGeneration: 2}
			live := db.ExecBackendAttemptKey{JobID: jobID, Attempt: 1, LifecycleGeneration: 3}
			seedExecBackendAttemptAt(t, store, stale, staleSandbox, ttlExpiresAt)
			for _, mark := range []func(context.Context, db.ExecBackendAttemptKey) (bool, error){
				store.MarkExecBackendAttemptCollecting, store.MarkExecBackendAttemptDestroying,
			} {
				if changed, err := mark(ctx, stale); err != nil || !changed {
					t.Fatalf("walk stale attempt to destroying: changed=%v err=%v", changed, err)
				}
			}
			seedExecBackendAttemptAt(t, store, live, reprovisionSbx, ttlExpiresAt)

			capPolicy := db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 100, MaxConcurrent: 2, PerAttemptUSD: 1}
			next := db.ExecBackendAttemptReservation{
				ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: "job-queued-remote-review", Attempt: 1, LifecycleGeneration: 1},
				Provider:              e2bAttemptProvider, DaemonFencingToken: "fence-current", BootID: "boot-current",
				TTLExpiresAt: ttlExpiresAt.Add(48 * time.Hour), CostReservedUSD: 1,
			}
			if err := store.ReserveExecBackendAttempt(ctx, next, capPolicy); !db.IsExecBackendCapRefusal(err) {
				t.Fatalf("reservation before reconcile = %v, want a cap refusal (the destroying row must be holding a slot)", err)
			}

			var inner execbackend.ExecutionBackend
			var provider *fakeE2BControlPlane
			var grace time.Duration
			if test.e2b != nil {
				provider = &fakeE2BControlPlane{t: t, listStatus: test.e2b.listStatus, listed: test.e2b.listed}
				server := httptest.NewServer(provider)
				t.Cleanup(server.Close)
				client, err := e2b.NewClient("e2b-test-key", e2b.Options{BaseURL: server.URL, HTTPClient: server.Client()})
				if err != nil {
					t.Fatal(err)
				}
				remoteBackend, err := remoteexec.NewBackend(client, remoteexec.Options{TemplateID: "template-a"})
				if err != nil {
					t.Fatal(err)
				}
				// Asserted through a local interface so this test compiles, and
				// fails on its assertions, against a backend without the bound.
				enforcer, ok := any(remoteBackend).(interface{ ProviderTTLGrace() time.Duration })
				if !ok {
					t.Fatal("the E2B backend declares no provider-enforced timeout, so an unlisted destroying attempt can never settle")
				}
				if grace = enforcer.ProviderTTLGrace(); grace <= 0 {
					t.Fatalf("E2B provider TTL grace = %s, want positive headroom over ttl_expires_at", grace)
				}
				inner = remoteBackend
			} else {
				inner = &ledgerTestBackend{report: *test.fake}
			}
			var output bytes.Buffer
			ledgered, err := newLedgeredExecutionBackend(store, inner, e2bAttemptProvider, "fence-current", "boot-current", &output, capPolicy)
			if err != nil {
				t.Fatal(err)
			}
			now := test.now(ttlExpiresAt, grace)
			ledgered.now = func() time.Time { return now }
			home := t.TempDir()
			if err := config.Initialize(config.PathsForHome(home)); err != nil {
				t.Fatal(err)
			}
			worker := jobWorker{
				Store: store, ConfigHome: home, ConfigHomeExplicit: true,
				ExecutionBackendFactory: func(backend execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
					if backend == execbackend.Remote {
						return ledgered, nil
					}
					return &ledgerTestBackend{}, nil
				},
			}
			prior := execBackendReconcileState
			execBackendReconcileState = &execBackendReconcileCadence{nextAt: map[string]time.Time{}, streak: map[string]int{}}
			t.Cleanup(func() { execBackendReconcileState = prior })

			reconcileErr := reconcileExecBackendInventory(ctx, worker, io.Discard, now)

			if got := execBackendAttemptForTest(t, store, live).State; got != db.ExecBackendAttemptStateRunning {
				t.Fatalf("live re-provision of the same job = %q, want running", got)
			}
			events, err := store.ListJobEvents(ctx, jobID)
			if err != nil {
				t.Fatal(err)
			}
			var confirmed []db.JobEvent
			for _, event := range events {
				if event.Kind == "execbackend_destroy_confirmed" {
					confirmed = append(confirmed, event)
				}
			}
			staleState := execBackendAttemptForTest(t, store, stale).State
			reserveErr := store.ReserveExecBackendAttempt(ctx, next, capPolicy)
			if !test.wantSettled {
				if staleState != db.ExecBackendAttemptStateDestroying {
					t.Fatalf("stale attempt = %q, want destroying: the sandbox may still exist", staleState)
				}
				if len(confirmed) != 0 {
					t.Fatalf("unproven absence recorded %v", confirmed)
				}
				if !db.IsExecBackendCapRefusal(reserveErr) {
					t.Fatalf("reservation after an inconclusive reconcile = %v, want the slot still held", reserveErr)
				}
				return
			}
			if reconcileErr != nil {
				t.Fatalf("reconcile: %v", reconcileErr)
			}
			if staleState != db.ExecBackendAttemptStateDestroyed {
				t.Fatalf("stale attempt = %q, want destroyed\noutput:\n%s", staleState, output.String())
			}
			if len(confirmed) != 1 || !strings.Contains(confirmed[0].Message, staleSandbox) || !strings.Contains(confirmed[0].Message, "generation 2") {
				t.Fatalf("confirmed-teardown events = %+v, want one naming %s generation 2", confirmed, staleSandbox)
			}
			if reserveErr != nil {
				t.Fatalf("reservation after the stale slot settled = %v, want admitted", reserveErr)
			}
		})
	}
}

// racingTeardownBackend runs a reconcile pass inside the provider delete, the
// window in which reconciliation can settle the row teardown is about to mark.
type racingTeardownBackend struct {
	*ledgerTestBackend
	during func()
}

func (b *racingTeardownBackend) Destroy(ctx context.Context, instance *execbackend.Instance) error {
	b.during()
	return b.ledgerTestBackend.Destroy(ctx, instance)
}

// A reconcile pass that settles this exact attempt while teardown's delete is
// in flight must not make the successful teardown report execbackend_destroy_failed.
func TestTeardownRacingReconcileSettleIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	store := openExecBackendLedgerTestStore(t)
	inner := &racingTeardownBackend{ledgerTestBackend: &ledgerTestBackend{
		report: execbackend.ReapReport{InventoryObserved: true, InventoryComplete: true},
	}}
	ledgered, err := newLedgeredExecutionBackend(store, inner, e2bAttemptProvider, "fence-current", "boot-current", io.Discard, testCLIExecBackendUncappedPolicy())
	if err != nil {
		t.Fatal(err)
	}
	key := db.ExecBackendAttemptKey{JobID: "job-teardown-race", Attempt: 1, LifecycleGeneration: 1}
	inner.during = func() {
		if attempt := execBackendAttemptForTest(t, store, key); attempt.State != db.ExecBackendAttemptStateDestroying {
			t.Fatalf("attempt during provider delete = %q, want destroying", attempt.State)
		}
		if _, err := ledgered.ReapInventory(ctx); err != nil {
			t.Fatalf("concurrent reconcile: %v", err)
		}
	}
	instance, err := ledgered.Provision(ctx, execbackend.JobScope{JobID: key.JobID, LifecycleGeneration: key.LifecycleGeneration, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	jobWorker{Store: store, Stdout: io.Discard}.destroyExecutionBackend(key.JobID, ledgered, instance)

	if got := execBackendAttemptForTest(t, store, key).State; got != db.ExecBackendAttemptStateDestroyed {
		t.Fatalf("attempt = %q, want destroyed", got)
	}
	events, err := store.ListJobEvents(ctx, key.JobID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, event := range events {
		kinds[event.Kind]++
		if event.Kind == "execbackend_destroy_failed" {
			t.Fatalf("successful teardown recorded %s: %s", event.Kind, event.Message)
		}
	}
	if inner.destroys != 1 || kinds["execbackend_destroy_confirmed"] != 1 {
		t.Fatalf("destroys=%d events=%v, want one provider delete and one settle event", inner.destroys, kinds)
	}
}

func seedExecBackendAttemptAt(t *testing.T, store *db.Store, key db.ExecBackendAttemptKey, sandboxID string, ttlExpiresAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := store.ReserveExecBackendAttempt(ctx, db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: key, Provider: e2bAttemptProvider, DaemonFencingToken: "fence-current", BootID: "boot-current",
		TTLExpiresAt: ttlExpiresAt, CostReservedUSD: 1,
	}, testCLIExecBackendUncappedPolicy()); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.MarkExecBackendAttemptProvisioning(ctx, key); err != nil || !changed {
		t.Fatalf("mark provisioning: changed=%v err=%v", changed, err)
	}
	if changed, err := store.MarkExecBackendAttemptRunning(ctx, key, sandboxID); err != nil || !changed {
		t.Fatalf("mark running: changed=%v err=%v", changed, err)
	}
}
