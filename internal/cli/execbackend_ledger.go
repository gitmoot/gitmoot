package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

const e2bAttemptProvider = "e2b"

type ledgeredExecutionBackend struct {
	inner           execbackend.ExecutionBackend
	inventoryReaper execbackend.InventoryReaper
	store           *db.Store
	provider        string
	fencingToken    string
	bootID          string
	stdout          io.Writer
	now             func() time.Time

	mu      sync.Mutex
	attempt map[string]db.ExecBackendAttemptKey
}

func newLedgeredExecutionBackend(store *db.Store, inner execbackend.ExecutionBackend, provider, fencingToken, bootID string, stdout io.Writer) (*ledgeredExecutionBackend, error) {
	if store == nil {
		return nil, errors.New("execution backend attempt ledger store is required")
	}
	if inner == nil {
		return nil, errors.New("execution backend attempt ledger provider is required")
	}
	inventoryReaper, ok := inner.(execbackend.InventoryReaper)
	if !ok {
		return nil, fmt.Errorf("execution backend %q does not expose provider inventory", inner.Name())
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, errors.New("execution backend attempt ledger provider name is required")
	}
	fencingToken = strings.TrimSpace(fencingToken)
	if fencingToken == "" {
		return nil, errors.New("execution backend attempt ledger fencing token is required")
	}
	bootID = strings.TrimSpace(bootID)
	if bootID == "" {
		return nil, errors.New("execution backend attempt ledger boot id is required")
	}
	return &ledgeredExecutionBackend{
		inner:           inner,
		inventoryReaper: inventoryReaper,
		store:           store,
		provider:        provider,
		fencingToken:    fencingToken,
		bootID:          bootID,
		stdout:          stdout,
		now:             time.Now,
		attempt:         make(map[string]db.ExecBackendAttemptKey),
	}, nil
}

func (b *ledgeredExecutionBackend) Name() execbackend.Backend { return b.inner.Name() }

func (b *ledgeredExecutionBackend) Provision(ctx context.Context, scope execbackend.JobScope) (*execbackend.Instance, error) {
	attempt := scope.Attempt
	if attempt == 0 {
		attempt = 1
	}
	key := db.ExecBackendAttemptKey{
		JobID:               strings.TrimSpace(scope.JobID),
		Attempt:             attempt,
		LifecycleGeneration: scope.LifecycleGeneration,
	}
	scope.Attempt = attempt
	scope.DaemonFencingToken = b.fencingToken
	if err := b.store.ReserveExecBackendAttempt(ctx, db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: key,
		Provider:              b.provider,
		DaemonFencingToken:    b.fencingToken,
		BootID:                b.bootID,
		TTLExpiresAt:          b.now().UTC().Add(scope.TTL),
		CostReservedUSD:       0,
	}); err != nil {
		return nil, fmt.Errorf("reserve execution backend attempt: %w", err)
	}
	changed, err := b.store.MarkExecBackendAttemptProvisioning(ctx, key)
	if err != nil || !changed {
		return nil, errors.Join(errors.New("mark execution backend attempt provisioning"), err)
	}

	instance, err := b.inner.Provision(ctx, scope)
	if err != nil {
		// A transport failure is not proof that the provider did not allocate.
		// Keep the metadata-keyed row recoverable until a complete inventory can
		// either attach its sandbox ID or mark it orphaned.
		return instance, err
	}
	if instance == nil || strings.TrimSpace(instance.ID) == "" {
		return nil, errors.New("execution backend provision returned an empty instance")
	}
	b.mu.Lock()
	b.attempt[instance.ID] = key
	b.mu.Unlock()
	changed, err = b.store.MarkExecBackendAttemptRunning(context.WithoutCancel(ctx), key, instance.ID)
	if err != nil || !changed {
		return instance, errors.Join(errors.New("mark execution backend attempt running"), err)
	}
	return instance, nil
}

func (b *ledgeredExecutionBackend) Attach(ctx context.Context, id string) (*execbackend.Instance, error) {
	return b.inner.Attach(ctx, id)
}

func (b *ledgeredExecutionBackend) SyncIn(ctx context.Context, instance *execbackend.Instance, materials execbackend.Materials) error {
	return b.inner.SyncIn(ctx, instance, materials)
}

