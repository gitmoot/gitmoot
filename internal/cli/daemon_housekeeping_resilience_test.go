package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type housekeepingDispatchHarness struct {
	ctx      context.Context
	home     string
	store    *db.Store
	checkout string
	worker   jobWorker
	adapter  *wedgeBlockingAdapter
	tracker  *inflightJobTracker
	stdout   bytes.Buffer
	now      time.Time
	queuedID string
}

func newHousekeepingDispatchHarness(t *testing.T, queuedID string) *housekeepingDispatchHarness {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: queuedID, Agent: "audit", Action: "ask",
		Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})

	h := &housekeepingDispatchHarness{
		ctx: ctx, home: home, store: store, checkout: checkout,
		now: time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC), queuedID: queuedID,
	}
	h.worker = defaultJobWorker(store, &h.stdout, home)
	h.worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	h.adapter = newWedgeBlockingAdapter(queuedID)
	h.worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return h.adapter, nil
	}
	h.tracker = newInflightJobTracker(ctx)
	t.Cleanup(func() {
		close(h.adapter.release)
		h.tracker.drain(os.Stderr, 5*time.Second)
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	return h
}

func (h *housekeepingDispatchHarness) runAndAssertDispatched(t *testing.T, repoFilter, wantLog string) {
	t.Helper()
	if err := runDaemonWorkerTickTracked(
		h.ctx, h.store, h.worker, 1, false, repoFilter, "", &h.stdout, h.now, h.tracker, nil,
	); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked returned housekeeping error before dispatch: %v", err)
	}
	if !waitForCondition(t, 5*time.Second, h.adapter.stillBlocked) {
		t.Fatalf("queued job %s was not admitted; deliveries=%v log=%q", h.queuedID, h.adapter.deliveredJobs(), h.stdout.String())
	}
	if !strings.Contains(h.stdout.String(), wantLog) {
		t.Fatalf("housekeeping failure %q was not logged: %q", wantLog, h.stdout.String())
	}
}

func TestSkippedDelegationWorktreeReclaimFailureDoesNotBlockDispatch(t *testing.T) {
	h := newHousekeepingDispatchHarness(t, "queued-after-skipped-reclaim")
	const failedJobID = "parent/delegation/skipped"
	worktreePath := managedDelegationReclaimTestPath(t, h.home, "parent", "skipped")
	seedCLIJob(t, h.store, db.Job{
		ID: failedJobID, Agent: "reader", Type: "implement", State: string(workflow.JobFailed),
		Repo: "owner/repo", ParentJobID: "parent", DelegationID: "skipped",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", ParentJobID: "parent", DelegationID: "skipped",
			WorktreePath: worktreePath, Branch: "gitmoot-delegation-skipped",
			Result: &workflow.AgentResult{Decision: "failed"},
		}),
	}, "seed skipped delegation")
	if err := h.store.AddJobEvent(h.ctx, db.JobEvent{
		JobID: failedJobID, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved",
	}); err != nil {
		t.Fatalf("add skipped cleanup marker: %v", err)
	}
	baseWorkflowFactory := h.worker.defaultWorkflow
	var factoryCalls atomic.Int32
	h.worker.WorkflowFactory = func(checkout string) workflow.Engine {
		// A nil store makes the selected item's real reclaim operation fail at
		// Engine validation without corrupting the scheduler's shared store. The
		// queued dispatch receives the normal engine on its later factory call.
		if factoryCalls.Add(1) == 1 {
			return workflow.Engine{}
		}
		return baseWorkflowFactory(checkout)
	}

	h.runAndAssertDispatched(t, "owner/repo", "store is required")
}

