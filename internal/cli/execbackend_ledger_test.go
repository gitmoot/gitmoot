package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

type ledgerTestBackend struct {
	provision  func(execbackend.JobScope) (*execbackend.Instance, error)
	report     execbackend.ReapReport
	reapErr    error
	destroyErr error
	cancelErr  error
	destroys   int
	cancels    int
}

func TestProvisionExecutionBackendPreservesInstanceOnProvisionError(t *testing.T) {
	instance := &execbackend.Instance{ID: "sandbox-needs-teardown", JobID: "job-needs-teardown"}
	inner := &ledgerTestBackend{provision: func(execbackend.JobScope) (*execbackend.Instance, error) {
		return instance, errors.New("persist running row")
	}}
	worker := jobWorker{ExecutionBackendFactory: func(execbackend.Backend) (execbackend.ExecutionBackend, error) {
		return inner, nil
	}}
	lifecycle, got, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, "shell", db.Job{ID: instance.JobID}, time.Minute, "/checkout")
	if err == nil || !strings.Contains(err.Error(), "persist running row") {
		t.Fatalf("provision error = %v", err)
	}
	if lifecycle != inner || got != instance {
		t.Fatalf("provision failure returned lifecycle=%T instance=%+v; teardown handle was discarded", lifecycle, got)
	}
}

func (*ledgerTestBackend) Name() execbackend.Backend { return execbackend.Remote }

func (b *ledgerTestBackend) Provision(_ context.Context, scope execbackend.JobScope) (*execbackend.Instance, error) {
	if b.provision != nil {
		return b.provision(scope)
	}
	return &execbackend.Instance{ID: "sandbox-ledger", JobID: scope.JobID, LifecycleGeneration: scope.LifecycleGeneration}, nil
}

func (*ledgerTestBackend) Attach(context.Context, string) (*execbackend.Instance, error) {
	return nil, errors.New("not attached")
}

func (*ledgerTestBackend) SyncIn(context.Context, *execbackend.Instance, execbackend.Materials) error {
	return nil
}

func (*ledgerTestBackend) Exec(context.Context, *execbackend.Instance, execbackend.Command) (execbackend.Stream, error) {
	return nil, errors.New("not executed")
}

func (*ledgerTestBackend) Collect(context.Context, *execbackend.Instance) (execbackend.ChangeSet, error) {
	return execbackend.ChangeSet{}, nil
}

func (b *ledgerTestBackend) Cancel(context.Context, *execbackend.Instance) error {
	b.cancels++
	return b.cancelErr
}

func (b *ledgerTestBackend) Destroy(context.Context, *execbackend.Instance) error {
	b.destroys++
	return b.destroyErr
}

func (b *ledgerTestBackend) Reap(context.Context) ([]string, error) {
	return append([]string(nil), b.report.Destroyed...), b.reapErr
}

func (b *ledgerTestBackend) ReapInventory(context.Context) (execbackend.ReapReport, error) {
	return b.report, b.reapErr
}

func TestExecBackendLedgerReservesBeforeProviderProvision(t *testing.T) {
	store := openExecBackendLedgerTestStore(t)
	key := db.ExecBackendAttemptKey{JobID: "job-order", Attempt: 1, LifecycleGeneration: 4}
	providerObservedReservation := false
	inner := &ledgerTestBackend{}
	inner.provision = func(scope execbackend.JobScope) (*execbackend.Instance, error) {
		attempt, err := store.GetExecBackendAttempt(context.Background(), key)
		if err != nil {
			t.Fatalf("provider entered before reservation: %v", err)
		}
		if attempt.State != db.ExecBackendAttemptStateProvisioning || attempt.SandboxID != nil {
			t.Fatalf("row at provider entry = %+v, want provisioning without sandbox id", attempt)
		}
		if scope.Attempt != 1 || scope.DaemonFencingToken != "fence-order" {
			t.Fatalf("provider scope = %+v", scope)
		}
		providerObservedReservation = true
		return &execbackend.Instance{ID: "sandbox-order", JobID: scope.JobID, LifecycleGeneration: scope.LifecycleGeneration}, nil
	}
	backend := newExecBackendLedgerForTest(t, store, inner, nil, "fence-order", "boot-order")
	backend.now = func() time.Time { return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) }
	instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: key.JobID, LifecycleGeneration: key.LifecycleGeneration, TTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !providerObservedReservation || instance.ID != "sandbox-order" {
		t.Fatalf("providerObservedReservation=%v instance=%+v", providerObservedReservation, instance)
	}
	attempt, err := store.GetExecBackendAttempt(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != db.ExecBackendAttemptStateRunning || attempt.SandboxID == nil || *attempt.SandboxID != instance.ID {
		t.Fatalf("running attempt = %+v", attempt)
	}
	if attempt.TTLExpiresAt != "2026-08-29T12:05:00Z" {
		t.Fatalf("ttl_expires_at = %q", attempt.TTLExpiresAt)
	}

	errorStore := openExecBackendLedgerTestStore(t)
	errorKey := db.ExecBackendAttemptKey{JobID: "job-ambiguous", Attempt: 1, LifecycleGeneration: 5}
	errorInner := &ledgerTestBackend{provision: func(execbackend.JobScope) (*execbackend.Instance, error) {
		attempt, err := errorStore.GetExecBackendAttempt(context.Background(), errorKey)
		if err != nil || attempt.State != db.ExecBackendAttemptStateProvisioning {
			t.Fatalf("ambiguous provider call did not have a provisioning row: attempt=%+v err=%v", attempt, err)
		}
		return nil, errors.New("ambiguous provider response")
	}}
	errorBackend := newExecBackendLedgerForTest(t, errorStore, errorInner, nil, "fence-ambiguous", "boot-ambiguous")
	if _, err := errorBackend.Provision(context.Background(), execbackend.JobScope{JobID: errorKey.JobID, LifecycleGeneration: errorKey.LifecycleGeneration, TTL: time.Minute}); err == nil || !strings.Contains(err.Error(), "ambiguous provider response") {
		t.Fatalf("ambiguous provision error = %v", err)
	}
	if got := execBackendAttemptForTest(t, errorStore, errorKey).State; got != db.ExecBackendAttemptStateProvisioning {
		t.Fatalf("ambiguous provision attempt state = %q, want recoverable provisioning", got)
	}
}

