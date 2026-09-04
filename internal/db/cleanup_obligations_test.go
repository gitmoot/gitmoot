package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestCleanupObligationsMigrationFreshAndUpgrade(t *testing.T) {
	ctx := context.Background()
	fresh, err := openRealTestStore(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	if exists, err := fresh.HasTable(ctx, "cleanup_obligations"); err != nil || !exists {
		t.Fatalf("fresh cleanup_obligations exists=%v err=%v", exists, err)
	}

	path := filepath.Join(t.TempDir(), "upgrade.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	preMigration := &Store{db: raw}
	// Unique to the original migration: the rebuild that adds unpublished_commits
	// repeats every other line of this CREATE TABLE.
	for version, migration := range migrationsBefore(t, "'identity_or_containment', 'unknown'") {
		if err := preMigration.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	if exists, err := preMigration.HasTable(ctx, "cleanup_obligations"); err != nil || exists {
		t.Fatalf("pre-migration cleanup_obligations exists=%v err=%v", exists, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close pre-migration store: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	if exists, err := upgraded.HasTable(ctx, "cleanup_obligations"); err != nil || !exists {
		t.Fatalf("upgraded cleanup_obligations exists=%v err=%v", exists, err)
	}
}

// The rebuild that widened the reason CHECK must carry existing rows across.
// Applying migrations only up to the ORIGINAL create leaves the table empty, so
// this test seeds it at the pre-rebuild version and asserts the data survives.
func TestCleanupObligationsRebuildPreservesLegacyRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rebuild.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	preRebuild := &Store{db: raw}
	// Unique to the rebuild: only that migration lists unpublished_commits.
	for version, migration := range migrationsBefore(t, "'identity_or_containment', 'unpublished_commits', 'unknown'") {
		if err := preRebuild.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	if exists, err := preRebuild.HasTable(ctx, "cleanup_obligations"); err != nil || !exists {
		t.Fatalf("pre-rebuild cleanup_obligations exists=%v err=%v", exists, err)
	}
	quarantinedID := CleanupObligationResourceID("job-q", "/tmp/managed/q")
	retryableID := CleanupObligationResourceID("job-r", "/tmp/managed/r")
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO cleanup_obligations (
			resource_id, resource_kind, owner_job_id, expected_path, state,
			reason, attempt_count, next_attempt_at, last_error, created_at, updated_at
		) VALUES
			(?, 'delegation_worktree', 'job-q', '/tmp/managed/q', 'quarantined',
				'identity_or_containment', 3, '2026-01-01T00:00:00Z', 'boom', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
			(?, 'delegation_worktree', 'job-r', '/tmp/managed/r', 'retryable',
				'checkout_lock', 1, '2026-01-02T00:00:00Z', '', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z')`,
		quarantinedID, retryableID); err != nil {
		t.Fatalf("seed pre-rebuild rows: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close pre-rebuild store: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	for _, want := range []CleanupObligation{
		{ResourceID: quarantinedID, OwnerJobID: "job-q", ExpectedPath: "/tmp/managed/q", State: "quarantined", Reason: CleanupReasonIdentityOrContainment, AttemptCount: 3, LastError: "boom"},
		{ResourceID: retryableID, OwnerJobID: "job-r", ExpectedPath: "/tmp/managed/r", State: "retryable", Reason: CleanupReasonCheckoutLock, AttemptCount: 1},
	} {
		got, err := upgraded.GetCleanupObligation(ctx, want.ResourceID)
		if err != nil {
			t.Fatalf("GetCleanupObligation(%s): %v", want.ResourceID, err)
		}
		if got.OwnerJobID != want.OwnerJobID || got.ExpectedPath != want.ExpectedPath ||
			got.State != want.State || got.Reason != want.Reason ||
			got.AttemptCount != want.AttemptCount || got.LastError != want.LastError {
			t.Fatalf("row %s = %+v, want %+v", want.ResourceID, got, want)
		}
	}
	// The widened CHECK and both indexes must exist on the rebuilt table.
	if _, err := upgraded.DeferCleanupObligation(ctx, "job-r", "/tmp/managed/r", CleanupReasonUnpublishedCommits, time.Now().UTC(), time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatalf("DeferCleanupObligation with the widened reason: %v", err)
	}
	for _, index := range []string{"idx_cleanup_obligations_owner_path", "idx_cleanup_obligations_due"} {
		var name string
		if err := upgraded.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&name); err != nil {
			t.Fatalf("index %s missing after rebuild: %v", index, err)
		}
	}
}

// A positional migration list must be a strict prefix-extension of the released
// one: a database already at the previous version has to upgrade by applying only
// the NEW tail. A fresh-init test cannot fail on a mis-ordered tail, which is
// exactly how a merge bricked every existing database here while every suite
// stayed green.
//
// The released prefix is identified by REMOVING this branch's own migration, not
// by slicing off the last element. Slicing would make a reordered list produce a
// synthetic "released" database that already contains the new migration, so the
// test would pass on precisely the mutant it exists to kill.
func TestMigrationsUpgradeFromPreviousReleasedVersion(t *testing.T) {
	ctx := context.Background()
	// The marker names THIS BRANCH's migration, and it has moved six times as main
	// advanced: the cleanup_obligations rebuild, #1766's SkillOpt/evals teardown,
	// #1770's Activepieces trigger removal, #1731's escalation_rounds table,
	// #1754's chat/moot teardown, #1756's preset-delivery removal and #1753's
	// cockpit/interactive table drop each joined the released prefix, leaving
	// #1822's findings ledger appended last. Two branches cannot both be "last",
	// and the ordering that matters is the one a deployed database sees, which is
	// why this marker MUST be repointed on every branch that appends a migration,
	// and why the failure reads as "not appended last" rather than as a merge
	// conflict.
	// The marker must name THIS BRANCH'S LAST migration and must be UNIQUE. The
	// #1850 round 2 F3 fix appends a rebuild of review_finding_observations, so
	// the bare CREATE string now matches TWO migrations (the original additive one
	// and the rebuild) and this test correctly refused it as ambiguous. The
	// rebuild's own table name is unique to it.
	const branchMigrationMarker = "review_finding_observations_1850 RENAME TO"
	branchIndex := -1
	for index, migration := range migrations {
		if strings.Contains(migration, branchMigrationMarker) {
			if branchIndex >= 0 {
				t.Fatalf("migration marker %q matches indexes %d and %d", branchMigrationMarker, branchIndex, index)
			}
			branchIndex = index
		}
	}
	if branchIndex < 0 {
		t.Fatalf("migration marker %q matches no migration", branchMigrationMarker)
	}
	// The new migration must be LAST, or a database at the released version would
	// apply somebody else's already-applied migration and fail to open.
	if branchIndex != len(migrations)-1 {
		t.Fatalf("branch migration is at index %d of %d; a new migration must be appended last", branchIndex, len(migrations)-1)
	}
	released := make([]string, 0, len(migrations)-1)
	released = append(released, migrations[:branchIndex]...)
	released = append(released, migrations[branchIndex+1:]...)

	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range released {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	var applied int
	if err := raw.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("read applied version: %v", err)
	}
	if applied != len(released) {
		t.Fatalf("applied version = %d, want %d", applied, len(released))
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous-release store: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade a database at the previous released version: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	var final int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&final); err != nil {
		t.Fatalf("read upgraded version: %v", err)
	}
	if final != len(migrations) {
		t.Fatalf("upgraded version = %d, want %d", final, len(migrations))
	}
	var retiredTableCount int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('chat_meta','chat_threads','chat_messages','chat_mentions','chat_thread_meta')`,
	).Scan(&retiredTableCount); err != nil {
		t.Fatalf("inspect upgraded chat tables: %v", err)
	}
	if retiredTableCount != 0 {
		t.Fatalf("retired chat table count = %d, want 0", retiredTableCount)
	}
	now := time.Now().UTC()
	if _, err := upgraded.DeferCleanupObligation(ctx, "job-upgraded", "/tmp/managed/upgraded", CleanupReasonUnpublishedCommits, now, now.Add(time.Minute)); err != nil {
		t.Fatalf("widened reason after upgrade: %v", err)
	}
}

func TestCleanupObligationAcceptsUnpublishedCommitsReason(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	obligation, err := store.DeferCleanupObligation(ctx, "job-unpublished", "/tmp/managed/fix-clone", CleanupReasonUnpublishedCommits, now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("DeferCleanupObligation: %v", err)
	}
	if obligation.Reason != CleanupReasonUnpublishedCommits {
		t.Fatalf("reason = %q, want %q", obligation.Reason, CleanupReasonUnpublishedCommits)
	}
	if obligation.State != "retryable" {
		t.Fatalf("state = %q, want retryable", obligation.State)
	}
}

func TestDeferCleanupObligationKeepsSpecificReasonOverTerminalDeferral(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	path := "/tmp/managed/fix-clone"
	if _, err := store.DeferCleanupObligation(ctx, "job-1", path, CleanupReasonUnpublishedCommits, now, now.Add(time.Minute)); err != nil {
		t.Fatalf("specific deferral: %v", err)
	}
	// The daemon stamps the generic deferral after every unfinished pass.
	obligation, err := store.DeferCleanupObligation(ctx, "job-1", path, CleanupReasonTerminalDeferred, now.Add(time.Minute), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("generic deferral: %v", err)
	}
	if obligation.Reason != CleanupReasonUnpublishedCommits {
		t.Fatalf("reason = %q, want the specific diagnosis to survive", obligation.Reason)
	}
	if obligation.NextAttemptAt == "" || obligation.State != "retryable" {
		t.Fatalf("obligation = %+v, want a rescheduled retryable row", obligation)
	}
	// A different specific reason must still win.
	obligation, err = store.DeferCleanupObligation(ctx, "job-1", path, CleanupReasonCheckoutLock, now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("second specific deferral: %v", err)
	}
	if obligation.Reason != CleanupReasonCheckoutLock {
		t.Fatalf("reason = %q, want checkout_lock", obligation.Reason)
	}
}

func TestCleanupObligationTerminalStatesRequireExplicitReopen(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	path := "/tmp/managed/delegation"
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := store.RecordCleanupObligationFailure(ctx, "job-1", path, CleanupReasonUnknown, errors.New("no typed match"), now, now.Add(time.Minute), 3); err != nil {
			t.Fatal(err)
		}
	}
	resourceID := CleanupObligationResourceID("job-1", path)
	obligation, err := store.EnsureCleanupObligation(ctx, "job-1", path, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if obligation.State != CleanupObligationQuarantined || obligation.AttemptCount != 3 {
		t.Fatalf("ensure reopened terminal obligation: %+v", obligation)
	}
	reopened, err := store.ReopenCleanupObligation(ctx, resourceID, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != CleanupObligationPending || reopened.AttemptCount != 0 || reopened.Reason != "operator_reopened" {
		t.Fatalf("reopened obligation = %+v", reopened)
	}
	removed, err := store.MarkCleanupObligationRemoved(ctx, "job-1", path, now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed.State != CleanupObligationRemoved {
		t.Fatalf("removed obligation = %+v", removed)
	}
	if _, err := store.RecordCleanupObligationFailure(ctx, "job-1", path, CleanupReasonUnknown, errors.New("stale failure"), now, now.Add(time.Minute), 3); err != nil {
		t.Fatal(err)
	}
	final, err := store.GetCleanupObligation(ctx, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != CleanupObligationRemoved {
		t.Fatalf("stale failure reopened removed obligation: %+v", final)
	}
}
