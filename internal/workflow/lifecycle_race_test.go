package workflow

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	dbtest "github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// TestSupersedeClosedPullRequestJobRefusesAStaleObservedGeneration stands in the ABA
// window the state-only compare-and-swap could not see.
//
// The sweep forms its verdict — this pull request is closed, so this leg is
// pointless — about the run it LISTED. Between that listing and the write the job
// can complete and be re-queued, and a state-only CAS accepts the new run because
// it is `queued` too, cancelling work the verdict was never about.
//
// The test occupies exactly that window: it takes the observation, lets the job
// complete and re-queue, and only then settles with the stale row.
//
// MUTATION PROOF: swap TransitionJobStateWithEventAtGeneration for
// TransitionJobStateWithEvent and the newer lifecycle is cancelled.
func TestSupersedeClosedPullRequestJobRefusesAStaleObservedGeneration(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	insertQueuedJob(t, store, db.Job{ID: "workflow-aba", Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	stale := mustJob(t, store, "workflow-aba")

	for _, state := range []JobState{JobRunning, JobSucceeded, JobQueued} {
		if err := store.UpdateJobState(ctx, "workflow-aba", string(state)); err != nil {
			t.Fatalf("UpdateJobState(%s): %v", state, err)
		}
	}
	current := mustJob(t, store, "workflow-aba")
	if current.LifecycleGeneration == stale.LifecycleGeneration {
		t.Fatalf("fixture did not advance the generation: stale=%d current=%d", stale.LifecycleGeneration, current.LifecycleGeneration)
	}

	job, superseded, err := SupersedeClosedPullRequestJob(ctx, store, stale, "pr closed")
	if err != nil {
		t.Fatalf("SupersedeClosedPullRequestJob returned error: %v", err)
	}
	if superseded {
		t.Fatal("a stale observation cancelled a newer lifecycle")
	}
	if job.State != string(JobQueued) {
		t.Fatalf("state = %q, want the re-queued run left alone", job.State)
	}
	for _, event := range mustJobEventKinds(t, store, "workflow-aba") {
		if event == JobEventSupersededPullRequestClosed || event == JobEventSupersedeFinalizePending {
			t.Fatalf("stale settlement wrote %q on the live run", event)
		}
	}

	// The CURRENT observation still settles: the anchor rejects staleness, not the
	// sweep. Without this arm a guard that refused everything would pass above.
	_, superseded, err = SupersedeClosedPullRequestJob(ctx, store, current, "pr closed")
	if err != nil {
		t.Fatalf("current-generation supersede returned error: %v", err)
	}
	if !superseded {
		t.Fatal("the observed generation was refused; the sweep can no longer terminate anything")
	}
}

// TestFinalizeClosedPullRequestDelegationChildRefusesAStaleObservedGeneration is the
// same window on the child path, where the cost is worse: the stale settlement
// would drive a synthetic `failed` result into a run that is alive.
func TestFinalizeClosedPullRequestDelegationChildRefusesAStaleObservedGeneration(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job) returned error: %v", err)
	}
	const child = "parent-job/delegation/api"
	stale := mustJob(t, store, child)
	for _, state := range []JobState{JobRunning, JobSucceeded, JobQueued} {
		if err := store.UpdateJobState(ctx, child, string(state)); err != nil {
			t.Fatalf("UpdateJobState(%s): %v", state, err)
		}
	}
	if mustJob(t, store, child).LifecycleGeneration == stale.LifecycleGeneration {
		t.Fatal("fixture did not advance the child's generation")
	}

	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, stale, "pr closed")
	if err != nil {
		t.Fatalf("FinalizeClosedPullRequestDelegationChild returned error: %v", err)
	}
	if finalized {
		t.Fatal("a stale observation finalized a newer child lifecycle")
	}
	live := mustJob(t, store, child)
	if live.State != string(JobQueued) {
		t.Fatalf("child state = %q, want the re-queued run left alone", live.State)
	}
	payload, err := unmarshalPayload(live.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.Result != nil {
		t.Fatalf("a synthetic result was stamped on a live run: %+v", payload.Result)
	}
}

