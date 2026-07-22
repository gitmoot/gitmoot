package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRunQueuedJobsExecutesShellAdapterSuccess(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, `printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-success", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-success")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) || !strings.Contains(job.Payload, `"summary":"done"`) {
		t.Fatalf("job after worker = %+v", job)
	}
}

func TestRunQueuedJobsMarksShellAdapterFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "exit 7", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-fail", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-fail")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

func TestRunQueuedJobsBlocksReadOnlyImplementBeforeRuntime(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskChangesRequested), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-readonly-implement", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	comments := &cliPollFakeGitHub{}
	checkoutCalls := 0
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"should not run","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		checkoutCalls++
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if checkoutCalls != 0 {
		t.Fatalf("checkout validator calls = %d, want 0", checkoutCalls)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-readonly-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, string(workflow.JobBlocked)) || !daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, want blocked and permission_blocked", events)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want 1", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "**Decision:** `blocked`") || !strings.Contains(body, agentPermissionBlockedMessage) {
		t.Fatalf("comment body missing permission block:\n%s", body)
	}
}

func TestRunQueuedJobsSkipsReadOnlySideEffectsWhenJobAlreadyMoved(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskChangesRequested), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-readonly-race", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	jobSnapshot, err := store.GetJob(ctx, "job-readonly-race")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if transitioned, err := store.TransitionJobState(ctx, jobSnapshot.ID, string(workflow.JobQueued), string(workflow.JobCancelled)); err != nil || !transitioned {
		t.Fatalf("TransitionJobState returned transitioned=%v err=%v", transitioned, err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		t.Fatal("checkout validator should not run")
		return "", nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := worker.run(ctx, jobSnapshot); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-readonly-race")
	if err != nil {
		t.Fatalf("GetJob after run returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskChangesRequested) {
		t.Fatalf("task state = %q, want changes_requested", task.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, did not want permission_blocked", events)
	}
	if len(comments.posted) != 0 {
		t.Fatalf("posted comments = %+v, want none", comments.posted)
	}
}

func TestRunQueuedJobsAllowsReadOnlyAsk(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-readonly-ask", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"ask ran","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-readonly-ask")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
}

func TestRunQueuedJobsNormalizesRuntimePermissionFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-permission-fail", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	comments := &cliPollFakeGitHub{}
	adapter := &cliWorkerFakeAdapter{err: errors.New("sandbox rejected write: permission denied")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-permission-fail")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, want permission_blocked", events)
	}
	if len(comments.posted) != 1 || !strings.Contains(comments.posted[0].body, agentPermissionBlockedMessage) {
		t.Fatalf("posted comments = %+v, want permission block comment", comments.posted)
	}
}

func TestRunQueuedJobsPreservesGenericRuntimePermissionFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-generic-permission-fail", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	comments := &cliPollFakeGitHub{}
	adapter := &cliWorkerFakeAdapter{err: errors.New("permission denied reading api token")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-generic-permission-fail")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReviewing) {
		t.Fatalf("task state = %q, want reviewing", task.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, did not want permission_blocked", events)
	}
	if len(comments.posted) != 1 || strings.Contains(comments.posted[0].body, agentPermissionBlockedMessage) {
		t.Fatalf("posted comments = %+v, want original failure comment", comments.posted)
	}
}

func TestRunQueuedJobsPreservesAdvanceRetryForPostDeliveryPermissionError(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-advance-permission", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"implemented","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store: store,
			PayloadRefresher: func(context.Context, db.Job, workflow.JobPayload) (workflow.JobPayload, error) {
				return workflow.JobPayload{}, errors.New("permission denied while refreshing implemented head")
			},
		}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-advance-permission")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded result preserved", job.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_retry") {
		t.Fatalf("events = %+v, want advance_retry", events)
	}
	if daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, did not want permission_blocked", events)
	}
}

func TestRunQueuedJobsUsesMailboxRepairForMalformedOutput(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, `if printf '%s' "$1" | grep -q 'Previous raw output'; then printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"repaired","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'; else printf '%s\n' 'not json'; fi`, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-repair", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-repair")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) || !strings.Contains(job.Payload, `"summary":"repaired"`) {
		t.Fatalf("job after repair = %+v", job)
	}
	events, err := store.ListJobEvents(ctx, "job-repair")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "malformed_output") || !daemonWorkerHasEvent(events, "repair_retry") {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunQueuedJobsPostsAttributedResultComment(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-comment", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 7})
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{
			output: `{"gitmoot_result":{"decision":"approved","summary":"done with token=ghp_abcdefghijklmnopqrstuvwxyz123456","findings":[{"severity":"low","body":"ok"}],"changes_made":["commented"],"tests_run":["go test ./..."],"needs":["none"],"delegations":[{"id":"follow-up","agent":"lead","action":"ask","prompt":"coordinate next steps"}]}}`,
		}, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want 1", comments.posted)
	}
	body := comments.posted[0].body
	for _, want := range []string{
		"> Agent: `audit`",
		"> Runtime: `shell`",
		"> Job: `job-comment`",
		"**Decision:** `approved`",
		"**Summary:** done with token=[REDACTED]",
		"**Findings**",
		"**Changes Made**",
		"**Tests Run**",
		"**Needs**",
		"**Delegations**",
		"- lead",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("comment leaked token:\n%s", body)
	}
	events, err := store.ListJobEvents(ctx, "job-comment")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "comment_posted") {
		t.Fatalf("events = %+v, want comment_posted", events)
	}
}

func TestRunQueuedJobsPostsMalformedOutputDiagnosticComment(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-malformed", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 7})
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{output: "not valid json with token=ghp_abcdefghijklmnopqrstuvwxyz123456"}, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want 1", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "> Agent: `audit`") || !strings.Contains(body, "**Decision:** `failed`") || !strings.Contains(body, "**Diagnostics:**") {
		t.Fatalf("comment body missing failure diagnostics:\n%s", body)
	}
	if strings.Contains(body, "not valid json") || strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("comment leaked raw output:\n%s", body)
	}
	if !strings.Contains(body, "Raw runtime output was retained in local Gitmoot state") {
		t.Fatalf("comment did not mention local raw output retention:\n%s", body)
	}
}

func TestRunQueuedJobsPostsCheckoutDiagnosticWithoutCheckoutCwd(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", "/missing/gitmoot-checkout")
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-checkout-comment", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 7})
	comments := &cliPollFakeGitHub{}
	commenterDir := "unset"
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return "", errors.New("checkout path is missing")
	}
	worker.CommenterFactory = func(dir string) github.Client {
		commenterDir = dir
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if commenterDir != "" {
		t.Fatalf("commenter dir = %q, want empty cwd for PR comment posting", commenterDir)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want checkout diagnostic comment", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "**Diagnostics:** checkout path is missing") {
		t.Fatalf("comment body lost checkout diagnostic:\n%s", body)
	}
}

func TestDaemonWorkerTickRetriesFailedResultCommentPost(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-comment-retry", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 7})
	comments := &cliPollFakeGitHub{postErr: errors.New("temporary github error")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{
			output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		}, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if len(comments.posted) != 0 {
		t.Fatalf("posted comments = %+v, want failed post only", comments.posted)
	}
	events, err := store.ListJobEvents(ctx, "job-comment-retry")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "comment_post_failed") {
		t.Fatalf("events = %+v, want comment_post_failed", events)
	}

	comments.postErr = nil
	if err := retryPendingJobComments(ctx, worker, "owner/repo", "", newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("retryPendingJobComments returned error: %v", err)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want retry post", comments.posted)
	}
	events, err = store.ListJobEvents(ctx, "job-comment-retry")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "comment_posted") {
		t.Fatalf("events = %+v, want comment_posted", events)
	}
}

func TestRetryPendingJobCommentsPreservesStoredFailureDiagnostic(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-comment-diagnostic-retry", Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-comment-diagnostic-retry",
		Kind:    string(workflow.JobFailed),
		Message: "checkout validation failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-comment-diagnostic-retry", Kind: "comment_post_failed", Message: "temporary github error"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := retryPendingJobComments(ctx, worker, "owner/repo", "", newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("retryPendingJobComments returned error: %v", err)
	}

	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want retry post", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "**Diagnostics:** checkout validation failed") {
		t.Fatalf("comment body lost stored failure diagnostic:\n%s", body)
	}
}

