package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
		return nil
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
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
			return nil
		}
		return fmt.Errorf("publish test schema template: %w", err)
	}
	return nil
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