// TestCompletePendingSupersedeFinalizationVoidsADebtFromAnOlderLifecycle is the
// recovery side of the same ABA problem, and it bites in both directions.
//
// The debt is recorded for the run that incurred it. `gitmoot job retry` then puts
// the job back in queued at a NEWER generation, and:
//
//	if that newer run FAILS, paying the old debt stamps it with the PR-closed
//	synthetic failure and advances the parent on a run that failed for its own
//	reasons;
//
//	if that newer run SUCCEEDS, the debt can never be paid at all, so the job stays
//	a candidate on every poll forever.
//
// Both are answered the same way: the debt is VOIDED — closed, with no work — and
// the newer run settles through its own path.
//
// MUTATION PROOF: drop the generation from the pending marker (or compare only the
// state) and the newer-failed arm stamps a result and advances the parent, while
// the newer-succeeded arm stays pending.
func TestCompletePendingSupersedeFinalizationVoidsADebtFromAnOlderLifecycle(t *testing.T) {
	for _, newer := range []JobState{JobFailed, JobSucceeded} {
		t.Run("newer "+string(newer), func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
			seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
			engine := testEngine(store)
			insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
				Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
				Result: &AgentResult{
					Decision: "approved",
					Summary:  "fan out",
					Delegations: []Delegation{
						{ID: "api", Agent: "api", Action: "review", Prompt: "review api"},
					},
				},
			})
			if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
				t.Fatalf("AdvanceJob(parent-job): %v", err)
			}
			const child = "parent-job/delegation/api"
			superseded := mustJob(t, store, child)
			// The supersession records its debt for THIS generation.
			if _, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, superseded, "pr closed"); err != nil && !isDelegationPolicyOutcome(err) {
				t.Fatalf("FinalizeClosedPullRequestDelegationChild: %v", err)
			}
			// PRODUCTION must anchor the marker itself: the recovery below can only
			// tell lifecycles apart if the generation is in the row the sweep wrote.
			// The NET debt is already settled at this point — block_parent is a policy
			// outcome, which pays it — so the marker event is what to inspect.
			markedGeneration, markedReason, anchored := pendingDebtMarker(t, store, child)
			if !anchored || markedGeneration != superseded.LifecycleGeneration {
				t.Fatalf("pending marker generation = %d anchored=%v, want the observed generation %d", markedGeneration, anchored, superseded.LifecycleGeneration)
			}
			// A retry starts a new lifecycle, and that run reaches its own outcome.
			if _, err := RetryJob(ctx, store, child); err != nil {
				t.Fatalf("RetryJob: %v", err)
			}
			retried := mustJob(t, store, child)
			if retried.State != string(JobQueued) {
				t.Fatalf("retried child = %q, want queued", retried.State)
			}
			if retried.LifecycleGeneration == superseded.LifecycleGeneration {
				t.Fatal("RetryJob did not start a new lifecycle generation")
			}
			// Re-arm the debt as an OUTSTANDING one against the new lifecycle: this is
			// the state a poll would find if the original finalization had failed. It
			// reuses the generation PRODUCTION recorded above, not an invented one.
			if err := store.AddJobEvent(ctx, db.JobEvent{
				JobID: child, Kind: JobEventSupersedeFinalizePending,
				Message: formatSupersedeFinalizeDebt(markedGeneration, markedReason),
			}); err != nil {
				t.Fatalf("re-arm pending debt: %v", err)
			}
			if err := clearChildResult(ctx, store, child); err != nil {
				t.Fatalf("clear child result: %v", err)
			}
			if err := store.UpdateJobState(ctx, child, string(newer)); err != nil {
				t.Fatalf("UpdateJobState(%s): %v", newer, err)
			}
			taskBefore := mustTaskState(t, store, "task-7")

			handled, err := engine.CompletePendingSupersedeFinalization(ctx, child)
			if err != nil {
				t.Fatalf("CompletePendingSupersedeFinalization: %v", err)
			}
			if !handled {
				t.Fatal("the outstanding debt was not handled at all; it would stay a candidate forever")
			}
			// Voided: closed for the candidate query, with no synthetic result and no
			// parent advance against the newer run.
			pending, err := store.JobIDsWithPendingSupersedeFinalization(ctx)
			if err != nil {
				t.Fatalf("JobIDsWithPendingSupersedeFinalization: %v", err)
			}
			for _, id := range pending {
				if id == child {
					t.Fatal("the debt is still outstanding: this job is a candidate on every future poll")
				}
			}
			live := mustJob(t, store, child)
			payload, err := unmarshalPayload(live.Payload)
			if err != nil {
				t.Fatalf("unmarshalPayload: %v", err)
			}
			if payload.Result != nil {
				t.Fatalf("the old debt stamped a result on lifecycle %d: %+v", live.LifecycleGeneration, payload.Result)
			}
			if got := mustTaskState(t, store, "task-7"); got != taskBefore {
				t.Fatalf("task state moved %q -> %q: the parent was advanced on a newer lifecycle", taskBefore, got)
			}
		})
	}
}

