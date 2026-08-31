package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// This file pins the shared-cleanup extraction on the #1673 supersede paths. Both
// terminal paths — SupersedeClosedPullRequestJob (top-level -> cancelled) and
// Engine.FinalizeClosedPullRequestDelegationChild (delegation child -> failed) —
// end a job OUTSIDE AdvanceJob, so each owes releaseAbortedJobResources. Without a
// test on each call site, removing either one leaks the job's resource locks, its
// per-delegation branch lock and its task lane lock in total silence: the sweep
// still reports a clean terminal row while the next same-repo work is refused.
//
// MUTATION 1 (semantic reversion): delete the releaseAbortedJobResources call from
// SupersedeClosedPullRequestJob and/or FinalizeClosedPullRequestDelegationChild.
// MUTATION 2: swap abortCauseSupersede for abortCauseCancel at either supersede
// site. Every assertion below is on a cause-rendered message or a lock row, so a
// PARTIAL reversion (one site, or one of the cleanups) is caught too.

// TestSupersedeClosedPullRequestJobReleasesAbortedResources covers the top-level
// path with the fixture shape TestCancelJobReleasesInactiveTaskLaneLock and
// TestCancelJobReleasesRuntimeSessionLock use: a queued task implement holding a
// runtime-session resource lock AND its task lane branch lock. A supersede must
// free both and say "supersede", not "cancel" — the wording is the audit trail an
// operator reads to find out which sweep took the lane.
func TestSupersedeClosedPullRequestJobReleasesAbortedResources(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "owner/repo")

	const branch = "feature/superseded-writer"
	const jobID = "job-superseded-writer"
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-superseded", RepoFullName: "owner/repo", State: string(TaskImplementing), Branch: branch,
	}); err != nil {
		t.Fatal(err)
	}
	acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: branch, Owner: "impl"})
	if err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	insertQueuedJob(t, store, db.Job{ID: jobID, Agent: "impl", Type: "implement", Repo: "owner/repo"}, JobPayload{
		Repo: "owner/repo", Branch: branch, TaskID: "task-superseded", PullRequest: 7, LeadAgent: "impl",
	})

	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	const lockKey = "runtime:codex:session-superseded"
	locked, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  jobID,
		OwnerToken:  "token-1",
		ExpiresAt:   now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !locked {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", locked, err)
	}

	job, superseded, err := SupersedeClosedPullRequestJob(ctx, store, jobID,
		"queued implement job superseded: owner/repo pull request #7 is no longer open")
	if err != nil {
		t.Fatalf("SupersedeClosedPullRequestJob returned error: %v", err)
	}
	if !superseded || job.State != string(JobCancelled) {
		t.Fatalf("superseded=%t state=%q, want the queued leg cancelled", superseded, job.State)
	}

	// Cleanup 1: the resource lock. A stranded runtime-session lock makes the next
	// job on that session wait out the full TTL.
	if _, err := store.GetResourceLock(ctx, lockKey); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetResourceLock after supersede error = %v, want sql.ErrNoRows (the superseded job leaked its runtime-session lock)", err)
	}
	reacquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  "job-next",
		OwnerToken:  "token-2",
		ExpiresAt:   now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("second AcquireResourceLock returned error: %v", err)
	}
	if !reacquired {
		t.Fatal("the next job could not re-acquire the superseded job's runtime-session lock")
	}

	// Cleanup 2: the task lane lock (#1565). Held, it refuses every later
	// same-branch dispatch.
	if _, err := store.GetBranchLock(ctx, "owner/repo", branch); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBranchLock after supersede error = %v, want sql.ErrNoRows (the superseded job leaked its task lane lock)", err)
	}

	// The wording is the discriminator between the two abort causes: a supersede
	// must not be filed in the audit trail as an operator cancellation.
	lockEvents, err := store.ListBranchLockEvents(ctx, "owner/repo", branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(lockEvents) != 1 || lockEvents[0].Kind != "released" {
		t.Fatalf("branch lock events = %+v, want exactly one release", lockEvents)
	}
	if !strings.Contains(lockEvents[0].Message, "supersession") {
		t.Fatalf("branch lock release message = %q, want the supersession cause named", lockEvents[0].Message)
	}
	if strings.Contains(lockEvents[0].Message, "cancellation") {
		t.Fatalf("branch lock release message = %q, blames a cancellation for a supersede", lockEvents[0].Message)
	}

	laneEvent := jobEventMessage(t, store, jobID, "task_lane_lock_released")
	if laneEvent == "" {
		t.Fatal("no task_lane_lock_released job event: the supersede did not record freeing the lane")
	}
	if !strings.Contains(laneEvent, "on supersede") {
		t.Fatalf("task_lane_lock_released message = %q, want \"on supersede\"", laneEvent)
	}

	// Task state and the dismissal audit stay with the stale-task reconciler, which
	// owns remote cleanup — the supersede frees the lane and nothing else.
	task, err := store.GetTask(ctx, "task-superseded")
	if err != nil || task.State != string(TaskImplementing) {
		t.Fatalf("task after supersede = %+v err=%v, want implementing retained", task, err)
	}
	taskEvents, err := store.ListTaskEvents(ctx, "task-superseded")
	if err != nil || len(taskEvents) != 0 {
		t.Fatalf("task events after supersede = %+v err=%v, dismissal audit belongs to the stale-task reconciler", taskEvents, err)
	}
}

