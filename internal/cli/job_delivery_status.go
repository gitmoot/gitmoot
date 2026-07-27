package cli

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	jobDeliveryStatusDelivered = "delivered"
	jobDeliveryStatusPending   = "pending"
	jobDeliveryStatusBlocked   = "blocked"
)

// deliveryStatusEventKinds contains every marker that can supersede an older
// delivery status. The first four are the affirmative status signals; the
// remaining markers suppress a stale status when advancement moved into another
// lifecycle state that this badge cannot classify confidently.
var deliveryStatusEventKinds = []string{
	"advance_started",
	"advance_retry",
	"advance_completed",
	"advance_blocked",
	"advance_retried",
	"advance_retry_skipped",
	"advance_awaiting_human",
	"retry_queued",
}

// deriveJobDeliveryStatus reports Gitmoot's own post-agent delivery status for
// implement jobs. An empty string means unknown/not applicable and is omitted
// from JSON. A persisted pull request is conclusive even when a later workflow
// advancement marker records work after delivery.
func deriveJobDeliveryStatus(job db.Job, payload workflow.JobPayload, latest db.JobEvent, hasLatest bool) string {
	if job.Type != "implement" {
		return ""
	}
	if payload.PullRequest > 0 {
		return jobDeliveryStatusDelivered
	}
	if !hasLatest {
		return ""
	}
	switch latest.Kind {
	case "advance_completed", "advance_retried":
		return jobDeliveryStatusDelivered
	case "advance_started", "advance_retry":
		return jobDeliveryStatusPending
	case "advance_blocked":
		return jobDeliveryStatusBlocked
	default:
		return ""
	}
}

// loadJobDeliveryStatus is the job-show counterpart to the bulk latest-event
// query used by job list. Lookup failures stay silent: an unknown status must
// never be presented as a delivery verdict.
func loadJobDeliveryStatus(store *db.Store, job db.Job, payload workflow.JobPayload) string {
	if job.Type != "implement" {
		return ""
	}
	if payload.PullRequest > 0 {
		return jobDeliveryStatusDelivered
	}
	events, err := store.ListJobEvents(context.Background(), job.ID)
	if err != nil {
		return ""
	}
	latest, ok := latestDeliveryStatusEvent(events)
	return deriveJobDeliveryStatus(job, payload, latest, ok)
}

func latestDeliveryStatusEvent(events []db.JobEvent) (db.JobEvent, bool) {
	var latest db.JobEvent
	var found bool
	for _, event := range events {
		for _, kind := range deliveryStatusEventKinds {
			if event.Kind == kind {
				latest, found = event, true
				break
			}
		}
	}
	return latest, found
}