func TestExecBackendLedgerTeardownUpdatesEveryPath(t *testing.T) {
	tests := []struct {
		name        string
		act         func(*ledgeredExecutionBackend, *execbackend.Instance) error
		destroyErr  error
		cancelErr   error
		wantState   string
		wantError   bool
		wantDestroy int
		wantCancel  int
	}{
		{name: "success", act: func(b *ledgeredExecutionBackend, i *execbackend.Instance) error {
			return b.Destroy(context.Background(), i)
		}, wantState: db.ExecBackendAttemptStateDestroyed, wantDestroy: 1},
		{name: "failure", act: func(b *ledgeredExecutionBackend, i *execbackend.Instance) error {
			return b.Destroy(context.Background(), i)
		}, destroyErr: errors.New("delete failed"), wantState: db.ExecBackendAttemptStateDestroying, wantError: true, wantDestroy: 1},
		{name: "cancel", act: func(b *ledgeredExecutionBackend, i *execbackend.Instance) error {
			return b.Cancel(context.Background(), i)
		}, wantState: db.ExecBackendAttemptStateDestroyed, wantCancel: 1},
		{name: "panic", act: func(b *ledgeredExecutionBackend, i *execbackend.Instance) (err error) {
			func() {
				defer func() { _ = recover() }()
				defer func() { err = b.Destroy(context.Background(), i) }()
				panic("ledger panic path")
			}()
			return err
		}, wantState: db.ExecBackendAttemptStateDestroyed, wantDestroy: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openExecBackendLedgerTestStore(t)
			inner := &ledgerTestBackend{destroyErr: test.destroyErr, cancelErr: test.cancelErr}
			backend := newExecBackendLedgerForTest(t, store, inner, nil, "fence-"+test.name, "boot-"+test.name)
			instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "job-" + test.name, LifecycleGeneration: 2, TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			err = test.act(backend, instance)
			if (err != nil) != test.wantError {
				t.Fatalf("teardown error = %v, wantError=%v", err, test.wantError)
			}
			key := db.ExecBackendAttemptKey{JobID: "job-" + test.name, Attempt: 1, LifecycleGeneration: 2}
			if got := execBackendAttemptForTest(t, store, key).State; got != test.wantState {
				t.Fatalf("state = %q, want %q", got, test.wantState)
			}
			if inner.destroys != test.wantDestroy || inner.cancels != test.wantCancel {
				t.Fatalf("destroy calls=%d cancel calls=%d, want %d/%d", inner.destroys, inner.cancels, test.wantDestroy, test.wantCancel)
			}
		})
	}

	t.Run("startup reap", func(t *testing.T) {
		store := openExecBackendLedgerTestStore(t)
		key := seedRunningExecBackendAttempt(t, store, "job-reap", "sandbox-reap", "fence-reap", "boot-reap")
		inner := &ledgerTestBackend{report: execbackend.ReapReport{
			InventoryObserved: true,
			Inventory:         []execbackend.ProviderInstance{{ID: "sandbox-reap", JobID: key.JobID, Attempt: key.Attempt, LifecycleGeneration: key.LifecycleGeneration, DaemonFencingToken: "fence-reap", BootID: "boot-reap"}},
			Destroyed:         []string{"sandbox-reap"},
		}}
		backend := newExecBackendLedgerForTest(t, store, inner, nil, "fence-current", "boot-current")
		if reaped, err := backend.Reap(context.Background()); err != nil {
			t.Fatal(err)
		} else if !reflect.DeepEqual(reaped, []string{"sandbox-reap"}) {
			t.Fatalf("reaped = %v", reaped)
		}
		if got := execBackendAttemptForTest(t, store, key).State; got != db.ExecBackendAttemptStateOrphaned {
			t.Fatalf("startup-reaped state = %q, want orphaned", got)
		}
	})
}

