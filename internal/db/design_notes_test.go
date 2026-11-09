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
	if won, err := store.ClaimDesignNoteIssueNotice(ctx, "o/r", "2026-W46", now); err != nil || !won {
		t.Fatalf("first claim = %v, %v", won, err)
	}
	if won, err := store.ClaimDesignNoteIssueNotice(ctx, "o/r", "2026-W46", now); err != nil || won {
		t.Fatalf("second claim = %v, %v; a notice is claimed once", won, err)
	}
	if err := store.ReleaseDesignNoteIssueNotice(ctx, "o/r", "2026-W46"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if won, err := store.ClaimDesignNoteIssueNotice(ctx, "o/r", "2026-W46", now); err != nil || !won {
		t.Fatalf("claim after release = %v, %v", won, err)
	}
}
