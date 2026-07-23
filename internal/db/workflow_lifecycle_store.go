package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// UnknownWorkflowError identifies a workflow label rejected by lifecycle
// mutation because it has no workflow-linked jobs.
type UnknownWorkflowError struct {
	Label string
}

func (e UnknownWorkflowError) Error() string {
	return fmt.Sprintf("workflow %q not found", e.Label)
}

// CloseWorkflowResult describes an explicit terminal transition.
type CloseWorkflowResult struct {
	WorkflowID      string         `json:"workflow_id"`
	Status          WorkflowStatus `json:"status"`
	AlreadyTerminal bool           `json:"already_terminal,omitempty"`
	Note            *WorkflowNote  `json:"note,omitempty"`
}

// PullRequestRef is one repository-scoped pull request referenced by a
// workflow's jobs or structured PR lifecycle notes.
type PullRequestRef struct {
	Repo   string
	Number int64
}

// WorkflowAutoSettleCandidate is the narrow projection used by the daemon
// lifecycle sweep. QuietAnchor excludes daemon-authored receipts.
type WorkflowAutoSettleCandidate struct {
	WorkflowID  string
	QuietAnchor time.Time
}

type workflowLifecycleQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// CloseWorkflow atomically refuses live work or records an explicit close.
// A workflow already in done or settled is returned unchanged without another
// close note, making repeated close requests idempotent.
func (s *Store) CloseWorkflow(ctx context.Context, label, reason string) (CloseWorkflowResult, error) {
	label = strings.TrimSpace(label)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CloseWorkflowResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var jobCount int
	if err := tx.QueryRowContext(ctx, CountJobsByWorkflowSQL, label).Scan(&jobCount); err != nil {
		return CloseWorkflowResult{}, err
	}
	if jobCount == 0 {
		return CloseWorkflowResult{}, &UnknownWorkflowError{Label: label}
	}
	activeJobs, err := countActiveWorkflowJobsTx(ctx, tx, label)
	if err != nil {
		return CloseWorkflowResult{}, err
	}
	if activeJobs > 0 {
		return CloseWorkflowResult{}, fmt.Errorf("cannot close workflow %q: %d job(s) still queued/running", label, activeJobs)
	}
	status, err := workflowMetaStatusTx(ctx, tx, label)
	if err != nil {
		return CloseWorkflowResult{}, err
	}
	if IsTerminalWorkflowStatus(status) {
		if err := tx.Commit(); err != nil {
			return CloseWorkflowResult{}, err
		}
		return CloseWorkflowResult{
			WorkflowID:      label,
			Status:          WorkflowStatus(status),
			AlreadyTerminal: true,
		}, nil
	}

	body := "[workflow:close]"
	if reason = strings.TrimSpace(reason); reason != "" {
		body += " " + reason
	}
	// The close breadcrumb is a structured machine-recorded note, like the
	// [auto:workflow:reopened] receipt and the daemon [auto:pr:*] notes: author
	// it "daemon" so it does not pollute last_human_author or seed the workflow
	// description (ensureWorkflowDescriptionTx only reads author != daemon notes).
	noteID, err := insertWorkflowNoteTx(ctx, tx, WorkflowNote{WorkflowID: label, Author: WorkflowAutoNoteAuthor, Body: body})
	if err != nil {
		return CloseWorkflowResult{}, err
	}
	if err := upsertWorkflowMetaTx(ctx, tx, WorkflowMeta{
		WorkflowID: label,
		Status:     string(WorkflowStatusDone),
		StatusSet:  true,
	}); err != nil {
		return CloseWorkflowResult{}, err
	}
	if err := ensureWorkflowDescriptionTx(ctx, tx, label); err != nil {
		return CloseWorkflowResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CloseWorkflowResult{}, err
	}
	note, err := s.getWorkflowNote(ctx, noteID)
	if err != nil {
		return CloseWorkflowResult{}, err
	}
	return CloseWorkflowResult{WorkflowID: label, Status: WorkflowStatusDone, Note: &note}, nil
}

