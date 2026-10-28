package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	remoteexec "github.com/gitmoot/gitmoot/internal/execbackend/remote"
)

// TestReconcileSettlesExpiredRunningAttemptOfAFinishedJob is #2365. A job failed
// without its E2B teardown starting, so its attempt stayed `running`: a billing
// state nothing left, which held one of the cost_max_concurrent slots for 16
// days in production. E2B's list is never complete, so the attempt may settle
// only on E2B's own timeout, and only when its job is final - a running job (or
// an unknown owner) keeps the reservation whatever the clock says.
func TestReconcileSettlesExpiredRunningAttemptOfAFinishedJob(t *testing.T) {
	const (
		jobID   = "local-review-review-router-18ddc960866bb9c4-1"
		sandbox = "izmbjeiv7urkmyfc7vkaw"
	)
	ttlExpiresAt := time.Date(2026, 10, 12, 17, 1, 53, 0, time.UTC)
	tests := []struct {
		name        string
		jobState    string // "" = no job row
		listed      []string
		pastTimeout bool
		wantSettled bool
	}{
		{name: "failed job, unlisted, past E2B's timeout", jobState: "failed", pastTimeout: true, wantSettled: true},
		{name: "cancelled job, unlisted, past E2B's timeout", jobState: "cancelled", pastTimeout: true, wantSettled: true},
		{name: "failed job, unlisted, E2B may not have enforced the timeout yet", jobState: "failed"},
		{name: "failed job, past the timeout, but still listed", jobState: "failed", listed: []string{sandbox}, pastTimeout: true},
		{name: "running job, unlisted, past the timeout", jobState: "running", pastTimeout: true},
		{name: "no owning job row, unlisted, past the timeout", pastTimeout: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openExecBackendLedgerTestStore(t)
			key := db.ExecBackendAttemptKey{JobID: jobID, Attempt: 1, LifecycleGeneration: 0}
			if test.jobState != "" {
				if err := store.CreateJob(ctx, db.Job{ID: jobID, Agent: "reviewer", Type: "review", State: test.jobState, Payload: "{}"}); err != nil {
					t.Fatal(err)
				}
			}
			seedExecBackendAttemptAt(t, store, key, sandbox, ttlExpiresAt)

			capPolicy := db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 100, MaxConcurrent: 1, PerAttemptUSD: 1}
			next := db.ExecBackendAttemptReservation{
				ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: "job-queued-remote-review", Attempt: 1, LifecycleGeneration: 1},
				Provider:              e2bAttemptProvider, DaemonFencingToken: "fence-current", BootID: "boot-current",
				TTLExpiresAt: ttlExpiresAt.Add(48 * time.Hour), CostReservedUSD: 1,
			}
			if err := store.ReserveExecBackendAttempt(ctx, next, capPolicy); !db.IsExecBackendCapRefusal(err) {
				t.Fatalf("reservation before reconcile = %v, want a cap refusal (the running row holds the slot)", err)
			}

			provider := &fakeE2BControlPlane{t: t, listStatus: http.StatusOK, listed: test.listed}
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
			grace := remoteBackend.ProviderTTLGrace()
			var output bytes.Buffer
			ledgered, err := newLedgeredExecutionBackend(store, remoteBackend, e2bAttemptProvider, "fence-current", "boot-current", &output, capPolicy)
			if err != nil {
				t.Fatal(err)
			}
			now := ttlExpiresAt.Add(grace - time.Second)
			if test.pastTimeout {
				now = ttlExpiresAt.Add(grace + time.Second)
			}
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

			if err := reconcileExecBackendInventory(ctx, worker, io.Discard, now); err != nil {
				t.Fatalf("reconcile: %v\noutput:\n%s", err, output.String())
			}
			state := execBackendAttemptForTest(t, store, key).State
			events, err := store.ListJobEvents(ctx, jobID)
			if err != nil {
				t.Fatal(err)
			}
			var settled []db.JobEvent
			for _, event := range events {
				if event.Kind == "execbackend_expired_attempt_settled" {
					settled = append(settled, event)
				}
			}
			reserveErr := store.ReserveExecBackendAttempt(ctx, next, capPolicy)
			if !test.wantSettled {
				if state != db.ExecBackendAttemptStateRunning || len(settled) != 0 {
					t.Fatalf("attempt = %q, events = %+v; want running and no settle", state, settled)
				}
				if !db.IsExecBackendCapRefusal(reserveErr) {
					t.Fatalf("reservation = %v, want the slot still held", reserveErr)
				}
				return
			}
			if state != db.ExecBackendAttemptStateDestroyed {
				t.Fatalf("attempt = %q, want destroyed\noutput:\n%s", state, output.String())
			}
			if len(settled) != 1 || !strings.Contains(settled[0].Message, sandbox) || !strings.Contains(settled[0].Message, test.jobState) {
				t.Fatalf("settle events = %+v, want one naming %s and the job state", settled, sandbox)
			}
			if reserveErr != nil {
				t.Fatalf("reservation after the slot settled = %v, want admitted", reserveErr)
			}
		})
	}
}
