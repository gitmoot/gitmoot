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
// trigger). failPolicy is the child delegation's failure_policy. The failing leg
// delegates a `review` action (no write/branch-lock setup needed).
func newPreflightHarness(t *testing.T, failPolicy string) *preflightHarness {
	t.Helper()
	return newPreflightHarnessForAction(t, failPolicy, "review")
}

// newPreflightHarnessForAction is newPreflightHarness with the failing leg's
// delegated action overridable (e.g. "implement" for the read-only-implement
// finding), so a single shared harness covers every pre-flight failure class.
func newPreflightHarnessForAction(t *testing.T, failPolicy string, failingAction string) *preflightHarness {
	t.Helper()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "jerryfane/gitmoot", checkout)
	seedDaemonWorkerAgent(t, store, "coord", runtime.ShellRuntime, "unused", []string{"ask"}, "jerryfane/gitmoot")
	seedDaemonWorkerAgent(t, store, "api", runtime.ShellRuntime, "unused", []string{failingAction}, "jerryfane/gitmoot")
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
	h.seedPreflightCoordinator(t, failPolicy, failingAction)
	return h
}

func checkoutMismatch(branch string) error {
	return errors.New("checkout branch is main, not job branch " + branch)
}

// seedPreflightCoordinator inserts a coordinator with a failing leg under
// failPolicy plus an independent sibling, then advances it once so both children
// are dispatched (JobQueued, ParentJobID set, Result nil) — the exact pre-flight
// state of a worktree-less delegation child.
func (h *preflightHarness) seedPreflightCoordinator(t *testing.T, failPolicy string, failingAction string) {
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
				{ID: "api", Agent: "api", Action: failingAction, Prompt: "build api", FailurePolicy: failPolicy},
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

			if tc.wantSiblingUp {
				// continue's distinguishing, load-bearing signal: the failing leg is
				// finalized WITHOUT taking down the independent sibling. The sibling
				// stays queued (runnable, not blocked) and result-less — proving the
				// failure_policy stopped only this branch, not the whole tree (the
				// block_parent case above would instead block the shared task).
				sibling := mustWorkerJob(t, h.store, "parent-job/delegation/ui")
				if sibling.State != string(workflow.JobQueued) {
					t.Fatalf("sibling state = %q, want queued (continue must not block the independent sibling)", sibling.State)
				}
				sp, err := daemonJobPayload(sibling)
				if err != nil {
					t.Fatalf("daemonJobPayload(sibling) returned error: %v", err)
				}
				if sp.Result != nil {
					t.Fatalf("sibling result = %+v, want nil (the sibling was not touched by the failing leg)", sp.Result)
				}
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

// TestPreflightDelegationChildBlockedAdvancesParent is the load-bearing #409
// regression for the BLOCKED class (finding 1): a delegation child that fails an
// executor pre-flight check returning a BlockedError (here, the agent lacks the
// delegated action's capability) is routed by handleRunJobError to
// finishQueuedJob(..., JobBlocked, ...). Before the gate was widened to include
// JobBlocked the child stranded `blocked` with Result == nil and the parent DAG
// never advanced — the exact #409 bug for blocked pre-flight failures. With the
// fix the engine finalizes the blocked child (synthetic result + DAG advance) so
// the escalate_human failure_policy fires.
func TestPreflightDelegationChildBlockedAdvancesParent(t *testing.T) {
	ctx := context.Background()
	h := newPreflightHarness(t, "escalate_human")
	// Let checkout + adapter bring-up SUCCEED so the failure happens INSIDE
	// engine.RunJob at ensureJobExecutorAllowed (the BlockedError class), not at a
	// finishQueuedJob(JobFailed) site. The stub adapter is never invoked because
	// ensureJobExecutorAllowed blocks first.
	h.worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return h.checkout, nil
	}
	h.worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return cockpitStubAdapter{}, nil
	}
	// Strip the api agent's "review" capability so ensureJobExecutorAllowed returns
	// e.block(...) -> BlockedError; handleRunJobError then routes the still-queued
	// child to finishQueuedJob(JobBlocked).
	if err := h.store.UpsertAgent(ctx, db.Agent{
		Name: "api", Role: "worker", Runtime: runtime.ShellRuntime, RuntimeRef: "unused",
		RepoScope: "jerryfane/gitmoot", Capabilities: []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto, HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent(api, no review cap) returned error: %v", err)
	}

	h.runChildTick(t)

	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	// The child landed BLOCKED (not failed) — the class the un-widened gate missed.
	if child.State != string(workflow.JobBlocked) {
		t.Fatalf("child state = %q, want blocked", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	// The synthetic result is the proof the engine finalized the result-less,
	// blocked child so advanceDelegations could run.
	if cp.Result == nil || cp.Result.Decision != "failed" {
		t.Fatalf("child result = %+v, want a synthetic failed result", cp.Result)
	}
	// The finalize bridge ran exactly once.
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 1", got)
	}
	// The DAG advanced and escalate_human fired: the shared parent task is paused
	// awaiting a human and the human was notified once with the resume context.
	if got := workerTaskState(t, h.store, "task-5"); got != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state = %q, want awaiting_human (DAG advanced for the blocked child)", got)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(h.notifier.calls))
	}
	if c := h.notifier.calls[0]; c.CoordinatorJobID != "parent-job" || c.DelegationID != "api" {
		t.Fatalf("notifier request = %+v, want coordinator parent-job / delegation api", c)
	}

	// Idempotency: a second worker tick over the now-finalized blocked child is a
	// no-op — the child already has a result, so finalize re-enters cleanly with no
	// second finalize and no second notification.
	child2 := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if err := h.worker.run(ctx, child2); err != nil {
		t.Fatalf("second worker.run(child) returned error: %v", err)
	}
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events after second tick = %d, want 1 (idempotent)", got)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls after second tick = %d, want 1 (idempotent)", len(h.notifier.calls))
	}
}

