package db

import (
	"context"
	"database/sql"
	"path/filepath"
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

	// Negative values clamp to 0 so a malformed runtime report cannot drive a tree
	// aggregate negative.
	if err := store.UpdateJobUsage(ctx, "j1", -5, -7); err != nil {
		t.Fatalf("UpdateJobUsage (negative) returned error: %v", err)
	}
	got, err = store.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob (after negative) returned error: %v", err)
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("clamped usage = (%d, %d), want (0, 0)", got.InputTokens, got.OutputTokens)
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
	// columns) with one row, and a schema_migrations table marking every prior
	// migration applied so Open()'s Migrate runs ONLY the new token-column ALTER.
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
	delegated_by TEXT NOT NULL DEFAULT ''
);
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
INSERT INTO jobs(id, agent, type, state, payload) VALUES ('old', 'w', 'ask', 'succeeded', '{}');
`); err != nil {
		t.Fatalf("seed old schema returned error: %v", err)
	}
	// Mark every migration except the final token-column ALTER as already applied.
	for v := 1; v < len(migrations); v++ {
		if _, err := raw.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'seed')`, v); err != nil {
			t.Fatalf("seed schema_migrations v%d returned error: %v", v, err)
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
