package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	remoteReviewAvoidedStale       = "stale"
	remoteReviewAvoidedDuplicate   = "duplicate"
	remoteReviewAvoidedUnsupported = "unsupported"
	remoteReviewAvoidedRedCI       = "red_ci"
	remoteReviewAvoidedRetry       = "retry"
)

var errRemoteReviewAdmissionCancelled = errors.New("remote review admission was cancelled")

type remoteReviewAdmissionRefusal struct {
	reason  string
	message string
}

func (e *remoteReviewAdmissionRefusal) Error() string { return e.message }

type remoteReviewAdmissionGitHub interface {
	GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error)
	ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error)
}

func (w jobWorker) remoteReviewAdmissionClient(checkout string, runner subprocess.Runner) remoteReviewAdmissionGitHub {
	if w.ReviewAdmissionGitHubFactory != nil {
		return w.ReviewAdmissionGitHubFactory(checkout)
	}
	return jobGitHubClient(checkout, github.NewClient(checkout), runner)
}

// admitRemoteReview is the single cloud-review boundary. It runs after the
// exact-head checkout is bound and immediately before lifecycle provisioning,
// whose ledger wrapper reserves cost before calling the provider.
//
// The final queued->running claim is deliberate: cancellation and admission
// contend on the same durable state transition. If cancellation wins, the
// subject claim is released and no later provision can start. If admission wins,
// the job is no longer pending and Mailbox.RunClaimed continues that exact run.
func (w jobWorker) admitRemoteReview(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, backend execbackend.Backend, checkout string, runner subprocess.Runner) (bool, error) {
	if backend != execbackend.Remote || !strings.EqualFold(strings.TrimSpace(job.Type), "review") {
		return false, nil
	}
	if err := validateRuntimeExecutionBackend(agent.Runtime, backend); err != nil {
		return false, newRemoteReviewRefusal(remoteReviewAvoidedUnsupported, err.Error())
	}

	repo, err := github.ParseRepository(payload.Repo)
	if err != nil {
		return false, newRemoteReviewRefusal(remoteReviewAvoidedStale, fmt.Sprintf("remote review admission has invalid repository identity: %v", err))
	}
	if payload.PullRequest <= 0 {
		return false, newRemoteReviewRefusal(remoteReviewAvoidedStale, "remote review admission requires a pull request")
	}
	head := strings.ToLower(strings.TrimSpace(payload.HeadSHA))
	if err := dispatchHeadSHAError(head); err != nil {
		return false, newRemoteReviewRefusal(remoteReviewAvoidedStale, fmt.Sprintf("remote review admission requires an exact head: %v", err))
	}

	client := w.remoteReviewAdmissionClient(checkout, runner)
	pull, err := client.GetPullRequest(ctx, repo, int64(payload.PullRequest))
	if err != nil {
		return false, newRemoteReviewRefusal(remoteReviewAvoidedStale, fmt.Sprintf("re-read pull request head before remote review: %v", err))
	}
	currentHead := strings.ToLower(strings.TrimSpace(pull.HeadSHA))
	if currentHead != head {
		return false, newRemoteReviewRefusal(remoteReviewAvoidedStale, fmt.Sprintf("remote review head %s is stale; pull request #%d is now at %s", head, payload.PullRequest, currentHead))
	}

	paths, err := w.configPaths()
	if err != nil {
		return false, err
	}
	reviewConfig, err := config.LoadReviewConfig(paths)
	if err != nil {
		return false, fmt.Errorf("load remote review admission policy: %w", err)
	}
	if reviewConfig.For(repo.FullName()).RemoteRequireCIGreen {
		checks, err := client.ListPullRequestChecks(ctx, repo, int64(payload.PullRequest))
		if err != nil {
			return false, newRemoteReviewRefusal(remoteReviewAvoidedRedCI, fmt.Sprintf("remote review CI policy could not read current-head checks: %v", err))
		}
		if ok, detail := remoteReviewChecksGreen(checks); !ok {
			return false, newRemoteReviewRefusal(remoteReviewAvoidedRedCI, detail)
		}
	}
	if err := w.remoteReviewRetryAdmission(ctx, job); err != nil {
		return false, err
	}

	purpose := strings.ToLower(strings.TrimSpace(payload.ReviewPurpose))
	if purpose == "" {
		purpose = db.DefaultReviewPurpose
	}
	subjectKey, err := db.ReviewRequestSubjectKey(repo.FullName(), payload.PullRequest, head, purpose)
	if err != nil {
		return false, err
	}
	requester := strings.TrimSpace(payload.ReviewRequester)
	if requester == "" {
		requester = strings.TrimSpace(payload.ActingOrgRole)
	}
	if requester == "" {
		requester = strings.TrimSpace(job.Agent)
	}
	claim, won, err := w.Store.ClaimReviewRequest(ctx, subjectKey, job.ID, purpose, requester, reviewRequestOwner())
	if err != nil {
		return false, err
	}
	if !won && claim.JobID != job.ID {
		return false, newRemoteReviewRefusal(remoteReviewAvoidedDuplicate, fmt.Sprintf("remote review subject %s is already owned by job %s", subjectKey, claim.JobID))
	}

	claimed, err := w.Store.ClaimRunningJob(ctx, job.ID, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{
		JobID:   job.ID,
		Kind:    string(workflow.JobRunning),
		Message: "remote review admitted before execution-backend reservation",
	}, os.Getpid(), db.BootID())
	if err != nil {
		_ = w.Store.ReleaseReviewRequest(context.WithoutCancel(ctx), subjectKey, job.ID)
		return false, err
	}
	if !claimed {
		_ = w.Store.ReleaseReviewRequest(context.WithoutCancel(ctx), subjectKey, job.ID)
		latest, getErr := w.Store.GetJob(ctx, job.ID)
		if getErr != nil {
			return false, getErr
		}
		if latest.State == string(workflow.JobCancelled) {
			return false, errRemoteReviewAdmissionCancelled
		}
		return false, fmt.Errorf("remote review admission lost queued claim: job %s is %s", job.ID, latest.State)
	}
	return true, nil
}

