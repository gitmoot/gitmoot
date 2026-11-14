package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The dashboard-index migration (#2639) upgrades a populated database at the
// previous schema through the real Open path: the new indexes exist, the
// rebuilt idx_jobs_workflow_id still serves the INDEXED BY workflow queries,
// and the PR acting-role selector seeks the partial implement index.
func TestDashboardIndexesMigrationUpgradesThroughOpen(t *testing.T) {
	ctx := context.Background()
	released := migrationsBefore(t, "CREATE INDEX IF NOT EXISTS idx_jobs_implement_repo_pr_created")
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
		`INSERT INTO jobs(id, agent, type, state, payload, workflow_id, repo, pull_request, created_at, updated_at)
			VALUES ('impl-1', 'worker', 'implement', 'succeeded', '{"acting_org_role":"builder"}', 'release-42', 'acme/widget', 7, '2026-11-01 10:00:00', '2026-11-01 10:05:00')`,
		`INSERT INTO jobs(id, agent, type, state, payload, workflow_id, repo, pull_request, created_at, updated_at)
			VALUES ('review-1', 'reviewer', 'review', 'failed', '{}', 'release-42', 'acme/widget', 7, '2026-11-01 11:00:00', '2026-11-01 11:05:00')`,
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
	for _, index := range []string{"idx_jobs_implement_repo_pr_created", "idx_jobs_terminal_state_updated_at", "idx_jobs_agent_state_created", "idx_jobs_workflow_id"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&count); err != nil || count != 1 {
			t.Fatalf("index %s after upgrade: count=%d err=%v, want present", index, count, err)
		}
	}

	role, err := store.PullRequestActingRole(ctx, "acme/widget", 7)
	if err != nil || role != "builder" {
		t.Fatalf("PullRequestActingRole after upgrade = %q, %v; want builder", role, err)
	}
	summaries, err := store.ListWorkflowSummaries(ctx)
	if err != nil || len(summaries) != 1 || summaries[0].JobCount != 2 || summaries[0].Failed != 1 {
		t.Fatalf("ListWorkflowSummaries after upgrade = %+v, %v; want release-42 with 2 jobs, 1 failed", summaries, err)
	}

	plans := []struct {
		name, query, index string
		args               []any
	}{
		{"acting-role", `SELECT COALESCE(` + pullRequestActingRoleSelector + `, '')
FROM (SELECT ? AS repo, ? AS pull_request) acting_scope`, "idx_jobs_implement_repo_pr_created", []any{"acme/widget", 7}},
		{"workflow-summaries", ListWorkflowSummariesSQL, "COVERING INDEX idx_jobs_workflow_id", nil},
		{"workflow-repos", ListWorkflowReposSQL, "COVERING INDEX idx_jobs_workflow_id", nil},
	}
	for _, tc := range plans {
		plan := explainQueryPlan(t, store, tc.query, tc.args...)
		if !strings.Contains(plan, tc.index) {
			t.Fatalf("%s plan does not use %s: %s", tc.name, tc.index, plan)
		}
	}
}

// A second process opening the store while another holds the write lock (the
// daemon restarting and migrating while the dashboard starts) must wait for the
// lock and then apply or skip the pending migration, not fail at once with
// SQLITE_BUSY. Before #2639 the pending migration was applied in a deferred
// transaction that read schema_migrations first, and a read transaction cannot
// wait to upgrade to a writer: every concurrent open during migration 173's
// index build failed with "apply migration 173: database is locked".
func TestOpenWaitsForAnotherWriterDuringMigration(t *testing.T) {
	ctx := context.Background()
	released := migrationsBefore(t, "CREATE INDEX IF NOT EXISTS idx_jobs_implement_repo_pr_created")
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
	if _, err := raw.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// The other process: holds the write lock, then commits a write, so the
	// opener both waits for the lock and sees a newer snapshot afterwards.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	other.SetMaxOpenConns(1)
	writer, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("hold write lock: %v", err)
	}
	if _, err := writer.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload) VALUES ('held', 'worker', 'ask', 'queued', '{}')`); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		store, err := openRealTestStore(t, path)
		if err == nil {
			err = store.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("Open returned while another writer held the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := writer.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("release write lock: %v", err)
	}
	_ = writer.Close()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("Open after the other writer committed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Open never finished after the write lock was released")
	}

	store, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != len(migrations) {
		t.Fatalf("schema_migrations = %d, %v; want %d", count, err, len(migrations))
	}
}
