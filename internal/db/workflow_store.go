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

// WorkflowMeta is the latest external-coordinator handoff identity recorded for
// one workflow. Empty fields are meaningful and remain empty on the wire.
type WorkflowMeta struct {
	WorkflowID string `json:"workflow_id"`
	Author     string `json:"author,omitempty"`
	Pane       string `json:"pane,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	WorkDir    string `json:"workdir,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
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
	LastNote     string `json:"last_note,omitempty"`
	LastAuthor   string `json:"last_author,omitempty"`
	// LastFailureAt/LastNoteAt let the dashboard's stalled derivation apply the
	// acknowledgment rule: a failure with a LATER journal note is not an alarm.
	LastFailureAt string `json:"last_failure_at,omitempty"`
	LastNoteAt    string `json:"last_note_at,omitempty"`
}

// Exported query constants keep production SQL and EXPLAIN regression tests on
// exactly the same statements.
const ListWorkflowNotesSQL = `SELECT id, workflow_id, author, body, repo, memory_observation_id, created_at
FROM workflow_notes INDEXED BY idx_workflow_notes_wid
WHERE workflow_id = ? ORDER BY created_at, id LIMIT ?`

const workflowSummarySelectSQL = `WITH job_summary AS (
	SELECT j.workflow_id,
		COUNT(*) AS job_count,
		SUM(CASE WHEN j.state = 'queued' THEN 1 ELSE 0 END) AS queued,
		SUM(CASE WHEN j.state = 'running' THEN 1 ELSE 0 END) AS running,
		SUM(CASE WHEN j.state = 'succeeded' THEN 1 ELSE 0 END) AS succeeded,
		SUM(CASE WHEN j.state = 'failed' THEN 1 ELSE 0 END) AS failed,
		SUM(CASE WHEN j.state = 'blocked' THEN 1 ELSE 0 END) AS blocked,
		SUM(CASE WHEN j.state = 'cancelled' THEN 1 ELSE 0 END) AS cancelled,
		COALESCE(SUM(j.input_tokens), 0) AS input_tokens,
		COALESCE(SUM(j.output_tokens), 0) AS output_tokens,
		MIN(j.created_at) AS first_at,
		MAX(j.updated_at) AS last_at,
		MAX(CASE WHEN j.state IN ('failed','blocked') THEN j.updated_at END) AS last_failure_at
	FROM jobs j INDEXED BY idx_jobs_workflow_id
	WHERE j.workflow_id != ''
	GROUP BY j.workflow_id
), note_summary AS (
	SELECT n.workflow_id,
		COUNT(*) AS note_count,
		MIN(n.created_at) AS first_at,
		MAX(n.created_at) AS last_at,
		COALESCE((SELECT latest.body FROM workflow_notes latest
			WHERE latest.workflow_id = n.workflow_id
			ORDER BY latest.created_at DESC, latest.id DESC LIMIT 1), '') AS last_note,
		COALESCE((SELECT latest.author FROM workflow_notes latest
			WHERE latest.workflow_id = n.workflow_id
			ORDER BY latest.created_at DESC, latest.id DESC LIMIT 1), '') AS last_author
	FROM workflow_notes n INDEXED BY idx_workflow_notes_wid
	GROUP BY n.workflow_id
), labels AS (
	SELECT workflow_id FROM job_summary
	UNION
	SELECT workflow_id FROM note_summary
)
SELECT labels.workflow_id,
	COALESCE(j.job_count, 0), COALESCE(j.queued, 0), COALESCE(j.running, 0),
	COALESCE(j.succeeded, 0), COALESCE(j.failed, 0), COALESCE(j.blocked, 0),
	COALESCE(j.cancelled, 0), COALESCE(j.input_tokens, 0), COALESCE(j.output_tokens, 0),
	COALESCE(n.note_count, 0),
	CASE
		WHEN j.first_at IS NULL THEN n.first_at
		WHEN n.first_at IS NULL THEN j.first_at
		WHEN j.first_at <= n.first_at THEN j.first_at ELSE n.first_at
	END AS first_at,
	CASE
		WHEN j.last_at IS NULL THEN n.last_at
		WHEN n.last_at IS NULL THEN j.last_at
		WHEN j.last_at >= n.last_at THEN j.last_at ELSE n.last_at
	END AS last_at,
	COALESCE(n.last_note, ''), COALESCE(n.last_author, ''),
	COALESCE(j.last_failure_at, ''), COALESCE(n.last_at, '')
FROM labels
LEFT JOIN job_summary j ON j.workflow_id = labels.workflow_id
LEFT JOIN note_summary n ON n.workflow_id = labels.workflow_id`

const ListWorkflowSummariesSQL = workflowSummarySelectSQL + `
ORDER BY last_at DESC, labels.workflow_id`

