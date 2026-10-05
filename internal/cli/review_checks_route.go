package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// reviewChecksRoute is a repository's [repos."o/r".review] checks_backend
// routing (#2316) as review producers apply it. A configured repository sends
// EVERY review, policy-routed or explicit, to its checks_provider with its
// checks_template, and skips the remote_routing_enabled spend gates: the reason
// is capability, its checks need a toolchain only the remote image has. A
// review that would run anywhere else is refused rather than quietly reviewed
// without that toolchain; the only way back to local review is removing
// checks_backend.
//
// Two places apply it: the dispatch verbs (applyReviewChecksRoute), which
// refuse early and record the route on the request, and the worker
// (routeReviewChecks), which every review job passes through whichever
// producer enqueued it.
type reviewChecksRoute struct {
	config.ReviewChecksRoute
	repo string
	// provider is Provider as a payload stores it: absent for e2b, the
	// convention requestExecProvider uses.
	provider string
}

// loadReviewChecksRoute reads repo's checks routing and reports whether it
// routes reviews remotely.
func loadReviewChecksRoute(paths config.Paths, repo string) (reviewChecksRoute, bool, error) {
	// The joined error covers every [review] field; only this repository's
	// checks routing decides here, and its error is kept per repository.
	reviewConfig, _ := config.LoadReviewConfig(paths)
	route, err := reviewConfig.ChecksRoute(repo)
	if err != nil {
		return reviewChecksRoute{}, false, fmt.Errorf("review of %s refused: its checks routing is invalid: %w", repo, err)
	}
	if !route.Enabled() {
		return reviewChecksRoute{}, false, nil
	}
	provider := route.Provider
	if provider == config.RemoteExecProviderE2B {
		provider = ""
	}
	return reviewChecksRoute{ReviewChecksRoute: route, repo: repo, provider: provider}, true, nil
}

func (r reviewChecksRoute) setting() string {
	return fmt.Sprintf("[repos.%q.review] checks_backend = %q", r.repo, r.Backend)
}

// reason is the review_backend_route_selected message of a routed review.
func (r reviewChecksRoute) reason() string {
	template := r.Template
	if template == "" {
		template = "provider default"
	}
	return fmt.Sprintf("backend=remote source=repo_checks reason=%s provider=%s template=%s", r.setting(), r.Provider, template)
}

// selectionError refuses a review whose own backend or provider selection
// contradicts the route. An absent provider adopts checks_provider; only an
// explicit different one is refused. flags names the selectors as the dispatch
// flags that set them rather than as payload fields.
func (r reviewChecksRoute) selectionError(backend string, backendSet bool, provider string, flags bool) error {
	backendName, providerName := "exec_backend", "exec_provider"
	if flags {
		backendName, providerName = "--exec-backend", "--exec-provider"
	}
	if backend = strings.TrimSpace(backend); backendSet && backend != string(execbackend.Remote) {
		return fmt.Errorf("%s %s refused for %s: %s runs every review of this repository remotely so its checks have their toolchain; remove that setting to review it locally", backendName, firstNonEmptyRemoteReview(backend, `""`), r.repo, r.setting())
	}
	provider = strings.TrimSpace(provider)
	// A payload queued before the rename may carry the deprecated "mac".
	normalized, _ := config.NormalizeRemoteExecProvider(provider)
	if normalized == config.RemoteExecProviderE2B {
		normalized = ""
	}
	if provider != "" && normalized != r.provider {
		return fmt.Errorf("%s %s refused for %s: %s routes every review of this repository to checks_provider %q", providerName, provider, r.repo, r.setting(), r.Provider)
	}
	return nil
}

// runtimeError refuses a reviewer runtime the route cannot run: one that
// cannot run remotely at all, or one the provider has no template for (#2316:
// omp reviews provision e2b_omp_template, the others e2b_template).
func (r reviewChecksRoute) runtimeError(paths config.Paths, agentName, runtimeName string) error {
	if !remoteCapableRuntime(runtimeName) {
		// The repository opted every review into remote execution because its
		// checks need the remote toolchain, so a local fallback would be the
		// static review the operator configured away from.
		return fmt.Errorf("review of %s refused: reviewer %s runs on runtime %q, which cannot run remotely, and %s sends every review of %s to remote execution; use a reviewer or --runtime on %s",
			r.repo, agentName, runtimeName, r.setting(), r.repo, remoteCapableRuntimeNames())
	}
	remote, err := config.LoadRemoteExecConfig(paths)
	if err != nil {
		return fmt.Errorf("review of %s refused: %s: load [remote_exec]: %w", r.repo, r.setting(), err)
	}
	if err := remote.ReviewChecksTemplateError(r.ReviewChecksRoute, runtimeName == runtime.OmpRuntime); err != nil {
		return fmt.Errorf("review of %s refused: %s, but reviewer %s runs on runtime %q: %w", r.repo, r.setting(), agentName, runtimeName, err)
	}
	return nil
}

