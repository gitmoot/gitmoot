package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
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
	// The engine's home MUST be the RESOLVED root (config.Paths.Home), which is what
	// production hands daemonWorkflowEngine and what the worker's cleanup validator
	// derives its managed path from. Passing the raw --home here was invisible while
	// the engine allocated nothing; now that it allocates before enqueue, the raw
	// home would place the worktree outside the root the validator accepts.
	fanout := routineNativeReviewFanoutEngine(store, sharedCheckout, home)
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
	fanout := routineNativeReviewFanoutEngine(store, sharedCheckout, home)
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

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"correctness lens reviewed","findings":[],"changes_made":[],"tests_run":["lens smoke"],"needs":[],"delegations":[]}}`}
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

	runErr := worker.run(ctx, job)
	if adapter.calls != 1 {
		t.Fatalf("high-risk lens deliveries = %d, want 1 (worker.run err=%v)", adapter.calls, runErr)
	}
	// The delivered checkout is asserted before runErr: a routing change that stops
	// reaching prepareNativeReviewWorktreeForRunner for a lens child delivers from the
	// shared checkout, and that is the defect worth naming, whatever the leg then does.
	if deliveredCheckout == sharedCheckout {
		t.Fatalf("high-risk lens delivered from shared checkout %q (worker.run err=%v)", deliveredCheckout, runErr)
	}
	if deliveredHead != reviewHead {
		t.Fatalf("high-risk lens delivered checkout head = %q, want review head %q", deliveredHead, reviewHead)
	}
	if runErr != nil {
		t.Fatalf("worker.run: %v", runErr)
	}
	reloaded, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob after run: %v", err)
	}
	persisted, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload after run: %v", err)
	}
	t.Cleanup(func() {
		if persisted.WorktreePath != "" {
			_ = gitutil.NewHostClient(sharedCheckout).RemoveWorktreeForce(context.Background(), persisted.WorktreePath)
		}
	})
	if !persisted.ReadOnlyWorktree || persisted.WorktreePath != deliveredCheckout {
		t.Fatalf("high-risk lens payload = %+v, want owned read-only worktree %q", persisted, deliveredCheckout)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	allocated := false
	for _, event := range events {
		if event.Kind == "review_worktree_allocated_exact_head" {
			allocated = true
		}
	}
	if !allocated {
		t.Fatalf("high-risk lens events = %+v, want review_worktree_allocated_exact_head", events)
	}
}

// routineNativeReviewFanoutEngine builds the daemon's REAL workflow engine for the
// registered checkout, with the native fan-out on. It is the production wiring:
// Home + DelegationCheckout + DelegationWorktrees are all set, which is the
// configuration in which the engine (not the worker) allocates.
// home is the RAW --home; it is resolved to config.Paths.Home here because that is
// the value production passes (daemon_supervision.go's resolvedRoot) and the value
// jobWorker.workflowHome() returns, so the engine's pre-enqueue allocation and the
// worker's cleanup validator agree on one managed worktree root.
func routineNativeReviewFanoutEngine(store *db.Store, checkout string, home string) workflow.Engine {
	fanout := daemonWorkflowEngine(store, github.NoopClient{}, checkout, config.PathsForHome(home).Home)
	fanout.RequireWorkflowPolicy = func(string) workflow.RequireWorkflowPolicy {
		return workflow.RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
	}
	fanout.NativeReviewFanoutEnabled = func(string) bool { return true }
	return fanout
}

func cleanupNativeReviewWorktrees(t *testing.T, checkout string, paths ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, path := range paths {
			if strings.TrimSpace(path) != "" {
				_ = gitutil.NewHostClient(checkout).RemoveWorktreeForce(context.Background(), path)
			}
		}
	})
}

// TestRoutineNativeReviewLegsGetDistinctSchedulerCheckoutKeys is F2. Two reviewer
// legs for ONE pull request used to be admitted on the same repo:<repo> checkout
// key, because queuedJobCheckoutKey reads payload.WorktreePath and the routine leg
// carried none until the worker allocated one AFTER admission. Scheduler
// exclusivity is strict (`if s.checkouts[checkoutKey] { return false }`), so only
// one leg ran per tick and it held that key for a full LLM review.
//
// MUTATION PROOF: make prepareNativeReviewWorktree return its request untouched
// and both keys collapse to "repo:owner/repo".
func TestRoutineNativeReviewLegsGetDistinctSchedulerCheckoutKeys(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	sharedCheckout, reviewHead, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", sharedCheckout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer-a", runtime.ClaudeRuntime, runtime.LastRef, []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer-b", runtime.ClaudeRuntime, runtime.LastRef, []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	fanout := routineNativeReviewFanoutEngine(store, sharedCheckout, home)
	if err := fanout.HandlePullRequestOpened(ctx, workflow.PullRequestEvent{
		Repo:              "owner/repo",
		Branch:            "feature/review",
		PullRequest:       1698,
		HeadSHA:           reviewHead,
		TaskID:            "task-1698",
		TaskTitle:         "Parallel native reviewers",
		LeadAgent:         "implementer",
		Sender:            "github",
		RequiredReviewers: []string{"reviewer-a", "reviewer-b"},
	}); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	reviewJobs := make([]db.Job, 0, 2)
	for _, job := range jobs {
		if job.Type == "review" {
			reviewJobs = append(reviewJobs, job)
		}
	}
	if len(reviewJobs) != 2 {
		t.Fatalf("review jobs = %d, want one per reviewer; jobs=%+v", len(reviewJobs), jobs)
	}

	keys := make(map[string]string, 2)
	worker := defaultJobWorker(store, io.Discard, home)
	for _, job := range reviewJobs {
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("daemonJobPayload %s: %v", job.ID, err)
		}
		cleanupNativeReviewWorktrees(t, sharedCheckout, payload.WorktreePath)
		if strings.TrimSpace(payload.WorktreePath) == "" || !payload.ReadOnlyWorktree {
			t.Fatalf("leg %s payload = %+v, want to be BORN with its exact-head read-only worktree", job.ID, payload)
		}
		if head := readonlyWorktreeHead(t, payload.WorktreePath); head != reviewHead {
			t.Fatalf("leg %s worktree head = %q, want the review head %q", job.ID, head, reviewHead)
		}
		key := queuedJobCheckoutKey(ctx, store, job)
		if !strings.HasPrefix(key, "worktree:") {
			t.Fatalf("leg %s checkout key = %q, want worktree:<path> (it would serialize on the shared repo key)", job.ID, key)
		}
		keys[job.ID] = key
		// The SAME configuration must not also allocate in the worker: the helper's
		// gate has to return this payload untouched, or there would be two live
		// allocation paths for one engine.
		unchanged, err := worker.prepareNativeReviewWorktreeForRunner(ctx, job, payload, subprocess.ExecRunner{})
		if err != nil {
			t.Fatalf("prepareNativeReviewWorktreeForRunner %s: %v", job.ID, err)
		}
		if unchanged.WorktreePath != payload.WorktreePath || unchanged.Instructions != payload.Instructions {
			t.Fatalf("worker re-allocated a pre-allocated leg %s: %+v", job.ID, unchanged)
		}
	}
	if keys[reviewJobs[0].ID] == keys[reviewJobs[1].ID] {
		t.Fatalf("both reviewer legs share checkout key %q — they would run one per tick", keys[reviewJobs[0].ID])
	}
}

// TestRoutineNativeReviewRepollAtSameHeadIsIdempotentThroughTheWorker is F1 on the
// production path. The failure window is a re-poll while the leg is still queued
// after the worker prepared it: the round is stable at one head, so the
// deterministic job id is re-derived identically, and when the WORKER had mutated
// Instructions + WorktreePath after enqueue, existingJobMatchesRequest returned
// false and Engine.enqueue surfaced the raw "UNIQUE constraint failed: jobs.id".
//
// MUTATION PROOF: make prepareNativeReviewWorktree return its request untouched
// (restoring worker-allocates-after-enqueue) and the second
// HandlePullRequestOpened fails with exactly that SQL error.
func TestRoutineNativeReviewRepollAtSameHeadIsIdempotentThroughTheWorker(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	sharedCheckout, reviewHead, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", sharedCheckout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime, runtime.LastRef, []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	fanout := routineNativeReviewFanoutEngine(store, sharedCheckout, home)
	event := workflow.PullRequestEvent{
		Repo:              "owner/repo",
		Branch:            "feature/review",
		PullRequest:       1698,
		HeadSHA:           reviewHead,
		TaskID:            "task-1698",
		TaskTitle:         "Re-poll at one head",
		LeadAgent:         "implementer",
		Sender:            "github",
		RequiredReviewers: []string{"reviewer"},
	}
	if err := fanout.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("first HandlePullRequestOpened: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs = %d jobs, err=%v", len(jobs), err)
	}
	job := jobs[0]
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}
	cleanupNativeReviewWorktrees(t, sharedCheckout, payload.WorktreePath)
	// Run the REAL worker preparation the leg would hit on dispatch. Whatever it
	// does to the payload is what the next poll compares against.
	worker := defaultJobWorker(store, io.Discard, home)
	if _, err := worker.prepareNativeReviewWorktreeForRunner(ctx, job, payload, subprocess.ExecRunner{}); err != nil {
		t.Fatalf("prepareNativeReviewWorktreeForRunner: %v", err)
	}

	if err := fanout.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("second HandlePullRequestOpened at the same head: %v", err)
	}
	after, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs after re-poll: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("jobs after re-poll = %d, want the same single leg; jobs=%+v", len(after), after)
	}
}

// TestNativeReviewWorktreeContentionDefersInsteadOfBurningTheLeg is F3. A leg that
// reaches the worker path-less (the configuration with no engine worktree manager)
// and loses the checkout MUTATION lock used to be finishQueuedJob(JobFailed) —
// and because the payload was left unmutated, the next poll's re-enqueue matched
// it and was a SILENT no-op, so FindRepeatedReviewers (succeeded verdicts only)
// never re-enlisted the reviewer. The id was burned for good.
//
// MUTATION PROOF (two independent mutants):
//   - drop the deferCheckoutContention call from the prepare error path in
//     daemon_worker.go and phase 1 sees state=failed;
//   - drop the workflow.CheckoutMutationLockContention arm from
//     classifyCheckoutContention and phase 1 sees state=failed too — the deferral
//     is reached but classifies the block as none.
func TestNativeReviewWorktreeContentionDefersInsteadOfBurningTheLeg(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	sharedCheckout, reviewHead, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", sharedCheckout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime, runtime.LastRef, []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	const jobID = "workflow-contended-review"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "feature/review",
		PullRequest: 1698, HeadSHA: reviewHead, TaskID: "task-1698", TaskTitle: "Contended allocation",
		LeadAgent: "implementer", Reviewers: []string{"reviewer"}, ReviewRound: "review-1",
		Instructions: "Review pull request #1698.",
	})
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"re-attempted after contention","findings":[],"changes_made":[],"tests_run":["retry smoke"],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.AdapterFactory = func(_ runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: true}}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		engine := daemonWorkflowEngine(store, github.NoopClient{}, checkout, worker.workflowHome())
		engine.MergeGate = gate
		return engine
	}

	// Phase 1: another worker holds the checkout mutation lock, so the allocation
	// spends its short budget and returns the self-healing block.
	release, err := workflow.AcquireCheckoutMutationLock(ctx, store, sharedCheckout, "other-worker", time.Now().UTC())
	if err != nil {
		t.Fatalf("AcquireCheckoutMutationLock: %v", err)
	}
	contendedAt := time.Now()
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run under contention: %v", err)
	}
	// The allocation must spend the SHORT dispatch budget. Passing 0 (the previous
	// value) expands to the ~2-minute checkoutMutationWaitTimeout, holding this
	// worker slot hostage to a lock taken for a short shared-.git op.
	if waited := time.Since(contendedAt); waited > 60*time.Second {
		t.Fatalf("contended allocation waited %s, want the short %s dispatch budget", waited, workflow.ReadOnlyWorktreeDispatchLockWaitBudget)
	}
	held, payload := blockerE2EJobPayload(t, store, jobID)
	if held.State != string(workflow.JobQueued) {
		t.Fatalf("contended leg state = %q, want queued (a failed leg burns the id forever)", held.State)
	}
	if payload.BlockerClass != string(blockerClassCheckoutContention) || payload.BlockerAttempts != 1 ||
		payload.BlockerRetryAt == "" || !payload.BlockerPreDelivery {
		t.Fatalf("contended leg hold = %+v, want a pre-delivery checkout_contention deferral", payload)
	}
	deferralReason := ""
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == blockerDeferredEventKind {
			deferralReason = event.Message
		}
	}
	if deferralReason == "" {
		t.Fatalf("missing %s job event for the deferred allocation", blockerDeferredEventKind)
	}
	// The recorded reason is what an operator reads off the stuck surface, so it must
	// name the contention and NOT a fetch that was never worth attempting: a spent
	// lock budget is not a cold checkout, and fetching pull/<n>/head cannot clear it.
	if strings.Contains(deferralReason, "fetch PR ref") {
		t.Fatalf("contention hold blames a spurious PR-ref fetch: %q", deferralReason)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter deliveries under contention = %d, want 0", adapter.calls)
	}
	if strings.TrimSpace(payload.WorktreePath) != "" {
		t.Fatalf("deferred leg carries worktree %q, want none allocated", payload.WorktreePath)
	}

	// Phase 2: the holder finishes. The SAME leg must be re-attemptable and now run.
	if err := release(ctx); err != nil {
		t.Fatalf("release checkout mutation lock: %v", err)
	}
	if err := worker.run(ctx, held); err != nil {
		t.Fatalf("worker.run after the lock cleared: %v", err)
	}
	reloaded, after := blockerE2EJobPayload(t, store, jobID)
	cleanupNativeReviewWorktrees(t, sharedCheckout, after.WorktreePath)
	if reloaded.State != string(workflow.JobSucceeded) || adapter.calls != 1 {
		t.Fatalf("re-attempted leg state=%q deliveries=%d, want succeeded with one delivery", reloaded.State, adapter.calls)
	}
	if strings.TrimSpace(after.WorktreePath) == "" || !after.ReadOnlyWorktree {
		t.Fatalf("re-attempted leg payload = %+v, want the exact-head worktree it was denied", after)
	}
}

// TestNativeReviewWorktreeHardFailureStaysTerminal is the other side of F3's
// classification, and the reason the new classifier arm is safe: a NON-transient
// allocation failure — a head the checkout does not carry and cannot fetch — must
// still settle TERMINALLY. Holding it would spin a leg that can never succeed.
//
// MUTATION PROOF: widen the new arm to every workflow.BlockedError (or to any
// allocation error) and this leg is held queued instead of failing.
func TestNativeReviewWorktreeHardFailureStaysTerminal(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	sharedCheckout, _, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", sharedCheckout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime, runtime.LastRef, []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	const jobID = "workflow-unreachable-head-review"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: "reviewer", Action: "review", Repo: "owner/repo", Branch: "feature/review",
		PullRequest: 1698, HeadSHA: "0123456789abcdef0123456789abcdef01234567", TaskID: "task-1698",
		TaskTitle: "Unreachable head", LeadAgent: "implementer", Reviewers: []string{"reviewer"},
		ReviewRound: "review-1", Instructions: "Review pull request #1698.",
	})
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.AdapterFactory = func(_ runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		return nil, errors.New("adapter must never be built for an unallocatable head")
	}
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, worker.workflowHome())
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	reloaded, payload := blockerE2EJobPayload(t, store, jobID)
	if reloaded.State != string(workflow.JobFailed) {
		t.Fatalf("unallocatable-head leg state = %q, want failed", reloaded.State)
	}
	if payload.BlockerClass != "" || payload.BlockerRetryAt != "" {
		t.Fatalf("unallocatable-head leg was HELD: %+v", payload)
	}
	if blockerE2EHasEventKind(t, store, jobID, blockerDeferredEventKind) {
		t.Fatal("a non-transient allocation failure recorded a blocker_deferred event")
	}
}
