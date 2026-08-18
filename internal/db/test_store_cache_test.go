package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var (
	cachedTestTemplateMu       sync.Mutex
	cachedTestTemplateReady    bool
	cachedTestTemplatePath     string
	cachedTestTemplateSnapshot []byte
	cachedTestCopyLocks        sync.Map
)

const cachedTestTemplatePublicationAttempts = 3

func openCachedTestStore(t *testing.T, path string) (*Store, error) {
	t.Helper()
	_, snapshot, err := cachedMigratedTestTemplate()
	if err != nil {
		return nil, err
	}
	if err := copyCachedTestTemplateSnapshotIfMissing(snapshot, path); err != nil {
		return nil, err
	}
	return OpenAlreadyMigrated(path)
}

func cachedMigratedTestTemplate() (string, []byte, error) {
	cachedTestTemplateMu.Lock()
	defer cachedTestTemplateMu.Unlock()
	if cachedTestTemplateReady {
		return cachedTestTemplatePath, cachedTestTemplateSnapshot, nil
	}
	cachedTestTemplatePath = MigratedTestTemplatePath(os.TempDir())
	var snapshotErr error
	for attempt := 1; attempt <= cachedTestTemplatePublicationAttempts; attempt++ {
		if err := ensureCachedMigratedTestTemplate(cachedTestTemplatePath); err != nil {
			return cachedTestTemplatePath, nil, err
		}
		cachedTestTemplateSnapshot, snapshotErr = SnapshotMigratedTestTemplate(cachedTestTemplatePath)
		if snapshotErr == nil {
			cachedTestTemplateReady = true
			return cachedTestTemplatePath, cachedTestTemplateSnapshot, nil
		}
	}
	return cachedTestTemplatePath, nil, fmt.Errorf("snapshot migrated test template after %d attempts: %w", cachedTestTemplatePublicationAttempts, snapshotErr)
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
	return ValidateMigratedTestTemplate(path)
}

func TestCachedMigratedTemplateUsesCurrentUnixUserPath(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("TMPDIR", cacheRoot)
	cachedTestTemplateMu.Lock()
	cachedTestTemplateReady = false
	cachedTestTemplatePath = ""
	cachedTestTemplateSnapshot = nil
	cachedTestTemplateMu.Unlock()
	t.Cleanup(func() {
		cachedTestTemplateMu.Lock()
		cachedTestTemplateReady = false
		cachedTestTemplatePath = ""
		cachedTestTemplateSnapshot = nil
		cachedTestTemplateMu.Unlock()
	})

	got, _, err := cachedMigratedTestTemplate()
	if err != nil {
		t.Fatalf("build cached migrated template: %v", err)
	}
	want := filepath.Join(cacheRoot, fmt.Sprintf("gitmoot-test-schema-%d-%s.db", os.Getuid(), SchemaMigrationFingerprint()[:12]))
	if got != want {
		t.Fatalf("cached migrated template path = %q, want current-user path %q", got, want)
	}
}

func replaceCachedTemplateWithForeignSQLite(t *testing.T, path string) {
	t.Helper()
	foreign := filepath.Join(t.TempDir(), "foreign.db")
	raw, err := sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatalf("open foreign SQLite database: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), `CREATE TABLE foreign_marker (id INTEGER PRIMARY KEY)`); err != nil {
		_ = raw.Close()
		t.Fatalf("create foreign SQLite schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close foreign SQLite database: %v", err)
	}
	data, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatalf("read foreign SQLite database: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("replace shared template: %v", err)
	}
}

func TestCachedStoreCopiesAuthenticatedSnapshotAfterSharedTemplateChanges(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("TMPDIR", cacheRoot)
	cachedTestTemplateMu.Lock()
	cachedTestTemplateReady = false
	cachedTestTemplatePath = ""
	cachedTestTemplateSnapshot = nil
	cachedTestTemplateMu.Unlock()
	t.Cleanup(func() {
		cachedTestTemplateMu.Lock()
		cachedTestTemplateReady = false
		cachedTestTemplatePath = ""
		cachedTestTemplateSnapshot = nil
		cachedTestTemplateMu.Unlock()
	})

	template, snapshot, err := cachedMigratedTestTemplate()
	if err != nil {
		t.Fatalf("build cached migrated template snapshot: %v", err)
	}
	replaceCachedTemplateWithForeignSQLite(t, template)
	if _, err := SnapshotMigratedTestTemplate(template); err == nil {
		t.Fatal("replacement unexpectedly authenticated as a template snapshot")
	}

	destination := filepath.Join(t.TempDir(), "copied.db")
	if err := copyCachedTestTemplateSnapshotIfMissing(snapshot, destination); err != nil {
		t.Fatalf("copy authenticated snapshot: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read copied snapshot: %v", err)
	}
	if !bytes.Equal(got, snapshot) {
		t.Fatal("copied database differs from the authenticated snapshot")
	}

	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "opened.db"))
	if err != nil {
		t.Fatalf("openCachedTestStore after shared template replacement: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close opened store: %v", err)
	}
}

func TestOpenCachedTestStoreRebuildsTemplateContainingApplicationRows(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("TMPDIR", cacheRoot)
	cachedTestTemplateMu.Lock()
	cachedTestTemplateReady = false
	cachedTestTemplatePath = ""
	cachedTestTemplateSnapshot = nil
	cachedTestTemplateMu.Unlock()
	t.Cleanup(func() {
		cachedTestTemplateMu.Lock()
		cachedTestTemplateReady = false
		cachedTestTemplatePath = ""
		cachedTestTemplateSnapshot = nil
		cachedTestTemplateMu.Unlock()
	})

	template := MigratedTestTemplatePath(cacheRoot)
	if err := ensureCachedMigratedTestTemplate(template); err != nil {
		t.Fatalf("build cached template: %v", err)
	}
	seed, err := OpenAlreadyMigrated(template)
	if err != nil {
		t.Fatalf("open cached template for seeding: %v", err)
	}
	if err := seed.CreateJob(context.Background(), Job{ID: "contaminant", Agent: "worker", Type: "ask", State: "queued"}); err != nil {
		_ = seed.Close()
		t.Fatalf("seed cached template: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeded cached template: %v", err)
	}
	if err := ValidateTestTemplateIdentity(template); err != nil {
		t.Fatalf("schema identity changed after inserting an application row: %v", err)
	}

	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("openCachedTestStore with contaminated persistent template: %v", err)
	}
	defer store.Close()
	if _, err := store.GetJob(context.Background(), "contaminant"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetJob(contaminant) error = %v, want sql.ErrNoRows", err)
	}
	if err := validateCachedMigratedTestTemplate(template); err != nil {
		t.Fatalf("validate rebuilt cached template: %v", err)
	}
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

func copyCachedTestTemplateSnapshotIfMissing(snapshot []byte, path string) error {
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