const WorkflowSummarySQL = workflowSummarySelectSQL + `
WHERE labels.workflow_id = ?`

const ListJobsByWorkflowSQL = `SELECT id, agent, type, state, workflow_id, repo, pull_request,
	blocker_retry_at, blocker_suggested_action, input_tokens, output_tokens, created_at, updated_at
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ?
ORDER BY created_at, id LIMIT ?`

// ListWorkflowGraphJobsSQL is the workflow dashboard's bounded payload
// projection. Unlike the scalar CLI list above, the workflow forest needs each
// labeled job's payload to render titles, dependency edges, runtime overrides,
// and models. The workflow_id predicate keeps that payload scan scoped to one
// indexed label instead of materializing payloads globally.
const ListWorkflowGraphJobsSQL = `SELECT id, agent, type, state, payload, parent_job_id,
	delegation_id, delegation_depth, root_id, workflow_id, input_tokens, output_tokens,
	created_at, updated_at
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ?
ORDER BY created_at, id`

const CountJobsByWorkflowSQL = `SELECT COUNT(*) FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ?`

const WorkflowReposSQL = `SELECT DISTINCT repo
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ? AND repo != ''
ORDER BY repo`

const ListWorkflowReposSQL = `SELECT workflow_id, repo
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND repo != ''
GROUP BY workflow_id, repo
ORDER BY workflow_id, repo`

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

// InsertWorkflowNoteWithMeta atomically appends a note and replaces the
// workflow's coordinator handoff metadata with the values from this note.
func (s *Store) InsertWorkflowNoteWithMeta(ctx context.Context, note WorkflowNote, meta WorkflowMeta) (WorkflowNote, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowNote{}, err
	}
	defer func() { _ = tx.Rollback() }()
	noteID, err := insertWorkflowNoteTx(ctx, tx, note)
	if err != nil {
		return WorkflowNote{}, err
	}
	meta.WorkflowID = note.WorkflowID
	if err := upsertWorkflowMetaTx(ctx, tx, meta); err != nil {
		return WorkflowNote{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowNote{}, err
	}
	return s.getWorkflowNote(ctx, noteID)
}

// InsertWorkflowNoteWithObservation atomically appends a journal note and its
// pending memory observation. The note id is part of the stable ingest key and
// provenance, so both rows are derived and committed in this one transaction.
func (s *Store) InsertWorkflowNoteWithObservation(ctx context.Context, note WorkflowNote, obs MemoryObservation) (WorkflowNote, MemoryObservation, error) {
	return s.insertWorkflowNoteWithObservationAndMeta(ctx, note, obs, WorkflowMeta{}, false)
}

// InsertWorkflowNoteWithObservationAndMeta is the coordinator-metadata variant
// of InsertWorkflowNoteWithObservation. Note, metadata, and memory observation
// either all commit or all roll back.
func (s *Store) InsertWorkflowNoteWithObservationAndMeta(ctx context.Context, note WorkflowNote, obs MemoryObservation, meta WorkflowMeta) (WorkflowNote, MemoryObservation, error) {
	return s.insertWorkflowNoteWithObservationAndMeta(ctx, note, obs, meta, true)
}