func newRemoteReviewRefusal(reason, message string) error {
	return &remoteReviewAdmissionRefusal{reason: reason, message: message}
}

func remoteReviewChecksGreen(checks []github.PullRequestCheck) (bool, string) {
	if len(checks) == 0 {
		return false, "remote review CI policy requires at least one current-head check, but none were reported"
	}
	for _, check := range checks {
		bucket := strings.ToLower(strings.TrimSpace(check.Bucket))
		state := strings.ToLower(strings.TrimSpace(check.State))
		passed := false
		if bucket != "" {
			passed = bucket == "pass" || bucket == "skipping"
		} else {
			passed = state == "success" || state == "skipped" || state == "neutral"
		}
		if !passed {
			return false, fmt.Sprintf("remote review CI policy requires green current-head checks; %q is %s", check.Name, firstNonEmptyRemoteReview(bucket, state, "unknown"))
		}
	}
	return true, ""
}

func firstNonEmptyRemoteReview(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (w jobWorker) remoteReviewRetryAdmission(ctx context.Context, job db.Job) error {
	attempts, err := w.Store.ListExecBackendAttemptsForJob(ctx, job.ID)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		return nil
	}
	generation := job.LifecycleGeneration
	for _, attempt := range attempts {
		if attempt.LifecycleGeneration == generation {
			return newRemoteReviewRefusal(remoteReviewAvoidedRetry, fmt.Sprintf("remote review job %s already consumed its cloud attempt for lifecycle_generation=%d", job.ID, generation))
		}
	}

	events, err := w.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return err
	}
	marker := "lifecycle_generation=" + strconv.FormatInt(generation, 10)
	for _, event := range events {
		if (event.Kind == "retry_queued" || event.Kind == "remote_review_provider_retryable") && strings.Contains(event.Message, marker) {
			return nil
		}
	}
	return newRemoteReviewRefusal(remoteReviewAvoidedRetry, fmt.Sprintf("remote review job %s already used its one cloud attempt for this exact-head subject; retry requires explicit operator action or a classified retryable provider failure", job.ID))
}

func (w jobWorker) recordRemoteReviewAdmissionRefusal(ctx context.Context, job db.Job, err error) error {
	var refusal *remoteReviewAdmissionRefusal
	if !errors.As(err, &refusal) {
		return w.finishQueuedJob(ctx, job, workflow.JobFailed, err)
	}
	kind := "remote_review_admission_avoided_" + refusal.reason
	if _, eventErr := w.Store.ClaimJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: kind, Message: refusal.message}); eventErr != nil {
		return eventErr
	}
	return w.finishQueuedJob(ctx, job, workflow.JobFailed, err)
}

func (w jobWorker) recordRetryableRemoteProviderFailure(ctx context.Context, job db.Job, err error) {
	var refused *e2b.RequestRefusedError
	if !errors.As(err, &refused) || refused.Operation != e2b.OperationCreate || !remoteReviewProviderStatusRetryable(refused.StatusCode) {
		return
	}
	message := fmt.Sprintf("retryable provider create refusal status=%d permits lifecycle_generation=%d", refused.StatusCode, job.LifecycleGeneration+1)
	_, _ = w.Store.ClaimJobEvent(context.WithoutCancel(ctx), db.JobEvent{
		JobID: job.ID, Kind: "remote_review_provider_retryable", Message: message,
	})
}

func remoteReviewProviderStatusRetryable(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}
