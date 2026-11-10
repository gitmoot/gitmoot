package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Design notes (#2558) are open P3 findings that a pull request merged without
// addressing. The weekly sweep in internal/workflow files them into one GitHub
// issue per repository per ISO week; this file is its ledger.

// DesignNoteSkipClosedUnmerged is the skip reason for a finding whose pull
// request closed without merging: there is nothing merged to follow up on.
const DesignNoteSkipClosedUnmerged = "closed-unmerged"

// DesignNoteSkipDeclaredResolved is the skip reason for a finding whose latest
// observation the reviewer declared answered or withdrawn, though the ledger
// recorded it open for want of evidence (#2566).
const DesignNoteSkipDeclaredResolved = "declared resolved by its reviewer"

// The skip reasons of a finding the default branch outdated after its pull
// request merged: the file it cites was removed or renamed away, or a later
// change rewrote the lines it cites.
const (
	DesignNoteSkipFileRemoved  = "cited file removed since merge"
	DesignNoteSkipLinesChanged = "cited lines changed since merge"
)

// DesignNoteCandidate is one finding the sweep may file: the LATEST observation
// of a finding whose first observation is inside the sweep window.
type DesignNoteCandidate struct {
	FindingUID  string
	Repo        string
	PullRequest int64
	HeadSHA     string
	ObservedAt  string
	ObserverJob string
	Title       string
	Detail      string
	File        string
	Line        int64
	// DeclaredState is the state the reviewer declared on the latest
	// observation (review_finding_observations.declared_state); "" on rows
	// recorded before #2566 that the migration could not backfill.
	DeclaredState FindingState
	// Seq is the latest observation's rowid: the ledger's insertion order,
	// comparable across findings.
	Seq int64
}

// ListDesignNoteCandidates returns every finding whose latest observation is an
// open P3, whose first observation is at or after since, and which has no
// design_note_filings row. Rows are ordered by repository, pull request and
// first observation.
//
// LATEST MEANS INSERTION ORDER (rowid), the ledger's authoritative order, as in
// ListReviewFindingObservations: observed_at is caller-supplied and must not
// decide which state is current. observed_at serves only the window, compared
// through julianday so a stamp with an offset orders correctly against the
// store's own UTC default.
func (s *Store) ListDesignNoteCandidates(ctx context.Context, since time.Time) ([]DesignNoteCandidate, error) {
	return queryList(ctx, s.db, `
WITH ranked AS (
	SELECT finding_uid, repo, pull_request, head_sha, observed_at, observer_job, state, severity,
		title, detail, file, line, declared_state, rowid AS seq,
		ROW_NUMBER() OVER (PARTITION BY finding_uid ORDER BY rowid DESC) AS latest_rank,
		MIN(julianday(observed_at)) OVER (PARTITION BY finding_uid) AS first_seen
	FROM review_finding_observations
)
SELECT finding_uid, repo, pull_request, head_sha, observed_at, observer_job, title, detail, file, line, declared_state, seq
FROM ranked
WHERE latest_rank = 1 AND state = 'open' AND UPPER(TRIM(severity)) = 'P3'
	AND first_seen >= julianday(?)
	AND TRIM(repo) <> '' AND pull_request > 0
	AND NOT EXISTS (SELECT 1 FROM design_note_filings d WHERE d.finding_uid = ranked.finding_uid)
ORDER BY repo, pull_request, first_seen, finding_uid`,
		[]any{since.UTC().Format(time.RFC3339Nano)},
		func(row rowScanner) (DesignNoteCandidate, error) {
			var c DesignNoteCandidate
			err := row.Scan(&c.FindingUID, &c.Repo, &c.PullRequest, &c.HeadSHA, &c.ObservedAt,
				&c.ObserverJob, &c.Title, &c.Detail, &c.File, &c.Line, &c.DeclaredState, &c.Seq)
			return c, err
		})
}

// DesignNoteFiling records that a finding was filed into IssueURL, or skipped
// for SkipReason. Exactly one of the two is set.
type DesignNoteFiling struct {
	FindingUID  string
	Repo        string
	PullRequest int64
	HeadSHA     string
	IssueURL    string
	SkipReason  string
}

// designNoteWriteFloor is the least time left before a deadline for which a
// bounded design-notes write is still attempted.
const designNoteWriteFloor = 10 * time.Millisecond

// designNoteWrite runs fn in one write transaction whose wait for SQLite's
// write lock is bounded by ctx's deadline (#2559 review round 2).
//
// A context does NOT shorten a contended write on its own: the driver ignores
// cancellation while it backs off for busy_timeout (15s here), see
// TestShortCallerBudgetDiscardsAWriteItAlreadyWon. So, like
// CheckpointWALTruncate, the write takes a dedicated connection and lowers
// that connection's busy_timeout to the time left before the deadline for the
// duration, then restores it; a connection whose restore fails is discarded so
// database/sql reopens it from the DSN with the normal pragmas. A ctx with no
// deadline keeps the store's normal wait.
//
// The budget is measured AFTER the connection is acquired: the pool holds one
// connection, so waiting for it can consume most of the deadline, and a busy
// wait sized before that wait would overrun the deadline by it (#2559 review
// round 3). With less than designNoteWriteFloor left, nothing is attempted.
func (s *Store) designNoteWrite(ctx context.Context, fn func(*sql.Tx) error) error {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if time.Until(deadline) < designNoteWriteFloor {
		return context.DeadlineExceeded
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	remaining := time.Until(deadline)
	if remaining < designNoteWriteFloor {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	waitMillis := (remaining + time.Millisecond - 1) / time.Millisecond
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout=%d`, waitMillis)); err != nil {
		return fmt.Errorf("bound busy wait: %w", err)
	}
	writeErr := func() error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA busy_timeout=%d`, sqliteBusyTimeoutMillis)); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return errors.Join(writeErr, fmt.Errorf("restore busy timeout (connection discarded): %w", err))
	}
	return writeErr
}

