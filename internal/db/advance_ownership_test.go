package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedAdvanceOwnedJob(t *testing.T, store *Store, jobID string, state string) {
	t.Helper()
	if err := store.CreateJob(context.Background(), Job{ID: jobID, Agent: "impl", Type: "implement", State: state, Payload: `{"repo":"o/r"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
}

func ownAdvanceLease(t *testing.T, store *Store, jobID string, token string, expires time.Time) {
	t.Helper()
	owned, err := store.AcquireResourceLock(context.Background(), ResourceLock{
		ResourceKey: SupersedeAdvanceLockKeyPrefix + jobID,
		OwnerJobID:  jobID,
		OwnerToken:  token,
		ExpiresAt:   expires.Format(time.RFC3339Nano),
	}, time.Now().UTC())
	if err != nil || !owned {
		t.Fatalf("acquire advance lease owned=%v err=%v", owned, err)
	}
}

// TestRenewAdvanceOwnershipLeaseRefusesAnExpiredToken is the resurrection guard at
// the store boundary: once a lease has lapsed, its token is dead permanently. A
// renewal that matched only key+token would revive a pass whose lifecycle has since
// been re-queued by an operator.
func TestRenewAdvanceOwnershipLeaseRefusesAnExpiredToken(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	seedAdvanceOwnedJob(t, store, "j-expired", "failed")
	ownAdvanceLease(t, store, "j-expired", "tok", time.Now().UTC().Add(-time.Minute))

	own := AdvanceOwnership{LockKey: SupersedeAdvanceLockKeyPrefix + "j-expired", OwnerToken: "tok", OwnerJobID: "j-expired"}
	renewed, err := store.RenewAdvanceOwnershipLease(ctx, own, time.Now().UTC().Add(time.Hour), time.Now().UTC())
	if err != nil {
		t.Fatalf("RenewAdvanceOwnershipLease: %v", err)
	}
	if renewed {
		t.Fatal("an expired lease was renewed; a dead pass would resume emitting effects")
	}
	lock, err := store.GetResourceLock(ctx, SupersedeAdvanceLockKeyPrefix+"j-expired")
	if err != nil {
		t.Fatalf("GetResourceLock: %v", err)
	}
	if parsed, perr := time.Parse(time.RFC3339Nano, lock.ExpiresAt); perr != nil {
		t.Fatalf("parse expiry %q: %v", lock.ExpiresAt, perr)
	} else if parsed.After(time.Now().UTC()) {
		t.Fatalf("expiry moved into the future (%s): the refused renewal still wrote", lock.ExpiresAt)
	}
}

// TestRenewAdvanceOwnershipLeaseRenewsALiveLease is the success control: an active
// pass must be able to renew indefinitely, so the short lease never becomes a
// deadline on legitimate work.
func TestRenewAdvanceOwnershipLeaseRenewsALiveLease(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	seedAdvanceOwnedJob(t, store, "j-live", "failed")
	ownAdvanceLease(t, store, "j-live", "tok", time.Now().UTC().Add(time.Minute))

	own := AdvanceOwnership{LockKey: SupersedeAdvanceLockKeyPrefix + "j-live", OwnerToken: "tok", OwnerJobID: "j-live"}
	for round := 0; round < 3; round++ {
		renewed, err := store.RenewAdvanceOwnershipLease(ctx, own, time.Now().UTC().Add(time.Duration(round+2)*time.Minute), time.Now().UTC())
		if err != nil {
			t.Fatalf("round %d RenewAdvanceOwnershipLease: %v", round, err)
		}
		if !renewed {
			t.Fatalf("round %d refused a live lease: an active pass would lose its advance", round)
		}
	}
}

// TestRenewAdvanceOwnershipLeaseRefusesAMovedLifecycle keeps the lease bound to the
// generation it was granted for, so a lease that somehow outlives a re-queue cannot
// authorize effects for the run that replaced it.
func TestRenewAdvanceOwnershipLeaseRefusesAMovedLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	seedAdvanceOwnedJob(t, store, "j-moved", "failed")
	ownAdvanceLease(t, store, "j-moved", "tok", time.Now().UTC().Add(time.Minute))

	own := AdvanceOwnership{LockKey: SupersedeAdvanceLockKeyPrefix + "j-moved", OwnerToken: "tok", OwnerJobID: "j-moved", AtGeneration: 7}
	renewed, err := store.RenewAdvanceOwnershipLease(ctx, own, time.Now().UTC().Add(time.Hour), time.Now().UTC())
	if err != nil {
		t.Fatalf("RenewAdvanceOwnershipLease: %v", err)
	}
	if renewed {
		t.Fatal("a lease was renewed for a generation the job is not on")
	}
}

// TestCreateJobWithEventIfAdvanceOwnedRefusesALostLease proves the insert carries
// the predicate itself. A caller that checked ownership a moment earlier is not
// enough: the check and the irreversible write must share one transaction.
func TestCreateJobWithEventIfAdvanceOwnedRefusesALostLease(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	seedAdvanceOwnedJob(t, store, "j-parent", "failed")
	own := AdvanceOwnership{LockKey: SupersedeAdvanceLockKeyPrefix + "j-parent", OwnerToken: "tok", OwnerJobID: "j-parent"}
	child := Job{ID: "j-parent/child", Agent: "impl", Type: "implement", State: "queued", Payload: `{"repo":"o/r"}`}

	err := store.CreateJobWithEventIfAdvanceOwned(ctx, child, own, time.Now().UTC(), JobEvent{Kind: "queued", Message: "job queued"})
	if !errors.Is(err, ErrAdvanceOwnershipLost) {
		t.Fatalf("insert error = %v, want ErrAdvanceOwnershipLost", err)
	}
	if _, err := store.GetJob(ctx, child.ID); err == nil {
		t.Fatal("the job was inserted without a lease; the effect is irreversible")
	}

	// SUCCESS CONTROL: with the lease held the same insert commits, event included.
	ownAdvanceLease(t, store, "j-parent", "tok", time.Now().UTC().Add(time.Minute))
	if err := store.CreateJobWithEventIfAdvanceOwned(ctx, child, own, time.Now().UTC(), JobEvent{Kind: "queued", Message: "job queued"}); err != nil {
		t.Fatalf("owned insert: %v", err)
	}
	if _, err := store.GetJob(ctx, child.ID); err != nil {
		t.Fatalf("GetJob after an owned insert: %v", err)
	}
	events, err := store.ListJobEvents(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "queued" {
		t.Fatalf("job events = %+v, want exactly the queued event", events)
	}
}

// TestCreateJobWithEventIfAdvanceOwnedRefusesAnExpiredLease separates ABSENT from
// EXPIRED. A lease row that still exists but has lapsed belongs to a pass that is
// gone — a retry may already have re-queued against it — so the insert must refuse
// exactly as it does for a missing row.
//
// MUTATION PROOF: drop `expires_at > ?` from advanceOwnershipLiveSQL and the stale
// insert commits; the absent-lease test alone does not notice.
func TestCreateJobWithEventIfAdvanceOwnedRefusesAnExpiredLease(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	seedAdvanceOwnedJob(t, store, "j-lapsed", "failed")
	ownAdvanceLease(t, store, "j-lapsed", "tok", time.Now().UTC().Add(-time.Minute))
	if _, err := store.GetResourceLock(ctx, SupersedeAdvanceLockKeyPrefix+"j-lapsed"); err != nil {
		t.Fatalf("the lapsed lease row must still exist for this test to differ from the absent case: %v", err)
	}

	own := AdvanceOwnership{LockKey: SupersedeAdvanceLockKeyPrefix + "j-lapsed", OwnerToken: "tok", OwnerJobID: "j-lapsed"}
	child := Job{ID: "j-lapsed/child", Agent: "impl", Type: "implement", State: "queued", Payload: `{"repo":"o/r"}`}
	err := store.CreateJobWithEventIfAdvanceOwned(ctx, child, own, time.Now().UTC(), JobEvent{Kind: "queued", Message: "job queued"})
	if !errors.Is(err, ErrAdvanceOwnershipLost) {
		t.Fatalf("insert error = %v, want ErrAdvanceOwnershipLost for an expired lease", err)
	}
	if _, err := store.GetJob(ctx, child.ID); err == nil {
		t.Fatal("an expired lease authorized an irreversible insert")
	}
}

// TestOwnerScopedDeletesSpareALiveAdvanceLease sweeps the whole class of broad,
// owner-keyed deletes at once. Each is keyed on owner_job_id, and an advance lease
// carries the terminal child's owner_job_id, so any of them could take a live lock
// belonging to a different pass.
func TestOwnerScopedDeletesSpareALiveAdvanceLease(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		delete func(t *testing.T, store *Store, jobID string)
	}{
		{"DeleteResourceLocksByOwner", func(t *testing.T, store *Store, jobID string) {
			if _, err := store.DeleteResourceLocksByOwner(ctx, jobID, time.Now().UTC()); err != nil {
				t.Fatalf("DeleteResourceLocksByOwner: %v", err)
			}
		}},
		{"DeleteResourceLocksByOwnerIfNotRunning", func(t *testing.T, store *Store, jobID string) {
			if _, err := store.DeleteResourceLocksByOwnerIfNotRunning(ctx, jobID, time.Now().UTC()); err != nil {
				t.Fatalf("DeleteResourceLocksByOwnerIfNotRunning: %v", err)
			}
		}},
		{"ReleaseSupersededJobResourceLocksAtGeneration", func(t *testing.T, store *Store, jobID string) {
			if _, _, err := store.ReleaseSupersededJobResourceLocksAtGeneration(ctx, jobID, 0, time.Now().UTC()); err != nil {
				t.Fatalf("ReleaseSupersededJobResourceLocksAtGeneration: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			seedAdvanceOwnedJob(t, store, "j-swept", "failed")
			ownAdvanceLease(t, store, "j-swept", "tok-live", time.Now().UTC().Add(time.Minute))
			if owned, err := store.AcquireResourceLock(ctx, ResourceLock{
				ResourceKey: "runtime:codex:sess", OwnerJobID: "j-swept", OwnerToken: "runtime-tok",
				ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
			}, time.Now().UTC()); err != nil || !owned {
				t.Fatalf("seed ordinary lock owned=%v err=%v", owned, err)
			}

			tc.delete(t, store, "j-swept")

			if _, err := store.GetResourceLock(ctx, SupersedeAdvanceLockKeyPrefix+"j-swept"); err != nil {
				t.Fatalf("%s deleted another pass's live advance lease: %v", tc.name, err)
			}
			if _, err := store.GetResourceLock(ctx, "runtime:codex:sess"); err == nil {
				t.Fatalf("%s left an ordinary lock behind; the exclusion is too wide", tc.name)
			}
		})
	}
}

// TestOwnerScopedDeletesStillSweepAnAbandonedLease is the other half: the exclusion
// is liveness-scoped, so a killed pass's lapsed lease must still be reclaimed by
// exactly the same cleanups.
func TestOwnerScopedDeletesStillSweepAnAbandonedLease(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	seedAdvanceOwnedJob(t, store, "j-dead", "failed")
	ownAdvanceLease(t, store, "j-dead", "tok-dead", time.Now().UTC().Add(-time.Hour))

	if _, err := store.DeleteResourceLocksByOwner(ctx, "j-dead", time.Now().UTC()); err != nil {
		t.Fatalf("DeleteResourceLocksByOwner: %v", err)
	}
	if _, err := store.GetResourceLock(ctx, SupersedeAdvanceLockKeyPrefix+"j-dead"); err == nil {
		t.Fatal("an abandoned lease survived owner cleanup; a crashed pass would wedge retries")
	}
}
