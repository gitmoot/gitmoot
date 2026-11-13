package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// heartbeatRemovalMarker identifies the #2585 migration inside the append-only
// slice, resolved through the canonical migrationsBefore locator.
const heartbeatRemovalMarker = "DROP TABLE IF EXISTS heartbeat_state"

// TestHeartbeatRemovalMigrationOnPreviousSchema builds a store at the schema
// immediately before the #2585 retirement, seeds a heartbeat_state row and an
// ordinary job, and upgrades through the real Open path. The table must be gone
// and the job must survive.
func TestHeartbeatRemovalMigrationOnPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, heartbeatRemovalMarker) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	seed := []string{
		`INSERT INTO heartbeat_state(agent, name, last_run_at, next_due_at, last_job_id, last_status)
			VALUES ('maintainer', 'daily', '2026-11-01T00:00:00Z', '2026-11-02T00:00:00Z', 'heartbeat-maintainer-daily-1', 'enqueued')`,
		`INSERT INTO jobs(id, agent, type, state, payload, root_id)
			VALUES ('heartbeat-maintainer-daily-1', 'maintainer', 'ask', 'succeeded', '{"sender":"heartbeat"}', 'heartbeat-maintainer-daily-1')`,
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
		t.Fatalf("upgrade a store seeded with heartbeat_state rows: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	var count int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'heartbeat_state'`).Scan(&count); err != nil {
		t.Fatalf("inspect heartbeat_state: %v", err)
	}
	if count != 0 {
		t.Fatal("table heartbeat_state survived the #2585 migration")
	}
	job, err := upgraded.GetJob(ctx, "heartbeat-maintainer-daily-1")
	if err != nil {
		t.Fatalf("GetJob(historical heartbeat job): %v", err)
	}
	if job.State != "succeeded" || job.Agent != "maintainer" {
		t.Fatalf("historical heartbeat job = %+v", job)
	}
}