func countWorkflowJobEvents(t *testing.T, store *db.Store, jobID string, kind string) int {
	t.Helper()
	count := 0
	for _, got := range mustJobEventKinds(t, store, jobID) {
		if got == kind {
			count++
		}
	}
	return count
}

// TestCompletePendingSupersedeFinalizationRefusesARetryClaimedMidPayment drives the
// interleaving itself through the production entry point, at every window a read
// cannot close.
//
// The recovery reads the job and its debt in one transaction and writes in others.
// `gitmoot job retry` can queue generation N+1 and a worker can claim it in ANY of
// those gaps, and a payment that acts by job id would then release the claimed
// run's locks, stamp the superseded run's PR-closed failure onto it, and advance
// its coordinator on that.
//
// One subtest per gap, because a different guard owns each and mutual masking would
// otherwise make all of them look tested by one:
//
//	after-read       the atomic claim CAS on (state, generation)
//	after-claim      the payment's own re-read, which gates the UNANCHORED cleanups
//	before-cleanup   the transactional generation guard inside the lock release
//	before-finalize  the anchored payload write inside the finalizer
//
// before-cleanup is the window F-1 named: the re-read has already VALIDATED
// generation N, and the by-owner resource-lock delete happens statements later, so
// a retry claimed in between had its own locks destroyed by a cleanup that was
// authorised for a lifecycle that no longer exists.
//
// The expected outcome is identical at every stage — that is the point.
func TestCompletePendingSupersedeFinalizationRefusesARetryClaimedMidPayment(t *testing.T) {
	for _, stage := range []string{
		supersedeDebtStageAfterRead,
		supersedeDebtStageAfterClaim,
		supersedeDebtStageBeforeCleanup,
		supersedeDebtStageBeforeFinalize,
	} {
		t.Run(stage, func(t *testing.T) {
			runSupersedeDebtInterleaveCase(t, stage)
		})
	}
}

func runSupersedeDebtInterleaveCase(t *testing.T, stage string) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", State: string(TaskImplementing), Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job): %v", err)
	}
	const child = "parent-job/delegation/api"
	observed := mustJob(t, store, child)
	if _, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, observed, "pr closed"); err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("FinalizeClosedPullRequestDelegationChild: %v", err)
	}
	// The debt is outstanding again, anchored on the superseded run.
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID: child, Kind: JobEventSupersedeFinalizePending,
		Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration, "pr closed"),
	}); err != nil {
		t.Fatalf("arm the superseded run's debt: %v", err)
	}

	// The retry+claim happens in the window named by stage.
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	const lockKey = "runtime:codex:session-retry"
	interleaved := 0
	supersedeDebtInterleaveHook = func(hookCtx context.Context, at string) {
		if at != stage {
			return
		}
		interleaved++
		if _, err := RetryJob(hookCtx, store, child); err != nil {
			t.Fatalf("RetryJob in the interleave window: %v", err)
		}
		if err := clearChildResult(hookCtx, store, child); err != nil {
			t.Fatalf("clear child result: %v", err)
		}
		if err := store.UpdateJobState(hookCtx, child, string(JobRunning)); err != nil {
			t.Fatalf("UpdateJobState(running): %v", err)
		}
		// The claimed run holds its own runtime-session lock, which a stale payment
		// would release: the observable that separates "refused" from "acted".
		locked, err := store.AcquireResourceLock(hookCtx, db.ResourceLock{
			ResourceKey: lockKey, OwnerJobID: child, OwnerToken: "token-retry",
			ExpiresAt: now.Add(30 * time.Minute).Format(time.RFC3339Nano),
		}, now)
		if err != nil || !locked {
			t.Fatalf("AcquireResourceLock acquired=%v err=%v", locked, err)
		}
	}
	t.Cleanup(func() { supersedeDebtInterleaveHook = nil })
	finalizedBefore := countWorkflowJobEvents(t, store, child, "delegation_timeout_finalized")

	handled, err := engine.CompletePendingSupersedeFinalization(ctx, child)
	if err != nil {
		t.Fatalf("CompletePendingSupersedeFinalization: %v", err)
	}
	if interleaved != 1 {
		t.Fatalf("interleave hook ran %d times at stage %q; the window under test was not entered", interleaved, stage)
	}
	if !handled {
		t.Fatal("the debt was left outstanding with no disposition")
	}
	live := mustJob(t, store, child)
	if live.State != string(JobRunning) {
		t.Fatalf("claimed run = %q, want running: the stale payment terminated it", live.State)
	}
	payload, err := unmarshalPayload(live.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.Result != nil {
		t.Fatalf("the superseded run's failure was stamped onto the claimed run: %+v", payload.Result)
	}
	if got := countWorkflowJobEvents(t, store, child, "delegation_timeout_finalized"); got != finalizedBefore {
		t.Fatalf("delegation_timeout_finalized events = %d, want %d: the claimed run was finalized", got, finalizedBefore)
	}
	held, err := store.GetResourceLock(ctx, lockKey)
	if err != nil || held.OwnerJobID != child {
		t.Fatalf("resource lock = %+v err=%v, want still held by the claimed run", held, err)
	}
}

