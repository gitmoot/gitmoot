package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestExecBackendAttemptsMigrationFreshAndCached(t *testing.T) {
	ctx := context.Background()
	fresh, err := openRealTestStore(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open freshly migrated store: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	assertExecBackendAttemptsTable(t, ctx, fresh, "freshly migrated database")

	cached, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "cached.db"))
	if err != nil {
		t.Fatalf("open already-migrated cached store: %v", err)
	}
	t.Cleanup(func() { _ = cached.Close() })
	assertExecBackendAttemptsTable(t, ctx, cached, "OpenAlreadyMigrated database")
}

func TestMigrateAddsExecBackendAttempts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	preMigration := &Store{db: raw}
	for version, migration := range migrationsBefore(t, "CREATE TABLE execbackend_attempts") {
		if err := preMigration.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	if exists, err := preMigration.HasTable(ctx, "execbackend_attempts"); err != nil {
		t.Fatalf("HasTable before migration: %v", err)
	} else if exists {
		t.Fatal("execbackend_attempts exists before its content-located migration")
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close pre-migration database: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	assertExecBackendAttemptsTable(t, ctx, upgraded, "upgraded database")
}

func TestReserveExecBackendAttempt(t *testing.T) {
	store, key := reserveTestExecBackendAttempt(t, "reserve")
	attempt := getTestExecBackendAttempt(t, store, key)
	if attempt.State != ExecBackendAttemptStateReserved {
		t.Fatalf("state = %q, want reserved", attempt.State)
	}
	if attempt.SandboxID != nil {
		t.Fatalf("sandbox_id = %v, want NULL during reserve/create crash window", attempt.SandboxID)
	}
	if attempt.CostReservedUSD == nil || *attempt.CostReservedUSD != 2.75 {
		t.Fatalf("cost_reserved_usd = %v, want 2.75 written with reservation", attempt.CostReservedUSD)
	}
	if attempt.CostActualUSD != nil {
		t.Fatalf("cost_actual_usd = %v, want NULL before collection", attempt.CostActualUSD)
	}
	if attempt.DaemonFencingToken != "daemon-start-7" || attempt.BootID != "boot-3" {
		t.Fatalf("fencing identity = (%q, %q), want daemon token and boot id", attempt.DaemonFencingToken, attempt.BootID)
	}

	if err := store.ReserveExecBackendAttempt(context.Background(), testExecBackendReservation(key)); err == nil {
		t.Fatal("duplicate reservation unexpectedly succeeded")
	}
}

func TestMarkExecBackendAttemptProvisioning(t *testing.T) {
	store, key := reserveTestExecBackendAttempt(t, "provisioning")
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptProvisioning(context.Background(), key))
	assertTestExecBackendState(t, store, key, ExecBackendAttemptStateProvisioning)
	requireExecBackendTransitionRejected(t)(store.MarkExecBackendAttemptProvisioning(context.Background(), key))
}

func TestMarkExecBackendAttemptRunning(t *testing.T) {
	store, key := reserveTestExecBackendAttempt(t, "running")
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptProvisioning(context.Background(), key))
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptRunning(context.Background(), key, "sandbox-42"))
	attempt := getTestExecBackendAttempt(t, store, key)
	if attempt.State != ExecBackendAttemptStateRunning {
		t.Fatalf("state = %q, want running", attempt.State)
	}
	if attempt.SandboxID == nil || *attempt.SandboxID != "sandbox-42" {
		t.Fatalf("sandbox_id = %v, want sandbox-42", attempt.SandboxID)
	}
	requireExecBackendTransitionRejected(t)(store.MarkExecBackendAttemptRunning(context.Background(), key, "sandbox-other"))
}

func TestMarkExecBackendAttemptCollecting(t *testing.T) {
	store, key := runningTestExecBackendAttempt(t, "collecting")
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptCollecting(context.Background(), key))
	assertTestExecBackendState(t, store, key, ExecBackendAttemptStateCollecting)
	requireExecBackendTransitionRejected(t)(store.MarkExecBackendAttemptCollecting(context.Background(), key))
}

