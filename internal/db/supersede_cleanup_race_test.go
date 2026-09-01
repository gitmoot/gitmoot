package db

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestReleaseSupersededJobResourceLocksAtGenerationSurvivesACrossStoreRetry is the
// cross-process proof the guarded release needs, and it uses TWO Stores on ONE
// database file because that is the only shape that reproduces the defect.
//
// The recovery pass and `gitmoot job retry` run in different processes. With a
// deferred transaction that SELECTs before it DELETEs, the SELECT takes a WAL read
// snapshot; a retry committed from the other connection then makes the DELETE fail
// SQLITE_BUSY_SNAPSHOT, and the caller could not tell that from "guard refused" —
// so the cleanup silently did not run while the debt was recorded paid. A
// single-Store test cannot show this: one connection pool serialises itself.
//
// The invariant asserted for every interleaving is the one that matters: the
// release either belongs to the claimed generation, or nothing of the NEW
// lifecycle's is touched. Never both.
//
// MUTATION PROOF: move the state/generation validation ahead of the DELETE (a
// SELECT-then-DELETE in the same transaction) and the retry arm below either
// returns an error the caller must not swallow, or deletes the new run's lock.
func TestReleaseSupersededJobResourceLocksAtGenerationSurvivesACrossStoreRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	recovery, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("open recovery store: %v", err)
	}
	retry, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("open retry store: %v", err)
	}
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	// Enough repetitions that the two connections genuinely interleave under -race.
	const rounds = 24
	for round := range rounds {
		jobID := fmt.Sprintf("child-%d", round)
		if err := recovery.CreateJob(ctx, Job{
			ID: jobID, Agent: "audit", Type: "review", State: "queued", Payload: `{"repo":"gitmoot/gitmoot"}`,
		}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		// The superseded run: terminal at the generation the recovery claims.
		if _, err := recovery.TransitionJobState(ctx, jobID, "queued", "failed"); err != nil {
			t.Fatalf("TransitionJobState: %v", err)
		}
		claimed, err := recovery.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		lockKey := "runtime:codex:session-" + jobID
		if locked, err := recovery.AcquireResourceLock(ctx, ResourceLock{
			ResourceKey: lockKey, OwnerJobID: jobID, OwnerToken: "token-old",
			ExpiresAt: now.Add(30 * time.Minute).Format(time.RFC3339Nano),
		}, now); err != nil || !locked {
			t.Fatalf("AcquireResourceLock acquired=%v err=%v", locked, err)
		}

		var wg sync.WaitGroup
		var released int64
		var guarded bool
		var releaseErr error
		var retryErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			released, guarded, releaseErr = recovery.ReleaseSupersededJobResourceLocksAtGeneration(ctx, jobID, claimed.LifecycleGeneration, time.Now().UTC())
		}()
		go func() {
			defer wg.Done()
			// The other process re-queues the job: a new lifecycle, from a SEPARATE
			// connection, at an unpredictable point in the release above.
			if _, err := retry.TransitionJobState(ctx, jobID, "failed", "queued"); err != nil {
				retryErr = err
				return
			}
			// ...and the new run acquires its own lock, exactly as a claimed retry does.
			if _, err := retry.AcquireResourceLock(ctx, ResourceLock{
				ResourceKey: lockKey + ":new", OwnerJobID: jobID, OwnerToken: "token-new",
				ExpiresAt: now.Add(30 * time.Minute).Format(time.RFC3339Nano),
			}, now); err != nil {
				retryErr = err
			}
		}()
		wg.Wait()
		if retryErr != nil {
			t.Fatalf("round %d: retry connection failed: %v", round, retryErr)
		}
		if releaseErr != nil {
			// The write-first formulation takes the write lock as its FIRST statement, so
			// it has no read snapshot to stale-upgrade: a cross-process re-queue either
			// loses the EXISTS predicate or serialises after the commit. Any error here
			// therefore means the implementation is NOT write-first — which is exactly the
			// SELECT-before-DELETE mutant, whose failure mode is SQLITE_BUSY_SNAPSHOT.
			// Accepting it was what let that mutant survive this test.
			t.Fatalf("round %d: guarded release returned %v; a write-first guard cannot stale-upgrade (guarded=%v)", round, releaseErr, guarded)
		}
		current, err := recovery.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob after race: %v", err)
		}
		newLock, lockErr := recovery.GetResourceLock(ctx, lockKey+":new")
		newLockHeld := lockErr == nil && newLock.OwnerJobID == jobID
		switch {
		case guarded:
			// The release won the race: it must have done so while the row was still
			// the claimed lifecycle, i.e. the retry had not yet committed its bump.
			if current.LifecycleGeneration != claimed.LifecycleGeneration && !newLockHeld {
				t.Fatalf("round %d: guarded release ran but the new lifecycle's lock is gone (generation %d -> %d)",
					round, claimed.LifecycleGeneration, current.LifecycleGeneration)
			}
		default:
			// The guard refused: the superseded run's lock may survive (the next poll
			// re-drives), but nothing of the new lifecycle may have been destroyed.
			if released != 0 {
				t.Fatalf("round %d: guard refused yet %d lock rows were deleted", round, released)
			}
		}
	}
}

