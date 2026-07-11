package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// WorkflowNote is one append-only external-coordinator journal entry.
type WorkflowNote struct {
	ID                  int64  `json:"id"`
	WorkflowID          string `json:"workflow_id"`
	Author              string `json:"author,omitempty"`
	Body                string `json:"body"`
	Repo                string `json:"repo,omitempty"`
	MemoryObservationID int64  `json:"memory_observation_id,omitempty"`
	CreatedAt           string `json:"created_at"`
}

// WorkflowSummary is the indexed jobs aggregate rendered by workflow list/show.
// Token counts are best-effort because not every runtime reports usage.
type WorkflowSummary struct {
	WorkflowID   string `json:"workflow_id"`
	JobCount     int    `json:"job_count"`
	Queued       int    `json:"queued"`
	Running      int    `json:"running"`
	Succeeded    int    `json:"succeeded"`
	Failed       int    `json:"failed"`
	Blocked      int    `json:"blocked"`
	Cancelled    int    `json:"cancelled"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	NoteCount    int    `json:"note_count"`
	FirstAt      string `json:"first_activity"`
	LastAt       string `json:"last_activity"`
}

// Exported query constants keep production SQL and EXPLAIN regression tests on
// exactly the same statements.
const ListWorkflowNotesSQL = `SELECT id, workflow_id, author, body, repo, memory_observation_id, created_at
FROM workflow_notes INDEXED BY idx_workflow_notes_wid
WHERE workflow_id = ? ORDER BY created_at, id LIMIT ?`

const ListWorkflowSummariesSQL = `SELECT j.workflow_id,
	COUNT(*),
	SUM(CASE WHEN j.state = 'queued' THEN 1 ELSE 0 END),
	SUM(CASE WHEN j.state = 'running' THEN 1 ELSE 0 END),
	SUM(CASE WHEN j.state = 'succeeded' THEN 1 ELSE 0 END),
	SUM(CASE WHEN j.state = 'failed' THEN 1 ELSE 0 END),
	SUM(CASE WHEN j.state = 'blocked' THEN 1 ELSE 0 END),
	SUM(CASE WHEN j.state = 'cancelled' THEN 1 ELSE 0 END),
	COALESCE(SUM(j.input_tokens), 0), COALESCE(SUM(j.output_tokens), 0),
	(SELECT COUNT(*) FROM workflow_notes n WHERE n.workflow_id = j.workflow_id),
	MIN(j.created_at), MAX(j.updated_at)
FROM jobs j INDEXED BY idx_jobs_workflow_id
WHERE j.workflow_id != ''
GROUP BY j.workflow_id
ORDER BY MAX(j.updated_at) DESC, j.workflow_id`

const WorkflowSummarySQL = `SELECT ?, COUNT(*),
	COALESCE(SUM(CASE WHEN j.state = 'queued' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN j.state = 'running' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN j.state = 'succeeded' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN j.state = 'failed' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN j.state = 'blocked' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN j.state = 'cancelled' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(j.input_tokens), 0), COALESCE(SUM(j.output_tokens), 0),
	(SELECT COUNT(*) FROM workflow_notes n WHERE n.workflow_id = ?),
	MIN(j.created_at), MAX(j.updated_at)
FROM jobs j INDEXED BY idx_jobs_workflow_id
WHERE j.workflow_id != '' AND j.workflow_id = ?`

const ListJobsByWorkflowSQL = `SELECT id, agent, type, state, workflow_id, repo, pull_request,
	blocker_retry_at, blocker_suggested_action, input_tokens, output_tokens, created_at, updated_at
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ?
ORDER BY created_at, id LIMIT ?`

const CountJobsByWorkflowSQL = `SELECT COUNT(*) FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ?`

const WorkflowReposSQL = `SELECT DISTINCT repo
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ? AND repo != ''
ORDER BY repo`

func workflowQueryLimit(limit int) int {
	if limit <= 0 {
		return -1
	}
	return limit
}

func (s *Store) InsertWorkflowNote(ctx context.Context, note WorkflowNote) (WorkflowNote, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO workflow_notes(workflow_id, author, body, repo, memory_observation_id)
VALUES (?, ?, ?, ?, ?)`, note.WorkflowID, note.Author, note.Body, note.Repo, note.MemoryObservationID)
	if err != nil {
		return WorkflowNote{}, fmt.Errorf("insert workflow note: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return WorkflowNote{}, err
	}
	return s.getWorkflowNote(ctx, id)
}

// InsertWorkflowNoteWithObservation atomically appends a journal note and its
// pending memory observation. The note id is part of the stable ingest key and
// provenance, so both rows are derived and committed in this one transaction.
func (s *Store) InsertWorkflowNoteWithObservation(ctx context.Context, note WorkflowNote, obs MemoryObservation) (WorkflowNote, MemoryObservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
INSERT INTO workflow_notes(workflow_id, author, body, repo)
VALUES (?, ?, ?, ?)`, note.WorkflowID, note.Author, note.Body, note.Repo)
	if err != nil {
		return WorkflowNote{}, MemoryObservation{}, fmt.Errorf("insert workflow note: %w", err)
	}
	noteID, err := res.LastInsertId()
	if err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	obs.Key = "workflow-" + note.WorkflowID + "-" + strconv.FormatInt(noteID, 10)
	obs.Provenance = fmt.Sprintf("workflow:%s#%d", note.WorkflowID, noteID)
	obsID, err := insertMemoryObservationTx(ctx, tx, obs)
	if err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_notes SET memory_observation_id = ? WHERE id = ?`, obsID, noteID); err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	note.ID, note.MemoryObservationID = noteID, obsID
	obs.ID = obsID
	stored, err := s.getWorkflowNote(ctx, noteID)
	if err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	return stored, obs, nil
}

