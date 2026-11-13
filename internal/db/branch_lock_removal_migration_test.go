package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// branchLockRemovalMarker identifies the #2591 migration inside the append-only
// slice, resolved through the canonical migrationsBefore locator.
const branchLockRemovalMarker = "DROP TABLE IF EXISTS lock_events"

// TestBranchLockRemovalMigrationOnPreviousSchema builds a store at the schema
// just before the #2591 retirement, seeds a held branch lock, a lock event and a
// resource lock, and upgrades through the real Open path. branch_locks and
// lock_events must be gone; the resource lock and the job (controls) survive.
//
// MUTATION PROOF: without the migration the table assertion is RED.
func TestBranchLockRemovalMigrationOnPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, branchLockRemovalMarker) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO jobs(id, agent, repo, type, state) VALUES ('job-1', 'reviewer', 'owner/repo', 'review', 'succeeded')`,
		`INSERT INTO branch_locks(repo_full_name, branch, owner, acting_org_role) VALUES ('owner/repo', 'feature', 'lead', 'builder')`,
		`INSERT INTO lock_events(repo_full_name, branch, owner, kind, message) VALUES ('owner/repo', 'feature', 'lead', 'released', 'done')`,
		`INSERT INTO resource_locks(resource_key, owner_job_id, acquired_at, updated_at, expires_at) VALUES ('runtime:x', 'job-1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2999-01-01T00:00:00Z')`,
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
		`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('branch_locks', 'lock_events')`).Scan(&leftover); err != nil {
		t.Fatalf("count branch lock tables: %v", err)
	}
	if leftover != 0 {
		t.Fatalf("branch lock tables after migration = %d, want 0", leftover)
	}
	if _, err := upgraded.GetResourceLock(ctx, "runtime:x"); err != nil {
		t.Fatalf("GetResourceLock(control lock): %v", err)
	}
	job, err := upgraded.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob(control job): %v", err)
	}
	if job.Agent != "reviewer" || job.State != "succeeded" {
		t.Fatalf("control job after migration = %+v", job)
	}
}
