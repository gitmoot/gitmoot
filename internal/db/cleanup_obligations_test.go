package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
