package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestImplementationFinalizationTargetRejectsEveryMissingField(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		payload workflow.JobPayload
		task    *db.Task
		want    string
	}{
		{
			name:    "task id",
			payload: workflow.JobPayload{Repo: "owner/repo", Branch: "feature/fix", FixWorktree: true, WorktreePath: "/tmp/fix"},
			want:    "`gitmoot agent implement lead \"Implement the task.\" --repo owner/repo --task <task-id> --branch <branch>`",
		},
		{
			name:    "worktree path",
			payload: workflow.JobPayload{Repo: "owner/repo", Branch: "feature/fix", TaskID: "task-1", FixWorktree: true},
			task:    &db.Task{ID: "task-1", RepoFullName: "owner/repo", Branch: "feature/fix"},
			want:    "no worktree path",
		},
		{
			name:    "branch",
			payload: workflow.JobPayload{Repo: "owner/repo", Branch: "feature/fix", PullRequest: 1514, TaskID: "task-1", FixWorktree: true, WorktreePath: "/tmp/fix"},
			task:    &db.Task{ID: "task-1", RepoFullName: "owner/repo", WorktreePath: "/tmp/stale-task"},
			want:    "no branch",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := daemonWorkerStore(t)
			if test.task != nil {
				if err := store.UpsertTask(ctx, *test.task); err != nil {
					t.Fatalf("UpsertTask returned error: %v", err)
				}
			}
			_, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "advance-fix", Agent: "lead", Type: "implement"}, test.payload, implementationFinalizationBeforeRun)
			var blocked workflow.BlockedError
			if !errors.As(err, &blocked) || !blocked.ResultDeliveryFailed {
				t.Fatalf("error = %v, want result-delivery BlockedError", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestAdvanceImplementationPreflightBlocksBeforeModelAndKeepsResultHonest(t *testing.T) {
	for _, test := range []struct {
		mode          string
		wantCondition string
	}{
		{mode: "stale", wantCondition: "stale or divergent"},
		{mode: "divergent", wantCondition: "stale or divergent"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			result := runAdvanceImplementationPreflightFixture(t, test.mode)
			for _, want := range []string{
				"task-1514", test.wantCondition, result.currentHead, result.expectedHead, result.fixWorktree,
				"fetch origin feature/semantic-census",
				"reset --hard " + result.expectedHead,
			} {
				if !strings.Contains(result.message, want) {
					t.Fatalf("blocked message missing %q: %s", want, result.message)
				}
			}
			if !result.remedyCleared {
				t.Fatal("documented reset remedy did not clear the preflight refusal")
			}
			if test.mode == "divergent" {
				git := gitutil.Client{Dir: result.fixWorktree}
				currentAncestorDispatch, err := git.IsAncestor(context.Background(), result.currentHead, result.expectedHead)
				if err != nil {
					t.Fatalf("compare divergent current head with dispatch head: %v", err)
				}
				dispatchAncestorCurrent, err := git.IsAncestor(context.Background(), result.expectedHead, result.currentHead)
				if err != nil {
					t.Fatalf("compare divergent dispatch head with current head: %v", err)
				}
				if currentAncestorDispatch || dispatchAncestorCurrent {
					t.Fatalf("divergent fixture ancestry current->dispatch=%t dispatch->current=%t, want neither", currentAncestorDispatch, dispatchAncestorCurrent)
				}
			}
		})
	}
}

func TestAdvanceImplementationPreflightFetchesAdvertisedMissingDispatchHead(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "unfetched-stale")
	for _, want := range []string{
		"task-1514", result.fixWorktree, "missing dispatch head object " + result.expectedHead,
		"origin refs/pull/1514/head still advertises it",
		"fetch origin refs/pull/1514/head",
		"cat-file -e " + result.expectedHead + "^{commit}",
		"reset --hard " + result.expectedHead,
		"gitmoot job retry advance-fix-unfetched-stale",
	} {
		if !strings.Contains(result.message, want) {
			t.Fatalf("blocked message missing %q: %s", want, result.message)
		}
	}
	if !result.remedyCleared {
		t.Fatal("advertised dispatch-head remedy did not clear the preflight refusal")
	}
}

