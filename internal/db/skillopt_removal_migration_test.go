package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// skillOptRemovalMarker identifies the #1752 migration inside the append-only
// slice. The migration is located by CONTENT, not by index, so appending a later
// migration never silently re-points these tests at somebody else's entry.
const skillOptRemovalMarker = "ALTER TABLE agent_template_versions DROP COLUMN canary_sample"

// releasedMigrationsBeforeSkillOptRemoval returns the migration prefix as it
// shipped BEFORE #1752 — every migration except the removal itself. It removes
// that one entry rather than slicing off the tail so a reordered slice cannot
// produce a synthetic "released" schema that already contains the removal.
func releasedMigrationsBeforeSkillOptRemoval(t *testing.T) []string {
	t.Helper()
	index := -1
	for i, migration := range migrations {
		if strings.Contains(migration, skillOptRemovalMarker) {
			if index >= 0 {
				t.Fatalf("marker %q matches migrations %d and %d", skillOptRemovalMarker, index, i)
			}
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("marker %q matches no migration", skillOptRemovalMarker)
	}
	// THE PREFIX IS EVERYTHING STRICTLY BEFORE THIS MIGRATION, not "every migration
	// except this one". The original form assumed this migration was LAST, so the
	// remainder was a valid previous-release schema. #1673 appended a later migration
	// (escalation_rounds), and keeping it in the seed made the seeded database already
	// contain a table the full re-apply then tries to create again - "table
	// escalation_rounds already exists".
	//
	// The reordering mutant the original form guarded against is still covered, by
	// TestMigrationsUpgradeFromPreviousReleasedVersion: that test pins which migration
	// is appended LAST, which is the property a slice-based prefix cannot check on its
	// own (#1673).
	return migrations[:index]
}

// TestSkillOptRemovalMigrationReconcilesCandidateAndCanaryRows is the data test
// for the #1752 migration. TestOpenMigratesSchema proves the tables, columns, and
// index are GONE; this proves the rows were RECONCILED FIRST, which is the half a
// schema assertion cannot see.
//
// It builds a database at the previous released version (loop tables present,
// canary columns present), seeds the hazard case a live home can actually be in —
// a `pending` candidate, an active `canary`, and an agent_templates.latest_version_id
// pointing at that canary — and then upgrades through the real Open path.
//
// Both reconciliation UPDATEs are load-bearing and each is asserted separately:
//
//   - latest_version_id must be repointed at the live `current` version. Left
//     alone it would keep resolving a version that the same migration is about to
//     make terminal, so `@latest` would answer with a rejected row.
//   - the pending/canary versions must become `rejected`, NOT `superseded`.
//     `superseded` is the one state RevertAgentTemplateVersion accepts, so
//     reusing it would let a never-reviewed candidate be reverted into `current`
//     after the review layer that gated it is gone. That refusal is asserted
//     through the real store method, not by reading the column.
//
// Deleting either UPDATE from the production migration fails this test.
func TestSkillOptRemovalMigrationReconcilesCandidateAndCanaryRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	released := releasedMigrationsBeforeSkillOptRemoval(t)
	for version, migration := range released {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}

	// A champion at v1 (current), a pending candidate at v2, and an active canary
	// at v3 — the shape a home that ran the loop is left in.
	if err := previous.UpsertAgentTemplate(ctx, AgentTemplate{
		ID: "planner", Name: "Planner", Description: "champion",
		SourceRepo: "local", SourceRef: "main", SourcePath: "p.md",
		ResolvedCommit: "c1", Content: "v1 body",
	}); err != nil {
		t.Fatalf("seed champion: %v", err)
	}
	currentVersion, err := previous.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("resolve seeded current version: %v", err)
	}
	for _, seed := range []struct {
		id     string
		number int
		state  string
		sample float64
	}{
		{id: "planner@v2", number: 2, state: "pending"},
		{id: "planner@v3", number: 3, state: "canary", sample: 0.25},
	} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO agent_template_versions(
				id, template_id, version, state, name, description, source_repo, source_ref,
				source_path, resolved_commit, content_hash, content, metadata_json, promoted_at,
				canary_sample, canary_started_at)
			VALUES (?, 'planner', ?, ?, 'Planner', 'd', 'local', 'main', 'p.md', ?, ?, ?, '{}', '', ?, '')`,
			seed.id, seed.number, seed.state, seed.id, "sha256:"+seed.id, seed.state+" body", seed.sample); err != nil {
			t.Fatalf("seed %s version: %v", seed.state, err)
		}
	}
	// The hazard: latest points at the CANARY, which the migration is about to
	// retire. current_version_id still points at the champion.
	if _, err := raw.ExecContext(ctx, `UPDATE agent_templates SET latest_version_id = 'planner@v3' WHERE id = 'planner'`); err != nil {
		t.Fatalf("point latest_version_id at the canary: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous-release store: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade a database seeded with pending/canary rows: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	// Reconciliation 1: latest_version_id follows the live current version.
	var latest, current string
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT latest_version_id, current_version_id FROM agent_templates WHERE id = 'planner'`).Scan(&latest, &current); err != nil {
		t.Fatalf("read template pointers: %v", err)
	}
	if latest != currentVersion.VersionID {
		t.Fatalf("latest_version_id = %q, want the live current version %q; the migration left it pointing at a retired candidate", latest, currentVersion.VersionID)
	}
	if current != currentVersion.VersionID {
		t.Fatalf("current_version_id = %q, want %q (the champion must be untouched)", current, currentVersion.VersionID)
	}

	// Reconciliation 2: both retired versions are `rejected`, and the champion is
	// still `current`.
	states := map[string]string{}
	rows, err := upgraded.db.QueryContext(ctx, `SELECT id, state FROM agent_template_versions WHERE template_id = 'planner'`)
	if err != nil {
		t.Fatalf("read version states: %v", err)
	}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatalf("scan version state: %v", err)
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate version states: %v", err)
	}
	if got := states[currentVersion.VersionID]; got != "current" {
		t.Fatalf("champion state = %q, want current", got)
	}
	for _, id := range []string{"planner@v2", "planner@v3"} {
		got, ok := states[id]
		if !ok {
			t.Fatalf("version %s disappeared; the migration must retire it in place, not delete it", id)
		}
		if got != "rejected" {
			t.Fatalf("version %s state = %q, want rejected; `superseded` would let revert resurrect a never-reviewed candidate", id, got)
		}
	}

	// The reason `rejected` was chosen, asserted through the real method rather
	// than the column: a retired candidate cannot be reverted into `current`.
	if _, err := upgraded.RevertAgentTemplateVersion(ctx, "planner", "planner@v3"); err == nil {
		t.Fatal("reverting to a retired ex-canary must be refused; `rejected` is what makes that true")
	}

	// The surviving read path resolves the champion, not a retired row.
	resolved, err := upgraded.GetAgentTemplateReference(ctx, "planner@latest")
	if err != nil {
		t.Fatalf("resolve planner@latest after upgrade: %v", err)
	}
	if resolved.VersionID != currentVersion.VersionID || resolved.Content != "v1 body" {
		t.Fatalf("planner@latest resolved to %+v, want the champion %q", resolved, currentVersion.VersionID)
	}
}