// TestSupersedeDebtCompletionDoesNotClearANewerDebt closes the other half of the
// same window, on the marker rather than the row.
//
// The candidate query is last-one-wins by event id. A retried run that gets
// superseded AGAIN writes its own pending marker; if the older payment then writes
// an unconditional completion, that completion becomes the latest marker and
// silently clears a debt nobody ever paid — the strand returns, now invisible.
//
// MUTATION PROOF: drop the latest-marker check from
// recordSupersedeFinalizationCompleted and the newer debt disappears from the
// candidate set.
func TestSupersedeDebtCompletionDoesNotClearANewerDebt(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	insertQueuedJob(t, store, db.Job{ID: "workflow-debt", Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	const older, newer = int64(3), int64(4)
	for _, generation := range []int64{older, newer} {
		if err := store.AddJobEvent(ctx, db.JobEvent{
			JobID: "workflow-debt", Kind: JobEventSupersedeFinalizePending,
			Message: formatSupersedeFinalizeDebt(generation, "pr closed"),
		}); err != nil {
			t.Fatalf("arm debt for generation %d: %v", generation, err)
		}
	}

	// The OLDER payment finishes last. It must not close the newer debt.
	if err := recordSupersedeFinalizationCompleted(ctx, store, "workflow-debt", "pr closed", older, nil); err != nil {
		t.Fatalf("recordSupersedeFinalizationCompleted: %v", err)
	}
	if debt, err := latestSupersedeFinalizeDebt(ctx, store, "workflow-debt"); err != nil || !debt.pending || debt.generation != newer {
		t.Fatalf("debt = %+v err=%v, want the newer generation %d still outstanding", debt, err, newer)
	}
	pending, err := store.JobIDsWithPendingSupersedeFinalization(ctx)
	if err != nil {
		t.Fatalf("JobIDsWithPendingSupersedeFinalization: %v", err)
	}
	found := false
	for _, id := range pending {
		if id == "workflow-debt" {
			found = true
		}
	}
	if !found {
		t.Fatal("the newer debt was cleared by the older payment; no poll will ever pay it")
	}

	// The matching payment DOES close it, so nothing is frozen.
	if err := recordSupersedeFinalizationCompleted(ctx, store, "workflow-debt", "pr closed", newer, nil); err != nil {
		t.Fatalf("matching recordSupersedeFinalizationCompleted: %v", err)
	}
	if debt, err := latestSupersedeFinalizeDebt(ctx, store, "workflow-debt"); err != nil || debt.pending {
		t.Fatalf("debt = %+v err=%v, want closed by its own payment", debt, err)
	}
}

// TestSupersedeDebtClosureRefusesADebtAppendedMidClosure is the F-2 window: the
// payment READS the latest marker, accepts generation N, and appends its closure
// statements later. A retry plus a fresh supersession in that gap appends pending
// generation N+1; an unconditional append then becomes the latest marker and
// erases N+1 from the candidate query — a debt that is now invisible and unpaid.
//
// Driven through the production recovery entry point
// Engine.CompletePendingSupersedeFinalization, at the stage between the read and
// the append, for BOTH terminal markers: the payment's completion and the
// failed-claim void.
//
// MUTATION PROOF: make CloseSupersedeFinalizationDebtAtGeneration append
// unconditionally and the job leaves JobIDsWithPendingSupersedeFinalization.
func TestSupersedeDebtClosureRefusesADebtAppendedMidClosure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		breakClaim bool
	}{
		{name: "completion"},
		{name: "void-after-failed-claim", breakClaim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
			engine := testEngine(store)
			const job = "workflow-debt-midclosure"
			insertQueuedJob(t, store, db.Job{ID: job, Agent: "impl", Type: "implement"}, JobPayload{
				Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
			})
			// Settle the run the way production does: one transaction carrying the
			// terminal state and the debt anchored to the generation it superseded.
			observed := mustJob(t, store, job)
			superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, job, observed.State, observed.LifecycleGeneration, string(JobCancelled),
				db.JobEvent{JobID: job, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
				db.JobEvent{JobID: job, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration, "pr closed")})
			if err != nil || !superseded {
				t.Fatalf("supersede the observed run: transitioned=%v err=%v", superseded, err)
			}

			fired := 0
			supersedeDebtInterleaveHook = func(hookCtx context.Context, at string) {
				// Breaking the claim routes the pass down the void path instead of the
				// payment path; both end in a terminal marker append.
				if tc.breakClaim && at == supersedeDebtStageAfterRead {
					if _, err := RetryJob(hookCtx, store, job); err != nil {
						t.Fatalf("RetryJob to break the claim: %v", err)
					}
					return
				}
				if at != supersedeDebtStageBeforeClosure || fired > 0 {
					return
				}
				fired++
				// A retry queues a new lifecycle and a fresh supersession records ITS
				// debt. Only the marker write is done here, not the payment: the race is
				// against a supersession whose own payment has not run yet. On the void
				// path the retry already happened (that is what broke the claim), so the
				// row is queued and only the supersession is left to do.
				queued := mustJob(t, store, job)
				if queued.State != string(JobQueued) {
					retried, err := RetryJob(hookCtx, store, job)
					if err != nil {
						t.Fatalf("RetryJob in the closure window: %v", err)
					}
					queued = retried
				}
				appended, err := store.TransitionJobStateWithEventAtGeneration(hookCtx, job, queued.State, queued.LifecycleGeneration, string(JobCancelled),
					db.JobEvent{JobID: job, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed again"},
					db.JobEvent{JobID: job, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(queued.LifecycleGeneration, "pr closed again")})
				if err != nil || !appended {
					t.Fatalf("append the newer debt: transitioned=%v err=%v", appended, err)
				}
			}
			t.Cleanup(func() { supersedeDebtInterleaveHook = nil })

			if _, err := engine.CompletePendingSupersedeFinalization(ctx, job); err != nil && !isDelegationPolicyOutcome(err) {
				t.Fatalf("CompletePendingSupersedeFinalization: %v", err)
			}
			if fired != 1 {
				t.Fatalf("closure-window interleavings = %d, want exactly 1", fired)
			}

			debt, err := latestSupersedeFinalizeDebt(ctx, store, job)
			if err != nil {
				t.Fatalf("latestSupersedeFinalizeDebt: %v", err)
			}
			if !debt.pending {
				t.Fatal("the newer debt was closed by the older payment; no poll will ever pay it")
			}
			pending, err := store.JobIDsWithPendingSupersedeFinalization(ctx)
			if err != nil {
				t.Fatalf("JobIDsWithPendingSupersedeFinalization: %v", err)
			}
			found := false
			for _, id := range pending {
				if id == job {
					found = true
				}
			}
			if !found {
				t.Fatalf("job left the candidate set with debt %+v outstanding", debt)
			}

			// And the newer debt is still payable: its own generation closes it.
			if err := recordSupersedeFinalizationCompleted(ctx, store, job, debt.reason, debt.generation, nil); err != nil {
				t.Fatalf("pay the newer debt: %v", err)
			}
			if after, err := latestSupersedeFinalizeDebt(ctx, store, job); err != nil || after.pending {
				t.Fatalf("debt = %+v err=%v, want closed by its own payment", after, err)
			}
		})
	}
}

