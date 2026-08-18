// Package dbtest provides isolated writable stores backed by a cached,
// fully-migrated schema template.
package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

var (
	templateMu       sync.Mutex
	templateReady    bool
	templatePath     string
	templateSnapshot []byte
	copyLocks        sync.Map
)

const templatePublicationAttempts = 3

// Open copies the current migrated schema into path when path does not exist,
// then opens it without replaying migrations. Existing databases are never
// replaced, which preserves data when a test reopens the same path.
func Open(t *testing.T, path string) (*db.Store, error) {
	t.Helper()

	_, snapshot, err := migratedTemplate()
	if err != nil {
		return nil, err
	}
	if err := copyTemplateSnapshotIfMissing(snapshot, path); err != nil {
		return nil, err
	}
	return db.OpenAlreadyMigrated(path)
}

var afterEnsureTemplate = func() error { return nil }

func migratedTemplate() (string, []byte, error) {
	templateMu.Lock()
	defer templateMu.Unlock()
	if templateReady {
		return templatePath, templateSnapshot, nil
	}
	templatePath = db.MigratedTestTemplatePath(os.TempDir())
	var snapshotErr error
	for attempt := 1; attempt <= templatePublicationAttempts; attempt++ {
		if err := ensureMigratedTemplate(templatePath); err != nil {
			return templatePath, nil, err
		}
		// Seam between validating the published template and reading it. The retry
		// loop exists because a concurrent test binary can replace the file in
		// exactly this window; production behaviour is a no-op and a test replaces
		// the hook to occupy the window deterministically. Unexported, so this adds
		// no surface outside the package. Mirrors afterEnsureCachedTemplate in
		// internal/db's own frontend and db.AfterTestTemplatePublish.
		if err := afterEnsureTemplate(); err != nil {
			return templatePath, nil, err
		}
		templateSnapshot, snapshotErr = db.SnapshotMigratedTestTemplate(templatePath)
		if snapshotErr == nil {
			templateReady = true
			return templatePath, templateSnapshot, nil
		}
	}
	return templatePath, nil, fmt.Errorf("snapshot migrated test template after %d attempts: %w", templatePublicationAttempts, snapshotErr)
}

func ensureMigratedTemplate(path string) error {
	for attempt := 1; attempt <= templatePublicationAttempts; attempt++ {
		retry, err := ensureMigratedTemplateOnce(path)
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
	}
	return fmt.Errorf("test schema template at %s was repeatedly replaced during publication", path)
}

func ensureMigratedTemplateOnce(path string) (bool, error) {
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("test schema template %s is not a regular file", path)
		}
		if err := validateMigratedTemplate(path); err == nil {
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

	store, err := db.Open(tempPath)
	if err != nil {
		return false, fmt.Errorf("migrate test schema template: %w", err)
	}
	if err := checkpointWAL(tempPath); err != nil {
		_ = store.Close()
		return false, err
	}
	if err := store.Close(); err != nil {
		return false, fmt.Errorf("close migrated test schema template: %w", err)
	}

	// Publish and stamp as ONE operation: package db owns the order, and requiring
	// the freshly built temp file means the stamped database is by construction the
	// one that was migrated. An exported bare stamp would launder provenance.
	published, err := db.PublishMigratedTestTemplate(tempPath, path)
	return !published, err
}

func validateMigratedTemplate(path string) error {
	return db.ValidateMigratedTestTemplate(path)
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

func copyTemplateSnapshotIfMissing(snapshot []byte, path string) error {
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
	if _, err := destination.Write(snapshot); err != nil {
		return fmt.Errorf("copy test schema template: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close copied test database: %w", err)
	}
	complete = true
	return nil
}