func TestAdvanceImplementationPreflightRequiresRedispatchWhenFrozenHeadMoved(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "force-pushed-away")
	for _, want := range []string{
		"task-1514", result.fixWorktree, "missing frozen dispatch head " + result.expectedHead,
		"origin refs/pull/1514/head now points to " + result.refreshedHead,
		"cannot be recovered by fetch/reset", "must not be retried",
		"dispatch a new fix job against current head " + result.refreshedHead,
	} {
		if !strings.Contains(result.message, want) {
			t.Fatalf("blocked message missing %q: %s", want, result.message)
		}
	}
	if !result.freshMetadataCleared {
		t.Fatal("re-dispatch against refreshed pull request metadata did not clear the preflight refusal")
	}
	if !result.obsoleteRecoveryRejected || !result.exactFetchRejected {
		t.Fatalf("force-pushed fixture obsolete recovery rejected=%t exact fetch rejected=%t, want both true", result.obsoleteRecoveryRejected, result.exactFetchRejected)
	}
}

func TestAdvanceImplementationPreflightReturnsInfrastructureErrorForCorruptObjectGraph(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "corrupt-object")
	if result.runErr == nil {
		t.Fatal("corrupt object graph returned nil worker error")
	}
	for _, want := range []string{"validate implementation target before model run", "verify implementation worktree object connectivity", "git fsck --connectivity-only"} {
		if !strings.Contains(result.runErr.Error(), want) {
			t.Fatalf("infrastructure error missing %q: %v", want, result.runErr)
		}
	}
	if result.adapterCalls != 0 {
		t.Fatalf("adapter calls = %d, want zero for corrupt repository", result.adapterCalls)
	}
	if result.jobState == string(workflow.JobBlocked) {
		t.Fatalf("corrupt repository settled terminally blocked: state=%s", result.jobState)
	}
	if result.message != "" {
		t.Fatalf("corrupt repository emitted operator recovery message %q", result.message)
	}
}

func TestAdvanceImplementationPreflightRejectsMissingHeadBeforeModel(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "missing-head")
	for _, want := range []string{
		"task-1514", result.fixWorktree, "no dispatch head SHA", "pull request #1514",
		"--task task-1514", "--pr 1514", "--branch feature/semantic-census", "--head-sha <sha>",
	} {
		if !strings.Contains(result.message, want) {
			t.Fatalf("blocked message missing %q: %s", want, result.message)
		}
	}
	if !result.remedyCleared {
		t.Fatal("fresh dispatch-head metadata did not clear the missing-head refusal")
	}
}

func TestAdvanceImplementationPreflightAllowsCheckoutAheadOfDispatchHead(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "ahead")
	if result.currentHead == result.expectedHead {
		t.Fatalf("ahead fixture current head = dispatch head %s", result.currentHead)
	}
	runDaemonWorkerGit(t, result.fixWorktree, "merge-base", "--is-ancestor", result.expectedHead, result.currentHead)
	if result.adapterCalls != 1 {
		t.Fatalf("adapter calls = %d, want one for checkout ahead of frozen dispatch head", result.adapterCalls)
	}
	if result.jobState != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed result from invoked adapter", result.jobState)
	}
}

func TestAdvanceImplementationPreflightRejectsDetachedAndWrongBranchBeforeModel(t *testing.T) {
	for _, mode := range []string{"detached", "wrong-branch"} {
		t.Run(mode, func(t *testing.T) {
			result := runAdvanceImplementationPreflightFixture(t, mode)
			for _, want := range []string{"task-1514", result.fixWorktree, "feature/semantic-census"} {
				if !strings.Contains(result.message, want) {
					t.Fatalf("blocked message missing %q: %s", want, result.message)
				}
			}
			if mode == "detached" && !strings.Contains(result.message, "current git branch is empty") {
				t.Fatalf("detached message = %q", result.message)
			}
			if mode == "wrong-branch" && !strings.Contains(result.message, "wrong-branch") {
				t.Fatalf("wrong-branch message = %q", result.message)
			}
		})
	}
}

