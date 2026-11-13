package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// routerRemovalMarker identifies the #2584 migration inside the append-only
// slice, resolved through the canonical migrationsBefore locator.
const routerRemovalMarker = "DROP TABLE IF EXISTS routing_telemetry"

// TestRouterRemovalMigrationOnPreviousSchema builds a store at the schema just
// before the #2584 retirement, seeds a routing_telemetry row next to the job it
// describes, and upgrades through the real Open path. The table and its indexes
// must be gone; the job row (the control) must survive.
//
// MUTATION PROOF: without the migration the table assertion is RED.
func TestRouterRemovalMigrationOnPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, routerRemovalMarker) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO jobs(id, agent, repo, type, state) VALUES ('job-1', 'reviewer', 'owner/repo', 'review', 'succeeded')`,
		`INSERT INTO routing_telemetry(job_id, repo, action, runtime, job_state, fan_out)
			VALUES ('job-1', 'owner/repo', 'review', 'codex', 'succeeded', 0)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous store: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("Open(previous-release db): %v", err)
	}
	defer upgraded.Close()

	var leftover int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%routing_telemetry%'`).Scan(&leftover); err != nil {
		t.Fatalf("count routing_telemetry objects: %v", err)
	}
	if leftover != 0 {
		t.Fatalf("routing_telemetry objects after migration = %d, want 0", leftover)
	}
	job, err := upgraded.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob(control job): %v", err)
	}
	if job.Agent != "reviewer" || job.State != "succeeded" {
		t.Fatalf("control job after migration = %+v", job)
	}
}
