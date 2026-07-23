package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// WorkflowMetaTextMax is the byte cap shared by summary, description, and
	// status. It is enforced in the store so daemon and CLI writes cannot diverge.
	WorkflowMetaTextMax = 300
	// WorkflowAutoNoteAuthor makes machine lifecycle breadcrumbs visibly distinct
	// from coordinator handoff notes and scopes the partial dedupe index.
	WorkflowAutoNoteAuthor = "daemon"
)

// WorkflowStatus is the canonical lifecycle vocabulary accepted by new writes.
// Legacy values remain readable; validation is intentionally write-only.
type WorkflowStatus string

const (
	WorkflowStatusActive       WorkflowStatus = "active"
	WorkflowStatusBlocked      WorkflowStatus = "blocked"
	WorkflowStatusReadyToMerge WorkflowStatus = "ready_to_merge"
	WorkflowStatusDone         WorkflowStatus = "done"
	WorkflowStatusSettled      WorkflowStatus = "settled"
	WorkflowStatusParked       WorkflowStatus = "parked"
)

// ValidateWorkflowStatus accepts the canonical lifecycle values and the empty
// string, which means unset. Existing legacy values are never validated on read.
func ValidateWorkflowStatus(status string) error {
	switch WorkflowStatus(status) {
	case "", WorkflowStatusActive, WorkflowStatusBlocked, WorkflowStatusReadyToMerge,
		WorkflowStatusDone, WorkflowStatusSettled, WorkflowStatusParked:
		return nil
	default:
		return fmt.Errorf("workflow status must be empty or one of active, blocked, ready_to_merge, done, settled, parked")
	}
}

// IsTerminalWorkflowStatus reports the only statuses that leave the active
// workflow lifecycle: explicit completion and quiescent settlement.
func IsTerminalWorkflowStatus(status string) bool {
	switch WorkflowStatus(status) {
	case WorkflowStatusDone, WorkflowStatusSettled:
		return true
	default:
		return false
	}
}

var workflowIssueReferencePattern = regexp.MustCompile(`#([1-9][0-9]*)`)

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
	Summary    string `json:"summary,omitempty"`
	// SummarySet distinguishes an omitted --summary flag from an explicit empty
	// value, which clears the stored summary.
	SummarySet     bool   `json:"-"`
	Description    string `json:"description,omitempty"`
	DescriptionSet bool   `json:"-"`
	Status         string `json:"status,omitempty"`
	StatusSet      bool   `json:"-"`
	UpdatedAt      string `json:"updated_at,omitempty"`
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
	// LastNote and LastAuthor describe the literal latest journal entry, including
	// daemon receipts. LastFailureAt/LastHumanNoteAt/LastMergedReceiptAt let the
	// dashboard apply the acknowledgment rule: daemon lifecycle receipts remain
	// activity, but merged daemon receipts may acknowledge like human notes.
	LastFailureAt       string `json:"last_failure_at,omitempty"`
	LastNoteAt          string `json:"last_note_at,omitempty"`
	LastHumanAuthor     string `json:"last_human_author,omitempty"`
	LastHumanNoteAt     string `json:"last_human_note_at,omitempty"`
	LastMergedReceiptAt string `json:"last_merged_receipt_at,omitempty"`
}

// Exported query constants keep production SQL and EXPLAIN regression tests on
// exactly the same statements.
const ListWorkflowNotesSQL = `SELECT id, workflow_id, author, body, repo, memory_observation_id, created_at
FROM workflow_notes INDEXED BY idx_workflow_notes_wid
WHERE workflow_id = ? ORDER BY created_at, id LIMIT ?`

