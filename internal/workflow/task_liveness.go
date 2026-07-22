package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

const (
	TaskEventBlockedTerminalNoPR = "task_blocked_terminal_no_pr"
	TaskEventBlockedJobFailed    = "task_blocked_job_failed"
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

// ReconcileTerminalDrivingJob closes the post-advancement lifecycle gap for a
// top-level implement job that has finished without a pull request or any live
// successor. It must be called only after RunJob/AdvanceJob (and delegation
// dispatch) has settled: FindLiveTaskJob is the authoritative successor/pending-
// advancement gate, so queued retries and continuation work keep the task live.
// Delegation children are excluded because their parent DAG owns advancement.
func (e Engine) ReconcileTerminalDrivingJob(ctx context.Context, jobID string) error {
	if err := e.validate(); err != nil {
		return err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Type != "implement" || strings.TrimSpace(payload.ParentJobID) != "" || strings.TrimSpace(payload.DelegationID) != "" {
		return nil
	}
	if !IsSettledJobState(job.State) || payload.PullRequest > 0 || strings.TrimSpace(payload.TaskID) == "" {
		return nil
	}
	task, err := e.Store.GetTask(ctx, payload.TaskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if TaskState(task.State) != TaskImplementing {
		return nil
	}
	if _, live, err := FindLiveTaskJob(ctx, e.Store, task); err != nil {
		return err
	} else if live {
		return nil
	}

	kind := TaskEventBlockedJobFailed
	reason := fmt.Sprintf("top-level implement job %s ended in %s without a pull request or live successor", job.ID, job.State)
	if job.State == string(JobSucceeded) && payload.Result != nil && payload.Result.Decision == "implemented" {
		kind = TaskEventBlockedTerminalNoPR
		reason = fmt.Sprintf("top-level implement job %s succeeded with decision implemented but produced no pull request or live successor", job.ID)
	} else if payload.Result != nil && strings.TrimSpace(payload.Result.Decision) != "" {
		reason = fmt.Sprintf("top-level implement job %s ended in %s with decision %s and no pull request or live successor", job.ID, job.State, payload.Result.Decision)
	}
	_, _, err = e.Store.TransitionTaskStateWithEvent(ctx, task.ID,
		[]string{string(TaskImplementing)}, string(TaskBlocked), kind, reason)
	return err
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