// routed reports whether payload already carries exactly this route.
func (r reviewChecksRoute) routed(payload workflow.JobPayload) bool {
	backend, set := payload.ExecBackendOverride()
	return set && strings.TrimSpace(backend) == string(execbackend.Remote) && payload.ReviewChecksRouted &&
		strings.TrimSpace(payload.ExecProvider) == r.provider && strings.TrimSpace(payload.ExecTemplate) == r.Template
}

// routeReviewChecks applies the repository's checks routing to a review job
// before its backend is resolved. Every review reaches the worker here,
// whichever producer enqueued it (dispatch verbs, native PR fan-out,
// comment-triggered, heartbeat and pipeline reviews), so a review of a
// configured repository can never silently run locally. The route is
// persisted onto the queued job, with its review_backend_route_selected event,
// so admission, the provider cap wait, the cost ledger and every later backend
// resolution see the remote job it is.
//
// owned is false when the job left the queued state before the route was
// written: cancellation or another worker owns it, and run returns. err is a
// refusal the caller fails the job with.
func (w jobWorker) routeReviewChecks(ctx context.Context, job db.Job, payload workflow.JobPayload) (db.Job, workflow.JobPayload, bool, error) {
	if !strings.EqualFold(strings.TrimSpace(job.Type), "review") {
		return job, payload, true, nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return job, payload, true, err
	}
	route, enabled, err := loadReviewChecksRoute(paths, strings.TrimSpace(payload.Repo))
	if err != nil || !enabled {
		return job, payload, true, err
	}
	backend, backendSet := payload.ExecBackendOverride()
	if err := route.selectionError(backend, backendSet, payload.ExecProvider, false); err != nil {
		return job, payload, true, err
	}
	// Checked on every run, routed or not: a model-pool fallback can move a
	// routed review onto a runtime this route has no template for.
	if agent, ok := selectedJobRuntimeAgent(ctx, w.Store, job, payload); ok {
		if err := route.runtimeError(paths, agent.Name, agent.Runtime); err != nil {
			return job, payload, true, err
		}
	}
	if route.routed(payload) {
		return job, payload, true, nil
	}
	payload.ExecBackend = string(execbackend.Remote)
	payload.ExecProvider = route.provider
	payload.ExecTemplate = route.Template
	payload.ReviewChecksRouted = true
	// The route is capability, not spend: neither the policy CI gate nor the
	// disk guard's undo-to-local applies to it.
	payload.PolicyRoutedReview = false
	payload.DiskGuardRouted = false
	encoded, err := json.Marshal(payload)
	if err != nil {
		return job, payload, true, err
	}
	moved, err := w.Store.RerouteQueuedJobPayload(ctx, job.ID, string(encoded), job.LifecycleGeneration, db.JobEvent{
		JobID: job.ID, Kind: "review_backend_route_selected", Message: route.reason(),
	})
	if err != nil {
		return job, payload, true, fmt.Errorf("persist checks routing of review %s: %w", job.ID, err)
	}
	if !moved {
		writeLine(w.Stdout, "job %s: no longer queued at lifecycle_generation=%d; checks routing not applied", job.ID, job.LifecycleGeneration)
		return job, payload, false, nil
	}
	job.Payload = string(encoded)
	return job, payload, true, nil
}

// reviewChecksRouteEnabled reports whether a review of repo is routed remote
// by its checks routing, counting an invalid routing as routed: run refuses it
// loudly instead of it waiting as a local review.
func (w jobWorker) reviewChecksRouteEnabled(repo string) bool {
	paths, err := w.configPaths()
	if err != nil {
		return false
	}
	_, enabled, err := loadReviewChecksRoute(paths, strings.TrimSpace(repo))
	return enabled || err != nil
}