// TestPreflightReadOnlyImplementDelegationChildAdvancesParent is the load-bearing
// regression for finding 2: a coordinator that delegates an `implement` action to
// a read-only-autonomy agent short-circuits at the readOnlyImplementationBlocked
// branch in run() — BEFORE CheckoutValidator/RunJob — via markJobPermissionBlocked
// (a direct queued->blocked transition, NOT finishQueuedJob) plus
// blockTaskForPermissionBlockedJob (which only blocks the task). Without routing
// this delegation child through the finalize helper the parent DAG never advances
// and the failure_policy never fires — #409 via a separate code path that
// widening the finishQueuedJob gate does NOT cover.
func TestPreflightReadOnlyImplementDelegationChildAdvancesParent(t *testing.T) {
	ctx := context.Background()
	// The failing leg delegates an `implement` action; its agent (api) is then made
	// read-only, so run() takes the readOnlyImplementationBlocked branch.
	h := newPreflightHarnessForAction(t, "escalate_human", "implement")
	// A panicking CheckoutValidator proves the short-circuit fires BEFORE checkout
	// (the finding-2 path never reaches finishQueuedJob).
	h.worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		t.Fatal("read-only implement block must short-circuit before CheckoutValidator")
		return "", nil
	}
	// Re-register api as a read-only implement agent so
	// readOnlyImplementationBlocked(job.Type, agent) is true.
	if err := h.store.UpsertAgent(ctx, db.Agent{
		Name: "api", Role: "worker", Runtime: runtime.CodexRuntime, RuntimeRef: "unused",
		RepoScope: "jerryfane/gitmoot", Capabilities: []string{"implement"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly, HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent(api, read-only implement) returned error: %v", err)
	}

	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if err := h.worker.run(ctx, child); err != nil {
		t.Fatalf("worker.run(read-only implement child) returned error: %v", err)
	}

	child = mustWorkerJob(t, h.store, "parent-job/delegation/api")
	// The child is permission-blocked (the existing behavior, preserved)...
	if child.State != string(workflow.JobBlocked) {
		t.Fatalf("child state = %q, want blocked", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	// ...AND the finalize ran: a synthetic result was attached so advanceDelegations
	// could drive the parent DAG.
	if cp.Result == nil || cp.Result.Decision != "failed" {
		t.Fatalf("child result = %+v, want a synthetic failed result (finalize must run for a delegation child)", cp.Result)
	}
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 1", got)
	}
	// The DAG advanced and escalate_human fired for the read-only-implement leg.
	if got := workerTaskState(t, h.store, "task-5"); got != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state = %q, want awaiting_human (DAG advanced for the read-only implement child)", got)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(h.notifier.calls))
	}

	// Idempotency: a second tick is a no-op (child already has a result).
	child2 := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if err := h.worker.run(ctx, child2); err != nil {
		t.Fatalf("second worker.run(child) returned error: %v", err)
	}
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events after second tick = %d, want 1 (idempotent)", got)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls after second tick = %d, want 1 (idempotent)", len(h.notifier.calls))
	}
}

