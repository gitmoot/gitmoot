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

// #2517 follow-up (seen live after the first deploy): the reclaim candidate
// list is shared by every repo in a sweep, and each repo's pass defers the
// rows of other repos. Those cheap skips were checked against the budget, so
// with a slow store the first repo used up the whole budget deferring other
// repos' rows, every owning repo was then turned away, and no worktree was
// reclaimed at all. Skipping another repo's candidate must not spend the
// budget: here a repo that owns none of many candidates goes first, and the
// owning repo must still get its attempt in the same tick.
func TestReclaimSkipsOfOtherReposRowsDoNotSpendTheBudget(t *testing.T) {
	previous := reclaimTickBudget
	reclaimTickBudget = 50 * time.Millisecond
	t.Cleanup(func() { reclaimTickBudget = previous })
	h := newHousekeepingDispatchHarness(t, "owner-gets-its-reclaim")
	const others = 30
	for i := 0; i < others+1; i++ {
		repo := "owner/other"
		if i == others {
			repo = "owner/repo"
		}
		id := fmt.Sprintf("parent/delegation/cand-%02d", i)
		path := managedDelegationReclaimTestPath(t, h.home, "parent", fmt.Sprintf("cand-%02d", i))
		seedCLIJob(t, h.store, db.Job{
			ID: id, Agent: "reader", Type: "implement", State: string(workflow.JobFailed),
			Repo: repo, ParentJobID: "parent", DelegationID: fmt.Sprintf("cand-%02d", i),
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo: repo, ParentJobID: "parent", DelegationID: fmt.Sprintf("cand-%02d", i),
				WorktreePath: path, Branch: fmt.Sprintf("gitmoot-delegation-cand-%02d", i),
				Result: &workflow.AgentResult{Decision: "failed"},
			}),
		}, "seed reclaim candidate")
		if err := h.store.AddJobEvent(h.ctx, db.JobEvent{JobID: id, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved"}); err != nil {
			t.Fatal(err)
		}
	}
	// A slow store: every skip of another repo's row costs time, as the
	// lifecycle-locked deferral writes do on the busy daemon.
	previousDefer := deferDelegationCleanupSkipHook
	deferDelegationCleanupSkipHook = func() { time.Sleep(20 * time.Millisecond) }
	t.Cleanup(func() { deferDelegationCleanupSkipHook = previousDefer })
	attempted := map[string]bool{}
	baseWorkflowFactory := h.worker.defaultWorkflow
	h.worker.WorkflowFactory = func(checkout string) workflow.Engine {
		attempted[checkout] = true
		return baseWorkflowFactory(checkout)
	}
	cand := newTickCandidates(h.store)
	cand.reclaimBudget.resume = h.tracker.reclaimResumeState()
	// The other repo's pass runs first in the sweep, then the owner's. Its
	// skip writes are bounded on their own (#2519 review): 31 writes of 20ms
	// each would take over 600ms; the bound stops writing after about 50ms.
	started := time.Now()
	if err := reclaimSkippedDelegationWorktrees(h.ctx, h.worker, "owner/other-without-rows", "", nil, cand, h.now); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("a repo owning none of the candidates spent %s deferring them before dispatch; skip writes must be bounded", elapsed.Round(time.Millisecond))
	}
	before := len(attempted)
	if err := reclaimSkippedDelegationWorktrees(h.ctx, h.worker, "owner/repo", "", nil, cand, h.now); err != nil {
		t.Fatal(err)
	}
	if len(attempted) == before {
		t.Fatalf("the owning repo got no reclaim attempt: the first repo's %d skips of other repos' rows used up the budget", others+1)
	}
}

