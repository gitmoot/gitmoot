package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2372: when a review cannot run on this host for capacity reasons, it runs
// on the remote (E2B) backend by default, and the agent that asked for it is
// told. Three pieces live here:
//
//   - diskGuardRemoteRoutePlan decides whether a queued review CAN run
//     remotely and with what runtime and model. A review on a runtime that
//     cannot run remotely (codex) runs as omp with the SAME model; it is never
//     given a different model. Anything else that cannot run remotely is
//     refused with a reason.
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
	// diskGuardRuntimeChangedEventKind records a codex->omp switch.
	diskGuardRuntimeChangedEventKind = "disk_guard_runtime_changed"
	// diskGuardRoutedAlertEventKind marks that the requester was told about a
	// remote route; it keeps the alert to one per job across daemon passes.
	diskGuardRoutedAlertEventKind = "disk_guard_route_alerted"
	// diskGuardRouteUndoneAlertEventKind marks that the requester was told a
	// remote run did not start and the review went back to waiting locally.
	diskGuardRouteUndoneAlertEventKind = "disk_guard_route_undone_alerted"
)

// diskGuardRoutePlan is how a queued review would run remotely.
type diskGuardRoutePlan struct {
	// runtimeOverride and model are set only when the review's own runtime
	// cannot run remotely and is replaced by omp with the same model.
	runtimeOverride string
	model           string
	// fromRuntime names the runtime that was replaced, for the event/alert.
	fromRuntime string
}

// diskGuardOmpModelFor maps a codex model to the same model under omp. omp
// names codex-served models "openai-codex/<model>"; a model that already names
// a provider is kept as is. An empty model has no known identity to keep, so
// it is refused rather than guessed.
func diskGuardOmpModelFor(codexModel string) (string, error) {
	model := strings.TrimSpace(codexModel)
	if model == "" {
		return "", fmt.Errorf("the review names no model, so it cannot be moved to omp with the same model; re-request it with --runtime omp --model <provider/model>")
	}
	if !strings.Contains(model, "/") {
		model = "openai-codex/" + model
	}
	if !remoteOmpModelAuthenticates(model) {
		provider, _, _ := strings.Cut(model, "/")
		return "", fmt.Errorf("model %q is not available to omp in the remote sandbox (provider %q has no credential route there); no other model is substituted", model, provider)
	}
	return model, nil
}

// diskGuardRemoteRoutePlan reports how a local review can run remotely, or
// why it cannot. Exact pull-request head, no ephemeral agent, no earlier
// cloud attempt (remote admission allows one), and a runtime that runs
// remotely, possibly after the codex->omp switch.
func diskGuardRemoteRoutePlan(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload) (diskGuardRoutePlan, error) {
	head := strings.ToLower(strings.TrimSpace(payload.HeadSHA))
	if payload.PullRequest <= 0 || head == "" || dispatchHeadSHAError(head) != nil {
		return diskGuardRoutePlan{}, fmt.Errorf("the review has no exact pull-request head, which a remote run requires")
	}
	if payload.Ephemeral != nil {
		return diskGuardRoutePlan{}, fmt.Errorf("an ephemeral reviewer cannot run remotely")
	}
	if payload.ReviewNoExecution {
		return diskGuardRoutePlan{}, fmt.Errorf("a no-execution review runs only on the local backend (#2364)")
	}
	attempts, err := store.ListExecBackendAttemptsForJob(ctx, job.ID)
	if err != nil {
		return diskGuardRoutePlan{}, fmt.Errorf("read earlier cloud attempts: %w", err)
	}
	if len(attempts) > 0 {
		return diskGuardRoutePlan{}, fmt.Errorf("the review already had a cloud attempt; remote admission allows one")
	}
	runtimeName := strings.TrimSpace(payload.RuntimeOverride)
	if runtimeName == "" {
		agent, err := store.GetAgent(ctx, job.Agent)
		if err != nil {
			return diskGuardRoutePlan{}, fmt.Errorf("read reviewer agent %q: %w", job.Agent, err)
		}
		runtimeName = agent.Runtime
	}
	if remoteCapableRuntime(runtimeName) {
		if runtimeName == runtime.OmpRuntime && !remoteOmpModelAuthenticates(payload.Model) {
			return diskGuardRoutePlan{}, fmt.Errorf("model %q is not available to omp in the remote sandbox", payload.Model)
		}
		return diskGuardRoutePlan{}, nil
	}
	if runtimeName != runtime.CodexRuntime {
		return diskGuardRoutePlan{}, fmt.Errorf("runtime %q cannot run remotely (remote runtimes: %s) and has no same-model omp equivalent", runtimeName, remoteCapableRuntimeNames())
	}
	model, err := diskGuardOmpModelFor(payload.Model)
	if err != nil {
		return diskGuardRoutePlan{}, err
	}
	return diskGuardRoutePlan{runtimeOverride: runtime.OmpRuntime, model: model, fromRuntime: runtimeName}, nil
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
	for _, job := range remote {
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
func diskGuardRoutedAlertText(reason string, plan diskGuardRoutePlan) string {
	text := "moved to the remote (E2B) backend because it cannot run on this host now: " + reason + "."
	if plan.runtimeOverride != "" {
		text += fmt.Sprintf(" Runtime changed %s -> %s with the same model %s.", plan.fromRuntime, plan.runtimeOverride, plan.model)
	}
	return text
}

// foregroundReviewDiskWarning returns the alert for a foreground review that
// runs on this host while the disk is below the review threshold or the
// dispatch floor (#2372: foreground reviews stay local, with an alert), or "".
func foregroundReviewDiskWarning(home string) string {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return ""
	}
	evaluation := evaluateConfiguredDiskGuard(paths)
	if evaluation.allowsDispatch() && !evaluation.reviewsBelowThreshold() {
		return ""
	}
	return "disk is low on this host (" + evaluation.detail() + "); this foreground review still runs here. Re-run it with --background to let the daemon run it on the remote backend."
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
