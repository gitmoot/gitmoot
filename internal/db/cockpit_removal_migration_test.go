package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// cockpitRemovalMarker identifies #1753's migration inside the append-only
// slice by CONTENT, so appending a later migration never re-points this test at
// somebody else's entry - the same convention the preset-removal tests use.
const cockpitRemovalMarker = "DROP TABLE IF EXISTS cockpit_panes"

var cockpitRemovedTables = []string{"cockpit_panes", "cockpit_workspaces", "interactive_prompts"}

// releasedMigrationsBeforeCockpitRemoval returns the strict prefix that shipped
// before this migration, located by marker rather than by slicing off the tail:
// a reordered slice must not produce a synthetic "released" database that
// already contains the migration under test.
func releasedMigrationsBeforeCockpitRemoval(t *testing.T) []string {
	t.Helper()
	index := -1
	for i, migration := range migrations {
		if strings.Contains(migration, cockpitRemovalMarker) {
			if index >= 0 {
				t.Fatalf("marker %q matches migrations %d and %d", cockpitRemovalMarker, index, i)
			}
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("marker %q matches no migration", cockpitRemovalMarker)
	}
	return append([]string(nil), migrations[:index]...)
}

// The FRESH arm: a brand-new database runs the whole slice, creating these
// tables and then dropping them, so it must end with none of them present.
func TestCockpitRemovalMigrationOnFreshHome(t *testing.T) {
	ctx := context.Background()
	store, err := openRealTestStore(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open a fresh home: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, table := range cockpitRemovedTables {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("query sqlite_master for %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("table %q survived the removal migration on a fresh home", table)
		}
	}
}

// The UPGRADE arm, which is the one that matters: build a database at the last
// released schema - where these tables DO exist and hold rows - then run the
// full slice over it and require the tables gone. What catches a no-op or a
// partial drop is the sqlite_master sweep below, which checks all three tables
// regardless of row count; seeding a row proves only that the drop survives
// DATA (#1787 review F4, correcting an over-claim this comment used to make).
func TestCockpitRemovalMigrationDropsPopulatedTablesOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open the previous-release database: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range releasedMigrationsBeforeCockpitRemoval(t) {
		// applyMigration RECORDS the version, so the upgrade below resumes
		// instead of replaying migration 1 against an existing schema.
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			_ = raw.Close()
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	// Prove the starting state rather than assuming it: if these tables are not
	// here now, the test below would pass for the wrong reason.
	for _, table := range cockpitRemovedTables {
		var count int
		if err := raw.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			_ = raw.Close()
			t.Fatalf("query sqlite_master for %s: %v", table, err)
		}
		if count != 1 {
			_ = raw.Close()
			t.Fatalf("table %q is absent at the previous release, so this test would prove nothing", table)
		}
	}
	// Seed a real row so the drop is exercised against a NON-EMPTY table rather
	// than an empty one. It is only interactive_prompts, and the sqlite_master
	// assertions below already cover all three tables regardless of row count -
	// so this proves the drop survives data, not that a partial drop is caught
	// (#1787 review F4, correcting an over-claim in the previous comment).
	if _, err := raw.ExecContext(ctx, `INSERT INTO interactive_prompts (id, question, state)
		VALUES ('p1', 'which branch?', 'pending')`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed interactive_prompts: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close the previous-release database: %v", err)
	}

	store, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade the previous-release database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, table := range cockpitRemovedTables {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("query sqlite_master for %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("table %q survived the upgrade from the previous release", table)
		}
	}
}