func (b *ledgeredExecutionBackend) Exec(ctx context.Context, instance *execbackend.Instance, command execbackend.Command) (execbackend.Stream, error) {
	return b.inner.Exec(ctx, instance, command)
}

func (b *ledgeredExecutionBackend) Collect(ctx context.Context, instance *execbackend.Instance) (execbackend.ChangeSet, error) {
	if key, ok := b.keyFor(instance); ok {
		changed, err := b.store.MarkExecBackendAttemptCollecting(context.WithoutCancel(ctx), key)
		if err != nil || !changed {
			return execbackend.ChangeSet{}, errors.Join(errors.New("mark execution backend attempt collecting"), err)
		}
	}
	return b.inner.Collect(ctx, instance)
}

func (b *ledgeredExecutionBackend) Cancel(ctx context.Context, instance *execbackend.Instance) error {
	return b.teardown(ctx, instance, b.inner.Cancel)
}

func (b *ledgeredExecutionBackend) Destroy(ctx context.Context, instance *execbackend.Instance) error {
	return b.teardown(ctx, instance, b.inner.Destroy)
}

func (b *ledgeredExecutionBackend) teardown(ctx context.Context, instance *execbackend.Instance, destroy func(context.Context, *execbackend.Instance) error) error {
	key, tracked := b.keyFor(instance)
	var ledgerErrs []error
	if tracked {
		if _, err := b.store.MarkExecBackendAttemptCollecting(context.WithoutCancel(ctx), key); err != nil {
			ledgerErrs = append(ledgerErrs, fmt.Errorf("mark execution backend attempt collecting for teardown: %w", err))
		}
		changed, err := b.store.MarkExecBackendAttemptDestroying(context.WithoutCancel(ctx), key)
		if err != nil {
			ledgerErrs = append(ledgerErrs, fmt.Errorf("mark execution backend attempt destroying: %w", err))
		} else if !changed {
			ledgerErrs = append(ledgerErrs, errors.New("mark execution backend attempt destroying changed no row"))
		}
	}

	providerErr := destroy(ctx, instance)
	if providerErr == nil && tracked {
		changed, err := b.store.MarkExecBackendAttemptDestroyed(context.WithoutCancel(ctx), key, 0)
		if err != nil {
			ledgerErrs = append(ledgerErrs, fmt.Errorf("mark execution backend attempt destroyed: %w", err))
		} else if !changed {
			ledgerErrs = append(ledgerErrs, errors.New("mark execution backend attempt destroyed changed no row"))
		} else {
			b.mu.Lock()
			delete(b.attempt, instance.ID)
			b.mu.Unlock()
		}
	}
	return errors.Join(append([]error{providerErr}, ledgerErrs...)...)
}

func (b *ledgeredExecutionBackend) Reap(ctx context.Context) ([]string, error) {
	report, err := b.ReapInventory(ctx)
	return report.Destroyed, err
}

func (b *ledgeredExecutionBackend) ReapInventory(ctx context.Context) (execbackend.ReapReport, error) {
	report, reapErr := b.inventoryReaper.ReapInventory(ctx)
	// An inconclusive List is not evidence that every active ledger row is
	// orphaned. Preserve all rows and retry recovery when inventory is observed.
	if !report.InventoryObserved {
		if reapErr == nil {
			reapErr = errors.New("execution backend provider inventory was not observed")
		}
		return report, reapErr
	}
	reconcileErr := b.reconcileInventory(context.WithoutCancel(ctx), report)
	return report, errors.Join(reapErr, reconcileErr)
}

