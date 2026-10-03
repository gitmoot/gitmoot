package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMessageInboxUpgradeIsAtomicAndNeverReplaysHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "historical.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	previous := &Store{db: raw}
	released := migrationsBefore(t, "CREATE TABLE messages")
	released = append(released, migrations[len(released)])
	for version, migration := range released {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatal(err)
		}
	}
	_, err = raw.Exec(`
 INSERT INTO workflow_notes(id,workflow_id,author,body,created_at) VALUES
 (10,'historic','owner','[org:directive to=worker from=owner wf=historic] Keep this pending.','2026-09-15 12:00:00'),
 (20,'historic','worker','[org:message from=worker to=owner wf=historic] Submission uncertain.','2026-09-15 12:01:00'),
 (30,'historic','legacy','[org:escalate to=owner wf=historic] Requester unknown.','2026-09-15 12:02:00'),
 (40,'historic','owner','[org:directive to=worker from=owner wf=historic] Completed work.','2026-09-15 12:03:00'),
 (41,'historic','worker','[org:directive-done id=40 by=worker] completed','2026-09-15 12:04:00'),
 (50,'historic','worker','[org:escalate to=owner from=worker wf=historic] Old decision.','2026-09-15 12:05:00'),
 (51,'historic','owner','[org:escalate-resolved id=50 by=owner] resolved','2026-09-15 12:06:00'),
 (52,'unrelated','owner','[org:escalate-resolved id=30 by=owner] unrelated resolution','2026-09-15 12:07:00'),
 (60,'historic','worker','[org:message from=worker to=worker wf=historic] Self-addressed journal.','2026-09-15 12:08:00');
 INSERT INTO wake_outbox(source_kind,source_id,target_role,coalesce_key,state,attempt_count,last_error) VALUES
 ('workflow_note','10','worker','directive:worker','pending',0,''),
 ('workflow_note','20','owner','reply:owner','delivery_unknown',1,'PTY write not confirmed'),
 ('workflow_note','40','worker','directive:worker','delivered',1,''),
 ('workflow_note','50','owner','reply:owner','delivered',1,''),
 ('workflow_note','51','worker','reply:worker','delivery_unknown',1,'old uncertain resolution'),
 ('workflow_note','60','worker','reply:worker','delivered',1,'');
 CREATE TRIGGER fail_inbox_upgrade BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'injected inbox migration failure'); END;
 `)
	if err != nil {
		t.Fatal(err)
	}
	before, err := previous.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := openRealTestStore(t, path); err == nil {
		store.Close()
		t.Fatal("injected migration failure unexpectedly succeeded")
	}
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version, columns int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='kind'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if version != len(released) || columns != 0 {
		t.Fatalf("partially committed migration: version=%d columns=%d", version, columns)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_inbox_upgrade`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		id, thread                          int64
		role, kind, status, lifecycle, body string
	}{
		{10, 10, "worker", "directive", "queued", "pending", "Keep this pending."},
		{20, 20, "owner", "message", "uncertain", "", "Submission uncertain."},
		{30, 30, "owner", "escalation", "not_notified", "open", "Requester unknown."},
		{40, 40, "worker", "directive", "submitted", "completed", "Completed work."},
		{41, 40, "owner", "directive_receipt", "not_notified", "completed", "[org:directive-done id=40 by=worker] completed"},
		{50, 50, "owner", "escalation", "submitted", "resolved", "Old decision."},
		{51, 50, "worker", "escalation_receipt", "uncertain", "resolved", "[org:escalate-resolved id=50 by=owner] resolved"},
		{60, 60, "worker", "notification", "submitted", "", "[org:message from=worker to=worker wf=historic] Self-addressed journal."},
	} {
		stored, err := upgraded.GetMessage(ctx, want.id, want.role)
		if err != nil {
			t.Fatal(err)
		}
		var message struct {
			Message
			Kind       string `json:"kind"`
			Lifecycle  string `json:"lifecycle"`
			Historical bool   `json:"historical"`
		}
		wire, err := json.Marshal(stored)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(wire, &message); err != nil {
			t.Fatal(err)
		}
		if message.ID != want.id || message.ThreadID != want.thread || message.Kind != want.kind || message.Status != want.status || message.Lifecycle != want.lifecycle || message.Body != want.body || !message.Historical {
			t.Fatalf("historical %d = %+v", want.id, message)
		}
	}
	after, err := upgraded.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("migration changed delivery evidence: before=%+v after=%+v", before, after)
	}
	inboxSnapshot := func(store *Store) []Message {
		t.Helper()
		var messages []Message
		for _, role := range []string{"owner", "worker"} {
			inbox, err := store.ListMessages(ctx, role, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			messages = append(messages, inbox...)
		}
		return messages
	}
	inboxBefore := inboxSnapshot(upgraded)
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	inboxAfter := inboxSnapshot(reopened)
	if !reflect.DeepEqual(inboxBefore, inboxAfter) {
		t.Fatalf("reopen rewrote historical identities: before=%+v after=%+v", inboxBefore, inboxAfter)
	}
}