type advanceImplementationPreflightResult struct {
	message       string
	currentHead   string
	expectedHead  string
	fixWorktree   string
	adapterCalls  int
	jobState      string
	remedyCleared bool
	runErr        error
	refreshedHead string

	freshMetadataCleared     bool
	obsoleteRecoveryRejected bool
	exactFetchRejected       bool
}

func runAdvanceImplementationPreflightFixture(t *testing.T, mode string) advanceImplementationPreflightResult {
	t.Helper()
	ctx := context.Background()
	const branch = "feature/semantic-census"
	store := daemonWorkerStore(t)
	registered := createDaemonWorkerGitCheckout(t, "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runDaemonWorkerGit(t, filepath.Dir(remote), "init", "--bare", remote)
	runDaemonWorkerGit(t, registered, "remote", "set-url", "origin", remote)
	runDaemonWorkerGit(t, registered, "push", "-u", "origin", "main")
	runDaemonWorkerGit(t, registered, "switch", "-c", branch)
	runDaemonWorkerGit(t, registered, "push", "-u", "origin", branch)
	oldHead := strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
	fixWorktree := filepath.Join(t.TempDir(), "fix-worktree")
	runDaemonWorkerGit(t, filepath.Dir(fixWorktree), "clone", "--branch", branch, remote, fixWorktree)
	configureTestGit(t, fixWorktree)
	expectedHead := oldHead
	refreshedHead := ""
	wantBlocked := true
	switch mode {
	case "stale":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance reviewed branch")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", branch)
	case "unfetched-stale":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance reviewed branch")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
		runDaemonWorkerGit(t, remote, "update-ref", "refs/pull/1514/head", expectedHead)
	case "force-pushed-away":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "frozen dispatch head")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
		runDaemonWorkerGit(t, registered, "reset", "--hard", oldHead)
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "replacement pull request head")
		refreshedHead = strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
		runDaemonWorkerGit(t, registered, "push", "--force", "origin", branch)
		runDaemonWorkerGit(t, remote, "update-ref", "refs/pull/1514/head", refreshedHead)
		runDaemonWorkerGit(t, remote, "reflog", "expire", "--expire=now", "--all")
		runDaemonWorkerGit(t, remote, "gc", "--prune=now")
	case "divergent":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance reviewed branch")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", branch)
		runDaemonWorkerGit(t, fixWorktree, "commit", "--allow-empty", "-m", "divergent local fix")
	case "corrupt-object":
		blob := strings.TrimSpace(runGitOutput(t, fixWorktree, "rev-parse", "HEAD:README.md"))
		objectPath := filepath.Join(fixWorktree, ".git", "objects", blob[:2], blob[2:])
		if err := os.Remove(objectPath); err != nil {
			t.Fatalf("remove reachable blob object %s: %v", objectPath, err)
		}
		expectedHead = strings.Repeat("f", 40)
	case "missing-head":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance branch without head evidence")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = ""
	case "ahead":
		runDaemonWorkerGit(t, fixWorktree, "commit", "--allow-empty", "-m", "newer local fix")
		wantBlocked = false
	case "detached":
		runDaemonWorkerGit(t, fixWorktree, "checkout", "--detach", oldHead)
	case "wrong-branch":
		runDaemonWorkerGit(t, fixWorktree, "switch", "-c", "wrong-branch")
	default:
		t.Fatalf("unknown fixture mode %q", mode)
	}
	currentHead := strings.TrimSpace(runGitOutput(t, fixWorktree, "rev-parse", "HEAD"))
	seedDaemonWorkerRepo(t, store, "owner/repo", registered)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-1514", RepoFullName: "owner/repo", State: string(workflow.TaskChangesRequested),
		Branch: branch, WorktreePath: registered,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: branch, Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	jobID := "advance-fix-" + mode
	if mode == "missing-head" {
		enqueueAdvanceCreatedHeadlessFixJob(t, store, jobID, branch, fixWorktree)
	} else {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: jobID, Agent: "lead", Action: "implement", Repo: "owner/repo",
			Branch: branch, PullRequest: 1514, HeadSHA: expectedHead, TaskID: "task-1514",
			WorktreePath: fixWorktree, FixWorktree: true,
		})
	}
	beforeRemote := strings.TrimSpace(runGitOutput(t, registered, "ls-remote", "origin", "refs/heads/"+branch))
	adapter := &cliWorkerFakeAdapter{output: resultJSON("failed")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return fixWorktree, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine { return workflow.Engine{Store: store} }
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob before run: %v", err)
	}
	runErr := worker.run(ctx, job)
	if runErr != nil && mode != "corrupt-object" {
		t.Fatalf("worker.run returned error: %v", runErr)
	}
	if runErr == nil && mode == "corrupt-object" {
		t.Fatal("worker.run accepted a corrupt object graph")
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob after run: %v", err)
	}
	if mode == "corrupt-object" {
		if adapter.calls != 0 {
			t.Fatalf("adapter calls = %d, want zero for infrastructure refusal", adapter.calls)
		}
	} else if wantBlocked {
		if adapter.calls != 0 {
			t.Fatalf("adapter calls = %d, want zero: checkout preflight must run before the model", adapter.calls)
		}
		if after.State != string(workflow.JobBlocked) {
			t.Fatalf("job state = %q, want blocked", after.State)
		}
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload returned error: %v", err)
		}
		if payload.Result != nil {
			t.Fatalf("preflight-blocked payload result = %+v, want nil", payload.Result)
		}
	} else if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want one: checkout includes the dispatch head", adapter.calls)
	}
	if got := strings.TrimSpace(runGitOutput(t, registered, "ls-remote", "origin", "refs/heads/"+branch)); got != beforeRemote {
		t.Fatalf("remote head changed from %q to %q despite preflight refusal", beforeRemote, got)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	message := ""
	for _, event := range events {
		if event.Kind == string(workflow.JobBlocked) {
			message = event.Message
		}
	}
	remedyCleared := false
	freshMetadataCleared := false
	obsoleteRecoveryRejected := false
	exactFetchRejected := false
	if mode == "stale" || mode == "divergent" {
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", branch)
		runDaemonWorkerGit(t, fixWorktree, "reset", "--hard", expectedHead)
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload after remedy: %v", err)
		}
		if _, err := implementationFinalizationTargetFor(ctx, store, after, payload, implementationFinalizationBeforeRun); err != nil {
			t.Fatalf("documented reset remedy did not clear preflight: %v", err)
		}
		remedyCleared = true
	} else if mode == "unfetched-stale" {
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", "refs/pull/1514/head")
		runDaemonWorkerGit(t, fixWorktree, "cat-file", "-e", expectedHead+"^{commit}")
		runDaemonWorkerGit(t, fixWorktree, "reset", "--hard", expectedHead)
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload after advertised-head remedy: %v", err)
		}
		if _, err := implementationFinalizationTargetFor(ctx, store, after, payload, implementationFinalizationBeforeRun); err != nil {
			t.Fatalf("advertised dispatch-head remedy did not clear preflight: %v", err)
		}
		remedyCleared = true
	} else if mode == "force-pushed-away" {
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", branch)
		obsoleteReset := exec.Command("git", "reset", "--hard", expectedHead)
		obsoleteReset.Dir = fixWorktree
		if output, err := obsoleteReset.CombinedOutput(); err == nil {
			t.Fatalf("obsolete branch fetch/reset unexpectedly recovered force-pushed-away head; output=%s", output)
		}
		obsoleteRecoveryRejected = true
		exactFetch := exec.Command("git", "fetch", "origin", expectedHead)
		exactFetch.Dir = fixWorktree
		if output, err := exactFetch.CombinedOutput(); err == nil {
			t.Fatalf("force-pushed-away dispatch object unexpectedly remained fetchable; output=%s", output)
		}
		exactFetchRejected = true
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", "refs/pull/1514/head")
		runDaemonWorkerGit(t, fixWorktree, "reset", "--hard", "FETCH_HEAD")
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload after metadata refresh: %v", err)
		}
		payload.HeadSHA = refreshedHead
		if _, err := implementationFinalizationTargetFor(ctx, store, after, payload, implementationFinalizationBeforeRun); err != nil {
			t.Fatalf("re-dispatch against refreshed pull request metadata did not clear preflight: %v", err)
		}
		freshMetadataCleared = true
	} else if mode == "missing-head" {
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload after missing-head refusal: %v", err)
		}
		payload.HeadSHA = currentHead
		if _, err := implementationFinalizationTargetFor(ctx, store, after, payload, implementationFinalizationBeforeRun); err != nil {
			t.Fatalf("fresh dispatch head did not clear missing-head preflight: %v", err)
		}
		remedyCleared = true
	}
	return advanceImplementationPreflightResult{
		message: message, currentHead: currentHead, expectedHead: expectedHead, fixWorktree: fixWorktree,
		adapterCalls: adapter.calls, jobState: after.State, remedyCleared: remedyCleared,
		runErr: runErr, refreshedHead: refreshedHead, freshMetadataCleared: freshMetadataCleared,
		obsoleteRecoveryRejected: obsoleteRecoveryRejected, exactFetchRejected: exactFetchRejected,
	}
}

