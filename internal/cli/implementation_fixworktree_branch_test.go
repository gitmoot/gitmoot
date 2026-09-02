package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1523: a FixWorktree job resolves its delivery branch from the payload (its
// task is typically a branchless review task), exactly the way it already
// resolves the worktree path. These tests pin the symmetric override, the
// fail-closed current-branch guard, and the refusal-message accuracy.

func lsRemoteHead(t *testing.T, dir, remote, branch string) string {
	t.Helper()
	fields := strings.Fields(runGitOutput(t, dir, "ls-remote", remote, "refs/heads/"+branch))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// Acceptance 1: FixWorktree job, task.Branch empty, payload.Branch set ->
// DELIVERS; the remote moves.
func TestFixWorktreeFinalizerDeliversPayloadBranchWhenTaskHasNone(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	const branch = "feature/fix-delivery"
	registered := createDaemonWorkerGitCheckout(t, "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runDaemonWorkerGit(t, filepath.Dir(remote), "init", "--bare", remote)
	runDaemonWorkerGit(t, registered, "remote", "set-url", "origin", remote)
	runDaemonWorkerGit(t, registered, "push", "-u", "origin", "main")
	runDaemonWorkerGit(t, registered, "switch", "-c", branch)
	runDaemonWorkerGit(t, registered, "push", "-u", "origin", branch)
	beforeRemote := lsRemoteHead(t, registered, "origin", branch)
	if beforeRemote == "" {
		t.Fatal("fixture remote branch is empty before delivery")
	}
	fixWorktree := filepath.Join(t.TempDir(), "fix-worktree")
	runDaemonWorkerGit(t, filepath.Dir(fixWorktree), "clone", "--branch", branch, remote, fixWorktree)
	configureTestGit(t, fixWorktree)
	seedDaemonWorkerRepo(t, store, "owner/repo", registered)
	// The review task legitimately owns no branch; it only points at the
	// registered checkout. The fix payload carries both the fix worktree and
	// the branch.
	reviewTask := db.Task{ID: "review-pr-1523-deadbeef", RepoFullName: "owner/repo", WorktreePath: registered}
	if strings.TrimSpace(reviewTask.Branch) != "" {
		t.Fatalf("review task branch = %q, want empty so the payload override is required", reviewTask.Branch)
	}
	if err := store.UpsertTask(ctx, reviewTask); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	// An open pull request record lets the finalizer adopt it instead of
	// calling GitHub; the push under test happens before that step either way.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo", Number: 1523, URL: "https://example.invalid/pull/1523",
		HeadBranch: branch, BaseBranch: "main", State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	// The completed fix: an uncommitted change sitting in the fix worktree.
	if err := os.WriteFile(filepath.Join(fixWorktree, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatalf("write fix change: %v", err)
	}
	payload := workflow.JobPayload{
		Repo: "owner/repo", Branch: branch, PullRequest: 1523, TaskID: "review-pr-1523-deadbeef",
		FixWorktree: true, WorktreePath: fixWorktree, Result: &workflow.AgentResult{Decision: "implemented"},
	}
	delivered, err := (newHostDaemonImplementationFinalizer(store, githubtest.NoopClient{})).FinalizeImplementation(
		ctx, db.Job{ID: "fix-delivers", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	localHead := strings.TrimSpace(runGitOutput(t, fixWorktree, "rev-parse", "HEAD"))
	afterRemote := lsRemoteHead(t, registered, "origin", branch)
	if afterRemote == beforeRemote {
		t.Fatalf("remote head did not move: still %s", afterRemote)
	}
	if afterRemote != localHead {
		t.Fatalf("remote head = %s, want pushed fix worktree head %s", afterRemote, localHead)
	}
	if delivered.Branch != branch {
		t.Fatalf("delivered payload branch = %q, want %q", delivered.Branch, branch)
	}
	if delivered.PullRequest != 1523 {
		t.Fatalf("delivered payload pull request = %d, want 1523", delivered.PullRequest)
	}
}

// Acceptance 2: FixWorktree job with neither task.Branch nor payload.Branch
// refuses BEFORE the model runs — zero adapter calls.
func TestFixWorktreePreflightRefusesWhenNeitherTaskNorPayloadHasBranch(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "feature/reviewed")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "review-pr-1523-branchless", RepoFullName: "owner/repo", WorktreePath: checkout}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "fix-neither-branch", Agent: "lead", Action: "implement", Repo: "owner/repo",
		PullRequest: 1523, TaskID: "review-pr-1523-branchless",
		WorktreePath: checkout, FixWorktree: true, // no Branch anywhere
	})
	adapter := &cliWorkerFakeAdapter{output: resultJSON("implemented")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine { return workflow.Engine{Store: store} }
	job, err := store.GetJob(ctx, "fix-neither-branch")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want zero: branchless fix job must refuse before the model runs", adapter.calls)
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob after run returned error: %v", err)
	}
	if after.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", after.State)
	}
	jobEvents, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	message := ""
	for _, event := range jobEvents {
		if event.Kind == string(workflow.JobBlocked) {
			message = event.Message
		}
	}
	if !strings.Contains(message, "carries no payload branch") {
		t.Fatalf("blocked message = %q, want it to name the missing payload branch", message)
	}
	if !strings.Contains(message, "--branch <branch>") {
		t.Fatalf("blocked message = %q, want the placeholder advice, not a fabricated value", message)
	}
	if strings.Contains(message, "--branch feature/") {
		t.Fatalf("blocked message = %q prints a usable branch value while refusing for a missing branch", message)
	}
}

// Acceptance 3: an ordinary (non-fix) job still takes the task's branch; the
// payload branch is not consulted.
func TestImplementationFinalizationTargetOrdinaryJobTaskBranchWins(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := createDaemonWorkerGitCheckout(t, "feature/task-branch")
	task := db.Task{ID: "task-ordinary", RepoFullName: "owner/repo", Branch: "feature/task-branch", WorktreePath: worktree}
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "feature/payload-branch", TaskID: task.ID}
	if payload.Branch == task.Branch {
		t.Fatalf("ordinary fixture branches are equal at %q; payload must conflict so task precedence is observable", task.Branch)
	}
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	target, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "ordinary", Agent: "lead", Type: "implement"}, payload, implementationFinalizationAfterRun)
	if err != nil {
		t.Fatalf("implementationFinalizationTargetFor returned error: %v", err)
	}
	if target.Task.Branch != "feature/task-branch" {
		t.Fatalf("resolved branch = %q, want the task branch for an ordinary job", target.Task.Branch)
	}
	if target.WorktreePath != worktree {
		t.Fatalf("resolved worktree = %q, want the task worktree for an ordinary job", target.WorktreePath)
	}
}

