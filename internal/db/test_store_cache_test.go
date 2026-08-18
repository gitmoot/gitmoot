package db

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
)

var (
	cachedTestTemplateMu    sync.Mutex
	cachedTestTemplateReady bool
	cachedTestTemplatePath  string
	cachedTestCopyLocks     sync.Map
)

const cachedTestTemplatePublicationAttempts = 3

func openCachedTestStore(t *testing.T, path string) (*Store, error) {
	t.Helper()
	template, err := cachedMigratedTestTemplate()
	if err != nil {
		return nil, err
	}
	if err := copyCachedTestTemplateIfMissing(template, path); err != nil {
		return nil, err
	}
	return OpenAlreadyMigrated(path)
}

func cachedMigratedTestTemplate() (string, error) {
	cachedTestTemplateMu.Lock()
	defer cachedTestTemplateMu.Unlock()
	if cachedTestTemplateReady {
		return cachedTestTemplatePath, nil
	}
	fingerprint := SchemaMigrationFingerprint()
	cachedTestTemplatePath = filepath.Join(os.TempDir(), "gitmoot-test-schema-"+fingerprint[:12]+".db")
	if err := ensureCachedMigratedTestTemplate(cachedTestTemplatePath); err != nil {
		return cachedTestTemplatePath, err
	}
	cachedTestTemplateReady = true
	return cachedTestTemplatePath, nil
}

func ensureCachedMigratedTestTemplate(path string) error {
	for attempt := 1; attempt <= cachedTestTemplatePublicationAttempts; attempt++ {
		retry, err := ensureCachedMigratedTestTemplateOnce(path)
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
	}
	return fmt.Errorf("test schema template at %s was repeatedly replaced during publication", path)
}

func ensureCachedMigratedTestTemplateOnce(path string) (bool, error) {
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("test schema template %s is not a regular file", path)
		}
		if err := validateCachedMigratedTestTemplate(path); err == nil {
			return false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat test schema template: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".gitmoot-test-schema-*.db")
	if err != nil {
		return false, fmt.Errorf("create test schema template: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return false, fmt.Errorf("close empty test schema template: %w", err)
	}
	defer func() {
		_ = os.Remove(tempPath)
		_ = os.Remove(tempPath + "-wal")
		_ = os.Remove(tempPath + "-shm")
	}()
	store, err := Open(tempPath)
	if err != nil {
		return false, fmt.Errorf("migrate test schema template: %w", err)
	}
	if _, err := store.db.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = store.Close()
		return false, fmt.Errorf("checkpoint test schema template: %w", err)
	}
	if err := store.Close(); err != nil {
		return false, fmt.Errorf("close migrated test schema template: %w", err)
	}
	// Publish + stamp as one operation, shared with internal/db/dbtest: both caches
	// use the SAME path, so provenance must be guaranteed identically for both.
	published, err := PublishMigratedTestTemplate(tempPath, path)
	return !published, err
}

func validateCachedMigratedTestTemplate(path string) error {
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
	if autoVacuum != SQLiteAutoVacuumIncremental {
		return fmt.Errorf("cached test schema auto_vacuum is %d, want %d", autoVacuum, SQLiteAutoVacuumIncremental)
	}

	var count, minimum, maximum int
	if err := raw.QueryRowContext(context.Background(), `
		SELECT COUNT(*), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0)
		FROM schema_migrations`).Scan(&count, &minimum, &maximum); err != nil {
		return fmt.Errorf("read cached test schema migration versions: %w", err)
	}
	want := SchemaMigrationCount()
	if count != want || minimum != 1 || maximum != want {
		return fmt.Errorf("cached test schema migration versions are count=%d range=%d..%d, want count=%d range=1..%d", count, minimum, maximum, want, want)
	}

	// Cardinality is not identity: schema_migrations records version NUMBERS, so a
	// template built from a DIFFERENT set of the same size passes every check
	// above, and the only other binding is a 48-bit filename prefix. Shared with
	// internal/db/dbtest because both caches use the SAME template path.
	return ValidateTestTemplateIdentity(path)
}

func TestEnsureCachedMigratedTestTemplateReplacesInvalidRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write invalid template: %v", err)
	}
	if err := ensureCachedMigratedTestTemplate(path); err != nil {
		t.Fatalf("repair invalid template: %v", err)
	}
	if err := validateCachedMigratedTestTemplate(path); err != nil {
		t.Fatalf("validate repaired template: %v", err)
	}
}

func TestEnsureCachedMigratedTestTemplateRetriesLostPublicationRace(t *testing.T) {
	template := filepath.Join(t.TempDir(), "schema.db")
	foreign := filepath.Join(t.TempDir(), "foreign.db")
	if err := ensureCachedMigratedTestTemplate(foreign); err != nil {
		t.Fatalf("build foreign template: %v", err)
	}
	raw, err := sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatalf("open foreign: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS seen_comments`); err != nil {
		_ = raw.Close()
		t.Fatalf("age foreign: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close foreign: %v", err)
	}

	previous := AfterTestTemplatePublish
	hookCalls := 0
	AfterTestTemplatePublish = func() error {
		hookCalls++
		if hookCalls != 1 {
			return nil
		}
		data, err := os.ReadFile(foreign)
		if err != nil {
			return err
		}
		return os.WriteFile(template, data, 0o600)
	}
	t.Cleanup(func() { AfterTestTemplatePublish = previous })

	if err := ensureCachedMigratedTestTemplate(template); err != nil {
		t.Fatalf("repair one lost publication race: %v", err)
	}
	AfterTestTemplatePublish = previous
	if hookCalls != 2 {
		t.Fatalf("publication hook calls = %d, want 2 (lost race plus in-call rebuild)", hookCalls)
	}
	if err := validateCachedMigratedTestTemplate(template); err != nil {
		t.Fatalf("validate template rebuilt after lost race: %v", err)
	}
}

func copyCachedTestTemplateIfMissing(template, path string) error {
	lockValue, _ := cachedTestCopyLocks.LoadOrStore(filepath.Clean(path), &sync.Mutex{})
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
