package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// chatRemovalMarker identifies the #1754 migration inside the append-only slice.
// The prefix is resolved through the CANONICAL migrationsBefore locator so a
// future hardening of that locator reaches this fixture too, and so the marker is
// fail-closed on a missing or ambiguous match rather than guessing.
const chatRemovalMarker = "DROP TABLE IF EXISTS chat_threads"

// TestChatRemovalMigrationDropsTablesAndPendingChatWakes is the DATA test for
// the #1754 migration. A schema assertion can see that the tables are gone; it
// cannot see the half that matters operationally — a `chat_message` wake_outbox
// row is an obligation the daemon would keep trying to deliver for a source that
// no longer exists, and nothing left in the tree can resolve it. So this builds a
// database at the previous released version, seeds the state a live home is
// actually in (a thread, a message, a mention, and a PENDING chat wake), then
// upgrades through the real Open path.
//
// The neighbouring workflow_note wake row is the control: it must SURVIVE. That
// is what separates a correctly scoped DELETE from one that clears the outbox.
//
// MUTATION PROOF: drop the DELETE from the migration and the chat-wake assertion
// flips RED; widen it to every row and the workflow_note control flips RED.
func TestChatRemovalMigrationDropsTablesAndPendingChatWakes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous-release.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	previous := &Store{db: raw}
	released := migrationsBefore(t, chatRemovalMarker)
	for version, migration := range released {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}

	if _, err := raw.ExecContext(ctx,
		`INSERT INTO chat_threads(id, slug, repo, state) VALUES ('thread-1', 'release-room', 'owner/repo', 'open')`); err != nil {
		t.Fatalf("seed chat thread: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO chat_messages(id, thread_id, seq, kind, body) VALUES ('msg-1', 'thread-1', 1, 'chat', '@owner look')`); err != nil {
		t.Fatalf("seed chat message: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO chat_mentions(message_id, thread_id, agent) VALUES ('msg-1', 'thread-1', 'owner')`); err != nil {
		t.Fatalf("seed chat mention: %v", err)
	}
	for _, wake := range []struct{ kind, id, coalesce string }{
		{kind: "chat_message", id: "msg-1", coalesce: "reply:owner"},
		{kind: "workflow_note", id: "77", coalesce: "reply:owner"},
	} {
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key, state)
			VALUES (?, ?, 'owner', ?, 'pending')`, wake.kind, wake.id, wake.coalesce); err != nil {
			t.Fatalf("seed %s wake: %v", wake.kind, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous-release store: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade a database seeded with chat rows: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	// The tables and their indexes are gone.
	for _, name := range []string{"chat_meta", "chat_threads", "chat_messages", "chat_mentions", "chat_thread_meta"} {
		var count int
		if err := upgraded.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect table %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("table %s survived the removal migration", name)
		}
	}
	var indexCount int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_chat_%'`).Scan(&indexCount); err != nil {
		t.Fatalf("inspect chat indexes: %v", err)
	}
	if indexCount != 0 {
		t.Fatalf("chat index count = %d, want 0", indexCount)
	}

	// The unresolvable chat wake is gone; the workflow_note wake is untouched.
	rows, err := upgraded.db.QueryContext(ctx, `SELECT source_kind, source_id FROM wake_outbox ORDER BY id`)
	if err != nil {
		t.Fatalf("read wake_outbox: %v", err)
	}
	defer rows.Close()
	surviving := map[string]string{}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			t.Fatalf("scan wake row: %v", err)
		}
		surviving[kind] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate wake_outbox: %v", err)
	}
	if _, ok := surviving["chat_message"]; ok {
		t.Fatalf("chat_message wake row survived: %v", surviving)
	}
	if id := surviving["workflow_note"]; id != "77" {
		t.Fatalf("workflow_note wake row = %q, want the untouched control row 77 (surviving=%v)", id, surviving)
	}
}
