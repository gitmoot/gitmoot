package workflow

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// supersedeAdvanceOwnership seeds a superseded child and returns its observed
// lifecycle generation. Every test below starts from this shape because it is what
// the closed-PR sweep hands the recovery.
func supersedeAdvanceOwnershipChild(t *testing.T, store *db.Store, id string) db.Job {
	t.Helper()
	ctx := context.Background()
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	insertQueuedJob(t, store, db.Job{ID: id, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	observed := mustJob(t, store, id)
	if _, err := store.TransitionJobStateWithEventAtGeneration(ctx, id, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: id, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	return observed
}

func ownAdvance(t *testing.T, store *db.Store, jobID string, token string, expires time.Time) {
	t.Helper()
	owned, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: db.SupersedeAdvanceLockKeyPrefix + jobID,
		OwnerJobID:  jobID,
		OwnerToken:  token,
		ExpiresAt:   expires.Format(time.RFC3339Nano),
	}, time.Now().UTC())
	if err != nil || !owned {
		t.Fatalf("acquire advance ownership owned=%v err=%v", owned, err)
	}
}

// TestRetryIsRefusedWhileAnAdvanceOwnsTheJob pins the PREVENTION half in the plain
// retry arm: the exclusion rides in the transition's own statement, so a retry
// cannot roll the lifecycle over while a recovery owns the parent advance.
//
// MUTATION PROOF: drop noLiveSupersedeAdvanceLockSQL from
// TransitionJobStatePayloadWithEventUnlessAdvanceOwned and the retry succeeds.
func TestRetryIsRefusedWhileAnAdvanceOwnsTheJob(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	const child = "workflow-advance-owned"
	supersedeAdvanceOwnershipChild(t, store, child)
	ownAdvance(t, store, child, "token-live", time.Now().UTC().Add(SupersedeAdvanceLeaseTTL))

	if _, err := RetryJob(ctx, store, child); err == nil {
		t.Fatal("a retry rolled the lifecycle over while the parent advance was owned")
	} else if !errorMentions(err, "parent-advance") {
		t.Fatalf("retry error = %v, want one naming the owned advance", err)
	}
	if state := mustJob(t, store, child).State; state != string(JobFailed) {
		t.Fatalf("child state = %q, want the owned lifecycle untouched", state)
	}

	// Explicit release, which is what a finished advance does: retries resume at once
	// instead of waiting out a lease.
	if released, err := store.ReleaseResourceLock(ctx, db.SupersedeAdvanceLockKeyPrefix+child, child, "token-live"); err != nil || !released {
		t.Fatalf("release ownership released=%v err=%v", released, err)
	}
	if _, err := RetryJob(ctx, store, child); err != nil {
		t.Fatalf("retry after the advance released ownership: %v", err)
	}
}

// TestRetryWithDismissedTaskIsRefusedWhileAnAdvanceOwnsTheJob is the SECOND retry
// arm. It re-queues exactly like the first, so leaving it unguarded would leave the
// class open on the path a real implement job actually takes.
//
// MUTATION PROOF: drop the predicate from
// TransitionJobStatePayloadWithEventAndTaskTransition and the retry succeeds while
// the advance is owned.
func TestRetryWithDismissedTaskIsRefusedWhileAnAdvanceOwnsTheJob(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	const child = "workflow-advance-owned-task"
	supersedeAdvanceOwnershipChild(t, store, child)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", Branch: "task-7", State: string(TaskDismissed),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	ownAdvance(t, store, child, "token-live", time.Now().UTC().Add(SupersedeAdvanceLeaseTTL))

	if _, err := RetryJob(ctx, store, child); err == nil {
		t.Fatal("the dismissed-task retry arm rolled the lifecycle over while the advance was owned")
	} else if !errorMentions(err, "parent-advance") {
		t.Fatalf("retry error = %v, want one naming the owned advance", err)
	}
	if state := mustJob(t, store, child).State; state != string(JobFailed) {
		t.Fatalf("child state = %q, want the owned lifecycle untouched", state)
	}
	if task, err := store.GetTask(ctx, "task-7"); err != nil {
		t.Fatalf("GetTask: %v", err)
	} else if task.State != string(TaskDismissed) {
		t.Fatalf("task state = %q, want the dismissed record untouched: the two writes commit together", task.State)
	}

	if released, err := store.ReleaseResourceLock(ctx, db.SupersedeAdvanceLockKeyPrefix+child, child, "token-live"); err != nil || !released {
		t.Fatalf("release ownership released=%v err=%v", released, err)
	}
	if _, err := RetryJob(ctx, store, child); err != nil {
		t.Fatalf("retry after release: %v", err)
	}
	if task, err := store.GetTask(ctx, "task-7"); err != nil {
		t.Fatalf("GetTask: %v", err)
	} else if task.State == string(TaskDismissed) {
		t.Fatal("the successful retry did not recover the dismissed task; the success path regressed")
	}
}

// TestAbandonedAdvanceOwnershipStopsBlockingRetries keeps prevention from becoming a
// wedge. A killed pass leaves its lock behind; the lease is what recovers it, and
// nothing renews a dead owner's lease.
func TestAbandonedAdvanceOwnershipStopsBlockingRetries(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	const child = "workflow-advance-abandoned"
	supersedeAdvanceOwnershipChild(t, store, child)
	ownAdvance(t, store, child, "token-dead", time.Now().UTC().Add(SupersedeAdvanceLeaseTTL))
	if _, err := RetryJob(ctx, store, child); err == nil {
		t.Fatal("a live lease did not block the retry")
	}

	// What a killed pass looks like once its lease has run out.
	if _, err := store.HeartbeatResourceLock(ctx, db.SupersedeAdvanceLockKeyPrefix+child, "token-dead", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	if _, err := RetryJob(ctx, store, child); err != nil {
		t.Fatalf("retry after the abandoned owner's lease expired: %v", err)
	}
}

// TestAdvanceLeaseIsRenewedThroughASlowAdvance is the LEASE-EXPIRY half of the
// class, driven through the production entry point. The lease is short on purpose,
// so a slow advance must survive by RENEWING rather than by being given a bigger
// budget: the hook expires the lease from underneath a running advance at the first
// barrier, exactly as a slow allocation outliving one window would, and the advance
// must still own the job at the next barrier.
//
// MUTATION PROOF: delete the renewSupersedeAdvanceLease call from
// assertSupersedeAdvanceAnchor and the interleaved retry wins.
func TestAdvanceLeaseIsRenewedThroughASlowAdvance(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: "continue"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job): %v", err)
	}
	const child = "parent-job/delegation/api"
	observed := mustJob(t, store, child)
	if _, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	stampSyntheticResult(t, store, child, "superseded")

	var (
		retryErr     error
		retryAttempt bool
		expired      bool
	)
	supersedeAdvanceBarrierHook = func(hookCtx context.Context, at string) {
		if !expired {
			// The slow phase: this pass's lease lapses while it is still working.
			expired = true
			if _, err := store.HeartbeatResourceLock(hookCtx, db.SupersedeAdvanceLockKeyPrefix+child, advanceOwnerToken(t, store, child), time.Now().UTC().Add(-time.Hour)); err != nil {
				t.Errorf("expire the live owner's lease: %v", err)
			}
			return
		}
		if retryAttempt {
			return
		}
		// A retry racing the renewed lease must still lose.
		retryAttempt = true
		_, retryErr = RetryJob(hookCtx, store, child)
	}
	t.Cleanup(func() { supersedeAdvanceBarrierHook = nil })

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !advanced {
		t.Fatal("a live-but-slow advance was treated as abandoned; the lease was not renewed")
	}
	if !retryAttempt {
		t.Fatal("no retry was interleaved; the test proves nothing")
	}
	if retryErr == nil {
		t.Fatal("a retry won against a renewed lease: an expired-but-live owner lost its advance")
	}
	if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceConfirmed); got != 1 {
		t.Fatalf("%s events = %d, want 1: the anchored advance did not complete", JobEventSupersedeAdvanceConfirmed, got)
	}
}