func (b *ledgeredExecutionBackend) reconcileInventory(ctx context.Context, report execbackend.ReapReport) error {
	attempts, err := b.store.ListRecoverableExecBackendAttempts(ctx, b.provider)
	if err != nil {
		return fmt.Errorf("list execution backend attempts for recovery: %w", err)
	}
	byKey := make(map[db.ExecBackendAttemptKey]*db.ExecBackendAttempt, len(attempts))
	bySandbox := make(map[string]*db.ExecBackendAttempt, len(attempts))
	for i := range attempts {
		attempt := &attempts[i]
		byKey[attempt.ExecBackendAttemptKey] = attempt
		if attempt.SandboxID != nil {
			bySandbox[*attempt.SandboxID] = attempt
		}
	}

	observed := make(map[string]struct{}, len(report.Inventory))
	matched := make(map[db.ExecBackendAttemptKey]struct{}, len(report.Inventory))
	var reconcileErrs []error
	for _, instance := range report.Inventory {
		id := strings.TrimSpace(instance.ID)
		if id == "" {
			continue
		}
		observed[id] = struct{}{}
		if attempt := bySandbox[id]; attempt != nil {
			matched[attempt.ExecBackendAttemptKey] = struct{}{}
			continue
		}
		key := db.ExecBackendAttemptKey{JobID: instance.JobID, Attempt: instance.Attempt, LifecycleGeneration: instance.LifecycleGeneration}
		attempt := byKey[key]
		if attempt != nil && attempt.SandboxID == nil && attempt.DaemonFencingToken == instance.DaemonFencingToken && attempt.BootID == instance.BootID {
			if attempt.State == db.ExecBackendAttemptStateReserved {
				if changed, markErr := b.store.MarkExecBackendAttemptProvisioning(ctx, key); markErr != nil || !changed {
					reconcileErrs = append(reconcileErrs, errors.Join(fmt.Errorf("recover execution backend attempt %+v to provisioning", key), markErr))
					continue
				}
			}
			if changed, markErr := b.store.MarkExecBackendAttemptRunning(ctx, key, id); markErr != nil || !changed {
				reconcileErrs = append(reconcileErrs, errors.Join(fmt.Errorf("recover execution backend attempt %+v sandbox %q", key, id), markErr))
				continue
			}
			attempt.SandboxID = &id
			attempt.State = db.ExecBackendAttemptStateRunning
			bySandbox[id] = attempt
			matched[key] = struct{}{}
			continue
		}
		writeLine(b.stdout, "execution backend recovery: provider sandbox %s has no matching ledger row", id)
	}

	destroyed := make(map[string]struct{}, len(report.Destroyed))
	for _, id := range report.Destroyed {
		destroyed[strings.TrimSpace(id)] = struct{}{}
	}
	for i := range attempts {
		attempt := &attempts[i]
		key := attempt.ExecBackendAttemptKey
		if attempt.SandboxID == nil {
			if _, ok := matched[key]; ok {
				continue
			}
			writeLine(b.stdout, "execution backend recovery: ledger attempt %s/%d/%d was not observed in provider inventory", key.JobID, key.LifecycleGeneration, key.Attempt)
			if report.InventoryComplete {
				if changed, markErr := b.store.MarkExecBackendAttemptOrphaned(ctx, key); markErr != nil || !changed {
					reconcileErrs = append(reconcileErrs, errors.Join(fmt.Errorf("mark execution backend attempt %+v orphaned", key), markErr))
				}
			}
			continue
		}
		id := *attempt.SandboxID
		if _, ok := destroyed[id]; ok {
			if changed, markErr := b.store.MarkExecBackendAttemptOrphaned(ctx, key); markErr != nil || !changed {
				reconcileErrs = append(reconcileErrs, errors.Join(fmt.Errorf("mark reaped execution backend attempt %+v orphaned", key), markErr))
			}
			continue
		}
		if _, ok := observed[id]; !ok {
			writeLine(b.stdout, "execution backend recovery: ledger sandbox %s was not observed in provider inventory", id)
			if report.InventoryComplete {
				if changed, markErr := b.store.MarkExecBackendAttemptOrphaned(ctx, key); markErr != nil || !changed {
					reconcileErrs = append(reconcileErrs, errors.Join(fmt.Errorf("mark execution backend attempt %+v orphaned", key), markErr))
				}
			}
		}
	}
	return errors.Join(reconcileErrs...)
}

func (b *ledgeredExecutionBackend) keyFor(instance *execbackend.Instance) (db.ExecBackendAttemptKey, bool) {
	if instance == nil {
		return db.ExecBackendAttemptKey{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key, ok := b.attempt[instance.ID]
	return key, ok
}