func TestRunQueuedJobsPostsCommentAfterRetryDespitePriorComment(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-comment-after-retry", Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-comment-after-retry",
		Kind:    string(workflow.JobFailed),
		Message: "job failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-comment-after-retry", Kind: "comment_posted", Message: "old result comment"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	if _, err := workflow.RetryJob(ctx, store, "job-comment-after-retry"); err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{
			output: `{"gitmoot_result":{"decision":"approved","summary":"retried","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		}, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want retried result comment", comments.posted)
	}
	if !strings.Contains(comments.posted[0].body, "**Summary:** retried") {
		t.Fatalf("retried comment body = %s", comments.posted[0].body)
	}
}

func TestRunQueuedJobsDrainsBeyondWorkerLimit(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	for _, id := range []string{"job-1", "job-2", "job-3"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 3 {
		t.Fatalf("adapter calls = %d, want 3", adapter.calls)
	}
	for _, id := range []string{"job-1", "job-2", "job-3"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob(%s) returned error: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded", id, job.State)
		}
	}
}

func TestRunQueuedJobsDefersJobsEnqueuedByCurrentSnapshot(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-implement",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
		LeadAgent:   "lead",
		HeadSHA:     strings.Repeat("a", 40),
	})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"implemented","summary":"opened","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, RequiredReviewers: []string{"reviewer"}}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want only initial implementation job", adapter.calls)
	}
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Agent != "reviewer" || jobs[0].Type != "review" {
		t.Fatalf("queued jobs = %+v, want deferred reviewer job", jobs)
	}
}

func TestRunQueuedJobsRefreshesImplementedHeadBeforeReviewDispatch(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	oldHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-implement",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
		LeadAgent:   "lead",
		HeadSHA:     oldHead,
	})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"implemented","summary":"opened","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("implemented\n"), 0o644); err != nil {
				t.Fatalf("WriteFile returned error: %v", err)
			}
			runGit(t, checkout, "add", "README.md")
			runGit(t, checkout, "commit", "-m", "implement")
		},
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, "")
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	newHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if newHead == oldHead {
		t.Fatal("new HEAD did not change")
	}
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Agent != "reviewer" || jobs[0].Type != "review" {
		t.Fatalf("queued jobs = %+v, want reviewer job", jobs)
	}
	payload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.HeadSHA != newHead {
		t.Fatalf("review payload head = %q, want %q", payload.HeadSHA, newHead)
	}
}

func TestRunQueuedJobsSerializesSameRepoCheckout(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	for _, id := range []string{"job-a", "job-b"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	}
	var mu sync.Mutex
	active := 0
	maxActive := 0
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
		},
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if maxActive != 1 {
		t.Fatalf("max concurrent same-repo deliveries = %d, want 1", maxActive)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

func TestRunQueuedJobsSerializesSameRuntimeSessionAcrossRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	content := strings.Replace(config.DefaultConfig(paths), `same_session = "fork_temp_session"`, `same_session = "queue"`, 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
	var mu sync.Mutex
	active := 0
	maxActive := 0
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
		},
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if maxActive != 1 {
		t.Fatalf("max concurrent same-runtime deliveries = %d, want 1", maxActive)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

func TestRunQueuedJobsAllowsDifferentRuntimeSessionsAcrossRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit-a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo-a")
	seedDaemonWorkerAgent(t, store, "audit-b", runtime.CodexRuntime, "session-b", []string{"ask"}, "owner/repo-b")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit-a", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit-b", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
	var mu sync.Mutex
	active := 0
	maxActive := 0
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
		},
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if maxActive != 2 {
		t.Fatalf("max concurrent different-runtime deliveries = %d, want 2", maxActive)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

func TestRunQueuedJobsLeavesBusyRuntimeSessionQueued(t *testing.T) {
	resetRuntimeLockWaitEpisodes() // #598: the runtime_lock_wait dedup is package-global; a neighbor reusing job-a could otherwise suppress our write.
	ctx := context.Background()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	content := strings.Replace(config.DefaultConfig(paths), `same_session = "fork_temp_session"`, `same_session = "queue"`, 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapter.calls)
	}
	events, err := store.ListJobEvents(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "runtime_lock_wait") {
		t.Fatalf("events = %+v, want runtime_lock_wait", events)
	}
}

func TestRunQueuedJobsDelegatesBusyRuntimeToTempWorker(t *testing.T) {
	resetRuntimeLockWaitEpisodes() // #598: package-global runtime_lock_wait dedup; reset so a neighbor reusing job-a-merge-back can't suppress our write.
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-a",
		Agent:       "audit",
		Action:      "ask",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 7,
		HeadSHA:     "abc123",
		GoalID:      "goal-1",
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
	})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	startAdapter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440111"}
	tempRuntimeLockKey := "runtime:codex:550e8400-e29b-41d4-a716-446655440111"
	var lockObservedMu sync.Mutex
	lockObserved := false
	deliveryAdapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			if _, err := store.GetResourceLock(ctx, tempRuntimeLockKey); err != nil {
				t.Errorf("GetResourceLock during temp delivery returned error: %v", err)
				return
			}
			lockObservedMu.Lock()
			lockObserved = true
			lockObservedMu.Unlock()
		},
	}
	startCheckouts := []string{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(_ string, path string) (runtime.Adapter, error) {
		startCheckouts = append(startCheckouts, path)
		return startAdapter, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	if job.Agent == "audit" || !strings.HasPrefix(job.Agent, "audit-temp-job-a") {
		t.Fatalf("job agent = %q, want temp worker", job.Agent)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.OriginalAgent != "audit" || payload.DelegatedAgent != job.Agent || payload.DelegationReason != "runtime_session_busy" {
		t.Fatalf("delegation payload = %+v, job agent=%s", payload, job.Agent)
	}
	if startAdapter.startCalls != 1 || deliveryAdapter.calls != 1 {
		t.Fatalf("start calls=%d delivery calls=%d, want 1 each", startAdapter.startCalls, deliveryAdapter.calls)
	}
	lockObservedMu.Lock()
	observed := lockObserved
	lockObservedMu.Unlock()
	if !observed {
		t.Fatal("temp worker delivery did not observe runtime session lock")
	}
	if _, err := store.GetResourceLock(ctx, tempRuntimeLockKey); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetResourceLock after temp delivery returned error %v, want no rows", err)
	}
	if len(startCheckouts) != 1 || startCheckouts[0] != checkout {
		t.Fatalf("start checkouts = %+v, want %q", startCheckouts, checkout)
	}
	instance, err := store.GetAgentInstance(ctx, job.Agent)
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.Type != tempWorkerAgentType("audit") || instance.RuntimeRef != "550e8400-e29b-41d4-a716-446655440111" {
		t.Fatalf("temp instance = %+v", instance)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents returned error: %v", err)
	}
	for _, agent := range agents {
		if agent.Name == job.Agent {
			t.Fatalf("temp worker %q was persisted as regular agent: %+v", job.Agent, agents)
		}
	}
	events, err := store.ListJobEvents(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, want := range []string{"temp_worker_eligible", "temp_worker_delegated", "temp_worker_merge_back_queued"} {
		if !daemonWorkerHasEvent(events, want) {
			t.Fatalf("events = %+v, want %s", events, want)
		}
	}
	mergeBack, err := store.GetJob(ctx, "job-a-merge-back")
	if err != nil {
		t.Fatalf("GetJob merge-back returned error: %v", err)
	}
	if mergeBack.Agent != "audit" || mergeBack.Type != "ask" || mergeBack.State != string(workflow.JobQueued) {
		t.Fatalf("merge-back job = %+v, want queued ask for original agent", mergeBack)
	}
	mergePayload, err := daemonJobPayload(mergeBack)
	if err != nil {
		t.Fatalf("daemonJobPayload merge-back returned error: %v", err)
	}
	if mergePayload.Sender != job.Agent || !strings.Contains(mergePayload.Instructions, "Temporary worker") || !strings.Contains(mergePayload.Instructions, "completed job job-a") {
		t.Fatalf("merge-back payload = %+v", mergePayload)
	}
	if mergePayload.Branch != "task-1" || mergePayload.PullRequest != 0 || mergePayload.HeadSHA != "" {
		t.Fatalf("merge-back payload checkout fields = %+v, want branch only", mergePayload)
	}
	if !strings.Contains(mergePayload.Instructions, "Pull request: #7") || !strings.Contains(mergePayload.Instructions, "Head SHA: abc123") {
		t.Fatalf("merge-back instructions missing PR context: %q", mergePayload.Instructions)
	}
	if mergePayload.OriginalAgent != "audit" || mergePayload.DelegatedAgent != job.Agent || mergePayload.DelegationReason != "temp_worker_merge_back" {
		t.Fatalf("merge-back delegation payload = %+v, completed job agent=%s", mergePayload, job.Agent)
	}
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs merge-back returned error: %v", err)
	}
	mergeBack, err = store.GetJob(ctx, "job-a-merge-back")
	if err != nil {
		t.Fatalf("GetJob merge-back after retry returned error: %v", err)
	}
	if mergeBack.State != string(workflow.JobQueued) {
		t.Fatalf("merge-back state after busy original runtime = %q, want queued", mergeBack.State)
	}
	if startAdapter.startCalls != 1 || deliveryAdapter.calls != 1 {
		t.Fatalf("calls after merge-back retry start=%d delivery=%d, want no recursive temp worker", startAdapter.startCalls, deliveryAdapter.calls)
	}
	mergeEvents, err := store.ListJobEvents(ctx, "job-a-merge-back")
	if err != nil {
		t.Fatalf("ListJobEvents merge-back returned error: %v", err)
	}
	if !daemonWorkerHasEvent(mergeEvents, "runtime_lock_wait") {
		t.Fatalf("merge-back events = %+v, want runtime_lock_wait", mergeEvents)
	}
}

func TestRunQueuedJobsResumesDelegatedTempWorkerAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := config.SaveAgentType(paths, config.AgentType{
		Name:           "planner",
		Runtime:        runtime.CodexRuntime,
		Role:           "planner",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		IdleTimeout:    "17m",
		JobTimeout:     "3m",
	}); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	now := time.Now().UTC()
	if err := store.UpsertAgentInstance(ctx, db.AgentInstance{
		Name:           "audit",
		Type:           "planner",
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     "session-1",
		RepoFullName:   "owner/repo",
		Role:           "planner",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		State:          "running",
		CreatedAt:      now.Format(time.RFC3339Nano),
		LastUsedAt:     now.Format(time.RFC3339Nano),
		ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance original returned error: %v", err)
	}
	if err := store.UpsertAgentInstance(ctx, db.AgentInstance{
		Name:           "audit-temp-job-a",
		Type:           tempWorkerAgentType("audit"),
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     "550e8400-e29b-41d4-a716-446655440333",
		RepoFullName:   "owner/repo",
		Role:           "planner",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		State:          "idle",
		CreatedAt:      now.Format(time.RFC3339Nano),
		LastUsedAt:     now.Format(time.RFC3339Nano),
		ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance temp returned error: %v", err)
	}
	if _, err := store.GetAgent(ctx, "audit-temp-job-a"); err != nil {
		t.Fatalf("GetAgent temp instance fallback returned error: %v", err)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents returned error: %v", err)
	}
	for _, agent := range agents {
		if agent.Name == "audit-temp-job-a" {
			t.Fatalf("temp worker %q was persisted as regular agent: %+v", agent.Name, agents)
		}
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:             "owner/repo",
		Branch:           "main",
		Sender:           "tester",
		Instructions:     "continue",
		OriginalAgent:    "audit",
		DelegatedAgent:   "audit-temp-job-a",
		DelegationReason: "runtime_session_busy",
	})
	if err != nil {
		t.Fatalf("Marshal payload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-a", Agent: "audit-temp-job-a", Type: "ask", State: string(workflow.JobQueued), Payload: string(payload)}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	deliveryAdapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"resumed","findings":[],"changes_made":[],"tests_run":["go test"],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	instance, err := store.GetAgentInstance(ctx, "audit-temp-job-a")
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.State != "idle" {
		t.Fatalf("temp instance state = %q, want idle", instance.State)
	}
}

func TestRunQueuedJobsReturnsTempWorkerIdleAfterDeliveryError(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	startAdapter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440222"}
	deliveryAdapter := &cliWorkerFakeAdapter{err: errors.New("delivery failed")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(string, string) (runtime.Adapter, error) {
		return startAdapter, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	instance, err := store.GetAgentInstance(ctx, job.Agent)
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.State != "idle" {
		t.Fatalf("temp instance state = %q, want idle", instance.State)
	}
}

func TestRunQueuedJobsCleansTempWorkerWhenDelegationRaceLoses(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	startAdapter := &cliWorkerFakeAdapter{
		startRuntimeRef: "550e8400-e29b-41d4-a716-446655440444",
		onStart: func() {
			if _, err := workflow.CancelJob(ctx, store, "job-a"); err != nil {
				t.Fatalf("CancelJob returned error: %v", err)
			}
		},
	}
	deliveryAdapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(string, string) (runtime.Adapter, error) {
		return startAdapter, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	if _, err := store.GetAgentInstance(ctx, "audit-temp-job-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgentInstance temp error = %v, want no rows", err)
	}
	if _, err := store.GetAgent(ctx, "audit-temp-job-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgent temp error = %v, want no rows", err)
	}
}

// TestRunQueuedJobsMaterializesAndDisposesEphemeralWorker proves a queued child
// carrying an inline EphemeralSpec (no pre-registered agent) gets a throwaway
// agent materialized from the spec, runs the job (Start + Deliver invoked), and
// is auto-disposed afterwards — both the agent row and its instance are gone.
func TestRunQueuedJobsMaterializesAndDisposesEphemeralWorker(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	ephemeralName := "reviewer-ephemeral-abc1234567"
	// No agent row is seeded: the worker must be materialized from the spec.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:        "job-eph",
		Agent:     ephemeralName,
		Action:    "ask",
		Repo:      "owner/repo",
		Branch:    "main",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.CodexRuntime, Model: "gpt-5.4", Effort: "high"},
	})

	startAdapter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440555"}
	deliveryAdapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	var deliveredModel, deliveredEffort string
	startCheckouts := []string{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(_ string, path string) (runtime.Adapter, error) {
		startCheckouts = append(startCheckouts, path)
		return startAdapter, nil
	}
	worker.AdapterFactory = func(agent runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		deliveredModel = agent.Model
		deliveredEffort = agent.Effort
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-eph")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	if startAdapter.startCalls != 1 || deliveryAdapter.calls != 1 {
		t.Fatalf("start calls=%d delivery calls=%d, want 1 each", startAdapter.startCalls, deliveryAdapter.calls)
	}
	if deliveredModel != "gpt-5.4" {
		t.Fatalf("delivered agent model = %q, want gpt-5.4", deliveredModel)
	}
	if deliveredEffort != "high" {
		t.Fatalf("delivered agent effort = %q, want high", deliveredEffort)
	}
	if len(startCheckouts) != 1 || startCheckouts[0] != checkout {
		t.Fatalf("start checkouts = %+v, want %q", startCheckouts, checkout)
	}
	// The throwaway agent row and its instance must be gone after the run.
	if _, err := store.GetAgentInstance(ctx, ephemeralName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgentInstance after run error = %v, want no rows", err)
	}
	if _, err := store.GetAgent(ctx, ephemeralName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgent after run error = %v, want no rows", err)
	}
	if !daemonWorkerHasEvent(mustListJobEvents(t, store, "job-eph"), "ephemeral_worker_started") {
		t.Fatalf("missing ephemeral_worker_started event")
	}
}

// TestRunQueuedJobsDisposesEphemeralWorkerOnFailure proves the throwaway agent
// is auto-disposed even when delivery fails: the job reaches a terminal failed
// state and neither the agent row nor its instance survives.
func TestRunQueuedJobsDisposesEphemeralWorkerOnFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	ephemeralName := "impl-ephemeral-def7654321"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:        "job-eph-fail",
		Agent:     ephemeralName,
		Action:    "ask",
		Repo:      "owner/repo",
		Branch:    "main",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.CodexRuntime, Model: "gpt-5.4"},
	})

	startAdapter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440666"}
	deliveryAdapter := &cliWorkerFakeAdapter{err: errors.New("delivery failed")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.StartAdapterFactory = func(string, string) (runtime.Adapter, error) {
		return startAdapter, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deliveryAdapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-eph-fail")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	if deliveryAdapter.calls != 1 {
		t.Fatalf("delivery calls = %d, want 1", deliveryAdapter.calls)
	}
	// Cleanup must still run on the failure path.
	if _, err := store.GetAgentInstance(ctx, ephemeralName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgentInstance after failed run error = %v, want no rows", err)
	}
	if _, err := store.GetAgent(ctx, ephemeralName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAgent after failed run error = %v, want no rows", err)
	}
}

func TestRunQueuedJobsPreservesCreationOrderForSameRepo(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	for _, id := range []string{"job-z", "job-a"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	want := []string{"job-z", "job-a"}
	if !reflect.DeepEqual(adapter.delivered, want) {
		t.Fatalf("delivered jobs = %v, want %v", adapter.delivered, want)
	}
}

func TestRunQueuedJobsPreservesCancellationRace(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-cancel", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"late","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			if _, err := workflow.CancelJob(ctx, store, "job-cancel"); err != nil {
				t.Fatalf("CancelJob returned error: %v", err)
			}
		},
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	if len(comments.posted) != 0 {
		t.Fatalf("posted comments = %+v, want no comment for cancelled job", comments.posted)
	}
	events, err := store.ListJobEvents(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "cancel_settled") {
		t.Fatalf("events = %+v, want cancel_settled", events)
	}
}

func TestRunQueuedJobsCancelsActiveDeliveryContext(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-active-cancel", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{
		waitForContextCancel: true,
		onDeliver: func() {
			if _, err := workflow.CancelJob(ctx, store, "job-active-cancel"); err != nil {
				t.Fatalf("CancelJob returned error: %v", err)
			}
		},
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if !adapter.observedContextCancel() {
		t.Fatal("adapter did not observe context cancellation")
	}
	job, err := store.GetJob(ctx, "job-active-cancel")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-active-cancel")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "cancel_settled") {
		t.Fatalf("events = %+v, want cancel_settled", events)
	}
}

func TestRunQueuedJobsReleasesRuntimeSessionLockAfterCancellation(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-cancel", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-active-cancel", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{
		waitForContextCancel: true,
		onDeliver: func() {
			if _, err := workflow.CancelJob(ctx, store, "job-active-cancel"); err != nil {
				t.Fatalf("CancelJob returned error: %v", err)
			}
		},
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if !adapter.observedContextCancel() {
		t.Fatal("adapter did not observe context cancellation")
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-cancel"); err == nil || err != sql.ErrNoRows {
		t.Fatalf("runtime lock after cancellation error = %v, want no rows", err)
	}
}

func TestRunQueuedJobsUsesConfiguredMergeGate(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-review",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
	})
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true}}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if gate.calls != 1 {
		t.Fatalf("merge gate calls = %d, want 1", gate.calls)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskMerged) {
		t.Fatalf("task state = %q, want merged", task.State)
	}
}

func TestRunQueuedJobsFailsImplementWithoutBranchLockBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-implement", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, TaskID: "task-1"})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want preflight to stop delivery", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

func TestRunQueuedJobsUsesTaskWorktreeForImplement(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-1")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	head, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertAgent(ctx, db.Agent{Name: "reviewer", Role: "reviewer", Runtime: runtime.ShellRuntime, RuntimeRef: "unused", RepoScope: "owner/repo", Capabilities: []string{"review"}, AutonomyPolicy: runtime.AutonomyPolicyAuto, HealthStatus: "ok"}); err != nil {
		t.Fatalf("UpsertAgent reviewer returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-implement-worktree", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, HeadSHA: head, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"implemented","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return workflow.Engine{
			Store:             store,
			RequiredReviewers: []string{"reviewer"},
			PayloadRefresher: func(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
				return refreshDaemonJobPayload(ctx, store, checkout, job, payload)
			},
		}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	wantCheckout, err := filepath.Abs(worktree)
	if err != nil {
		t.Fatalf("Abs returned error: %v", err)
	}
	if adapterCheckout != filepath.Clean(wantCheckout) {
		t.Fatalf("adapter checkout = %q, want %q", adapterCheckout, filepath.Clean(wantCheckout))
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunQueuedJobsResumesSelfDirtyTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-resume")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-resume", worktree, "main")
	head, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	completedWork := filepath.Join(worktree, "completed-work.txt")
	if err := os.WriteFile(completedWork, []byte("completed before runtime death\n"), 0o644); err != nil {
		t.Fatalf("WriteFile completed work returned error: %v", err)
	}

	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-resume",
		RepoFullName: "owner/repo",
		GoalID:       "goal-1",
		Title:        "Resume completed work",
		State:        string(workflow.TaskImplementing),
		Branch:       "task-resume",
		WorktreePath: worktree,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-resume", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:           "job-resume-dirty",
		Agent:        "lead",
		Action:       "implement",
		Repo:         "owner/repo",
		Branch:       "task-resume",
		HeadSHA:      head,
		GoalID:       "goal-1",
		TaskID:       "task-resume",
		TaskTitle:    "Resume completed work",
		Instructions: "Finish the implementation.",
	})

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"prior work is complete","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1 resumed delivery", adapter.calls)
	}
	wantCheckout, err := normalizeTaskWorktreePath(worktree)
	if err != nil {
		t.Fatalf("normalizeTaskWorktreePath returned error: %v", err)
	}
	if adapterCheckout != wantCheckout {
		t.Fatalf("adapter checkout = %q, want task worktree %q", adapterCheckout, wantCheckout)
	}
	if len(adapter.prompts) != 1 || !strings.Contains(adapter.prompts[0], "COMPLETED work that is present but UNCOMMITTED") {
		t.Fatalf("delivered prompt missing self-dirty resume notice: %q", adapter.prompts)
	}
	if strings.Contains(adapter.prompts[0], "NOTE (operational retry") || strings.Contains(adapter.prompts[0], "pushed branches") {
		t.Fatalf("delivered prompt carried generic operational-blocker reconciliation notice: %q", adapter.prompts[0])
	}

	job, err := store.GetJob(ctx, "job-resume-dirty")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if !payload.ResumedSelfDirtyWorktree {
		t.Fatalf("payload does not mark resumed self-dirty worktree: %+v", payload)
	}
	if payload.BlockerAttempts != 1 || payload.BlockerRetryAt != "" || !payload.BlockerPreDelivery {
		t.Fatalf("resume blocker fields = attempts=%d retry_at=%q pre_delivery=%v", payload.BlockerAttempts, payload.BlockerRetryAt, payload.BlockerPreDelivery)
	}
	if payload.HeadSHA != head {
		t.Fatalf("payload head = %q, want original base %q", payload.HeadSHA, head)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	resumedEvent := false
	failedEvent := false
	for _, event := range events {
		resumedEvent = resumedEvent || event.Kind == "worktree_resumed"
		failedEvent = failedEvent || event.Kind == string(workflow.JobFailed) || event.Kind == "job.failed"
	}
	if !resumedEvent {
		t.Fatalf("job events missing worktree_resumed: %+v", events)
	}
	if failedEvent {
		t.Fatalf("resumed job recorded a failure event: %+v", events)
	}
	contents, err := os.ReadFile(completedWork)
	if err != nil {
		t.Fatalf("prior completed work did not survive re-delivery: %v", err)
	}
	if string(contents) != "completed before runtime death\n" {
		t.Fatalf("prior completed work changed during resume: %q", contents)
	}
}

func TestRunQueuedJobsResumesDelegatedImplementWithOriginalBranchLock(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-1")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	head, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead-temp-job-implement", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:               "job-implement",
		Agent:            "lead-temp-job-implement",
		Action:           "implement",
		Repo:             "owner/repo",
		Branch:           "task-1",
		HeadSHA:          head,
		GoalID:           "goal-1",
		TaskID:           "task-1",
		TaskTitle:        "Task 1",
		LeadAgent:        "lead",
		OriginalAgent:    "lead",
		DelegatedAgent:   "lead-temp-job-implement",
		DelegationReason: "runtime_session_busy",
	})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"implemented","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return workflow.Engine{
			Store: store,
			PayloadRefresher: func(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
				return refreshDaemonJobPayload(ctx, store, checkout, job, payload)
			},
		}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	wantCheckout := filepath.Clean(worktree)
	if adapterCheckout != wantCheckout {
		t.Fatalf("adapter checkout = %q, want %q", adapterCheckout, wantCheckout)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunQueuedJobsUsesTaskWorktreeForReview(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-1")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	head, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review-worktree", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, HeadSHA: head, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	wantCheckout, err := filepath.Abs(worktree)
	if err != nil {
		t.Fatalf("Abs returned error: %v", err)
	}
	if adapterCheckout != filepath.Clean(wantCheckout) {
		t.Fatalf("adapter checkout = %q, want %q", adapterCheckout, filepath.Clean(wantCheckout))
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunQueuedJobsKeepsReviewOnRegisteredCheckoutWithoutTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "task-1")
	head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskReviewing), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review-main-checkout", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, HeadSHA: head, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1"})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	adapterCheckout := ""
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		adapterCheckout = checkout
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapterCheckout != checkout {
		t.Fatalf("adapter checkout = %q, want registered checkout %q", adapterCheckout, checkout)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestSelectRunnableQueuedJobsAllowsSeparateTaskWorktrees(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "lead-a", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead-b", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: "/tmp/gitmoot/task-a"}); err != nil {
		t.Fatalf("UpsertTask task-a returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-b", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-b", WorktreePath: "/tmp/gitmoot/task-b"}); err != nil {
		t.Fatalf("UpsertTask task-b returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead-a", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "lead-b", Action: "implement", Repo: "owner/repo", Branch: "task-b", TaskID: "task-b"})
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}

	selected, remaining := selectRunnableQueuedJobsWithPolicy(ctx, store, jobs, 2, config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue})

	if len(selected) != 2 || len(remaining) != 0 {
		t.Fatalf("selected=%d remaining=%d, want two selected separate worktrees", len(selected), len(remaining))
	}
}

func TestSelectRunnableQueuedJobsKeepsSameRuntimeSerializedAcrossWorktrees(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "lead-a", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead-b", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: "/tmp/gitmoot/task-a"}); err != nil {
		t.Fatalf("UpsertTask task-a returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-b", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-b", WorktreePath: "/tmp/gitmoot/task-b"}); err != nil {
		t.Fatalf("UpsertTask task-b returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead-a", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "lead-b", Action: "implement", Repo: "owner/repo", Branch: "task-b", TaskID: "task-b"})
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}

	selected, remaining := selectRunnableQueuedJobsWithPolicy(ctx, store, jobs, 2, config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue})

	if len(selected) != 1 || len(remaining) != 1 {
		t.Fatalf("selected=%d remaining=%d, want same runtime session serialized", len(selected), len(remaining))
	}
}

func TestSelectRunnableQueuedJobsAllowsForkEligibleSameRuntime(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead-a", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead-b", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	worktreeA := t.TempDir()
	worktreeB := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: worktreeA}); err != nil {
		t.Fatalf("UpsertTask task-a returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-b", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-b", WorktreePath: worktreeB}); err != nil {
		t.Fatalf("UpsertTask task-b returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead-a", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "lead-b", Action: "implement", Repo: "owner/repo", Branch: "task-b", TaskID: "task-b"})
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}

	selected, remaining := selectRunnableQueuedJobsWithPolicy(ctx, store, jobs, 2, config.DefaultParallelSessionPolicy())

	if len(selected) != 2 || len(remaining) != 0 {
		t.Fatalf("selected=%d remaining=%d, want fork-eligible same runtime selected", len(selected), len(remaining))
	}
}

func TestSelectRunnableQueuedJobsCountsExternallyBusyRuntimeAgainstTempCap(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	worktreeA := t.TempDir()
	worktreeB := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: worktreeA}); err != nil {
		t.Fatalf("UpsertTask task-a returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-b", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-b", WorktreePath: worktreeB}); err != nil {
		t.Fatalf("UpsertTask task-b returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-b", TaskID: "task-b"})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	policy := config.DefaultParallelSessionPolicy()
	policy.MaxTempSessionsPerAgent = 1

	selected, remaining := selectRunnableQueuedJobsWithPolicy(ctx, store, jobs, 2, policy)

	if len(selected) != 1 || len(remaining) != 1 {
		t.Fatalf("selected=%d remaining=%d, want external busy runtime counted against temp cap", len(selected), len(remaining))
	}
}

func TestRunQueuedJobsFailsReviewOnWrongCheckoutBranchBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, HeadSHA: head})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want preflight to stop delivery", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-review")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

// A wrong-checkout-HEAD review pre-flight is a checkout_contention (#532 slice C):
// it DEFERS (queued with a suggested_action) instead of failing terminally, since
// the checkout may just be mid-sync — pre-flight still stops delivery.
func TestRunQueuedJobsDefersReviewOnWrongCheckoutHeadBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-review", Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, HeadSHA: strings.Repeat("0", 40), TaskID: "task-1"})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want preflight to stop delivery", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-review")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued (checkout_contention deferral)", job.State)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.BlockerClass != string(blockerClassCheckoutContention) || strings.TrimSpace(payload.BlockerSuggestedAction) == "" {
		t.Fatalf("wrong-head review did not defer as checkout_contention with an action: %+v", payload)
	}
}

// A wrong-checkout-HEAD PR-scoped ask pre-flight also DEFERS as
// checkout_contention (#532 slice C) rather than failing terminally.
func TestRunQueuedJobsDefersPRScopedAskOnWrongCheckoutHeadBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-ask", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "task-1", PullRequest: 1, HeadSHA: strings.Repeat("0", 40)})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want preflight to stop delivery", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-ask")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued (checkout_contention deferral)", job.State)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.BlockerClass != string(blockerClassCheckoutContention) {
		t.Fatalf("wrong-head ask did not defer as checkout_contention: %+v", payload)
	}
}

func TestRunQueuedJobsForRepoSkipsOtherRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "owner/repo-a", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}

	jobA, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob job-a returned error: %v", err)
	}
	jobB, err := store.GetJob(ctx, "job-b")
	if err != nil {
		t.Fatalf("GetJob job-b returned error: %v", err)
	}
	if jobA.State != string(workflow.JobSucceeded) {
		t.Fatalf("job-a state = %q, want succeeded", jobA.State)
	}
	if jobB.State != string(workflow.JobQueued) {
		t.Fatalf("job-b state = %q, want queued", jobB.State)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunQueuedJobsPoolDrainsIndependentJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	worker := poolSchedulerWorker(t, store, adapter, true)

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("pool runQueuedJobsForRepo: %v", err)
	}
	for _, id := range []string{"job-a", "job-b"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded", id, job.State)
		}
	}
}

func TestRunQueuedJobsPoolPicksUpMidFlightJob(t *testing.T) {
	if got := runMidFlightEnqueueScenario(t, true); got != string(workflow.JobSucceeded) {
		t.Fatalf("pool: job-b state = %q, want succeeded (mid-flight job not picked up)", got)
	}
	if got := runMidFlightEnqueueScenario(t, false); got != string(workflow.JobQueued) {
		t.Fatalf("barrier: job-b state = %q, want queued (barrier must not re-query mid-tick)", got)
	}
}

func TestRunQueuedJobsPoolStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	// The worker blocks until its context is cancelled; the pool must drain it and
	// return rather than hang.
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult, waitForContextCancel: true}
	worker := poolSchedulerWorker(t, store, adapter, true)

	errCh := make(chan error, 1)
	go func() { errCh <- runQueuedJobsForRepo(ctx, worker, 2, "", "") }()
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not return after context cancellation (hang)")
	}
}

func TestRunQueuedJobsPoolHonorsCheckoutSafety(t *testing.T) {
	// Same repo, no worktree ⇒ same checkout key ⇒ live accounting must serialize
	// them even at --workers 2 (working-tree safety, #394 layer 2).
	if peak := runPoolConcurrencyScenario(t, "owner/repo-a"); peak != 1 {
		t.Fatalf("same-repo peak concurrency = %d, want 1 (must serialize same checkout key)", peak)
	}
	// Distinct repos ⇒ distinct checkout keys ⇒ the pool runs them in parallel.
	if peak := runPoolConcurrencyScenario(t, "owner/repo-b"); peak != 2 {
		t.Fatalf("distinct-repo peak concurrency = %d, want 2 (distinct keys must parallelize)", peak)
	}
}

func TestRunQueuedJobsPoolIsolatesContendedReadJob(t *testing.T) {
	// Two same-repo read (ask) jobs under the pool with isolation enabled
	// (ConfigHome + a real checkout): one runs in the shared checkout, the other is
	// auto-isolated into a detached worktree so it runs beside it (#394 part 2)
	// instead of serializing/deadlocking, and the worktree is disposed afterward.
	ctx := context.Background()
	home := t.TempDir()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-1", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-2", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	worker := poolSchedulerWorker(t, store, adapter, true)
	worker.ConfigHome = home // enable isolation

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("pool run: %v", err)
	}

	isolated := 0
	for _, id := range []string{"job-1", "job-2"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded (contended read job must run, not stay queued)", id, job.State)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("payload %s: %v", id, err)
		}
		if payload.WorktreePath != "" {
			isolated++
			if _, statErr := os.Stat(payload.WorktreePath); !os.IsNotExist(statErr) {
				t.Fatalf("%s isolation worktree %s was not cleaned up", id, payload.WorktreePath)
			}
		}
	}
	if isolated != 1 {
		t.Fatalf("isolated read jobs = %d, want exactly 1 (the contended one)", isolated)
	}
}

func TestPoolIsolationAppendsCommittedTipNote(t *testing.T) {
	// #696: three same-repo top-level read-only (ask) jobs submitted together under
	// the pool run concurrently — one in the shared checkout, the other two
	// auto-isolated into detached committed-tip worktrees (#394 part 2). Each
	// auto-isolated job must carry the #654 canonical-checkout note in its prompt
	// (parity with read-only delegation fan-out), while the un-isolated job that
	// runs in the shared base checkout stays byte-identical (no note). Every
	// worktree is disposed on completion.
	ctx := context.Background()
	home := t.TempDir()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	const goal = "audit the config loader"
	for i, id := range []string{"job-1", "job-2", "job-3"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: i + 1, Instructions: goal})
	}
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	worker := poolSchedulerWorker(t, store, adapter, true)
	worker.ConfigHome = home // enable isolation

	if err := runQueuedJobsForRepo(ctx, worker, 3, "", ""); err != nil {
		t.Fatalf("pool run: %v", err)
	}

	isolated, shared := 0, 0
	for _, id := range []string{"job-1", "job-2", "job-3"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded (all three read jobs must run concurrently, not stay queued)", id, job.State)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("payload %s: %v", id, err)
		}
		if payload.WorktreePath != "" {
			isolated++
			if _, statErr := os.Stat(payload.WorktreePath); !os.IsNotExist(statErr) {
				t.Fatalf("%s isolation worktree %s was not cleaned up", id, payload.WorktreePath)
			}
			// The auto-isolated job runs in a detached committed-tip worktree, so it
			// must carry the #654 note pointing at the canonical repo checkout.
			if !strings.Contains(payload.Instructions, "COMMITTED TIP") || !strings.Contains(payload.Instructions, checkout) {
				t.Fatalf("%s isolated but Instructions missing committed-tip note pointing at %q: %q", id, checkout, payload.Instructions)
			}
			if !strings.HasPrefix(payload.Instructions, goal) {
				t.Fatalf("%s note must be APPENDED after the original goal, got %q", id, payload.Instructions)
			}
		} else {
			shared++
			// The un-isolated job stays in the shared base checkout: its prompt must
			// be byte-identical (no committed-tip note appended).
			if payload.Instructions != goal {
				t.Fatalf("%s ran in the shared checkout but its Instructions changed: %q (want byte-identical %q)", id, payload.Instructions, goal)
			}
		}
	}
	if isolated != 2 || shared != 1 {
		t.Fatalf("isolated=%d shared=%d, want exactly 2 isolated + 1 shared", isolated, shared)
	}
}

func TestRunQueuedJobsPoolRecoversWorkerPanicAndCleansWorktree(t *testing.T) {
	// A panicking worker must not hang the pool or crash the daemon, and any
	// isolation worktree allocated for the contended job must still be disposed
	// (the always-send-to-done invariant keeps reap's cleanup intact).
	ctx := context.Background()
	home := t.TempDir()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-1", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-2", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = func() { panic("boom") }
	worker := poolSchedulerWorker(t, store, adapter, true)
	worker.ConfigHome = home

	resultCh := make(chan error, 1)
	go func() { resultCh <- runQueuedJobsForRepo(ctx, worker, 2, "", "") }()
	select {
	case err := <-resultCh:
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("err = %v, want a recovered panic error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pool hung after a worker panic (recovery did not send to done)")
	}
	for _, id := range []string{"job-1", "job-2"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("payload %s: %v", id, err)
		}
		if payload.WorktreePath != "" {
			if _, statErr := os.Stat(payload.WorktreePath); !os.IsNotExist(statErr) {
				t.Fatalf("%s isolation worktree %s leaked after panic", id, payload.WorktreePath)
			}
		}
	}
}

// TestRunQueuedJobsBarrierAdmissionDefers proves the host-global session cap (#365)
// serializes two otherwise-parallel jobs under the barrier scheduler, and that a
// nil Admission (the default) preserves byte-identical parallel behavior.
func TestRunQueuedJobsBarrierAdmissionDefers(t *testing.T) {
	if peak := runAdmissionConcurrencyScenario(t, false, nil); peak != 2 {
		t.Fatalf("nil-Admission barrier peak = %d, want 2 (default parallelism must be unchanged)", peak)
	}
	budget := newAdmissionBudget(config.AdmissionPolicy{MaxConcurrentSessions: 1})
	if peak := runAdmissionConcurrencyScenario(t, false, budget); peak != 1 {
		t.Fatalf("max_concurrent_sessions=1 barrier peak = %d, want 1 (cap must override --workers)", peak)
	}
}

// TestRunQueuedJobsPoolAdmissionDefers proves the same host-global cap serializes
// two otherwise-parallel jobs under the pool scheduler, and the nil default keeps
// the pool's parallel dispatch byte-identical.
func TestRunQueuedJobsPoolAdmissionDefers(t *testing.T) {
	if peak := runAdmissionConcurrencyScenario(t, true, nil); peak != 2 {
		t.Fatalf("nil-Admission pool peak = %d, want 2 (default parallelism must be unchanged)", peak)
	}
	budget := newAdmissionBudget(config.AdmissionPolicy{MaxConcurrentSessions: 1})
	if peak := runAdmissionConcurrencyScenario(t, true, budget); peak != 1 {
		t.Fatalf("max_concurrent_sessions=1 pool peak = %d, want 1 (cap must override pool width)", peak)
	}
}

// TestRunQueuedJobsAdmissionMemoryCapDefers proves the memory gate serializes two
// jobs whose summed per-runtime RAM estimate exceeds the cap. Two codex sessions
// (0.2GB prior each) fit a 0.5GB cap together but two claude sessions (0.85GB
// each) do not, so the claude pair serializes.
func TestRunQueuedJobsAdmissionMemoryCapDefers(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+`
[admission]
max_memory_gb = 1.0
codex_memory_gb = 0.2
claude_memory_gb = 0.85
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "claude-a", runtime.ClaudeRuntime, "session-a", []string{"ask"}, "owner/repo-a")
	seedDaemonWorkerAgent(t, store, "claude-b", runtime.ClaudeRuntime, "session-b", []string{"ask"}, "owner/repo-b")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "claude-a", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "claude-b", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})

	tracker := &poolConcurrencyTracker{}
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = tracker.span
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.Admission = worker.loadAdmissionBudget()
	if worker.Admission == nil {
		t.Fatal("a [admission] config with max_memory_gb set must yield a non-nil budget")
	}

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo: %v", err)
	}
	if peak := tracker.peak(); peak != 1 {
		t.Fatalf("two 0.85GB claude jobs under a 1.0GB cap peak = %d, want 1 (summed estimate must defer)", peak)
	}
	for _, id := range []string{"job-a", "job-b"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded", id, job.State)
		}
	}
}