// TestAdvanceOwnershipIsReleasedOnEveryExit is the success-path control for the
// primitive itself: an advance that finishes must not leave a lock behind, or every
// later retry pays a full lease for work that already ended.
func TestAdvanceOwnershipIsReleasedOnEveryExit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store := openEngineStoreAt(t, path)
	engine := testEngine(store)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: "continue"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job): %v", err)
	}
	const child = "parent-job/delegation/api"
	observed := mustJob(t, store, child)
	if _, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	stampSyntheticResult(t, store, child, "superseded")

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil || !advanced {
		t.Fatalf("advanceSupersededChildAtGeneration advanced=%v err=%v", advanced, err)
	}
	if _, err := store.GetResourceLock(ctx, db.SupersedeAdvanceLockKeyPrefix+child); err == nil {
		t.Fatal("a finished advance kept its ownership lock; retries would pay a lease for work that ended")
	} else if err != sql.ErrNoRows {
		t.Fatalf("GetResourceLock: %v", err)
	}
	if _, err := RetryJob(ctx, store, child); err != nil {
		t.Fatalf("retry after a completed advance: %v", err)
	}
}

// TestConcurrentAdvancePassesDoNotBothOwnTheJob pins the mutual exclusion the retry
// predicate depends on: ownership is the same primitive on both sides, so a second
// recovery pass cannot advance a job the first is advancing.
func TestConcurrentAdvancePassesDoNotBothOwnTheJob(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	const child = "workflow-advance-exclusive"
	observed := supersedeAdvanceOwnershipChild(t, store, child)
	ownAdvance(t, store, child, "token-other-pass", time.Now().UTC().Add(SupersedeAdvanceLeaseTTL))

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil {
		t.Fatalf("a contended advance reported an error rather than deferring: %v", err)
	}
	if advanced {
		t.Fatal("two passes both owned the same parent advance")
	}
	if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceClaimed); got != 0 {
		t.Fatalf("%s events = %d, want 0: the losing pass claimed anyway", JobEventSupersedeAdvanceClaimed, got)
	}
}