func countActiveWorkflowJobsTx(ctx context.Context, tx *sql.Tx, label string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ? AND state IN ('queued','running')`, label).Scan(&count)
	return count, err
}

// WorkflowPullRequestRefs returns the deterministic union of scalar job PR
// references and structured [auto:pr:N:*] workflow receipts.
func (s *Store) WorkflowPullRequestRefs(ctx context.Context, workflowID string) ([]PullRequestRef, error) {
	return workflowPullRequestRefs(ctx, s.db, workflowID)
}

func workflowPullRequestRefs(ctx context.Context, q workflowLifecycleQueryer, workflowID string) ([]PullRequestRef, error) {
	type refKey struct {
		repo   string
		number int64
	}
	refs := make(map[refKey]struct{})
	jobRepos := make(map[string]struct{})
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT repo, pull_request
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ? AND pull_request > 0 AND repo != ''
ORDER BY repo, pull_request`, workflowID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var repo string
		var number int64
		if err := rows.Scan(&repo, &number); err != nil {
			_ = rows.Close()
			return nil, err
		}
		repo = strings.TrimSpace(repo)
		if repo != "" && number > 0 {
			refs[refKey{repo: repo, number: number}] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = q.QueryContext(ctx, WorkflowReposSQL, workflowID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if repo = strings.TrimSpace(repo); repo != "" {
			jobRepos[repo] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	fallbackRepo := ""
	if len(jobRepos) == 1 {
		for repo := range jobRepos {
			fallbackRepo = repo
		}
	}
	rows, err = q.QueryContext(ctx, `SELECT body, repo
FROM workflow_notes INDEXED BY idx_workflow_notes_wid
WHERE workflow_id = ? AND author = ?
ORDER BY created_at, id`, workflowID, WorkflowAutoNoteAuthor)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var body, repo string
		if err := rows.Scan(&body, &repo); err != nil {
			_ = rows.Close()
			return nil, err
		}
		number, ok := workflowPullRequestNumber(body)
		if !ok {
			continue
		}
		repo = strings.TrimSpace(repo)
		if repo == "" {
			repo = fallbackRepo
		}
		// Keep an unresolved empty-repo reference. Eligibility then fails closed
		// because no local pull_requests mirror row can match it.
		refs[refKey{repo: repo, number: number}] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	out := make([]PullRequestRef, 0, len(refs))
	for ref := range refs {
		out = append(out, PullRequestRef{Repo: ref.repo, Number: ref.number})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out, nil
}

func workflowPullRequestNumber(body string) (int64, bool) {
	body = strings.TrimSpace(body)
	end := strings.IndexByte(body, ']')
	if end <= 0 || body[0] != '[' {
		return 0, false
	}
	parts := strings.Split(body[1:end], ":")
	if len(parts) != 4 || parts[0] != "auto" || parts[1] != "pr" || parts[3] == "" {
		return 0, false
	}
	number, err := strconv.ParseInt(parts[2], 10, 64)
	return number, err == nil && number > 0
}

// ListWorkflowAutoSettleCandidates lists workflows that could plausibly settle:
// status not terminal/parked/blocked (blocked is a deliberate human-awaiting
// signal, #1094), AND referencing at least one PR (a workflow with zero PR refs
// can never settle, so journal-only coordinator workflows are excluded here to
// keep the per-tick sweep from opening a no-op transaction for each of them).
// The QuietAnchor is display-only; the authoritative gate rechecks in-tx.
func (s *Store) ListWorkflowAutoSettleCandidates(ctx context.Context) ([]WorkflowAutoSettleCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `WITH workflow_ids(workflow_id) AS (
	SELECT workflow_id FROM jobs WHERE workflow_id != ''
	UNION
	SELECT workflow_id FROM workflow_notes WHERE workflow_id != ''
	UNION
	SELECT workflow_id FROM workflow_meta WHERE workflow_id != ''
)
SELECT w.workflow_id,
	COALESCE((
		SELECT MAX(n.created_at)
		FROM workflow_notes n INDEXED BY idx_workflow_notes_wid
		WHERE n.workflow_id = w.workflow_id AND n.author != ?
	), ''),
	COALESCE((
		SELECT MAX(j.updated_at)
		FROM jobs j INDEXED BY idx_jobs_workflow_id
		WHERE j.workflow_id != '' AND j.workflow_id = w.workflow_id
	), '')
FROM workflow_ids w
LEFT JOIN workflow_meta m ON m.workflow_id = w.workflow_id
WHERE COALESCE(m.status, '') NOT IN ('parked', 'done', 'settled', 'blocked')
	AND (
		EXISTS (
			SELECT 1 FROM jobs jr INDEXED BY idx_jobs_workflow_id
			WHERE jr.workflow_id != '' AND jr.workflow_id = w.workflow_id AND jr.pull_request > 0
		)
		OR EXISTS (
			SELECT 1 FROM workflow_notes nr
			WHERE nr.workflow_id = w.workflow_id AND nr.author = ? AND substr(nr.body, 1, 9) = '[auto:pr:'
		)
	)
ORDER BY w.workflow_id`, WorkflowAutoNoteAuthor, WorkflowAutoNoteAuthor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []WorkflowAutoSettleCandidate
	for rows.Next() {
		var candidate WorkflowAutoSettleCandidate
		var humanNoteAt, jobUpdatedAt string
		if err := rows.Scan(&candidate.WorkflowID, &humanNoteAt, &jobUpdatedAt); err != nil {
			return nil, err
		}
		candidate.QuietAnchor = latestWorkflowActivity(humanNoteAt, jobUpdatedAt)
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func workflowQuietAnchor(ctx context.Context, q workflowLifecycleQueryer, workflowID string) (time.Time, error) {
	var humanNoteAt, jobUpdatedAt string
	if err := q.QueryRowContext(ctx, `SELECT
	COALESCE((
		SELECT MAX(created_at)
		FROM workflow_notes INDEXED BY idx_workflow_notes_wid
		WHERE workflow_id = ? AND author != ?
	), ''),
	COALESCE((
		SELECT MAX(updated_at)
		FROM jobs INDEXED BY idx_jobs_workflow_id
		WHERE workflow_id != '' AND workflow_id = ?
	), '')`, workflowID, WorkflowAutoNoteAuthor, workflowID).Scan(&humanNoteAt, &jobUpdatedAt); err != nil {
		return time.Time{}, err
	}
	return latestWorkflowActivity(humanNoteAt, jobUpdatedAt), nil
}

func latestWorkflowActivity(values ...string) time.Time {
	var latest time.Time
	for _, value := range values {
		if parsed := parseStoredTimestamp(value); parsed.After(latest) {
			latest = parsed
		}
	}
	return latest
}

// SettleWorkflowIfEligible atomically rechecks every conservative lifecycle
// condition and records one reversible settled transition.
func (s *Store) SettleWorkflowIfEligible(ctx context.Context, workflowID string, now time.Time, after time.Duration) (bool, error) {
	if after <= 0 {
		return false, nil
	}
	workflowID = strings.TrimSpace(workflowID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	status, err := workflowMetaStatusTx(ctx, tx, workflowID)
	if err != nil {
		return false, err
	}
	if IsTerminalWorkflowStatus(status) || status == string(WorkflowStatusParked) ||
		status == string(WorkflowStatusBlocked) {
		return false, nil
	}
	refs, err := workflowPullRequestRefs(ctx, tx, workflowID)
	if err != nil {
		return false, err
	}
	if len(refs) == 0 {
		return false, nil
	}
	for _, ref := range refs {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state
FROM pull_requests
WHERE repo_full_name = ? AND number = ?`, ref.Repo, ref.Number).Scan(&state)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		if state != "merged" && state != "closed" {
			return false, nil
		}
	}
	activeJobs, err := countActiveWorkflowJobsTx(ctx, tx, workflowID)
	if err != nil {
		return false, err
	}
	if activeJobs != 0 {
		return false, nil
	}
	anchor, err := workflowQuietAnchor(ctx, tx, workflowID)
	if err != nil {
		return false, err
	}
	now = now.UTC()
	if anchor.IsZero() || now.Before(anchor) || now.Sub(anchor) < after {
		return false, nil
	}
	if _, err := insertWorkflowNoteTx(ctx, tx, WorkflowNote{
		WorkflowID: workflowID,
		Author:     WorkflowAutoNoteAuthor,
		Body:       fmt.Sprintf("[auto:workflow:settled] merged/closed PRs, quiet ≥ %s", after),
	}); err != nil {
		return false, err
	}
	if err := upsertWorkflowMetaTx(ctx, tx, WorkflowMeta{
		WorkflowID: workflowID,
		Status:     string(WorkflowStatusSettled),
		StatusSet:  true,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