// TestPreflightReadOnlyImplementNonDelegationUnaffected pins the byte-identical
// guarantee for finding 2: a NON-delegation read-only implement job (no
// ParentJobID) is permission-blocked exactly as before — no synthetic result, no
// finalize, no engine call.
func TestPreflightReadOnlyImplementNonDelegationUnaffected(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "jerryfane/gitmoot", t.TempDir())
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.CodexRuntime, "unused", []string{"implement"}, "jerryfane/gitmoot", runtime.AutonomyPolicyReadOnly)
	job := db.Job{ID: "impl-job", Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: mustJobPayload(t, workflow.JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "feature", TaskID: "task-impl", TaskTitle: "Solo implement", Sender: "lead",
	})}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	worker := defaultJobWorker(store, io.Discard)
	// A WorkflowFactory that fails proves the non-delegation permission-block path
	// never touches the engine.
	worker.WorkflowFactory = func(string) workflow.Engine {
		t.Fatal("non-delegation permission-block must not build an engine")
		return workflow.Engine{}
	}

	if err := worker.run(ctx, mustWorkerJob(t, store, "impl-job")); err != nil {
		t.Fatalf("worker.run(non-delegation implement) returned error: %v", err)
	}
	got := mustWorkerJob(t, store, "impl-job")
	if got.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", got.State)
	}
	cp, err := daemonJobPayload(got)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if cp.Result != nil {
		t.Fatalf("non-delegation permission-blocked job must NOT get a synthetic result: %+v", cp.Result)
	}
	if n := countWorkerJobEvents(t, store, "impl-job", "delegation_timeout_finalized"); n != 0 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 0 for a non-delegation job", n)
	}
}

// TestPreflightEphemeralDelegationChildAdvancesParent is the load-bearing test for
// finding 3: an EPHEMERAL delegation child whose pre-flight fails goes through the
// ephemeral wrapper at run() (~2083-2093) — an `ephemeral_worker_failed` event +
// finishQueuedJob(JobFailed) + postJobResultComment + a cleanupTempWorker defer —
// which is NOT byte-identical routing to the other finishQueuedJob sites. Assert
// that this wrapper still finalizes the delegation child (synthetic result + DAG
// advance + failure_policy).
func TestPreflightEphemeralDelegationChildAdvancesParent(t *testing.T) {
	h := newPreflightHarness(t, "escalate_human")
	// Mark the failing leg ephemeral. startEphemeralWorker calls the harness's
	// failing CheckoutValidator (the #409 pre-flight trigger) BEFORE starting any
	// runtime, so run() takes the ephemeral failure branch -> finishQueuedJob.
	markWorkerJobEphemeral(t, h.store, "parent-job/delegation/api", &workflow.EphemeralSpec{
		Runtime:      runtime.CodexRuntime,
		Capabilities: []string{"review"},
	})

	h.runChildTick(t)

	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if child.State != string(workflow.JobFailed) {
		t.Fatalf("child state = %q, want failed", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	// The synthetic result proves the ephemeral wrapper's finishQueuedJob still
	// finalized the result-less delegation child.
	if cp.Result == nil || cp.Result.Decision != "failed" {
		t.Fatalf("child result = %+v, want a synthetic failed result", cp.Result)
	}
	// The ephemeral wrapper recorded its distinguishing event AND the finalize ran.
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "ephemeral_worker_failed"); got != 1 {
		t.Fatalf("ephemeral_worker_failed events = %d, want 1 (the ephemeral wrapper path was taken)", got)
	}
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 1", got)
	}
	// The DAG advanced and escalate_human fired.
	if got := workerTaskState(t, h.store, "task-5"); got != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state = %q, want awaiting_human (DAG advanced for the ephemeral child)", got)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(h.notifier.calls))
	}
}