func TestMarkExecBackendAttemptDestroying(t *testing.T) {
	store, key := runningTestExecBackendAttempt(t, "destroying")
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptCollecting(context.Background(), key))
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptDestroying(context.Background(), key))
	assertTestExecBackendState(t, store, key, ExecBackendAttemptStateDestroying)
	requireExecBackendTransitionRejected(t)(store.MarkExecBackendAttemptDestroying(context.Background(), key))
}

func TestMarkExecBackendAttemptDestroyed(t *testing.T) {
	store, key := runningTestExecBackendAttempt(t, "destroyed")
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptCollecting(context.Background(), key))
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptDestroying(context.Background(), key))
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptDestroyed(context.Background(), key, 1.25))
	attempt := getTestExecBackendAttempt(t, store, key)
	if attempt.State != ExecBackendAttemptStateDestroyed {
		t.Fatalf("state = %q, want destroyed", attempt.State)
	}
	if attempt.CostActualUSD == nil || *attempt.CostActualUSD != 1.25 {
		t.Fatalf("cost_actual_usd = %v, want 1.25", attempt.CostActualUSD)
	}
	requireExecBackendTransitionRejected(t)(store.MarkExecBackendAttemptDestroyed(context.Background(), key, 2.5))
}

func TestMarkExecBackendAttemptOrphaned(t *testing.T) {
	store, key := reserveTestExecBackendAttempt(t, "orphaned")
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptOrphaned(context.Background(), key))
	assertTestExecBackendState(t, store, key, ExecBackendAttemptStateOrphaned)
	requireExecBackendTransitionRejected(t)(store.MarkExecBackendAttemptOrphaned(context.Background(), key))
}

func TestMarkExecBackendAttemptFailed(t *testing.T) {
	store, key := runningTestExecBackendAttempt(t, "failed")
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptFailed(context.Background(), key))
	assertTestExecBackendState(t, store, key, ExecBackendAttemptStateFailed)
	requireExecBackendTransitionRejected(t)(store.MarkExecBackendAttemptFailed(context.Background(), key))
}

func TestListExecBackendAttemptsWithoutSandboxID(t *testing.T) {
	store, nullKey := reserveTestExecBackendAttempt(t, "null-sandbox")
	runningKey := ExecBackendAttemptKey{JobID: "running-has-handle", Attempt: 1, LifecycleGeneration: 4}
	if err := store.ReserveExecBackendAttempt(context.Background(), testExecBackendReservation(runningKey)); err != nil {
		t.Fatalf("reserve running row: %v", err)
	}
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptProvisioning(context.Background(), runningKey))
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptRunning(context.Background(), runningKey, "sandbox-present"))

	attempts, err := store.ListExecBackendAttemptsWithoutSandboxID(context.Background())
	if err != nil {
		t.Fatalf("ListExecBackendAttemptsWithoutSandboxID: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ExecBackendAttemptKey != nullKey {
		t.Fatalf("NULL-sandbox attempts = %+v, want only %+v", attempts, nullKey)
	}
	if attempts[0].SandboxID != nil {
		t.Fatalf("listed sandbox_id = %v, want nil", attempts[0].SandboxID)
	}
}

