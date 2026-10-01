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

// fakeE2BControlPlane serves the two control-plane routes reconciliation uses:
// the account-wide List and the per-sandbox DELETE.
type fakeE2BControlPlane struct {
	t          *testing.T
	listStatus int
	listed     []string
	deleteResp func(sandboxID string) (int, string)

	mu      sync.Mutex
	deletes []string
}

func (f *fakeE2BControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
		if f.listStatus != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.listStatus)
			_, _ = w.Write([]byte(`{"code":503,"message":"unavailable"}`))
			return
		}
		items := make([]string, 0, len(f.listed))
		for _, id := range f.listed {
			items = append(items, fmt.Sprintf(`{"templateID":"template-a","sandboxID":%q,"clientID":"client","startedAt":"2026-10-01T05:00:00Z","cpuCount":2,"memoryMB":512,"diskSizeMB":1024,"endAt":"2026-10-01T06:00:00Z","state":"running","envdVersion":"1.0"}`, id))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
		id := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
		f.mu.Lock()
		f.deletes = append(f.deletes, id)
		f.mu.Unlock()
		status, body := f.deleteResp(id)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	default:
		f.t.Errorf("unexpected E2B request: %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	}
}

func (f *fakeE2BControlPlane) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func absent404(id string) (int, string) {
	return http.StatusNotFound, fmt.Sprintf(`{"code":404,"message":"Sandbox \"%s\" doesn't exist or you don't have access to it"}`, id)
}

// TestReconcileSettlesDestroyingAttemptWhoseSandboxIsGone is #2282. A teardown
// whose DELETE came back as an inconclusive 404 left the attempt `destroying`,
// a billing state, so it held a cost/concurrency cap slot forever while every
// reconcile logged that the sandbox was not in provider inventory.
//
// Each case drives the daemon's periodic reconcile against a destroying row
// (generation 2) plus a live re-provision of the SAME job (generation 3, running,
// its sandbox absent from E2B's non-exhaustive list), with the concurrency cap
// filled by those two rows. Settling must free exactly the destroying row's slot,
// and only on provider-confirmed absence.
func TestReconcileSettlesDestroyingAttemptWhoseSandboxIsGone(t *testing.T) {
	const (
		jobID          = "local-review-review-router-18da4e7e7043ba72-1"
		staleSandbox   = "in9do8wv4ygc0hruoosht"
		reprovisionSbx = "sandbox-reprovision-live"
	)
	type e2bCase struct {
		listStatus int
		listed     []string
		deleteResp func(string) (int, string)
	}
	tests := []struct {
		name string
		// Exactly one of e2b/fake is set.
		e2b         *e2bCase
		fake        *execbackend.ReapReport
		wantSettled bool
		wantDeletes []string
	}{
		{
			name:        "E2B confirms the exact sandbox does not exist",
			e2b:         &e2bCase{listStatus: http.StatusOK, deleteResp: absent404},
			wantSettled: true,
			wantDeletes: []string{staleSandbox},
		},
		{
			name:        "E2B deletes the sandbox on the retry",
			e2b:         &e2bCase{listStatus: http.StatusOK, deleteResp: func(string) (int, string) { return http.StatusNoContent, "" }},
			wantSettled: true,
			wantDeletes: []string{staleSandbox},
		},
		{
			name: "routing 404 is not absence",
			e2b: &e2bCase{listStatus: http.StatusOK, deleteResp: func(string) (int, string) {
				return http.StatusNotFound, `{"code":404,"message":"validation error: no matching operation was found"}`
			}},
			wantDeletes: []string{staleSandbox},
		},
		{
			name:        "404 naming a different sandbox is not absence of this one",
			e2b:         &e2bCase{listStatus: http.StatusOK, deleteResp: func(string) (int, string) { return absent404("some-other-sandbox") }},
			wantDeletes: []string{staleSandbox},
		},
		{
			name: "410 is not absence even with the absence message",
			e2b: &e2bCase{listStatus: http.StatusOK, deleteResp: func(id string) (int, string) {
				_, body := absent404(id)
				return http.StatusGone, body
			}},
			wantDeletes: []string{staleSandbox},
		},
		{
			name: "failed inventory is not absence",
			e2b:  &e2bCase{listStatus: http.StatusServiceUnavailable, deleteResp: absent404},
		},
		{
			name: "sandbox still listed is not absent",
			e2b:  &e2bCase{listStatus: http.StatusOK, listed: []string{staleSandbox}, deleteResp: absent404},
		},
		{
			name: "complete inventory without the sandbox",
			fake: &execbackend.ReapReport{InventoryObserved: true, InventoryComplete: true, Inventory: []execbackend.ProviderInstance{{
				ID: reprovisionSbx, JobID: jobID, Attempt: 1, LifecycleGeneration: 3,
				DaemonFencingToken: "fence-current", BootID: "boot-current",
			}}},
			wantSettled: true,
		},
		{
			name: "partial inventory without the sandbox and no provider confirmation",
			fake: &execbackend.ReapReport{InventoryObserved: true, Inventory: []execbackend.ProviderInstance{{
				ID: reprovisionSbx, JobID: jobID, Attempt: 1, LifecycleGeneration: 3,
				DaemonFencingToken: "fence-current", BootID: "boot-current",
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openExecBackendLedgerTestStore(t)
			stale := db.ExecBackendAttemptKey{JobID: jobID, Attempt: 1, LifecycleGeneration: 2}
			live := db.ExecBackendAttemptKey{JobID: jobID, Attempt: 1, LifecycleGeneration: 3}
			seedExecBackendAttemptAt(t, store, stale, staleSandbox)
			for _, mark := range []func(context.Context, db.ExecBackendAttemptKey) (bool, error){
				store.MarkExecBackendAttemptCollecting, store.MarkExecBackendAttemptDestroying,
			} {
				if changed, err := mark(ctx, stale); err != nil || !changed {
					t.Fatalf("walk stale attempt to destroying: changed=%v err=%v", changed, err)
				}
			}
			seedExecBackendAttemptAt(t, store, live, reprovisionSbx)

			capPolicy := db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 100, MaxConcurrent: 2, PerAttemptUSD: 1}
			next := db.ExecBackendAttemptReservation{
				ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: "job-queued-remote-review", Attempt: 1, LifecycleGeneration: 1},
				Provider:              e2bAttemptProvider, DaemonFencingToken: "fence-current", BootID: "boot-current",
				TTLExpiresAt: time.Now().Add(time.Hour), CostReservedUSD: 1,
			}
			if err := store.ReserveExecBackendAttempt(ctx, next, capPolicy); !db.IsExecBackendCapRefusal(err) {
				t.Fatalf("reservation before reconcile = %v, want a cap refusal (the destroying row must be holding a slot)", err)
			}

			var inner execbackend.ExecutionBackend
			var provider *fakeE2BControlPlane
			if test.e2b != nil {
				provider = &fakeE2BControlPlane{t: t, listStatus: test.e2b.listStatus, listed: test.e2b.listed, deleteResp: test.e2b.deleteResp}
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
				inner = remoteBackend
			} else {
				inner = &ledgerTestBackend{report: *test.fake}
			}
			var output bytes.Buffer
			ledgered, err := newLedgeredExecutionBackend(store, inner, e2bAttemptProvider, "fence-current", "boot-current", &output, capPolicy)
			if err != nil {
				t.Fatal(err)
			}
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

			reconcileErr := reconcileExecBackendInventory(ctx, worker, io.Discard, time.Now())

			if provider != nil {
				if got := provider.deleted(); fmt.Sprint(got) != fmt.Sprint(test.wantDeletes) {
					t.Fatalf("E2B DELETEs = %v, want %v (a live re-provision's sandbox must never be deleted)", got, test.wantDeletes)
				}
			}
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
					t.Fatalf("stale attempt = %q, want destroying: absence was not confirmed", staleState)
				}
				if len(confirmed) != 0 {
					t.Fatalf("unconfirmed absence recorded %v", confirmed)
				}
				if !db.IsExecBackendCapRefusal(reserveErr) {
					t.Fatalf("reservation after inconclusive reconcile = %v, want the slot still held", reserveErr)
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

func seedExecBackendAttemptAt(t *testing.T, store *db.Store, key db.ExecBackendAttemptKey, sandboxID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.ReserveExecBackendAttempt(ctx, db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: key, Provider: e2bAttemptProvider, DaemonFencingToken: "fence-current", BootID: "boot-current",
		TTLExpiresAt: time.Now().Add(time.Hour), CostReservedUSD: 1,
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
