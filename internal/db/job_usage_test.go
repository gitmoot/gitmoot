package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdateJobUsageRoundTrip pins the #338 Part B usage write path: a job starts
// at 0/0 (the column default), UpdateJobUsage persists input/output counts, and
// every jobs SELECT reads them back consistently.
func TestUpdateJobUsageRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.CreateJob(ctx, Job{ID: "j1", Agent: "w", Type: "ask", State: "succeeded", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	// Default before any usage is recorded.
	got, err := store.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("new job usage = (%d, %d), want (0, 0)", got.InputTokens, got.OutputTokens)
	}

	if err := store.UpdateJobUsage(ctx, "j1", 1500, 320); err != nil {
		t.Fatalf("UpdateJobUsage returned error: %v", err)
	}

	got, err = store.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob (after update) returned error: %v", err)
	}
	if got.InputTokens != 1500 || got.OutputTokens != 320 {
		t.Fatalf("GetJob usage = (%d, %d), want (1500, 320)", got.InputTokens, got.OutputTokens)
	}

	// ListJobs must scan the same columns consistently.
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].InputTokens != 1500 || jobs[0].OutputTokens != 320 {
		t.Fatalf("ListJobs usage = %+v, want one job with (1500, 320)", jobs)
	}

	// Negative deltas clamp to 0 so a malformed runtime report cannot drive a tree
	// aggregate negative; usage accumulates, so a clamped delta preserves the prior
	// total rather than resetting it.
	if err := store.UpdateJobUsage(ctx, "j1", -5, -7); err != nil {
		t.Fatalf("UpdateJobUsage (negative) returned error: %v", err)
	}
	got, err = store.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob (after negative) returned error: %v", err)
	}
	if got.InputTokens != 1500 || got.OutputTokens != 320 {
		t.Fatalf("after a clamped-negative delta usage = (%d, %d), want (1500, 320) preserved", got.InputTokens, got.OutputTokens)
	}

	// A second positive delivery ACCUMULATES (a malformed-output repair re-delivers
	// the same job id) instead of overwriting the first write.
	if err := store.UpdateJobUsage(ctx, "j1", 100, 80); err != nil {
		t.Fatalf("UpdateJobUsage (accumulate) returned error: %v", err)
	}
	got, err = store.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob (after accumulate) returned error: %v", err)
	}
	if got.InputTokens != 1600 || got.OutputTokens != 400 {
		t.Fatalf("accumulated usage = (%d, %d), want (1600, 400)", got.InputTokens, got.OutputTokens)
	}

	// Unknown job id is an error (mirrors UpdateJobPayload's requireAffected).
	if err := store.UpdateJobUsage(ctx, "nope", 1, 1); err == nil {
		t.Fatal("UpdateJobUsage on unknown job should error")
	}
}

// TestJobTokenMigrationOnPreExistingDB pins that the input_tokens/output_tokens
// migration applies to a database that already has a populated jobs table without
// the token columns: existing rows gain the columns at their 0 default and remain
// readable through the standard Job scans.
func TestJobTokenMigrationOnPreExistingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")

	// Build an "old" database: a jobs table at the pre-#338B shape (no token
	// columns, no root_id) with one row, and a schema_migrations table marking
	// every migration up to but excluding the token-column ALTER as applied, so
	// Open()'s Migrate runs the token-column ALTER (the column under test) onto the
	// pre-existing populated table. The #420 root_id ALTER+backfill (the last
	// migration) runs in the same pass and is a no-op for this assertion.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