func TestRunQueuedJobsForRepoSkipsOtherSessions(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	// The root coordinator job (its own id is the root) and a child carrying the
	// root id both belong to session "root-coordinator"; a job from a different
	// root does not.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "root-coordinator", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "child-of-root", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 2, RootJobID: "root-coordinator"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "other-root-job", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 3, RootJobID: "other-root"})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 3, "", "root-coordinator"); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}

	root, err := store.GetJob(ctx, "root-coordinator")
	if err != nil {
		t.Fatalf("GetJob root-coordinator returned error: %v", err)
	}
	child, err := store.GetJob(ctx, "child-of-root")
	if err != nil {
		t.Fatalf("GetJob child-of-root returned error: %v", err)
	}
	other, err := store.GetJob(ctx, "other-root-job")
	if err != nil {
		t.Fatalf("GetJob other-root-job returned error: %v", err)
	}
	// The root coordinator (job.ID == session) ran.
	if root.State != string(workflow.JobSucceeded) {
		t.Fatalf("root-coordinator state = %q, want succeeded", root.State)
	}
	// The child (payload.RootJobID == session) ran.
	if child.State != string(workflow.JobSucceeded) {
		t.Fatalf("child-of-root state = %q, want succeeded", child.State)
	}
	// The non-matching root stayed queued.
	if other.State != string(workflow.JobQueued) {
		t.Fatalf("other-root-job state = %q, want queued", other.State)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

func TestRunQueuedJobsForRepoAppliesRepoAndSessionAndMatch(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	// Only the job matching BOTH repo AND session should run.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "match", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1, RootJobID: "root-coordinator"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "wrong-repo", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2, RootJobID: "root-coordinator"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "wrong-session", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 3, RootJobID: "other-root"})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 3, "owner/repo-a", "root-coordinator"); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}

	match, err := store.GetJob(ctx, "match")
	if err != nil {
		t.Fatalf("GetJob match returned error: %v", err)
	}
	wrongRepo, err := store.GetJob(ctx, "wrong-repo")
	if err != nil {
		t.Fatalf("GetJob wrong-repo returned error: %v", err)
	}
	wrongSession, err := store.GetJob(ctx, "wrong-session")
	if err != nil {
		t.Fatalf("GetJob wrong-session returned error: %v", err)
	}
	if match.State != string(workflow.JobSucceeded) {
		t.Fatalf("match state = %q, want succeeded", match.State)
	}
	if wrongRepo.State != string(workflow.JobQueued) {
		t.Fatalf("wrong-repo state = %q, want queued", wrongRepo.State)
	}
	if wrongSession.State != string(workflow.JobQueued) {
		t.Fatalf("wrong-session state = %q, want queued", wrongSession.State)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunEnabledRepoWorkerTicksSkipsDisabledRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/enabled", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/disabled", t.TempDir())
	if err := store.SetRepoEnabled(ctx, "owner/disabled", false); err != nil {
		t.Fatalf("SetRepoEnabled returned error: %v", err)
	}
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/enabled")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/disabled"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-enabled", Agent: "audit", Action: "ask", Repo: "owner/enabled", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-disabled", Agent: "audit", Action: "ask", Repo: "owner/disabled", Branch: "main", PullRequest: 2})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 2, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("runEnabledRepoWorkerTicks returned error: %v", err)
	}

	enabledJob, err := store.GetJob(ctx, "job-enabled")
	if err != nil {
		t.Fatalf("GetJob job-enabled returned error: %v", err)
	}
	disabledJob, err := store.GetJob(ctx, "job-disabled")
	if err != nil {
		t.Fatalf("GetJob job-disabled returned error: %v", err)
	}
	if enabledJob.State != string(workflow.JobSucceeded) {
		t.Fatalf("enabled job state = %q, want succeeded", enabledJob.State)
	}
	if disabledJob.State != string(workflow.JobQueued) {
		t.Fatalf("disabled job state = %q, want queued", disabledJob.State)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestRunQueuedJobsRecordsPostDeliveryWorkflowErrorForRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-review",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		HeadSHA:     strings.Repeat("a", 40),
	})
	gate := &cliWorkerFakeMergeGate{err: errors.New("github unavailable")}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	job, getErr := store.GetJob(ctx, "job-review")
	if getErr != nil {
		t.Fatalf("GetJob returned error: %v", getErr)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded result preserved", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_retry") {
		t.Fatalf("events = %+v, want advance retry event", events)
	}
	gate.err = nil
	gate.decision = workflow.MergeDecision{Ready: true}
	if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want no redelivery during advance retry", adapter.calls)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want ready_to_merge", task.State)
	}
	events, err = store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_retried") {
		t.Fatalf("events = %+v, want advance retried event", events)
	}
}

