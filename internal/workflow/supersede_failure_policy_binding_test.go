package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

type countingEscalationNotifier struct {
	calls int
}

func (n *countingEscalationNotifier) NotifyEscalation(ctx context.Context, request EscalationRequest) error {
	n.calls++
	return nil
}

// seedFailurePolicyTree fans out one delegation under the named failure policy,
// fails the child, and stamps its synthetic result — the shape a closed-PR
// supersession recovery inherits when it owes the parent an advance.
func seedFailurePolicyTree(t *testing.T, store *db.Store, engine Engine, policy string) (string, db.Job) {
	t.Helper()
	ctx := context.Background()
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", Branch: "task-7", GoalID: "g1",
		Title: "impl", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: policy},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job): %v", err)
	}
	const child = "parent-job/delegation/api"
	observed := mustJob(t, store, child)
	// The leg FAILS: that is what makes the parent's failure policy fire.
	if _, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	stampSyntheticResult(t, store, child, "leg failed")
	return child, observed
}

// expireLeaseAndWinRetry is the production ordering the directive names: the pass
// stalls past its lease, and a retry legally re-queues the child at generation N+1.
//
// IT MUST FIRE AT THE EFFECT'S OWN PRE-WRITE SEAM, not at the failure-policy
// barrier. An interleaving placed at the barrier is refused by that barrier's own
// renewal, so the effect is never attempted and a test written that way passes even
// with the commit binding deleted — measured: three mutants survived the first
// version of these tests for exactly that reason.
func expireLeaseAndWinRetry(t *testing.T, store *db.Store, child string, won *bool) func(context.Context, string) {
	t.Helper()
	fired := false
	return func(hookCtx context.Context, taskID string) {
		if fired {
			return
		}
		fired = true
		lock, err := store.GetResourceLock(hookCtx, db.SupersedeAdvanceLockKeyPrefix+child)
		if err != nil {
			t.Errorf("GetResourceLock: %v", err)
			return
		}
		if _, err := store.HeartbeatResourceLock(hookCtx, lock.ResourceKey, lock.OwnerToken, time.Now().UTC().Add(-time.Hour)); err != nil {
			t.Errorf("expire the lease: %v", err)
			return
		}
		if _, err := RetryJob(hookCtx, store, child); err != nil {
			t.Errorf("retry against an expired lease: %v", err)
			return
		}
		*won = true
	}
}

// TestBlockParentIsRefusedAfterTheLeaseExpiresAndARetryWins is the block_parent half
// of the remaining P1. The barrier at the top of the failure-policy loop cannot
// protect the write: the pass can stall between them. The block must therefore be
// bound to live ownership INSIDE its own transaction.
//
// MUTATION PROOF: route blockTask through BlockTaskWithEvent (dropping the
// in-transaction predicate) and the stale block commits.
func TestBlockParentIsRefusedAfterTheLeaseExpiresAndARetryWins(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	child, observed := seedFailurePolicyTree(t, store, engine, "block_parent")

	retryWon := false
	blockTaskPreWriteHook = expireLeaseAndWinRetry(t, store, child, &retryWon)
	t.Cleanup(func() { blockTaskPreWriteHook = nil })

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !retryWon {
		t.Fatal("the retry never committed; the ordering under test did not happen")
	}
	if advanced {
		t.Fatal("a pass that lost its lease still reported a completed advance")
	}
	// ZERO stale effects: the parent task never moved, and no block event exists.
	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskImplementing) {
		t.Fatalf("parent task state = %q, want implementing: a stale pass blocked the parent", task.State)
	}
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range events {
		if event.ToState == string(TaskBlocked) || event.Kind == "workflow_blocked" {
			t.Fatalf("stale block event recorded: %+v", event)
		}
	}
}