func assertExecBackendAttemptsTable(t *testing.T, ctx context.Context, store *Store, destination string) {
	t.Helper()
	exists, err := store.HasTable(ctx, "execbackend_attempts")
	if err != nil {
		t.Fatalf("HasTable at %s: %v", destination, err)
	}
	if !exists {
		t.Fatalf("execbackend_attempts absent from %s", destination)
	}
	type columnContract struct {
		name     string
		typeName string
		notNull  bool
		pk       int
	}
	want := []columnContract{
		{name: "job_id", typeName: "TEXT", notNull: true, pk: 1},
		{name: "attempt", typeName: "INTEGER", notNull: true, pk: 2},
		{name: "lifecycle_generation", typeName: "INTEGER", pk: 3},
		{name: "provider", typeName: "TEXT", notNull: true},
		{name: "sandbox_id", typeName: "TEXT"},
		{name: "daemon_fencing_token", typeName: "TEXT", notNull: true},
		{name: "boot_id", typeName: "TEXT", notNull: true},
		{name: "ttl_expires_at", typeName: "TIMESTAMP", notNull: true},
		{name: "state", typeName: "TEXT", notNull: true},
		{name: "cost_reserved_usd", typeName: "REAL"},
		{name: "cost_actual_usd", typeName: "REAL"},
		{name: "created_at", typeName: "TIMESTAMP", notNull: true},
		{name: "updated_at", typeName: "TIMESTAMP", notNull: true},
	}
	rows, err := store.db.QueryContext(ctx, `PRAGMA table_info(execbackend_attempts)`)
	if err != nil {
		t.Fatalf("read execbackend_attempts columns at %s: %v", destination, err)
	}
	defer rows.Close()
	var got []columnContract
	for rows.Next() {
		var (
			cid          int
			column       columnContract
			notNull      int
			defaultValue sql.NullString
		)
		if err := rows.Scan(&cid, &column.name, &column.typeName, &notNull, &defaultValue, &column.pk); err != nil {
			t.Fatalf("scan execbackend_attempts column at %s: %v", destination, err)
		}
		column.notNull = notNull != 0
		got = append(got, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate execbackend_attempts columns at %s: %v", destination, err)
	}
	if len(got) != len(want) {
		t.Fatalf("execbackend_attempts columns at %s = %+v, want %+v", destination, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execbackend_attempts column %d at %s = %+v, want %+v", i, destination, got[i], want[i])
		}
	}
}

func reserveTestExecBackendAttempt(t *testing.T, jobID string) (*Store, ExecBackendAttemptKey) {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("openCachedTestStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := ExecBackendAttemptKey{JobID: jobID, Attempt: 2, LifecycleGeneration: 4}
	if err := store.ReserveExecBackendAttempt(context.Background(), testExecBackendReservation(key)); err != nil {
		t.Fatalf("ReserveExecBackendAttempt: %v", err)
	}
	return store, key
}

func runningTestExecBackendAttempt(t *testing.T, jobID string) (*Store, ExecBackendAttemptKey) {
	t.Helper()
	store, key := reserveTestExecBackendAttempt(t, jobID)
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptProvisioning(context.Background(), key))
	requireExecBackendTransition(t)(store.MarkExecBackendAttemptRunning(context.Background(), key, "sandbox-running"))
	return store, key
}

func testExecBackendReservation(key ExecBackendAttemptKey) ExecBackendAttemptReservation {
	return ExecBackendAttemptReservation{
		ExecBackendAttemptKey: key,
		Provider:              "e2b",
		DaemonFencingToken:    "daemon-start-7",
		BootID:                "boot-3",
		TTLExpiresAt:          time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC),
		CostReservedUSD:       2.75,
	}
}

func getTestExecBackendAttempt(t *testing.T, store *Store, key ExecBackendAttemptKey) ExecBackendAttempt {
	t.Helper()
	attempt, err := store.GetExecBackendAttempt(context.Background(), key)
	if err != nil {
		t.Fatalf("GetExecBackendAttempt(%+v): %v", key, err)
	}
	return attempt
}

func assertTestExecBackendState(t *testing.T, store *Store, key ExecBackendAttemptKey, want string) {
	t.Helper()
	if got := getTestExecBackendAttempt(t, store, key).State; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
}

func requireExecBackendTransition(t *testing.T) func(bool, error) {
	t.Helper()
	return func(changed bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("transition returned error: %v", err)
		}
		if !changed {
			t.Fatal("transition did not change its expected row")
		}
	}
}

func requireExecBackendTransitionRejected(t *testing.T) func(bool, error) {
	t.Helper()
	return func(changed bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("rejected transition returned error: %v", err)
		}
		if changed {
			t.Fatal("transition changed a row from the wrong state")
		}
	}
}
