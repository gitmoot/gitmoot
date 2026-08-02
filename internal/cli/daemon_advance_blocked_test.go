package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestBlockedAdvanceSettlesQueriedJobState(t *testing.T) {
	job, _, events := runNonFastForwardBlockedAdvance(t)
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("queried job state = %q, want blocked", job.State)
	}
	if !daemonWorkerHasEvent(events, "advance_blocked") {
		t.Fatalf("events = %+v, want advance_blocked", events)
	}
}

func TestBlockedAdvancePostsBlockedDecision(t *testing.T) {
	_, body, _ := runNonFastForwardBlockedAdvance(t)
	if !strings.Contains(body, "**Decision:** `blocked`") {
		t.Fatalf("blocked advancement comment did not report blocked:\n%s", body)
	}
	if strings.Contains(body, "**Decision:** `implemented`") {
		t.Fatalf("blocked advancement comment claimed implementation:\n%s", body)
	}
	if !strings.Contains(body, "**Diagnostics:**") || !strings.Contains(body, "push implementation branch failed") {
		t.Fatalf("blocked advancement comment omitted push diagnostics:\n%s", body)
	}
}

func TestRetryBlockedAdvanceSettlesQueriedJobState(t *testing.T) {
	job, _, events := runRetryBlockedAdvance(t)
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("queried retry job state = %q, want blocked", job.State)
	}
	if !daemonWorkerHasEvent(events, "advance_blocked") {
		t.Fatalf("retry events = %+v, want advance_blocked", events)
	}
}

func TestRetryBlockedAdvancePostsBlockedDecision(t *testing.T) {
	_, body, _ := runRetryBlockedAdvance(t)
	if !strings.Contains(body, "**Decision:** `blocked`") {
		t.Fatalf("retry-blocked advancement comment did not report blocked:\n%s", body)
	}
	if strings.Contains(body, "**Decision:** `approved`") {
		t.Fatalf("retry-blocked advancement comment repeated the stored approval:\n%s", body)
	}
}

func TestBlockedAdvanceDoesNotAbortQueuedJobPass(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-blocked", RepoFullName: "owner/repo", Title: "Blocked", State: string(workflow.TaskImplementing), Branch: "task-blocked", WorktreePath: t.TempDir()}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "a-blocked", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-blocked", TaskID: "task-blocked"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "b-healthy", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})

	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(agent runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		decision := "approved"
		if agent.Name == "lead" {
			decision = "implemented"
		}
		return &cliWorkerFakeAdapter{output: resultJSON(decision)}, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, ImplementationFinalizer: selectiveBlockedFinalizer{blockedJobID: "a-blocked"}}
	}

	runErr := runQueuedJobsForRepo(ctx, worker, 1, "", "")
	blocked, err := store.GetJob(ctx, "a-blocked")
	if err != nil {
		t.Fatalf("GetJob(a-blocked) returned error: %v", err)
	}
	healthy, err := store.GetJob(ctx, "b-healthy")
	if err != nil {
		t.Fatalf("GetJob(b-healthy) returned error: %v", err)
	}
	if blocked.State != string(workflow.JobBlocked) {
		t.Fatalf("blocked job state = %q, want blocked", blocked.State)
	}
	if healthy.State != string(workflow.JobSucceeded) {
		t.Fatalf("other job state = %q, want succeeded in the same pass (pass error: %v)", healthy.State, runErr)
	}
	if runErr != nil {
		t.Fatalf("runQueuedJobsForRepo returned error after processing other jobs: %v", runErr)
	}
}