// TestCleanupAccountingFailureDoesNotBlockDispatch kills the mutant that
// returns a selected candidate's cleanup-accounting failure from the worker
// tick before queued-job dispatch. The failed durable write must stop cleanup,
// while the unrelated queued job is still admitted.
func TestCleanupAccountingFailureDoesNotBlockDispatch(t *testing.T) {
	h := newHousekeepingDispatchHarness(t, "queued-after-cleanup-accounting")
	const failedJobID = "parent/delegation/accounting"
	worktreePath := managedDelegationReclaimTestPath(t, h.home, "parent", "accounting")
	seedCLIJob(t, h.store, db.Job{
		ID: failedJobID, Agent: "reader", Type: "implement", State: string(workflow.JobFailed),
		Repo: "owner/repo", ParentJobID: "parent", DelegationID: "accounting",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", ParentJobID: "parent", DelegationID: "accounting",
			WorktreePath: worktreePath, Branch: "gitmoot-delegation-accounting",
			Result: &workflow.AgentResult{Decision: "failed"},
		}),
	}, "seed accounting-failure delegation")
	if err := h.store.AddJobEvent(h.ctx, db.JobEvent{
		JobID: failedJobID, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved",
	}); err != nil {
		t.Fatalf("add skipped cleanup marker: %v", err)
	}
	if _, err := h.store.EnsureCleanupObligation(h.ctx, failedJobID, worktreePath, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("seed cleanup obligation: %v", err)
	}
	raw, err := sql.Open("sqlite", h.store.DatabasePath())
	if err != nil {
		t.Fatalf("open trigger connection: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	if _, err := raw.Exec(`
CREATE TRIGGER fail_tick_cleanup_accounting
BEFORE UPDATE ON cleanup_obligations
WHEN OLD.owner_job_id = 'parent/delegation/accounting'
BEGIN
  SELECT RAISE(ABORT, 'injected tick cleanup accounting failure');
END;`); err != nil {
		t.Fatalf("create cleanup accounting trigger: %v", err)
	}
	manager := &fakeReclaimWorktreeManager{branches: map[string]bool{"gitmoot-delegation-accounting": true}}
	h.worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store: h.store, Home: h.worker.workflowHome(), DelegationCheckout: h.checkout,
			DelegationWorktrees: manager,
		}
	}

	h.runAndAssertDispatched(t, "owner/repo", "injected tick cleanup accounting failure")
	if len(manager.removed) != 0 || len(manager.deleted) != 0 {
		t.Fatalf("unaccounted cleanup actuated: removed=%v deleted=%v", manager.removed, manager.deleted)
	}
	obligation, err := h.store.GetCleanupObligation(h.ctx, db.CleanupObligationResourceID(failedJobID, worktreePath))
	if err != nil {
		t.Fatal(err)
	}
	if obligation.State != db.CleanupObligationPending || obligation.AttemptCount != 0 {
		t.Fatalf("cleanup obligation changed despite failed accounting: %+v", obligation)
	}
}

func TestPendingJobAdvancementFailureDoesNotBlockDispatch(t *testing.T) {
	h := newHousekeepingDispatchHarness(t, "queued-after-advancement")
	const failedJobID = "pending-advancement"
	seedDaemonWorkerAgent(t, h.store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	seedCLIJob(t, h.store, db.Job{
		ID: failedJobID, Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded),
		Repo: "owner/repo",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", Branch: "main", PullRequest: 7, TaskID: "task-advance",
			Result: &workflow.AgentResult{Decision: "approved", Summary: "approved"},
		}),
	}, "seed pending advancement")
	if err := h.store.AddJobEvent(h.ctx, db.JobEvent{
		JobID: failedJobID, Kind: "advance_started", Message: "pending",
	}); err != nil {
		t.Fatalf("add advancement marker: %v", err)
	}
	raw, err := sql.Open("sqlite", h.store.DatabasePath())
	if err != nil {
		t.Fatalf("open trigger connection: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	if _, err := raw.Exec(`
CREATE TRIGGER fail_pending_advancement_retry
BEFORE INSERT ON job_events
WHEN NEW.job_id = 'pending-advancement' AND NEW.kind = 'advance_retry'
BEGIN
  SELECT RAISE(ABORT, 'injected pending advancement failure');
END;`); err != nil {
		t.Fatalf("create advancement failure trigger: %v", err)
	}
	h.worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store: h.store,
			MergeGate: &cliWorkerFakeMergeGate{
				err: errors.New("force advancement retry marker"),
			},
		}
	}

	h.runAndAssertDispatched(t, "owner/repo", "injected pending advancement failure")
}

func TestPendingJobCommentFailureDoesNotBlockDispatch(t *testing.T) {
	h := newHousekeepingDispatchHarness(t, "queued-after-comment")
	const failedJobID = "pending-comment"
	seedCLIJob(t, h.store, db.Job{
		ID: failedJobID, Agent: "audit", Type: "ask", State: string(workflow.JobFailed),
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "malformed-repository", PullRequest: 9,
			Result: &workflow.AgentResult{Decision: "failed", Summary: "failed"},
		}),
	}, "seed pending comment")
	if err := h.store.AddJobEvent(h.ctx, db.JobEvent{
		JobID: failedJobID, Kind: "comment_post_failed", Message: "pending",
	}); err != nil {
		t.Fatalf("add comment retry marker: %v", err)
	}
	h.worker.CommenterFactory = func(string) github.Client {
		return &cliPollFakeGitHub{}
	}

	h.runAndAssertDispatched(t, "", "repo must be owner/repo")
}

// #2517: worktree reclaim runs before dispatch in the sequential repo sweep.
// With slow cleanup (captures on an sshfs mount took up to two minutes each)
// a tick with a backlog of cleanup candidates held every queued job back for
// 8-36 minutes. The tick's reclaim budget must stop starting candidates once
// spent, so dispatch follows within about one candidate past the budget.
func TestSlowWorktreeReclaimBacklogDoesNotHoldDispatch(t *testing.T) {
	previous := reclaimTickBudget
	reclaimTickBudget = 300 * time.Millisecond
	t.Cleanup(func() { reclaimTickBudget = previous })
	h := newHousekeepingDispatchHarness(t, "queued-behind-reclaim-backlog")
	const backlog = 8
	for i := 0; i < backlog; i++ {
		id := fmt.Sprintf("parent/delegation/slow-%d", i)
		path := managedDelegationReclaimTestPath(t, h.home, "parent", fmt.Sprintf("slow-%d", i))
		seedCLIJob(t, h.store, db.Job{
			ID: id, Agent: "reader", Type: "implement", State: string(workflow.JobFailed),
			Repo: "owner/repo", ParentJobID: "parent", DelegationID: fmt.Sprintf("slow-%d", i),
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo: "owner/repo", ParentJobID: "parent", DelegationID: fmt.Sprintf("slow-%d", i),
				WorktreePath: path, Branch: fmt.Sprintf("gitmoot-delegation-slow-%d", i),
				Result: &workflow.AgentResult{Decision: "failed"},
			}),
		}, "seed slow reclaim candidate")
		if err := h.store.AddJobEvent(h.ctx, db.JobEvent{JobID: id, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved"}); err != nil {
			t.Fatal(err)
		}
	}
	// Every engine is slow, as a capture on the sshfs mount is. The tick makes
	// one engine per reclaim candidate it starts, then one for the dispatch.
	baseWorkflowFactory := h.worker.defaultWorkflow
	var engines atomic.Int32
	h.worker.WorkflowFactory = func(checkout string) workflow.Engine {
		engines.Add(1)
		time.Sleep(200 * time.Millisecond)
		return baseWorkflowFactory(checkout)
	}
	started := time.Now()
	if err := runDaemonWorkerTickTracked(h.ctx, h.store, h.worker, 1, false, "owner/repo", "", &h.stdout, h.now, h.tracker, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	if !waitForCondition(t, 5*time.Second, h.adapter.stillBlocked) {
		t.Fatalf("queued job was not dispatched; log=%q", h.stdout.String())
	}
	// Budget 300ms, 200ms per candidate: two candidates start, then dispatch.
	// Without the budget all eight run first (nine engines, about 1.8s).
	if elapsed, made := time.Since(started), engines.Load(); made > 4 || elapsed > 1500*time.Millisecond {
		t.Fatalf("tick started %d engines (%d reclaim candidates of %d) over %s before dispatching; the reclaim budget must stop it", made, made-1, backlog, elapsed.Round(time.Millisecond))
	}
}

// #2518 review (P2): the repo sweep order is fixed, so with one shared budget
// the first repo whose cleanup candidates are slow took the whole budget on
// every tick and later repos never reclaimed anything. The first repo the
// budget turns away must go first on the next tick, and every repo must get a
// turn within a few ticks while the first repo still has slow candidates.
func TestReclaimBudgetRotatesAcrossReposWhenOneRepoIsSlow(t *testing.T) {
	previous := reclaimTickBudget
	reclaimTickBudget = 10 * time.Second
	t.Cleanup(func() { reclaimTickBudget = previous })
	tracker := &inflightJobTracker{}
	repos := []string{"owner/a", "owner/b", "owner/c", "owner/d"}
	// Every candidate takes 12s, longer than the whole budget; each repo
	// always has more.
	served := map[string][]int{}
	clock := time.Unix(1_800_000_000, 0)
	for tick := 1; tick <= 4; tick++ {
		cand := &tickCandidates{}
		cand.reclaimBudget.resume = tracker.reclaimResumeState()
		for _, repo := range repos {
			cand.reclaimPassStart(reclaimPassSkipped, repo)
			for cand.reclaimMayStart(reclaimPassSkipped, repo, clock) {
				served[repo] = append(served[repo], tick)
				clock = clock.Add(12 * time.Second)
			}
		}
		cand.finishReclaimTick()
	}
	for i, repo := range repos {
		if got := served[repo]; len(got) != 1 || got[0] != i+1 {
			t.Fatalf("repo %s served on ticks %v, want exactly tick %d (round robin); all: %v", repo, got, i+1, served)
		}
	}
}