// TestEscalateHumanIsRefusedAfterTheLeaseExpiresAndARetryWins is the escalate_human
// half. Its effects are wider — a task move to awaiting_human, a durable escalation
// event, and a human notification — so all three must be absent.
//
// MUTATION PROOF: route setTaskStateResolved through the unbound
// UpsertTaskUnlessStates and the stale pause lands, with its event and its
// notification.
func TestEscalateHumanIsRefusedAfterTheLeaseExpiresAndARetryWins(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &countingEscalationNotifier{}
	engine.EscalationNotifier = notifier
	child, observed := seedFailurePolicyTree(t, store, engine, "escalate_human")

	retryWon := false
	taskStatePreWriteHook = expireLeaseAndWinRetry(t, store, child, &retryWon)
	t.Cleanup(func() { taskStatePreWriteHook = nil })

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !retryWon {
		t.Fatal("the retry never committed; the ordering under test did not happen")
	}
	if advanced {
		t.Fatal("a pass that lost its lease still reported a completed advance")
	}
	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State == string(TaskAwaitingHuman) {
		t.Fatal("a stale pass paused the parent for a human")
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0: a stale pass opened a human round", escalationRequestedEvent, got)
	}
	if notifier.calls != 0 {
		t.Fatalf("escalation notifications = %d, want 0: a stale pass called a human", notifier.calls)
	}
}

// TestFailurePolicyEffectsStillCommitUnderALiveLease is the success control for both
// arms. The binding must not reject VALID work: with the lease held throughout, the
// block still blocks and the pause still pauses, notification included.
func TestFailurePolicyEffectsStillCommitUnderALiveLease(t *testing.T) {
	for _, tc := range []struct {
		policy string
		verify func(t *testing.T, store *db.Store, notifier *countingEscalationNotifier)
	}{
		{"block_parent", func(t *testing.T, store *db.Store, notifier *countingEscalationNotifier) {
			task, err := store.GetTask(context.Background(), "task-7")
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.State != string(TaskBlocked) {
				t.Fatalf("parent task state = %q, want blocked: a legitimate block was refused", task.State)
			}
		}},
		{"escalate_human", func(t *testing.T, store *db.Store, notifier *countingEscalationNotifier) {
			task, err := store.GetTask(context.Background(), "task-7")
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.State != string(TaskAwaitingHuman) {
				t.Fatalf("parent task state = %q, want awaiting_human: a legitimate pause was refused", task.State)
			}
			if notifier.calls != 1 {
				t.Fatalf("escalation notifications = %d, want 1", notifier.calls)
			}
		}},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			engine := testEngine(store)
			notifier := &countingEscalationNotifier{}
			engine.EscalationNotifier = notifier
			child, observed := seedFailurePolicyTree(t, store, engine, tc.policy)

			advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
			// A fired failure policy reports itself as a delegation-policy outcome
			// (BlockedError / AwaitingHumanError). That IS the success path here: the
			// effect committed and the recovery records it.
			if err != nil && !isDelegationPolicyOutcome(err) {
				t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
			}
			if !advanced {
				t.Fatal("a fully owned advance was refused; the binding rejects valid work")
			}
			tc.verify(t, store, notifier)
			if _, err := store.GetResourceLock(ctx, db.SupersedeAdvanceLockKeyPrefix+child); err == nil {
				t.Fatal("the completed advance kept its lease")
			}
		})
	}
}

