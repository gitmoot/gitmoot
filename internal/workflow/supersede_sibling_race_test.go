package workflow

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	dbtest "github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// TestSupersedeDebtMarkerClassificationIsCanonicalOnly is the Go half of the
// parser/SQL parity. The SQL side accepts a marker as naming a generation only
// when CAST(CAST(message AS INTEGER) AS TEXT) = message, so the Go side must apply
// exactly that rule: anything else is a marker that names no generation. A parser
// that were more permissive would classify `07` as generation 7 and then ask SQL
// to close a debt SQL does not consider anchored — a debt nothing can ever close.
//
// MUTATION PROOF: drop the FormatInt round-trip in parseSupersedeFinalizeDebt and
// the non-canonical rows below flip to anchored.
func TestSupersedeDebtMarkerClassificationIsCanonicalOnly(t *testing.T) {
	for _, tc := range []struct {
		message        string
		wantGeneration int64
		wantAnchored   bool
	}{
		{message: "0", wantGeneration: 0, wantAnchored: true},
		{message: "7", wantGeneration: 7, wantAnchored: true},
		{message: "1234567890", wantGeneration: 1234567890, wantAnchored: true},
		{message: "07"},
		{message: "+7"},
		{message: " 7"},
		{message: "7 "},
		{message: "-7", wantGeneration: -7, wantAnchored: true},
		{message: "generation=7: pr closed"},
		{message: "pr closed"},
		{message: ""},
	} {
		t.Run("marker "+strconv.Quote(tc.message), func(t *testing.T) {
			generation, anchored := parseSupersedeFinalizeDebt(tc.message)
			if anchored != tc.wantAnchored || generation != tc.wantGeneration {
				t.Fatalf("parse(%q) = (%d, %v), want (%d, %v)", tc.message, generation, anchored, tc.wantGeneration, tc.wantAnchored)
			}
			if tc.wantAnchored && formatSupersedeFinalizeDebt(generation) != tc.message {
				t.Fatalf("format(%d) = %q, want the round-trip to reproduce %q", generation, formatSupersedeFinalizeDebt(generation), tc.message)
			}
		})
	}
}

// TestSupersedeCleanupFailureLeavesTheDebtOutstanding pins the half of the
// cross-process finding that is about ERROR HANDLING rather than ordering.
//
// Under WAL a cleanup write can fail for reasons that are not "the guard refused"
// — SQLITE_BUSY_SNAPSHOT from another process's commit being the one the review
// named. Swallowing that error and then recording the debt paid destroyed it: the
// locks stayed held and no later poll could rediscover the obligation. The error
// must propagate and the marker must stay pending.
//
// The fault is injected with a trigger on the lock delete, which is the same class
// of failure with a deterministic trigger.
//
// MUTATION PROOF: swallow cleanupErr in completeSupersedeFinalization and the debt
// is closed while the lock survives.
func TestSupersedeCleanupFailureLeavesTheDebtOutstanding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store := openEngineStoreAt(t, path)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	const child = "workflow-cleanup-fault"
	insertQueuedJob(t, store, db.Job{ID: child, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	observed := mustJob(t, store, child)
	superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobCancelled),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: child, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)})
	if err != nil || !superseded {
		t.Fatalf("supersede: transitioned=%v err=%v", superseded, err)
	}
	// A lock the cleanup will try, and fail, to delete.
	if locked, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-fault", OwnerJobID: child, OwnerToken: "token",
		ExpiresAt: "2030-01-01T00:00:00Z",
	}, mustParseTestTime(t, "2026-09-01T09:00:00Z")); err != nil || !locked {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", locked, err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open trigger connection: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`
CREATE TRIGGER fail_supersede_lock_release
BEFORE DELETE ON resource_locks
WHEN OLD.owner_job_id = 'workflow-cleanup-fault'
BEGIN
  SELECT RAISE(ABORT, 'injected cleanup failure');
END;`); err != nil {
		t.Fatalf("create cleanup failure trigger: %v", err)
	}

	engine := testEngine(store)
	if _, err := engine.CompletePendingSupersedeFinalization(ctx, child); err == nil {
		t.Fatal("the cleanup failure was swallowed; the caller cannot know the cleanup did not run")
	}
	debt, err := latestSupersedeFinalizeDebt(ctx, store, child)
	if err != nil {
		t.Fatalf("latestSupersedeFinalizeDebt: %v", err)
	}
	if !debt.pending {
		t.Fatal("the debt was recorded paid while its cleanup failed; nothing can rediscover it")
	}
	if lock, err := store.GetResourceLock(ctx, "runtime:codex:session-fault"); err != nil || lock.OwnerJobID != child {
		t.Fatalf("resource lock = %+v err=%v, want still held (the delete was aborted)", lock, err)
	}
	// And once the fault clears, the same debt is payable.
	if _, err := raw.Exec(`DROP TRIGGER fail_supersede_lock_release`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := engine.CompletePendingSupersedeFinalization(ctx, child); err != nil {
		t.Fatalf("recovery after the fault cleared: %v", err)
	}
	if after, err := latestSupersedeFinalizeDebt(ctx, store, child); err != nil || after.pending {
		t.Fatalf("debt = %+v err=%v, want closed once the cleanup succeeded", after, err)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-fault"); err == nil {
		t.Fatal("the lock survived a successful cleanup")
	}
}

// openEngineStoreAt is openEngineStore with a CALLER-CHOSEN path, so a test can
// open a second raw connection to the same file and inject a fault.
func openEngineStoreAt(t *testing.T, path string) *db.Store {
	t.Helper()
	store, err := dbtest.Open(t, path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func mustParseTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

func insertQueuedJob(t *testing.T, store *db.Store, job db.Job, payload JobPayload) {
	t.Helper()
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	job.State = string(JobQueued)
	job.Payload = encoded
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatalf("CreateJob(%s) returned error: %v", job.ID, err)
	}
}
