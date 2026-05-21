package workflow

import (
	"context"
	"fmt"

	"github.com/jerryfane/gitmoot/internal/db"
)

func RetryJob(ctx context.Context, store *db.Store, jobID string) (db.Job, error) {
	if store == nil {
		return db.Job{}, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, err
	}
	switch job.State {
	case string(JobFailed), string(JobBlocked), string(JobCancelled):
	default:
		return db.Job{}, fmt.Errorf("job %s is %s; retry requires failed, blocked, or cancelled", job.ID, job.State)
	}
	if job.State == string(JobCancelled) {
		fromRunning, err := latestCancellationWasFromRunning(ctx, store, job.ID)
		if err != nil {
			return db.Job{}, err
		}
		if fromRunning {
			return db.Job{}, fmt.Errorf("job %s was cancelled while running; wait for the active worker to settle before retrying", job.ID)
		}
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return db.Job{}, err
	}
	payload.Result = nil
	encoded, err := marshalPayload(payload)
	if err != nil {
		return db.Job{}, err
	}
	transitioned, err := store.TransitionJobStatePayloadWithEvent(ctx, job.ID, job.State, string(JobQueued), encoded, db.JobEvent{
		JobID:   job.ID,
		Kind:    "retry_queued",
		Message: fmt.Sprintf("retry requested from %s", job.State),
	})
	if err != nil {
		return db.Job{}, err
	}
	if !transitioned {
		latest, getErr := store.GetJob(ctx, job.ID)
		if getErr != nil {
			return db.Job{}, getErr
		}
		return db.Job{}, fmt.Errorf("job %s is %s; retry requires failed, blocked, or cancelled", latest.ID, latest.State)
	}
	return store.GetJob(ctx, job.ID)
}

func latestCancellationWasFromRunning(ctx context.Context, store *db.Store, jobID string) (bool, error) {
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Kind == "cancel_settled" {
			return false, nil
		}
		if event.Kind == string(JobCancelled) {
			return event.Message == "cancel requested from running", nil
		}
	}
	return false, nil
}

func SettleCancelledRunningJob(ctx context.Context, store *db.Store, jobID string, message string) (bool, error) {
	if store == nil {
		return false, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	if job.State != string(JobCancelled) {
		return false, nil
	}
	fromRunning, err := latestCancellationWasFromRunning(ctx, store, job.ID)
	if err != nil {
		return false, err
	}
	if !fromRunning {
		return false, nil
	}
	if message == "" {
		message = "cancelled job worker settled"
	}
	return true, store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "cancel_settled", Message: message})
}

func CancelJob(ctx context.Context, store *db.Store, jobID string) (db.Job, error) {
	if store == nil {
		return db.Job{}, fmt.Errorf("store is required")
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, err
	}
	switch job.State {
	case string(JobQueued), string(JobRunning):
	default:
		return db.Job{}, fmt.Errorf("job %s is %s; cancel requires queued or running", job.ID, job.State)
	}
	transitioned, err := store.TransitionJobStateWithEvent(ctx, job.ID, job.State, string(JobCancelled), db.JobEvent{
		JobID:   job.ID,
		Kind:    string(JobCancelled),
		Message: fmt.Sprintf("cancel requested from %s", job.State),
	})
	if err != nil {
		return db.Job{}, err
	}
	if !transitioned {
		latest, getErr := store.GetJob(ctx, job.ID)
		if getErr != nil {
			return db.Job{}, getErr
		}
		return db.Job{}, fmt.Errorf("job %s is %s; cancel requires queued or running", latest.ID, latest.State)
	}
	return store.GetJob(ctx, job.ID)
}