// Acceptance 4: a checkout sitting on a different branch than the RESOLVED one
// still refuses — the fail-closed guard reads the resolved value, so a wrong
// payload.Branch cannot silently deliver.
func TestImplementationFinalizationTargetRefusesCheckoutOffResolvedBranch(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := createDaemonWorkerGitCheckout(t, "feature/task-branch")
	for _, test := range []struct {
		name           string
		task           db.Task
		payload        workflow.JobPayload
		want           []string
		branchlessTask bool
	}{
		{
			// Risk 1: the payload branch is wrong for this checkout even
			// though the checkout matches the task. Resolution prefers the
			// payload for a fix job, so the guard must refuse.
			name: "payload branch wins and the checkout does not match it",
			task: db.Task{ID: "task-guard", RepoFullName: "owner/repo", Branch: "feature/task-branch", WorktreePath: worktree},
			payload: workflow.JobPayload{
				Repo: "owner/repo", Branch: "feature/payload-branch", TaskID: "task-guard",
				FixWorktree: true, WorktreePath: worktree,
			},
			want: []string{"is on branch feature/task-branch, not feature/payload-branch", "refusing to run or deliver from the wrong checkout"},
		},
		{
			// Branchless review task: the guard compares against the payload
			// branch, not against the task's empty branch.
			name: "branchless task still guards the payload branch",
			task: db.Task{ID: "task-guard-2", RepoFullName: "owner/repo", WorktreePath: worktree},
			payload: workflow.JobPayload{
				Repo: "owner/repo", Branch: "feature/payload-branch", TaskID: "task-guard-2",
				FixWorktree: true, WorktreePath: worktree,
			},
			want:           []string{"is on branch feature/task-branch, not feature/payload-branch", "refusing to run or deliver from the wrong checkout"},
			branchlessTask: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.branchlessTask && strings.TrimSpace(test.task.Branch) != "" {
				t.Fatalf("task branch = %q, want empty so the branchless-task guard lane remains distinct", test.task.Branch)
			}
			if err := store.UpsertTask(ctx, test.task); err != nil {
				t.Fatalf("UpsertTask returned error: %v", err)
			}
			_, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "guard", Agent: "lead", Type: "implement"}, test.payload, implementationFinalizationAfterRun)
			var blocked workflow.BlockedError
			if !errors.As(err, &blocked) || !blocked.ResultDeliveryFailed {
				t.Fatalf("error = %v, want result-delivery BlockedError", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want %q", err, want)
				}
			}
		})
	}
}

