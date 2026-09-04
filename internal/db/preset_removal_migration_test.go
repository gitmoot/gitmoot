package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// presetRemovalMarker identifies the #1756 migration inside the append-only
// slice. Located by CONTENT, not by index, so appending a later migration never
// silently re-points these tests at somebody else's entry.
const presetRemovalMarker = "ALTER TABLE agents DROP COLUMN preset_delivery"

// releasedMigrationsBeforePresetRemoval returns the strict migration prefix that
// shipped before #1756: everything ahead of #1756's own entry, located by marker
// rather than by slicing off the tail, so a reordered slice cannot produce a
// synthetic "released" database that already contains the migration under test.
//
// This used to also assert #1756's migration was the LAST element. That held only
// while #1756 was the newest branch; #1753 then appended the cockpit/interactive
// table drop after it, and two branches cannot both be last. The invariant that
// actually protects a deployed database — a new migration is APPENDED, never
// inserted, so no already-applied entry shifts version number — is enforced once,
// for whichever migration is currently newest, by
// TestMigrationsUpgradeFromPreviousReleasedVersion's branchMigrationMarker in
// cleanup_obligations_test.go. Asserting it here as well only ever fires on the
// NEXT branch to append, which is a merge conflict wearing a test's clothes.
func releasedMigrationsBeforePresetRemoval(t *testing.T) []string {
	t.Helper()
	index := -1
	for i, migration := range migrations {
		if strings.Contains(migration, presetRemovalMarker) {
			if index >= 0 {
				t.Fatalf("marker %q matches migrations %d and %d", presetRemovalMarker, index, i)
			}
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("marker %q matches no migration", presetRemovalMarker)
	}
	return append([]string(nil), migrations[:index]...)
}

// TestPresetDeliveryRemovalMigrationOnFreshHome is the FRESH arm the #1756
// migration comment claims. A brand-new database runs the whole slice — the
// additive #33 migration that creates the column and table, then this removal —
// so it must end with neither present.
//
// A fresh home is the arm most likely to pass for the wrong reason: if the removal
// silently no-opped, a fresh schema would still look right to any assertion that
// only checks the tables it EXPECTS to exist. So this asserts ABSENCE directly,
// against sqlite's own catalog, and separately for the table and the column.
func TestPresetDeliveryRemovalMigrationOnFreshHome(t *testing.T) {
	ctx := context.Background()
	store, err := openRealTestStore(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open a fresh home: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var tables int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'preset_session_state'`).Scan(&tables); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if tables != 0 {
		t.Fatalf("preset_session_state table count = %d, want 0 on a fresh home", tables)
	}

	if columnExists(t, store.db, "agents", "preset_delivery") {
		t.Fatal("agents.preset_delivery still exists on a fresh home")
	}
}

// TestPresetDeliveryRemovalMigrationOnPreChangeHome is the arm that actually
// carries risk: a database built at the PREVIOUS released version, holding real
// agent rows and a preset_session_state row, upgraded through the real Open path.
//
// The measurement that justified this deletion was that the live deployment had 0
// preset_session_state rows and every agent on 'full'. A migration proven only
// against that state would be untested against the state it is nominally there to
// handle, so this fixture seeds the NON-empty case on purpose: a populated table
// and an agent whose column value is non-default. Both must survive the upgrade as
// a working database with its agent rows intact.
func TestPresetDeliveryRemovalMigrationOnPreChangeHome(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range releasedMigrationsBeforePresetRemoval(t) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}

	// The pre-change schema must really have the things this migration drops,
	// otherwise the upgrade below would pass without ever exercising a drop.
	if !columnExists(t, raw, "agents", "preset_delivery") {
		t.Fatal("fixture is not a pre-#1756 database: agents.preset_delivery is already absent")
	}

	// Two agents, one of them on a NON-default mode, plus a delivered-state row:
	// the exact rows a home that opted into the removed modes would hold.
	for _, seed := range []struct {
		name string
		mode string
	}{
		{name: "reviewer-seat", mode: "full"},
		{name: "legacy-seat", mode: "referenced"},
	} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO agents(
				name, role, runtime, runtime_ref, repo_scope, template_id, model, effort,
				capabilities_json, autonomy_policy, health_status, preset_delivery, updated_at)
			VALUES (?, 'reviewer', 'codex', 'fresh:seed', 'gitmoot/gitmoot', 'thermo', '', '', '["review"]', 'auto', 'unknown', ?, CURRENT_TIMESTAMP)`,
			seed.name, seed.mode); err != nil {
			t.Fatalf("seed agent %s: %v", seed.name, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO preset_session_state(runtime, session_id, preset_id, preset_commit)
		VALUES ('codex', 'session-1', 'thermo', 'abc123')`); err != nil {
		t.Fatalf("seed preset_session_state row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous-release store: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade a database carrying preset rows: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	var tables int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'preset_session_state'`).Scan(&tables); err != nil {
		t.Fatalf("query sqlite_master after upgrade: %v", err)
	}
	if tables != 0 {
		t.Fatalf("preset_session_state table count = %d after upgrade, want 0", tables)
	}
	if columnExists(t, upgraded.db, "agents", "preset_delivery") {
		t.Fatal("agents.preset_delivery survived the upgrade")
	}

	// The agents themselves must be intact and readable through the real store:
	// a column drop that rebuilt or truncated the table would still satisfy the
	// absence assertions above.
	for _, name := range []string{"reviewer-seat", "legacy-seat"} {
		agent, err := upgraded.GetAgent(ctx, name)
		if err != nil {
			t.Fatalf("GetAgent(%s) after upgrade: %v", name, err)
		}
		if agent.TemplateID != "thermo" || agent.Runtime != "codex" || agent.Role != "reviewer" {
			t.Fatalf("agent %s lost fields across the upgrade: %+v", name, agent)
		}
	}
	agents, err := upgraded.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents after upgrade: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("ListAgents returned %d agents after upgrade, want 2", len(agents))
	}
}

// columnExists answers from sqlite's own table_info rather than from a SELECT that
// would fail for many reasons besides a missing column.
func columnExists(t *testing.T, database *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := database.QueryContext(context.Background(), `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan pragma_table_info(%s): %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pragma_table_info(%s): %v", table, err)
	}
	return false
}
