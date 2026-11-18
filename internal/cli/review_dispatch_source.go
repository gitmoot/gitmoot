package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Resolve review identity before choosing where source preparation belongs.
// Existing checkout metadata is retained, never inspected or repaired here.
func resolveDispatchRepoIdentity(ctx context.Context, store *db.Store, request localAgentDispatchRequest) (github.Repository, db.Repo, string, bool, error) {
	if request.Action != "review" {
		return resolveLocalAgentRepo(ctx, store, request.RepoFlag, localDispatchJobRunner(request))
	}
	repo, err := localAgentTargetRepo(ctx, request.RepoFlag, localDispatchJobRunner(request))
	if err != nil {
		return github.Repository{}, db.Repo{}, "", false, err
	}
	record, err := store.GetRepo(ctx, repo.FullName())
	if errors.Is(err, sql.ErrNoRows) {
		record = db.Repo{Owner: repo.Owner, Name: repo.Name, RemoteURL: "https://github.com/" + repo.FullName() + ".git", Enabled: true}
	} else if err != nil {
		return github.Repository{}, db.Repo{}, "", false, err
	}
	return repo, record, record.DefaultBranch, false, nil
}

func admitDispatchPreparation(ctx context.Context, store *db.Store, request *localAgentDispatchRequest, repo github.Repository, record db.Repo) (context.Context, func() error, error) {
	stop := func() error { return nil }
	if request.Background && request.Action != "review" && !dispatchReadOnlyWorktreeEligible(*request) {
		return ctx, stop, nil
	}
	paths, err := pathsFromFlag(request.Home)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve review admission paths: %w", err)
	}
	capacity, err := config.LoadJanitorCapacity(paths)
	if err != nil {
		return nil, nil, err
	}
	if request.Action == "review" && (request.remoteSource || capacity.Enabled && capacity.ReviewFreshSpace) {
		request.JobID = firstNonEmpty(strings.TrimSpace(request.JobID), localAgentJobID(request.Action, request.Agent))
		return startReviewCapacityMonitor(ctx, store, paths, repo.FullName(), request.HeadSHA, request.JobID, request.remoteSource, nil, record.CheckoutPath, record.PrimaryCheckoutPath)
	}
	if request.Action == "review" {
		if err := reviewDiskGuardRefusal(evaluateConfiguredDiskGuard(paths), request.remoteSource); err != nil {
			return nil, nil, err
		}
	}
	if err := janitorPressureRefusal(ctx, store, paths, repo.FullName(), record.CheckoutPath, record.PrimaryCheckoutPath); err != nil {
		return nil, nil, err
	}
	return ctx, stop, nil
}

// Scope selection never repairs a checkout. Preserve local graph semantics for
// local reviews; E2B uses authenticated forge data. The one fetch a local scope
// makes is origin's refresh before it takes the merge base (#2682), since a
// stale base ref widens the scope to commits the base already holds. Dispatch
// rechecks routing before preparation if policy or PR metadata moves.
func resolveDispatchReviewScope(ctx context.Context, store *db.Store, record db.Repo, repo github.Repository, request localAgentDispatchRequest, runtimeName, purpose string) (*workflow.ReviewScope, string, error) {
	backend, err := reviewDispatchExecBackend(request.ExecBackend)
	if err != nil {
		return nil, "", err
	}
	checksRouted, err := applyReviewChecksRoute(&request, repo.FullName())
	if err != nil {
		return nil, "", err
	}
	if checksRouted {
		backend = execbackend.Remote
	} else {
		var policy bool
		backend, policy, _ = reviewPolicyBackend(ctx, request, repo, "")
		if policy && !remoteCapableRuntime(runtimeName) {
			backend = execbackend.Local
		}
	}
	runner, err := jobSubprocessRunnerForBackend(backend)
	if err != nil {
		return nil, "", err
	}
	gh := jobGitHubClient("", newAgentDispatchGitHubClient(""), runner)
	subject, err := newReviewScopeSubject(ctx, store, gh, repo, request.PullRequest)
	if err != nil {
		return nil, "scope unavailable: " + err.Error(), nil
	}
	if backend != execbackend.Remote || config.IsSandboxdProvider(request.ExecProvider) {
		if strings.TrimSpace(record.CheckoutPath) == "" {
			return nil, "local checkout unavailable", nil
		}
		scope, skipped := resolveDeltaReviewScope(ctx, store, jobGitClient(record.CheckoutPath, runner), subject, repo.FullName(), request.PullRequest, request.HeadSHA, purpose)
		return scope, skipped, nil
	}
	graph := &remoteReviewGraph{client: gh, repo: repo, head: request.HeadSHA, subject: subject}
	scope, skipped := resolveDeltaReviewScopeUsing(ctx, store, graph, graph.ScopeDiff, repo.FullName(), request.PullRequest, request.HeadSHA, purpose)
	return scope, skipped, nil
}
