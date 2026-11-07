package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// pipelineRemovalMarker identifies the #2542 migration inside the append-only
// slice, resolved through the canonical migrationsBefore locator.
const pipelineRemovalMarker = "DROP TABLE IF EXISTS pipeline_exposures"

// TestPipelineRemovalMigrationOnPreviousSchema is the DATA test for #2542. It
// builds a store at the schema immediately before the retirement migration,
// seeds the state a live home is in - a pipeline with runs, stages, a service
// run, an exposure, a trigger state, keychain grants of both consumer kinds, a
// hidden runner agent, and the historical stage jobs those runs left behind -
// and upgrades through the real Open path.
//
// Controls that must SURVIVE, because each one separates a scoped retirement
// from an over-broad one: an ordinary agent whose NAME starts with "pipeline-"
// (a role filter, not a name filter), its agent_repos row, the keychain key
// rows themselves, and every historical job row including the produce jobs.
//
// MUTATION PROOF: drop the agent DELETE and the runner assertions flip RED;
// match agents by name instead of role and the control agent flips RED; leave
// keychain_grants in place and the table assertion flips RED; delete produce
// jobs and the listing assertions flip RED. Run against a binary without the
// migration, every dropped-table assertion is RED.
func TestPipelineRemovalMigrationOnPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, pipelineRemovalMarker) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}

	seed := []string{
		`INSERT INTO pipelines(name, spec_yaml, spec_hash, repo, enabled, created_at, updated_at)
			VALUES ('doorbell', 'name: doorbell', 'hash-1', 'owner/repo', 1, '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		`INSERT INTO pipeline_runs(id, pipeline, spec_hash, state, started_at)
			VALUES ('prun-doorbell-1', 'doorbell', 'hash-1', 'running', '2026-11-06T19:01:51Z')`,
		`INSERT INTO pipeline_run_stages(run_id, stage_id, state, job_id, attempt)
			VALUES ('prun-doorbell-1', 'render', 'running', 'prun-doorbell-1-render-a0', 0)`,
		`INSERT INTO pipeline_service_runs(run_id, pipeline_name, created_at)
			VALUES ('prun-doorbell-1', 'doorbell', '2026-11-06T19:01:51Z')`,
		`INSERT INTO pipeline_exposures(pipeline_name, schema_version, schema_json, schema_hash, token_hash)
			VALUES ('doorbell', 1, '{}', 'schema-hash', x'00')`,
		`INSERT INTO pipeline_trigger_states(downstream_pipeline, upstream_pipeline)
			VALUES ('doorbell', 'upstream')`,
		`INSERT INTO keychain_keys(name, mode) VALUES ('TELEGRAM_BOT_TOKEN', 'injected')`,
		`INSERT INTO keychain_keys(name, mode, proxy_upstream, proxy_auth_kind) VALUES ('OMP_AUTH_GATEWAY', 'proxied', 'https://example.test', 'bearer')`,
		`INSERT INTO keychain_grants(consumer_kind, consumer_id, key_name) VALUES ('pipeline', 'doorbell', 'TELEGRAM_BOT_TOKEN')`,
		`INSERT INTO keychain_grants(consumer_kind, consumer_id, key_name) VALUES ('agent', 'pipeline-reviewer', 'OMP_AUTH_GATEWAY')`,
		`INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope)
			VALUES ('pipeline-doorbell-runner', 'pipeline-runner', 'shell', '', 'owner/repo')`,
		`INSERT INTO agent_repos(agent_name, repo_full_name) VALUES ('pipeline-doorbell-runner', 'owner/repo')`,
		`INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope)
			VALUES ('pipeline-reviewer', 'reviewer', 'codex', '', 'owner/repo')`,
		`INSERT INTO agent_repos(agent_name, repo_full_name) VALUES ('pipeline-reviewer', 'owner/repo')`,
		`INSERT INTO jobs(id, agent, type, state, payload, root_id)
			VALUES ('prun-doorbell-1-render-a0', 'pipeline-doorbell-runner', 'ask', 'blocked',
				'{"sender":"pipeline","pipeline_name":"doorbell","shell_env":["GITMOOT_PIPELINE_NAME=doorbell"]}', 'prun-doorbell-1')`,
		`INSERT INTO jobs(id, agent, type, state, payload, root_id)
			VALUES ('prun-appkit-docus-1-derive-a0', 'docus-appkit-writer', 'produce', 'succeeded',
				'{"sender":"pipeline","pipeline_name":"appkit-docus","writable_paths":["/srv/docs"],"network":true}', 'prun-appkit-docus-1')`,
		`INSERT INTO job_events(job_id, kind, message) VALUES ('prun-appkit-docus-1-derive-a0', 'produce_sandbox_unsupported', 'historical event')`,
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
		t.Fatalf("upgrade a store seeded with pipeline rows: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	for _, name := range []string{
		"pipelines", "pipeline_runs", "pipeline_run_stages", "pipeline_service_runs",
		"pipeline_exposures", "pipeline_trigger_states", "keychain_grants",
	} {
		var count int
		if err := upgraded.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("table %s survived the #2542 migration", name)
		}
	}
	var leftover int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%pipeline%'`).Scan(&leftover); err != nil {
		t.Fatalf("inspect pipeline indexes: %v", err)
	}
	if leftover != 0 {
		t.Fatalf("%d pipeline schema objects survived the #2542 migration", leftover)
	}

	if _, err := upgraded.GetAgent(ctx, "pipeline-doorbell-runner"); err == nil {
		t.Fatal("hidden pipeline runner agent survived the #2542 migration")
	}
	var runnerRepos int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_repos WHERE agent_name = 'pipeline-doorbell-runner'`).Scan(&runnerRepos); err != nil {
		t.Fatalf("inspect runner agent_repos: %v", err)
	}
	if runnerRepos != 0 {
		t.Fatalf("runner agent_repos rows = %d, want 0", runnerRepos)
	}

	if _, err := upgraded.GetAgent(ctx, "pipeline-reviewer"); err != nil {
		t.Fatalf("control agent named pipeline-* but not a runner was removed: %v", err)
	}
	var controlRepos int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_repos WHERE agent_name = 'pipeline-reviewer'`).Scan(&controlRepos); err != nil {
		t.Fatalf("inspect control agent_repos: %v", err)
	}
	if controlRepos != 1 {
		t.Fatalf("control agent_repos rows = %d, want 1", controlRepos)
	}

	keys, err := upgraded.ListKeychainKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeychainKeys after migration: %v", err)
	}
	if len(keys) != 2 || keys[0].Name != "OMP_AUTH_GATEWAY" || keys[1].Name != "TELEGRAM_BOT_TOKEN" || keys[1].Mode != "injected" {
		t.Fatalf("keychain keys after migration = %+v, want both rows kept unchanged", keys)
	}

	jobs, err := upgraded.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs after migration: %v", err)
	}
	listed := map[string]string{}
	for _, job := range jobs {
		listed[job.ID] = job.Type
	}
	if listed["prun-appkit-docus-1-derive-a0"] != "produce" || listed["prun-doorbell-1-render-a0"] != "ask" {
		t.Fatalf("historical pipeline jobs after migration = %v, want the produce and ask stage jobs listed", listed)
	}
	produce, err := upgraded.GetJob(ctx, "prun-appkit-docus-1-derive-a0")
	if err != nil {
		t.Fatalf("GetJob(historical produce job): %v", err)
	}
	if produce.Type != "produce" || produce.State != "succeeded" || produce.Agent != "docus-appkit-writer" {
		t.Fatalf("historical produce job = %+v", produce)
	}
	events, err := upgraded.ListJobEvents(ctx, produce.ID)
	if err != nil {
		t.Fatalf("ListJobEvents(historical produce job): %v", err)
	}
	if len(events) != 1 || events[0].Kind != "produce_sandbox_unsupported" {
		t.Fatalf("historical produce job events = %+v", events)
	}
}