func enqueueAdvanceCreatedHeadlessFixJob(t *testing.T, store *db.Store, jobID, branch, fixWorktree string) {
	t.Helper()
	ctx := context.Background()
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "headless-review", Agent: "audit", Action: "review", Repo: "owner/repo",
		Branch: branch, PullRequest: 1514, TaskID: "task-1514", LeadAgent: "lead",
	})
	review, err := store.GetJob(ctx, "headless-review")
	if err != nil {
		t.Fatalf("GetJob(headless-review): %v", err)
	}
	payload, err := daemonJobPayload(review)
	if err != nil {
		t.Fatalf("daemonJobPayload(headless-review): %v", err)
	}
	payload.Result = &workflow.AgentResult{Decision: "changes_requested", Summary: "fix the headless review"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal headless review payload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, review.ID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload(headless-review): %v", err)
	}
	if err := store.UpdateJobState(ctx, review.ID, string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("UpdateJobState(headless-review): %v", err)
	}
	engine := workflow.Engine{
		Store: store,
		JobID: func(workflow.JobRequest) string { return jobID },
		RequireWorkflowPolicy: func(string) workflow.RequireWorkflowPolicy {
			return workflow.RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
		},
		FixWorktreeAllocator: func(context.Context, workflow.FixWorktreeRequest) (workflow.FixWorktreeAllocation, error) {
			return workflow.FixWorktreeAllocation{Path: fixWorktree}, nil
		},
	}
	if err := engine.AdvanceJob(ctx, review.ID); err != nil {
		t.Fatalf("AdvanceJob(headless-review): %v", err)
	}
	fix, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	fixPayload, err := daemonJobPayload(fix)
	if err != nil {
		t.Fatalf("daemonJobPayload(%s): %v", jobID, err)
	}
	if !fixPayload.FixWorktree || fixPayload.WorktreePath != fixWorktree || strings.TrimSpace(fixPayload.HeadSHA) != "" {
		t.Fatalf("advance-created fix payload = %+v, want headless dedicated fix worktree", fixPayload)
	}
}

