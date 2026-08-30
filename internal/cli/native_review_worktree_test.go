package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestNativeReviewWorkerRunsInOwnedExactHeadWorktree(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	sharedCheckout, reviewHead, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", sharedCheckout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime, runtime.LastRef, []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	fanout := daemonWorkflowEngine(store, github.NoopClient{}, sharedCheckout, home)
	fanout.RequireWorkflowPolicy = func(string) workflow.RequireWorkflowPolicy {
		return workflow.RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
	}
	fanout.NativeReviewFanoutEnabled = func(string) bool { return true }
	if err := fanout.HandlePullRequestOpened(ctx, workflow.PullRequestEvent{
		Repo:              "owner/repo",
		Branch:            "feature/review",
		PullRequest:       1698,
		HeadSHA:           reviewHead,
		TaskID:            "task-1698",
		TaskTitle:         "Fix native review checkout",
		LeadAgent:         "implementer",
		Sender:            "github",
		RequiredReviewers: []string{"reviewer"},
	}); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("native review jobs = %d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.Type != "review" || !strings.HasPrefix(job.ID, "workflow-") {
		t.Fatalf("scheduled job is not a native workflow review: %+v", job)
	}

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"exact head reviewed","findings":[],"changes_made":[],"tests_run":["exact-head smoke"],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard, home)
	var deliveredCheckout, deliveredHead string
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		deliveredCheckout = checkout
		deliveredHead = readonlyWorktreeHead(t, checkout)
		return adapter, nil
	}
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: true}}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		engine := daemonWorkflowEngine(store, github.NoopClient{}, checkout, worker.workflowHome())
		engine.MergeGate = gate
		return engine
	}

	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	reloaded, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob after run: %v", err)
	}
	if reloaded.State != string(workflow.JobSucceeded) || adapter.calls != 1 {
		t.Fatalf("native review state=%q deliveries=%d, want succeeded with one delivery", reloaded.State, adapter.calls)
	}
	payload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}
	if !payload.ReadOnlyWorktree || payload.WorktreePath == "" {
		t.Fatalf("native review payload has no owned read-only worktree: %+v", payload)
	}
	if deliveredCheckout == sharedCheckout {
		t.Fatalf("native review delivered from shared checkout %q", deliveredCheckout)
	}
	if deliveredHead != reviewHead {
		t.Fatalf("delivered checkout head = %q, want review head %q", deliveredHead, reviewHead)
	}
	if _, err := os.Stat(payload.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("terminal native review worktree still exists: path=%q err=%v", payload.WorktreePath, err)
	}
}

func TestNativeReviewWorkerCleansOwnedWorktreeWhenAdapterPreflightFails(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	sharedCheckout, reviewHead, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", sharedCheckout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime, runtime.LastRef, []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	fanout := daemonWorkflowEngine(store, github.NoopClient{}, sharedCheckout, home)
	fanout.RequireWorkflowPolicy = func(string) workflow.RequireWorkflowPolicy {
		return workflow.RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
	}
	fanout.NativeReviewFanoutEnabled = func(string) bool { return true }
	if err := fanout.HandlePullRequestOpened(ctx, workflow.PullRequestEvent{
		Repo: "owner/repo", Branch: "feature/review", PullRequest: 1698, HeadSHA: reviewHead,
		TaskID: "task-1698", TaskTitle: "Clean failed native review checkout", LeadAgent: "implementer",
		Sender: "github", RequiredReviewers: []string{"reviewer"},
	}); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs = %d jobs, err=%v", len(jobs), err)
	}
	job := jobs[0]
	worker := defaultJobWorker(store, io.Discard, home)
	var preparedPath string
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		preparedPath = checkout
		return nil, errors.New("adapter preflight failed")
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, worker.workflowHome())
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	reloaded, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if reloaded.State != string(workflow.JobFailed) {
		t.Fatalf("native review state = %q, want failed", reloaded.State)
	}
	payload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}
	if preparedPath == "" || payload.WorktreePath != preparedPath || !payload.ReadOnlyWorktree {
		t.Fatalf("prepared worktree = %q payload=%+v", preparedPath, payload)
	}
	if _, err := os.Stat(preparedPath); !os.IsNotExist(err) {
		t.Fatalf("failed native review worktree still exists: path=%q err=%v", preparedPath, err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	removed := false
	for _, event := range events {
		if event.Kind == "delegation_worktree_removed" {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("native review cleanup events = %+v, want delegation_worktree_removed", events)
	}
}

func TestNativeReviewWorktreePreparationCoversHighRiskLensChild(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	sharedCheckout, reviewHead, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", sharedCheckout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime, runtime.LastRef, []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	const jobID = "review-coordinator/task-1698/review-1/delegation/correctness"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:              jobID,
		Agent:           "reviewer",
		Action:          "review",
		Repo:            "owner/repo",
		Branch:          "feature/review",
		PullRequest:     1698,
		HeadSHA:         reviewHead,
		TaskID:          "task-1698",
		TaskTitle:       "Fix high-risk review checkout",
		LeadAgent:       "implementer",
		Reviewers:       []string{"reviewer"},
		ReviewRound:     "review-1",
		ParentJobID:     "review-coordinator/task-1698/review-1",
		DelegationID:    "correctness",
		DelegationDepth: 1,
		Instructions:    "Review the correctness lens at the exact head.",
	})
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	prepared, err := worker.prepareNativeReviewWorktreeForRunner(ctx, job, payload, subprocess.ExecRunner{})
	if err != nil {
		t.Fatalf("prepareNativeReviewWorktreeForRunner: %v", err)
	}
	t.Cleanup(func() {
		_ = gitutil.NewHostClient(sharedCheckout).RemoveWorktreeForce(context.Background(), prepared.WorktreePath)
	})
	if !prepared.ReadOnlyWorktree || prepared.WorktreePath == "" {
		t.Fatalf("high-risk lens has no owned worktree: %+v", prepared)
	}
	if got := readonlyWorktreeHead(t, prepared.WorktreePath); got != reviewHead {
		t.Fatalf("high-risk lens worktree head = %q, want %q", got, reviewHead)
	}
	reloaded, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob after preparation: %v", err)
	}
	persisted, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload after preparation: %v", err)
	}
	if persisted.WorktreePath != prepared.WorktreePath || !persisted.ReadOnlyWorktree {
		t.Fatalf("persisted high-risk lens worktree = %+v, want %+v", persisted, prepared)
	}
}
