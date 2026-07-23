package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// PullRequestJournalTransition is the stable dedupe component written into a
// daemon-authored workflow note. Values are wire/storage keys; never rename one
// after release or an old transition could be emitted again.
type PullRequestJournalTransition string

const (
	PullRequestJournalOpened PullRequestJournalTransition = "opened"
	PullRequestJournalReady  PullRequestJournalTransition = "ready"
	PullRequestJournalMerged PullRequestJournalTransition = "merged"
	PullRequestJournalClosed PullRequestJournalTransition = "closed"
)

// RecordPullRequestWorkflowTransition appends one machine-owned PR breadcrumb
// and updates live status for the workflow linked to repo+PR/branch unless an
// operator-owned terminal/hold status is present. A missing workflow link is a
// successful no-op. The store's partial unique index makes the structured body
// an at-most-once receipt for (workflow, PR, transition).
func RecordPullRequestWorkflowTransition(ctx context.Context, store *db.Store, event PullRequestEvent, transition PullRequestJournalTransition) (bool, error) {
	if store == nil {
		return false, nil
	}
	workflowID, err := store.WorkflowIDForPullRequest(ctx, event.Repo, event.PullRequest, event.Branch)
	if err != nil || strings.TrimSpace(workflowID) == "" {
		return false, err
	}
	// Catch-up polling may observe an old PR-open state after a later lifecycle
	// receipt already exists (for example immediately after an upgrade). Keep the
	// missing older breadcrumb from regressing the current live status.
	for _, later := range laterPullRequestJournalTransitions(transition) {
		key := fmt.Sprintf("[auto:pr:%d:%s]", event.PullRequest, later)
		exists, err := store.WorkflowAutoNoteExists(ctx, workflowID, key)
		if err != nil {
			return false, err
		}
		if exists {
			return false, nil
		}
	}
	var message, status string
	switch transition {
	case PullRequestJournalOpened:
		message = fmt.Sprintf("PR #%d opened (%s)", event.PullRequest, strings.TrimSpace(event.Branch))
		status = "active"
	case PullRequestJournalReady:
		message = fmt.Sprintf("PR #%d checks green — ready to merge", event.PullRequest)
		status = "ready_to_merge"
	case PullRequestJournalMerged:
		message = fmt.Sprintf("PR #%d merged", event.PullRequest)
		status = "active"
	case PullRequestJournalClosed:
		message = fmt.Sprintf("PR #%d closed without merging", event.PullRequest)
		status = "active"
	default:
		return false, fmt.Errorf("unknown pull request journal transition %q", transition)
	}
	note := db.WorkflowNote{
		WorkflowID: workflowID,
		Author:     db.WorkflowAutoNoteAuthor,
		Body:       fmt.Sprintf("[auto:pr:%d:%s] %s", event.PullRequest, transition, message),
		Repo:       strings.TrimSpace(event.Repo),
	}
	_, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx, note, db.WorkflowMeta{
		WorkflowID: workflowID,
		Status:     status,
		StatusSet:  true,
	})
	return inserted, err
}

func laterPullRequestJournalTransitions(transition PullRequestJournalTransition) []PullRequestJournalTransition {
	switch transition {
	case PullRequestJournalOpened:
		return []PullRequestJournalTransition{PullRequestJournalReady, PullRequestJournalClosed, PullRequestJournalMerged}
	case PullRequestJournalReady:
		return []PullRequestJournalTransition{PullRequestJournalClosed, PullRequestJournalMerged}
	case PullRequestJournalClosed:
		return []PullRequestJournalTransition{PullRequestJournalMerged}
	default:
		return nil
	}
}
