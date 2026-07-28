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
	reviewStatusUnknown    = "unknown"

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
// reviews. Eligible reviews always get an explicit state: only a workflow note
// authored by this job's reviewer and observed at or after this job opened can
// establish in_progress or stalled; missing or unreadable evidence is unknown
// rather than silently omitted.
func deriveReviewStatus(ctx context.Context, store *db.Store, job db.Job, now time.Time) string {
	if job.Type != "review" ||
		!job.ExternallyDriven ||
		job.State != string(workflow.JobRunning) {
		return ""
	}

	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil || payload.PullRequest <= 0 || strings.TrimSpace(payload.HeadSHA) == "" {
		return reviewStatusUnknown
	}
	openedAt := parseTranscriptStoreTime(job.CreatedAt)
	workflowID := strings.TrimSpace(job.WorkflowID)
	reviewer := strings.TrimSpace(job.Agent)
	if store == nil || openedAt.IsZero() || workflowID == "" || reviewer == "" {
		return reviewStatusUnknown
	}
	note, err := store.LatestWorkflowNoteByAuthor(ctx, workflowID, reviewer)
	if err != nil {
		return reviewStatusUnknown
	}
	latestSignal := parseTranscriptStoreTime(note.CreatedAt)
	if latestSignal.IsZero() || latestSignal.Before(openedAt) || now.UTC().Before(latestSignal) {
		return reviewStatusUnknown
	}
	if now.UTC().Sub(latestSignal) > sessionReviewStaleGap {
		return reviewStatusStalled
	}
	return reviewStatusInProgress
}
