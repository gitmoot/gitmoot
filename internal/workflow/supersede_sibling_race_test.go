package workflow

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	dbtest "github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// TestSupersedeCleanupKeepsARetriedDelegationBranchLock is the sibling-cleanup gap
// the review named, on the ONE cleanup that can destroy a live run's lane.
//
// A worktree-isolated implement delegation leg holds a per-delegation branch lock.
// The unguarded force-release deletes by (repo, branch) alone, so when a retry
// re-queued the leg and re-acquired that same lane, the previous run's cleanup —
// running after the resource-lock transaction had already committed — deleted the
// live run's lock. The generation now rides in that DELETE's own predicate.
//
// MUTATION PROOF: pass any other generation to
// ForceReleaseDelegationBranchLockAtJobGeneration (or call the unguarded
// ForceReleaseLockWithEvent) and the retried lane's lock disappears.
func TestSupersedeCleanupKeepsARetriedDelegationBranchLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	const (
		child  = "parent-job/delegation/api"
		repo   = "gitmoot/gitmoot"
		branch = "gitmoot-delegation-parent-api"
	)
	insertQueuedJob(t, store, db.Job{ID: child, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: repo, Branch: branch, PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
		ParentJobID: "parent-job", DelegationID: "api", WorktreePath: t.TempDir(),
	})
	observed := mustJob(t, store, child)
	// Supersede the observed run, then leave its debt outstanding.
	superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: child, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)})
	if err != nil || !superseded {
		t.Fatalf("supersede: transitioned=%v err=%v", superseded, err)
	}

	// The retry re-queues the leg and re-acquires the SAME delegation lane, in the
	// window after the guarded resource transaction commits.
	interleaved := 0
	supersedeDebtInterleaveHook = func(hookCtx context.Context, at string) error {
		if at != supersedeDebtStageAfterResourceCommit {
			return nil
		}
		interleaved++
		if _, err := RetryJob(hookCtx, store, child); err != nil {
			t.Fatalf("RetryJob: %v", err)
		}
		if acquired, err := store.AcquireLock(hookCtx, db.BranchLock{
			RepoFullName: repo, Branch: branch, Owner: "impl",
		}); err != nil || !acquired {
			t.Fatalf("retry re-acquire delegation lane: acquired=%v err=%v", acquired, err)
		}
		return nil
	}
	t.Cleanup(func() { supersedeDebtInterleaveHook = nil })

	engine := testEngine(store)
	if _, err := engine.CompletePendingSupersedeFinalization(ctx, child); err != nil {
		t.Fatalf("CompletePendingSupersedeFinalization: %v", err)
	}
	if interleaved != 1 {
		t.Fatalf("interleave hook ran %d times, want 1: the window under test was not entered", interleaved)
	}
	lock, err := store.GetBranchLock(ctx, repo, branch)
	if err != nil {
		t.Fatalf("the retried run's delegation lane was released by the superseded run's cleanup: %v", err)
	}
	if lock.Owner != "impl" {
		t.Fatalf("delegation lane owner = %q, want the retried run's", lock.Owner)
	}
}

// TestSupersedeDebtMarkerClassificationIsCanonicalOnly is the Go half of the
// parser/SQL parity. The SQL side accepts a marker as naming a generation only
// when CAST(CAST(message AS INTEGER) AS TEXT) = message, so the Go side must apply
// exactly that rule: anything else is a marker that names no generation. A parser
// that were more permissive would classify `07` as generation 7 and then ask SQL
// to close a debt SQL does not consider anchored — a debt nothing can ever close.
//
// MUTATION PROOF: drop the FormatInt round-trip in parseSupersedeFinalizeDebt and
// the non-canonical rows below flip to anchored.
func TestSupersedeDebtMarkerClassificationIsCanonicalOnly(t *testing.T) {
	for _, tc := range []struct {
		message        string
		wantGeneration int64
		wantAnchored   bool
	}{
		{message: "0", wantGeneration: 0, wantAnchored: true},
		{message: "7", wantGeneration: 7, wantAnchored: true},
		{message: "1234567890", wantGeneration: 1234567890, wantAnchored: true},
		{message: "07"},
		{message: "+7"},
		{message: " 7"},
		{message: "7 "},
		{message: "-7", wantGeneration: -7, wantAnchored: true},
		{message: "generation=7: pr closed"},
		{message: "pr closed"},
		{message: ""},
	} {
		t.Run("marker "+strconv.Quote(tc.message), func(t *testing.T) {
			generation, anchored := parseSupersedeFinalizeDebt(tc.message)
			if anchored != tc.wantAnchored || generation != tc.wantGeneration {
				t.Fatalf("parse(%q) = (%d, %v), want (%d, %v)", tc.message, generation, anchored, tc.wantGeneration, tc.wantAnchored)
			}
			if tc.wantAnchored && formatSupersedeFinalizeDebt(generation) != tc.message {
				t.Fatalf("format(%d) = %q, want the round-trip to reproduce %q", generation, formatSupersedeFinalizeDebt(generation), tc.message)
			}
		})
	}
}

