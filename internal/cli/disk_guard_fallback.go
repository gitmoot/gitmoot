package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/execbackend"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2372: when a review cannot run on this host for capacity reasons, it runs
// on the remote (E2B) backend by default, and the agent that asked for it is
// told. Three pieces live here:
//
//   - diskGuardRemoteRouteCheck decides whether a queued review CAN run
//     remotely. A review on a runtime or model that cannot run remotely is
//     refused with a reason; it is never given a different runtime or model.
//   - diskGuardReviewBandDispatch is the review-only trigger: below
//     [disk_guard] remote_reviews_below_bytes but above the dispatch floor,
//     reviews move to the remote backend (or wait, with an alert) while every
//     other job keeps dispatching locally.
//   - diskGuardAlert sends the requesting role one gitmoot message per
//     job and outcome.

const (
	// diskGuardReviewHeldEventKind marks a review that is held off this host
	// for capacity and cannot run remotely; its alert is sent once.
	diskGuardReviewHeldEventKind = "disk_guard_review_held"
	// diskGuardRoutedAlertEventKind marks that the requester was told about a
	// remote route; it keeps the alert to one per job across daemon passes.
	diskGuardRoutedAlertEventKind = "disk_guard_route_alerted"
	// diskGuardRouteUndoneAlertEventKind marks that the requester was told a
	// remote run did not start and the review went back to waiting locally.
	diskGuardRouteUndoneAlertEventKind = "disk_guard_route_undone_alerted"
)

// diskGuardRemoteRouteCheck reports why a local review cannot run remotely, or
// nil when it can. Exact pull-request head, no ephemeral agent, no earlier
// cloud attempt (remote admission allows one), and a runtime and model that
// run remotely.
func diskGuardRemoteRouteCheck(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload) error {
	head := strings.ToLower(strings.TrimSpace(payload.HeadSHA))
	if payload.PullRequest <= 0 || head == "" || dispatchHeadSHAError(head) != nil {
		return fmt.Errorf("the review has no exact pull-request head, which a remote run requires")
	}
	if payload.Ephemeral != nil {
		return fmt.Errorf("an ephemeral reviewer cannot run remotely")
	}
	if payload.ReviewNoExecution {
		return fmt.Errorf("a no-execution review runs only on the local backend (#2364)")
	}
	attempts, err := store.ListExecBackendAttemptsForJob(ctx, job.ID)
	if err != nil {
		return fmt.Errorf("read earlier cloud attempts: %w", err)
	}
	if len(attempts) > 0 {
		return fmt.Errorf("the review already had a cloud attempt; remote admission allows one")
	}
	runtimeName := strings.TrimSpace(payload.RuntimeOverride)
	if runtimeName == "" {
		agent, err := store.GetAgent(ctx, job.Agent)
		if err != nil {
			return fmt.Errorf("read reviewer agent %q: %w", job.Agent, err)
		}
		runtimeName = agent.Runtime
	}
	if !remoteCapableRuntime(runtimeName) {
		return fmt.Errorf("runtime %q cannot run remotely (remote runtimes: %s)", runtimeName, remoteCapableRuntimeNames())
	}
	if runtimeName == runtime.OmpRuntime && !remoteOmpModelAuthenticates(payload.Model) {
		return fmt.Errorf("model %q is not available to omp in the remote sandbox", payload.Model)
	}
	return nil
}

// diskGuardAlertRecipient is the role told about a capacity move or hold: the
// role that requested the review, else the role that dispatched it.
func diskGuardAlertRecipient(payload workflow.JobPayload) string {
	for _, role := range []string{payload.ReviewRequester, payload.ActingOrgRole} {
		if role = strings.ToLower(strings.TrimSpace(role)); role != "" {
			return role
		}
	}
	return ""
}

// diskGuardAlert records kind for the job and, the first time only, sends the
// requesting role a gitmoot message. A job with no requesting role gets the
// event only. Failures are logged, never fatal: the alert must not stop the
// review from being routed or held.
func diskGuardAlert(ctx context.Context, worker jobWorker, job db.Job, payload workflow.JobPayload, kind, text string) {
	first, err := worker.Store.ClaimJobEventKind(ctx, db.JobEvent{JobID: job.ID, Kind: kind, Message: text})
	if err != nil {
		writeLine(worker.Stdout, "job %s: record %s: %v", job.ID, kind, err)
		return
	}
	if !first {
		return
	}
	recipient := diskGuardAlertRecipient(payload)
	if recipient == "" {
		return
	}
	body := fmt.Sprintf("Review job %s (%s#%d at %s): %s", job.ID, payload.Repo, payload.PullRequest, shortReviewHead(payload.HeadSHA), text)
	if _, err := worker.Store.CreateMessage(ctx, db.Message{Sender: db.MessageSystemSender, Recipient: recipient, Body: body}); err != nil {
		writeLine(worker.Stdout, "job %s: alert %s to %s: %v", job.ID, kind, recipient, err)
	}
}

