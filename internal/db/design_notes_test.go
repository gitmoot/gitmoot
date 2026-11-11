package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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
	for _, table := range []string{"design_note_filings", "design_note_issues", "design_note_pending_posts"} {
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
	pending, err := store.ListDesignNoteIssuesAwaitingNotice(ctx, now.Add(-time.Hour))
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
	if pending, err := store.ListDesignNoteIssuesAwaitingNotice(ctx, now.Add(-time.Hour)); err != nil || len(pending) != 1 {
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

// #2559 review round 2: a design-notes write gives up at its context's
// deadline under REAL writer contention, instead of waiting out the store's
// 15s busy_timeout (which the driver alone does not shorten for a context,
// see TestShortCallerBudgetDiscardsAWriteItAlreadyWon), and the connection's
// normal busy_timeout is restored afterwards.
func TestDesignNoteWritesBoundTheirLockWaitByTheDeadline(t *testing.T) {
	const hold = 4 * time.Second
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("second connection: %v", err)
	}
	defer raw.Close()
	blocker, err := raw.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker connection: %v", err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(hold)
		_, _ = blocker.ExecContext(context.Background(), `ROLLBACK`)
	}()

	writeCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = store.RecordDesignNoteFilings(writeCtx, []DesignNoteFiling{{FindingUID: "o/r#1-f1", Repo: "o/r", PullRequest: 1, IssueURL: "u"}}, time.Now())
	waited := time.Since(start)
	<-released
	if err == nil {
		t.Fatal("a write against a held lock succeeded")
	}
	if waited >= hold-time.Second {
		t.Fatalf("a 300ms write waited %s for a %s lock hold; the deadline did not bound the busy wait", waited, hold)
	}
	var busyTimeout int
	if err := store.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != sqliteBusyTimeoutMillis {
		t.Fatalf("busy_timeout after a bounded write = %d, want the store's %d restored", busyTimeout, sqliteBusyTimeoutMillis)
	}
	if err := store.RecordDesignNoteFilings(ctx, []DesignNoteFiling{{FindingUID: "o/r#1-f1", Repo: "o/r", PullRequest: 1, IssueURL: "u"}}, time.Now()); err != nil {
		t.Fatalf("write after the lock freed: %v", err)
	}
}

// #2559 review round 3, finding 1: the busy wait is sized from the time left
// AFTER the store's single pooled connection is acquired. Here another caller
// holds that connection for most of the budget while an external writer holds
// the write lock; the write must still return near its deadline.
func TestDesignNoteWriteBudgetCountsTheWaitForThePooledConnection(t *testing.T) {
	const (
		budget   = time.Second
		poolHold = 800 * time.Millisecond
		lockHold = 4 * time.Second
	)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("second connection: %v", err)
	}
	defer raw.Close()
	blocker, err := raw.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker connection: %v", err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	lockReleased := make(chan struct{})
	go func() {
		defer close(lockReleased)
		time.Sleep(lockHold)
		_, _ = blocker.ExecContext(context.Background(), `ROLLBACK`)
	}()
	pooled, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatalf("take the pooled connection: %v", err)
	}
	go func() {
		time.Sleep(poolHold)
		_ = pooled.Close()
	}()

	writeCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	start := time.Now()
	err = store.RecordDesignNoteFilings(writeCtx, []DesignNoteFiling{{FindingUID: "o/r#1-f1", Repo: "o/r", PullRequest: 1, IssueURL: "u"}}, time.Now())
	elapsed := time.Since(start)
	<-lockReleased
	if err == nil {
		t.Fatal("a write against a held lock succeeded")
	}
	if elapsed > budget+250*time.Millisecond {
		t.Fatalf("a %s write returned after %s: its busy wait was sized before the %s wait for the pooled connection", budget, elapsed, poolHold)
	}
}