// TestEscalateHumanRoundOpensAtomically covers the CRASH-AFTER-TRANSITION ordering:
// the two writes that open a human round must be one durable operation, or a crash
// between them strands a task in awaiting_human that no open round explains — and
// nothing re-opens it, because the round guard reads the event, not the task.
//
// The crash is simulated the only way a test can: the seam fires between the
// resolution and the write, and the ROUND-OPEN EVENT COUNT is compared against the
// TASK STATE. Either both moved or neither did.
//
// MUTATION PROOF: write the task and the event in two statements (route through
// UpsertTaskUnlessStatesIfAdvanceOwned + AddJobEvent) and inject a failure between
// them; the task strands in awaiting_human with zero round events.
func TestEscalateHumanRoundOpensAtomically(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &countingEscalationNotifier{}
	engine.EscalationNotifier = notifier
	child, observed := seedFailurePolicyTree(t, store, engine, "escalate_human")

	// Force the round-open write to be REFUSED at the store, which is what a crashed
	// or superseded write looks like from the caller's side: the transaction does not
	// commit. Ownership is dropped in the seam, so the bound write refuses.
	fired := false
	taskStatePreWriteHook = func(hookCtx context.Context, taskID string) {
		if fired {
			return
		}
		fired = true
		lock, err := store.GetResourceLock(hookCtx, db.SupersedeAdvanceLockKeyPrefix+child)
		if err != nil {
			t.Errorf("GetResourceLock: %v", err)
			return
		}
		if released, err := store.ReleaseResourceLock(hookCtx, lock.ResourceKey, child, lock.OwnerToken); err != nil || !released {
			t.Errorf("drop ownership released=%v err=%v", released, err)
		}
	}
	t.Cleanup(func() { taskStatePreWriteHook = nil })

	if _, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration); err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !fired {
		t.Fatal("the round-open seam never fired; the ordering under test did not happen")
	}

	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	rounds := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent)
	paused := task.State == string(TaskAwaitingHuman)
	if paused != (rounds > 0) {
		t.Fatalf("task awaiting_human=%v but round events=%d: the two halves of a round-open are not atomic", paused, rounds)
	}
	if paused {
		t.Fatal("a write refused for lost ownership still paused the parent")
	}
	// ZERO announcements: every announcement follows the commit, so a refused
	// round-open announces nothing.
	if notifier.calls != 0 {
		t.Fatalf("escalation notifications = %d, want 0: an unopened round called a human", notifier.calls)
	}
}

// TestEscalateHumanRoundOpenIsAtomicOnTheOrdinaryPath is the same invariant with NO
// supersession anchor: the atomicity is a property of the round-open itself, not of
// the ownership binding, so an ordinary pause must also never strand one half.
func TestEscalateHumanRoundOpenIsAtomicOnTheOrdinaryPath(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &countingEscalationNotifier{}
	engine.EscalationNotifier = notifier
	child, _ := seedFailurePolicyTree(t, store, engine, "escalate_human")

	// The ordinary production entry with no anchor at all: advancing the settled
	// child is what fires the coordinator's failure policy.
	if err := engine.AdvanceJob(ctx, child); err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("AdvanceJob: %v", err)
	}

	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	rounds := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent)
	if task.State != string(TaskAwaitingHuman) {
		t.Fatalf("parent task state = %q, want awaiting_human: a legitimate ordinary pause was refused", task.State)
	}
	if rounds != 1 {
		t.Fatalf("%s events = %d, want exactly 1", escalationRequestedEvent, rounds)
	}
	if notifier.calls != 1 {
		t.Fatalf("escalation notifications = %d, want 1", notifier.calls)
	}
}

// TestEscalateHumanDoesNotAnnounceAfterResumingSuperseded covers the
// RESUME-AFTER-SUPERSEDE ordering: the pass stalls past its lease, a retry commits
// generation N+1, and only THEN does the old pass reach the round-open. Nothing it
// writes or announces may survive, and the retried lifecycle must be left intact.
//
// MUTATION PROOF: drop the ownership assertion from the round-open write and the
// obsolete generation records a round and calls a human.
func TestEscalateHumanDoesNotAnnounceAfterResumingSuperseded(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &countingEscalationNotifier{}
	engine.EscalationNotifier = notifier
	child, observed := seedFailurePolicyTree(t, store, engine, "escalate_human")

	retryWon := false
	taskStatePreWriteHook = expireLeaseAndWinRetry(t, store, child, &retryWon)
	t.Cleanup(func() { taskStatePreWriteHook = nil })

	if _, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration); err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !retryWon {
		t.Fatal("the retry never committed; the ordering under test did not happen")
	}
	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State == string(TaskAwaitingHuman) {
		t.Fatal("a superseded generation paused the parent for a human")
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationRequestedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0: an obsolete generation opened a human round", escalationRequestedEvent, got)
	}
	if notifier.calls != 0 {
		t.Fatalf("escalation notifications = %d, want 0: an obsolete generation announced", notifier.calls)
	}
	// The retry's lifecycle is untouched by the loser.
	if state := mustJob(t, store, child).State; state != string(JobQueued) {
		t.Fatalf("child state = %q, want queued: the retry's re-queue was disturbed", state)
	}
}