// TestSupersedeCleanupKeepsATaskLaneOwnedByATerminalRetry is the task-lane gap.
//
// Its inactivity vetoes look sufficient and are not. The tasks veto EXCLUDES the
// exact implementing task being cleaned up, so a retry of that same task cannot
// re-assert the lane through it; and the jobs veto only fires while some job on the
// branch is non-terminal. A retry that re-queues, runs and SETTLES TERMINAL
// therefore passes both — and the previous run's cleanup deleted the lane the retry
// had re-acquired. The generation now rides in the DELETE's own predicate.
//
// MUTATION PROOF: swap ReleaseTaskLaneBranchLockAtJobGeneration for the unanchored
// ReleaseBranchLockIfInactiveWithEvent and the newer lane disappears.
func TestSupersedeCleanupKeepsATaskLaneOwnedByATerminalRetry(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	const (
		child  = "workflow-task-lane"
		repo   = "gitmoot/gitmoot"
		branch = "task-7"
	)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: repo, State: string(TaskImplementing), Branch: branch,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertQueuedJob(t, store, db.Job{ID: child, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: repo, Branch: branch, PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	observed := mustJob(t, store, child)
	superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobCancelled),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: child, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)})
	if err != nil || !superseded {
		t.Fatalf("supersede: transitioned=%v err=%v", superseded, err)
	}

	interleaved := 0
	supersedeDebtInterleaveHook = func(hookCtx context.Context, at string) error {
		if at != supersedeDebtStageBeforeTaskLane {
			return nil
		}
		interleaved++
		// The retry advances the generation AND settles terminal, so neither
		// inactivity veto fires; only the lifecycle anchor can refuse.
		for _, state := range []JobState{JobQueued, JobRunning, JobFailed} {
			if err := store.UpdateJobState(hookCtx, child, string(state)); err != nil {
				t.Fatalf("UpdateJobState(%s): %v", state, err)
			}
		}
		if acquired, err := store.AcquireLock(hookCtx, db.BranchLock{RepoFullName: repo, Branch: branch, Owner: "impl"}); err != nil || !acquired {
			t.Fatalf("retry re-acquire task lane: acquired=%v err=%v", acquired, err)
		}
		return nil
	}
	t.Cleanup(func() { supersedeDebtInterleaveHook = nil })

	guarded, err := releaseSupersededJobResourcesAtGeneration(ctx, store, mustJob(t, store, child), abortCauseSupersede, observed.LifecycleGeneration)
	if err != nil {
		t.Fatalf("releaseSupersededJobResourcesAtGeneration: %v", err)
	}
	if interleaved != 1 {
		t.Fatalf("interleave hook ran %d times, want 1", interleaved)
	}
	if lock, lerr := store.GetBranchLock(ctx, repo, branch); lerr != nil || lock.Owner != "impl" {
		t.Fatalf("task lane = %+v err=%v, want still held by the terminal retry", lock, lerr)
	}
	if got := countWorkflowJobEvents(t, store, child, "task_lane_lock_released"); got != 0 {
		t.Fatalf("task_lane_lock_released events = %d, want 0", got)
	}
	// No LATER cleanup effect may run once the guard is lost.
	if got := countWorkflowJobEvents(t, store, child, "delegation_worktree_cleanup_skipped"); got != 0 {
		t.Fatalf("delegation_worktree_cleanup_skipped events = %d, want 0: cleanup continued past a lost guard", got)
	}
	_ = guarded
}