// diskGuardReviewBandDispatch handles free space below remote_reviews_below_bytes
// but above the dispatch floor: reviews move to the remote backend or wait
// (with an alert), and every other job dispatches as normal.
func diskGuardReviewBandDispatch(ctx context.Context, worker jobWorker, jobs, matching []db.Job, evaluation diskGuardEvaluation) []db.Job {
	reason := fmt.Sprintf("free space %d bytes is below the review threshold [disk_guard] remote_reviews_below_bytes=%d", evaluation.Usage.FreeBytes, evaluation.Policy.RemoteReviewsBelowBytes)
	remote := diskGuardRemoteReviews(ctx, worker, matching, reason)
	routed := make(map[string]bool, len(remote))
	paths, pathsErr := diskGuardPaths(worker)
	for _, job := range remote {
		payload, err := daemonJobPayload(job)
		if err != nil || pathsErr != nil {
			continue
		}
		route, checksRouted, err := loadReviewChecksRoute(paths, payload.Repo)
		if err != nil {
			diskGuardHoldReview(ctx, worker, job, payload, reason, err)
			continue
		}
		e2b := false
		if checksRouted {
			e2b = route.Provider == config.RemoteExecProviderE2B
		} else {
			backend, cfg, err := worker.resolveExecutionBackendForJob(job, payload)
			e2b = err == nil && backend == execbackend.Remote && !config.IsSandboxdProvider(cfg.Provider)
		}
		if !e2b {
			diskGuardHoldReview(ctx, worker, job, payload, reason, fmt.Errorf("warning-band reviews require E2B; the frozen or repository checks route selects another backend"))
			continue
		}
		routed[job.ID] = true
	}
	held := make(map[string]bool)
	remoteAvailable := diskGuardRemoteAvailable(worker)
	for _, job := range matching {
		if strings.EqualFold(strings.TrimSpace(job.Type), "review") && !routed[job.ID] {
			held[job.ID] = true
			// No silent starvation (#2373 review, P3): every held review gets a
			// durable event and one alert naming why. A review whose route plan
			// already failed has been alerted with that cause; the atomic
			// once-per-kind claim makes this call a no-op for it.
			if payload, err := daemonJobPayload(job); err == nil {
				diskGuardHoldReview(ctx, worker, job, payload, reason, diskGuardBandHoldCause(job, payload, remoteAvailable))
			}
		}
	}
	out := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if held[job.ID] {
			continue
		}
		if routed[job.ID] {
			for _, r := range remote {
				if r.ID == job.ID {
					job = r // the routed payload
					break
				}
			}
		}
		out = append(out, job)
	}
	return out
}

// diskGuardHoldReview alerts that a review cannot run here for capacity and
// cannot run remotely either, so it waits.
func diskGuardHoldReview(ctx context.Context, worker jobWorker, job db.Job, payload workflow.JobPayload, reason string, cause error) {
	diskGuardAlert(ctx, worker, job, payload, diskGuardReviewHeldEventKind,
		fmt.Sprintf("cannot run on this host now (%s) and cannot run on the remote backend: %v. It waits for local capacity; change the request to make it remote-capable if it should not wait.", reason, cause))
}

// diskGuardRoutedAlertText describes a remote move for the requester.
func diskGuardRoutedAlertText(reason string) string {
	return "moved to the remote (E2B) backend because it cannot run on this host now: " + reason + "."
}

// diskGuardRemoteAvailable reports whether the remote (E2B) backend is
// configured for disk-guard routing at all.
func diskGuardRemoteAvailable(worker jobWorker) bool {
	backend, _, err := daemonJobExecBackendFor(worker, string(execbackend.Remote), true)
	return err == nil && backend == execbackend.Remote
}

// diskGuardBandHoldCause names why a review in the review band is held
// instead of moved to the remote backend.
func diskGuardBandHoldCause(job db.Job, payload workflow.JobPayload, remoteAvailable bool) error {
	switch {
	case !remoteAvailable:
		return errors.New("the remote (E2B) backend is not configured or not available on this daemon")
	case payload.DiskGuardRouteDeclined:
		return errors.New("an earlier remote run of this review was refused, so it is not moved again")
	case payload.PolicyRoutedReview:
		return errors.New("its review policy route is still being decided")
	case queuedJobBlockerHeld(job, time.Now().UTC()):
		return errors.New("it is waiting for its blocker retry time")
	}
	if name, present := payload.ExecBackendOverride(); present {
		return fmt.Errorf("it was requested with --exec-backend %s", name)
	}
	return errors.New("it is not eligible for the remote route")
}