// permissionErrorAdapter is a DeliveryAdapter whose Deliver returns a runtime
// PERMISSION error (matched by runtimePermissionFailure), staging the MID-RUN
// permission failure: Mailbox.Run claims the child (queued->running), calls
// Deliver, gets this error, fails the job, and surfaces the error from RunJob —
// exactly how a writable implement child whose sandbox denies a write mid-run
// reaches handleRunJobError.
type permissionErrorAdapter struct{}

func (permissionErrorAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	return runtime.Result{}, errors.New("runtime write permissions denied: read-only file system")
}

// TestMidRunPermissionBlockedDelegationChildAdvancesParent is the load-bearing
// regression for the MID-RUN sibling of #409: a WRITABLE `implement` delegation
// child that passes pre-flight and reaches engine.RunJob, then whose runtime fails
// mid-run with a PERMISSION error (read-only FS / sandbox denies write), is routed
// by handleRunJobError through markJobPermissionBlocked (running/failed -> blocked)
// and RETURNS EARLY. Before the fix that early return never reached the ParentJobID
// finalize branch, so the child stranded JobBlocked with Result == nil,
// advanceDelegations never ran, and the parent DAG stranded — the exact #409
// symptom for the mid-run permission case. With the fix the child is routed through
// the SAME finalize helper (a synthetic result + DAG advance) so escalate_human
// fires.
func TestMidRunPermissionBlockedDelegationChildAdvancesParent(t *testing.T) {
	ctx := context.Background()
	// The failing leg delegates `implement`; its agent (api) stays WRITABLE (the
	// harness's default AutonomyPolicyAuto), so readOnlyImplementationBlocked is
	// false and the child reaches RunJob rather than the pre-flight read-only
	// short-circuit.
	h := newPreflightHarnessForAction(t, "escalate_human", "implement")
	// Pre-flight succeeds (checkout + adapter bring-up) so the failure happens
	// INSIDE engine.RunJob's Mailbox.Run -> Deliver, mid-run.
	h.worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return h.checkout, nil
	}
	h.worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return permissionErrorAdapter{}, nil
	}

	h.runChildTick(t)

	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	// The child landed BLOCKED via the mid-run permission branch (markJobPermissionBlocked).
	if child.State != string(workflow.JobBlocked) {
		t.Fatalf("child state = %q, want blocked", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	// The synthetic result is the proof the finalize helper ran for the result-less,
	// mid-run permission-blocked child so advanceDelegations could drive the parent.
	if cp.Result == nil || cp.Result.Decision != "failed" {
		t.Fatalf("child result = %+v, want a synthetic failed result", cp.Result)
	}
	// The finalize bridge ran exactly once.
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 1", got)
	}
	// The DAG advanced and escalate_human fired: the shared parent task is paused
	// awaiting a human and the human was notified once with the resume context.
	if got := workerTaskState(t, h.store, "task-5"); got != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state = %q, want awaiting_human (DAG advanced for the mid-run permission-blocked child)", got)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(h.notifier.calls))
	}
	if c := h.notifier.calls[0]; c.CoordinatorJobID != "parent-job" || c.DelegationID != "api" {
		t.Fatalf("notifier request = %+v, want coordinator parent-job / delegation api", c)
	}

	// Idempotency: a second worker tick over the now-finalized blocked child is a
	// no-op — the child already has a result, so finalize re-enters cleanly with no
	// second finalize and no second notification.
	child2 := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if err := h.worker.run(ctx, child2); err != nil {
		t.Fatalf("second worker.run(child) returned error: %v", err)
	}
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events after second tick = %d, want 1 (idempotent)", got)
	}
	if len(h.notifier.calls) != 1 {
		t.Fatalf("notifier calls after second tick = %d, want 1 (idempotent)", len(h.notifier.calls))
	}
}

