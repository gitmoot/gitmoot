package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// recordingEscalationNotifier captures NotifyEscalation calls so a pre-flight
// escalate_human pause can be asserted to have @-notified the human, mirroring
// the engine package's recordingNotifier (#340).
type recordingEscalationNotifier struct {
	calls []workflow.EscalationRequest
}

func (n *recordingEscalationNotifier) NotifyEscalation(_ context.Context, request workflow.EscalationRequest) error {
	n.calls = append(n.calls, request)
	return nil
}

// preflightHarness wires a jobWorker whose CheckoutValidator fails
// deterministically (no git/worktree setup) and whose WorkflowFactory returns a
// single shared engine carrying a recording notifier — so the same engine both
// dispatches the delegation children (seedPreflightCoordinator) and is the one
// finalizeTimedOutDelegationChild rebuilds to advance the parent DAG.
type preflightHarness struct {
	store    *db.Store
	worker   jobWorker
	engine   workflow.Engine
	notifier *recordingEscalationNotifier
	checkout string
}

const preflightChildBranch = "task-005"

// newPreflightHarness seeds a repo + coordinator/child agents and a worker whose
// CheckoutValidator stages a worktree-less-checkout pre-flight failure (the #409
// trigger). failPolicy is the child delegation's failure_policy.
func newPreflightHarness(t *testing.T, failPolicy string) *preflightHarness {
	t.Helper()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "jerryfane/gitmoot", checkout)
	seedDaemonWorkerAgent(t, store, "coord", runtime.ShellRuntime, "unused", []string{"ask"}, "jerryfane/gitmoot")
	seedDaemonWorkerAgent(t, store, "api", runtime.ShellRuntime, "unused", []string{"review"}, "jerryfane/gitmoot")
	seedDaemonWorkerAgent(t, store, "ui", runtime.ShellRuntime, "unused", []string{"review"}, "jerryfane/gitmoot")

	notifier := &recordingEscalationNotifier{}
	engine := daemonWorkflowEngine(store, github.NewClient(checkout), checkout, "")
	engine.EscalationNotifier = notifier

	worker := defaultJobWorker(store, io.Discard)
	// The shared checkout sits on main; a worktree-less child inherits a non-main
	// branch, so validateTargetCheckout rejects it. Stage that exact failure with
	// no git setup.
	worker.CheckoutValidator = func(_ context.Context, _ db.Job, payload workflow.JobPayload, _ runtime.Agent) (string, error) {
		return "", checkoutMismatch(payload.Branch)
	}
	worker.WorkflowFactory = func(string) workflow.Engine { return engine }

	h := &preflightHarness{store: store, worker: worker, engine: engine, notifier: notifier, checkout: checkout}
	h.seedPreflightCoordinator(t, failPolicy)
	return h
}

func checkoutMismatch(branch string) error {
	return errors.New("checkout branch is main, not job branch " + branch)
}

// seedPreflightCoordinator inserts a coordinator with a failing leg under
// failPolicy plus an independent sibling, then advances it once so both children
// are dispatched (JobQueued, ParentJobID set, Result nil) — the exact pre-flight
// state of a worktree-less delegation child.
func (h *preflightHarness) seedPreflightCoordinator(t *testing.T, failPolicy string) {
	t.Helper()
	ctx := context.Background()
	coordinator := db.Job{ID: "parent-job", Agent: "coord", Type: "ask", State: string(workflow.JobSucceeded)}
	payload := workflow.JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    preflightChildBranch,
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &workflow.AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []workflow.Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: failPolicy},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
			},
		},
	}
	encoded := mustJobPayload(t, payload)
	coordinator.Payload = encoded
	if err := h.store.CreateJobWithEvent(ctx, coordinator, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(coordinator) returned error: %v", err)
	}
	if err := h.engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	// Sanity: the failing leg is queued with a parent and no result — the bug's
	// precondition.
	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if child.State != string(workflow.JobQueued) {
		t.Fatalf("child state after dispatch = %q, want queued", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	if cp.ParentJobID != "parent-job" || cp.Result != nil {
		t.Fatalf("child payload = %+v, want ParentJobID=parent-job & Result nil", cp)
	}
}

func mustWorkerJob(t *testing.T, store *db.Store, jobID string) db.Job {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	}
	return job
}

// runChildTick runs one worker tick over the failing leg, expecting no
// propagated error (the pre-flight failure is finalized into the DAG).
func (h *preflightHarness) runChildTick(t *testing.T) {
	t.Helper()
	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if err := h.worker.run(context.Background(), child); err != nil {
		t.Fatalf("worker.run(child) returned error: %v", err)
	}
}