func (s *Store) getWorkflowNote(ctx context.Context, id int64) (WorkflowNote, error) {
	var note WorkflowNote
	err := s.db.QueryRowContext(ctx, `
SELECT id, workflow_id, author, body, repo, memory_observation_id, created_at
FROM workflow_notes WHERE id = ?`, id).Scan(
		&note.ID, &note.WorkflowID, &note.Author, &note.Body, &note.Repo,
		&note.MemoryObservationID, &note.CreatedAt)
	return note, err
}

func (s *Store) ListWorkflowNotes(ctx context.Context, workflowID string, limit int) ([]WorkflowNote, error) {
	rows, err := s.db.QueryContext(ctx, ListWorkflowNotesSQL, workflowID, workflowQueryLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []WorkflowNote
	for rows.Next() {
		var note WorkflowNote
		if err := rows.Scan(&note.ID, &note.WorkflowID, &note.Author, &note.Body, &note.Repo, &note.MemoryObservationID, &note.CreatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

func (s *Store) ListWorkflowSummaries(ctx context.Context) ([]WorkflowSummary, error) {
	rows, err := s.db.QueryContext(ctx, ListWorkflowSummariesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowSummary
	for rows.Next() {
		var item WorkflowSummary
		if err := rows.Scan(&item.WorkflowID, &item.JobCount, &item.Queued, &item.Running,
			&item.Succeeded, &item.Failed, &item.Blocked, &item.Cancelled,
			&item.InputTokens, &item.OutputTokens, &item.NoteCount, &item.FirstAt, &item.LastAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListJobsByWorkflow intentionally omits payload: workflow membership, filters,
// rendering, blocker detail, timestamps, and token totals use scalar columns.
func (s *Store) ListJobsByWorkflow(ctx context.Context, workflowID string, limit int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, ListJobsByWorkflowSQL, workflowID, workflowQueryLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.WorkflowID, &job.Repo, &job.PullRequest,
			&job.BlockerRetryAt, &job.BlockerSuggestedAction,
			&job.InputTokens, &job.OutputTokens, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) CountJobsByWorkflow(ctx context.Context, workflowID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, CountJobsByWorkflowSQL, workflowID).Scan(&count)
	return count, err
}

func (s *Store) WorkflowSummary(ctx context.Context, workflowID string) (WorkflowSummary, error) {
	var item WorkflowSummary
	var firstAt, lastAt sql.NullString
	err := s.db.QueryRowContext(ctx, WorkflowSummarySQL, workflowID, workflowID, workflowID).Scan(
		&item.WorkflowID, &item.JobCount, &item.Queued, &item.Running, &item.Succeeded,
		&item.Failed, &item.Blocked, &item.Cancelled, &item.InputTokens,
		&item.OutputTokens, &item.NoteCount, &firstAt, &lastAt)
	if err != nil {
		return WorkflowSummary{}, err
	}
	if item.JobCount == 0 {
		return WorkflowSummary{}, sql.ErrNoRows
	}
	item.FirstAt, item.LastAt = firstAt.String, lastAt.String
	return item, nil
}

// WorkflowRepos returns distinct non-empty denormalized repo values for a
// workflow. It is used only for --remember repo inference.
func (s *Store) WorkflowRepos(ctx context.Context, workflowID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, WorkflowReposSQL, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repos []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, err
		}
		if repo = strings.TrimSpace(repo); repo != "" {
			repos = append(repos, repo)
		}
	}
	return repos, rows.Err()
}
