package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The design-notes migration (#2558) upgrades a database at the previous
// schema through the real Open path, and the upgraded tables enforce the
// exactly-once contract.
func TestDesignNotesMigrationUpgradesThroughOpen(t *testing.T) {
	ctx := context.Background()
	released := migrationsBefore(t, "CREATE TABLE IF NOT EXISTS design_note_filings")
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
	if exists, err := previous.HasTable(ctx, "design_note_filings"); err != nil || exists {
		t.Fatalf("design_note_filings before the migration: exists=%v err=%v", exists, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous store: %v", err)
	}

	store, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade through Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, table := range []string{"design_note_filings", "design_note_issues"} {
		if exists, err := store.HasTable(ctx, table); err != nil || !exists {
			t.Fatalf("%s after upgrade: exists=%v err=%v", table, exists, err)
		}
	}

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	if err := store.RecordDesignNoteFilings(ctx, []DesignNoteFiling{{FindingUID: "o/r#1-f1", Repo: "o/r", PullRequest: 1, IssueURL: "u", SkipReason: "x"}}, now); err == nil {
		t.Fatal("a filing with both an issue url and a skip reason was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO design_note_filings (finding_uid, repo, pull_request, filed_at) VALUES ('o/r#1-f9', 'o/r', 1, 'now')`); err == nil {
		t.Fatal("the table CHECK admitted a filing with neither an issue url nor a skip reason")
	}
	first := []DesignNoteFiling{{FindingUID: "o/r#1-f1", Repo: "o/r", PullRequest: 1, HeadSHA: "h", IssueURL: "https://github.com/o/r/issues/5"}}
	if err := store.RecordDesignNoteFilings(ctx, first, now); err != nil {
		t.Fatalf("RecordDesignNoteFilings: %v", err)
	}
	if err := store.RecordDesignNoteFilings(ctx, []DesignNoteFiling{{FindingUID: "o/r#1-f1", Repo: "o/r", PullRequest: 1, SkipReason: DesignNoteSkipClosedUnmerged}}, now); err != nil {
		t.Fatalf("re-recording a filed finding: %v", err)
	}
	var url, skip string
	if err := store.db.QueryRowContext(ctx, `SELECT issue_url, skip_reason FROM design_note_filings WHERE finding_uid = 'o/r#1-f1'`).Scan(&url, &skip); err != nil {
		t.Fatalf("read filing: %v", err)
	}
	if url != "https://github.com/o/r/issues/5" || skip != "" {
		t.Fatalf("filing = (%q, %q), want the first record kept", url, skip)
	}

	issue, err := store.RecordDesignNoteIssue(ctx, DesignNoteIssue{Repo: "o/r", ISOWeek: "2026-W46", Number: 5, URL: "https://github.com/o/r/issues/5", Role: "lead", Created: true}, now)
	if err != nil || issue.Number != 5 || !issue.Created {
		t.Fatalf("RecordDesignNoteIssue = %+v, %v", issue, err)
	}
	if again, err := store.RecordDesignNoteIssue(ctx, DesignNoteIssue{Repo: "o/r", ISOWeek: "2026-W46", Number: 6, URL: "https://github.com/o/r/issues/6"}, now); err != nil || again.Number != 5 {
		t.Fatalf("second issue for the week = %+v, %v; want the recorded #5 kept", again, err)
	}
	pending, err := store.ListDesignNoteIssuesAwaitingNotice(ctx, "2026-W46")
	if err != nil || len(pending) != 1 {
		t.Fatalf("awaiting notice = %+v, %v", pending, err)
	}
}

// The role notice and the notified mark are ONE transaction (#2559 review 6):
// a message write that fails leaves the issue un-notified, so a later sweep
// sends it; once sent, it is never sent again.
func TestDesignNoteIssueNoticeIsAtomicWithItsMessage(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	if _, err := store.RecordDesignNoteIssue(ctx, DesignNoteIssue{Repo: "o/r", ISOWeek: "2026-W46", Number: 5, URL: "https://github.com/o/r/issues/5", Role: "lead", Created: true}, now); err != nil {
		t.Fatalf("RecordDesignNoteIssue: %v", err)
	}
	countLead := func() int {
		t.Helper()
		messages, err := store.ListMessages(ctx, "lead", 0, 100)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		return len(messages)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER design_note_notice_crash BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'crash between the writes'); END`); err != nil {
		t.Fatalf("install failing trigger: %v", err)
	}
	if sent, err := store.SendDesignNoteIssueNotice(ctx, "o/r", "2026-W46", "notice", now); err == nil || sent {
		t.Fatalf("send with a failing message write = %v, %v; want an error", sent, err)
	}
	if issue, _, err := store.GetDesignNoteIssue(ctx, "o/r", "2026-W46"); err != nil || issue.NotifiedAt != "" {
		t.Fatalf("after a failed send the issue is marked notified (%q, %v); the mark must roll back with the message", issue.NotifiedAt, err)
	}
	if n := countLead(); n != 0 {
		t.Fatalf("lead has %d messages after a failed send", n)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER design_note_notice_crash`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if pending, err := store.ListDesignNoteIssuesAwaitingNotice(ctx, "2026-W46"); err != nil || len(pending) != 1 {
		t.Fatalf("awaiting notice = %+v, %v", pending, err)
	}
	if sent, err := store.SendDesignNoteIssueNotice(ctx, "o/r", "2026-W46", "notice", now); err != nil || !sent {
		t.Fatalf("retried send = %v, %v", sent, err)
	}
	if sent, err := store.SendDesignNoteIssueNotice(ctx, "o/r", "2026-W46", "notice", now); err != nil || sent {
		t.Fatalf("second send = %v, %v; a notice is sent once", sent, err)
	}
	if n := countLead(); n != 1 {
		t.Fatalf("lead has %d messages, want exactly 1", n)
	}
	if issue, _, _ := store.GetDesignNoteIssue(ctx, "o/r", "2026-W46"); issue.NotifiedAt == "" {
		t.Fatal("a sent notice left the issue un-notified")
	}
}
