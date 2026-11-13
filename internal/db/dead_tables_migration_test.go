package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// The dead-tables migration (#2594) upgrades a populated database at the
// previous schema through the real Open path: the groom verdict tables and
// goals are gone, and tasks keep their goal_id.
func TestDeadTablesMigrationDropsPopulatedTablesThroughOpen(t *testing.T) {
	ctx := context.Background()
	dropped := []string{"groom_llm_verdicts", "groom_quality_verdicts", "groom_stale_verdicts", "goals"}
	released := migrationsBefore(t, "DROP TABLE IF EXISTS groom_llm_verdicts")
	path := filepath.Join(t.TempDir(), "previous.db")
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
	seed := []string{
		`INSERT INTO groom_llm_verdicts(content_hash, verdict) VALUES ('h1', 'keep')`,
		`INSERT INTO groom_quality_verdicts(content_hash, verdict, confidence) VALUES ('h2', 'keep', 0.9)`,
		`INSERT INTO groom_stale_verdicts(content_hash, verdict) VALUES ('h3', 'stale')`,
		`INSERT INTO goals(id, title) VALUES ('goal-1', 'planned goal')`,
	}
	for _, statement := range seed {
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
	for _, table := range dropped {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("table %s after upgrade: count=%d err=%v, want dropped", table, count, err)
		}
	}
	var goalID int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'goal_id'`).Scan(&goalID); err != nil || goalID != 1 {
		t.Fatalf("tasks.goal_id after upgrade: %d, %v; want kept", goalID, err)
	}
}