// TestSupersedeCleanupReleasesTheTaskLaneItOwns is the matching success control: the
// anchored release must still free the lane on an ordinary supersession.
func TestSupersedeCleanupReleasesTheTaskLaneItOwns(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	const (
		child  = "workflow-task-lane-ok"
		repo   = "gitmoot/gitmoot"
		branch = "task-8"
	)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-8", RepoFullName: repo, State: string(TaskImplementing), Branch: branch,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertQueuedJob(t, store, db.Job{ID: child, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: repo, Branch: branch, PullRequest: 8, TaskID: "task-8", LeadAgent: "impl",
	})
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo, Branch: branch, Owner: "impl"}); err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	observed := mustJob(t, store, child)
	if _, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobCancelled),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"}); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if guarded, err := releaseSupersededJobResourcesAtGeneration(ctx, store, mustJob(t, store, child), abortCauseSupersede, observed.LifecycleGeneration); err != nil || !guarded {
		t.Fatalf("releaseSupersededJobResourcesAtGeneration guarded=%v err=%v", guarded, err)
	}
	if _, err := store.GetBranchLock(ctx, repo, branch); err == nil {
		t.Fatal("the superseded run's task lane was not released")
	}
	if got := countWorkflowJobEvents(t, store, child, "task_lane_lock_released"); got != 1 {
		t.Fatalf("task_lane_lock_released events = %d, want 1", got)
	}
}

// TestSupersedeCleanupReleasesTheDelegationLaneItOwns is the positive half. A guard
// that refuses everything would satisfy the race test above while leaking the lane
// on every ordinary supersession, so the happy path is pinned too.
//
// MUTATION PROOF: pass any generation other than the claimed one to the guarded
// release and the lane is never freed.
func TestSupersedeCleanupReleasesTheDelegationLaneItOwns(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	const (
		child  = "parent-job/delegation/api"
		repo   = "gitmoot/gitmoot"
		branch = "gitmoot-delegation-parent-api"
	)
	insertQueuedJob(t, store, db.Job{ID: child, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: repo, Branch: branch, PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
		ParentJobID: "parent-job", DelegationID: "api", WorktreePath: filepath.Join(t.TempDir(), "worktrees", "leg"),
	})
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo, Branch: branch, Owner: "impl"}); err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	observed := mustJob(t, store, child)
	superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: child, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)})
	if err != nil || !superseded {
		t.Fatalf("supersede: transitioned=%v err=%v", superseded, err)
	}

	// Scoped to the cleanup itself: the production entry is covered by the race case
	// above, and driving it here would additionally exercise the parent advance,
	// which is a different guard with its own tests.
	guarded, err := releaseSupersededJobResourcesAtGeneration(ctx, store, mustJob(t, store, child), abortCauseSupersede, observed.LifecycleGeneration)
	if err != nil || !guarded {
		t.Fatalf("releaseSupersededJobResourcesAtGeneration guarded=%v err=%v", guarded, err)
	}
	if _, err := store.GetBranchLock(ctx, repo, branch); err == nil {
		t.Fatal("the superseded leg's delegation lane was not released; the next same-branch leg waits on a dead run")
	}
	if got := countWorkflowJobEvents(t, store, child, "delegation_branch_lock_released"); got != 1 {
		t.Fatalf("delegation_branch_lock_released events = %d, want 1", got)
	}
}