// TestReleaseSupersededJobResourceLocksAtGenerationRefusesAnAlreadyRequeuedRow is
// the deterministic arm: the retry has already committed before the release runs,
// so the guard must refuse, delete nothing, and report guarded false without an
// error — the shape the caller turns into "leave the debt outstanding".
func TestReleaseSupersededJobResourceLocksAtGenerationRefusesAnAlreadyRequeuedRow(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	const jobID = "child-requeued"
	if err := store.CreateJob(ctx, Job{
		ID: jobID, Agent: "audit", Type: "review", State: "queued", Payload: `{"repo":"gitmoot/gitmoot"}`,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := store.TransitionJobState(ctx, jobID, "queued", "failed"); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}
	claimed, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if _, err := store.TransitionJobState(ctx, jobID, "failed", "queued"); err != nil {
		t.Fatalf("re-queue: %v", err)
	}
	const lockKey = "runtime:codex:session-requeued"
	if locked, err := store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: lockKey, OwnerJobID: jobID, OwnerToken: "token-new",
		ExpiresAt: now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	}, now); err != nil || !locked {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", locked, err)
	}

	released, guarded, err := store.ReleaseSupersededJobResourceLocksAtGeneration(ctx, jobID, claimed.LifecycleGeneration, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReleaseSupersededJobResourceLocksAtGeneration: %v", err)
	}
	if guarded || released != 0 {
		t.Fatalf("guarded=%v released=%d, want the guard to refuse a re-queued row", guarded, released)
	}
	if lock, err := store.GetResourceLock(ctx, lockKey); err != nil || lock.OwnerJobID != jobID {
		t.Fatalf("new lifecycle's lock = %+v err=%v, want still held", lock, err)
	}
}

// TestSupersedeDebtClosurePredicateMatchesTheGoParser pins the parity the P2
// finding named. Every message shape has to land in exactly one of anchored or
// unanchored in BOTH languages; a shape that is anchored-looking to SQL and
// unanchored to Go (or the reverse) is a debt no path can close.
//
// MUTATION PROOF: restore either the `generation=<n>: ` LIKE predicate or the
// `NOT LIKE 'generation=%'` complement and the malformed and non-canonical rows
// below become permanently pending.
func TestSupersedeDebtClosurePredicateMatchesTheGoParser(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{"canonical", "7"},
		{"zero", "0"},
		{"non-canonical leading zero", "07"},
		{"malformed legacy prefix", "generation=abc: pr closed"},
		{"legacy prefixed canonical", "generation=7: pr closed"},
		{"plain reason", "pr closed"},
		{"signed", "+7"},
		{"padded", " 7 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			const jobID = "debt-parity"
			if err := store.CreateJob(ctx, Job{
				ID: jobID, Agent: "audit", Type: "review", State: "failed", Payload: `{"repo":"gitmoot/gitmoot"}`,
			}); err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			if err := store.AddJobEvent(ctx, JobEvent{
				JobID: jobID, Kind: "supersede_finalize_pending", Message: tc.message,
			}); err != nil {
				t.Fatalf("AddJobEvent: %v", err)
			}
			// The Go side's classification, reproduced here as the test's oracle: a
			// canonical decimal names a generation, anything else does not.
			anchored := false
			var generation int64
			if parsed, err := parseCanonicalGenerationForTest(tc.message); err == nil {
				anchored, generation = true, parsed
			}

			closed, err := store.CloseSupersedeFinalizationDebtAtGeneration(ctx, jobID, "paid", generation, anchored)
			if err != nil {
				t.Fatalf("CloseSupersedeFinalizationDebtAtGeneration: %v", err)
			}
			if !closed {
				t.Fatalf("message %q classified anchored=%v generation=%d in Go was not closable in SQL: the debt is permanently pending",
					tc.message, anchored, generation)
			}
			// And the complementary classification must NOT close it, or a debt could be
			// cleared by a payment that never matched it.
			if err := store.AddJobEvent(ctx, JobEvent{
				JobID: jobID, Kind: "supersede_finalize_pending", Message: tc.message,
			}); err != nil {
				t.Fatalf("re-arm: %v", err)
			}
			wrongClose, err := store.CloseSupersedeFinalizationDebtAtGeneration(ctx, jobID, "paid", generation, !anchored)
			if err != nil {
				t.Fatalf("complementary close: %v", err)
			}
			if wrongClose {
				t.Fatalf("message %q was closed by the complementary classification too; the predicates overlap", tc.message)
			}
		})
	}
}

// parseCanonicalGenerationForTest mirrors workflow.parseSupersedeFinalizeDebt. It
// is duplicated here deliberately: internal/db cannot import internal/workflow, and
// the point of the test is that the SQL predicate agrees with THIS rule.
func parseCanonicalGenerationForTest(message string) (int64, error) {
	var parsed int64
	if _, err := fmt.Sscanf(message, "%d", &parsed); err != nil {
		return 0, err
	}
	if fmt.Sprintf("%d", parsed) != message {
		return 0, fmt.Errorf("non-canonical")
	}
	return parsed, nil
}