// TestAskGateRoundOpensAtomically covers the SIBLING round-open the class audit
// found: the ask gate opens a human round with the same two writes and the same
// announcements, so it gets the same atomic, ownership-bound operation. Without this
// the fix would close one site and leave its twin reachable.
func TestAskGateRoundOpensAtomically(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &countingEscalationNotifier{}
	engine.EscalationNotifier = notifier
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-ask", RepoFullName: "gitmoot/gitmoot", Branch: "task-ask", GoalID: "g1",
		Title: "ask", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "ask-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-ask", TaskID: "task-ask", LeadAgent: "coord",
		Result: &AgentResult{
			Decision:       "blocked",
			Summary:        "needs a human",
			HumanQuestions: []HumanQuestion{{ID: "q1", Prompt: "which branch?"}},
		},
	})

	paused, err := engine.pauseAwaitingHumanAnswer(ctx, mustJob(t, store, "ask-job"), mustPayload(t, store, "ask-job"), taskRef{ID: "task-ask", Repo: "gitmoot/gitmoot", Branch: "task-ask"})
	if err != nil {
		t.Fatalf("pauseAwaitingHumanAnswer: %v", err)
	}
	if !paused {
		t.Fatal("the ask gate did not pause; the success path regressed")
	}
	task, err := store.GetTask(ctx, "task-ask")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	rounds := countWorkflowJobEvents(t, store, "ask-job", escalationRequestedEvent)
	if task.State != string(TaskAwaitingHuman) || rounds != 1 {
		t.Fatalf("ask round: task=%q rounds=%d, want awaiting_human and exactly 1", task.State, rounds)
	}
	if notifier.calls != 1 {
		t.Fatalf("escalation notifications = %d, want 1", notifier.calls)
	}
}

func mustPayload(t *testing.T, store *db.Store, jobID string) JobPayload {
	t.Helper()
	payload, err := unmarshalPayload(mustJob(t, store, jobID).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(%s): %v", jobID, err)
	}
	return payload
}