// TestSupersedeCleanupRefusesATerminalRetryAtANewerGeneration separates the two
// halves of the cleanup guard. The interleaved retry here ends TERMINAL again, so
// the state half of the predicate passes and only the GENERATION half can refuse:
// a state-only guard deletes the newer lifecycle's locks while looking correct.
//
// MUTATION PROOF: drop `generation != atGeneration` from
// ReleaseSupersededJobResourceLocksAtGeneration and this fails while the
// running-retry cases still pass.
func TestSupersedeCleanupRefusesATerminalRetryAtANewerGeneration(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	const job = "workflow-debt-terminal-retry"
	insertQueuedJob(t, store, db.Job{ID: job, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	observed := mustJob(t, store, job)
	superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, job, observed.State, observed.LifecycleGeneration, string(JobCancelled),
		db.JobEvent{JobID: job, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: job, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration, "pr closed")})
	if err != nil || !superseded {
		t.Fatalf("supersede the observed run: transitioned=%v err=%v", superseded, err)
	}

	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	const lockKey = "runtime:codex:session-terminal-retry"
	interleaved := 0
	supersedeDebtInterleaveHook = func(hookCtx context.Context, at string) {
		if at != supersedeDebtStageBeforeCleanup || interleaved > 0 {
			return
		}
		interleaved++
		if _, err := RetryJob(hookCtx, store, job); err != nil {
			t.Fatalf("RetryJob in the cleanup window: %v", err)
		}
		// The new lifecycle runs and fails: TERMINAL, like the row this pass
		// claimed, but at a different generation and holding its own lock.
		if err := store.UpdateJobState(hookCtx, job, string(JobRunning)); err != nil {
			t.Fatalf("UpdateJobState(running): %v", err)
		}
		if err := store.UpdateJobState(hookCtx, job, string(JobFailed)); err != nil {
			t.Fatalf("UpdateJobState(failed): %v", err)
		}
		locked, err := store.AcquireResourceLock(hookCtx, db.ResourceLock{
			ResourceKey: lockKey, OwnerJobID: job, OwnerToken: "token-terminal-retry",
			ExpiresAt: now.Add(30 * time.Minute).Format(time.RFC3339Nano),
		}, now)
		if err != nil || !locked {
			t.Fatalf("AcquireResourceLock acquired=%v err=%v", locked, err)
		}
	}
	t.Cleanup(func() { supersedeDebtInterleaveHook = nil })

	if _, err := engine.CompletePendingSupersedeFinalization(ctx, job); err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("CompletePendingSupersedeFinalization: %v", err)
	}
	if interleaved != 1 {
		t.Fatalf("cleanup-window interleavings = %d, want exactly 1", interleaved)
	}
	held, err := store.GetResourceLock(ctx, lockKey)
	if err != nil || held.OwnerJobID != job {
		t.Fatalf("resource lock = %+v err=%v, want still held by the newer lifecycle", held, err)
	}
	live := mustJob(t, store, job)
	if live.LifecycleGeneration == observed.LifecycleGeneration {
		t.Fatalf("interleave did not move the generation off %d", observed.LifecycleGeneration)
	}
}