func TestBlockedAdvanceIsNotRetried(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload, err := json.Marshal(workflow.JobPayload{Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "blocked-no-retry", Agent: "lead", Type: "implement", State: string(workflow.JobBlocked), Payload: string(payload)}, db.JobEvent{
		JobID: "blocked-no-retry", Kind: "advance_blocked", Message: "push rejected",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	needsRetry, err := worker.jobNeedsAdvanceRetry(ctx, "blocked-no-retry")
	if err != nil {
		t.Fatalf("jobNeedsAdvanceRetry returned error: %v", err)
	}
	if needsRetry {
		t.Fatal("advance_blocked was classified for retry")
	}
}

type selectiveBlockedFinalizer struct {
	blockedJobID string
}

func runRetryBlockedAdvance(t *testing.T) (db.Job, string, []db.JobEvent) {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-review", RepoFullName: "owner/repo", Title: "Review", State: string(workflow.TaskReviewing), Branch: "task-review"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", Branch: "task-review", PullRequest: 9, TaskID: "task-review", HeadSHA: strings.Repeat("a", 40),
		Result: &workflow.AgentResult{Decision: "approved", Summary: "review approved"},
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-retry-blocked", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: string(payload)}, db.JobEvent{
		JobID: "job-retry-blocked", Kind: string(workflow.JobSucceeded), Message: "review result stored",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-retry-blocked", Kind: "advance_retry", Message: "retry advancement"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: &cliWorkerFakeMergeGate{err: workflow.BlockedError{Reason: "review advancement blocked"}}}
	}
	worker.CommenterFactory = func(string) github.Client { return comments }
	if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(store)); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}
	if err := worker.postJobResultComment(ctx, "job-retry-blocked", runtime.Agent{Name: "reviewer", Runtime: runtime.ShellRuntime}, checkout, nil); err != nil {
		t.Fatalf("postJobResultComment returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-retry-blocked")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want one", comments.posted)
	}
	return job, comments.posted[0].body, events
}

func (f selectiveBlockedFinalizer) FinalizeImplementation(_ context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
	if job.ID == f.blockedJobID {
		return payload, workflow.BlockedError{Reason: "push implementation branch failed: non-fast-forward"}
	}
	return payload, nil
}

func runNonFastForwardBlockedAdvance(t *testing.T) (db.Job, string, []db.JobEvent) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	remote := filepath.Join(home, "remote.git")
	checkout := filepath.Join(home, "checkout")
	peer := filepath.Join(home, "peer")
	runGit(t, home, "init", "--bare", remote)
	runGit(t, home, "clone", remote, checkout)
	configureTestGit(t, checkout)
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base README: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "base")
	runGit(t, checkout, "branch", "-M", "main")
	runGit(t, checkout, "push", "-u", "origin", "main")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, checkout, "switch", "-c", "task-1")
	runGit(t, checkout, "push", "-u", "origin", "task-1")
	runGit(t, home, "clone", remote, peer)
	configureTestGit(t, peer)
	runGit(t, peer, "switch", "task-1")
	if err := os.WriteFile(filepath.Join(peer, "remote.txt"), []byte("remote advance\n"), 0o644); err != nil {
		t.Fatalf("write peer change: %v", err)
	}
	runGit(t, peer, "add", "remote.txt")
	runGit(t, peer, "commit", "-m", "advance remote")
	runGit(t, peer, "push", "origin", "task-1")
	remoteHead := strings.TrimSpace(runGitOutput(t, peer, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(checkout, "local.txt"), []byte("local implementation\n"), 0o644); err != nil {
		t.Fatalf("write local change: %v", err)
	}

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: checkout}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-non-fast-forward", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, TaskID: "task-1", TaskTitle: "Task 1"})
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{output: resultJSON("implemented")}, nil
	}
	worker.CommenterFactory = func(string) github.Client { return comments }
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-non-fast-forward")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want one", comments.posted)
	}
	remoteAfter := strings.TrimSpace(runGitOutput(t, checkout, "ls-remote", "origin", "refs/heads/task-1"))
	if !strings.HasPrefix(remoteAfter, remoteHead) {
		t.Fatalf("remote task branch changed after rejected push: got %q, want head %s", remoteAfter, remoteHead)
	}
	return job, comments.posted[0].body, events
}

func configureTestGit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot Test")
}

func resultJSON(decision string) string {
	return `{"gitmoot_result":{"decision":"` + decision + `","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
}