// TestRetryPendingJobAdvancementsDoesNotAccumulateAdvanceRetry guards the fix for
// the unbounded job_events growth that pinned a daemon core: a job whose
// post-delivery advancement keeps failing must NOT append a fresh advance_retry
// on every tick. Across many failed retry passes the job stays a candidate but
// its advance_retry row count stays at one.
func TestRetryPendingJobAdvancementsDoesNotAccumulateAdvanceRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-review",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		HeadSHA:     strings.Repeat("a", 40),
	})
	gate := &cliWorkerFakeMergeGate{err: errors.New("github unavailable")}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	// The gate stays broken: every retry pass fails advancement again.
	for i := 0; i < 5; i++ {
		if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(worker.Store)); err != nil {
			t.Fatalf("retryPendingJobAdvancements pass %d returned error: %v", i, err)
		}
	}
	events, err := store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	retries := 0
	for _, e := range events {
		if e.Kind == "advance_retry" {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("advance_retry events = %d after 5 failed retry passes, want 1 (no per-tick accumulation)", retries)
	}
	// Still a live candidate — the job is not silently dropped from retry.
	ids, err := store.JobIDsWithPendingAdvanceRetry(ctx)
	if err != nil {
		t.Fatalf("JobIDsWithPendingAdvanceRetry returned error: %v", err)
	}
	if len(ids) != 1 || ids[0] != "job-review" {
		t.Fatalf("pending advance-retry candidates = %v, want [job-review]", ids)
	}
}

func TestRetryPendingJobAdvancementsRecoversStartedAdvancement(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	payload := workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		Result:      &workflow.AgentResult{Decision: "approved", Summary: "approved"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-review", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-review",
		Kind:    string(workflow.JobSucceeded),
		Message: "job succeeded",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-review", Kind: "advance_started", Message: "workflow advancement started"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: true}}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}

	if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want ready_to_merge", task.State)
	}
	events, err := store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_retried") {
		t.Fatalf("events = %+v, want advance retried event", events)
	}
}

func TestRetryPendingJobAdvancementsAdvancesFailedStoredResult(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	payload := workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		Result:      &workflow.AgentResult{Decision: "failed", Summary: "tests failed"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-review", Agent: "reviewer", Type: "review", State: string(workflow.JobFailed), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-review",
		Kind:    string(workflow.JobFailed),
		Message: "job failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-review", Kind: "advance_retry", Message: "transient workflow failure"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}

	if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
	events, err := store.ListJobEvents(ctx, "job-review")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "advance_blocked") {
		t.Fatalf("events = %+v, want advance blocked event", events)
	}
}