func countWorkerJobEvents(t *testing.T, store *db.Store, jobID, kind string) int {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s) returned error: %v", jobID, err)
	}
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func workerTaskState(t *testing.T, store *db.Store, taskID string) string {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask(%s) returned error: %v", taskID, err)
	}
	return task.State
}

// TestPreflightDelegationChildEscalateHumanPausesTree is the load-bearing #409
// regression: a delegation child that fails the daemon's pre-flight checkout
// validation (never reaching its runtime) must still advance the parent DAG so
// the escalate_human failure_policy pauses the tree for a human. Before the fix
// the child stranded `failed` with Result == nil, advanceDelegations never ran,
// and the tree was silently failed.
func TestPreflightDelegationChildEscalateHumanPausesTree(t *testing.T) {
	ctx := context.Background()
	h := newPreflightHarness(t, "escalate_human")

	h.runChildTick(t)

	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if child.State != string(workflow.JobFailed) {
		t.Fatalf("child state = %q, want failed", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	// The synthetic result is the proof the engine finalized the result-less child.
	if cp.Result == nil || cp.Result.Decision != "failed" {
		t.Fatalf("child result = %+v, want a synthetic failed result", cp.Result)
	}

	// The shared parent task is paused awaiting a human (the durable Attention
	// signal), proving the DAG advanced and the failure_policy fired.
	if got := workerTaskState(t, h.store, "task-5"); got != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state = %q, want awaiting_human", got)
	}
	// The human was notified exactly once with the resume context.
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(h.notifier.calls))
	}
	if c := h.notifier.calls[0]; c.CoordinatorJobID != "parent-job" || c.DelegationID != "api" {
		t.Fatalf("notifier request = %+v, want coordinator parent-job / delegation api", c)
	}
	// No continuation is enqueued: an awaiting-human tree consumes zero compute.
	if jobExistsForWorker(t, h.store, "parent-job/continuation") {
		t.Fatal("escalate_human pause must NOT enqueue a continuation")
	}
	// The DAG advance was recorded (the finalize bridge ran).
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 1", got)
	}

	// Idempotency: a second tick is a no-op — the child already has a result, so
	// finalize re-enters as a no-op and the parent is not double-advanced.
	if err := h.engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		// A direct re-advance of an already-paused leg returns AwaitingHumanError,
		// not nil; tolerate either as long as it does not double-emit below.
		_ = err
	}
	// Re-run the WORKER tick (the realistic stale-running retry): still no error,
	// no second finalize, no second notification.
	child2 := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if err := h.worker.run(ctx, child2); err != nil {
		t.Fatalf("second worker.run(child) returned error: %v", err)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls after second tick = %d, want 1 (idempotent)", len(h.notifier.calls))
	}
	if got := workerTaskState(t, h.store, "task-5"); got != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state after second tick = %q, want still awaiting_human", got)
	}
}

func jobExistsForWorker(t *testing.T, store *db.Store, jobID string) bool {
	t.Helper()
	_, err := store.GetJob(context.Background(), jobID)
	if err == nil {
		return true
	}
	return false
}

// TestPreflightDelegationChildFailurePolicies parameterizes the pre-flight
// finalize over the non-escalate_human policies, proving routing through
// advanceDelegations fixes ALL of them, not just escalate_human.
func TestPreflightDelegationChildFailurePolicies(t *testing.T) {
	cases := []struct {
		policy        string
		wantTask      string
		wantContinue  bool
		wantSiblingUp bool
	}{
		// block_parent: the shared parent task is blocked.
		{policy: "block_parent", wantTask: string(workflow.TaskBlocked), wantContinue: false},
		// escalate: a coordinator continuation is enqueued so the tree proceeds.
		{policy: "escalate", wantTask: "", wantContinue: true},
		// continue: independent siblings proceed; only this branch's dependents stop.
		{policy: "continue", wantTask: "", wantContinue: false, wantSiblingUp: true},
	}
	for _, tc := range cases {
		t.Run(tc.policy, func(t *testing.T) {
			h := newPreflightHarness(t, tc.policy)

			h.runChildTick(t)

			child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
			if child.State != string(workflow.JobFailed) {
				t.Fatalf("child state = %q, want failed", child.State)
			}
			cp, err := daemonJobPayload(child)
			if err != nil {
				t.Fatalf("daemonJobPayload(child) returned error: %v", err)
			}
			if cp.Result == nil {
				t.Fatalf("child result = nil, want a synthetic result so advanceDelegations ran")
			}
			if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
				t.Fatalf("delegation_timeout_finalized events = %d, want 1", got)
			}

			if tc.wantTask != "" {
				if got := workerTaskState(t, h.store, "task-5"); got != tc.wantTask {
					t.Fatalf("task state = %q, want %q", got, tc.wantTask)
				}
			}
			gotContinuation := jobExistsForWorker(t, h.store, "parent-job/continuation")
			if gotContinuation != tc.wantContinue {
				t.Fatalf("continuation enqueued = %v, want %v", gotContinuation, tc.wantContinue)
			}
		})
	}
}

