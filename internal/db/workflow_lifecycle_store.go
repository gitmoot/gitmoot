package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
