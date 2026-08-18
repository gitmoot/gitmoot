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
	cachedTestTemplateOnce sync.Once
	cachedTestTemplatePath string
	cachedTestTemplateErr  error
	cachedTestCopyLocks    sync.Map
)

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
	cachedTestTemplateOnce.Do(func() {
		fingerprint := SchemaMigrationFingerprint()
		cachedTestTemplatePath = filepath.Join(os.TempDir(), "gitmoot-test-schema-"+fingerprint[:12]+".db")
		cachedTestTemplateErr = ensureCachedMigratedTestTemplate(cachedTestTemplatePath)
	})
	return cachedTestTemplatePath, cachedTestTemplateErr
}

func ensureCachedMigratedTestTemplate(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("test schema template %s is not a regular file", path)
		}
		if err := validateCachedMigratedTestTemplate(path); err == nil {
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
	store, err := Open(tempPath)
	if err != nil {
		return fmt.Errorf("migrate test schema template: %w", err)
	}
	if _, err := store.db.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = store.Close()
		return fmt.Errorf("checkpoint test schema template: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close migrated test schema template: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		if validateErr := validateCachedMigratedTestTemplate(path); validateErr == nil {
			return nil
		}
		return fmt.Errorf("publish test schema template: %w", err)
	}
	return nil
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
	return nil
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
