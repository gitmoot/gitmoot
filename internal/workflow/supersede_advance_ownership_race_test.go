package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// seedSupersededDelegationChild builds the real shape the closed-PR sweep hands the
// recovery: a coordinator that fanned out one delegation, whose child is then
// superseded with a synthetic result. Both interleavings below drive production
// entry points against it — no hand-placed locks standing in for cleanup or
// renewal.
func seedSupersededDelegationChild(t *testing.T, store *db.Store, engine Engine) (string, db.Job) {
	t.Helper()
	ctx := context.Background()
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
	return child, observed
}

// TestCompetingFinalizerCleanupCannotDeleteALiveAdvanceLease is the first P1 from
// head c07aec45, driven through the PRODUCTION cleanup entry point rather than a
// direct store call. A second finalizer for the same terminal child reaches
// releaseSupersededJobResourcesAtGeneration while THIS pass is mid-advance; its
// owner-scoped DELETE must not take this pass's unexpired lease, or the retry it
// unblocks rolls the lifecycle over mid-advance.
//
// MUTATION PROOF: drop excludeLiveAdvanceLockSQL from
// ReleaseSupersededJobResourceLocksAtGeneration and this test fails while the
// existing cleanup regressions still pass.
func TestCompetingFinalizerCleanupCannotDeleteALiveAdvanceLease(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	child, observed := seedSupersededDelegationChild(t, store, engine)

	var (
		fired      bool
		cleanupRan bool
		guardHeld  bool
		lockAlive  bool
		retryErr   error
	)
	supersedeAdvanceBarrierHook = func(hookCtx context.Context, at string) {
		if fired {
			return
		}
		fired = true
		// A COMPETING finalizer for the same terminal child, through production.
		terminal := mustJob(t, store, child)
		guarded, err := releaseSupersededJobResourcesAtGeneration(hookCtx, store, terminal, abortCauseSupersede, observed.LifecycleGeneration)
		if err != nil {
			t.Errorf("competing cleanup: %v", err)
			return
		}
		cleanupRan = true
		guardHeld = guarded
		if _, lockErr := store.GetResourceLock(hookCtx, db.SupersedeAdvanceLockKeyPrefix+child); lockErr == nil {
			lockAlive = true
		}
		_, retryErr = RetryJob(hookCtx, store, child)
	}
	t.Cleanup(func() { supersedeAdvanceBarrierHook = nil })

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !cleanupRan {
		t.Fatal("the competing cleanup never ran; the test proves nothing")
	}
	if !guardHeld {
		t.Fatal("the competing cleanup lost its own generation guard; the interleaving is not the one under test")
	}
	if !lockAlive {
		t.Fatal("owner-scoped cleanup deleted another pass's live advance lease")
	}
	if retryErr == nil {
		t.Fatal("a retry won after the competing cleanup: the lease it depends on was destroyed")
	}
	if !advanced {
		t.Fatal("the owning pass lost its advance to a competing cleanup")
	}
	// EXACTLY ONE effect: the advance ran once, so the parent's continuation must
	// exist once and no duplicate child may appear.
	if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceConfirmed); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", JobEventSupersedeAdvanceConfirmed, got)
	}
}

// TestOwnerCleanupStillReleasesEveryOtherLock is the control for the exclusion: it
// is scoped to the advance-lease CLASS and to LIVE leases only, so ordinary locks
// and abandoned leases must still be swept by the same cleanup.
func TestOwnerCleanupStillReleasesEveryOtherLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	child, observed := seedSupersededDelegationChild(t, store, engine)
	terminal := mustJob(t, store, child)

	if owned, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-x", OwnerJobID: child, OwnerToken: "runtime-token",
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !owned {
		t.Fatalf("seed ordinary lock owned=%v err=%v", owned, err)
	}
	// An ABANDONED advance lease: a pass that was killed, its lease already lapsed.
	ownAdvance(t, store, child, "token-dead", time.Now().UTC().Add(-time.Hour))

	guarded, err := releaseSupersededJobResourcesAtGeneration(ctx, store, terminal, abortCauseSupersede, observed.LifecycleGeneration)
	if err != nil || !guarded {
		t.Fatalf("cleanup guarded=%v err=%v", guarded, err)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-x"); err == nil {
		t.Fatal("the exclusion widened: an ordinary resource lock survived owner cleanup")
	}
	if _, err := store.GetResourceLock(ctx, db.SupersedeAdvanceLockKeyPrefix+child); err == nil {
		t.Fatal("an abandoned advance lease survived cleanup; a crashed pass would wedge retries")
	}
}

