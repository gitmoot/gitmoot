package cli

import (
	"context"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	reviewStatusInProgress = "in_progress"
	reviewStatusStalled    = "stalled"

	// sessionReviewStaleGap is four times the existing five-minute
	// unconfirmed-activity precedent. That leaves enough room for a substantive
	// review pass between journal notes while making a silent lane visible well
	// before the 24-hour orphan reaper.
	sessionReviewStaleGap = 20 * time.Minute
)

func deriveReviewStatuses(ctx context.Context, store *db.Store, jobs []db.Job, now time.Time) map[string]string {
	statuses := make(map[string]string)
	for _, job := range jobs {
		if status := deriveReviewStatus(ctx, store, job, now); status != "" {
			statuses[job.ID] = status
		}
	}
	return statuses
}

// deriveReviewStatus projects liveness only for running, externally-driven
// reviews. Every unknown is neutral: ineligible jobs, a failed note lookup, or
// an unparseable signal timestamp omit the field rather than inventing a state.
func deriveReviewStatus(ctx context.Context, store *db.Store, job db.Job, now time.Time) string {
	if store == nil ||
		job.Type != "review" ||
		!job.ExternallyDriven ||
		job.State != string(workflow.JobRunning) {
		return ""
	}

	latestSignal := parseTranscriptStoreTime(job.CreatedAt)
	if latestSignal.IsZero() {
		return ""
	}
	if workflowID := strings.TrimSpace(job.WorkflowID); workflowID != "" {
		notes, err := store.ListWorkflowNotes(ctx, workflowID, 1)
		if err != nil {
			return ""
		}
		if len(notes) > 0 {
			noteAt := parseTranscriptStoreTime(notes[len(notes)-1].CreatedAt)
			if noteAt.IsZero() {
				return ""
			}
			// A workflow label may be reused after older notes already exist.
			// Opening this job is itself the newer signal in that case.
			if noteAt.After(latestSignal) {
				latestSignal = noteAt
			}
		}
	}
	if now.UTC().Sub(latestSignal) > sessionReviewStaleGap {
		return reviewStatusStalled
	}
	return reviewStatusInProgress
}
