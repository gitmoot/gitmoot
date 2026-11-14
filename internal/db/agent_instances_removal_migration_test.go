package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// agentInstancesRemovalMarker identifies the #2590 migration inside the
// append-only slice, resolved through the canonical migrationsBefore locator.
const agentInstancesRemovalMarker = "DROP TABLE IF EXISTS agent_instances"

// TestAgentInstancesRemovalMigrationOnPreviousSchema is the DATA test for
// #2590. It builds a store at the schema immediately before the retirement
// migration, seeds the two shapes agent_instances held - a temp worker forked
// for a busy runtime session and a managed agent-type pool instance - and
// upgrades through the real Open path.
//
// Controls that must SURVIVE, because each one separates the worker-session
// removal from an over-broad one: a registered agent sharing a pool
// instance's name (GetAgent used to fall back to agent_instances, so the
// agents row must still resolve on its own), the runtime-session resource
// lock (resource_locks still serializes a runtime session), and a historical
// job the temp worker ran, with its runtime_session_busy hand-off payload and
// temp_worker_delegated event.
//
// MUTATION PROOF: drop the DROP statement and the table assertion flips RED;
// add a resource_locks, agents or jobs delete and the controls flip RED.
func TestAgentInstancesRemovalMigrationOnPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, agentInstancesRemovalMarker) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}

	const delegatedPayload = `{"repo":"owner/repo","original_agent":"reviewer","delegated_agent":"reviewer-temp-job-1","delegation_reason":"runtime_session_busy"}`
	seed := []string{
		`INSERT INTO agent_instances(name, type, runtime, runtime_ref, repo_full_name, role, template_id, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at)
			VALUES ('reviewer-temp-job-1', 'temp:reviewer', 'omp', 'fresh:job:1', 'owner/repo', 'reviewer', '', '["review"]', 'read-only', 'running', '2026-10-01T07:42:44Z', '2026-10-01T07:42:44Z', '2026-10-01T08:42:44Z')`,
		`INSERT INTO agent_instances(name, type, runtime, runtime_ref, repo_full_name, role, template_id, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at)
			VALUES ('planner', 'planner', 'omp', 'fresh:planner', 'owner/repo', 'planner', '', '["ask"]', 'danger-full-access', 'idle', '2026-06-08T06:01:28Z', '2026-06-08T06:01:28Z', '2026-06-08T06:21:28Z')`,
		`INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, capabilities_json, autonomy_policy, health_status)
			VALUES ('planner', 'planner', 'omp', 'fresh:planner', 'owner/repo', '["ask"]', 'read-only', 'ok')`,
		`INSERT INTO resource_locks(resource_key, owner_job_id, acquired_at, updated_at, expires_at)
			VALUES ('runtime:omp:session-a', 'job-1', '2026-10-01T07:42:44Z', '2026-10-01T07:42:44Z', '2999-01-01T00:00:00Z')`,
		`INSERT INTO jobs(id, agent, type, state, payload) VALUES ('job-1', 'reviewer-temp-job-1', 'review', 'succeeded', '` + delegatedPayload + `')`,
		`INSERT INTO job_events(job_id, kind, message) VALUES ('job-1', 'temp_worker_delegated', 'delegated from reviewer to reviewer-temp-job-1')`,
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
		t.Fatalf("upgrade a store seeded with agent instances: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	var leftover int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE tbl_name = 'agent_instances'`).Scan(&leftover); err != nil {
		t.Fatalf("inspect agent_instances: %v", err)
	}
	if leftover != 0 {
		t.Fatalf("%d agent_instances schema objects survived the #2590 migration", leftover)
	}

	planner, err := upgraded.GetAgent(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgent(planner) after migration: %v", err)
	}
	if planner.AutonomyPolicy != "read-only" {
		t.Fatalf("registered planner after migration = %+v, want its own read-only row", planner)
	}
	if _, err := upgraded.GetAgent(ctx, "reviewer-temp-job-1"); err != sql.ErrNoRows {
		t.Fatalf("GetAgent(temp worker) after migration err = %v, want sql.ErrNoRows", err)
	}
	if _, err := upgraded.GetResourceLock(ctx, "runtime:omp:session-a"); err != nil {
		t.Fatalf("runtime-session resource lock after migration: %v, want kept", err)
	}
	job, err := upgraded.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob(job-1) after migration: %v", err)
	}
	if job.Agent != "reviewer-temp-job-1" || !strings.Contains(job.Payload, `"delegation_reason":"runtime_session_busy"`) {
		t.Fatalf("historical temp-worker job after migration = %+v, want kept", job)
	}
	events, err := upgraded.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents after migration: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "temp_worker_delegated" {
		t.Fatalf("historical job events after migration = %+v, want the temp_worker_delegated event kept", events)
	}
}