// TestAnExpiredLeaseCannotBeResurrectedAfterARetryCommits is the second P1, in the
// exact ordering the directive names: the lease expires during a slow phase, the
// retry commits generation N+1, the old pass then attempts renewal, and only then
// would it reach an enqueue.
//
// MUTATION PROOF: drop `expires_at > ?` (or the generation clause) from
// RenewAdvanceOwnershipLease and the dead pass renews and advances.
func TestAnExpiredLeaseCannotBeResurrectedAfterARetryCommits(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	child, observed := seedSupersededDelegationChild(t, store, engine)

	var (
		fired       bool
		retryWon    bool
		expiredText string
	)
	supersedeAdvanceBarrierHook = func(hookCtx context.Context, at string) {
		if fired {
			return
		}
		fired = true
		// The slow phase outlives the lease. HeartbeatResourceLock is used only to
		// SET the clock back; nothing renews it, exactly like a stalled pass.
		token := advanceOwnerToken(t, store, child)
		if _, err := store.HeartbeatResourceLock(hookCtx, db.SupersedeAdvanceLockKeyPrefix+child, token, time.Now().UTC().Add(-time.Hour)); err != nil {
			t.Errorf("expire the lease: %v", err)
			return
		}
		// The retry now legitimately wins: an expired lease must not block an operator.
		if _, err := RetryJob(hookCtx, store, child); err != nil {
			t.Errorf("retry against an expired lease: %v", err)
			return
		}
		retryWon = true
		if lock, err := store.GetResourceLock(hookCtx, db.SupersedeAdvanceLockKeyPrefix+child); err == nil {
			expiredText = lock.ExpiresAt
		}
	}
	t.Cleanup(func() { supersedeAdvanceBarrierHook = nil })

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !retryWon {
		t.Fatal("the retry never committed; the ordering under test did not happen")
	}
	if advanced {
		t.Fatal("a dead pass resurrected its lease and advanced a lifecycle that had moved")
	}
	lock, lockErr := store.GetResourceLock(ctx, db.SupersedeAdvanceLockKeyPrefix+child)
	if lockErr == nil && lock.ExpiresAt != expiredText {
		t.Fatalf("lease expiry moved from %q to %q: renewal resurrected a dead token", expiredText, lock.ExpiresAt)
	}
	// ZERO stale effects: no confirmation, and the retried lifecycle is queued.
	if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceConfirmed); got != 0 {
		t.Fatalf("%s events = %d, want 0: a stale pass confirmed an advance", JobEventSupersedeAdvanceConfirmed, got)
	}
	if state := mustJob(t, store, child).State; state != string(JobQueued) {
		t.Fatalf("child state = %q, want queued: the retry's re-queue was undone", state)
	}
}

// TestEnqueueIsBoundToOwnershipAtCommit pins what a pre-write check cannot: the
// window BETWEEN the ownership renewal and the insert. A cleanup or an expiry that
// lands there would otherwise mint a job for a lifecycle the pass no longer owns,
// and the enqueue is irreversible.
//
// MUTATION PROOF: route the anchored mailbox through CreateJobWithEvent (dropping
// the in-transaction predicate) and the stale job is minted even though the
// pre-write renewal still runs.
func TestEnqueueIsBoundToOwnershipAtCommit(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	const child = "workflow-commit-binding"
	observed := supersedeAdvanceOwnershipChild(t, store, child)
	ownAdvance(t, store, child, "token-live", time.Now().UTC().Add(SupersedeAdvanceLeaseTTL))

	anchored := engine
	anchored.supersedeAdvance = &supersedeAdvanceAnchor{
		JobID:      child,
		Generation: observed.LifecycleGeneration,
		LockKey:    db.SupersedeAdvanceLockKeyPrefix + child,
		Token:      "token-live",
	}

	// SUCCESS CONTROL FIRST: with ownership held throughout, the insert commits.
	request := JobRequest{ID: "workflow-commit-binding/dependent", Repo: "gitmoot/gitmoot", Branch: "task-7", Agent: "impl", Action: "implement"}
	if err := anchored.enqueue(ctx, request); err != nil {
		t.Fatalf("enqueue on a held lease: %v", err)
	}
	if _, err := store.GetJob(ctx, request.ID); err != nil {
		t.Fatalf("GetJob after an owned enqueue: %v", err)
	}

	// Now ownership is lost INSIDE the renewal-to-insert window, which is the only
	// place left once renewal itself is generation- and expiry-bound.
	fired := false
	enqueuePreWriteHook = func(hookCtx context.Context, jobID string) {
		if fired {
			return
		}
		fired = true
		if released, err := store.ReleaseResourceLock(hookCtx, db.SupersedeAdvanceLockKeyPrefix+child, child, "token-live"); err != nil || !released {
			t.Errorf("drop ownership in the window released=%v err=%v", released, err)
		}
	}
	t.Cleanup(func() { enqueuePreWriteHook = nil })

	stale := JobRequest{ID: "workflow-commit-binding/stale", Repo: "gitmoot/gitmoot", Branch: "task-7", Agent: "impl", Action: "implement"}
	if err := anchored.enqueue(ctx, stale); err == nil {
		t.Fatal("an enqueue committed after ownership was lost in the renewal-to-insert window")
	}
	if !fired {
		t.Fatal("the seam hook never fired; the window under test was not entered")
	}
	if _, err := store.GetJob(ctx, stale.ID); err == nil {
		t.Fatal("the stale enqueue landed; the binding is not at commit")
	}
}
