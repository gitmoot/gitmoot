package workflow

import (
	"context"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

// FindLiveTaskJob returns one job that still owns lifecycle progress for task.
// Matching is deliberately broader than implement jobs: a redirected canonical
// branch can be advanced by any job type, so either the payload task id or the
// exact non-empty repo+branch identity is sufficient.
func FindLiveTaskJob(ctx context.Context, store *db.Store, task db.Task) (db.Job, bool, error) {
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return db.Job{}, false, err
	}
	for _, job := range jobs {
		payload, err := ParseJobPayload(job.Payload)
		if err != nil || !jobMatchesTask(payload, task) {
			continue
		}
		if job.State == string(JobQueued) || job.State == string(JobRunning) {
			return job, true, nil
		}
		events, err := store.ListJobEvents(ctx, job.ID)
		if err != nil {
			return db.Job{}, false, err
		}
		if jobKeepsTaskLive(job, events) {
			return job, true, nil
		}
	}
	return db.Job{}, false, nil
}

func jobMatchesTask(payload JobPayload, task db.Task) bool {
	if taskID := strings.TrimSpace(task.ID); taskID != "" && strings.TrimSpace(payload.TaskID) == taskID {
		return true
	}
	branch := strings.TrimSpace(task.Branch)
	return branch != "" && strings.TrimSpace(payload.Branch) == branch &&
		strings.TrimSpace(payload.Repo) == strings.TrimSpace(task.RepoFullName)
}

func jobKeepsTaskLive(job db.Job, events []db.JobEvent) bool {
	switch JobState(job.State) {
	case JobQueued, JobRunning:
		return true
	}
	if IsSettledJobState(job.State) && advancementPending(events) {
		return true
	}
	return job.State == string(JobCancelled) && cancellationFromRunningUnsettled(events)
}

func advancementPending(events []db.JobEvent) bool {
	pending := false
	seen := false
	for _, event := range events {
		switch event.Kind {
		case "advance_started", "advance_retry":
			pending = true
			seen = true
		case "advance_completed", "advance_retried", "advance_blocked", "advance_retry_skipped", "retry_queued":
			pending = false
			seen = true
		}
	}
	return seen && pending
}

func cancellationFromRunningUnsettled(events []db.JobEvent) bool {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Kind {
		case "cancel_settled":
			return false
		case string(JobCancelled), JobEventSupersededStaleHead:
			return strings.HasPrefix(events[i].Message, "cancel requested from running")
		}
	}
	return false
}