// TestSupersedeAdvanceClaimPreventsAdvancingAMovedLifecycle isolates the CLAIM half
// of the bracket, which the confirm half masks in the outcome-only assertions.
//
// The claim's job is PREVENTION: when the row has already moved before the bracket
// starts, AdvanceJob must never run at all. The confirm's job is detection after
// the fact. Without the claim the parent gets advanced from a superseded verdict
// and the damage is only noticed afterwards, so the observable here is the parent
// itself — under a `continue` policy, whether a coordinator continuation was
// enqueued.
//
// MUTATION PROOF: make the claim append unconditional and the continuation appears.
func TestSupersedeAdvanceClaimPreventsAdvancingAMovedLifecycle(t *testing.T) {
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
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: "continue"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job): %v", err)
	}
	const child = "parent-job/delegation/api"
	observed := mustJob(t, store, child)
	// Stamp the synthetic result WITHOUT advancing, so the recovery enters the
	// already-finalized arm and the bracket is the only thing left to do.
	superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: child, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)})
	if err != nil || !superseded {
		t.Fatalf("supersede: transitioned=%v err=%v", superseded, err)
	}
	stamped := mustJob(t, store, child)
	payload, err := unmarshalPayload(stamped.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	payload.Result = &AgentResult{Decision: "failed", Summary: "pr closed"}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, child, encoded); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}

	interleaved := 0
	supersedeDebtInterleaveHook = func(hookCtx context.Context, at string) error {
		if at != supersedeDebtStageBeforeAdvanceClaim {
			return nil
		}
		interleaved++
		// A payload-preserving lifecycle bump: AdvanceJob would SUCCEED if it ran.
		for _, state := range []JobState{JobQueued, JobRunning} {
			if err := store.UpdateJobState(hookCtx, child, string(state)); err != nil {
				t.Fatalf("UpdateJobState(%s): %v", state, err)
			}
		}
		return nil
	}
	t.Cleanup(func() { supersedeDebtInterleaveHook = nil })

	if _, err := engine.CompletePendingSupersedeFinalization(ctx, child); err != nil {
		t.Fatalf("CompletePendingSupersedeFinalization: %v", err)
	}
	if interleaved != 1 {
		t.Fatalf("interleave hook ran %d times, want 1", interleaved)
	}
	if _, err := store.GetJob(ctx, "parent-job/continuation"); err == nil {
		t.Fatal("the coordinator was advanced from a superseded verdict: the claim did not prevent the advance")
	}
	if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceClaimed); got != 0 {
		t.Fatalf("%s events = %d, want 0: the claim must refuse a moved lifecycle", JobEventSupersedeAdvanceClaimed, got)
	}
	debt, err := latestSupersedeFinalizeDebt(ctx, store, child)
	if err != nil {
		t.Fatalf("latestSupersedeFinalizeDebt: %v", err)
	}
	if !debt.pending {
		t.Fatal("the debt was closed although the parent advance never ran")
	}
}

