package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// A review its repository's checks_backend routed remote (#2316) has nowhere
// else to run, so a full provider cap makes it WAIT instead of failing it the
// way every other remote job fails. The wait reuses the scheduler: the job goes
// back to queued with payload.blocker_retry_at set, which listPendingQueuedJobs
// already skips until it passes, and the next tick after that retries the
// reservation. Nothing polls in between.
const (
	remoteReviewCapWaitingEventKind  = "remote_review_cap_waiting"
	remoteReviewCapAdmittedEventKind = "remote_review_cap_admitted"
)

// remoteReviewCapWaitRetryInterval is how long a waiting review stays held
// before its next reservation attempt. It is a variable for tests.
var remoteReviewCapWaitRetryInterval = 30 * time.Second

// remoteReviewCapWaitDefaultBound bounds the wait when the job has no
// resolved timeout.
const remoteReviewCapWaitDefaultBound = time.Hour

var remoteReviewCapWaitNow = func() time.Time { return time.Now().UTC() }

// waitForRemoteReviewCapacity is called when provisioning a remote review
// failed. If the cause is a full provider cap and the review is checks-routed,
// it returns the admitted (running) job to the queue held until the next retry
// and reports requeued=true. The wait is bounded by bound, measured from the
// first refusal: past it, expired carries the failure the caller records
// instead of cause. Any other cause, or a lost compare-and-set, returns
// neither, and the caller fails the job as before.
func (w jobWorker) waitForRemoteReviewCapacity(ctx context.Context, job db.Job, provider string, bound time.Duration, cause error) (requeued bool, expired error) {
	var refusal *db.ExecBackendCapRefusal
	if !errors.As(cause, &refusal) || refusal.Clause == "unconfigured" {
		// An unconfigured cap never frees; waiting on it would only delay
		// the operator's fix.
		return false, nil
	}
	latest, err := w.Store.GetJob(ctx, job.ID)
	if err != nil || latest.LifecycleGeneration != job.LifecycleGeneration || latest.State != string(workflow.JobRunning) {
		return false, nil
	}
	payload, err := daemonJobPayload(latest)
	if err != nil || !payload.ReviewChecksRouted {
		return false, nil
	}
	if bound <= 0 {
		bound = remoteReviewCapWaitDefaultBound
	}
	now := remoteReviewCapWaitNow()
	since := now
	if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(payload.RemoteCapWaitSince)); err == nil {
		since = parsed
	}
	waited := now.Sub(since)
	if waited >= bound {
		return false, fmt.Errorf("remote review waited %s for %s capacity, its bound (the job timeout %s), and the provider cap is still full: %w",
			waited.Round(time.Second), provider, bound, cause)
	}
	retryAt := now.Add(remoteReviewCapWaitRetryInterval)
	if deadline := since.Add(bound); deadline.Before(retryAt) {
		retryAt = deadline
	}
	payload.RemoteCapWaitSince = since.Format(time.RFC3339Nano)
	payload.BlockerRetryAt = retryAt.Format(time.RFC3339Nano)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, nil
	}
	// The marker grants the next lifecycle its cloud attempt in
	// remoteReviewRetryAdmission: a refused reservation allocated nothing.
	event := db.JobEvent{JobID: job.ID, Kind: remoteReviewCapWaitingEventKind, Message: fmt.Sprintf(
		"waiting for %s capacity: retry at %s, waited %s of at most %s; permits lifecycle_generation=%d; %v",
		provider, retryAt.Format(time.RFC3339), waited.Round(time.Second), bound, job.LifecycleGeneration+1, cause)}
	moved, err := w.Store.TransitionJobStatePayloadWithEventAtGeneration(ctx, job.ID, string(workflow.JobRunning),
		job.LifecycleGeneration, string(workflow.JobQueued), string(encoded), event)
	if err != nil || !moved {
		return false, nil
	}
	writeLine(w.Stdout, "job %s: %s capacity full, review queued until %s: %v", job.ID, provider, retryAt.Format(time.RFC3339), cause)
	return true, nil
}

// clearRemoteReviewCapWait ends a finished wait once the review provisioned:
// it removes the hold and the wait start, so a later retry of this job waits
// against a fresh bound, and records how long the review waited.
func (w jobWorker) clearRemoteReviewCapWait(ctx context.Context, job db.Job, provider string) {
	latest, err := w.Store.GetJob(ctx, job.ID)
	if err != nil || latest.LifecycleGeneration != job.LifecycleGeneration {
		return
	}
	payload, err := daemonJobPayload(latest)
	if err != nil || strings.TrimSpace(payload.RemoteCapWaitSince) == "" {
		return
	}
	waited := ""
	if since, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(payload.RemoteCapWaitSince)); err == nil {
		waited = " after waiting " + remoteReviewCapWaitNow().Sub(since).Round(time.Second).String()
	}
	payload.RemoteCapWaitSince = ""
	payload.BlockerRetryAt = ""
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if updated, err := w.Store.UpdateJobPayloadAtGeneration(ctx, job.ID, string(encoded), job.LifecycleGeneration); err != nil || !updated {
		return
	}
	_, _ = w.Store.AddJobEventAtGeneration(ctx, db.JobEvent{JobID: job.ID, Kind: remoteReviewCapAdmittedEventKind,
		Message: fmt.Sprintf("%s capacity freed; review provisioned%s", provider, waited)}, job.LifecycleGeneration)
}