func TestRetryPendingJobAdvancementsRefreshesImplementedHeadBeforePreflight(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "task-1")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	oldHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("implemented\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "implement")
	newHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	payload := workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		HeadSHA:     oldHead,
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
		LeadAgent:   "lead",
		Result:      &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-implement", Agent: "lead", Type: "implement", State: string(workflow.JobSucceeded), Payload: string(encoded)}, db.JobEvent{
		JobID:   "job-implement",
		Kind:    string(workflow.JobSucceeded),
		Message: "job succeeded",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-implement", Kind: "advance_retry", Message: "transient refresh failure"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, "")
	}

	if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}

	implementJob, err := store.GetJob(ctx, "job-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	implementPayload, err := daemonJobPayload(implementJob)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if implementPayload.HeadSHA != newHead {
		t.Fatalf("implement payload head = %q, want %q", implementPayload.HeadSHA, newHead)
	}
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Agent != "reviewer" || jobs[0].Type != "review" {
		t.Fatalf("queued jobs = %+v, want reviewer job", jobs)
	}
	reviewPayload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if reviewPayload.HeadSHA != newHead {
		t.Fatalf("review payload head = %q, want %q", reviewPayload.HeadSHA, newHead)
	}
}

func TestRunQueuedJobsSwallowsPostDeliveryBlockedWorkflow(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          "job-review",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 1,
		TaskID:      "task-1",
		HeadSHA:     strings.Repeat("a", 40),
	})
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: false, Reason: "ci pending"}}
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-review")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded result preserved", job.State)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
}

func TestRecoverRunningJobsRequeuesStaleRunningJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}

	now := time.Now().UTC()
	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, now, now.Add(time.Second), "", ""); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-running")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, string(workflow.JobQueued)) {
		t.Fatalf("events = %+v, want queued recovery event", events)
	}
}

func TestRecoverRunningJobsKeepsRecentRunningJobsOnStartup(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}

	now := time.Now().UTC()
	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, now, now.Add(-configuredDaemonRunningJobStaleAfter(io.Discard)), "", ""); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("job state = %q, want running", job.State)
	}
}

func TestRecoverRunningJobsUsesConfiguredStaleWindow(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GITMOOT_STALE_RUNNING_AFTER", "2m")
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)

	if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "owner/repo", "", io.Discard, time.Now().UTC().Add(3*time.Minute), nil, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
}

func TestRecoverExpiredRuntimeSessionLocksRequeuesOwnerBeforeGlobalStaleWindow(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1, JobTimeout: "10m"})
	if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	now := time.Now().UTC()
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   "runtime:codex:session-timeout",
		OwnerJobID:    "job-running",
		OwnerToken:    "token-timeout",
		OwnerPID:      int64(os.Getpid()),
		OwnerHostname: thisHostname(t),
		ExpiresAt:     now.Add(10 * time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	worker := defaultJobWorker(store, io.Discard)

	if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "owner/repo", "", io.Discard, now.Add(3*time.Minute), nil, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked before timeout returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("job state after short wait = %q, want running", job.State)
	}

	if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "owner/repo", "", io.Discard, now.Add(11*time.Minute), nil, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked after timeout returned error: %v", err)
	}
	job, err = store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob after timeout returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state after job timeout = %q, want queued", job.State)
	}
}

// TestSweepExpiredBlockedJobsCancelsOnlyOlderThanTTL pins #631: with a positive
// TTL the sweep dismisses only the blocked job aged past now-ttl (via CancelJob,
// so it lands in cancelled with a distinct blocked_ttl_expired history event),
// leaving a recently-blocked job and its history untouched.
func TestSweepExpiredBlockedJobsCancelsOnlyOlderThanTTL(t *testing.T) {
	ctx := context.Background()
	store, paths := blockedTTLTestStore(t)
	for _, id := range []string{"job-old", "job-recent"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
		if err := store.UpdateJobState(ctx, id, string(workflow.JobBlocked)); err != nil {
			t.Fatalf("UpdateJobState(%s) returned error: %v", id, err)
		}
	}
	now := time.Now().UTC()
	// job-old blocked two hours ago (past the 1h TTL); job-recent stays at ~now.
	backdateJobUpdatedAt(t, paths.Database, "job-old", now.Add(-2*time.Hour))

	if err := sweepExpiredBlockedJobs(ctx, store, time.Hour, io.Discard, now); err != nil {
		t.Fatalf("sweepExpiredBlockedJobs returned error: %v", err)
	}

	old, err := store.GetJob(ctx, "job-old")
	if err != nil {
		t.Fatalf("GetJob(job-old) returned error: %v", err)
	}
	if old.State != string(workflow.JobCancelled) {
		t.Fatalf("job-old state = %q, want cancelled", old.State)
	}
	oldEvents, err := store.ListJobEvents(ctx, "job-old")
	if err != nil {
		t.Fatalf("ListJobEvents(job-old) returned error: %v", err)
	}
	if !daemonWorkerHasEvent(oldEvents, jobEventBlockedTTLExpired) {
		t.Fatalf("job-old events = %+v, want a %s event", oldEvents, jobEventBlockedTTLExpired)
	}
	if !daemonWorkerHasEvent(oldEvents, string(workflow.JobCancelled)) {
		t.Fatalf("job-old events = %+v, want the CancelJob cancelled event too", oldEvents)
	}

	recent, err := store.GetJob(ctx, "job-recent")
	if err != nil {
		t.Fatalf("GetJob(job-recent) returned error: %v", err)
	}
	if recent.State != string(workflow.JobBlocked) {
		t.Fatalf("job-recent state = %q, want blocked (untouched)", recent.State)
	}
	recentEvents, err := store.ListJobEvents(ctx, "job-recent")
	if err != nil {
		t.Fatalf("ListJobEvents(job-recent) returned error: %v", err)
	}
	if daemonWorkerHasEvent(recentEvents, jobEventBlockedTTLExpired) {
		t.Fatalf("job-recent events = %+v, want no %s event", recentEvents, jobEventBlockedTTLExpired)
	}
}

// TestSweepExpiredBlockedJobsDisabledSweepsNothing pins the off-by-default
// contract (#631): with ttl <= 0 the sweep is an immediate no-op even for a
// long-blocked job, so a human-awaiting decision is never silently discarded.
func TestSweepExpiredBlockedJobsDisabledSweepsNothing(t *testing.T) {
	ctx := context.Background()
	store, paths := blockedTTLTestStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-blocked", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-blocked", string(workflow.JobBlocked)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	now := time.Now().UTC()
	backdateJobUpdatedAt(t, paths.Database, "job-blocked", now.Add(-30*24*time.Hour))

	if err := sweepExpiredBlockedJobs(ctx, store, 0, io.Discard, now); err != nil {
		t.Fatalf("sweepExpiredBlockedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-blocked")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked (sweep disabled)", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-blocked")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if daemonWorkerHasEvent(events, jobEventBlockedTTLExpired) {
		t.Fatalf("events = %+v, want no %s event when disabled", events, jobEventBlockedTTLExpired)
	}
}

// TestRunDaemonWorkerTickBlockedTTLSweepsAgedBlockedJob closes the config-path
// wiring gap for #631: the direct sweep tests above pass the TTL in literally, so
// they never exercise resolveBlockedTTL(worker.workflowHome()) or the tick call that
// feeds it. Here a configured [orchestrate].blocked_ttl must reach the tick's sweep
// end-to-end and cancel a blocked job aged past the window — proving the config ->
// resolve -> sweep seam (the #446/#459 home-resolution path) is wired, not just that
// sweepExpiredBlockedJobs works when handed a TTL.
func TestRunDaemonWorkerTickBlockedTTLSweepsAgedBlockedJob(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	// Opt into the sweep with a 1h window (disabled by default).
	if err := os.WriteFile(paths.ConfigFile, []byte("[orchestrate]\nblocked_ttl = \"1h\"\n"), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-blocked", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-blocked", string(workflow.JobBlocked)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	now := time.Now().UTC()
	// Aged two hours ago, past the configured 1h window.
	backdateJobUpdatedAt(t, paths.Database, "job-blocked", now.Add(-2*time.Hour))

	worker := defaultJobWorker(store, io.Discard, home)
	if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "", "", io.Discard, now, nil, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-blocked")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled (blocked_ttl sweep driven by tick)", job.State)
	}
	events, err := store.ListJobEvents(ctx, "job-blocked")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, jobEventBlockedTTLExpired) {
		t.Fatalf("events = %+v, want a %s event", events, jobEventBlockedTTLExpired)
	}
}

// TestRecoverRunningJobsHonorsLiveRuntimeLease is the #536 regression: a
// long-running job (e.g. a 4h delegation) holds a runtime-session lock whose LEASE
// reflects its real job timeout. The coarse `updated_at < before` staleness
// threshold must NOT requeue such a job while its lease is unexpired — regardless
// of the lock's owner PID. The lock records the gitmoot DAEMON's PID, not the
// spawned runtime worker's, so on a daemon restart the recorded PID is the DEAD
// prior daemon even while the reparented worker keeps running; keying recovery on
// the lease (not the PID) is what makes the restart path correct. A job whose lease
// has expired, or that holds no runtime lock at all, is still recovered.
//
// The "dead owner, unexpired lease" row is the daemon-restart scenario the recovery
// is named for: it MUST stay running (a PID-liveness gate would wrongly requeue it
// and fail the still-progressing worker — the original bug).
func TestRecoverRunningJobsHonorsLiveRuntimeLease(t *testing.T) {
	cases := []struct {
		name        string
		acquireLock bool
		ownerPID    int64
		expiresIn   time.Duration
		wantState   string
	}{
		{name: "live owner unexpired lease stays running", acquireLock: true, ownerPID: int64(os.Getpid()), expiresIn: 4 * time.Hour, wantState: string(workflow.JobRunning)},
		{name: "dead owner unexpired lease stays running (daemon restart)", acquireLock: true, ownerPID: 0, expiresIn: 4 * time.Hour, wantState: string(workflow.JobRunning)},
		{name: "live owner expired lease recovers", acquireLock: true, ownerPID: int64(os.Getpid()), expiresIn: -time.Minute, wantState: string(workflow.JobQueued)},
		{name: "dead owner expired lease recovers", acquireLock: true, ownerPID: 0, expiresIn: -time.Minute, wantState: string(workflow.JobQueued)},
		{name: "no runtime lock recovers", acquireLock: false, wantState: string(workflow.JobQueued)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := daemonWorkerStore(t)
			enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
			if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
				t.Fatalf("UpdateJobState returned error: %v", err)
			}
			now := time.Now().UTC()
			if tc.acquireLock {
				ownerPID := tc.ownerPID
				if ownerPID == 0 {
					ownerPID = deadPID(t)
				}
				acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
					ResourceKey:   "runtime:codex:session-536",
					OwnerJobID:    "job-running",
					OwnerToken:    "token-536",
					OwnerPID:      ownerPID,
					OwnerHostname: thisHostname(t),
					ExpiresAt:     now.Add(tc.expiresIn).Format(time.RFC3339Nano),
				}, now)
				if err != nil || !acquired {
					t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
				}
			}

			// before = now+time so the running job (updated_at ~ now) is past the coarse
			// staleness threshold; only the liveness gate may keep it running.
			if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, now, now.Add(time.Minute), "", ""); err != nil {
				t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
			}

			job, err := store.GetJob(ctx, "job-running")
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if job.State != tc.wantState {
				t.Fatalf("job state = %q, want %q", job.State, tc.wantState)
			}
		})
	}
}

// TestReclaimSkippedDelegationWorktrees is the #536 leak regression: when a
// terminal delegation child's worktree cleanup was SKIPPED because a foreign
// runtime owner was active (delegation_worktree_cleanup_skipped), nothing
// re-advances the terminal job, so the preserved worktree + branch would leak
// forever. The daemon's reclaim pass must re-fire the (idempotent, liveness-gated)
// cleanup: a no-op while the owner is still active, a real reclaim once it is gone.
func TestReclaimSkippedDelegationWorktrees(t *testing.T) {
	cases := []struct {
		name        string
		acquireLock bool
		expiresIn   time.Duration
		wantRemoved bool
	}{
		{name: "owner gone reclaims preserved worktree", acquireLock: false, wantRemoved: true},
		{name: "expired lease reclaims preserved worktree", acquireLock: true, expiresIn: -time.Minute, wantRemoved: true},
		{name: "active foreign owner keeps preserving", acquireLock: true, expiresIn: 4 * time.Hour, wantRemoved: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := daemonWorkerStore(t)
			branch := "gitmoot-delegation-x-d1"
			wt := t.TempDir()
			jobID := "parent/delegation/d1"
			payload := workflow.JobPayload{
				Repo: "owner/repo", DelegationID: "d1", WorktreePath: wt, Branch: branch,
				Result: &workflow.AgentResult{Decision: "failed"},
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("Marshal payload returned error: %v", err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID: jobID, Agent: "producer", Type: "implement", State: string(workflow.JobFailed),
				ParentJobID: "parent", DelegationID: "d1", Payload: string(encoded),
			}, db.JobEvent{Kind: string(workflow.JobFailed), Message: "seed"}); err != nil {
				t.Fatalf("CreateJobWithEvent returned error: %v", err)
			}
			// Prior terminal advance preserved the worktree (foreign owner was active).
			if err := store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved"}); err != nil {
				t.Fatalf("AddJobEvent returned error: %v", err)
			}
			if tc.acquireLock {
				now := time.Now().UTC()
				acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
					ResourceKey: "runtime:codex:session-536", OwnerJobID: jobID, OwnerToken: "foreign-tok",
					OwnerPID: deadPID(t), OwnerHostname: thisHostname(t),
					ExpiresAt: now.Add(tc.expiresIn).Format(time.RFC3339Nano),
				}, now)
				if err != nil || !acquired {
					t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
				}
			}

			manager := &fakeReclaimWorktreeManager{branches: map[string]bool{branch: true}}
			worker := defaultJobWorker(store, io.Discard)
			worker.WorkflowFactory = func(string) workflow.Engine {
				return workflow.Engine{
					Store:               store,
					DelegationCheckout:  t.TempDir(),
					DelegationWorktrees: manager,
					OwnerPIDLive:        func(int64) bool { return false },
				}
			}

			if err := reclaimSkippedDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(worker.Store)); err != nil {
				t.Fatalf("reclaimSkippedDelegationWorktrees returned error: %v", err)
			}

			pending, err := delegationWorktreeCleanupPendingForTest(ctx, worker.Store, jobID)
			if err != nil {
				t.Fatalf("delegationWorktreeCleanupPending returned error: %v", err)
			}
			if tc.wantRemoved {
				if len(manager.removed) != 1 || manager.removed[0] != wt {
					t.Fatalf("preserved worktree must be reclaimed: removed=%+v", manager.removed)
				}
				if pending {
					t.Fatalf("cleanup pending must clear after reclaim")
				}
			} else {
				if len(manager.removed) != 0 {
					t.Fatalf("active foreign owner must keep preserving: removed=%+v", manager.removed)
				}
				if !pending {
					t.Fatalf("cleanup must still be pending while owner active")
				}
			}
		})
	}
}