// TestEnqueueRefusesWhenTheAdvanceLostOwnership covers the window BETWEEN the last
// barrier and the enqueue itself. The barriers cannot cover it: they run before the
// decision, and the enqueue is the irreversible act. e.enqueue is the single funnel
// both effect classes (dependent enqueue, continuation) take, so the binding lives
// there.
//
// MUTATION PROOF: remove the renewSupersedeAdvanceLease call from Engine.enqueue and
// this test goes green — which is exactly how it was found.
func TestEnqueueRefusesWhenTheAdvanceLostOwnership(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	const child = "workflow-enqueue-binding"
	observed := supersedeAdvanceOwnershipChild(t, store, child)

	anchored := engine
	anchored.supersedeAdvance = &supersedeAdvanceAnchor{
		JobID:      child,
		Generation: observed.LifecycleGeneration,
		LockKey:    db.SupersedeAdvanceLockKeyPrefix + child,
		Token:      "token-gone",
	}
	request := JobRequest{ID: "workflow-enqueue-binding/dependent", Repo: "gitmoot/gitmoot", Branch: "task-7", Agent: "impl", Action: "implement"}
	err := anchored.enqueue(ctx, request)
	var rolled supersedeAdvanceRolledBackError
	if !errors.As(err, &rolled) {
		t.Fatalf("enqueue error = %v, want a rolled-back advance: an unowned pass minted a job", err)
	}
	if _, getErr := store.GetJob(ctx, request.ID); getErr == nil {
		t.Fatal("the enqueue landed despite lost ownership; the effect is irreversible")
	}

	// SUCCESS CONTROL: with ownership held the same enqueue must go through, and an
	// ordinary caller (no anchor) must be unaffected.
	ownAdvance(t, store, child, "token-gone", time.Now().UTC().Add(SupersedeAdvanceLeaseTTL))
	if err := anchored.enqueue(ctx, request); err != nil {
		t.Fatalf("enqueue under held ownership: %v", err)
	}
	if _, getErr := store.GetJob(ctx, request.ID); getErr != nil {
		t.Fatalf("GetJob after an owned enqueue: %v", getErr)
	}
	plain := JobRequest{ID: "workflow-enqueue-plain", Repo: "gitmoot/gitmoot", Branch: "task-7", Agent: "impl", Action: "implement"}
	if err := engine.enqueue(ctx, plain); err != nil {
		t.Fatalf("an ordinary enqueue with no anchor was affected: %v", err)
	}
}

func advanceOwnerToken(t *testing.T, store *db.Store, jobID string) string {
	t.Helper()
	lock, err := store.GetResourceLock(context.Background(), db.SupersedeAdvanceLockKeyPrefix+jobID)
	if err != nil {
		t.Fatalf("GetResourceLock: %v", err)
	}
	return lock.OwnerToken
}