// TestSupersedeCleanupFailureLeavesTheDebtOutstanding pins the half of the
// cross-process finding that is about ERROR HANDLING rather than ordering.
//
// Under WAL a cleanup write can fail for reasons that are not "the guard refused"
// — SQLITE_BUSY_SNAPSHOT from another process's commit being the one the review
// named. Swallowing that error and then recording the debt paid destroyed it: the
// locks stayed held and no later poll could rediscover the obligation. The error
// must propagate and the marker must stay pending.
//
// The fault is injected with a trigger on the lock delete, which is the same class
// of failure with a deterministic trigger.
//
// MUTATION PROOF: swallow cleanupErr in completeSupersedeFinalization and the debt
// is closed while the lock survives.
func TestSupersedeCleanupFailureLeavesTheDebtOutstanding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store := openEngineStoreAt(t, path)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	const child = "workflow-cleanup-fault"
	insertQueuedJob(t, store, db.Job{ID: child, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	observed := mustJob(t, store, child)
	superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobCancelled),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: child, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)})
	if err != nil || !superseded {
		t.Fatalf("supersede: transitioned=%v err=%v", superseded, err)
	}
	// A lock the cleanup will try, and fail, to delete.
	if locked, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-fault", OwnerJobID: child, OwnerToken: "token",
		ExpiresAt: "2030-01-01T00:00:00Z",
	}, mustParseTestTime(t, "2026-09-01T09:00:00Z")); err != nil || !locked {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", locked, err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open trigger connection: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`
CREATE TRIGGER fail_supersede_lock_release
BEFORE DELETE ON resource_locks
WHEN OLD.owner_job_id = 'workflow-cleanup-fault'
BEGIN
  SELECT RAISE(ABORT, 'injected cleanup failure');
END;`); err != nil {
		t.Fatalf("create cleanup failure trigger: %v", err)
	}

	engine := testEngine(store)
	if _, err := engine.CompletePendingSupersedeFinalization(ctx, child); err == nil {
		t.Fatal("the cleanup failure was swallowed; the caller cannot know the cleanup did not run")
	}
	debt, err := latestSupersedeFinalizeDebt(ctx, store, child)
	if err != nil {
		t.Fatalf("latestSupersedeFinalizeDebt: %v", err)
	}
	if !debt.pending {
		t.Fatal("the debt was recorded paid while its cleanup failed; nothing can rediscover it")
	}
	if lock, err := store.GetResourceLock(ctx, "runtime:codex:session-fault"); err != nil || lock.OwnerJobID != child {
		t.Fatalf("resource lock = %+v err=%v, want still held (the delete was aborted)", lock, err)
	}
	// And once the fault clears, the same debt is payable.
	if _, err := raw.Exec(`DROP TRIGGER fail_supersede_lock_release`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := engine.CompletePendingSupersedeFinalization(ctx, child); err != nil {
		t.Fatalf("recovery after the fault cleared: %v", err)
	}
	if after, err := latestSupersedeFinalizeDebt(ctx, store, child); err != nil || after.pending {
		t.Fatalf("debt = %+v err=%v, want closed once the cleanup succeeded", after, err)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-fault"); err == nil {
		t.Fatal("the lock survived a successful cleanup")
	}
}

// TestSupersedeAdvanceRefusesALifecycleMovedDuringASuccessfulAdvance isolates the
// CONFIRM half of the parent-advance bracket.
//
// The other before-advance case moves the row with RetryJob, which clears the
// child's result, so AdvanceJob fails and the error path classifies it. A re-queue
// that PRESERVES the payload lets AdvanceJob succeed while the row has moved, and
// then the confirm append is the only thing standing between a stale verdict and a
// debt recorded paid.
//
// MUTATION PROOF: make the confirm unconditional and the debt is closed for a
// lifecycle that no longer owns the row.
func TestSupersedeAdvanceRefusesALifecycleMovedDuringASuccessfulAdvance(t *testing.T) {
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
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: "continue"},
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
	// Re-arm the debt for the superseded run: the shape a failed first payment leaves.
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID: child, Kind: JobEventSupersedeFinalizePending,
		Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration),
	}); err != nil {
		t.Fatalf("arm the debt: %v", err)
	}

	interleaved := 0
	supersedeDebtInterleaveHook = func(hookCtx context.Context, at string) error {
		if at != supersedeDebtStageBeforeAdvance {
			return nil
		}
		interleaved++
		// A lifecycle bump that PRESERVES the payload, so AdvanceJob still has a
		// result to work from and succeeds.
		for _, state := range []JobState{JobQueued, JobRunning} {
			if err := store.UpdateJobState(hookCtx, child, string(state)); err != nil {
				t.Fatalf("UpdateJobState(%s): %v", state, err)
			}
		}
		return nil
	}
	t.Cleanup(func() { supersedeDebtInterleaveHook = nil })

	if _, err := engine.CompletePendingSupersedeFinalization(ctx, child); err != nil {
		t.Fatalf("CompletePendingSupersedeFinalization: %v", err)
	}
	if interleaved != 1 {
		t.Fatalf("interleave hook ran %d times, want 1", interleaved)
	}
	debt, err := latestSupersedeFinalizeDebt(ctx, store, child)
	if err != nil {
		t.Fatalf("latestSupersedeFinalizeDebt: %v", err)
	}
	if !debt.pending {
		t.Fatal("the debt was closed for a lifecycle that moved during its parent advance")
	}
	if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceConfirmed); got != 1 {
		// Exactly one: the fixture's own successful supersession confirmed once. The
		// stale payment must not add a second.
		t.Fatalf("%s events = %d, want 1 (the fixture's own, none from the stale payment)", JobEventSupersedeAdvanceConfirmed, got)
	}
	if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceSuperseded); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1 durable trace", JobEventSupersedeAdvanceSuperseded, got)
	}
}

// openEngineStoreAt is openEngineStore with a CALLER-CHOSEN path, so a test can
// open a second raw connection to the same file and inject a fault.
func openEngineStoreAt(t *testing.T, path string) *db.Store {
	t.Helper()
	store, err := dbtest.Open(t, path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func mustParseTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}