// TestFinalizeClosedPullRequestDelegationChildReleasesAbortedResources covers the
// OTHER call site, which the top-level test cannot reach: a queued ephemeral
// implement delegation child, holding the per-delegation branch lock
// AllocateDelegationWorktree took at dispatch (#617) plus a runtime-session lock.
//
// It reuses the #617 e2e fan-out (newBurst617Engine / fanOutEphemeralImplementBurst)
// because those legs are REAL dispatched children: ParentJobID set, queued, and
// carrying the DelegationID+WorktreePath+Branch payload releaseDelegationBranchLock
// gates on. A hand-rolled payload would pin the helper instead of the path.
//
// The branch-lock ROW alone is not a discriminating assertion here: the finalizer
// this path chains into (FinalizeTimedOutDelegationChild -> the engine's terminal
// cleanup) also releases that lock, just with a different message. The
// cause-rendered EVENT is what only releaseAbortedJobResources can write.
func TestFinalizeClosedPullRequestDelegationChildReleasesAbortedResources(t *testing.T) {
	ctx := context.Background()
	engine, store := newBurst617Engine(t)
	legIDs := fanOutEphemeralImplementBurst(t, engine, store, "burst-pr-closed", 2)
	if got := countBranchLocks(t, store, burst617Repo); got != 2 {
		t.Fatalf("after fan-out: %d branch locks held, want 2", got)
	}
	child := legIDs[0]

	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	const lockKey = "runtime:codex:session-child"
	locked, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  child,
		OwnerToken:  "token-1",
		ExpiresAt:   now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !locked {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", locked, err)
	}

	// The parent's failure_policy decides what a dead child means; the default
	// block_parent surfaces as a BlockedError, which is the DAG deciding, not the
	// sweep failing.
	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, child,
		"queued implement job superseded: "+burst617Repo+" pull request #7 is no longer open")
	var blocked BlockedError
	if err != nil && !errors.As(err, &blocked) {
		t.Fatalf("FinalizeClosedPullRequestDelegationChild returned error: %v", err)
	}
	if !finalized {
		t.Fatal("finalized = false, want the queued child terminated")
	}
	if mustJob(t, store, child).State != string(JobFailed) {
		t.Fatalf("child state = %q, want failed", mustJob(t, store, child).State)
	}

	// Cleanup 1: the resource lock the child still owned.
	if _, err := store.GetResourceLock(ctx, lockKey); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetResourceLock after the child was superseded error = %v, want sql.ErrNoRows (the child leaked its runtime-session lock)", err)
	}

	// Cleanup 2: the per-delegation branch lock, released BY THE SUPERSEDE and
	// recorded as such. The sibling leg's lock must survive — this path terminates
	// one child, not the burst.
	branchEvent := jobEventMessage(t, store, child, "delegation_branch_lock_released")
	if branchEvent == "" {
		t.Fatal("no delegation_branch_lock_released job event on the superseded child (#617 leak)")
	}
	if !strings.Contains(branchEvent, "on supersede") {
		t.Fatalf("delegation_branch_lock_released message = %q, want \"on supersede\"", branchEvent)
	}
	if strings.Contains(branchEvent, "on cancel") {
		t.Fatalf("delegation_branch_lock_released message = %q, blames a cancel for a supersede", branchEvent)
	}
	if got := countBranchLocks(t, store, burst617Repo); got != 1 {
		t.Fatalf("%d branch locks held after one child was superseded, want 1 (the untouched sibling leg)", got)
	}
}