// #2566: declared_state keeps the state a reviewer declared beside the state
// the ledger recorded. Upgrading backfills it from the writer's own
// findings_ledger_downgraded events, but only where the job's CURRENT result
// still says what the event says: RetryJob keeps a job's events and replaces
// its result, so finding[N] of an older lifecycle may name another finding
// now. Anything unproven stays empty (the note is filed). New rows record the
// declared state directly, and ListDesignNoteCandidates returns it.
func TestDeclaredStateMigrationBackfillsFromDowngradeEvents(t *testing.T) {
	ctx := context.Background()
	released := migrationsBefore(t, "ADD COLUMN declared_state")
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
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	for _, job := range []struct{ id, payload string }{
		// f1 cited by uid, f2 by the repo-less short form, f3 minted.
		{"review-1", `{"head_sha":"` + headA + `","result":{"findings":[{"uid":"o/r#1-f1","state":"answered"},{"continues_uid":"#1-f2","disposition":"withdrawn"},{"title":"new"}]}}`},
		{"review-2", `{"head_sha":"` + headA + `","result":{"findings":[{"uid":"o/r#1-f1","state":"answered"}]}}`},
		{"review-3", `not json`},
		// Retried: finding[0] was f1 (answered) in the first lifecycle; the
		// current result's finding[0] is f2, declared open.
		{"review-4", `{"head_sha":"` + headA + `","result":{"findings":[{"uid":"o/r#4-f2","state":"open"}]}}`},
		// Retried at a new head: f5 was answered at headA, re-declared open at headB.
		{"review-5", `{"head_sha":"` + headB + `","result":{"findings":[{"uid":"o/r#5-f5","state":"open"}]}}`},
		// Retried at the same head: the first lifecycle answered f6 as
		// finding[0] and reported f7 open; the retry answers f7 as finding[0],
		// whose row the first lifecycle already wrote (open).
		{"review-6", `{"head_sha":"` + headA + `","result":{"findings":[{"uid":"o/r#6-f7","state":"answered"}]}}`},
		// Now at headB, answering f8, which an earlier run observed at headA.
		{"review-7", `{"head_sha":"` + headB + `","result":{"findings":[{"uid":"o/r#7-f8","state":"answered"}]}}`},
	} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO jobs (id, agent, type, state, payload) VALUES (?, 'reviewer', 'review', 'succeeded', ?)`, job.id, job.payload); err != nil {
			t.Fatalf("insert job %s: %v", job.id, err)
		}
	}
	const quoted = " recorded open (evidence QUOTED)"
	for _, event := range []struct{ job, kind, message string }{
		{"review-1", "findings_ledger_downgraded", "finding[0] declared answered, recorded open (evidence QUOTED: no file or path-shaped evidence_locator, and the review declared no executed checks)"},
		{"review-1", "findings_ledger_downgraded", "finding[1] declared withdrawn, recorded open (withdrawal carried no withdraw_reason)"},
		{"review-3", "findings_ledger_downgraded", "finding[0] declared answered," + quoted},
		{"review-4", "findings_ledger_downgraded", "finding[0] declared answered," + quoted},
		{"review-5", "findings_ledger_downgraded", "finding[0] declared answered," + quoted},
		{"review-5", "retry_queued", "retry requested from failed for lifecycle_generation=2"},
		{"review-6", "findings_ledger_downgraded", "finding[0] declared answered," + quoted},
		{"review-6", "retry_queued", "retry requested from failed for lifecycle_generation=2"},
		{"review-7", "findings_ledger_downgraded", "finding[0] declared answered," + quoted},
	} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO job_events (job_id, kind, message) VALUES (?, ?, ?)`, event.job, event.kind, event.message); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	type row struct{ uid, job, head string }
	rows := []row{
		{"o/r#1-f1", "review-1", headA}, {"o/r#1-f2", "review-1", headA}, {"o/r#1-f3", "review-1", headA},
		{"o/r#1-f1", "review-2", headA}, {"o/r#1-f1", "review-3", headA},
		{"o/r#4-f1", "review-4", headA}, {"o/r#4-f2", "review-4", headA},
		{"o/r#5-f5", "review-5", headA}, {"o/r#5-f5", "review-5", headB},
		{"o/r#6-f6", "review-6", headA}, {"o/r#6-f7", "review-6", headA},
		{"o/r#7-f8", "review-7", headA}, {"o/r#7-f8", "review-7", headB},
	}
	for _, r := range rows {
		if _, err := raw.ExecContext(ctx, `INSERT INTO review_finding_observations (finding_uid, repo, pull_request, head_sha, observer_job, state, severity, title, evidence_kind)
VALUES (?, 'o/r', 1, ?, ?, 'open', 'P3', 'note', 'QUOTED')`, r.uid, r.head, r.job); err != nil {
			t.Fatalf("insert observation %+v: %v", r, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close previous store: %v", err)
	}

	store, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("upgrade through Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	want := map[row]string{
		{"o/r#1-f1", "review-1", headA}: "answered",
		{"o/r#1-f2", "review-1", headA}: "withdrawn",
		// Minted by its review: no cited uid to match.
		{"o/r#1-f3", "review-1", headA}: "",
		// review-2 emitted no downgrade event; review-3's payload is unreadable.
		{"o/r#1-f1", "review-2", headA}: "",
		{"o/r#1-f1", "review-3", headA}: "",
		// The current finding[0] is another finding, declared open.
		{"o/r#4-f1", "review-4", headA}: "",
		{"o/r#4-f2", "review-4", headA}: "",
		// The current finding[0] re-declares f5 open, at a new head.
		{"o/r#5-f5", "review-5", headA}: "",
		{"o/r#5-f5", "review-5", headB}: "",
		// The event is from before the retry: its finding[0] was f6, not f7.
		{"o/r#6-f6", "review-6", headA}: "",
		{"o/r#6-f7", "review-6", headA}: "",
		// Only the row at the current payload's head is the one it answered.
		{"o/r#7-f8", "review-7", headA}: "",
		{"o/r#7-f8", "review-7", headB}: "answered",
	}
	for r, declared := range want {
		var got string
		if err := store.db.QueryRowContext(ctx, `SELECT declared_state FROM review_finding_observations WHERE finding_uid = ? AND observer_job = ? AND head_sha = ?`, r.uid, r.job, r.head).Scan(&got); err != nil {
			t.Fatalf("read %+v: %v", r, err)
		}
		if got != declared {
			t.Fatalf("declared_state of %+v = %q, want %q", r, got, declared)
		}
	}
	head := headA

	uid, err := store.RecordReviewFindingObservation(ctx, ReviewFindingObservation{
		Repo: "o/r", PullRequest: 2, HeadSHA: head, State: FindingOpen, DeclaredState: FindingAnswered, Severity: "P3",
		Title: "said answered", File: "x.go", EvidenceKind: EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1, ObserverJob: "review-4",
	})
	if err != nil {
		t.Fatalf("RecordReviewFindingObservation: %v", err)
	}
	candidates, err := store.ListDesignNoteCandidates(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ListDesignNoteCandidates: %v", err)
	}
	var found bool
	for _, c := range candidates {
		if c.FindingUID == uid {
			found = true
			if c.DeclaredState != FindingAnswered {
				t.Fatalf("candidate %s declared state = %q, want answered", uid, c.DeclaredState)
			}
		}
	}
	if !found {
		t.Fatalf("candidates %+v lack %s", candidates, uid)
	}
}
