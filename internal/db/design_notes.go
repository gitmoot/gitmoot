package db

import (
	"context"
	"database/sql"
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
}

// ListDesignNoteCandidates returns every finding whose latest observation (by
// observed_at, then insertion order) is an open P3, whose first observation is
// at or after since, and which has no design_note_filings row. Rows are ordered
// by repository, pull request and first observation.
//
// observed_at is compared through julianday so a caller-supplied RFC3339 stamp
// with an offset orders correctly against the store's own UTC default.
func (s *Store) ListDesignNoteCandidates(ctx context.Context, since time.Time) ([]DesignNoteCandidate, error) {
	return queryList(ctx, s.db, `
WITH ranked AS (
	SELECT finding_uid, repo, pull_request, head_sha, observed_at, observer_job, state, severity,
		title, detail, file, line,
		ROW_NUMBER() OVER (PARTITION BY finding_uid ORDER BY julianday(observed_at) DESC, rowid DESC) AS latest_rank,
		MIN(julianday(observed_at)) OVER (PARTITION BY finding_uid) AS first_seen
	FROM review_finding_observations
)
SELECT finding_uid, repo, pull_request, head_sha, observed_at, observer_job, title, detail, file, line
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
				&c.ObserverJob, &c.Title, &c.Detail, &c.File, &c.Line)
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

// RecordDesignNoteFilings writes the rows in one transaction, so a pull
// request's findings are marked together or not at all. A finding that already
// has a row keeps it.
func (s *Store) RecordDesignNoteFilings(ctx context.Context, filings []DesignNoteFiling, now time.Time) error {
	if len(filings) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, f := range filings {
		uid := strings.TrimSpace(f.FindingUID)
		if uid == "" || (strings.TrimSpace(f.IssueURL) == "") == (strings.TrimSpace(f.SkipReason) == "") {
			return fmt.Errorf("design note filing %q needs a finding uid and exactly one of issue url or skip reason", uid)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO design_note_filings (finding_uid, repo, pull_request, head_sha, issue_url, skip_reason, filed_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(finding_uid) DO NOTHING`,
			uid, f.Repo, f.PullRequest, f.HeadSHA, strings.TrimSpace(f.IssueURL), strings.TrimSpace(f.SkipReason), stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
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
func (s *Store) RecordDesignNoteIssue(ctx context.Context, issue DesignNoteIssue, now time.Time) (DesignNoteIssue, error) {
	if strings.TrimSpace(issue.Repo) == "" || strings.TrimSpace(issue.ISOWeek) == "" || issue.Number <= 0 || strings.TrimSpace(issue.URL) == "" {
		return DesignNoteIssue{}, fmt.Errorf("design note issue needs a repo, week, number and url")
	}
	created := 0
	if issue.Created {
		created = 1
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO design_note_issues (repo, iso_week, issue_number, issue_url, role, created, notified_at, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, '', ?)
ON CONFLICT(repo, iso_week) DO NOTHING`,
		issue.Repo, issue.ISOWeek, issue.Number, issue.URL, issue.Role, created, now.UTC().Format(time.RFC3339Nano)); err != nil {
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
// created whose role notice is not yet claimed.
func (s *Store) ListDesignNoteIssuesAwaitingNotice(ctx context.Context, week string) ([]DesignNoteIssue, error) {
	return queryList(ctx, s.db,
		`SELECT `+designNoteIssueColumns+` FROM design_note_issues WHERE iso_week = ? AND created = 1 AND role <> '' AND notified_at = '' ORDER BY repo`,
		[]any{week}, scanDesignNoteIssue)
}

// ClaimDesignNoteIssueNotice marks the issue's role notice as sent and reports
// whether this caller won the claim. Claiming BEFORE sending makes the notice
// at most once per repository per week.
func (s *Store) ClaimDesignNoteIssueNotice(ctx context.Context, repo, week string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE design_note_issues SET notified_at = ? WHERE repo = ? AND iso_week = ? AND created = 1 AND notified_at = ''`,
		now.UTC().Format(time.RFC3339Nano), repo, week)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ReleaseDesignNoteIssueNotice undoes a claim whose notice could not be sent,
// so the next sweep retries it.
func (s *Store) ReleaseDesignNoteIssueNotice(ctx context.Context, repo, week string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE design_note_issues SET notified_at = '' WHERE repo = ? AND iso_week = ?`, repo, week)
	return err
}