// RecordDesignNoteFilings writes the rows in one transaction, so a pull
// request's findings are marked together or not at all, and deletes the
// pending posts of exactly those findings in the same transaction. A finding
// that already has a row keeps it. The wait for the write lock is bounded by
// ctx's deadline.
func (s *Store) RecordDesignNoteFilings(ctx context.Context, filings []DesignNoteFiling, now time.Time) error {
	if len(filings) == 0 {
		return nil
	}
	if err := validateDesignNoteFilings(filings); err != nil {
		return err
	}
	return s.designNoteWrite(ctx, func(tx *sql.Tx) error {
		return insertDesignNoteFilingsTx(ctx, tx, filings, now)
	})
}

func validateDesignNoteFilings(filings []DesignNoteFiling) error {
	for _, f := range filings {
		uid := strings.TrimSpace(f.FindingUID)
		if uid == "" || (strings.TrimSpace(f.IssueURL) == "") == (strings.TrimSpace(f.SkipReason) == "") {
			return fmt.Errorf("design note filing %q needs a finding uid and exactly one of issue url or skip reason", uid)
		}
	}
	return nil
}

// insertDesignNoteFilingsTx records the filings and retires every pending post
// of each filed finding: a filed finding is never a candidate again.
func insertDesignNoteFilingsTx(ctx context.Context, tx *sql.Tx, filings []DesignNoteFiling, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, f := range filings {
		uid := strings.TrimSpace(f.FindingUID)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO design_note_filings (finding_uid, repo, pull_request, head_sha, issue_url, skip_reason, filed_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(finding_uid) DO NOTHING`,
			uid, f.Repo, f.PullRequest, f.HeadSHA, strings.TrimSpace(f.IssueURL), strings.TrimSpace(f.SkipReason), stamp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM design_note_pending_posts WHERE finding_uid = ?`, uid); err != nil {
			return err
		}
	}
	return nil
}

// DesignNotePendingPost is one UNRESOLVED attempt to post one finding to one
// issue: recorded BEFORE the comment is posted, so a comment whose outcome was
// lost is found on that issue again, even after the week and so the week's
// issue have changed. A row concerns only its own finding, so resolving one
// finding never retires another's attempt.
type DesignNotePendingPost struct {
	FindingUID  string
	IssueNumber int64
	Repo        string
	PullRequest int64
	IssueURL    string
}