// Acceptance 5: symmetry guard. The task points at a stale worktree and a
// stale branch; the payload points at the real fix worktree and its branch.
// Removing EITHER override independently fails the subtest named for it.
func TestImplementationFinalizationTargetFixWorktreeOverridesAreSymmetric(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	realWorktree := createDaemonWorkerGitCheckout(t, "feature/fix")
	staleWorktree := t.TempDir() // not a git checkout at all
	staleTask := db.Task{
		ID: "task-stale", RepoFullName: "owner/repo",
		Branch: "feature/stale", WorktreePath: staleWorktree,
	}
	payload := workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", TaskID: "task-stale",
		FixWorktree: true, WorktreePath: realWorktree,
	}
	if staleTask.Branch == payload.Branch {
		t.Fatalf("stale task branch = payload branch %q; branch override is no longer required by the fixture", payload.Branch)
	}
	if staleTask.WorktreePath == payload.WorktreePath {
		t.Fatalf("stale task worktree = payload worktree %q; worktree override is no longer required by the fixture", payload.WorktreePath)
	}
	if err := store.UpsertTask(ctx, staleTask); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	job := db.Job{ID: "symmetry", Agent: "lead", Type: "implement"}

	target, err := implementationFinalizationTargetFor(ctx, store, job, payload, implementationFinalizationAfterRun)
	if err != nil {
		t.Fatalf("implementationFinalizationTargetFor returned error: %v", err)
	}
	t.Run("worktree path override", func(t *testing.T) {
		if target.WorktreePath != realWorktree {
			t.Fatalf("resolved worktree = %q, want the payload fix worktree %q", target.WorktreePath, realWorktree)
		}
	})
	t.Run("branch override", func(t *testing.T) {
		if target.Task.Branch != "feature/fix" {
			t.Fatalf("resolved branch = %q, want the payload branch %q", target.Task.Branch, "feature/fix")
		}
	})
}

// Acceptance 6: message accuracy. A refusal that fires must be TRUE for the
// case that produced it and must not print a usable --branch value while
// claiming no branch exists.
func TestImplementationFinalizationMissingBranchMessageIsAccurate(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	job := db.Job{ID: "message", Agent: "lead", Type: "implement"}

	t.Run("fix job names the payload and prints only the placeholder", func(t *testing.T) {
		if err := store.UpsertTask(ctx, db.Task{ID: "task-msg-fix", RepoFullName: "owner/repo", WorktreePath: t.TempDir()}); err != nil {
			t.Fatalf("UpsertTask returned error: %v", err)
		}
		_, err := implementationFinalizationTargetFor(ctx, store, job, workflow.JobPayload{
			Repo: "owner/repo", TaskID: "task-msg-fix", FixWorktree: true, WorktreePath: t.TempDir(),
		}, implementationFinalizationBeforeRun)
		var blocked workflow.BlockedError
		if !errors.As(err, &blocked) || !blocked.ResultDeliveryFailed {
			t.Fatalf("error = %v, want result-delivery BlockedError", err)
		}
		if !strings.Contains(err.Error(), "carries no payload branch") {
			t.Fatalf("error = %q, want the refusal to name the missing payload branch", err)
		}
		if !strings.Contains(err.Error(), "--branch <branch>") {
			t.Fatalf("error = %q, want placeholder advice, not a fabricated branch", err)
		}
	})

	t.Run("ordinary job still refuses and the task claim is true", func(t *testing.T) {
		if err := store.UpsertTask(ctx, db.Task{ID: "task-msg-ordinary", RepoFullName: "owner/repo", WorktreePath: t.TempDir()}); err != nil {
			t.Fatalf("UpsertTask returned error: %v", err)
		}
		_, err := implementationFinalizationTargetFor(ctx, store, job, workflow.JobPayload{
			Repo: "owner/repo", Branch: "feature/hint", TaskID: "task-msg-ordinary",
		}, implementationFinalizationBeforeRun)
		var blocked workflow.BlockedError
		if !errors.As(err, &blocked) || !blocked.ResultDeliveryFailed {
			t.Fatalf("error = %v, want result-delivery BlockedError", err)
		}
		// The claim "task has no branch" is true here, and the printed
		// --branch value is rerun advice: the payload branch was NOT usable
		// for delivery in this ordinary job, which is exactly why the
		// refusal fired.
		if !strings.Contains(err.Error(), "implementation task task-msg-ordinary has no branch") {
			t.Fatalf("error = %q, want the true missing-task-branch claim", err)
		}
		if !strings.Contains(err.Error(), "--branch feature/hint") {
			t.Fatalf("error = %q, want the rerun advice to carry the known branch hint", err)
		}
	})

	t.Run("fix job with a payload branch produces no refusal at all", func(t *testing.T) {
		worktree := createDaemonWorkerGitCheckout(t, "feature/has-branch")
		if err := store.UpsertTask(ctx, db.Task{ID: "task-msg-ok", RepoFullName: "owner/repo", WorktreePath: t.TempDir()}); err != nil {
			t.Fatalf("UpsertTask returned error: %v", err)
		}
		head := strings.TrimSpace(runGitOutput(t, worktree, "rev-parse", "HEAD"))
		_, err := implementationFinalizationTargetFor(ctx, store, job, workflow.JobPayload{
			Repo: "owner/repo", Branch: "feature/has-branch", HeadSHA: head, TaskID: "task-msg-ok",
			FixWorktree: true, WorktreePath: worktree,
		}, implementationFinalizationBeforeRun)
		if err != nil {
			t.Fatalf("the case the old message lied about must now resolve, got: %v", err)
		}
	})
}