// clearChildResult removes a synthetic result so the retried lifecycle starts from
// the same shape a real re-dispatch would: queued work with no verdict yet.
func clearChildResult(ctx context.Context, store *db.Store, jobID string) error {
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return err
	}
	payload.Result = nil
	encoded, err := marshalPayload(payload)
	if err != nil {
		return err
	}
	return store.UpdateJobPayload(ctx, jobID, encoded)
}

// pendingDebtMarker returns the generation, reason and anchored flag of the LAST
// pending-debt marker a job carries. It reads the marker EVENT rather than the net
// debt on purpose: a supersession that ends in a policy outcome pays its debt
// immediately, so the net debt is empty while the marker's contents are still the
// thing production has to get right.
func pendingDebtMarker(t *testing.T, store *db.Store, jobID string) (int64, string, bool) {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	generation, reason, anchored := int64(0), "", false
	found := false
	for _, event := range events {
		if event.Kind != JobEventSupersedeFinalizePending {
			continue
		}
		found = true
		generation, reason, anchored = parseSupersedeFinalizeDebt(event.Message)
	}
	if !found {
		t.Fatalf("job %s carries no %s marker", jobID, JobEventSupersedeFinalizePending)
	}
	return generation, reason, anchored
}

// TestPersistTaskStateRefusesADisposalWrittenAfterTheRead pins the third race.
//
// Every caller checks for a disposed task with a pre-read, and a pre-read is
// exactly what a concurrent disposal invalidates: an operator dismisses the task
// between the read and the write, and automation then resurrects it into
// `reviewing`, `blocked` or `implementing`. The exclusion has to be part of the
// statement that writes.
//
// MUTATION PROOF: drop the disposed states from the forbidden list (leaving only
// the merged guard) and every disposed arm below writes over the disposition.
func TestPersistTaskStateRefusesADisposalWrittenAfterTheRead(t *testing.T) {
	for _, disposed := range []TaskState{TaskDismissed, TaskSuperseded, TaskStranded} {
		for _, target := range []TaskState{TaskImplementing, TaskReviewing, TaskBlocked, TaskMerged} {
			t.Run(string(disposed)+"->"+string(target), func(t *testing.T) {
				store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				ctx := context.Background()
				const repo, branch = "owner/repo", "feature/one"
				if err := store.UpsertTask(ctx, db.Task{
					ID: "task-1", RepoFullName: repo, State: string(TaskReviewing), Branch: branch,
				}); err != nil {
					t.Fatal(err)
				}
				// The caller's snapshot, taken while the task was still live.
				snapshot, err := store.GetTask(ctx, "task-1")
				if err != nil {
					t.Fatal(err)
				}
				// A second writer disposes of it in the window.
				disposal := snapshot
				disposal.State = string(disposed)
				if err := store.UpsertTask(ctx, disposal); err != nil {
					t.Fatal(err)
				}

				written, err := PersistTaskState(ctx, store, snapshot, target)
				if written {
					t.Fatalf("wrote %s over a %s task", target, disposed)
				}
				if err == nil || !strings.Contains(err.Error(), string(disposed)) {
					t.Fatalf("error = %v, want one naming %s", err, disposed)
				}
				task, err := store.GetTask(ctx, "task-1")
				if err != nil {
					t.Fatal(err)
				}
				if task.State != string(disposed) {
					t.Fatalf("task state = %q, want the disposition preserved", task.State)
				}
			})
		}
	}
}