// RecordDesignNotePendingPosts records one attempt per uid: the sweep may
// post each to issue for the pull request. Recording an attempt again is a
// no-op. The wait for the write lock is bounded by ctx's deadline.
func (s *Store) RecordDesignNotePendingPosts(ctx context.Context, repo string, pullRequest, issueNumber int64, issueURL string, uids []string, now time.Time) error {
	if strings.TrimSpace(repo) == "" || pullRequest <= 0 || issueNumber <= 0 || strings.TrimSpace(issueURL) == "" || len(uids) == 0 {
		return fmt.Errorf("design note pending posts need a repo, pull request, issue number, url and findings")
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	return s.designNoteWrite(ctx, func(tx *sql.Tx) error {
		for _, uid := range uids {
			if strings.TrimSpace(uid) == "" {
				return fmt.Errorf("design note pending post needs a finding uid")
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO design_note_pending_posts (finding_uid, issue_number, repo, pull_request, issue_url, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(finding_uid, issue_number) DO NOTHING`,
				strings.TrimSpace(uid), issueNumber, repo, pullRequest, issueURL, stamp); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordDesignNoteReadBack records the outcome of reading issueNumber back: the
// findings whose marker it carries are filed (which retires all their
// attempts), and the attempts on this issue of the absent uids are retired,
// since their comment demonstrably did not land there. One transaction. The
// wait for the write lock is bounded by ctx's deadline.
func (s *Store) RecordDesignNoteReadBack(ctx context.Context, issueNumber int64, found []DesignNoteFiling, absent []string, now time.Time) error {
	if len(found) == 0 && len(absent) == 0 {
		return nil
	}
	if err := validateDesignNoteFilings(found); err != nil {
		return err
	}
	return s.designNoteWrite(ctx, func(tx *sql.Tx) error {
		if err := insertDesignNoteFilingsTx(ctx, tx, found, now); err != nil {
			return err
		}
		for _, uid := range absent {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM design_note_pending_posts WHERE finding_uid = ? AND issue_number = ?`, uid, issueNumber); err != nil {
				return err
			}
		}
		return nil
	})
}

// RetireAgedDesignNotePendingPosts deletes the attempts whose finding can never
// be filed again: it has a filing row, or was first observed before since
// (outside the sweep window). An attempt whose finding is unfiled and inside
// the window is kept even when the finding is not a candidate now (answered,
// for example), because it may reopen and only a read-back can tell whether
// its comment landed. The wait for the write lock is bounded by ctx's deadline.
func (s *Store) RetireAgedDesignNotePendingPosts(ctx context.Context, since time.Time) error {
	return s.designNoteWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
DELETE FROM design_note_pending_posts
WHERE EXISTS (SELECT 1 FROM design_note_filings f WHERE f.finding_uid = design_note_pending_posts.finding_uid)
	OR COALESCE((SELECT MIN(julianday(o.observed_at)) FROM review_finding_observations o
		WHERE o.finding_uid = design_note_pending_posts.finding_uid), 0) < julianday(?)`,
			since.UTC().Format(time.RFC3339Nano))
		return err
	})
}

// ListDesignNotePendingPosts returns the pull request's unresolved attempts,
// one per finding per issue, oldest first.
func (s *Store) ListDesignNotePendingPosts(ctx context.Context, repo string, pullRequest int64) ([]DesignNotePendingPost, error) {
	return queryList(ctx, s.db, `
SELECT finding_uid, issue_number, repo, pull_request, issue_url FROM design_note_pending_posts
WHERE repo = ? AND pull_request = ? ORDER BY created_at, issue_number, finding_uid`,
		[]any{repo, pullRequest},
		func(row rowScanner) (DesignNotePendingPost, error) {
			var p DesignNotePendingPost
			err := row.Scan(&p.FindingUID, &p.IssueNumber, &p.Repo, &p.PullRequest, &p.IssueURL)
			return p, err
		})
}

// AddDesignNoteJobEvent records a design-notes diagnostic on a job, with the
// wait for the write lock bounded by ctx's deadline.
func (s *Store) AddDesignNoteJobEvent(ctx context.Context, event JobEvent) error {
	return s.designNoteWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
			event.JobID, event.Kind, event.Message)
		return err
	})
}

// DesignNoteIssue is one repository's design-notes issue for one ISO week.
// Created is true when the sweep opened the issue itself; such an issue owes
// its Role one notice, claimed by setting NotifiedAt.
type DesignNoteIssue struct {
	Repo       string
	ISOWeek    string
	Number     int64
	URL        string
	Role       string
	Created    bool
	NotifiedAt string
}

const designNoteIssueColumns = `repo, iso_week, issue_number, issue_url, role, created, notified_at`

func scanDesignNoteIssue(row rowScanner) (DesignNoteIssue, error) {
	var issue DesignNoteIssue
	var created int
	err := row.Scan(&issue.Repo, &issue.ISOWeek, &issue.Number, &issue.URL, &issue.Role, &created, &issue.NotifiedAt)
	issue.Created = created == 1
	return issue, err
}

// GetDesignNoteIssue returns the recorded issue for repo and week; ok is false
// when none is recorded.
func (s *Store) GetDesignNoteIssue(ctx context.Context, repo, week string) (DesignNoteIssue, bool, error) {
	issue, err := scanDesignNoteIssue(s.db.QueryRowContext(ctx,
		`SELECT `+designNoteIssueColumns+` FROM design_note_issues WHERE repo = ? AND iso_week = ?`, repo, week))
	if errors.Is(err, sql.ErrNoRows) {
		return DesignNoteIssue{}, false, nil
	}
	if err != nil {
		return DesignNoteIssue{}, false, err
	}
	return issue, true, nil
}

// RecordDesignNoteIssue stores the week's issue unless one is already recorded,
// and returns the stored row, so a concurrent writer's issue wins consistently.
// The wait for the write lock is bounded by ctx's deadline.
func (s *Store) RecordDesignNoteIssue(ctx context.Context, issue DesignNoteIssue, now time.Time) (DesignNoteIssue, error) {
	if strings.TrimSpace(issue.Repo) == "" || strings.TrimSpace(issue.ISOWeek) == "" || issue.Number <= 0 || strings.TrimSpace(issue.URL) == "" {
		return DesignNoteIssue{}, fmt.Errorf("design note issue needs a repo, week, number and url")
	}
	created := 0
	if issue.Created {
		created = 1
	}
	if err := s.designNoteWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO design_note_issues (repo, iso_week, issue_number, issue_url, role, created, notified_at, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, '', ?)
ON CONFLICT(repo, iso_week) DO NOTHING`,
			issue.Repo, issue.ISOWeek, issue.Number, issue.URL, issue.Role, created, now.UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		return DesignNoteIssue{}, err
	}
	stored, ok, err := s.GetDesignNoteIssue(ctx, issue.Repo, issue.ISOWeek)
	if err != nil {
		return DesignNoteIssue{}, err
	}
	if !ok {
		return DesignNoteIssue{}, fmt.Errorf("design note issue for %s %s vanished after insert", issue.Repo, issue.ISOWeek)
	}
	return stored, nil
}

// ListDesignNoteIssuesAwaitingNotice returns the week's issues the sweep
// created whose role notice is not yet sent.
func (s *Store) ListDesignNoteIssuesAwaitingNotice(ctx context.Context, week string) ([]DesignNoteIssue, error) {
	return queryList(ctx, s.db,
		`SELECT `+designNoteIssueColumns+` FROM design_note_issues WHERE iso_week = ? AND created = 1 AND role <> '' AND notified_at = '' ORDER BY repo`,
		[]any{week}, scanDesignNoteIssue)
}

// SendDesignNoteIssueNotice sends the system message body to the role of the
// issue recorded for repo and week, and marks the issue notified, in ONE
// transaction: either both writes land or neither does, so a role hears at
// most once per repository per week and a failed send stays pending. It
// reports whether the message was sent; false means the issue was not created
// by the sweep, has no role, or was already notified. The wait for the write
// lock is bounded by ctx's deadline.
func (s *Store) SendDesignNoteIssueNotice(ctx context.Context, repo, week, body string, now time.Time) (bool, error) {
	if strings.TrimSpace(body) == "" {
		return false, fmt.Errorf("design note notice needs a body")
	}
	errNothingToSend := errors.New("no design note notice owed")
	err := s.designNoteWrite(ctx, func(tx *sql.Tx) error {
		var role string
		err := tx.QueryRowContext(ctx,
			`UPDATE design_note_issues SET notified_at = ? WHERE repo = ? AND iso_week = ? AND created = 1 AND role <> '' AND notified_at = '' RETURNING role`,
			now.UTC().Format(time.RFC3339Nano), repo, week).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return errNothingToSend
		}
		if err != nil {
			return err
		}
		role = strings.TrimSpace(role)
		if role == MessageSystemSender {
			return fmt.Errorf("design note notice needs a real recipient")
		}
		message := Message{Sender: MessageSystemSender, Recipient: role, Body: body, Kind: "message", SourceKind: WakeOutboxSourceWorkflowNote}
		_, err = insertWorkflowNoteTx(ctx, tx, WorkflowNote{
			Author:          MessageSystemSender,
			Body:            fmt.Sprintf("[message from=%s to=%s]\n%s", MessageSystemSender, role, body),
			AddressedTarget: role, Inbox: &message,
		})
		return err
	})
	if errors.Is(err, errNothingToSend) {
		return false, nil
	}
	return err == nil, err
}
