package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
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

func TestEnsureMigratedTemplateReplacesInvalidRegularFile(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "empty", content: nil},
		{name: "not_sqlite", content: []byte("not a sqlite database")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := filepath.Join(t.TempDir(), "schema.db")
			if err := os.WriteFile(template, test.content, 0o600); err != nil {
				t.Fatalf("write invalid template: %v", err)
			}
			if err := ensureMigratedTemplate(template); err != nil {
				t.Fatalf("repair invalid template: %v", err)
			}
			if err := validateMigratedTemplate(template); err != nil {
				t.Fatalf("validate repaired template: %v", err)
			}

			copyPath := filepath.Join(t.TempDir(), "copy.db")
			if err := copyTemplateIfMissing(template, copyPath); err != nil {
				t.Fatalf("copy repaired template: %v", err)
			}
			store, err := db.OpenAlreadyMigrated(copyPath)
			if err != nil {
				t.Fatalf("open repaired template copy: %v", err)
			}
			defer store.Close()
			if err := store.CreateJob(context.Background(), db.Job{ID: "usable", Agent: "worker", Type: "ask", State: "queued"}); err != nil {
				t.Fatalf("use repaired template copy: %v", err)
			}
		})
	}
}

// TestEnsureMigratedTemplateReplacesForeignIdentity covers the case the other
// repair tests cannot reach: a template that is a perfectly valid, fully
// migrated database — passing quick_check, carrying auto_vacuum=INCREMENTAL and
// all SchemaMigrationCount() versions numbered 1..N — but built from a DIFFERENT
// migration set. schema_migrations records version NUMBERS, not content, so
// cardinality alone cannot distinguish it, and the cache path only carries a
// 48-bit fingerprint prefix. Only the stamped full fingerprint can reject it.
func TestEnsureMigratedTemplateReplacesForeignIdentity(t *testing.T) {
	template := filepath.Join(t.TempDir(), "schema.db")
	if err := ensureMigratedTemplate(template); err != nil {
		t.Fatalf("build template: %v", err)
	}
	if err := validateMigratedTemplate(template); err != nil {
		t.Fatalf("freshly built template must validate: %v", err)
	}

	// Forge the identity of an otherwise-perfect template.
	foreign := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := os.WriteFile(db.TestTemplateIdentityPath(template), []byte(foreign), 0o600); err != nil {
		t.Fatalf("write foreign identity: %v", err)
	}
	if err := validateMigratedTemplate(template); err == nil {
		t.Fatal("validation accepted a template whose stamped identity is not the current fingerprint")
	}

	// A foreign identity must cause a REBUILD, not a hard failure: a stale cache
	// entry is an ordinary condition on a long-lived host.
	if err := ensureMigratedTemplate(template); err != nil {
		t.Fatalf("repair foreign-identity template: %v", err)
	}
	if err := validateMigratedTemplate(template); err != nil {
		t.Fatalf("validate rebuilt template: %v", err)
	}

	// A missing sidecar must fail closed as well, so a crash between the two
	// publishes can never yield an unidentified template.
	if err := os.Remove(db.TestTemplateIdentityPath(template)); err != nil {
		t.Fatalf("remove identity sidecar: %v", err)
	}
	if err := validateMigratedTemplate(template); err == nil {
		t.Fatal("validation accepted a template with no stamped identity")
	}
}