// TestMidRunPermissionBlockedDelegationChildBlockParent proves the mid-run
// permission finalize drives a NON-escalate_human policy too: under block_parent the
// finalized child blocks the shared parent task (the policy fired via the same
// advanceDelegations path), distinguishing it from escalate_human's awaiting_human.
func TestMidRunPermissionBlockedDelegationChildBlockParent(t *testing.T) {
	h := newPreflightHarnessForAction(t, "block_parent", "implement")
	h.worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return h.checkout, nil
	}
	h.worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return permissionErrorAdapter{}, nil
	}

	h.runChildTick(t)

	child := mustWorkerJob(t, h.store, "parent-job/delegation/api")
	if child.State != string(workflow.JobBlocked) {
		t.Fatalf("child state = %q, want blocked", child.State)
	}
	cp, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	if cp.Result == nil || cp.Result.Decision != "failed" {
		t.Fatalf("child result = %+v, want a synthetic failed result", cp.Result)
	}
	if got := countWorkerJobEvents(t, h.store, "parent-job/delegation/api", "delegation_timeout_finalized"); got != 1 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 1", got)
	}
	if got := workerTaskState(t, h.store, "task-5"); got != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked (block_parent fired for the mid-run permission-blocked child)", got)
	}
}

// TestMidRunPermissionBlockedNonDelegationUnaffected pins the byte-identical
// guarantee: a SOLO writable `implement` job (no ParentJobID) whose runtime fails
// mid-run with the same permission error is permission-blocked exactly as before —
// no synthetic result, no finalize event, and the WorkflowFactory is NOT rebuilt by
// the finalize helper (the engine that ran RunJob is the only engine built).
func TestMidRunPermissionBlockedNonDelegationUnaffected(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "jerryfane/gitmoot", checkout)
	// Writable implement agent so the job reaches RunJob (not the read-only block).
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "jerryfane/gitmoot")
	job := db.Job{ID: "impl-job", Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: mustJobPayload(t, workflow.JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "feature", TaskID: "task-impl", TaskTitle: "Solo implement", Sender: "lead",
	})}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return permissionErrorAdapter{}, nil
	}
	// The WorkflowFactory IS built once (RunJob needs an engine), so a panicking
	// factory would over-fire. Count builds instead and assert the finalize helper
	// did NOT trigger a second build for this non-delegation job.
	var factoryBuilds int
	realEngine := daemonWorkflowEngine(store, github.NewClient(checkout), checkout, "")
	worker.WorkflowFactory = func(string) workflow.Engine {
		factoryBuilds++
		return realEngine
	}

	if err := worker.run(ctx, mustWorkerJob(t, store, "impl-job")); err != nil {
		t.Fatalf("worker.run(non-delegation implement) returned error: %v", err)
	}
	got := mustWorkerJob(t, store, "impl-job")
	if got.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", got.State)
	}
	cp, err := daemonJobPayload(got)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if cp.Result != nil {
		t.Fatalf("non-delegation permission-blocked job must NOT get a synthetic result: %+v", cp.Result)
	}
	if n := countWorkerJobEvents(t, store, "impl-job", "delegation_timeout_finalized"); n != 0 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 0 for a non-delegation job", n)
	}
	// Exactly one engine build (RunJob's), proving the finalize helper short-circuited
	// for the non-delegation job rather than rebuilding the engine to advance a DAG.
	if factoryBuilds != 1 {
		t.Fatalf("WorkflowFactory builds = %d, want 1 (finalize must not rebuild for a non-delegation job)", factoryBuilds)
	}
}

// markWorkerJobEphemeral attaches an EphemeralSpec to a seeded delegation child's
// stored payload so run() takes the ephemeral materialization branch.
func markWorkerJobEphemeral(t *testing.T, store *db.Store, jobID string, spec *workflow.EphemeralSpec) {
	t.Helper()
	job := mustWorkerJob(t, store, jobID)
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload(%s) returned error: %v", jobID, err)
	}
	payload.Ephemeral = spec
	if err := store.UpdateJobPayload(context.Background(), jobID, mustJobPayload(t, payload)); err != nil {
		t.Fatalf("UpdateJobPayload(%s ephemeral) returned error: %v", jobID, err)
	}
}