func (s *Store) insertWorkflowNoteWithObservationAndMeta(ctx context.Context, note WorkflowNote, obs MemoryObservation, meta WorkflowMeta, writeMeta bool) (WorkflowNote, MemoryObservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	noteID, err := insertWorkflowNoteTx(ctx, tx, note)
	if err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	if writeMeta {
		meta.WorkflowID = note.WorkflowID
		if err := upsertWorkflowMetaTx(ctx, tx, meta); err != nil {
			return WorkflowNote{}, MemoryObservation{}, err
		}
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

func insertWorkflowNoteTx(ctx context.Context, tx *sql.Tx, note WorkflowNote) (int64, error) {
	res, err := tx.ExecContext(ctx, `
INSERT INTO workflow_notes(workflow_id, author, body, repo, memory_observation_id)
VALUES (?, ?, ?, ?, ?)`, note.WorkflowID, note.Author, note.Body, note.Repo, note.MemoryObservationID)
	if err != nil {
		return 0, fmt.Errorf("insert workflow note: %w", err)
	}
	return res.LastInsertId()
}

func upsertWorkflowMetaTx(ctx context.Context, tx *sql.Tx, meta WorkflowMeta) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_meta(workflow_id, author, pane, session_id, workdir, updated_at)
VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(workflow_id) DO UPDATE SET
	author = excluded.author,
	pane = CASE WHEN excluded.pane != '' THEN excluded.pane ELSE workflow_meta.pane END,
	session_id = CASE WHEN excluded.session_id != '' THEN excluded.session_id ELSE workflow_meta.session_id END,
	workdir = CASE WHEN excluded.workdir != '' THEN excluded.workdir ELSE workflow_meta.workdir END,
	updated_at = CURRENT_TIMESTAMP`, meta.WorkflowID, meta.Author, meta.Pane, meta.SessionID, meta.WorkDir)
	return err
}

// GetWorkflowMeta returns one workflow's latest coordinator handoff metadata.
func (s *Store) GetWorkflowMeta(ctx context.Context, workflowID string) (WorkflowMeta, error) {
	var meta WorkflowMeta
	err := s.db.QueryRowContext(ctx, `SELECT workflow_id, author, pane, session_id, workdir, updated_at
FROM workflow_meta WHERE workflow_id = ?`, strings.TrimSpace(workflowID)).Scan(
		&meta.WorkflowID, &meta.Author, &meta.Pane, &meta.SessionID, &meta.WorkDir, &meta.UpdatedAt)
	return meta, err
}

// ListWorkflowMeta returns all coordinator metadata keyed by workflow id.
func (s *Store) ListWorkflowMeta(ctx context.Context) (map[string]WorkflowMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT workflow_id, author, pane, session_id, workdir, updated_at FROM workflow_meta ORDER BY workflow_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WorkflowMeta{}
	for rows.Next() {
		var meta WorkflowMeta
		if err := rows.Scan(&meta.WorkflowID, &meta.Author, &meta.Pane, &meta.SessionID, &meta.WorkDir, &meta.UpdatedAt); err != nil {
			return nil, err
		}
		out[meta.WorkflowID] = meta
	}
	return out, rows.Err()
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
			&item.InputTokens, &item.OutputTokens, &item.NoteCount, &item.FirstAt, &item.LastAt,
			&item.LastNote, &item.LastAuthor, &item.LastFailureAt, &item.LastNoteAt); err != nil {
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

// ListWorkflowGraphJobs returns every job carrying workflowID, including the
// payload and denormalized root needed to assemble complete run trees. The
// query is intentionally label-bounded and uses idx_jobs_workflow_id.
func (s *Store) ListWorkflowGraphJobs(ctx context.Context, workflowID string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, ListWorkflowGraphJobsSQL, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload,
			&job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.RootID,
			&job.WorkflowID, &job.InputTokens, &job.OutputTokens, &job.CreatedAt,
			&job.UpdatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// WorkflowNoteCounts returns note totals for the requested workflow labels in
// one grouped indexed query. Missing labels are omitted from the result map.
func (s *Store) WorkflowNoteCounts(ctx context.Context, workflowIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(workflowIDs))
	if len(workflowIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(workflowIDs)), ",")
	args := make([]any, len(workflowIDs))
	for i, workflowID := range workflowIDs {
		args[i] = workflowID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT workflow_id, COUNT(*)
FROM workflow_notes INDEXED BY idx_workflow_notes_wid
WHERE workflow_id IN (`+placeholders+`)
GROUP BY workflow_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var workflowID string
		var count int
		if err := rows.Scan(&workflowID, &count); err != nil {
			return nil, err
		}
		out[workflowID] = count
	}
	return out, rows.Err()
}

func (s *Store) CountJobsByWorkflow(ctx context.Context, workflowID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, CountJobsByWorkflowSQL, workflowID).Scan(&count)
	return count, err
}

func (s *Store) WorkflowSummary(ctx context.Context, workflowID string) (WorkflowSummary, error) {
	var item WorkflowSummary
	var firstAt, lastAt sql.NullString
	err := s.db.QueryRowContext(ctx, WorkflowSummarySQL, workflowID).Scan(
		&item.WorkflowID, &item.JobCount, &item.Queued, &item.Running, &item.Succeeded,
		&item.Failed, &item.Blocked, &item.Cancelled, &item.InputTokens,
		&item.OutputTokens, &item.NoteCount, &firstAt, &lastAt, &item.LastNote, &item.LastAuthor,
		&item.LastFailureAt, &item.LastNoteAt)
	if err != nil {
		return WorkflowSummary{}, err
	}
	if item.JobCount == 0 && item.NoteCount == 0 {
		return WorkflowSummary{}, sql.ErrNoRows
	}
	item.FirstAt, item.LastAt = firstAt.String, lastAt.String
	return item, nil
}

// ListWorkflowRepos returns distinct repositories for every indexed workflow
// without reading job payloads.
func (s *Store) ListWorkflowRepos(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, ListWorkflowReposSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var workflowID, repo string
		if err := rows.Scan(&workflowID, &repo); err != nil {
			return nil, err
		}
		out[workflowID] = append(out[workflowID], repo)
	}
	return out, rows.Err()
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
