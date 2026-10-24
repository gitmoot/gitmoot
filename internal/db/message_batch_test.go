package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoalescedMessagesReportActualBatchOutcome(t *testing.T) {
	for _, outcome := range []struct{ state, status string }{
		{WakeOutboxStateDelivered, "submitted"},
		{WakeOutboxStateDeliveryUnknown, "uncertain"},
		{WakeOutboxStatePending, "queued"},
	} {
		t.Run(outcome.state, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			var messages []Message
			for _, body := range []string{"first request", "second request"} {
				m, err := store.CreateMessage(ctx, Message{Sender: "sender", Recipient: "worker", Body: body})
				if err != nil {
					t.Fatal(err)
				}
				messages = append(messages, m)
			}
			rows, err := store.ListWakeOutbox(ctx, WakeOutboxStatePending)
			if err != nil || len(rows) != 2 {
				t.Fatalf("seed: rows=%v err=%v", rows, err)
			}
			ids := []int64{rows[0].ID, rows[1].ID}
			now := time.Now().UTC()
			if claimed, err := store.ClaimWakeOutbox(ctx, ids[0], ids[1:], now); err != nil || !claimed {
				t.Fatalf("claim: %t %v", claimed, err)
			}
			if outcome.state == WakeOutboxStateDelivered {
				err = store.FinishWakeOutbox(ctx, ids, WakeOutboxStateDelivered, "", now)
			} else {
				_, err = store.ExpireAgedWakeOutbox(ctx, now.Add(time.Second), now.Add(time.Minute))
			}
			if err != nil {
				t.Fatal(err)
			}
			if outcome.state == WakeOutboxStatePending {
				// A surviving obligation can be explicitly revived. Its members
				// must follow its current state, not a cached successful outcome.
				if _, err := store.db.Exec(`UPDATE wake_outbox SET state='pending',last_error='' WHERE id=?`, ids[0]); err != nil {
					t.Fatal(err)
				}
			}
			for _, original := range messages {
				got, err := store.GetMessage(ctx, original.ID, "worker")
				if err != nil || got.Status != outcome.status || got.Body != original.Body {
					t.Fatalf("message %d: %+v err=%v want=%s", original.ID, got, err, outcome.status)
				}
				if outcome.state == WakeOutboxStateDeliveryUnknown && !strings.Contains(got.NotificationReason, "unknown") {
					t.Fatalf("uncertainty reason lost: %+v", got)
				}
			}
			inbox, err := store.ListMessages(ctx, "worker", 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			dashboard, err := store.ListDashboardMessages(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for surface, entries := range map[string][]Message{"inbox": inbox, "dashboard": dashboard} {
				if len(entries) != 2 {
					t.Fatalf("%s omitted a batch member: %+v", surface, entries)
				}
				for _, m := range entries {
					if m.Status != outcome.status {
						t.Fatalf("%s status=%s want=%s", surface, m.Status, outcome.status)
					}
				}
			}
			rows, err = store.ListWakeOutbox(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.AttemptCount != 1 {
					t.Fatalf("reading status retried notification: %+v", row)
				}
			}
		})
	}
}

func TestCoalescedMessageRejectsBrokenAndCyclicLinks(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	first, err := store.CreateMessage(ctx, Message{Sender: "sender", Recipient: "worker", Body: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateMessage(ctx, Message{Sender: "sender", Recipient: "worker", Body: "second"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListWakeOutbox(ctx, "")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	a, b := rows[0].ID, rows[1].ID
	if _, err := store.db.Exec(`UPDATE wake_outbox SET state='superseded',coalesced_into=CASE id WHEN ? THEN ? ELSE ? END,last_error='batch'`, a, b, a); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		m, err := store.GetMessage(ctx, id, "worker")
		if err != nil || m.Status != "uncertain" {
			t.Fatalf("cycle: %+v %v", m, err)
		}
	}
	if _, err := store.db.Exec(`UPDATE wake_outbox SET state='delivered',coalesced_into=NULL,target_role='unrelated' WHERE id=?`, b); err != nil {
		t.Fatal(err)
	}
	m, err := store.GetMessage(ctx, first.ID, "worker")
	if err != nil || m.Status != "uncertain" {
		t.Fatalf("cross-role link: %+v %v", m, err)
	}
	if _, err := store.db.Exec(`DELETE FROM wake_outbox WHERE id=?`, b); err != nil {
		t.Fatal(err)
	}
	m, err = store.GetMessage(ctx, first.ID, "worker")
	if err != nil || m.Status != "uncertain" {
		t.Fatalf("missing link: %+v %v", m, err)
	}
	if _, err := store.db.Exec(`UPDATE wake_outbox SET coalesced_into=NULL,last_error='operator cancellation' WHERE id=?`, a); err != nil {
		t.Fatal(err)
	}
	m, err = store.GetMessage(ctx, first.ID, "worker")
	if err != nil || m.Status != "resolved" || m.NotificationReason != "operator cancellation" {
		t.Fatalf("cancellation: %+v %v", m, err)
	}
}

func TestCoalescedLinkMigrationPreservesHistoricalOutcomes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	previous := &Store{db: raw}
	// If the migration is absent, seed the old program's complete schema so
	// regression proof reaches stored outcomes rather than failing setup.
	released := migrations
	for index, migration := range migrations {
		if strings.Contains(migration, "ADD COLUMN coalesced_into") {
			released = migrations[:index]
			break
		}
	}
	for version, migration := range released {
		if err := previous.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatal(err)
		}
	}
	_, err = raw.Exec(`INSERT INTO wake_outbox(id,source_kind,source_id,target_role,coalesce_key,state,attempt_count,last_error) VALUES
	(1,'workflow_note','1','worker','reply:worker','delivery_unknown',1,'unconfirmed'),
	(2,'workflow_note','2','worker','reply:worker','superseded',1,'coalesced into wake outbox row 1'),
	(3,'workflow_note','3','worker','reply:worker','superseded',1,'coalesced into wake outbox row 1garbage'),
	(4,'workflow_note','4','worker','reply:worker','superseded',1,'coalesced into wake outbox row 5'),
	(5,'workflow_note','5','worker','reply:worker','superseded',1,'coalesced into wake outbox row 4'),
	(6,'workflow_note','6','other','reply:worker','superseded',1,'coalesced into wake outbox row 1');`)
	if err != nil {
		t.Fatal(err)
	}
	before, err := previous.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := len(released); i < len(migrations); i++ {
		if err := previous.applyMigration(ctx, i+1, migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	after, err := previous.ListWakeOutbox(ctx, "")
	if err != nil || len(after) != len(before) {
		t.Fatalf("history changed: %+v %v", after, err)
	}
	for i, row := range after {
		if row != before[i] {
			t.Fatalf("migration rewrote delivery evidence: before=%+v after=%+v", before[i], row)
		}
		var parent sql.NullInt64
		if err := raw.QueryRow(`SELECT coalesced_into FROM wake_outbox WHERE id=?`, row.ID).Scan(&parent); err != nil {
			t.Fatal(err)
		}
		if row.ID == 2 {
			if !parent.Valid || parent.Int64 != 1 {
				t.Fatalf("valid uncertain link omitted: %v", parent)
			}
		} else if parent.Valid {
			t.Fatalf("invalid history linked: row=%d parent=%v", row.ID, parent)
		}
	}
}