CREATE TABLE jobs (
	id TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	type TEXT NOT NULL,
	state TEXT NOT NULL,
	payload TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	parent_job_id TEXT NOT NULL DEFAULT '',
	delegation_id TEXT NOT NULL DEFAULT '',
	delegation_depth INTEGER NOT NULL DEFAULT 0,
	delegated_by TEXT NOT NULL DEFAULT '',
	root_killed INTEGER NOT NULL DEFAULT 0
);
-- A minimal agent_template_versions table (as it existed at the input_tokens
-- migration point) so the later #484 canary ALTER ADD COLUMN has its table; the
-- real table was created by an earlier (here pre-seeded-as-applied) migration.
CREATE TABLE agent_template_versions (
	id TEXT PRIMARY KEY,
	template_id TEXT NOT NULL,
	version INTEGER NOT NULL,
	state TEXT NOT NULL
);
-- A minimal agents table (as it existed at the input_tokens migration point) so
-- the later #33 preset_delivery ALTER ADD COLUMN that runs in this pass has its
-- table; the real table was created by an earlier (here pre-seeded-as-applied)
-- migration.
CREATE TABLE agents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	role TEXT NOT NULL,
	runtime TEXT NOT NULL,
	runtime_ref TEXT NOT NULL,
	repo_scope TEXT NOT NULL
);
-- A minimal agent_instances table so later additive per-instance column
-- migrations can run. The real table was created by an earlier migration that
-- this fixture marks as applied.
CREATE TABLE agent_instances (
	name TEXT PRIMARY KEY
);
-- A minimal repos table (as it existed at the input_tokens migration point) so
-- the later #831 primary_checkout_path ALTER ADD COLUMN that runs in this pass
-- has its table; the real table was created by an earlier (here pre-seeded-as-
-- applied) migration.
CREATE TABLE repos (
	owner TEXT NOT NULL,
	name TEXT NOT NULL,
	full_name TEXT NOT NULL,
	default_branch TEXT NOT NULL DEFAULT 'main',
	remote_url TEXT NOT NULL DEFAULT '',
	checkout_path TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	poll_interval TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (owner, name)
);
-- job_events as it existed at an earlier (here pre-seeded-as-applied) migration,
-- so the #549 job_events index migration that runs in this pass has its table.
CREATE TABLE job_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	message TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- Minimal memory tables at the pre-#842 shape. The original memory base-table
-- migration is marked applied below, while later additive migrations still run;
-- this makes the new confirmed_memories.context ALTER exercise a pre-existing
-- table instead of relying on a fresh-schema create in this old-schema fixture.
CREATE TABLE confirmed_memories (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_kind TEXT NOT NULL,
	owner_ref TEXT NOT NULL,
	owner_version TEXT NOT NULL DEFAULT '',
	repo TEXT,
	key TEXT NOT NULL,
	superseded_by INTEGER
);
CREATE UNIQUE INDEX idx_confirmed_repo_key ON confirmed_memories(owner_kind, owner_ref, owner_version, repo, key) WHERE repo IS NOT NULL;
CREATE UNIQUE INDEX idx_confirmed_general_key ON confirmed_memories(owner_kind, owner_ref, owner_version, key) WHERE repo IS NULL;
CREATE TABLE memory_observations (id INTEGER PRIMARY KEY AUTOINCREMENT);
-- A minimal resource_locks table (created by an earlier, here pre-seeded-as-
-- applied migration) so the #651 owner_boot_id ALTER ADD COLUMN that runs in this
-- pass has its table.
CREATE TABLE resource_locks (
	resource_key TEXT PRIMARY KEY,
	owner_job_id TEXT NOT NULL,
	acquired_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
INSERT INTO jobs(id, agent, type, state, payload) VALUES ('old', 'w', 'ask', 'succeeded', '{}');
`); err != nil {
		t.Fatalf("seed old schema returned error: %v", err)
	}
	// Mark every migration before the token-column ALTER as already applied, then
	// leave that ALTER (and everything after it) unapplied so they all run on Open.
	// The seeded jobs table is missing the token columns AND root_id, so the
	// token-column ALTER and the #420 root_id ALTER both succeed; any later
	// migrations (e.g. #473's skillopt_bandit_arms) are independent and also run.
	// We locate the token migration by CONTENT rather than position so appending
	// new migrations never breaks this pin.
	tokenMigrationVersion := -1
	confirmedMemoryBaseVersion := -1
	for i, m := range migrations {
		if tokenMigrationVersion < 0 && strings.Contains(m, "input_tokens") {
			tokenMigrationVersion = i + 1 // migration versions are 1-indexed
		}
		if strings.Contains(m, "CREATE TABLE confirmed_memories") {
			confirmedMemoryBaseVersion = i + 1
		}
	}
	if tokenMigrationVersion < 1 {
		t.Fatalf("could not locate the input_tokens migration")
	}
	if confirmedMemoryBaseVersion < 1 {
		t.Fatalf("could not locate the confirmed-memory base migration")
	}
	for v := 1; v < tokenMigrationVersion; v++ {
		if _, err := raw.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'seed')`, v); err != nil {
			t.Fatalf("seed schema_migrations v%d returned error: %v", v, err)
		}
	}
	if confirmedMemoryBaseVersion >= tokenMigrationVersion {
		if _, err := raw.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'seed')`, confirmedMemoryBaseVersion); err != nil {
			t.Fatalf("seed confirmed-memory base migration v%d returned error: %v", confirmedMemoryBaseVersion, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db returned error: %v", err)
	}

	// Re-open through the real Store: Migrate must apply the token-column ALTER to
	// the pre-existing, populated jobs table without error.
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate) returned error: %v", err)
	}
	defer store.Close()

	got, err := store.GetJob(ctx, "old")
	if err != nil {
		t.Fatalf("GetJob after migration returned error: %v", err)
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("migrated existing row usage = (%d, %d), want (0, 0)", got.InputTokens, got.OutputTokens)
	}

	// The new columns are writable on the migrated row.
	if err := store.UpdateJobUsage(ctx, "old", 42, 7); err != nil {
		t.Fatalf("UpdateJobUsage on migrated row returned error: %v", err)
	}
	got, err = store.GetJob(ctx, "old")
	if err != nil {
		t.Fatalf("GetJob (post-write) returned error: %v", err)
	}
	if got.InputTokens != 42 || got.OutputTokens != 7 {
		t.Fatalf("migrated row usage after write = (%d, %d), want (42, 7)", got.InputTokens, got.OutputTokens)
	}
}