// TestReclaimSkippedDelegationWorktreesBoundedToMarkedJobs is the #549 wiring
// guard: the reclaim pass must source its candidates from the store's bounded
// pending-marker query, so a large backlog of terminal jobs WITHOUT a preserve
// marker (the ~95% steady-state majority) is never event-scanned and never
// touched — only the one genuinely-pending child is reclaimed. The fix replaced
// ListJobs + per-job ListJobEvents (O(jobs × events) every 1s tick) with
// JobIDsWithPendingDelegationWorktreeReclaim + a per-candidate GetJob.
func TestReclaimSkippedDelegationWorktreesBoundedToMarkedJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	// A large backlog of terminal jobs with rich, immutable event history but NO
	// cleanup marker. These must stay out of the candidate set entirely.
	for i := 0; i < 50; i++ {
		id := "terminal-no-marker-" + strconv.Itoa(i)
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: id, Agent: "producer", Type: "implement", State: string(workflow.JobSucceeded),
		}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent(%s) returned error: %v", id, err)
		}
		for _, kind := range []string{"queued", "running", "advance_succeeded"} {
			if err := store.AddJobEvent(ctx, db.JobEvent{JobID: id, Kind: kind, Message: "noise"}); err != nil {
				t.Fatalf("AddJobEvent returned error: %v", err)
			}
		}
	}
	// A job whose preserve was already reconciled (skip then removed): not pending.
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "reconciled", Agent: "producer", Type: "implement", State: string(workflow.JobFailed),
	}, db.JobEvent{Kind: string(workflow.JobFailed), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(reconciled) returned error: %v", err)
	}
	for _, kind := range []string{"delegation_worktree_cleanup_skipped", "delegation_worktree_removed"} {
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "reconciled", Kind: kind, Message: "m"}); err != nil {
			t.Fatalf("AddJobEvent returned error: %v", err)
		}
	}

	// The one genuinely-pending delegation child.
	branch := "gitmoot-delegation-x-d1"
	wt := t.TempDir()
	pendingID := "parent/delegation/d1"
	payload := workflow.JobPayload{
		Repo: "owner/repo", DelegationID: "d1", WorktreePath: wt, Branch: branch,
		Result: &workflow.AgentResult{Decision: "failed"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: pendingID, Agent: "producer", Type: "implement", State: string(workflow.JobFailed),
		ParentJobID: "parent", DelegationID: "d1", Payload: string(encoded),
	}, db.JobEvent{Kind: string(workflow.JobFailed), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(pending) returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: pendingID, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}

	manager := &fakeReclaimWorktreeManager{branches: map[string]bool{branch: true}}
	worker := defaultJobWorker(store, io.Discard)
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store:               store,
			DelegationCheckout:  t.TempDir(),
			DelegationWorktrees: manager,
			OwnerPIDLive:        func(int64) bool { return false },
		}
	}

	if err := reclaimSkippedDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("reclaimSkippedDelegationWorktrees returned error: %v", err)
	}

	if len(manager.removed) != 1 || manager.removed[0] != wt {
		t.Fatalf("only the marked pending worktree must be reclaimed: removed=%+v", manager.removed)
	}
	pending, err := delegationWorktreeCleanupPendingForTest(ctx, worker.Store, pendingID)
	if err != nil {
		t.Fatalf("delegationWorktreeCleanupPending returned error: %v", err)
	}
	if pending {
		t.Fatalf("cleanup pending must clear after reclaim")
	}
}

func TestRecoverCancelledRunningJobsSettlesAbandonedCancellation(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-cancelled", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-cancelled", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	if _, err := workflow.CancelJob(ctx, store, "job-cancelled"); err != nil {
		t.Fatalf("CancelJob returned error: %v", err)
	}

	if err := recoverCancelledRunningJobsForRepo(ctx, store, io.Discard, "owner/repo", ""); err != nil {
		t.Fatalf("recoverCancelledRunningJobsForRepo returned error: %v", err)
	}

	events, err := store.ListJobEvents(ctx, "job-cancelled")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "cancel_settled") {
		t.Fatalf("events = %+v, want cancel_settled", events)
	}
	if _, err := workflow.RetryJob(ctx, store, "job-cancelled"); err != nil {
		t.Fatalf("RetryJob after cancelled recovery returned error: %v", err)
	}
}

func TestRecoverCancelledRunningJobsForRepoSkipsOtherRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 1})
	for _, id := range []string{"job-a", "job-b"} {
		if err := store.UpdateJobState(ctx, id, string(workflow.JobRunning)); err != nil {
			t.Fatalf("UpdateJobState(%s) returned error: %v", id, err)
		}
		if _, err := workflow.CancelJob(ctx, store, id); err != nil {
			t.Fatalf("CancelJob(%s) returned error: %v", id, err)
		}
	}

	if err := recoverCancelledRunningJobsForRepo(ctx, store, io.Discard, "owner/repo-a", ""); err != nil {
		t.Fatalf("recoverCancelledRunningJobsForRepo returned error: %v", err)
	}

	eventsA, err := store.ListJobEvents(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListJobEvents job-a returned error: %v", err)
	}
	eventsB, err := store.ListJobEvents(ctx, "job-b")
	if err != nil {
		t.Fatalf("ListJobEvents job-b returned error: %v", err)
	}
	if !daemonWorkerHasEvent(eventsA, "cancel_settled") {
		t.Fatalf("eventsA = %+v, want cancel_settled", eventsA)
	}
	if daemonWorkerHasEvent(eventsB, "cancel_settled") {
		t.Fatalf("eventsB = %+v, want no cancel_settled", eventsB)
	}
}

func TestDaemonWorkerTickRechecksStaleRunningJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-running", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-running", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	now := time.Now().UTC().Add(defaultDaemonRunningJobStaleAfter + time.Second)

	if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "", "", io.Discard, now, nil, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
}

func TestRecoverRunningJobsForRepoSkipsOtherRepos(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 1})
	for _, id := range []string{"job-a", "job-b"} {
		if err := store.UpdateJobState(ctx, id, string(workflow.JobRunning)); err != nil {
			t.Fatalf("UpdateJobState(%s) returned error: %v", id, err)
		}
	}

	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, time.Now().UTC(), time.Now().UTC().Add(time.Second), "owner/repo-a", ""); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}

	jobA, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob job-a returned error: %v", err)
	}
	jobB, err := store.GetJob(ctx, "job-b")
	if err != nil {
		t.Fatalf("GetJob job-b returned error: %v", err)
	}
	if jobA.State != string(workflow.JobQueued) {
		t.Fatalf("job-a state = %q, want queued", jobA.State)
	}
	if jobB.State != string(workflow.JobRunning) {
		t.Fatalf("job-b state = %q, want running", jobB.State)
	}
}

// TestRunDaemonWorkerTickHonorsPerRepoMaxParallel pins the #576 daemon half: a
// [repos."owner/repo"] max_parallel=1 override caps THAT repo's in-flight
// concurrency to serial, while a repo with no override is unaffected by the same
// override file and keeps the global pool/workers parallelism. Same config
// home, same job shape — the ONLY difference is which repo the section names, so
// the peak split proves the override is what serializes repo-a.
func TestRunDaemonWorkerTickHonorsPerRepoMaxParallel(t *testing.T) {
	configHome := writeRepoConcurrencyConfigHome(t, `
[repos."owner/repo-a"]
max_parallel = 1
`)
	if peak := runRepoConcurrencyTickPeak(t, configHome, "owner/repo-a", 2, 2); peak != 1 {
		t.Fatalf("owner/repo-a peak concurrency = %d, want 1 (max_parallel=1 must serialize)", peak)
	}
	if peak := runRepoConcurrencyTickPeak(t, configHome, "owner/repo-b", 2, 2); peak != 2 {
		t.Fatalf("owner/repo-b peak concurrency = %d, want 2 (no override ⇒ global pool/workers=2 unaffected)", peak)
	}
	// Wider fan-out (3-way): with workers=3 the capped repo STILL serializes
	// (peak 1) while an unconfigured repo fans all 3 jobs out at once (peak 3).
	// The peak-3 leg proves the pool really can parallelize under this exact
	// on-disk config, so the capped repo's peak 1 is the file override doing its
	// job — not a pool that simply failed to fan out.
	t.Run("WiderFanOut", func(t *testing.T) {
		if peak := runRepoConcurrencyTickPeak(t, configHome, "owner/repo-b", 3, 3); peak != 3 {
			t.Fatalf("owner/repo-b peak concurrency = %d, want 3 (no override ⇒ global pool/workers=3 must fan out)", peak)
		}
		if peak := runRepoConcurrencyTickPeak(t, configHome, "owner/repo-a", 3, 3); peak != 1 {
			t.Fatalf("owner/repo-a peak concurrency = %d, want 1 (max_parallel=1 caps a 3-way fan-out)", peak)
		}
	})
}

// TestRunDaemonWorkerTickNoRepoConfigIsUnchanged pins behavior preservation
// (#576): with NO [repos.*] section anywhere, the per-repo tick runs at the
// global limit exactly as today — the override plumbing is inert when unset.
func TestRunDaemonWorkerTickNoRepoConfigIsUnchanged(t *testing.T) {
	configHome := writeRepoConcurrencyConfigHome(t, "")
	if peak := runRepoConcurrencyTickPeak(t, configHome, "owner/repo-a", 2, 2); peak != 2 {
		t.Fatalf("no-config peak concurrency = %d, want 2 (global pool/workers=2 unchanged)", peak)
	}
}

// TestRunQueuedJobsForRepoSkipsChildrenOfKilledRoot pins the #341 daemon half of
// the operator kill switch: once a tree's root is marked killed, a queued CHILD
// of that root is skipped by runQueuedJobsForRepo (never delivered, stays
// queued), while a queued child of an un-killed root still runs.
func TestRunQueuedJobsForRepoSkipsChildrenOfKilledRoot(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "w", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")

	killedChildPayload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", Sender: "w", RootJobID: "killed-root", DelegationID: "d1"})
	liveChildPayload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", Sender: "w", RootJobID: "live-root", DelegationID: "d2"})
	// A continuation of the killed root carries NO DelegationID and MUST still run
	// so the engine routes the tree through the graceful #305 finalize.
	killedContinuationPayload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", Sender: "w", RootJobID: "killed-root", DelegationFinalize: true})
	for _, j := range []db.Job{
		{ID: "killed-root", Agent: "w", Type: "ask", State: string(workflow.JobSucceeded), Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Sender: "w"})},
		{ID: "live-root", Agent: "w", Type: "ask", State: string(workflow.JobSucceeded), Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Sender: "w"})},
		{ID: "killed-child", Agent: "w", Type: "ask", State: string(workflow.JobQueued), Payload: killedChildPayload},
		{ID: "killed-root/continuation", Agent: "w", Type: "ask", State: string(workflow.JobQueued), Payload: killedContinuationPayload},
		{ID: "live-child", Agent: "w", Type: "ask", State: string(workflow.JobQueued), Payload: liveChildPayload},
	} {
		if err := store.CreateJobWithEvent(ctx, j, db.JobEvent{Kind: j.State, Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent(%s) returned error: %v", j.ID, err)
		}
	}

	// Operator kills only the first tree.
	if err := store.SetRootJobKilled(ctx, "killed-root"); err != nil {
		t.Fatalf("SetRootJobKilled returned error: %v", err)
	}

	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 4, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}

	for _, id := range adapter.delivered {
		if id == "killed-child" {
			t.Fatalf("a queued child of a killed root must not be delivered; delivered=%v", adapter.delivered)
		}
	}
	liveDelivered := false
	for _, id := range adapter.delivered {
		if id == "live-child" {
			liveDelivered = true
		}
	}
	if !liveDelivered {
		t.Fatalf("a queued child of an un-killed root must still run; delivered=%v", adapter.delivered)
	}
	continuationDelivered := false
	for _, id := range adapter.delivered {
		if id == "killed-root/continuation" {
			continuationDelivered = true
		}
	}
	if !continuationDelivered {
		t.Fatalf("the continuation of a killed root must run so the graceful finalize executes; delivered=%v", adapter.delivered)
	}

	killedChild, err := store.GetJob(ctx, "killed-child")
	if err != nil {
		t.Fatalf("GetJob(killed-child) returned error: %v", err)
	}
	if killedChild.State != string(workflow.JobQueued) {
		t.Fatalf("killed-child state = %q, want still queued", killedChild.State)
	}
}

func TestRunDaemonPollWithTimeoutCancelsPoll(t *testing.T) {
	start := time.Now()
	err := runDaemonPollWithTimeout(context.Background(), time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runDaemonPollWithTimeout error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("runDaemonPollWithTimeout took %v, want bounded", elapsed)
	}
}

