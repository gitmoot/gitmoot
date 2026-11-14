package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// agentTemplatesRemovalMarker identifies the #2589 migration inside the
// append-only slice, resolved through the canonical migrationsBefore locator.
const agentTemplatesRemovalMarker = "DROP TABLE IF EXISTS agent_template_versions"

// TestAgentTemplatesRemovalMigrationOnPreviousSchema is the DATA test for
// #2589. It builds a store at the schema immediately before the retirement
// migration, seeds a template with two versions and an agent that names it, and
// upgrades through the real Open path.
//
// The agent and its template_id must SURVIVE: the column is kept as history.
//
// MUTATION PROOF: drop either DROP statement and the table assertion flips RED;
// add an agents delete or a template_id reset and the control flips RED.
func TestAgentTemplatesRemovalMigrationOnPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, agentTemplatesRemovalMarker) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}

	seed := []string{
		`INSERT INTO agent_templates(id, name, source_repo, source_ref, source_path, resolved_commit, content, current_version_id, latest_version_id)
			VALUES ('planner', 'Planner', 'gitmoot/gitmoot', 'main', 'skills/gitmoot/agent-templates/planner.md', 'abc', 'plan things', 'planner@v2', 'planner@v2')`,
		`INSERT INTO agent_template_versions(id, template_id, version, state, name, source_repo, source_ref, source_path, resolved_commit, content)
			VALUES ('planner@v1', 'planner', 1, 'retired', 'Planner', 'gitmoot/gitmoot', 'main', 'p.md', 'abc', 'old')`,
		`INSERT INTO agent_template_versions(id, template_id, version, state, name, source_repo, source_ref, source_path, resolved_commit, content)
			VALUES ('planner@v2', 'planner', 2, 'current', 'Planner', 'gitmoot/gitmoot', 'main', 'p.md', 'def', 'plan things')`,
		`INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, template_id, capabilities_json)
			VALUES ('planner-1', 'planner', 'codex', 'ref', 'gitmoot/gitmoot', 'planner', '["ask"]')`,
	}
	for _, statement := range seed {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed previous-schema store: %v\n%s", err, statement)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous-schema store: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade a store seeded with agent templates: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	var leftover int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE tbl_name IN ('agent_templates', 'agent_template_versions')`).Scan(&leftover); err != nil {
		t.Fatalf("inspect template tables: %v", err)
	}
	if leftover != 0 {
		t.Fatalf("%d agent template schema objects survived the #2589 migration", leftover)
	}

	var templateID string
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT template_id FROM agents WHERE name = 'planner-1'`).Scan(&templateID); err != nil {
		t.Fatalf("agent planner-1 after migration: %v", err)
	}
	if templateID != "planner" {
		t.Fatalf("agents.template_id = %q after migration, want the historical %q", templateID, "planner")
	}
}
