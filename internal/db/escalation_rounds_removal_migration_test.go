package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// escalationRoundsRemovalMarker identifies the #2588 migration inside the
// append-only slice, resolved through the canonical migrationsBefore locator.
const escalationRoundsRemovalMarker = "DROP TABLE IF EXISTS escalation_rounds"

// TestEscalationRoundsRemovalMigrationOnPreviousSchema is the DATA test for
// #2588 PR 3. It builds a store at the schema immediately before the
// retirement migration, seeds an escalation round (a settled one and the
// coordinator's unsettled slot) and upgrades through the real Open path.
//
// Controls that must SURVIVE, because each one separates the engine removal
// from an over-broad one: a historical delegation tree (the coordinator and a
// child carrying parent_job_id, delegation_id, delegation_depth, delegated_by,
// root_id and root_killed) with its delegation_escalation_requested event, and
// an org escalation wake (wake_outbox source_kind 'escalation'), which is the
// live `org escalate` path and not Orchestra.
//
// MUTATION PROOF: drop the DROP statement and the table assertion flips RED;
// add a jobs column drop or a wake_outbox delete and the controls flip RED.
func TestEscalationRoundsRemovalMigrationOnPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	for version, migration := range migrationsBefore(t, escalationRoundsRemovalMarker) {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}

	seed := []string{
		`INSERT INTO jobs(id, agent, type, state, payload, root_id, root_killed)
			VALUES ('coord-1', 'planner', 'ask', 'succeeded', '{}', 'coord-1', 1)`,
		`INSERT INTO jobs(id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_id)
			VALUES ('coord-1/delegation/a', 'reviewer', 'review', 'failed', '{"parent_job_id":"coord-1","delegation_id":"a","root_job_id":"coord-1"}', 'coord-1', 'a', 1, 'planner', 'coord-1')`,
		`INSERT INTO job_events(job_id, kind, message) VALUES ('coord-1', 'delegation_escalation_requested', 'historical round')`,
		`INSERT INTO escalation_rounds(job_id, round_id, kind, resolved_at, effects_completed_at)
			VALUES ('coord-1', 'round-settled', 'failure', '2026-08-03 13:20:16', '2026-08-03 13:21:00')`,
		`INSERT INTO escalation_rounds(job_id, round_id, kind) VALUES ('coord-1', 'round-open', 'ask')`,
		`INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key)
			VALUES ('escalation', 'org-escalate-1', 'lead', 'lead:escalation')`,
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
		t.Fatalf("upgrade a store seeded with escalation rounds: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	var leftover int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE tbl_name = 'escalation_rounds'`).Scan(&leftover); err != nil {
		t.Fatalf("inspect escalation_rounds: %v", err)
	}
	if leftover != 0 {
		t.Fatalf("%d escalation_rounds schema objects survived the #2588 migration", leftover)
	}

	child, err := upgraded.GetJob(ctx, "coord-1/delegation/a")
	if err != nil {
		t.Fatalf("GetJob(child) after migration: %v", err)
	}
	if child.ParentJobID != "coord-1" || child.DelegationID != "a" || child.DelegationDepth != 1 || child.DelegatedBy != "planner" {
		t.Fatalf("historical child after migration = %+v, want its delegation columns kept", child)
	}
	tree, err := upgraded.ListJobsByRoot(ctx, "coord-1")
	if err != nil {
		t.Fatalf("ListJobsByRoot after migration: %v", err)
	}
	if len(tree) != 2 {
		t.Fatalf("historical tree after migration = %d jobs, want 2", len(tree))
	}
	var rootKilled int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT root_killed FROM jobs WHERE id = 'coord-1'`).Scan(&rootKilled); err != nil || rootKilled != 1 {
		t.Fatalf("root_killed after migration = %d err=%v, want kept", rootKilled, err)
	}
	events, err := upgraded.ListJobEvents(ctx, "coord-1")
	if err != nil {
		t.Fatalf("ListJobEvents after migration: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "delegation_escalation_requested" {
		t.Fatalf("historical job events after migration = %+v, want the escalation event kept", events)
	}
	var wakes int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wake_outbox WHERE source_kind = 'escalation'`).Scan(&wakes); err != nil {
		t.Fatalf("inspect org escalation wakes: %v", err)
	}
	if wakes != 1 {
		t.Fatalf("org escalation wakes after migration = %d, want 1 kept", wakes)
	}
}
