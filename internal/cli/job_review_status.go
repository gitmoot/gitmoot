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

	sessionReviewHeartbeatEventKind = "session_review_heartbeat"

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
// reviews. Eligible reviews always get an explicit state: only a heartbeat
// event bound to this exact review job can establish in_progress or stalled;
// missing or unreadable evidence is unknown rather than silently omitted.
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
	if store == nil || openedAt.IsZero() || workflowID == "" {
		return reviewStatusUnknown
	}
	heartbeat, ok, err := store.GetLatestJobEventByKind(ctx, job.ID, sessionReviewHeartbeatEventKind)
	if err != nil || !ok {
		return reviewStatusUnknown
	}
	latestSignal := parseTranscriptStoreTime(heartbeat.CreatedAt)
	if latestSignal.IsZero() || latestSignal.Before(openedAt) || now.UTC().Before(latestSignal) {
		return reviewStatusUnknown
	}
	if now.UTC().Sub(latestSignal) > sessionReviewStaleGap {
		return reviewStatusStalled
	}
	return reviewStatusInProgress
}
