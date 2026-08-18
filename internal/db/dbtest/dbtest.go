// Package dbtest provides isolated writable stores backed by a cached,
// fully-migrated schema template.
package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

var (
	templateOnce sync.Once
	templatePath string
	templateErr  error
	copyLocks    sync.Map
)

// Open copies the current migrated schema into path when path does not exist,
// then opens it without replaying migrations. Existing databases are never
// replaced, which preserves data when a test reopens the same path.
func Open(t *testing.T, path string) (*db.Store, error) {
	t.Helper()

	template, err := migratedTemplate()
	if err != nil {
		return nil, err
	}
	if err := copyTemplateIfMissing(template, path); err != nil {
		return nil, err
	}
	return db.OpenAlreadyMigrated(path)
}

func migratedTemplate() (string, error) {
	templateOnce.Do(func() {
		fingerprint := db.SchemaMigrationFingerprint()
		templatePath = filepath.Join(os.TempDir(), "gitmoot-test-schema-"+fingerprint[:12]+".db")
		templateErr = ensureMigratedTemplate(templatePath)
	})
	return templatePath, templateErr
}

func ensureMigratedTemplate(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("test schema template %s is not a regular file", path)
		}
		if err := validateMigratedTemplate(path); err == nil {
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat test schema template: %w", err)
	}

	temp, err := os.CreateTemp(filepath.Dir(path), ".gitmoot-test-schema-*.db")
	if err != nil {
		return fmt.Errorf("create test schema template: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close empty test schema template: %w", err)
	}
	defer func() {
		_ = os.Remove(tempPath)
		_ = os.Remove(tempPath + "-wal")
		_ = os.Remove(tempPath + "-shm")
	}()

	store, err := db.Open(tempPath)
	if err != nil {
		return fmt.Errorf("migrate test schema template: %w", err)
	}
	if err := checkpointWAL(tempPath); err != nil {
		_ = store.Close()
		return err
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close migrated test schema template: %w", err)
	}

	// Publish the identity sidecar BEFORE the database becomes visible. Validation
	// fails closed on a missing or mismatched sidecar, so a crash between the two
	// renames costs a rebuild and never yields an unidentified template.
	if err := stampTemplateIdentity(path); err != nil {
		return err
	}

	// The unique file is complete and closed before it becomes visible at the
	// shared cache path. Concurrent test binaries may race here; each candidate
	// is complete, and rename is atomic because both names share a directory.
	if err := os.Rename(tempPath, path); err != nil {
		if validateErr := validateMigratedTemplate(path); validateErr == nil {
			return nil
		}
		return fmt.Errorf("publish test schema template: %w", err)
	}
	return nil
}

// templateIdentityPath is the sidecar holding a template's FULL migration
// fingerprint. It cannot live inside the database: a copy of the template
// becomes a test's store, and any extra schema object would make that store
// differ from a freshly migrated one — the exact equivalence this package exists
// to guarantee, and which TestOpenMatchesFreshlyMigratedSchema asserts.
func templateIdentityPath(path string) string { return path + ".fingerprint" }

// stampTemplateIdentity records the full fingerprint next to the template, via
// create-temp + rename so a reader never observes a partial value.
func stampTemplateIdentity(path string) error {
	want := db.SchemaMigrationFingerprint()
	temp, err := os.CreateTemp(filepath.Dir(path), ".gitmoot-test-schema-id-*")
	if err != nil {
		return fmt.Errorf("create test schema identity sidecar: %w", err)
	}
	tempPath := temp.Name()
	if _, err := io.WriteString(temp, want); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("write test schema identity sidecar: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close test schema identity sidecar: %w", err)
	}
	if err := os.Rename(tempPath, templateIdentityPath(path)); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("publish test schema identity sidecar: %w", err)
	}
	return nil
}

func validateMigratedTemplate(path string) error {
	dsn := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	raw, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return fmt.Errorf("open cached test schema template: %w", err)
	}
	defer raw.Close()

	var integrity string
	if err := raw.QueryRowContext(context.Background(), `PRAGMA quick_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("check cached test schema template integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("cached test schema template integrity check returned %q", integrity)
	}
	var autoVacuum int
	if err := raw.QueryRowContext(context.Background(), `PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		return fmt.Errorf("read cached test schema auto_vacuum: %w", err)
	}
	if autoVacuum != db.SQLiteAutoVacuumIncremental {
		return fmt.Errorf("cached test schema auto_vacuum is %d, want %d", autoVacuum, db.SQLiteAutoVacuumIncremental)
	}

	var count, minimum, maximum int
	if err := raw.QueryRowContext(context.Background(), `
		SELECT COUNT(*), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0)
		FROM schema_migrations`).Scan(&count, &minimum, &maximum); err != nil {
		return fmt.Errorf("read cached test schema migration versions: %w", err)
	}
	want := db.SchemaMigrationCount()
	if count != want || minimum != 1 || maximum != want {
		return fmt.Errorf("cached test schema migration versions are count=%d range=%d..%d, want count=%d range=1..%d", count, minimum, maximum, want, want)
	}

	// Cardinality is not identity. schema_migrations records version NUMBERS, so a
	// template built from a DIFFERENT set of the same size satisfies every check
	// above, and the only other binding is a 48-bit filename prefix. Compare the
	// stamped full fingerprint so a wrong-schema template is rejected — and
	// therefore rebuilt — instead of silently backing every writable test store.
	stamped, err := os.ReadFile(templateIdentityPath(path))
	if err != nil {
		return fmt.Errorf("read cached test schema identity: %w", err)
	}
	if got := string(stamped); got != db.SchemaMigrationFingerprint() {
		return fmt.Errorf("cached test schema identity is %q, want %q", got, db.SchemaMigrationFingerprint())
	}
	return nil
}

func checkpointWAL(path string) error {
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open test schema template for checkpoint: %w", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint test schema template: %w", err)
	}
	return nil
}

func copyTemplateIfMissing(template, path string) error {
	lockValue, _ := copyLocks.LoadOrStore(filepath.Clean(path), &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat test database: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create test database directory: %w", err)
	}

	source, err := os.Open(template)
	if err != nil {
		return fmt.Errorf("open test schema template: %w", err)
	}
	defer source.Close()
	destination, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create test database: %w", err)
	}
	complete := false
	defer func() {
		_ = destination.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(destination, source); err != nil {
		return fmt.Errorf("copy test schema template: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close copied test database: %w", err)
	}
	complete = true
	return nil
}
