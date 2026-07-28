package cli

import (
	"context"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/evidence"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	reviewStatusInProgress = "in_progress"
	reviewStatusStalled    = "stalled"

	reviewStatusAuthorityNonAuthoritative = "non_authoritative"

	// sessionReviewStaleGap is four times the existing five-minute
	// unconfirmed-activity precedent. That leaves room for a substantive review
	// pass between journal notes while making a silent lane visible well before
	// the 24-hour orphan reaper.
	sessionReviewStaleGap = 20 * time.Minute
)

// reviewStatusDisplay is intentionally incapable of expressing an authoritative
// verdict. The liveness signal comes from caller-authored workflow notes, so the
// strongest honest evidence grade is reported.
type reviewStatusDisplay struct {
	Status    string
	Grade     evidence.Grade
	Authority string
	HeadSHA   string
}

func reportedReviewStatus(status string) reviewStatusDisplay {
	if strings.TrimSpace(status) == "" {
		return reviewStatusDisplay{}
	}
	return reviewStatusDisplay{
		Status:    status,
		Grade:     evidence.GradeReported,
		Authority: reviewStatusAuthorityNonAuthoritative,
	}
}

func deriveReviewStatuses(ctx context.Context, store *db.Store, jobs []db.Job, now time.Time) map[string]reviewStatusDisplay {
	statuses := make(map[string]reviewStatusDisplay)
	for _, job := range jobs {
		if status := deriveReviewStatus(ctx, store, job, now); status.Status != "" || status.HeadSHA != "" {
			statuses[job.ID] = status
		}
	}
	return statuses
}

// deriveReviewStatus projects a display-only liveness hint for running,
// externally-driven reviews. Workflow notes are caller assertions, not observed
// reviewer identity, so the result is always reported/non-authoritative and is
// never suitable for merge policy. Every unknown is neutral: ineligible jobs, a
// failed note lookup, or an unparseable/future timestamp omit the display.
func deriveReviewStatus(ctx context.Context, store *db.Store, job db.Job, now time.Time) reviewStatusDisplay {
	if store == nil ||
		job.Type != "review" ||
		!job.ExternallyDriven {
		return reviewStatusDisplay{}
	}
	display := reviewStatusDisplay{HeadSHA: loadSessionJobDisplayHeadSHA(ctx, store, job.ID)}
	if job.State != string(workflow.JobRunning) {
		return display
	}

	latestSignal := parseTranscriptStoreTime(job.CreatedAt)
	if latestSignal.IsZero() {
		return display
	}
	if workflowID := strings.TrimSpace(job.WorkflowID); workflowID != "" {
		notes, err := store.ListWorkflowNotes(ctx, workflowID, 1)
		if err != nil {
			return display
		}
		if len(notes) > 0 {
			noteAt := parseTranscriptStoreTime(notes[len(notes)-1].CreatedAt)
			if noteAt.IsZero() {
				return display
			}
			// A workflow label may be reused after older notes already exist.
			// Opening this job is itself the newer signal in that case.
			if noteAt.After(latestSignal) {
				latestSignal = noteAt
			}
		}
	}
	now = now.UTC()
	if now.Before(latestSignal) {
		return display
	}
	if now.Sub(latestSignal) > sessionReviewStaleGap {
		status := reportedReviewStatus(reviewStatusStalled)
		status.HeadSHA = display.HeadSHA
		return status
	}
	status := reportedReviewStatus(reviewStatusInProgress)
	status.HeadSHA = display.HeadSHA
	return status
}

func loadSessionJobDisplayHeadSHA(ctx context.Context, store *db.Store, jobID string) string {
	if store == nil || strings.TrimSpace(jobID) == "" {
		return ""
	}
	event, ok, err := store.GetLatestJobEventByKind(ctx, jobID, workflow.SessionJobDisplayEventKind)
	if err != nil || !ok {
		return ""
	}
	display, ok := workflow.ParseSessionJobDisplayEvent(event)
	if !ok {
		return ""
	}
	return display.HeadSHA
}