// #2519 review: the candidate list is computed once per sweep, so a later
// repo still sees a row its owner just attempted. If the attempt kept the
// worktree with a later retry (a failed capture backs off up to six hours),
// the later repo's skip rewrote it to one minute, making it due again almost
// at once. A skip must leave a row that is not due alone.
func TestReclaimSkipOfAnotherReposRowKeepsItsLaterRetry(t *testing.T) {
	h := newHousekeepingDispatchHarness(t, "later-retry-kept")
	id := "parent/delegation/owned"
	path := managedDelegationReclaimTestPath(t, h.home, "parent", "owned")
	seedCLIJob(t, h.store, db.Job{
		ID: id, Agent: "reader", Type: "implement", State: string(workflow.JobFailed),
		Repo: "owner/repo", ParentJobID: "parent", DelegationID: "owned",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", ParentJobID: "parent", DelegationID: "owned",
			WorktreePath: path, Branch: "gitmoot-delegation-owned",
			Result: &workflow.AgentResult{Decision: "failed"},
		}),
	}, "seed reclaim candidate")
	if err := h.store.AddJobEvent(h.ctx, db.JobEvent{JobID: id, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.EnsureCleanupObligation(h.ctx, id, path, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	cand := newTickCandidates(h.store)
	cand.reclaimBudget.resume = h.tracker.reclaimResumeState()
	// The sweep's candidate list is taken while the row is due ...
	if ids, err := cand.delegationReclaimCandidates(h.ctx, ""); err != nil || len(ids) == 0 {
		t.Fatalf("candidates = %v, %v; want the seeded row", ids, err)
	}
	// ... then its owner's attempt keeps the worktree for six hours.
	later := time.Now().UTC().Add(6 * time.Hour)
	if _, err := h.store.DeferCleanupObligation(h.ctx, id, path, db.CleanupReasonTerminalDeferred, time.Now().UTC(), later); err != nil {
		t.Fatal(err)
	}
	if err := reclaimSkippedDelegationWorktrees(h.ctx, h.worker, "owner/other", "", nil, cand, h.now); err != nil {
		t.Fatal(err)
	}
	obligation, err := h.store.GetCleanupObligation(h.ctx, db.CleanupObligationResourceID(id, path))
	if err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt)
	if err != nil {
		t.Fatal(err)
	}
	if next.Before(later.Add(-time.Second)) {
		t.Fatalf("another repo's skip moved the retry from %s to %s", later.Format(time.RFC3339), next.Format(time.RFC3339))
	}
}

// #2519 review: skip writes had one budget for the skipped and aged passes.
// The skipped pass runs first, so once it spent that budget every aged skip
// wrote nothing and the aged pass's ordered window, full of other repos' rows,
// never advanced. Each pass kind's skip writes need their own budget.
func TestReclaimSkipWriteBudgetIsPerPassKind(t *testing.T) {
	previous := reclaimTickBudget
	reclaimTickBudget = 30 * time.Millisecond
	t.Cleanup(func() { reclaimTickBudget = previous })
	h := newHousekeepingDispatchHarness(t, "aged-skip-advances")
	cand := newTickCandidates(h.store)
	seed := func(name string) (string, string) {
		id := "parent/delegation/" + name
		path := managedDelegationReclaimTestPath(t, h.home, "parent", name)
		seedCLIJob(t, h.store, db.Job{ID: id, Agent: "reader", Type: "implement", State: string(workflow.JobFailed), Repo: "owner/other",
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/other", WorktreePath: path})}, "seed")
		if _, err := h.store.EnsureCleanupObligation(h.ctx, id, path, time.Now().UTC().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		return id, path
	}
	// The skipped pass spends its whole skip-write budget.
	previousDefer := deferDelegationCleanupSkipHook
	deferDelegationCleanupSkipHook = func() { time.Sleep(40 * time.Millisecond) }
	t.Cleanup(func() { deferDelegationCleanupSkipHook = previousDefer })
	for i := 0; i < 2; i++ {
		id, path := seed(fmt.Sprintf("skipped-%d", i))
		if err := cand.deferReclaimSkip(h.ctx, h.worker, reclaimPassSkipped, id, path, db.CleanupReasonTerminalDeferred); err != nil {
			t.Fatal(err)
		}
	}
	// The aged pass's skip of another repo's row must still be written.
	id, path := seed("aged")
	if err := cand.deferReclaimSkip(h.ctx, h.worker, reclaimPassAged, id, path, db.CleanupReasonTerminalDeferred); err != nil {
		t.Fatal(err)
	}
	obligation, err := h.store.GetCleanupObligation(h.ctx, db.CleanupObligationResourceID(id, path))
	if err != nil {
		t.Fatal(err)
	}
	if next, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt); err != nil || !next.After(time.Now().UTC()) {
		t.Fatalf("aged skip was not deferred after the skipped pass spent its budget: next attempt %q (%v)", obligation.NextAttemptAt, err)
	}
}

// #2517 follow-up, seen live after #2519: the reclaim candidate window was
// shared by every repo. An earlier repo's pass deferred another repo's due row
// by a minute, so when the owning repo reached it in the same sweep the
// cleanup refused itself as not due, and no worktree was removed; and rows of a
// repo that never reclaims could fill the shared window for everyone. Each
// repo now has its own window: a pass sees only its own repo's rows, never
// touches another repo's, and many due rows of one repo cannot hide another
// repo's row.
func TestReclaimCandidateWindowsArePerRepo(t *testing.T) {
	h := newHousekeepingDispatchHarness(t, "per-repo-windows")
	seed := func(repo, name string) (string, string) {
		id := "parent/delegation/" + name
		path := managedDelegationReclaimTestPath(t, h.home, "parent", name)
		seedCLIJob(t, h.store, db.Job{
			ID: id, Agent: "reader", Type: "implement", State: string(workflow.JobFailed),
			Repo: repo, ParentJobID: "parent", DelegationID: name,
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo: repo, ParentJobID: "parent", DelegationID: name,
				WorktreePath: path, Branch: "gitmoot-delegation-" + name,
				Result: &workflow.AgentResult{Decision: "failed"},
			}),
		}, "seed reclaim candidate")
		if err := h.store.AddJobEvent(h.ctx, db.JobEvent{JobID: id, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved"}); err != nil {
			t.Fatal(err)
		}
		return id, path
	}
	// More due rows of one repo than a whole window holds, all due earlier
	// than the other repo's single row.
	for i := 0; i < 70; i++ {
		id, path := seed("owner/crowded", fmt.Sprintf("crowded-%02d", i))
		if _, err := h.store.EnsureCleanupObligation(h.ctx, id, path, time.Now().UTC().Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	ownID, ownPath := seed("owner/later", "later-own")
	if _, err := h.store.EnsureCleanupObligation(h.ctx, ownID, ownPath, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	resource := db.CleanupObligationResourceID(ownID, ownPath)
	before, err := h.store.GetCleanupObligation(h.ctx, resource)
	if err != nil {
		t.Fatal(err)
	}
	cand := newTickCandidates(h.store)
	later, err := cand.delegationReclaimCandidates(h.ctx, "owner/later")
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || later[0] != ownID {
		t.Fatalf("owner/later candidates = %v; its row must not be hidden behind owner/crowded's", later)
	}
	// Another repo's pass in the same sweep leaves the row untouched.
	if err := reclaimSkippedDelegationWorktrees(h.ctx, h.worker, "owner/crowded", "", nil, cand, h.now); err != nil {
		t.Fatal(err)
	}
	after, err := h.store.GetCleanupObligation(h.ctx, resource)
	if err != nil {
		t.Fatal(err)
	}
	if after.NextAttemptAt != before.NextAttemptAt {
		t.Fatalf("another repo's pass moved this row's retry from %s to %s; its owner would refuse it as not due", before.NextAttemptAt, after.NextAttemptAt)
	}
}

// #2517 step 3: a reclaim attempt on the background loop and a job starting
// on the same checkout must exclude each other atomically: the reservation is
// refused while a job holds a key, and a job is refused while the reservation
// is held. A check-then-act between two goroutines would let a job start in
// the registered checkout while the loop rewrites its worktree metadata.
func TestReclaimReservationAndJobStartExcludeEachOther(t *testing.T) {
	tracker := newInflightJobTracker(context.Background())
	release, ok := tracker.tryReserveCheckouts("worktree:/seat", "repo:owner/repo")
	if !ok {
		t.Fatal("reservation refused on free keys")
	}
	if admitted, _ := tracker.beginWithinLane(0, false, "job-on-repo", "owner/repo", "repo:owner/repo", ""); admitted {
		t.Fatal("a job started on the registered checkout while reclaim held it")
	}
	if admitted, _ := tracker.beginWithinLane(0, false, "job-elsewhere", "owner/repo", "worktree:/other", ""); !admitted {
		t.Fatal("a job on an unrelated checkout was refused")
	}
	if _, again := tracker.tryReserveCheckouts("repo:owner/repo"); again {
		t.Fatal("a second reclaim reserved a key already reserved")
	}
	if seeds, _ := tracker.seeds(); !seeds["repo:owner/repo"] {
		t.Fatal("the selector does not see the reserved key as held")
	}
	release()
	release() // idempotent
	if admitted, _ := tracker.beginWithinLane(0, false, "job-on-repo", "owner/repo", "repo:owner/repo", ""); !admitted {
		t.Fatal("the job is still refused after the reservation was released")
	}
	if _, ok := tracker.tryReserveCheckouts("worktree:/x", "repo:owner/repo"); ok {
		t.Fatal("reclaim reserved the registered checkout while a job holds it")
	}
	if _, ok := tracker.tryReserveCheckouts("worktree:/x"); !ok {
		t.Fatal("a refused reservation leaked a partial hold on its other key")
	}
	tracker.end("job-on-repo")
	tracker.end("job-elsewhere")
}

// #2517 step 3: with the background reclaim loop running, the worker tick
// must not run the delegation reclaim passes itself: a backlog of slow
// candidates may not delay dispatch at all.
func TestWorkerTickLeavesDelegationReclaimToTheBackgroundLoop(t *testing.T) {
	h := newHousekeepingDispatchHarness(t, "queued-beside-reclaim-loop")
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("slow-%d", i)
		id := "parent/delegation/" + name
		path := managedDelegationReclaimTestPath(t, h.home, "parent", name)
		seedCLIJob(t, h.store, db.Job{
			ID: id, Agent: "reader", Type: "implement", State: string(workflow.JobFailed),
			Repo: "owner/repo", ParentJobID: "parent", DelegationID: name,
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo: "owner/repo", ParentJobID: "parent", DelegationID: name,
				WorktreePath: path, Branch: "gitmoot-delegation-" + name,
				Result: &workflow.AgentResult{Decision: "failed"},
			}),
		}, "seed slow reclaim candidate")
		if err := h.store.AddJobEvent(h.ctx, db.JobEvent{JobID: id, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved"}); err != nil {
			t.Fatal(err)
		}
	}
	h.tracker.setReclaimLoopActive(true)
	baseWorkflowFactory := h.worker.defaultWorkflow
	var engines atomic.Int32
	h.worker.WorkflowFactory = func(checkout string) workflow.Engine {
		engines.Add(1)
		time.Sleep(200 * time.Millisecond)
		return baseWorkflowFactory(checkout)
	}
	if err := runDaemonWorkerTickTracked(h.ctx, h.store, h.worker, 1, false, "owner/repo", "", &h.stdout, h.now, h.tracker, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	if !waitForCondition(t, 5*time.Second, h.adapter.stillBlocked) {
		t.Fatalf("queued job was not dispatched; log=%q", h.stdout.String())
	}
	if made := engines.Load(); made > 1 {
		t.Fatalf("the tick started %d engines; with the reclaim loop active it must start only the dispatch's", made)
	}
}

// #2517 step 3: the background loop pass reclaims a repo's candidate, and
// while its attempt runs no job can start on that repo's registered checkout;
// once the attempt ends the job can start.
func TestBackgroundReclaimPassHoldsTheRepoCheckoutOnlyDuringItsAttempt(t *testing.T) {
	h := newHousekeepingDispatchHarness(t, "background-reclaim-pass")
	id := "parent/delegation/bg"
	path := managedDelegationReclaimTestPath(t, h.home, "parent", "bg")
	seedCLIJob(t, h.store, db.Job{
		ID: id, Agent: "reader", Type: "implement", State: string(workflow.JobFailed),
		Repo: "owner/repo", ParentJobID: "parent", DelegationID: "bg",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", ParentJobID: "parent", DelegationID: "bg",
			WorktreePath: path, Branch: "gitmoot-delegation-bg",
			Result: &workflow.AgentResult{Decision: "failed"},
		}),
	}, "seed reclaim candidate")
	if err := h.store.AddJobEvent(h.ctx, db.JobEvent{JobID: id, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved"}); err != nil {
		t.Fatal(err)
	}
	baseWorkflowFactory := h.worker.defaultWorkflow
	var engines atomic.Int32
	var startedDuringAttempt atomic.Bool
	h.worker.WorkflowFactory = func(checkout string) workflow.Engine {
		engines.Add(1)
		if admitted, _ := h.tracker.beginWithinLane(0, false, "probe-during", "owner/repo", "repo:owner/repo", ""); admitted {
			startedDuringAttempt.Store(true)
			h.tracker.end("probe-during")
		}
		return baseWorkflowFactory(checkout)
	}
	repos := func(context.Context) ([]string, error) { return []string{"owner/repo"}, nil }
	runWorktreeReclaimLoopPass(h.ctx, h.store, h.worker, h.tracker, "", repos, &h.stdout, time.Now().UTC(), time.Time{})
	if engines.Load() == 0 {
		t.Fatalf("the loop pass made no reclaim attempt; log=%q", h.stdout.String())
	}
	if startedDuringAttempt.Load() {
		t.Fatal("a job started on the registered checkout during the reclaim attempt")
	}
	if admitted, _ := h.tracker.beginWithinLane(0, false, "probe-after", "owner/repo", "repo:owner/repo", ""); !admitted {
		t.Fatal("the registered checkout stayed reserved after the attempt")
	}
	h.tracker.end("probe-after")
}
