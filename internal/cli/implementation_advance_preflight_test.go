package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
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
			_, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "advance-fix", Agent: "lead", Type: "implement"}, test.payload)
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
	ctx := context.Background()
	store := daemonWorkerStore(t)
	registered := createDaemonWorkerGitCheckout(t, "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runDaemonWorkerGit(t, filepath.Dir(remote), "init", "--bare", remote)
	runDaemonWorkerGit(t, registered, "remote", "set-url", "origin", remote)
	runDaemonWorkerGit(t, registered, "push", "-u", "origin", "main")
	staleTaskWorktree := filepath.Join(t.TempDir(), "stale-task")
	runDaemonWorkerGit(t, registered, "worktree", "add", "--detach", staleTaskWorktree, "HEAD")
	runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance remote past stale task worktree")
	runDaemonWorkerGit(t, registered, "push", "origin", "main")
	fixWorktree := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", registered)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-1514", RepoFullName: "owner/repo", State: string(workflow.TaskChangesRequested),
		WorktreePath: staleTaskWorktree,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "feature/semantic-census", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "advance-fix", Agent: "lead", Action: "implement", Repo: "owner/repo",
		Branch: "feature/semantic-census", PullRequest: 1514, TaskID: "task-1514",
		WorktreePath: fixWorktree, FixWorktree: true,
	})
	beforeRemote := strings.TrimSpace(runGitOutput(t, registered, "ls-remote", "origin", "refs/heads/main"))
	adapter := &cliWorkerFakeAdapter{output: resultJSON("implemented")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return fixWorktree, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine { return workflow.Engine{Store: store} }

	job, err := store.GetJob(ctx, "advance-fix")
	if err != nil {
		t.Fatalf("GetJob before run: %v", err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want zero: preflight must run before the model", adapter.calls)
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob after run: %v", err)
	}
	if after.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", after.State)
	}
	payload, err := daemonJobPayload(after)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.Result != nil {
		t.Fatalf("preflight-blocked payload result = %+v, want nil (nothing ran or delivered)", payload.Result)
	}
	if got := strings.TrimSpace(runGitOutput(t, registered, "ls-remote", "origin", "refs/heads/main")); got != beforeRemote {
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
	for _, want := range []string{
		"task-1514", "no branch", staleTaskWorktree,
		"fetch origin refs/pull/1514/head", "reset --hard FETCH_HEAD",
		"--task task-1514", "--branch feature/semantic-census",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("blocked message missing %q: %s", want, message)
		}
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
	if err := store.UpsertTask(ctx, db.Task{ID: "task-ok", RepoFullName: "owner/repo", Branch: "feature/ok", WorktreePath: "/task/worktree"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	target, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "advance-ok", Agent: "lead", Type: "implement"}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/ok", TaskID: "task-ok", FixWorktree: true, WorktreePath: "/fix/worktree",
	})
	if err != nil {
		t.Fatalf("implementationFinalizationTargetFor returned error: %v", err)
	}
	if target.Task.ID != "task-ok" || target.WorktreePath != "/fix/worktree" {
		t.Fatalf("target = %+v, want task-ok and fix worktree", target)
	}
}
