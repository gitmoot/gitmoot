package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The deferral-notice migration (#2574) upgrades a database at the previous
// schema through the real Open path, leaves existing rows un-noticed, and the
// upgraded column makes the sender notice exactly-once per wake row.
func TestMessageDeferralNoticeMigrationUpgradesThroughOpen(t *testing.T) {
	ctx := context.Background()
	released := migrationsBefore(t, "ADD COLUMN deferral_notice_at")
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
	if _, err := raw.ExecContext(ctx, `INSERT INTO wake_outbox (source_kind, source_id, target_role, coalesce_key) VALUES ('workflow_note', '999', 'worker', 'reply:worker')`); err != nil {
		t.Fatalf("seed previous wake row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous store: %v", err)
	}

	store, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade through Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var seeded string
	if err := store.db.QueryRowContext(ctx, `SELECT deferral_notice_at FROM wake_outbox WHERE source_id = '999'`).Scan(&seeded); err != nil || seeded != "" {
		t.Fatalf("existing row after upgrade: deferral_notice_at=%q err=%v, want un-noticed", seeded, err)
	}

	message, err := store.CreateMessage(ctx, Message{Sender: "owner", Recipient: "worker", Body: "please rebase"})
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	var wakeID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM wake_outbox WHERE source_kind = 'workflow_note' AND source_id = ? AND target_role = 'worker'`, message.ID).Scan(&wakeID); err != nil {
		t.Fatalf("message wake row: %v", err)
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if sent, err := store.SendMessageDeferralNotice(ctx, wakeID, "intruder", "notice", now); err != nil || sent {
		t.Fatalf("notice to a non-sender = %v, %v; want nothing sent", sent, err)
	}
	if sent, err := store.SendMessageDeferralNotice(ctx, wakeID, "owner", "notice", now); err != nil || !sent {
		t.Fatalf("first notice = %v, %v; want sent", sent, err)
	}
	if sent, err := store.SendMessageDeferralNotice(ctx, wakeID, "owner", "notice", now.Add(time.Minute)); err != nil || sent {
		t.Fatalf("second notice = %v, %v; want nothing sent", sent, err)
	}
	inbox, err := store.ListMessages(ctx, "owner", 0, 10)
	if err != nil || len(inbox) != 1 || inbox[0].Sender != MessageSystemSender || inbox[0].Body != "notice" {
		t.Fatalf("owner inbox = %+v, %v; want the one system notice", inbox, err)
	}
	var stamped string
	if err := store.db.QueryRowContext(ctx, `SELECT deferral_notice_at FROM wake_outbox WHERE id = ?`, wakeID).Scan(&stamped); err != nil || stamped != now.Format(time.RFC3339Nano) {
		t.Fatalf("deferral_notice_at = %q, %v; want %q", stamped, err, now.Format(time.RFC3339Nano))
	}
}
