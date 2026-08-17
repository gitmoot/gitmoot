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

	// The unique file is complete and closed before it becomes visible at the
	// shared cache path. Concurrent test binaries may race here; each candidate
	// is complete, and rename is atomic because both names share a directory.
	if err := os.Rename(tempPath, path); err != nil {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
			return nil
		}
		return fmt.Errorf("publish test schema template: %w", err)
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