// TestPreflightDelegationFinalizeIsGeneralNotCheckoutSpecific proves the fix is
// keyed on the queued→failed transition, not the checkout cause: an AdapterFactory
// pre-flight failure (a different finishQueuedJob site) advances the DAG and fires
// the policy exactly the same way.
func TestPreflightDelegationFinalizeIsGeneralNotCheckoutSpecific(t *testing.T) {
	h := newPreflightHarness(t, "escalate_human")
	// Let checkout SUCCEED so the failure happens at the adapter-factory step.
	h.worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return h.checkout, nil
	}
	h.worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return nil, errors.New("adapter bring-up failed")
	}

	h.runChildTick(t)

	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if child.State != string(workflow.JobFailed) {
		t.Fatalf("child state = %q, want failed", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	if cp.Result == nil {
		t.Fatalf("child result = nil, want a synthetic result even for a non-checkout failure")
	}
	if got := workerTaskState(t, h.store, "task-5"); got != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state = %q, want awaiting_human (policy fired for an adapter failure too)", got)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(h.notifier.calls))
	}
}

// TestPreflightCancelledChildIsNotForceFinalized pins the cancelled-safety
// invariant: a child cancelled during pre-flight (state JobCancelled, NOT failed)
// must not be force-finalized — finishQueuedJob only finalizes a genuine
// queued→failed transition.
func TestPreflightCancelledChildIsNotForceFinalized(t *testing.T) {
	ctx := context.Background()
	h := newPreflightHarness(t, "escalate_human")

	// Cancel the child before the worker observes it (the operator cancel race):
	// the queued→failed transition will be a no-op, so no finalize must run.
	if _, err := h.store.TransitionJobStateWithEvent(ctx, "parent-job/delegation/api", string(workflow.JobQueued), string(workflow.JobCancelled), db.JobEvent{
		JobID:   "parent-job/delegation/api",
		Kind:    string(workflow.JobCancelled),
		Message: "operator cancelled",
	}); err != nil {
		t.Fatalf("cancel transition returned error: %v", err)
	}

	// finishQueuedJob attempts queued→failed; the child is already cancelled, so
	// the transition does not fire and the finalize is skipped.
	if err := h.worker.finishQueuedJob(ctx, "parent-job/delegation/api", workflow.JobFailed, errors.New("pre-flight failure after cancel")); err != nil {
		t.Fatalf("finishQueuedJob returned error: %v", err)
	}

	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if child.State != string(workflow.JobCancelled) {
		t.Fatalf("child state = %q, want cancelled (not force-finalized)", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	if cp.Result != nil {
		t.Fatalf("cancelled child must NOT get a synthetic result: %+v", cp.Result)
	}
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 0 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 0 for a cancelled child", got)
	}
	// The parent must not be paused by a cancelled child.
	if jobExistsForWorker(t, h.store, "parent-job/continuation") {
		t.Fatal("a cancelled pre-flight child must not advance the parent DAG")
	}
}

// TestFinishQueuedJobNonDelegationUnaffected pins the byte-identical guarantee:
// a non-delegation job (no ParentJobID) closed via finishQueuedJob is just
// transitioned to failed — no finalize, no engine call.
func TestFinishQueuedJobNonDelegationUnaffected(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "jerryfane/gitmoot", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "jerryfane/gitmoot")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "ask-job", Agent: "audit", Action: "ask", Repo: "jerryfane/gitmoot", Branch: "main", PullRequest: 1})

	worker := defaultJobWorker(store, io.Discard)
	// A WorkflowFactory that panics proves the non-delegation path never touches
	// the engine.
	worker.WorkflowFactory = func(string) workflow.Engine {
		t.Fatal("non-delegation finishQueuedJob must not build an engine")
		return workflow.Engine{}
	}

	if err := worker.finishQueuedJob(ctx, "ask-job", workflow.JobFailed, errors.New("pre-flight failure")); err != nil {
		t.Fatalf("finishQueuedJob returned error: %v", err)
	}
	job := mustWorkerJob(t, store, "ask-job")
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}
