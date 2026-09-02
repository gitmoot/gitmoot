package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// openRealTestStore bypasses the migrated-schema cache. Use it only when a test
// observes Open, migration or backfill application, or fresh SQLite setup.
// Ordinary store tests must use openCachedTestStore.
func openRealTestStore(t *testing.T, path string) (*Store, error) {
	t.Helper()
	return Open(path)
}

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

func TestStoreOpenPolicy(t *testing.T) {
	realPathTests := map[string]bool{
		"TestAdvanceRetryCollapseMigration":                                      true,
		"TestBackfillGhostSessionJobsHonorsDisabledAgePolicy":                    true,
		"TestBackfillGhostSessionJobsReusesReaperAndIsIdempotent":                true,
		"TestCleanupObligationsMigrationFreshAndUpgrade":                         true,
		"TestCleanupObligationsRebuildPreservesLegacyRows":                       true,
		"TestMigrationsUpgradeFromPreviousReleasedVersion":                       true,
		"TestSkillOptRemovalMigrationReconcilesCandidateAndCanaryRows":           true,
		"TestExecBackendAttemptsMigrationFreshAndCached":                         true,
		"TestExternallyDrivenColumnMigratesOnPreExistingDB":                      true,
		"TestIncrementalVacuumReclaimsOnlyRequestedPages":                        true,
		"TestJobModelMigrationOnPreExistingDB":                                   true,
		"TestJobRepoBackfillMigrationUpdatesOnlyStaleRows":                       true,
		"TestJobTokenMigrationOnPreExistingDB":                                   true,
		"TestKeychainMigrationAppliesToExistingDatabase":                         true,
		"TestMemoryEventBackfillLiveShapeIsIdempotent":                           true,
		"TestMemoryEventBackfillMixedLiveHistory":                                true,
		"TestMemoryEventsMigrationFreshAndUpgradeConverge":                       true,
		"TestMemoryHarvestMigrationFreshAndUpgrade":                              true,
		"TestMemoryMigrationCreatesTables":                                       true,
		"TestMigrateAddsExecBackendAttempts":                                     true,
		"TestMigrateAddsPipelinesToUpgradedDB":                                   true,
		"TestMigrateAddsTriggerBindingToExistingPipeline":                        true,
		"TestMigrateAppendsAgentInstanceAutonomyPolicy":                          true,
		"TestMigrateAppendsRootKilled":                                           true,
		"TestMigrateAppendsTaskWorktreePath":                                     true,
		"TestMigrateBackfillsRootID":                                             true,
		"TestMigrationAddsRunnerAndOwnerBootColumns":                             true,
		"TestMigrationCopiesPresetsToAgentTemplates":                             true,
		"TestMigrationDeduplicatesExistingTaskBranches":                          true,
		"TestOpenConfiguresSQLiteContentionPragmas":                              true,
		"TestOpenConfiguresSynchronousNormal":                                    true,
		"TestOpenDoesNotFullVacuumLegacyDatabase":                                true,
		"TestOpenMigratesSchema":                                                 true,
		"TestOpenPreservesExistingFullAutoVacuumMode":                            true,
		"TestOrgRolePresenceMigrationAndUpsert":                                  true,
		"TestTaskEventsMigrationAppliesToExistingDatabase":                       true,
		"TestWakeOutboxSupersededMigrationPreservesRowsAndScopesTerminalMarkers": true,
		"TestWorkflowMetaTextMigrations":                                         true,
	}
	realPathTests["TestMigrateExecBackendAttemptsLifecycleGenerationNotNullPreservesRows"] = true
	// #1673: proves the round-open pair is ONE transaction by breaking the event
	// insert from a SECOND connection to the same file, which a cached shared-memory
	// store cannot express.
	realPathTests["TestUpsertTaskWithJobEventUnlessStatesRollsBackTheTaskWhenTheEventFails"] = true
	directOpenFunctions := map[string]bool{
		"ensureCachedMigratedTestTemplateOnce": true,
		"openRealTestStore":                    true,
	}
	seenRealPath := make(map[string]bool, len(realPathTests))
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch callee.Name {
				case "Open":
					if !directOpenFunctions[function.Name.Name] {
						t.Errorf("%s: direct Open in %s; ordinary tests use openCachedTestStore and declared carve-outs use openRealTestStore", fset.Position(call.Pos()), function.Name.Name)
					}
				case "openRealTestStore":
					if !realPathTests[function.Name.Name] {
						t.Errorf("%s: undeclared real-path store in %s", fset.Position(call.Pos()), function.Name.Name)
					} else {
						seenRealPath[function.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	for name := range realPathTests {
		if !seenRealPath[name] {
			t.Errorf("declared real-path test %s does not call openRealTestStore", name)
		}
	}
}

// afterEnsureCachedTemplate is the seam described at its call site: a no-op in
// every ordinary run, replaced by a test to occupy the window between validating
// the published template and reading it.
var afterEnsureCachedTemplate = func() error { return nil }

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
		// Seam between validating the file and reading it. The retry loop above
		// exists because another test binary can replace the template in exactly
		// this window; production behaviour is a no-op, and a test replaces the
		// hook to drive that window deterministically. Zero production surface:
		// this whole frontend lives in a _test.go file.
		if err := afterEnsureCachedTemplate(); err != nil {
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

// TestTestTemplateStampIsBoundToMigrationFingerprint pins the stamp to the
// CURRENT migration set. Every consumer of testTemplateStamp compares one stamp
// against another stamp (test_template_identity.go:113, :137, :207, :269), so a
// mutant that detaches the stamp from SchemaMigrationFingerprint() -- returning
// only the file digest -- changes both sides of every comparison identically and
// passes the entire cache-contract set. Found by g7-review one level downstream
// of the exported-wrapper gap closed at 7bd3fe46, which is the same defect class
// a third time: a guard that holds where it is installed and is absent at the
// next consumer.
//
// Why the detachment is exploitable rather than cosmetic: the migration
// fingerprint reaches the cache PATH only as a 48-bit prefix
// (gitmoot-test-schema-<uid>-<fingerprint[:12]>.db), so the path alone cannot
// distinguish two different migration sets that truncate alike, nor a leftover
// file at a reused path. The stamp carrying the FULL fingerprint is what closes
// that, and this assertion is what keeps the stamp carrying it.
func TestTestTemplateStampIsBoundToMigrationFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stamped.db")
	if err := os.WriteFile(path, []byte("not a database, only bytes to digest"), 0o600); err != nil {
		t.Fatalf("write stamp subject: %v", err)
	}
	stamp, err := testTemplateStamp(path)
	if err != nil {
		t.Fatalf("testTemplateStamp: %v", err)
	}
	fingerprint, _, found := strings.Cut(stamp, "\n")
	if !found {
		t.Fatalf("stamp %q has no newline: it must be the migration fingerprint, a newline, then the file digest", stamp)
	}
	if want := SchemaMigrationFingerprint(); fingerprint != want {
		t.Fatalf("stamp fingerprint line = %q, want %q (stamp is not bound to the current migration set)", fingerprint, want)
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

// TestCachedFrontendIsBoundToAuthenticatedSnapshot pins the in-package cache
// FRONTEND to the authenticated snapshot operation, which is a different claim
// from the one the test above makes. That test proves
// SnapshotMigratedTestTemplate rejects a replaced file; this one proves
// cachedMigratedTestTemplate actually goes through it. Nothing else did: swapping
// the frontend's SnapshotMigratedTestTemplate call for a plain os.ReadFile
// compiles (identical ([]byte, error) shape) and passes the whole cache-contract
// suite, because every other assertion either exercises the operation directly or
// only checks that the bytes it returned round-trip. Found by g7-review as the
// FOURTH hop of one class on this PR: pure helper -> exported wrapper -> stamp
// consumer -> cache frontend, each correct where installed and unbound at the
// next call site.
//
// The exploitable window is time-of-check-to-time-of-use: validation happens
// against the file on disk, and a plain read would hand back whatever bytes are
// there when the read lands, so a database replaced after validation gets cached
// and every test in the process then runs against it.
func TestCachedFrontendIsBoundToAuthenticatedSnapshot(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("TMPDIR", cacheRoot)
	reset := func() {
		cachedTestTemplateMu.Lock()
		cachedTestTemplateReady = false
		cachedTestTemplatePath = ""
		cachedTestTemplateSnapshot = nil
		cachedTestTemplateMu.Unlock()
	}
	reset()
	t.Cleanup(reset)

	template, _, err := cachedMigratedTestTemplate()
	if err != nil {
		t.Fatalf("build cached migrated template snapshot: %v", err)
	}
	reset()

	// Occupy the window: ensure() has just validated the file, so replace it
	// before the snapshot read lands. Fire once so the retry loop can recover.
	var foreign []byte
	fired := false
	afterEnsureCachedTemplate = func() error {
		if fired {
			return nil
		}
		fired = true
		replaceCachedTemplateWithForeignSQLite(t, template)
		data, readErr := os.ReadFile(template)
		if readErr != nil {
			return readErr
		}
		foreign = data
		return nil
	}
	t.Cleanup(func() { afterEnsureCachedTemplate = func() error { return nil } })

	_, snapshot, err := cachedMigratedTestTemplate()
	if !fired {
		t.Fatal("seam never fired: the frontend did not re-read the template")
	}
	if err != nil {
		// Refusing outright is a correct authenticated outcome.
		return
	}
	if bytes.Equal(snapshot, foreign) {
		t.Fatal("frontend served the database that replaced the validated one: it read the file directly instead of through the authenticated snapshot")
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
	if err := ValidateTestTemplateIdentity(template); err == nil {
		t.Fatal("identity validation accepted a template containing an application row")
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

// TestCachedOpenDoesNotMigrate binds the in-package cached frontend to
// OpenAlreadyMigrated. Swapping it for Open leaves every assertion in the suite
// green -- a cached copy already carries all 116 migrations and applyMigration is
// idempotent, so Migrate is a no-op that changes nothing observable (measured:
// PRAGMA data_version unmoved). What it costs is 115 wasted transactions per store
// open, which is the entire point of #1550, so the contract needs a non-timing
// guard. Requested by g7-review; MigrateObserver is the seam.
func TestCachedOpenDoesNotMigrate(t *testing.T) {
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

	// Warm the template first: building it legitimately migrates once.
	if _, _, err := cachedMigratedTestTemplate(); err != nil {
		t.Fatalf("build cached migrated template: %v", err)
	}

	migrations := 0
	MigrateObserver = func() { migrations++ }
	t.Cleanup(func() { MigrateObserver = func() {} })

	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "copy.db"))
	if err != nil {
		t.Fatalf("open cached test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if migrations != 0 {
		t.Fatalf("Migrate ran %d time(s) on a cached copy: the frontend is not bound to OpenAlreadyMigrated", migrations)
	}
}