func TestExecBackendLedgerReconcilesBothInventoryDirections(t *testing.T) {
	store := openExecBackendLedgerTestStore(t)
	ledgerOnly := seedRunningExecBackendAttempt(t, store, "job-ledger-only", "sandbox-ledger-only", "fence-ledger", "boot-ledger")
	crashKey := db.ExecBackendAttemptKey{JobID: "job-crash", Attempt: 1, LifecycleGeneration: 8}
	if err := store.ReserveExecBackendAttempt(context.Background(), db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: crashKey, Provider: e2bAttemptProvider, DaemonFencingToken: "fence-crash", BootID: "boot-crash", TTLExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.MarkExecBackendAttemptProvisioning(context.Background(), crashKey); err != nil || !changed {
		t.Fatalf("mark crash row provisioning: changed=%v err=%v", changed, err)
	}
	inner := &ledgerTestBackend{report: execbackend.ReapReport{InventoryObserved: true, Inventory: []execbackend.ProviderInstance{
		{ID: "sandbox-provider-only", JobID: "job-provider-only", Attempt: 1, LifecycleGeneration: 1, DaemonFencingToken: "foreign-fence", BootID: "foreign-boot"},
		{ID: "sandbox-crash", JobID: crashKey.JobID, Attempt: crashKey.Attempt, LifecycleGeneration: crashKey.LifecycleGeneration, DaemonFencingToken: "fence-crash", BootID: "boot-crash"},
	}}}
	var output bytes.Buffer
	backend := newExecBackendLedgerForTest(t, store, inner, &output, "fence-current", "boot-current")
	if _, err := backend.ReapInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "provider sandbox sandbox-provider-only has no matching ledger row") || !strings.Contains(got, "ledger sandbox sandbox-ledger-only was not observed in provider inventory") {
		t.Fatalf("recovery output did not name both mismatch directions:\n%s", got)
	}
	crash := execBackendAttemptForTest(t, store, crashKey)
	if crash.State != db.ExecBackendAttemptStateRunning || crash.SandboxID == nil || *crash.SandboxID != "sandbox-crash" {
		t.Fatalf("crash-window attempt = %+v, want recovered running sandbox", crash)
	}
	if got := execBackendAttemptForTest(t, store, ledgerOnly).State; got != db.ExecBackendAttemptStateRunning {
		t.Fatalf("ledger-only attempt state = %q, want running without authoritative absence", got)
	}
}

func TestExecBackendLedgerRefusesIncompleteInventory(t *testing.T) {
	for _, test := range []struct {
		name    string
		reapErr error
		wantErr string
	}{
		{name: "provider error", reapErr: errors.New("provider list failed"), wantErr: "provider list failed"},
		{name: "silent unavailable report", wantErr: "provider inventory was not observed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openExecBackendLedgerTestStore(t)
			key := seedRunningExecBackendAttempt(t, store, "job-incomplete", "sandbox-incomplete", "fence-incomplete", "boot-incomplete")
			inner := &ledgerTestBackend{reapErr: test.reapErr}
			var output bytes.Buffer
			backend := newExecBackendLedgerForTest(t, store, inner, &output, "fence-current", "boot-current")
			if _, err := backend.ReapInventory(context.Background()); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("incomplete inventory error = %v, want %q", err, test.wantErr)
			}
			if got := execBackendAttemptForTest(t, store, key).State; got != db.ExecBackendAttemptStateRunning {
				t.Fatalf("incomplete inventory changed ledger state to %q, want running", got)
			}
			if output.Len() != 0 {
				t.Fatalf("incomplete inventory produced mismatch conclusions: %q", output.String())
			}
		})
	}
}

func openExecBackendLedgerTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newExecBackendLedgerForTest(t *testing.T, store *db.Store, inner *ledgerTestBackend, output *bytes.Buffer, fencingToken, bootID string) *ledgeredExecutionBackend {
	t.Helper()
	backend, err := newLedgeredExecutionBackend(store, inner, e2bAttemptProvider, fencingToken, bootID, output)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func seedRunningExecBackendAttempt(t *testing.T, store *db.Store, jobID, sandboxID, fencingToken, bootID string) db.ExecBackendAttemptKey {
	t.Helper()
	key := db.ExecBackendAttemptKey{JobID: jobID, Attempt: 1, LifecycleGeneration: 3}
	if err := store.ReserveExecBackendAttempt(context.Background(), db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: key, Provider: e2bAttemptProvider, DaemonFencingToken: fencingToken, BootID: bootID, TTLExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.MarkExecBackendAttemptProvisioning(context.Background(), key); err != nil || !changed {
		t.Fatalf("mark provisioning: changed=%v err=%v", changed, err)
	}
	if changed, err := store.MarkExecBackendAttemptRunning(context.Background(), key, sandboxID); err != nil || !changed {
		t.Fatalf("mark running: changed=%v err=%v", changed, err)
	}
	return key
}

func execBackendAttemptForTest(t *testing.T, store *db.Store, key db.ExecBackendAttemptKey) db.ExecBackendAttempt {
	t.Helper()
	attempt, err := store.GetExecBackendAttempt(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}