// SQLite LIKE is ASCII-case-insensitive but harmless here: the sole writer path
// (InsertWorkflowAutoNoteWithMeta) validates the lowercase "[auto:pr:" prefix.
// Matching only the first bracketed key keeps the anchored ":merged]" from
// matching opened/ready/closed receipt prose.
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
			ORDER BY latest.created_at DESC, latest.id DESC LIMIT 1), '') AS last_author,
		COALESCE((SELECT latest.author FROM workflow_notes latest
			WHERE latest.workflow_id = n.workflow_id AND latest.author != 'daemon'
				AND substr(latest.body, 1, length('[org:escalate ')) != '[org:escalate '
			ORDER BY latest.created_at DESC, latest.id DESC LIMIT 1), '') AS last_human_author,
		MAX(CASE WHEN n.author != 'daemon' AND substr(n.body, 1, length('[org:escalate ')) != '[org:escalate ' THEN n.created_at END) AS last_human_at,
		MAX(CASE WHEN n.author = 'daemon' AND substr(n.body, 1, instr(n.body, ']')) LIKE '[auto:pr:%:merged]' THEN n.created_at END) AS last_merged_at
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
	COALESCE(n.last_note, ''), COALESCE(n.last_author, ''), COALESCE(n.last_human_author, ''),
	COALESCE(j.last_failure_at, ''), COALESCE(n.last_at, ''), COALESCE(n.last_human_at, ''), COALESCE(n.last_merged_at, '')
FROM labels
LEFT JOIN job_summary j ON j.workflow_id = labels.workflow_id
LEFT JOIN note_summary n ON n.workflow_id = labels.workflow_id`

const ListWorkflowSummariesSQL = workflowSummarySelectSQL + `
ORDER BY last_at DESC, labels.workflow_id`

const WorkflowSummarySQL = workflowSummarySelectSQL + `
WHERE labels.workflow_id = ?`

const ListJobsByWorkflowSQL = `SELECT id, agent, type, state, model, workflow_id, repo, pull_request,
	blocker_retry_at, blocker_suggested_action, input_tokens, output_tokens, created_at, updated_at
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ?
ORDER BY created_at, id LIMIT ?`

// ListWorkflowGraphJobsSQL is the workflow dashboard's bounded payload
// projection. Unlike the scalar CLI list above, the workflow forest needs each
// labeled job's payload to render titles, dependency edges, runtime overrides,
// and models. The workflow_id predicate keeps that payload scan scoped to one
// indexed label instead of materializing payloads globally.
const ListWorkflowGraphJobsSQL = `SELECT id, agent, type, state, payload, model, parent_job_id,
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
	stored, _, err := s.insertWorkflowNoteWithObservationAndMeta(ctx, note, MemoryObservation{}, WorkflowMeta{}, false, false)
	return stored, err
}

// InsertWorkflowNoteWithMeta atomically appends a note and updates the
// workflow's coordinator handoff metadata with the values from this note.
func (s *Store) InsertWorkflowNoteWithMeta(ctx context.Context, note WorkflowNote, meta WorkflowMeta) (WorkflowNote, error) {
	stored, _, err := s.insertWorkflowNoteWithObservationAndMeta(ctx, note, MemoryObservation{}, meta, true, false)
	return stored, err
}