// TestAnchoredTaskStateWriteIsRefusedWhenOwnershipIsLost pins the SHARED CHOKEPOINT
// every other anchored task-state move funnels through. Enumerated call sites
// reachable from an anchored AdvanceJob: the merge gate's ready_to_merge and merged
// writes and the awaiting_human_merge park (engine_routing_merge.go), the reviewing
// and pull_request_open writes (engine_pr_lifecycle.go), the changes_requested write
// (engine_run_budgets.go), and the escalation-resolution planned writes
// (engine_escalation_resume.go). All of them reach the store through
// setTaskState → persistTaskStateOwned, so the binding is proven once, here, rather
// than by building a fixture per site.
//
// MUTATION PROOF: route persistTaskStateOwned through the unbound
// UpsertTaskUnlessStates and this write lands for a lifecycle that has moved.
func TestAnchoredTaskStateWriteIsRefusedWhenOwnershipIsLost(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	const child = "workflow-anchored-taskwrite"
	observed := supersedeAdvanceOwnershipChild(t, store, child)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", Branch: "task-7", GoalID: "g1",
		Title: "impl", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	ownAdvance(t, store, child, "token-live", time.Now().UTC().Add(SupersedeAdvanceLeaseTTL))

	anchored := engine
	anchored.supersedeAdvance = &supersedeAdvanceAnchor{
		JobID:      child,
		Generation: observed.LifecycleGeneration,
		LockKey:    db.SupersedeAdvanceLockKeyPrefix + child,
		Token:      "token-live",
	}
	ref := taskRef{ID: "task-7", Repo: "gitmoot/gitmoot", Branch: "task-7"}

	// SUCCESS CONTROL FIRST: a held lease must not block a legitimate move.
	if err := anchored.setTaskState(ctx, ref, TaskReviewing); err != nil {
		t.Fatalf("anchored task-state write under a held lease: %v", err)
	}
	if task, err := store.GetTask(ctx, "task-7"); err != nil {
		t.Fatalf("GetTask: %v", err)
	} else if task.State != string(TaskReviewing) {
		t.Fatalf("task state = %q, want reviewing: the binding rejected valid work", task.State)
	}

	// Ownership is lost in the write's own pre-write window.
	fired := false
	taskStatePreWriteHook = func(hookCtx context.Context, taskID string) {
		if fired {
			return
		}
		fired = true
		if released, err := store.ReleaseResourceLock(hookCtx, db.SupersedeAdvanceLockKeyPrefix+child, child, "token-live"); err != nil || !released {
			t.Errorf("drop ownership released=%v err=%v", released, err)
		}
	}
	t.Cleanup(func() { taskStatePreWriteHook = nil })

	err := anchored.setTaskState(ctx, ref, TaskReadyToMerge)
	var rolled supersedeAdvanceRolledBackError
	if !errors.As(err, &rolled) {
		t.Fatalf("setTaskState error = %v, want a rolled-back advance", err)
	}
	if task, err := store.GetTask(ctx, "task-7"); err != nil {
		t.Fatalf("GetTask: %v", err)
	} else if task.State != string(TaskReviewing) {
		t.Fatalf("task state = %q, want reviewing: a pass that lost ownership still moved the task", task.State)
	}
}

// TestTasklessRoundOpenIsRefusedWhenOwnershipIsLost covers the coordinator with NO
// task: it still owes a durable round record, so that append is ownership-bound too.
// Without this the taskless branch of openHumanRound is an unguarded hole.
//
// MUTATION PROOF: route the taskless branch through the plain AddJobEvent and the
// obsolete generation records a round.
func TestTasklessRoundOpenIsRefusedWhenOwnershipIsLost(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	const child = "workflow-taskless-round"
	observed := supersedeAdvanceOwnershipChild(t, store, child)

	anchored := engine
	anchored.supersedeAdvance = &supersedeAdvanceAnchor{
		JobID:      child,
		Generation: observed.LifecycleGeneration,
		LockKey:    db.SupersedeAdvanceLockKeyPrefix + child,
		Token:      "token-gone",
	}
	// No taskRef at all: the round-open has only the event to write.
	_, _, err := anchored.openHumanRound(ctx, taskRef{}, child, "", EscalationRecord{Reason: "obsolete round"},
		func(rec EscalationRecord) string { return rec.Reason })
	var rolled supersedeAdvanceRolledBackError
	if !errors.As(err, &rolled) {
		t.Fatalf("openHumanRound error = %v, want a rolled-back advance", err)
	}
	if got := countWorkflowJobEvents(t, store, child, escalationRequestedEvent); got != 0 {
		t.Fatalf("%s events = %d, want 0: a taskless round-open landed without ownership", escalationRequestedEvent, got)
	}

	// SUCCESS CONTROL: with the lease held the same taskless round-open commits.
	ownAdvance(t, store, child, "token-gone", time.Now().UTC().Add(SupersedeAdvanceLeaseTTL))
	if _, announce, err := anchored.openHumanRound(ctx, taskRef{}, child, "", EscalationRecord{Reason: "owned round"},
		func(rec EscalationRecord) string { return rec.Reason }); err != nil || !announce {
		t.Fatalf("taskless round-open under a held lease: %v", err)
	}
	if got := countWorkflowJobEvents(t, store, child, escalationRequestedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", escalationRequestedEvent, got)
	}
}