// TestPersistTaskStateStillWritesEveryNonDisposedState is the other half: the
// atomic exclusions must not freeze ordinary advancement. A guard that refused
// everything would satisfy the race tests above and break the workflow.
func TestPersistTaskStateStillWritesEveryNonDisposedState(t *testing.T) {
	for _, target := range []TaskState{
		TaskPlanned, TaskImplementing, TaskPullRequestOpen, TaskReviewing, TaskChangesRequested,
		TaskReadyToMerge, TaskAwaitingHuman, TaskAwaitingHumanMerge, TaskMerged,
		TaskDismissed, TaskSuperseded, TaskStranded,
	} {
		t.Run(string(target), func(t *testing.T) {
			store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-1", RepoFullName: "owner/repo", State: string(TaskImplementing), Branch: "feature/one",
			}); err != nil {
				t.Fatal(err)
			}
			task, err := store.GetTask(ctx, "task-1")
			if err != nil {
				t.Fatal(err)
			}
			written, err := PersistTaskState(ctx, store, task, target)
			if err != nil || !written {
				t.Fatalf("PersistTaskState(%s) written=%v err=%v", target, written, err)
			}
			if got := mustTaskState(t, store, "task-1"); got != string(target) {
				t.Fatalf("task state = %q, want %s", got, target)
			}
			// Idempotent: writing the same state again is permitted, including for a
			// disposed target, so a repeated disposal is not an error.
			written, err = PersistTaskState(ctx, store, task, target)
			if err != nil || !written {
				t.Fatalf("repeated PersistTaskState(%s) written=%v err=%v", target, written, err)
			}
		})
	}
}

func mustTaskState(t *testing.T, store *db.Store, taskID string) string {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	return task.State
}

func mustJobEventKinds(t *testing.T, store *db.Store, jobID string) []string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}