// TestStampOrderRejectsStaleDatabaseWithFreshIdentity pins the publication ORDER,
// which round-4 review found inverted in my first version of this fix.
//
// The sidecar vouches for the database, so it must be published SECOND. If it is
// stamped first, a crash before the database rename leaves a NEW fingerprint
// sitting beside the OLD database — and validation then blesses a stale schema,
// which is strictly worse than no cache at all because ~600 tests consume it
// while OpenAlreadyMigrated deliberately skips Migrate.
//
// This test simulates that window directly: an old database at the cache path
// with a current-fingerprint sidecar must be REJECTED, not accepted.
func TestValidationRejectsStaleDatabaseWithFreshIdentity(t *testing.T) {
	template := filepath.Join(t.TempDir(), "schema.db")
	if err := ensureMigratedTemplate(template); err != nil {
		t.Fatalf("build template: %v", err)
	}

	// Stand in for "an older database that is structurally fine but not the
	// current schema": drop a migration row so cardinality no longer matches.
	raw, err := sql.Open("sqlite", template)
	if err != nil {
		t.Fatalf("open template: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(),
		`DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)`); err != nil {
		_ = raw.Close()
		t.Fatalf("age the template: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close aged template: %v", err)
	}

	// Re-stamp a CURRENT identity beside that stale database — exactly what the
	// inverted order produced after a crash.
	if err := db.StampTestTemplateIdentity(template); err != nil {
		t.Fatalf("stamp identity: %v", err)
	}
	if err := validateMigratedTemplate(template); err == nil {
		t.Fatal("validation blessed a stale database carrying a current identity sidecar")
	}

	// And the recovery path still works: rebuild, then validate.
	if err := ensureMigratedTemplate(template); err != nil {
		t.Fatalf("rebuild after stale detection: %v", err)
	}
	if err := validateMigratedTemplate(template); err != nil {
		t.Fatalf("validate rebuilt template: %v", err)
	}
}

func TestEnsureMigratedTemplateReplacesWrongAutoVacuum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE cache_seed (id INTEGER PRIMARY KEY)`); err != nil {
		raw.Close()
		t.Fatalf("seed existing database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}
	store, err := db.Open(path)
	if err != nil {
		t.Fatalf("migrate seed database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated seed database: %v", err)
	}
	if got := readSchemaSnapshot(t, path).autoVacuum; got == db.SQLiteAutoVacuumIncremental {
		t.Fatalf("seed auto_vacuum unexpectedly equals INCREMENTAL")
	}

	if err := ensureMigratedTemplate(path); err != nil {
		t.Fatalf("repair wrong auto_vacuum template: %v", err)
	}
	if got := readSchemaSnapshot(t, path).autoVacuum; got != db.SQLiteAutoVacuumIncremental {
		t.Fatalf("repaired auto_vacuum = %d, want %d", got, db.SQLiteAutoVacuumIncremental)
	}
}

func TestEnsureMigratedTemplateReplacesIncompleteMigrationBookkeeping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.db")
	if err := ensureMigratedTemplate(path); err != nil {
		t.Fatalf("create valid template: %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open template: %v", err)
	}
	if _, err := raw.Exec(`DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)`); err != nil {
		raw.Close()
		t.Fatalf("remove migration bookkeeping: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close incomplete template: %v", err)
	}

	if err := ensureMigratedTemplate(path); err != nil {
		t.Fatalf("repair incomplete template: %v", err)
	}
	if err := validateMigratedTemplate(path); err != nil {
		t.Fatalf("validate repaired template: %v", err)
	}
	if got, want := len(readSchemaSnapshot(t, path).migrationVersions), db.SchemaMigrationCount(); got != want {
		t.Fatalf("repaired migration count = %d, want %d", got, want)
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

// TestPublishOrderSurvivesCrashBetweenRenames pins the ORDER of the two
// publishes, which no ordinary test can reach: whatever order they happen in, a
// COMPLETED build ends up consistent, so only an interruption between them tells
// correct from inverted. I learned this the hard way — my first attempt at an
// order test passed happily against a deliberately inverted implementation.
//
// Correct order (database, then identity): an interruption leaves a database
// with no sidecar, so validation fails closed and the next run rebuilds.
// Inverted order (identity, then database): the interruption leaves a CURRENT
// fingerprint beside whatever database was already there, and validation blesses
// a stale schema.
func TestPublishOrderSurvivesCrashBetweenRenames(t *testing.T) {
	template := filepath.Join(t.TempDir(), "schema.db")

	crash := errors.New("simulated crash between publishes")
	previous := afterTemplatePublish
	afterTemplatePublish = func() error { return crash }
	t.Cleanup(func() { afterTemplatePublish = previous })

	if err := ensureMigratedTemplate(template); !errors.Is(err, crash) {
		t.Fatalf("ensureMigratedTemplate error = %v, want the simulated crash", err)
	}

	// The database exists (it was published first) but carries no identity, so it
	// MUST NOT validate. Under the inverted order the sidecar would already be
	// present here and this check would pass, which is the bug being pinned.
	if _, err := os.Stat(template); err != nil {
		t.Fatalf("database should already be published before the crash: %v", err)
	}
	if err := validateMigratedTemplate(template); err == nil {
		t.Fatal("validation accepted a database published without its identity sidecar")
	}

	// Recovery: a normal run rebuilds and stamps.
	afterTemplatePublish = previous
	if err := ensureMigratedTemplate(template); err != nil {
		t.Fatalf("rebuild after crash: %v", err)
	}
	if err := validateMigratedTemplate(template); err != nil {
		t.Fatalf("validate rebuilt template: %v", err)
	}
}
