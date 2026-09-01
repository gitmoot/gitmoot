package workflow

import (
	"context"
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
