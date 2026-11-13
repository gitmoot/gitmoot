package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// observabilityRemovalMarker identifies the #2592 migration inside the
// append-only slice, resolved through the canonical migrationsBefore locator.
const observabilityRemovalMarker = "DROP TABLE IF EXISTS permission_policy_warning_claims"

// TestObservabilityRemovalMigrationOnPreviousSchema is the DATA test for #2592.
// It builds a store at the schema immediately before the retirement migration,
// seeds the rows a live home holds - permission-policy warning claims, the
// ratchet baseline singleton and an open recycle-overdue episode - and upgrades
// through the real Open path.
//
// Controls that must SURVIVE, because each one separates a scoped retirement
// from an over-broad one: the historical permission_policy_not_applied job
// event (job history is not observability state), the org_blocked_episodes row
// that shares the episode table shape, and the org_role_presence row the kept
// recycle_after dispatch warning reads.
//
// MUTATION PROOF: drop any one DROP statement and its table assertion flips RED;
// run against a binary without the migration and every table assertion is RED.
func TestObservabilityRemovalMigrationOnPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, observabilityRemovalMarker) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}

	seed := []string{
		`INSERT INTO permission_policy_warning_claims(agent, runtime, policy, capability, window_start, job_id)
			VALUES ('gm-review-opus', 'claude', 'auto', 'not-applied', '2026-11-13T00:00:00Z', 'job-review-1')`,
		`INSERT INTO permission_policy_observation_baseline(id, affected_count, configs_json)
			VALUES (1, 90, '["gm-review-opus\u001fclaude\u001fauto\u001fnot-applied"]')`,
		`INSERT INTO org_recycle_overdue_episodes(subject, overdue_since, emitted_at, updated_at)
			VALUES ('owner', '2026-11-13T01:00:00Z', NULL, '2026-11-13T02:00:00Z')`,
		`INSERT INTO org_blocked_episodes(subject, blocked_since, emitted_at, updated_at)
			VALUES ('role:review', '2026-11-13T01:00:00Z', NULL, '2026-11-13T02:00:00Z')`,
		`INSERT INTO org_role_presence(role, last_seen_at, last_command) VALUES ('owner', '2026-11-13 00:00:00', 'agent_run')`,
		`INSERT INTO jobs(id, agent, type, state, payload, root_id)
			VALUES ('job-review-1', 'gm-review-opus', 'review', 'succeeded', '{}', 'job-review-1')`,
		`INSERT INTO job_events(job_id, kind, message) VALUES ('job-review-1', 'permission_policy_not_applied', 'historical event')`,
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
		t.Fatalf("upgrade a store seeded with observability rows: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	for _, name := range []string{
		"permission_policy_warning_claims", "permission_policy_observation_baseline", "org_recycle_overdue_episodes",
	} {
		var count int
		if err := upgraded.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("table %s survived the #2592 migration", name)
		}
	}

	blocked, err := upgraded.ListBlockedEpisodes(ctx)
	if err != nil {
		t.Fatalf("ListBlockedEpisodes after migration: %v", err)
	}
	if len(blocked) != 1 || blocked[0].Subject != "role:review" {
		t.Fatalf("blocked episodes after migration = %+v, want the role:review row kept", blocked)
	}
	presence, found, err := upgraded.GetOrgRolePresence(ctx, "owner")
	if err != nil || !found || presence.LastCommand != "agent_run" {
		t.Fatalf("owner presence after migration = %+v found=%v err=%v, want kept", presence, found, err)
	}
	events, err := upgraded.ListJobEvents(ctx, "job-review-1")
	if err != nil {
		t.Fatalf("ListJobEvents after migration: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "permission_policy_not_applied" {
		t.Fatalf("historical job events after migration = %+v, want the not-applied event kept", events)
	}
}
