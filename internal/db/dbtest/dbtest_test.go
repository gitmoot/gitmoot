package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

type schemaSnapshot struct {
	autoVacuum        int
	migrationVersions []int
	schemaObjects     []string
}

func TestOpenMatchesFreshlyMigratedSchema(t *testing.T) {
	ctx := context.Background()
	realPath := filepath.Join(t.TempDir(), "real.db")
	realStore, err := db.Open(realPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer realStore.Close()

	cachedPath := filepath.Join(t.TempDir(), "cached.db")
	cachedStore, err := Open(t, cachedPath)
	if err != nil {
		t.Fatalf("dbtest.Open: %v", err)
	}
	defer cachedStore.Close()

	realSnapshot := readSchemaSnapshot(t, realPath)
	cachedSnapshot := readSchemaSnapshot(t, cachedPath)
	if !reflect.DeepEqual(cachedSnapshot, realSnapshot) {
		t.Fatalf("cached schema differs from freshly migrated schema\n cached: %#v\n   real: %#v", cachedSnapshot, realSnapshot)
	}
	if cachedSnapshot.autoVacuum != db.SQLiteAutoVacuumIncremental {
		t.Fatalf("cached auto_vacuum = %d, want %d (INCREMENTAL)", cachedSnapshot.autoVacuum, db.SQLiteAutoVacuumIncremental)
	}
	if got, want := len(cachedSnapshot.migrationVersions), db.SchemaMigrationCount(); got != want {
		t.Fatalf("cached migration count = %d, want %d", got, want)
	}

	before := readMigrationBookkeeping(t, cachedPath)
	if err := cachedStore.Migrate(ctx); err != nil {
		t.Fatalf("Migrate cached store: %v", err)
	}
	after := readMigrationBookkeeping(t, cachedPath)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("Migrate changed cached schema bookkeeping\n before: %v\n  after: %v", before, after)
	}
}

func TestOpenPreservesExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopened.db")
	store, err := Open(t, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "seeded", Agent: "worker", Type: "ask", State: "queued"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := Open(t, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer reopened.Close()
	job, err := reopened.GetJob(ctx, "seeded")
	if err != nil {
		t.Fatalf("GetJob after reopen: %v", err)
	}
	if job.ID != "seeded" {
		t.Fatalf("reopened job ID = %q, want seeded", job.ID)
	}
}

func TestOpenConcurrentSamePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	const callers = 4
	stores := make(chan *db.Store, callers)
	errors := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			store, err := Open(t, path)
			if err != nil {
				errors <- err
				return
			}
			stores <- store
		}()
	}
	wait.Wait()
	close(errors)
	close(stores)
	for err := range errors {
		t.Errorf("Open shared path: %v", err)
	}
	for store := range stores {
		if err := store.Close(); err != nil {
			t.Errorf("Close shared store: %v", err)
		}
	}
}

func readSchemaSnapshot(t *testing.T, path string) schemaSnapshot {
	t.Helper()
	raw := openRaw(t, path)
	defer raw.Close()

	var snapshot schemaSnapshot
	if err := raw.QueryRow(`PRAGMA auto_vacuum`).Scan(&snapshot.autoVacuum); err != nil {
		t.Fatalf("read auto_vacuum: %v", err)
	}
	rows, err := raw.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema migrations: %v", err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			t.Fatalf("scan schema migration: %v", err)
		}
		snapshot.migrationVersions = append(snapshot.migrationVersions, version)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close schema migration rows: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema migrations: %v", err)
	}

	rows, err = raw.Query(`
		SELECT type || ':' || name || ':' || tbl_name || ':' || COALESCE(sql, '')
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name`)
	if err != nil {
		t.Fatalf("query sqlite schema: %v", err)
	}
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			rows.Close()
			t.Fatalf("scan sqlite schema: %v", err)
		}
		snapshot.schemaObjects = append(snapshot.schemaObjects, object)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close sqlite schema rows: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite schema: %v", err)
	}
	return snapshot
}

func readMigrationBookkeeping(t *testing.T, path string) []string {
	t.Helper()
	raw := openRaw(t, path)
	defer raw.Close()
	rows, err := raw.Query(`SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query migration bookkeeping: %v", err)
	}
	defer rows.Close()
	var bookkeeping []string
	for rows.Next() {
		var version int
		var appliedAt string
		if err := rows.Scan(&version, &appliedAt); err != nil {
			t.Fatalf("scan migration bookkeeping: %v", err)
		}
		bookkeeping = append(bookkeeping, fmt.Sprintf("%d:%s", version, appliedAt))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migration bookkeeping: %v", err)
	}
	return bookkeeping
}

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", path, err)
	}
	return raw
}
