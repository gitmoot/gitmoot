package cli

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

// Source preparation in E2B does not consume local checkout/build capacity.
// Fresh-space local reviews likewise use their explicit reserve/monitor policy,
// not the ordinary cleanup-target gate. Janitor filtering follows this pass.
func diskGuardQueuedDispatch(ctx context.Context, worker jobWorker, jobs []db.Job, repoFilter, rootFilter string) []db.Job {
	freshReviews := false
	if paths, err := diskGuardPaths(worker); err == nil {
		if policy, err := config.LoadJanitorCapacity(paths); err == nil {
			freshReviews = policy.Enabled && policy.ReviewFreshSpace
		}
	}
	var independent map[string]bool
	var local []db.Job
	for i, job := range jobs {
		independentReview := false
		if job.Type == "review" && queuedJobMatchesRepo(job, repoFilter) && queuedJobMatchesSession(job, rootFilter) {
			independentReview = freshReviews
			if !independentReview {
				if payload, err := daemonJobPayload(job); err == nil {
					backend, cfg, err := worker.resolveExecutionBackendForJob(job, payload)
					independentReview = err == nil && backend == execbackend.Remote && !config.IsSandboxdProvider(cfg.Provider)
				}
			}
		}
		if independentReview {
			if independent == nil {
				independent = make(map[string]bool)
				local = append(make([]db.Job, 0, len(jobs)-1), jobs[:i]...)
			}
			independent[job.ID] = true
		} else if independent != nil {
			local = append(local, job)
		}
	}
	if independent == nil {
		return diskGuardLocalQueuedDispatch(ctx, worker, jobs, repoFilter, rootFilter)
	}
	if len(local) == 0 {
		return jobs
	}
	allowedLocal := diskGuardLocalQueuedDispatch(ctx, worker, local, repoFilter, rootFilter)
	// Keep scheduler order and any route-updated payload returned by the guard.
	byID := make(map[string]db.Job, len(allowedLocal))
	for _, job := range allowedLocal {
		byID[job.ID] = job
	}
	allowed := make([]db.Job, 0, len(independent)+len(allowedLocal))
	for _, job := range jobs {
		if independent[job.ID] {
			allowed = append(allowed, job)
		} else if updated, ok := byID[job.ID]; ok {
			allowed = append(allowed, updated)
		}
	}
	return allowed
}
