package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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
	worktreePath := t.TempDir()
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
	baseWorkflowFactory := h.worker.WorkflowFactory
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