func TestOrdinaryImplementationSkipsAdvanceFinalizationPreflight(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "main", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "ordinary-implement", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "main",
	})
	adapter := &cliWorkerFakeAdapter{output: resultJSON("failed")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine { return workflow.Engine{Store: store} }
	job, err := store.GetJob(ctx, "ordinary-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want one: ordinary implement is not an advance finalization candidate", adapter.calls)
	}
}

func TestDaemonImplementationFinalizerKeepsMissingBranchBackstop(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-backstop", RepoFullName: "owner/repo", WorktreePath: t.TempDir()}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	payload := workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", PullRequest: 12, TaskID: "task-backstop",
		FixWorktree: true, WorktreePath: t.TempDir(), Result: &workflow.AgentResult{Decision: "implemented"},
	}
	_, err := (daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}).FinalizeImplementation(ctx, db.Job{ID: "late-backstop", Agent: "lead", Type: "implement"}, payload)
	var blocked workflow.BlockedError
	if !errors.As(err, &blocked) || !blocked.ResultDeliveryFailed || !strings.Contains(err.Error(), "no branch") {
		t.Fatalf("FinalizeImplementation error = %v, want delivery-blocked missing-branch backstop", err)
	}
}

func TestImplementationFinalizationTargetAcceptsCompleteTarget(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := createDaemonWorkerGitCheckout(t, "feature/ok")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-ok", RepoFullName: "owner/repo", Branch: "feature/ok", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	head := strings.TrimSpace(runGitOutput(t, worktree, "rev-parse", "HEAD"))
	target, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "advance-ok", Agent: "lead", Type: "implement"}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/ok", HeadSHA: head, TaskID: "task-ok", FixWorktree: true, WorktreePath: worktree,
	}, implementationFinalizationBeforeRun)
	if err != nil {
		t.Fatalf("implementationFinalizationTargetFor returned error: %v", err)
	}
	if target.Task.ID != "task-ok" || target.WorktreePath != worktree {
		t.Fatalf("target = %+v, want task-ok and fix worktree", target)
	}
	if got := strings.TrimSpace(runGitOutput(t, worktree, "rev-parse", "HEAD")); got != head {
		t.Fatalf("complete-target fixture HEAD = %s, want exact dispatch head %s", got, head)
	}
}

