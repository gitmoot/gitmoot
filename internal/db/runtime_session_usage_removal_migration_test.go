package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestRuntimeSessionUsageRemovalMigrationUpgradesThroughOpen is the upgrade test
// for #2587: a home built at the schema before the drop, holding codex session
// baselines and a job with recorded token usage, upgrades through the real Open
// path. The table is gone and the job's per-job token counts survive.
func TestRuntimeSessionUsageRemovalMigrationUpgradesThroughOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, "DROP TABLE IF EXISTS runtime_session_usage") {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO runtime_session_usage(session_key, input_cum, output_cum) VALUES ('codex:019a-thread', 103779, 45)`,
		`INSERT INTO jobs(id, agent, type, state, payload, root_id, input_tokens, output_tokens)
			VALUES ('codex-review-1', 'legacy-codex-review', 'review', 'succeeded', '{"effective_runtime":"codex"}', 'codex-review-1', 18098, 5)`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed previous-schema store: %v\n%s", err, statement)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous store: %v", err)
	}

	store, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade through Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var tables int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'runtime_session_usage'`).Scan(&tables); err != nil {
		t.Fatalf("inspect sqlite_master: %v", err)
	}
	if tables != 0 {
		t.Fatal("runtime_session_usage survived the #2587 migration")
	}
	var input, output int
	if err := store.db.QueryRowContext(ctx,
		`SELECT input_tokens, output_tokens FROM jobs WHERE id = 'codex-review-1'`).Scan(&input, &output); err != nil {
		t.Fatalf("historical codex job after upgrade: %v", err)
	}
	if input != 18098 || output != 5 {
		t.Fatalf("historical job usage = %d/%d, want 18098/5 preserved", input, output)
	}
}