func TestRunQueuedJobsFansOutDelegationsAndEnqueuesContinuation(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "coord", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "api", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "ui", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:     "coordinator",
		Agent:  "coord",
		Action: "ask",
		Repo:   "owner/repo",
		Branch: "task-1",
		TaskID: "task-1",
		Sender: "coord",
	})

	// Acceptance criterion #1: a coordinator returning delegations fans out one
	// child per delegation. Each subsequent reviewer approves, and the last sibling
	// to finish triggers the auto-created continuation (acceptance criterion #4).
	outputs := map[string]string{
		"coordinator": `{"gitmoot_result":{"decision":"approved","summary":"split the work","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"api","agent":"api","action":"review","prompt":"review the api"},{"id":"ui","agent":"ui","action":"review","prompt":"review the ui"}]}}`,
	}
	defaultOutput := `{"gitmoot_result":{"decision":"approved","summary":"looks good","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	adapter := &cliWorkerDelegationAdapter{outputs: outputs, defaultOutput: defaultOutput}

	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, "")
	}

	// Drain the DAG: coordinator -> two reviewer children -> continuation.
	for range 5 {
		if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
			t.Fatalf("runQueuedJobs returned error: %v", err)
		}
	}

	// Both delegation children were fanned out and succeeded.
	for _, id := range []string{"coordinator/delegation/api", "coordinator/delegation/ui"} {
		child, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("delegation child %s not created: %v", id, err)
		}
		if child.State != string(workflow.JobSucceeded) {
			t.Fatalf("child %s state = %q, want succeeded", id, child.State)
		}
		if child.ParentJobID != "coordinator" {
			t.Fatalf("child %s parent = %q, want coordinator", id, child.ParentJobID)
		}
	}

	// The continuation job was auto-created after the children finished.
	cont, err := store.GetJob(ctx, "coordinator/continuation")
	if err != nil {
		t.Fatalf("continuation job not auto-created: %v", err)
	}
	if cont.ParentJobID != "coordinator" || strings.TrimSpace(cont.DelegationID) != "" {
		t.Fatalf("continuation job = %+v, want parent=coordinator and empty delegation id", cont)
	}
	events, err := store.ListJobEvents(ctx, "coordinator")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "delegation_continuation_enqueued") {
		t.Fatalf("coordinator events = %+v, want delegation_continuation_enqueued", events)
	}
}

func TestParallelizableSerialJobsCountsDistinctRuntimeSessions(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	// Two codex agents with DISTINCT sessions -> two parallelizable slots.
	seedDaemonWorkerAgent(t, store, "a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "b", runtime.CodexRuntime, "session-b", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "a", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "b", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	worker := defaultJobWorker(store, io.Discard)
	if got, _ := parallelizableSerialJobs(ctx, worker, "owner/repo", ""); got != 2 {
		t.Fatalf("parallelizableSerialJobs = %d, want 2", got)
	}
}

func TestParallelizableSerialJobsCollapsesSameSession(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	// One codex agent (single session) -> both jobs serialize on the session lock,
	// so only ONE parallelizable slot. Raw same-repo count (2) would over-warn.
	seedDaemonWorkerAgent(t, store, "a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "a", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "a", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	worker := defaultJobWorker(store, io.Discard)
	if got, _ := parallelizableSerialJobs(ctx, worker, "owner/repo", ""); got != 1 {
		t.Fatalf("parallelizableSerialJobs = %d, want 1 (same session collapses)", got)
	}
}

func TestParallelizableSerialJobsCountsSessionlessJobsOnce(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	// ShellRuntime agents have no resumable runtime session key, so each queued
	// job is its own would-be parallel slot. The session-less branch must count
	// each such job EXACTLY ONCE (regression: it previously both incremented a
	// noSession counter AND inserted a job-ID-keyed entry, double-counting).
	seedDaemonWorkerAgent(t, store, "a", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "b", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "a", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "b", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	worker := defaultJobWorker(store, io.Discard)
	if got, _ := parallelizableSerialJobs(ctx, worker, "owner/repo", ""); got != 2 {
		t.Fatalf("parallelizableSerialJobs = %d, want 2 (two session-less jobs, counted once each)", got)
	}
}

func TestParallelizableSerialJobsMixesKeyedAndSessionlessOnce(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	// One keyed (codex session) job + one session-less (shell) job -> two distinct
	// parallel slots. The session-less job must not be double-counted alongside the
	// keyed one (regression guard: previously returned 3).
	seedDaemonWorkerAgent(t, store, "a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "b", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "a", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "b", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	worker := defaultJobWorker(store, io.Discard)
	if got, _ := parallelizableSerialJobs(ctx, worker, "owner/repo", ""); got != 2 {
		t.Fatalf("parallelizableSerialJobs = %d, want 2 (keyed + session-less, no double count)", got)
	}
}

func TestWarnSerializedParallelJobsEmitsRelaunchCommand(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "b", runtime.CodexRuntime, "session-b", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "a", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "b", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	var out bytes.Buffer
	worker := defaultJobWorker(store, &out)
	resetPreflightWarnState()
	// Serializing config (single worker) with 2 parallelizable jobs warns.
	warnSerializedParallelJobs(ctx, worker, 1, "owner/repo", "")
	got := out.String()
	if !strings.Contains(got, "will run serially") {
		t.Fatalf("warning = %q, want serialization notice", got)
	}
	if !strings.Contains(got, "--parallel 2") {
		t.Fatalf("warning = %q, want exact relaunch command with --parallel 2", got)
	}
}

func TestWarnSerializedParallelJobsRateLimitsUnchangedBacklog(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "b", runtime.CodexRuntime, "session-b", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "a", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "b", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})
	var out bytes.Buffer
	worker := defaultJobWorker(store, &out)
	resetPreflightWarnState()
	// First tick warns.
	warnSerializedParallelJobs(ctx, worker, 1, "owner/repo", "")
	if !strings.Contains(out.String(), "will run serially") {
		t.Fatalf("first tick = %q, want a warning", out.String())
	}
	// Second consecutive tick with the SAME backlog must stay quiet.
	out.Reset()
	warnSerializedParallelJobs(ctx, worker, 1, "owner/repo", "")
	if out.Len() != 0 {
		t.Fatalf("second tick re-emitted for an unchanged backlog: %q", out.String())
	}
	// A changed parallelizable set (new distinct session) re-warns.
	seedDaemonWorkerAgent(t, store, "c", runtime.CodexRuntime, "session-c", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-c", Agent: "c", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 3})
	out.Reset()
	warnSerializedParallelJobs(ctx, worker, 1, "owner/repo", "")
	if !strings.Contains(out.String(), "will run serially") {
		t.Fatalf("changed backlog = %q, want a fresh warning", out.String())
	}
}

func TestWarnSerializedParallelJobsSilentBelowTwo(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "a", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	var out bytes.Buffer
	worker := defaultJobWorker(store, &out)
	warnSerializedParallelJobs(ctx, worker, 1, "owner/repo", "")
	if out.Len() != 0 {
		t.Fatalf("warning emitted for a single parallelizable job: %q", out.String())
	}
}

// TestJobStateEligibleForWorktreeReclaim pins the #739-review fix that the
// worktree reclaim pass also disposes a CANCELLED job's dispatch-allocated
// read-only worktree, while keeping cancelled OUT of advancement re-run.
func TestJobStateEligibleForWorktreeReclaim(t *testing.T) {
	for _, s := range []string{
		string(workflow.JobSucceeded), string(workflow.JobFailed),
		string(workflow.JobBlocked), string(workflow.JobCancelled),
	} {
		if !jobStateEligibleForWorktreeReclaim(s) {
			t.Fatalf("state %q must be worktree-reclaim eligible", s)
		}
	}
	for _, s := range []string{string(workflow.JobQueued), string(workflow.JobRunning)} {
		if jobStateEligibleForWorktreeReclaim(s) {
			t.Fatalf("state %q must not be worktree-reclaim eligible", s)
		}
	}
	// A cancelled job's worktree is reclaimed, but the job must never RE-ADVANCE.
	if jobStateCanRetryAdvancement(string(workflow.JobCancelled)) {
		t.Fatal("cancelled must never be advancement-retry eligible")
	}
}

func TestSerializingConfig(t *testing.T) {
	cases := []struct {
		usePool bool
		limit   int
		want    bool
	}{
		{false, 1, true}, // barrier, single worker
		{false, 4, true}, // barrier serializes regardless of workers
		{true, 1, true},  // pool but single worker
		{true, 4, false}, // pool + multi worker: parallel-capable
	}
	for _, tc := range cases {
		if got := serializingConfig(tc.usePool, tc.limit); got != tc.want {
			t.Fatalf("serializingConfig(%t, %d) = %t, want %t", tc.usePool, tc.limit, got, tc.want)
		}
	}
}

// TestRetryPendingJobAdvancementsBoundedToCandidates proves the bounded (#598)
// advancement retry pass acts on ONLY the jobs whose latest advancement event is a
// pending marker: a large backlog of terminal jobs whose advancement already
// completed is never GetJob'd or advanced, a #602-deferred (state=queued) job with
// an unresolved advance_started is filtered by state, and a candidate id whose job
// row was pruned is skipped without aborting the tick.
func TestRetryPendingJobAdvancementsBoundedToCandidates(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")

	validPayload := func(pr int) string {
		encoded, err := json.Marshal(workflow.JobPayload{
			Repo: "owner/repo", Branch: "task-1", PullRequest: pr, TaskID: "task-1",
			Result: &workflow.AgentResult{Decision: "approved", Summary: "ok"},
		})
		if err != nil {
			t.Fatalf("Marshal payload returned error: %v", err)
		}
		return string(encoded)
	}

	// Large backlog of terminal jobs whose advancement already reconciled
	// (advance_started -> advance_completed): NOT candidates.
	for i := 0; i < 50; i++ {
		id := "adv-done-" + strconv.Itoa(i)
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: id, Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: validPayload(1000 + i),
		}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent(%s) returned error: %v", id, err)
		}
		for _, kind := range []string{"advance_started", "advance_completed", "queued"} {
			if err := store.AddJobEvent(ctx, db.JobEvent{JobID: id, Kind: kind, Message: "noise"}); err != nil {
				t.Fatalf("AddJobEvent returned error: %v", err)
			}
		}
	}

	// The one genuinely-pending advancement (latest tracked event = advance_started).
	pendingID := "adv-pending"
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: pendingID, Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: validPayload(7),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(pending) returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: pendingID, Kind: "advance_started", Message: "started"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}

	// #602: a job deferred back to queued still carries an unresolved advance_started
	// so the candidate query returns it, but the state filter rejects it (identical
	// to the old ListJobs gate).
	deferredID := "adv-deferred-queued"
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: deferredID, Agent: "reviewer", Type: "review", State: string(workflow.JobQueued), Payload: validPayload(8),
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(deferred) returned error: %v", err)
	}
	for _, kind := range []string{"advance_started", "blocker_deferred"} {
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: deferredID, Kind: kind, Message: "m"}); err != nil {
			t.Fatalf("AddJobEvent returned error: %v", err)
		}
	}

	// Pruned row: a candidate marker with no surviving jobs row.
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "adv-pruned-orphan", Kind: "advance_started", Message: "orphan"}); err != nil {
		t.Fatalf("AddJobEvent(orphan) returned error: %v", err)
	}

	var advanced []string
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(_ context.Context, job db.Job, _ workflow.JobPayload, _ runtime.Agent) (string, error) {
		advanced = append(advanced, job.ID)
		// Short-circuit before any workflow engine call; advanceJob records an
		// advance_retry event and returns nil. The spy captured the job id, which
		// is all this bounded-dispatch test asserts.
		return "", errors.New("stop after capture")
	}

	if err := retryPendingJobAdvancements(ctx, worker, "owner/repo", "", nil, newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}

	if len(advanced) != 1 || advanced[0] != pendingID {
		t.Fatalf("advanceJob reached %v, want exactly [%s]", advanced, pendingID)
	}
}

// TestRetryPendingJobCommentsBoundedToCandidates is the comment-retry analogue: the
// bounded pass re-posts ONLY the jobs whose latest comment event is
// comment_post_failed, ignoring a backlog of already-posted jobs, a #602-deferred
// queued job, and a pruned-row candidate.
func TestRetryPendingJobCommentsBoundedToCandidates(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")

	payloadFor := func(pr int) string {
		encoded, err := json.Marshal(workflow.JobPayload{
			Repo: "owner/repo", Branch: "main", PullRequest: pr,
			Result: &workflow.AgentResult{Decision: "approved", Summary: "ok"},
		})
		if err != nil {
			t.Fatalf("Marshal payload returned error: %v", err)
		}
		return string(encoded)
	}

	// Backlog of jobs whose comment already posted (failed -> posted): NOT candidates.
	for i := 0; i < 40; i++ {
		id := "cmt-done-" + strconv.Itoa(i)
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: id, Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: payloadFor(2000 + i),
		}, db.JobEvent{Kind: string(workflow.JobFailed), Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent(%s) returned error: %v", id, err)
		}
		for _, kind := range []string{"comment_post_failed", "comment_posted"} {
			if err := store.AddJobEvent(ctx, db.JobEvent{JobID: id, Kind: kind, Message: "noise"}); err != nil {
				t.Fatalf("AddJobEvent returned error: %v", err)
			}
		}
	}

	// The one genuinely-pending comment retry.
	pendingID := "cmt-pending"
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: pendingID, Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: payloadFor(9),
	}, db.JobEvent{Kind: string(workflow.JobFailed), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(pending) returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: pendingID, Kind: "comment_post_failed", Message: "temporary github error"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}

	// #602: comment_post_failed but state=queued → rejected by state filter.
	deferredID := "cmt-deferred-queued"
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: deferredID, Agent: "audit", Type: "ask", State: string(workflow.JobQueued), Payload: payloadFor(10),
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(deferred) returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: deferredID, Kind: "comment_post_failed", Message: "m"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}

	// Pruned row: candidate marker with no surviving job row.
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "cmt-pruned-orphan", Kind: "comment_post_failed", Message: "orphan"}); err != nil {
		t.Fatalf("AddJobEvent(orphan) returned error: %v", err)
	}

	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CommenterFactory = func(string) github.Client { return comments }

	if err := retryPendingJobComments(ctx, worker, "owner/repo", "", newTickCandidates(worker.Store)); err != nil {
		t.Fatalf("retryPendingJobComments returned error: %v", err)
	}

	if len(comments.posted) != 1 || comments.posted[0].issueNumber != 9 {
		t.Fatalf("posted comments = %+v, want exactly one on PR 9 (the pending candidate)", comments.posted)
	}
}

// TestRuntimeLockWaitEventDedupedPerEpisode proves the #598 dedup: repeated busy
// dispatch attempts of the same job write exactly ONE runtime_lock_wait row per wait
// episode, and closing the episode (a successful acquire) lets the next wait record
// a fresh row.
func TestRuntimeLockWaitEventDedupedPerEpisode(t *testing.T) {
	resetRuntimeLockWaitEpisodes()
	ctx := context.Background()
	store, worker, _ := queuePolicyBusyRuntimeStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})

	// Five dispatch attempts, all bounce busy → exactly ONE runtime_lock_wait row.
	for i := 0; i < 5; i++ {
		if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
			t.Fatalf("runQueuedJobs attempt %d returned error: %v", i, err)
		}
	}
	if n := countWorkerJobEvents(t, store, "job-a", "runtime_lock_wait"); n != 1 {
		t.Fatalf("runtime_lock_wait rows after 5 busy attempts = %d, want 1 (deduped per episode)", n)
	}

	// A successful acquire closes the episode; the next busy wait is a fresh episode.
	endRuntimeLockWaitEpisode("job-a")
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs after episode end returned error: %v", err)
	}
	if n := countWorkerJobEvents(t, store, "job-a", "runtime_lock_wait"); n != 2 {
		t.Fatalf("runtime_lock_wait rows after new episode = %d, want 2", n)
	}
}

// TestRuntimeLockWaitEpisodeMarkAfterWrite pins the #615-review helper contract
// directly (no store mocking): the episode is opened only by an explicit mark (which
// the call sites run ONLY after AddJobEvent succeeds), so a failed write — modeled by
// simply not calling mark — leaves the episode closed and the next bounce re-attempts
// the write. Once marked the episode dedups further bounces until its recorded emit
// ages past the TTL, at which point it re-opens as a liveness re-emit.
func TestRuntimeLockWaitEpisodeMarkAfterWrite(t *testing.T) {
	resetRuntimeLockWaitEpisodes()
	defer resetRuntimeLockWaitEpisodes()

	const jobID = "job-mark-after-write"
	if runtimeLockWaitEpisodeOpen(jobID) {
		t.Fatal("episode should be closed before any event is emitted")
	}
	// Simulate a FAILED AddJobEvent: the call site would return without marking.
	// The episode must stay closed so the next bounce retries the write.
	if runtimeLockWaitEpisodeOpen(jobID) {
		t.Fatal("episode must stay closed when the event write failed (mark not called)")
	}
	// Successful write → mark. Now the episode is open and dedups further bounces.
	markRuntimeLockWaitEpisode(jobID)
	if !runtimeLockWaitEpisodeOpen(jobID) {
		t.Fatal("episode should be open after a successful write is marked")
	}
	// Age the recorded emit past the TTL: the episode re-opens (liveness re-emit).
	runtimeLockWaitMu.Lock()
	runtimeLockWaitEpisodes[jobID] = time.Now().Add(-2 * runtimeLockWaitEpisodeTTL)
	runtimeLockWaitMu.Unlock()
	if runtimeLockWaitEpisodeOpen(jobID) {
		t.Fatal("episode should re-open once the recorded emit is older than the TTL")
	}
}

// TestRuntimeLockWaitEpisodePrunesExpired proves the #615-review map bound: a job that
// hits contention and then terminates WITHOUT ever acquiring (so
// endRuntimeLockWaitEpisode never clears its entry) leaves a stale timestamp behind,
// and mark's over-cap sweep drops it once it is older than the TTL — so such jobs can
// no longer grow the episode map unboundedly.
func TestRuntimeLockWaitEpisodePrunesExpired(t *testing.T) {
	resetRuntimeLockWaitEpisodes()
	defer resetRuntimeLockWaitEpisodes()

	runtimeLockWaitMu.Lock()
	// The terminal-without-acquire leftover, emitted long ago.
	runtimeLockWaitEpisodes["terminal-no-acquire"] = time.Now().Add(-2 * runtimeLockWaitEpisodeTTL)
	// Fill up to the cap with fresh entries so the next mark pushes len over the
	// threshold and triggers the expiry sweep.
	for i := 0; i < runtimeLockWaitEpisodeMax; i++ {
		runtimeLockWaitEpisodes[fmt.Sprintf("fresh-%d", i)] = time.Now()
	}
	runtimeLockWaitMu.Unlock()

	// This mark takes len past runtimeLockWaitEpisodeMax, triggering the sweep.
	markRuntimeLockWaitEpisode("trigger")

	runtimeLockWaitMu.Lock()
	_, stale := runtimeLockWaitEpisodes["terminal-no-acquire"]
	_, fresh := runtimeLockWaitEpisodes["fresh-0"]
	runtimeLockWaitMu.Unlock()
	if stale {
		t.Fatal("expected the expired terminal-without-acquire entry to be pruned")
	}
	if !fresh {
		t.Fatal("a still-fresh entry must NOT be pruned (only entries older than the TTL)")
	}
}

// TestPoolDoesNotRespinRuntimeBusyJob proves the #598 spin-stop: a queued job whose
// runtime session is externally locked bounces busy, and the pool dispatcher must
// dispatch it at most once per invocation and then RETURN, instead of re-selecting
// it every pass in a tight spin. Mutation proof: dropping the excludeBouncedBusy
// call makes the attempt count unbounded (the goroutine below never returns and the
// wall-clock guard trips).
func TestPoolDoesNotRespinRuntimeBusyJob(t *testing.T) {
	resetRuntimeLockWaitEpisodes()
	ctx := context.Background()
	store, worker, attempts := queuePolicyBusyRuntimeStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})

	errCh := make(chan error, 1)
	go func() {
		errCh <- runQueuedJobsForRepoPoolTracked(ctx, worker, 1, 1, "owner/repo", "", nil)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runQueuedJobsForRepoPoolTracked returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("pool did not return within 15s (busy job re-spinning); attempts so far = %d", atomic.LoadInt64(attempts))
	}
	if got := atomic.LoadInt64(attempts); got != 1 {
		t.Fatalf("dispatch attempts = %d, want exactly 1 (busy job re-selected)", got)
	}
	if n := countWorkerJobEvents(t, store, "job-a", "runtime_lock_wait"); n != 1 {
		t.Fatalf("runtime_lock_wait rows = %d, want 1", n)
	}
	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued (held for a later tick)", job.State)
	}
}

// TestSameSessionSiblingsHeldBackOnBusy proves the #598 runtime-key holdback: two
// queued jobs share a runtime session; once the first bounces busy, its sibling
// (same session key) must NOT be dispatched in the same invocation. With only the
// job-id set (no bouncedBusyRuntimes) the sibling would be dispatched after the
// first is reaped, giving 2 attempts; the runtime-key holdback keeps it at 1.
func TestSameSessionSiblingsHeldBackOnBusy(t *testing.T) {
	resetRuntimeLockWaitEpisodes()
	ctx := context.Background()
	store, worker, attempts := queuePolicyBusyRuntimeStore(t)
	// Both jobs use the same audit agent ⇒ the same runtime:codex:session-1 key.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})

	errCh := make(chan error, 1)
	go func() {
		errCh <- runQueuedJobsForRepoPoolTracked(ctx, worker, 2, 2, "owner/repo", "", nil)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runQueuedJobsForRepoPoolTracked returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("pool did not return within 15s; attempts so far = %d", atomic.LoadInt64(attempts))
	}
	if got := atomic.LoadInt64(attempts); got != 1 {
		t.Fatalf("dispatch attempts = %d, want exactly 1 (same-session sibling must be held back)", got)
	}
}

// TestTickCandidatesComputedOncePerTick pins the #619 hoist: the three repo-agnostic
// candidate GROUP BY queries must run ONCE per supervisor tick, not once per enabled
// repo. It substitutes a carrier over a shared counter for every newTickCandidates
// call the tick makes; a regression that rebuilds the carrier per repo fires the
// constructor (and thus the shared counter) once per repo and trips the asserts.
func TestTickCandidatesComputedOncePerTick(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	const repoCount = 3
	for i := 0; i < repoCount; i++ {
		seedDaemonWorkerRepo(t, store, fmt.Sprintf("owner/repo%d", i), t.TempDir())
	}
	worker := defaultJobWorker(store, io.Discard)

	counter := &countingCandidateStore{inner: store}
	realNewTickCandidates := newTickCandidates
	newTickCandidates = func(tickCandidateStore) *tickCandidates {
		return realNewTickCandidates(counter)
	}
	defer func() { newTickCandidates = realNewTickCandidates }()

	reset := func() {
		atomic.StoreInt32(&counter.advance, 0)
		atomic.StoreInt32(&counter.comment, 0)
		atomic.StoreInt32(&counter.reclaim, 0)
	}
	now := time.Now().UTC()

	// Multi-repo sweep: each candidate query runs exactly once for the whole sweep.
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, now, nil, nil); err != nil {
		t.Fatalf("runEnabledRepoWorkerTicksTracked returned error: %v", err)
	}
	if got := atomic.LoadInt32(&counter.advance); got != 1 {
		t.Fatalf("advance-retry query ran %d times across %d repos, want 1", got, repoCount)
	}
	if got := atomic.LoadInt32(&counter.comment); got != 1 {
		t.Fatalf("comment-retry query ran %d times across %d repos, want 1", got, repoCount)
	}
	if got := atomic.LoadInt32(&counter.reclaim); got != 1 {
		t.Fatalf("delegation-reclaim query ran %d times across %d repos, want 1", got, repoCount)
	}

	// Single-repo tick with a nil carrier self-computes each query at most once.
	reset()
	if err := runDaemonWorkerTickTracked(ctx, store, worker, 1, false, "owner/repo0", "", io.Discard, now, nil, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked returned error: %v", err)
	}
	if got := atomic.LoadInt32(&counter.advance); got != 1 {
		t.Fatalf("single-repo advance-retry query ran %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&counter.comment); got != 1 {
		t.Fatalf("single-repo comment-retry query ran %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&counter.reclaim); got != 1 {
		t.Fatalf("single-repo delegation-reclaim query ran %d times, want 1", got)
	}

	// A dry-run tick returns before computing any candidate set.
	reset()
	if err := runDaemonWorkerTickTracked(ctx, store, worker, 1, true, "owner/repo0", "", io.Discard, now, nil, nil); err != nil {
		t.Fatalf("dry-run runDaemonWorkerTickTracked returned error: %v", err)
	}
	if got := atomic.LoadInt32(&counter.advance) + atomic.LoadInt32(&counter.comment) + atomic.LoadInt32(&counter.reclaim); got != 0 {
		t.Fatalf("dry-run ran %d candidate queries, want 0", got)
	}
}

// TestTickCandidatesRetriesOnError pins FIX-A (#620 review): the per-tick carrier
// memoizes SUCCESSES only. The first access to each query returns the store's
// transient error WITHOUT memoizing it; the second access re-runs the query and
// returns the now-successful result. A regression that memoized the error (the old
// done=true-on-error behavior) would replay the first error on every later access
// and never re-query — exactly the failure that turned one SQLITE_BUSY into a
// whole-sweep error and fed the daemon self-exit streak.
func TestTickCandidatesRetriesOnError(t *testing.T) {
	ctx := context.Background()
	store := &flakyCandidateStore{}
	cand := newTickCandidates(store)

	// advance-retry: first access errors and is not memoized; second retries and wins.
	if _, err := cand.advanceRetryCandidates(ctx); !errors.Is(err, errCandidateTransient) {
		t.Fatalf("first advanceRetryCandidates err = %v, want transient", err)
	}
	ids, err := cand.advanceRetryCandidates(ctx)
	if err != nil {
		t.Fatalf("second advanceRetryCandidates err = %v, want nil (retry succeeds)", err)
	}
	if len(ids) != 1 || ids[0] != "advance-job" {
		t.Fatalf("second advanceRetryCandidates ids = %v, want [advance-job]", ids)
	}
	if got := atomic.LoadInt32(&store.advanceCalls); got != 2 {
		t.Fatalf("advance query ran %d times, want 2 (error not memoized ⇒ retried)", got)
	}
	// A third access reuses the memoized success — no further store call.
	if _, err := cand.advanceRetryCandidates(ctx); err != nil {
		t.Fatalf("third advanceRetryCandidates err = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&store.advanceCalls); got != 2 {
		t.Fatalf("advance query ran %d times after a cached success, want still 2", got)
	}

	// The same retry-on-error / memoize-on-success contract holds for the other two.
	if _, err := cand.commentRetryCandidates(ctx); !errors.Is(err, errCandidateTransient) {
		t.Fatalf("first commentRetryCandidates err = %v, want transient", err)
	}
	if ids, err := cand.commentRetryCandidates(ctx); err != nil || len(ids) != 1 || ids[0] != "comment-job" {
		t.Fatalf("second commentRetryCandidates ids=%v err=%v, want [comment-job] nil", ids, err)
	}
	if got := atomic.LoadInt32(&store.commentCalls); got != 2 {
		t.Fatalf("comment query ran %d times, want 2", got)
	}
	if _, err := cand.delegationReclaimCandidates(ctx); !errors.Is(err, errCandidateTransient) {
		t.Fatalf("first delegationReclaimCandidates err = %v, want transient", err)
	}
	if ids, err := cand.delegationReclaimCandidates(ctx); err != nil || len(ids) != 1 || ids[0] != "reclaim-job" {
		t.Fatalf("second delegationReclaimCandidates ids=%v err=%v, want [reclaim-job] nil", ids, err)
	}
	if got := atomic.LoadInt32(&store.reclaimCalls); got != 2 {
		t.Fatalf("reclaim query ran %d times, want 2", got)
	}
}