func TestImplementationFinalizationTargetClassifiesBranchLookupByPhase(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-branch-error", RepoFullName: "owner/repo", Branch: "feature/fix", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	payload := workflow.JobPayload{Repo: "owner/repo", TaskID: "task-branch-error", FixWorktree: true, WorktreePath: worktree}
	job := db.Job{ID: "advance-branch-error", Agent: "lead", Type: "implement"}

	for _, test := range []struct {
		name        string
		phase       implementationFinalizationPhase
		wantBlocked bool
	}{
		{name: "before run blocks", phase: implementationFinalizationBeforeRun, wantBlocked: true},
		{name: "after run retries", phase: implementationFinalizationAfterRun, wantBlocked: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := implementationFinalizationTargetFor(ctx, store, job, payload, test.phase)
			if err == nil {
				t.Fatal("implementationFinalizationTargetFor returned nil error")
			}
			var blocked workflow.BlockedError
			if got := errors.As(err, &blocked); got != test.wantBlocked {
				t.Fatalf("BlockedError = %t, want %t: %v", got, test.wantBlocked, err)
			}
			if !test.wantBlocked && !strings.Contains(err.Error(), "resolve implementation branch") {
				t.Fatalf("after-run error = %q, want retryable branch-resolution context", err)
			}
		})
	}
}

func TestImplementationFinalizationTargetRejectsUnsetPhase(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	_, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "advance-unset"}, workflow.JobPayload{}, implementationFinalizationPhaseUnset)
	if err == nil || !strings.Contains(err.Error(), "finalization phase 0 is invalid") {
		t.Fatalf("implementationFinalizationTargetFor error = %v, want invalid zero phase", err)
	}
}