// InsertWorkflowAutoNoteWithMeta atomically appends one structured daemon note
// and updates metadata. The partial unique index makes the bracketed receipt
// prefix an at-most-once key across repeated polls and concurrent writers.
// inserted=false is a successful replay and leaves a later manual status alone.
func (s *Store) InsertWorkflowAutoNoteWithMeta(ctx context.Context, note WorkflowNote, meta WorkflowMeta) (stored WorkflowNote, inserted bool, err error) {
	if note.Author != WorkflowAutoNoteAuthor || !strings.HasPrefix(note.Body, "[auto:pr:") || !strings.Contains(note.Body, "]") {
		return WorkflowNote{}, false, fmt.Errorf("workflow auto note requires author %q and a bracketed [auto:pr: receipt", WorkflowAutoNoteAuthor)
	}
	if err := validateWorkflowMeta(meta); err != nil {
		return WorkflowNote{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowNote{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workflow_notes(workflow_id, author, body, repo, memory_observation_id)
VALUES (?, ?, ?, ?, ?)`, note.WorkflowID, note.Author, note.Body, note.Repo, note.MemoryObservationID)
	if err != nil {
		return WorkflowNote{}, false, fmt.Errorf("insert workflow auto note: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return WorkflowNote{}, false, err
	}
	if affected == 0 {
		return WorkflowNote{}, false, tx.Commit()
	}
	noteID, err := result.LastInsertId()
	if err != nil {
		return WorkflowNote{}, false, err
	}
	meta.WorkflowID = note.WorkflowID
	if err := upsertWorkflowMetaConditionalStatusTx(ctx, tx, meta); err != nil {
		return WorkflowNote{}, false, err
	}
	if !meta.DescriptionSet {
		if err := ensureWorkflowDescriptionTx(ctx, tx, note.WorkflowID); err != nil {
			return WorkflowNote{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return WorkflowNote{}, false, err
	}
	stored, err = s.getWorkflowNote(ctx, noteID)
	return stored, err == nil, err
}

// WorkflowAutoNoteExists checks one structured receipt key using the same
// expression as idx_workflow_notes_daemon_auto.
func (s *Store) WorkflowAutoNoteExists(ctx context.Context, workflowID, key string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
	SELECT 1 FROM workflow_notes
	WHERE workflow_id = ? AND author = ?
		AND substr(body, 1, instr(body, ']')) = ?
)`, strings.TrimSpace(workflowID), WorkflowAutoNoteAuthor, strings.TrimSpace(key)).Scan(&exists)
	return exists, err
}

// InsertWorkflowNoteWithObservation atomically appends a journal note and its
// pending memory observation. The note id is part of the stable ingest key and
// provenance, so both rows are derived and committed in this one transaction.
func (s *Store) InsertWorkflowNoteWithObservation(ctx context.Context, note WorkflowNote, obs MemoryObservation) (WorkflowNote, MemoryObservation, error) {
	return s.insertWorkflowNoteWithObservationAndMeta(ctx, note, obs, WorkflowMeta{}, false, true)
}

// InsertWorkflowNoteWithObservationAndMeta is the coordinator-metadata variant
// of InsertWorkflowNoteWithObservation. Note, metadata, and memory observation
// either all commit or all roll back.
func (s *Store) InsertWorkflowNoteWithObservationAndMeta(ctx context.Context, note WorkflowNote, obs MemoryObservation, meta WorkflowMeta) (WorkflowNote, MemoryObservation, error) {
	return s.insertWorkflowNoteWithObservationAndMeta(ctx, note, obs, meta, true, true)
}

func (s *Store) insertWorkflowNoteWithObservationAndMeta(ctx context.Context, note WorkflowNote, obs MemoryObservation, meta WorkflowMeta, writeMeta, writeObservation bool) (WorkflowNote, MemoryObservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowNote{}, MemoryObservation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if !writeMeta || !meta.StatusSet {
		status, err := workflowMetaStatusTx(ctx, tx, note.WorkflowID)
		if err != nil {
			return WorkflowNote{}, MemoryObservation{}, err
		}
		if IsTerminalWorkflowStatus(status) {
			if _, err := insertWorkflowNoteTx(ctx, tx, WorkflowNote{
				WorkflowID: note.WorkflowID,
				Author:     WorkflowAutoNoteAuthor,
				Body:       fmt.Sprintf("[auto:workflow:reopened] reopened from %s", status),
				Repo:       note.Repo,
			}); err != nil {
				return WorkflowNote{}, MemoryObservation{}, err
			}
			if err := upsertWorkflowMetaTx(ctx, tx, WorkflowMeta{
				WorkflowID: note.WorkflowID,
				Status:     string(WorkflowStatusActive),
				StatusSet:  true,
			}); err != nil {
				return WorkflowNote{}, MemoryObservation{}, err
			}
		}
	}
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
	if !writeMeta || !meta.DescriptionSet {
		if err := ensureWorkflowDescriptionTx(ctx, tx, note.WorkflowID); err != nil {
			return WorkflowNote{}, MemoryObservation{}, err
		}
	}
	if !writeObservation {
		if err := tx.Commit(); err != nil {
			return WorkflowNote{}, MemoryObservation{}, err
		}
		stored, err := s.getWorkflowNote(ctx, noteID)
		return stored, MemoryObservation{}, err
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

func workflowMetaStatusTx(ctx context.Context, tx *sql.Tx, workflowID string) (string, error) {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM workflow_meta WHERE workflow_id = ?`, workflowID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return status, err
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
	return upsertWorkflowMetaTxWithStatusGuard(ctx, tx, meta, false)
}

// upsertWorkflowMetaConditionalStatusTx preserves operator-owned lifecycle
// states while allowing daemon PR receipts to canonicalize stale machine-owned
// status. Only the status field is guarded; coordinator metadata keeps the same
// last-write-wins behavior as upsertWorkflowMetaTx.
func upsertWorkflowMetaConditionalStatusTx(ctx context.Context, tx *sql.Tx, meta WorkflowMeta) error {
	return upsertWorkflowMetaTxWithStatusGuard(ctx, tx, meta, true)
}

func upsertWorkflowMetaTxWithStatusGuard(ctx context.Context, tx *sql.Tx, meta WorkflowMeta, protectStatus bool) error {
	if err := validateWorkflowMeta(meta); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_meta(workflow_id, author, pane, session_id, workdir, summary, description, status, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(workflow_id) DO UPDATE SET
	author = CASE WHEN excluded.author != '' THEN excluded.author ELSE workflow_meta.author END,
	pane = CASE WHEN excluded.pane != '' THEN excluded.pane ELSE workflow_meta.pane END,
	session_id = CASE WHEN excluded.session_id != '' THEN excluded.session_id ELSE workflow_meta.session_id END,
	workdir = CASE WHEN excluded.workdir != '' THEN excluded.workdir ELSE workflow_meta.workdir END,
	summary = CASE WHEN ? THEN excluded.summary ELSE workflow_meta.summary END,
	description = CASE WHEN ? THEN excluded.description ELSE workflow_meta.description END,
	status = CASE
		WHEN ? AND (NOT ? OR workflow_meta.status NOT IN ('blocked', 'parked', 'done', 'settled'))
			THEN excluded.status
		ELSE workflow_meta.status
	END,
	updated_at = CURRENT_TIMESTAMP`, meta.WorkflowID, meta.Author, meta.Pane, meta.SessionID, meta.WorkDir,
		meta.Summary, meta.Description, meta.Status, meta.SummarySet, meta.DescriptionSet, meta.StatusSet, protectStatus)
	return err
}

func validateWorkflowMeta(meta WorkflowMeta) error {
	if meta.StatusSet {
		if err := ValidateWorkflowStatus(meta.Status); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "summary", value: meta.Summary},
		{name: "description", value: meta.Description},
		{name: "status", value: meta.Status},
	} {
		if len(field.value) > WorkflowMetaTextMax {
			return fmt.Errorf("workflow %s must be at most %d bytes", field.name, WorkflowMetaTextMax)
		}
	}
	return nil
}

// SetWorkflowDescription updates the stable human intent and mirrors it into
// legacy summary so older CLI/API consumers keep seeing the human "what" line.
func (s *Store) SetWorkflowDescription(ctx context.Context, workflowID, description string) error {
	if len(description) > WorkflowMetaTextMax {
		return fmt.Errorf("workflow description must be at most %d bytes", WorkflowMetaTextMax)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO workflow_meta(workflow_id, summary, description, updated_at)
VALUES (?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(workflow_id) DO UPDATE SET
	summary = excluded.summary,
	description = excluded.description,
	updated_at = CURRENT_TIMESTAMP`, strings.TrimSpace(workflowID), description, description)
	return err
}

// WorkflowIDForPullRequest resolves the newest workflow-linked job for a PR.
// The indexed workflow subset is small; branch remains payload-only, so it is
// decoded only for rows in the requested repo. Pull-request equality wins and
// the head branch is the compatibility fallback for pre-finalizer job payloads.
func (s *Store) WorkflowIDForPullRequest(ctx context.Context, repo string, pullRequest int, branch string) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT workflow_id, pull_request, payload
FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND repo = ?
ORDER BY updated_at DESC, id DESC`, strings.TrimSpace(repo))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	branch = strings.TrimSpace(branch)
	// Pull-request equality wins over the head-branch fallback: an exact PR
	// number match returns immediately, while the first (most-recent) branch-only
	// match is only remembered and returned if no PR match is found anywhere.
	// A single early-return-on-either loop could let a newer branch-only job in a
	// different workflow (a reused branch name) beat the correct PR-stamped job.
	branchMatch := ""
	for rows.Next() {
		var workflowID, payloadRaw string
		var storedPullRequest int
		if err := rows.Scan(&workflowID, &storedPullRequest, &payloadRaw); err != nil {
			return "", err
		}
		if pullRequest > 0 && storedPullRequest == pullRequest {
			return workflowID, nil
		}
		if branch == "" || branchMatch != "" {
			continue
		}
		var payload struct {
			Branch string `json:"branch"`
		}
		if json.Unmarshal([]byte(payloadRaw), &payload) == nil && strings.TrimSpace(payload.Branch) == branch {
			branchMatch = workflowID
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return branchMatch, nil
}

func ensureWorkflowDescriptionTx(ctx context.Context, tx *sql.Tx, workflowID string) error {
	workflowID = strings.TrimSpace(workflowID)
	if workflowID == "" {
		return nil
	}
	var description, legacySummary string
	err := tx.QueryRowContext(ctx, `SELECT description, summary FROM workflow_meta WHERE workflow_id = ?`, workflowID).Scan(&description, &legacySummary)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.TrimSpace(description) != "" {
		return nil
	}
	seed := strings.TrimSpace(legacySummary)
	if seed == "" {
		var kickoff string
		_ = tx.QueryRowContext(ctx, `SELECT body FROM workflow_notes
WHERE workflow_id = ? AND author != ? ORDER BY created_at, id LIMIT 1`, workflowID, WorkflowAutoNoteAuthor).Scan(&kickoff)
		seed = workflowDescriptionSeedTx(ctx, tx, workflowID, kickoff)
	}
	seed = truncateWorkflowMetaText(seed)
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_meta(workflow_id, description, updated_at)
VALUES (?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(workflow_id) DO UPDATE SET
	description = CASE WHEN workflow_meta.description = '' THEN excluded.description ELSE workflow_meta.description END,
	updated_at = CASE WHEN workflow_meta.description = '' THEN CURRENT_TIMESTAMP ELSE workflow_meta.updated_at END`, workflowID, seed)
	return err
}

func workflowDescriptionSeedTx(ctx context.Context, tx *sql.Tx, workflowID, kickoff string) string {
	for _, candidate := range []string{workflowID, kickoff} {
		for _, match := range workflowIssueReferencePattern.FindAllStringSubmatch(candidate, -1) {
			if len(match) == 2 {
				if title := workflowIssueTitleTx(ctx, tx, workflowID, match[1]); title != "" {
					return title
				}
			}
		}
	}
	if sentence := firstWorkflowSentence(kickoff); sentence != "" {
		return sentence
	}
	if _, campaign, ok := strings.Cut(workflowID, "/"); ok {
		return strings.TrimSpace(campaign)
	}
	return workflowID
}

func workflowIssueTitleTx(ctx context.Context, tx *sql.Tx, workflowID, issueNumber string) string {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM jobs INDEXED BY idx_jobs_workflow_id
WHERE workflow_id != '' AND workflow_id = ? ORDER BY created_at, id`, workflowID)
	if err != nil {
		return ""
	}
	var taskIDs []string
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			return ""
		}
		var payload struct {
			TaskID       string `json:"task_id"`
			TaskTitle    string `json:"task_title"`
			Instructions string `json:"instructions"`
		}
		if json.Unmarshal([]byte(raw), &payload) != nil {
			continue
		}
		if textReferencesWorkflowIssue(payload.TaskID, issueNumber) || textReferencesWorkflowIssue(payload.Instructions, issueNumber) {
			if title := strings.TrimSpace(payload.TaskTitle); title != "" {
				_ = rows.Close()
				return title
			}
			if taskID := strings.TrimSpace(payload.TaskID); taskID != "" {
				taskIDs = append(taskIDs, taskID)
			}
		}
	}
	_ = rows.Close()
	// Imported task rows are another cheap local title source. They cover jobs
	// whose payload carries task_id but predates the task_title projection.
	for _, taskID := range taskIDs {
		var title string
		if tx.QueryRowContext(ctx, `SELECT title FROM tasks WHERE id = ?`, taskID).Scan(&title) == nil && strings.TrimSpace(title) != "" {
			return strings.TrimSpace(title)
		}
	}
	return ""
}

func textReferencesWorkflowIssue(value, issueNumber string) bool {
	for _, match := range workflowIssueReferencePattern.FindAllStringSubmatch(value, -1) {
		if len(match) == 2 && match[1] == issueNumber {
			return true
		}
	}
	needle := "issue-" + issueNumber
	lower := strings.ToLower(value)
	index := strings.Index(lower, needle)
	if index < 0 {
		return false
	}
	end := index + len(needle)
	return end == len(lower) || lower[end] < '0' || lower[end] > '9'
}

func firstWorkflowSentence(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	for i, r := range value {
		if r == '.' || r == '!' || r == '?' {
			return strings.TrimSpace(value[:i+1])
		}
	}
	return strings.TrimSpace(value)
}

func truncateWorkflowMetaText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= WorkflowMetaTextMax {
		return value
	}
	value = value[:WorkflowMetaTextMax]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

// GetWorkflowMeta returns one workflow's latest coordinator handoff metadata.
func (s *Store) GetWorkflowMeta(ctx context.Context, workflowID string) (WorkflowMeta, error) {
	var meta WorkflowMeta
	err := s.db.QueryRowContext(ctx, `SELECT workflow_id, author, pane, session_id, workdir, summary, description, status, updated_at
FROM workflow_meta WHERE workflow_id = ?`, strings.TrimSpace(workflowID)).Scan(
		&meta.WorkflowID, &meta.Author, &meta.Pane, &meta.SessionID, &meta.WorkDir,
		&meta.Summary, &meta.Description, &meta.Status, &meta.UpdatedAt)
	return meta, err
}

// ListWorkflowMeta returns all coordinator metadata keyed by workflow id.
func (s *Store) ListWorkflowMeta(ctx context.Context) (map[string]WorkflowMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT workflow_id, author, pane, session_id, workdir, summary, description, status, updated_at FROM workflow_meta ORDER BY workflow_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WorkflowMeta{}
	for rows.Next() {
		var meta WorkflowMeta
		if err := rows.Scan(&meta.WorkflowID, &meta.Author, &meta.Pane, &meta.SessionID, &meta.WorkDir,
			&meta.Summary, &meta.Description, &meta.Status, &meta.UpdatedAt); err != nil {
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
			&item.LastNote, &item.LastAuthor, &item.LastHumanAuthor, &item.LastFailureAt,
			&item.LastNoteAt, &item.LastHumanNoteAt, &item.LastMergedReceiptAt); err != nil {
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
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Model, &job.WorkflowID, &job.Repo, &job.PullRequest,
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
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model,
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
		&item.LastHumanAuthor, &item.LastFailureAt, &item.LastNoteAt, &item.LastHumanNoteAt,
		&item.LastMergedReceiptAt)
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
