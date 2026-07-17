package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

type WorktreeManager interface {
	AddWorktree(ctx context.Context, branch string, path string, base string) error
}

type ExistingBranchWorktreeManager interface {
	AddExistingBranchWorktree(ctx context.Context, branch string, path string) error
}

type BranchExistenceChecker interface {
	BranchExists(ctx context.Context, branch string) (bool, error)
}

// ReadOnlyWorktreeManager allocates and disposes throwaway detached worktrees
// for read-only (ask/review) delegation fan-out. Unlike implement worktrees
// these carry no branch and no branch lock: the worker only reads the checkout,
// so the worktree exists solely to give concurrent same-repo read-only siblings
// distinct checkout keys (otherwise they serialize on the shared repo checkout).
// The checkout-bound gitutil.Client satisfies this interface.
type ReadOnlyWorktreeManager interface {
	AddDetachedWorktree(ctx context.Context, path string, ref string) error
	RemoveWorktreeForce(ctx context.Context, path string) error
}

// BranchDeleter deletes a local branch. The checkout-bound gitutil.Client
// satisfies it; used to tear down a terminal implement delegation's branch.
type BranchDeleter interface {
	DeleteBranch(ctx context.Context, branch string) error
}

// IntegrationWorktreeManager builds a detached worktree off the parent base and
// merges the per-delegation branches of succeeded implement legs into it, so a
// dependent verify/review step sees the legs' combined work instead of the base
// checkout (issue #332). The detached worktree carries no branch and no branch
// lock, so it is disposed by the same read-only cleanup as fan-out worktrees.
type IntegrationWorktreeManager interface {
	AddDetachedWorktree(ctx context.Context, path string, ref string) error
	MergeBranches(ctx context.Context, dir string, branches []string, message string) error
}

// WorktreeCommitter commits an implement delegation leg's work to its own branch
// on success, so the leg's changes are available on its branch for a dependent
// integration step (#332) even in a PR-less local orchestrate where the task/PR
// finalizer never runs. The checkout-bound gitutil.Client satisfies it.
type WorktreeCommitter interface {
	CommitWorktree(ctx context.Context, dir string, message string) (bool, error)
}

type TaskWorktreeRequest struct {
	Home       string
	Repo       string
	TaskID     string
	GoalID     string
	TaskTitle  string
	Branch     string
	BaseBranch string
	Owner      string
	Checkout   string
}

func (e Engine) AllocateTaskWorktree(ctx context.Context, request TaskWorktreeRequest, manager WorktreeManager) (db.Task, error) {
	if err := e.validate(); err != nil {
		return db.Task{}, err
	}
	if manager == nil {
		return db.Task{}, errors.New("worktree manager is required")
	}
	if strings.TrimSpace(request.TaskID) == "" {
		return db.Task{}, errors.New("task worktree task id is required")
	}
	if strings.TrimSpace(request.Branch) == "" {
		return db.Task{}, errors.New("task worktree branch is required")
	}
	if strings.TrimSpace(request.Owner) == "" {
		return db.Task{}, errors.New("task worktree owner is required")
	}
	path, err := TaskWorktreePath(request.Home, request.Repo, request.TaskID)
	if err != nil {
		return db.Task{}, err
	}
	task, err := e.Store.GetTask(ctx, request.TaskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return db.Task{}, err
		}
		task = db.Task{ID: request.TaskID, RepoFullName: request.Repo, State: string(TaskPlanned)}
	}
	if task.State == string(TaskDismissed) {
		return db.Task{}, fmt.Errorf("task %s is dismissed; recover it explicitly before allocating a worktree", request.TaskID)
	}
	if strings.TrimSpace(task.RepoFullName) != "" && task.RepoFullName != request.Repo {
		return db.Task{}, fmt.Errorf("task %s belongs to repo %s, not %s", request.TaskID, task.RepoFullName, request.Repo)
	}
	existing, err := e.Store.GetTaskByRepoBranch(ctx, request.Repo, request.Branch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return db.Task{}, err
	}
	if err == nil && existing.ID != request.TaskID {
		return db.Task{}, errors.New("task branch is already assigned to another task")
	}
	lock := db.BranchLock{RepoFullName: request.Repo, Branch: request.Branch, Owner: request.Owner}
	createdLock, err := e.Store.CreateLock(ctx, lock)
	if err != nil {
		return db.Task{}, err
	}
	if !createdLock {
		existingLock, err := e.Store.GetBranchLock(ctx, request.Repo, request.Branch)
		if err != nil {
			return db.Task{}, err
		}
		if existingLock.Owner != request.Owner {
			return db.Task{}, BlockedError{Reason: "branch lock rejected action for " + request.Branch}
		}
	}
	if task.Branch == request.Branch && task.WorktreePath == path {
		task.State = string(TaskImplementing)
		if err := e.Store.UpsertTask(ctx, task); err != nil {
			if createdLock {
				_, _ = e.Store.ReleaseLock(ctx, lock)
			}
			return db.Task{}, err
		}
		return task, nil
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, request.Checkout, "worktree:"+request.TaskID, time.Now().UTC())
	if err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, err
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	if err := addTaskWorktree(ctx, manager, request.Branch, path, request.BaseBranch); err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, err
	}
	taskGoalID := task.GoalID
	if taskGoalID == "" {
		taskGoalID = request.GoalID
	}
	taskTitle := task.Title
	if taskTitle == "" {
		taskTitle = request.TaskTitle
	}
	task = db.Task{
		ID:           request.TaskID,
		RepoFullName: request.Repo,
		GoalID:       taskGoalID,
		Title:        taskTitle,
		State:        string(TaskImplementing),
		Branch:       request.Branch,
		WorktreePath: path,
	}
	if err := e.Store.UpsertTask(ctx, task); err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, err
	}
	return task, nil
}

// DelegationWorktreeRequest carries the inputs needed to allocate a git
// worktree for a delegated implement job. Unlike TaskWorktreeRequest it does not
// touch the tasks table; the resulting path and branch are returned to the
// dispatcher for storage in the child JobPayload.
type DelegationWorktreeRequest struct {
	Home         string
	Repo         string
	ParentJobID  string
	DelegationID string
	Delegation   Delegation
	BaseBranch   string
	Owner        string
	Checkout     string
	// RetryAttempt is the 1-based retry number for a re-enqueued delegation. It
	// is 0 for the original attempt. A non-zero value gives the retry an isolated
	// worktree path and branch so it never collides with the failed attempt's
	// still-present worktree directory and checked-out branch.
	RetryAttempt int
}

// DelegationWorktreeResult is the allocated worktree path and branch for a
// delegated implement job.
type DelegationWorktreeResult struct {
	Path   string
	Branch string
}

// AllocateDelegationWorktree creates an isolated git worktree for a delegated
// implement job. It mirrors AllocateTaskWorktree's lock ordering (branch lock,
// then checkout mutation lock, then the git worktree add) but writes nothing to
// the tasks table: the deterministic path and computed branch are returned so
// the dispatcher can store them in the child JobPayload. Two delegations from
// the same parent get distinct paths and branches.
func (e Engine) AllocateDelegationWorktree(ctx context.Context, request DelegationWorktreeRequest, manager WorktreeManager) (DelegationWorktreeResult, error) {
	if err := e.validate(); err != nil {
		return DelegationWorktreeResult{}, err
	}
	if manager == nil {
		return DelegationWorktreeResult{}, errors.New("worktree manager is required")
	}
	if strings.TrimSpace(request.ParentJobID) == "" {
		return DelegationWorktreeResult{}, errors.New("delegation worktree parent job id is required")
	}
	if strings.TrimSpace(request.DelegationID) == "" {
		return DelegationWorktreeResult{}, errors.New("delegation worktree delegation id is required")
	}
	if strings.TrimSpace(request.Owner) == "" {
		return DelegationWorktreeResult{}, errors.New("delegation worktree owner is required")
	}
	path, err := DelegationWorktreePath(request.Home, request.Repo, request.ParentJobID, request.DelegationID, request.RetryAttempt)
	if err != nil {
		return DelegationWorktreeResult{}, err
	}
	branch := delegationBranchName(request.Delegation, request.ParentJobID, request.DelegationID, request.RetryAttempt)
	if strings.TrimSpace(branch) == "" {
		return DelegationWorktreeResult{}, errors.New("delegation worktree branch could not be derived")
	}
	lock := db.BranchLock{RepoFullName: request.Repo, Branch: branch, Owner: request.Owner}
	createdLock, err := e.Store.CreateLock(ctx, lock)
	if err != nil {
		return DelegationWorktreeResult{}, err
	}
	if !createdLock {
		existingLock, err := e.Store.GetBranchLock(ctx, request.Repo, branch)
		if err != nil {
			return DelegationWorktreeResult{}, err
		}
		if existingLock.Owner != request.Owner {
			return DelegationWorktreeResult{}, BlockedError{Reason: "branch lock rejected action for " + branch}
		}
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, request.Checkout, "worktree:"+request.ParentJobID+"/"+request.DelegationID, time.Now().UTC())
	if err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return DelegationWorktreeResult{}, err
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	if err := addTaskWorktree(ctx, manager, branch, path, request.BaseBranch); err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return DelegationWorktreeResult{}, err
	}
	return DelegationWorktreeResult{Path: path, Branch: branch}, nil
}

// ReadOnlyWorktreeDispatchLockWaitBudget bounds how long the SCHEDULER-LOOP
// read-only worktree allocators (#739 dispatch-time isolation and the reactive
// pool-isolation dispatcher) wait for the checkout mutation lock before failing
// open. They run synchronously on the per-repo dispatch/poll loop, so the full
// checkoutMutationWaitTimeout (2m) would stall that repo's dispatch AND reap for up
// to two minutes whenever a same-repo merge gate holds the lock. Isolation is a
// throughput optimization, not correctness: a miss just serializes the seat (which
// the next tick retries), so a short, bounded wait is the right trade.
const ReadOnlyWorktreeDispatchLockWaitBudget = 5 * time.Second

// AllocateReadOnlyWorktree is the shared, package-level primitive that creates a
// detached, branch-lock-free git worktree at the deterministic
// DelegationWorktreePath(home, repo, pathParent, pathSegment, retryAttempt),
// holding the checkout mutation lock (a detached `git worktree add` mutates the
// shared .git) but taking NO branch lock and creating NO branch: a read-only
// worker owns nothing to merge. The ref defaults to baseBranch, else HEAD (always
// resolvable), and every failure is returned LOUDLY. It is the single source of
// truth for both the read-only delegation fan-out
// (AllocateReadOnlyDelegationWorktree) and the top-level dispatch-time read-only
// isolation (#739), so the two paths stay behaviorally aligned. It takes an
// explicit *db.Store rather than an Engine so the cli dispatch layer can call it
// without an import cycle. The lock key mirrors the delegation path
// ("worktree:<pathParent>/<pathSegment>") so distinct owners never collide.
//
// lockWaitBudget bounds how long it waits for the checkout mutation lock before
// returning a BlockedError. The read-only DELEGATION fan-out passes the full
// checkoutMutationWaitTimeout (it runs inside an already-dispatched worker, off
// any scheduler loop). The two HOT-PATH callers — the #739 dispatch-time
// allocation and the reactive pool-isolation dispatcher — run SYNCHRONOUSLY on the
// per-repo dispatch/poll loop, so they pass the much shorter
// ReadOnlyWorktreeDispatchLockWaitBudget: under merge-gate lock contention the full
// 2-minute wait would freeze that repo's whole dispatch+reap loop, and isolation is
// a fail-open throughput optimization (a miss just serializes the seat, which the
// next tick retries) — never worth stalling the scheduler.
func AllocateReadOnlyWorktree(ctx context.Context, store *db.Store, home string, repo string, checkout string, pathParent string, pathSegment string, retryAttempt int, baseBranch string, lockWaitBudget time.Duration, manager ReadOnlyWorktreeManager) (string, error) {
	if store == nil {
		return "", errors.New("read-only worktree store is required")
	}
	if manager == nil {
		return "", errors.New("read-only worktree manager is required")
	}
	if strings.TrimSpace(pathParent) == "" {
		return "", errors.New("read-only worktree path parent is required")
	}
	if strings.TrimSpace(pathSegment) == "" {
		return "", errors.New("read-only worktree path segment is required")
	}
	path, err := DelegationWorktreePath(home, repo, pathParent, pathSegment, retryAttempt)
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(baseBranch)
	if ref == "" {
		ref = "HEAD"
	}
	if lockWaitBudget <= 0 {
		lockWaitBudget = checkoutMutationWaitTimeout
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWaitBudget(ctx, store, checkout, "worktree:"+strings.TrimSpace(pathParent)+"/"+strings.TrimSpace(pathSegment), time.Now().UTC(), lockWaitBudget, checkoutMutationWaitBackoff)
	if err != nil {
		return "", err
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	// Ensure the parent chain exists before `git worktree add` (matches the reactive
	// pool-isolation path); the leaf is created by git itself.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := manager.AddDetachedWorktree(ctx, path, ref); err != nil {
		return "", err
	}
	return path, nil
}

// AllocateReadOnlyDelegationWorktree creates a detached, branch-lock-free git
// worktree for a read-only (ask/review) delegation child so it does not
// serialize with its same-repo siblings on the shared repo checkout key. It
// reuses the deterministic DelegationWorktreePath and the checkout mutation lock
// (a detached `git worktree add` mutates the shared .git) but takes no branch
// lock and creates no branch: a read-only child owns nothing to merge. The
// worktree is disposed by cleanupReadOnlyDelegationWorktree once the child job
// reaches a terminal state. It is a thin Engine wrapper over the shared
// AllocateReadOnlyWorktree primitive: the delegation-specific validation (a
// present parent+delegation id) is kept here so its error messages are unchanged.
func (e Engine) AllocateReadOnlyDelegationWorktree(ctx context.Context, request DelegationWorktreeRequest, manager ReadOnlyWorktreeManager) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	if manager == nil {
		return "", errors.New("read-only worktree manager is required")
	}
	if strings.TrimSpace(request.ParentJobID) == "" {
		return "", errors.New("delegation worktree parent job id is required")
	}
	if strings.TrimSpace(request.DelegationID) == "" {
		return "", errors.New("delegation worktree delegation id is required")
	}
	return AllocateReadOnlyWorktree(ctx, e.Store, request.Home, request.Repo, request.Checkout, request.ParentJobID, request.DelegationID, request.RetryAttempt, request.BaseBranch, checkoutMutationWaitTimeout, manager)
}

// AllocateIntegrationWorktree creates a detached worktree off the parent base
// branch and sequentially merges the given succeeded implement-leg branches into
// it, so a dependent read-only step (a decompose-and-verify verify gate) sees the
// legs' combined work rather than the base checkout (issue #332). The worktree is
// keyed on a synthetic "integration-<delegation-id>" so it never collides with
// the dependent's own id, carries no branch/branch lock, and is disposed by the
// same read-only cleanup as fan-out worktrees. A merge conflict means the
// decomposition was not actually file-disjoint: it is returned as a BlockedError
// so the caller blocks the parent rather than auto-resolving.
func (e Engine) AllocateIntegrationWorktree(ctx context.Context, request DelegationWorktreeRequest, legBranches []string, manager IntegrationWorktreeManager) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	if manager == nil {
		return "", errors.New("integration worktree manager is required")
	}
	if strings.TrimSpace(request.ParentJobID) == "" {
		return "", errors.New("delegation worktree parent job id is required")
	}
	if strings.TrimSpace(request.DelegationID) == "" {
		return "", errors.New("delegation worktree delegation id is required")
	}
	if len(legBranches) == 0 {
		return "", errors.New("integration worktree requires at least one leg branch")
	}
	integrationID := "integration-" + request.DelegationID
	path, err := DelegationWorktreePath(request.Home, request.Repo, request.ParentJobID, integrationID, request.RetryAttempt)
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(request.BaseBranch)
	if ref == "" {
		ref = "HEAD"
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, request.Checkout, "worktree:"+request.ParentJobID+"/"+integrationID, time.Now().UTC())
	if err != nil {
		return "", err
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	// A delegation is dispatched once (advanceDelegations skips already-enqueued
	// dependents; retries use a retry-suffixed path), so allocate a fresh detached
	// worktree like the implement and read-only paths rather than reusing one.
	if err := manager.AddDetachedWorktree(ctx, path, ref); err != nil {
		return "", err
	}
	msg := "Gitmoot integration merge for delegation " + request.DelegationID
	if err := manager.MergeBranches(ctx, path, legBranches, msg); err != nil {
		return "", BlockedError{Reason: fmt.Sprintf("integration merge for delegation %q failed (decomposition is not file-disjoint): %v", request.DelegationID, err)}
	}
	return path, nil
}

// readOnlyDelegationAction reports whether a delegation action runs read-only.
// implement is the only write action (it mutates a branch and merges); every
// other action (ask, review) only reads the checkout.
func readOnlyDelegationAction(action string) bool {
	a := strings.ToLower(strings.TrimSpace(action))
	return a != "" && a != "implement"
}

// readOnlyFanoutNeedsWorktree reports whether read-only delegation d should run
// in its own detached worktree to avoid serializing with its siblings. It is
// true only when d is read-only and the coordinator emitted >=2 read-only
// delegations: all delegation children inherit the parent repo, so >=2 read-only
// siblings otherwise collapse to the same repo:<repo> checkout key and run
// one-at-a-time. A single read-only delegation stays in the shared checkout (a
// worktree would be pure overhead with no parallelism to gain).
func readOnlyFanoutNeedsWorktree(payload JobPayload, d Delegation) bool {
	if !readOnlyDelegationAction(d.Action) {
		return false
	}
	if payload.Result == nil {
		return false
	}
	count := 0
	for _, sib := range payload.Result.Delegations {
		if readOnlyDelegationAction(sib.Action) {
			count++
			if count >= 2 {
				return true
			}
		}
	}
	return false
}

// readOnlyWorktreeContextNote returns a deterministic prompt appendix warning a
// read-only fan-out delegation child that its detached worktree is the COMMITTED
// TIP of the base branch and therefore omits gitignored paths (e.g. vendored
// clones under repos/**) and any uncommitted working-tree changes. It points the
// child at the canonical base checkout so an analysis task reads those files
// there instead of silently reporting a working-tree feature as absent (#654).
// It returns "" for a blank baseCheckout so the ask-path and any engine that does
// not set Engine.DelegationCheckout produce byte-identical prompts. The text is
// built only from static strings and baseCheckout, so a re-dispatch/retry
// recomputes it identically — required by the idempotent-enqueue payload-equality
// check (payloadMatchesRequest compares Instructions); this mirrors the #419
// upstream-context append.
func readOnlyWorktreeContextNote(baseCheckout string) string {
	base := strings.TrimSpace(baseCheckout)
	if base == "" {
		return ""
	}
	return "\n\nWorktree context (read-only): you are running in a detached git worktree checked out at the COMMITTED TIP of the base branch. It does NOT contain gitignored paths (for example vendored clones under repos/**) or any uncommitted working-tree changes. If a path this task references is absent from your working directory, read it from the canonical base checkout at " + base + " before concluding it is missing — do not report a working-tree feature as absent without checking there. This is a read-only analysis; do not write outside your worktree."
}

// ReadOnlyWorktreeContextNote is the exported entry point to
// readOnlyWorktreeContextNote for callers outside the workflow package — namely
// the daemon's top-level pool-isolation path (#696), which auto-isolates a
// contended top-level read-only (ask/review) job into a detached committed-tip
// worktree exactly as read-only delegation fan-out does (#394 part 2) and must
// append the identical #654 note so an isolated analysis job is pointed at the
// canonical checkout for gitignored/uncommitted paths. It is a thin pass-through
// so the delegation and top-level paths share one source of truth for the text;
// a blank baseCheckout yields "" (byte-identical, no note).
func ReadOnlyWorktreeContextNote(baseCheckout string) string {
	return readOnlyWorktreeContextNote(baseCheckout)
}

// isReadOnlyDelegationWorktree reports whether a job ran in a detached read-only
// worktree that the terminal cleanup must dispose. Two disjoint shapes qualify:
//
//  1. A TOP-LEVEL read-only (ask) worktree allocated at DISPATCH time (#739),
//     flagged by the explicit payload.ReadOnlyWorktree marker. It carries NO
//     DelegationID, so without the marker the delegation-gated branch below would
//     orphan it. The marker is set ONLY at the dispatch allocation site and ONLY
//     for ask/review, so it can never be an implement/task worktree.
//  2. A read-only DELEGATION child: a read-only action under a DelegationID with a
//     WorktreePath. implement children carry a Branch and are cleaned through the
//     merge gate (isImplementDelegationWorktree), so they are excluded here.
//
// Preferring the explicit marker over an implicit heuristic keeps implement/task
// worktrees (marker false, Branch set) from ever matching.
func isReadOnlyDelegationWorktree(jobType string, payload JobPayload) bool {
	if strings.TrimSpace(payload.WorktreePath) == "" {
		return false
	}
	if payload.ReadOnlyWorktree {
		return true
	}
	return strings.TrimSpace(payload.DelegationID) != "" &&
		readOnlyDelegationAction(jobType)
}

// cleanupReadOnlyDelegationWorktree disposes the detached worktree allocated for
// a read-only delegation child once the child job is terminal. It is best-effort
// and idempotent: a missing worktree (already removed on a prior advance, or
// never allocated) is logged, not fatal. Removal mutates the shared .git, so it
// holds the checkout mutation lock like allocation does.
func (e Engine) cleanupReadOnlyDelegationWorktree(ctx context.Context, jobID string, jobType string, payload JobPayload) {
	if !isReadOnlyDelegationWorktree(jobType, payload) {
		return
	}
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	if !ok || manager == nil {
		return
	}
	// Detach from the caller's cancellation: this runs on the child's terminal
	// AdvanceJob, which may carry a job context already cancelled by a run timeout.
	// The worktree must still be disposed, so keep context values but drop the
	// deadline/cancel.
	opCtx := context.WithoutCancel(ctx)
	path := strings.TrimSpace(payload.WorktreePath)
	// Idempotent: AdvanceJob can run more than once for a job (re-advance / retry
	// passes). If the worktree directory is already gone, do not re-lock or emit a
	// spurious cleanup-failed event.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return
	}
	if e.BeforeReadOnlyWorktreeCleanup != nil {
		if err := e.BeforeReadOnlyWorktreeCleanup(opCtx, jobID, jobType, payload); err != nil {
			_ = e.Store.AddJobEvent(opCtx, db.JobEvent{
				JobID: jobID, Kind: "readonly_worktree_precleanup_failed",
				Message: fmt.Sprintf("pre-cleanup hook failed before worktree disposal: %v", err),
			})
		}
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(opCtx, e.Store, e.DelegationCheckout, "worktree-cleanup:"+jobID, time.Now().UTC())
	if err != nil {
		// A transient failure (lock contention) must NOT be terminal: emit the same
		// reclaim marker the daemon's reclaimSkippedDelegationWorktrees pass keys on,
		// so ReclaimTerminalDelegationWorktree re-fires this cleanup on a later tick
		// rather than leaking the worktree. A bare delegation_worktree_cleanup_failed
		// is never re-selected by any pass (it is not the latest advance marker, and
		// the reclaim SQL only picks _skipped), so it would leak permanently (#739 review).
		e.recordReadOnlyCleanupSkippedOnce(opCtx, jobID, path, fmt.Sprintf("could not lock checkout: %v", err))
		return
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	if err := manager.RemoveWorktreeForce(opCtx, path); err != nil {
		e.recordReadOnlyCleanupSkippedOnce(opCtx, jobID, path, fmt.Sprintf("force-remove failed: %v", err))
		return
	}
	_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_removed", Message: fmt.Sprintf("read-only worktree %s removed", path)})
}

// isImplementDelegationWorktree reports whether a job ran in a per-delegation
// implement worktree (carries a branch) that must be torn down on terminal.
func isImplementDelegationWorktree(jobType string, payload JobPayload) bool {
	return strings.TrimSpace(payload.DelegationID) != "" &&
		strings.TrimSpace(payload.WorktreePath) != "" &&
		strings.TrimSpace(payload.Branch) != "" &&
		!readOnlyDelegationAction(jobType) // i.e. jobType == "implement"
}

// releaseDelegationBranchLock releases the branch lock a worktree-isolated
// implement delegation leg acquired in AllocateDelegationWorktree (#617), once
// the leg has reached a terminal state. It is force-scoped by (repo, branch): a
// gitmoot-delegation-<parent-short>-<id>[-retry-N] branch is unique to exactly one
// leg, so a force release cannot clobber another job's lock and is robust to any
// owner drift, while an owner-scoped release could silently miss a lock whose
// recorded owner no longer matches. It is gated by isImplementDelegationWorktree so
// it fires ONLY for worktree-isolated implement legs — whose Branch is a real
// per-delegation branch — and NEVER for the shared-checkout fallback leg, whose
// Branch is the PARENT branch the coordinator still owns. Best-effort and
// idempotent: a no-op (released=false, no branch_locks event) once the lock is gone
// or when the payload lacks the repo/branch identity. Returns whether a lock was
// actually released this call.
func releaseDelegationBranchLock(ctx context.Context, store *db.Store, jobType string, payload JobPayload) (bool, error) {
	if store == nil || !isImplementDelegationWorktree(jobType, payload) {
		return false, nil
	}
	repo := strings.TrimSpace(payload.Repo)
	branch := strings.TrimSpace(payload.Branch)
	if repo == "" || branch == "" {
		return false, nil
	}
	_, released, err := store.ForceReleaseLockWithEvent(ctx, repo, branch, db.BranchLockEvent{
		Kind:    "released",
		Message: "released after delegation leg reached a terminal state (#617)",
	})
	return released, err
}

// cleanupImplementDelegationWorktree disposes the per-delegation worktree AND
// deletes the gitmoot-delegation-* branch allocated for an implement delegation
// child once the child job is terminal, so they do not accumulate in the shared
// checkout and mislead a later coordinator (#478). It also releases the child's
// per-delegation branch lock, symmetric with AllocateDelegationWorktree (#617). It is best-effort and
// idempotent: an already-gone worktree+branch short-circuit to a no-op. Removal
// and branch deletion mutate the shared .git, so it holds the checkout mutation
// lock like allocation does. The worktree is removed FIRST so the branch is no
// longer checked out, then `git branch -D` can succeed.
func (e Engine) cleanupImplementDelegationWorktree(ctx context.Context, jobID string, jobType string, payload JobPayload) {
	if !isImplementDelegationWorktree(jobType, payload) {
		return
	}
	// #332 guard: a succeeded implement leg's branch is merged into a dependent
	// integration worktree (integrationDepBranches requires JobSucceeded). Do
	// NOT delete a succeeded leg whose branch a sibling lists in Deps, or a
	// pending integration would fail to merge it. Failed/blocked legs are never
	// merged, so they are always safe to clean.
	if payload.Result != nil && payload.Result.Decision == "implemented" &&
		e.implementLegBranchMayBeMerged(ctx, payload) {
		return
	}
	// Liveness gate (#536): NEVER force-remove a worktree (and delete its branch)
	// while a live runtime worker could still be writing to it — even past lease
	// expiry. Two independent signals each block the destructive removal:
	//
	//  1. runtimeOwnerActive: a FOREIGN runtime-session lock whose LEASE is unexpired
	//     (the job's timeout has not elapsed). On a healthy terminal the run's OWN
	//     lock is still held here (the daemon releases it only after RunJob ->
	//     AdvanceJob returns) but is excluded by owner token via the run context, so
	//     cleanup proceeds unchanged. A DIFFERENT worker's unexpired lease — the
	//     stale-recovery / dirty-checkout-validation window — blocks it.
	//  2. worktreeHasLiveProcess: a live process whose cwd is inside the worktree.
	//     This is the post-lease-expiry backstop (#536 finding 1): once a crashed
	//     daemon's worker outlives its lease, the lock is reaped and gate (1) no
	//     longer fires, but the reparented worker can still be writing. Removing the
	//     worktree then would orphan it onto a deleted cwd — the original #536
	//     corruption shifted to the lease boundary. This probe is lock-independent
	//     and PID-reuse-/hostname-rename-immune, so it holds where gate (1) cannot.
	//
	// In either case the dirty worktree is PRESERVED for salvage rather than clobbered.
	// The daemon's reclaimSkippedDelegationWorktrees pass re-fires this cleanup on a
	// later tick; once the foreign lease expires AND no live process holds the
	// worktree (the worker has actually exited), it is reclaimed rather than leaked.
	if skip, reason := e.cleanupBlockedByLiveOwner(ctx, jobID, payload); skip {
		e.recordCleanupSkippedOnce(ctx, jobID, payload, reason)
		return
	}
	// Detach from the caller's cancellation: this runs on the child's terminal
	// AdvanceJob, which may carry a job context already cancelled by a run timeout.
	// The worktree must still be disposed, so keep context values but drop the
	// deadline/cancel.
	opCtx := context.WithoutCancel(ctx)
	branch := strings.TrimSpace(payload.Branch)
	// #617: release the per-delegation branch lock now that this leg is terminal and
	// has cleared the preserve-guards above (no integration consumer still needs its
	// branch, no live runtime owner may still push). Symmetric with the CreateLock
	// AllocateDelegationWorktree took at dispatch — an ephemeral leg's owner process
	// is gone by the time it is terminal, so nothing else would ever release it. The
	// leak stranded a gitmoot-delegation-* lock on EVERY terminal state (success
	// included), and the next same-repo burst mis-read those stale locks as live
	// workers and was refused. This is a pure branch_locks DELETE (no checkout
	// mutation lock needed), placed BEFORE the worktree-manager and on-disk
	// idempotency checks below so the lock is reclaimed even when no manager is wired
	// or the worktree/branch are already gone. Idempotent: once released it is a
	// no-op and emits nothing further.
	if released, rerr := releaseDelegationBranchLock(opCtx, e.Store, jobType, payload); rerr != nil {
		_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("delegation branch lock %s release failed: %v", branch, rerr)})
	} else if released {
		_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_branch_lock_released", Message: fmt.Sprintf("released delegation branch lock %s after terminal state (#617)", branch)})
	}
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager) // RemoveWorktreeForce
	if !ok || manager == nil {
		return
	}
	deleter, _ := e.DelegationWorktrees.(BranchDeleter)
	checker, _ := e.DelegationWorktrees.(BranchExistenceChecker)
	path := strings.TrimSpace(payload.WorktreePath)
	// Idempotency: short-circuit (no lock, no spurious event) once there is nothing
	// left to do. The pending work is (a) removing the worktree if it still exists
	// and (b) deleting the branch if a BranchDeleter is wired and the branch is not
	// already gone. A branch delete can only be pending when BOTH a deleter and a
	// checker are available: without a checker we cannot prove the branch survived,
	// so a `git branch -D` on every re-advance would error on a missing branch and
	// emit a spurious cleanup_failed event. In that case (and when no deleter
	// exists at all) treat an already-removed worktree as sufficient.
	_, statErr := os.Stat(path)
	worktreeGone := os.IsNotExist(statErr)
	branchKnownGone := false
	if checker != nil {
		if exists, err := checker.BranchExists(opCtx, branch); err == nil {
			branchKnownGone = !exists
		}
	}
	branchCleanupPending := deleter != nil && checker != nil && !branchKnownGone
	if worktreeGone && !branchCleanupPending {
		return
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(opCtx, e.Store, e.DelegationCheckout, "worktree-cleanup:"+jobID, time.Now().UTC())
	if err != nil {
		_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("implement worktree %s cleanup could not lock checkout: %v", path, err)})
		return
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	if !worktreeGone {
		if err := manager.RemoveWorktreeForce(opCtx, path); err != nil {
			_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("implement worktree %s force-remove failed: %v", path, err)})
			return
		}
	}
	branchDeleted := false
	if deleter != nil && !branchKnownGone {
		if err := deleter.DeleteBranch(opCtx, branch); err != nil {
			_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("implement branch %s delete failed: %v", branch, err)})
			return
		}
		branchDeleted = true
	}
	// Only claim the branch was removed when a delete actually occurred this pass
	// (or the checker confirmed it already gone). With no BranchDeleter the branch
	// is intentionally kept, so the event must not say it was removed.
	message := fmt.Sprintf("implement worktree %s removed", path)
	if branchDeleted || branchKnownGone {
		message = fmt.Sprintf("implement worktree %s and branch %s removed", path, branch)
	}
	_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_removed", Message: message})
}

// cleanupBlockedByLiveOwner reports whether the destructive implement-delegation
// cleanup for jobID must be REFUSED because a live runtime worker could still be
// writing to the worktree, and a short reason. It composes the two never-clobber
// signals (#536): an active FOREIGN runtime-session lock (unexpired lease), and a
// live process whose cwd is inside the worktree (the post-lease-expiry backstop).
func (e Engine) cleanupBlockedByLiveOwner(ctx context.Context, jobID string, payload JobPayload) (bool, string) {
	if active, reason := e.runtimeOwnerActive(ctx, jobID); active {
		return true, fmt.Sprintf("runtime owner still active (%s)", reason)
	}
	if path := strings.TrimSpace(payload.WorktreePath); path != "" && e.worktreeHasLiveProcess(path) {
		return true, fmt.Sprintf("a live process still has its cwd in worktree %s", path)
	}
	return false, ""
}

// recordCleanupSkippedOnce emits a delegation_worktree_cleanup_skipped event, but
// at most once per preserve window (#536 finding 3): reclaimSkippedDelegationWorktrees
// re-fires the cleanup every 1s tick for the whole lease duration while the owner
// stays active, so emitting a fresh event each time would grow the job event log
// without bound (and make every ListJobEvents scan O(n^2)). If the LAST cleanup
// outcome event is already a skip, this is a no-op; a later delegation_worktree_removed
// closes the window so a subsequent (genuinely new) skip would emit again.
func (e Engine) recordCleanupSkippedOnce(ctx context.Context, jobID string, payload JobPayload, reason string) {
	if e.lastCleanupOutcomeIsSkip(ctx, jobID) {
		return
	}
	_ = e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{
		JobID:   jobID,
		Kind:    "delegation_worktree_cleanup_skipped",
		Message: fmt.Sprintf("implement worktree %s cleanup skipped: %s", strings.TrimSpace(payload.WorktreePath), reason),
	})
}

// recordReadOnlyCleanupSkippedOnce emits the reclaim-eligible
// delegation_worktree_cleanup_skipped marker for a read-only worktree whose
// terminal disposal FAILED transiently (lock contention / force-remove error). It
// is the read-only twin of recordCleanupSkippedOnce: without it a bare
// delegation_worktree_cleanup_failed is never re-selected (the reclaim SQL keys on
// _skipped, and the advance-retry pass keys on advance markers), so the worktree
// would leak. Deduped by lastCleanupOutcomeIsSkip so a persistently-failing removal
// does not grow the event log without bound; a later delegation_worktree_removed
// closes the window.
func (e Engine) recordReadOnlyCleanupSkippedOnce(ctx context.Context, jobID string, path string, reason string) {
	if e.lastCleanupOutcomeIsSkip(ctx, jobID) {
		return
	}
	_ = e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{
		JobID:   jobID,
		Kind:    "delegation_worktree_cleanup_skipped",
		Message: fmt.Sprintf("read-only worktree %s cleanup skipped: %s", strings.TrimSpace(path), reason),
	})
}

// lastCleanupOutcomeIsSkip reports whether the most recent terminal-cleanup outcome
// event for jobID is a skip (preserve) not yet followed by a removal — i.e. another
// skip would be redundant. Order matters (a worktree can be preserved, then later
// removed), so the LAST of the two kinds wins.
func (e Engine) lastCleanupOutcomeIsSkip(ctx context.Context, jobID string) bool {
	events, err := e.Store.ListJobEvents(context.WithoutCancel(ctx), jobID)
	if err != nil {
		return false
	}
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Kind {
		case "delegation_worktree_cleanup_skipped":
			return true
		case "delegation_worktree_removed":
			return false
		}
	}
	return false
}

// ReclaimTerminalDelegationWorktree re-attempts the terminal worktree cleanup for
// a job whose earlier disposal was DEFERRED, keyed by a
// delegation_worktree_cleanup_skipped marker that the daemon's
// reclaimSkippedDelegationWorktrees pass selects on. Two shapes reach here:
//
//   - an implement child PRESERVED because a foreign runtime owner was still active
//     (#536): reclaimed once the owner's lock releases or its lease expires;
//   - a read-only worktree (top-level ask #739, or a read-only delegation child)
//     whose disposal was deferred — either its terminal cleanup hit transient lock
//     contention / a force-remove error (cleanupReadOnlyDelegationWorktree now
//     records a _skipped marker on failure instead of a dead-end _cleanup_failed),
//     or the job was ABORTED (cancel/kill/supersede) before it ever ran and
//     recordReadOnlyWorktreeReclaimOnAbort marked its dispatch-allocated worktree.
//
// It re-runs BOTH idempotent, liveness-gated cleanups, so it is a no-op when the
// owner is still active, when the worktree is already gone, or for a job that
// allocated no worktree. Reachability is via the _skipped marker only: a pure crash
// in the sub-millisecond window between advance_completed and the deferred cleanup
// is NOT covered (that residual is shared with the implement path and unchanged by
// #739).
func (e Engine) ReclaimTerminalDelegationWorktree(ctx context.Context, jobID string) error {
	if err := e.validate(); err != nil {
		return err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return err
	}
	e.cleanupImplementDelegationWorktree(ctx, jobID, job.Type, payload)
	e.cleanupReadOnlyDelegationWorktree(ctx, jobID, job.Type, payload)
	return nil
}

// implementLegBranchMayBeMerged reports whether a succeeded implement leg's
// branch must be PRESERVED because a dependent integration step (#332) may still
// merge it. A sibling delegation that lists this leg in its Deps consumes the
// leg's branch (integrationDepBranches requires the leg JobSucceeded), but only
// until that consumer reaches a terminal state: once every consumer is terminal
// the merge has already run (or failed terminally) and the branch can be torn
// down (#478). cleanupConsumedImplementLegWorktrees re-fires this leg's cleanup
// from the consumer's terminal advance so an integration-fed leg is reclaimed
// rather than accumulating forever.
//
// Cleanup is DESTRUCTIVE (git branch -D + force worktree removal), so on any
// uncertainty it fails safe by returning true (preserve): a missing/unreadable
// parent result, a not-yet-dispatched consumer, or an inability to read consumer
// job states all mean "cannot prove the branch is unneeded". It returns false
// (safe to clean) only when the parent result was read AND either no sibling
// lists this leg in its Deps or every such consumer is terminal.
func (e Engine) implementLegBranchMayBeMerged(ctx context.Context, payload JobPayload) bool {
	parentID := strings.TrimSpace(payload.ParentJobID)
	if parentID == "" {
		return false
	}
	_, parentPayload, err := e.jobPayload(ctx, parentID)
	if err != nil || parentPayload.Result == nil {
		return true // cannot determine -> preserve (destructive op fails safe)
	}
	legID := strings.TrimSpace(payload.DelegationID)
	var consumerIDs []string
	for _, sib := range parentPayload.Result.Delegations {
		for _, dep := range sib.Deps {
			if strings.TrimSpace(dep) == legID {
				consumerIDs = append(consumerIDs, strings.TrimSpace(sib.ID))
				break
			}
		}
	}
	if len(consumerIDs) == 0 {
		return false // no integration consumer -> always safe to clean
	}
	children, err := e.childDelegationJobs(ctx, parentID)
	if err != nil {
		return true // cannot read consumer job states -> preserve
	}
	for _, consumerID := range consumerIDs {
		consumer, ok := children[consumerID]
		if !ok || !IsSettledJobState(consumer.State) {
			return true // consumer not yet dispatched or still running -> preserve
		}
	}
	return false // every consumer is terminal: the merge is done -> clean the leg
}

// cleanupConsumedImplementLegWorktrees tears down the per-delegation worktrees
// and branches of the implement legs that THIS now-terminal integration step
// consumed via its Deps (#332/#478). A leg's own terminal advance preserves its
// branch while a consumer is still pending/running (implementLegBranchMayBeMerged),
// and the merge gate only cleans the task worktree, so without this nothing would
// ever reclaim an integration-fed leg and its gitmoot-delegation-* branch and
// worktree would accumulate forever after the tree finished. It re-runs each
// leg's cleanup, whose #332 guard now observes this consumer terminal and (absent
// another live consumer) proceeds. Best-effort and idempotent: a no-op for a job
// with no parent/deps and for already-cleaned legs.
func (e Engine) cleanupConsumedImplementLegWorktrees(ctx context.Context, payload JobPayload) {
	parentID := strings.TrimSpace(payload.ParentJobID)
	deps := compactStrings(payload.Deps)
	if parentID == "" || len(deps) == 0 {
		return
	}
	children, err := e.childDelegationJobs(ctx, parentID)
	if err != nil {
		return
	}
	for _, dep := range deps {
		legJob, ok := children[strings.TrimSpace(dep)]
		if !ok {
			continue
		}
		legPayload, err := unmarshalPayload(legJob.Payload)
		if err != nil {
			continue
		}
		e.cleanupImplementDelegationWorktree(ctx, legJob.ID, legJob.Type, legPayload)
	}
}

func addTaskWorktree(ctx context.Context, manager WorktreeManager, branch string, path string, base string) error {
	if checker, ok := manager.(BranchExistenceChecker); ok {
		exists, err := checker.BranchExists(ctx, branch)
		if err != nil {
			return err
		}
		if exists {
			existingManager, ok := manager.(ExistingBranchWorktreeManager)
			if !ok {
				return errors.New("existing branch worktree manager is required")
			}
			return existingManager.AddExistingBranchWorktree(ctx, branch, path)
		}
	}
	return manager.AddWorktree(ctx, branch, path, base)
}

func TaskWorktreePath(home string, repo string, taskID string) (string, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return "", errors.New("task worktree home is required")
	}
	repoSegment, err := taskWorktreeRepoSegment(repo)
	if err != nil {
		return "", err
	}
	taskSegment, err := taskWorktreePathSegment(taskID, "task id")
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "worktrees", repoSegment, taskSegment), nil
}

// DelegationWorktreePath builds the deterministic on-disk worktree path for a
// delegated implement job:
// $GITMOOT_HOME/worktrees/<owner>--<repo>/delegations/<parent-job-id>/<delegation-id>/.
// A retryAttempt > 0 appends /retry/<n> so a re-enqueued delegation gets a fresh
// isolated directory rather than colliding with the failed attempt's worktree.
// It reuses the same repo/segment sanitization as TaskWorktreePath.
func DelegationWorktreePath(home string, repo string, parentJobID string, delegationID string, retryAttempt int) (string, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return "", errors.New("delegation worktree home is required")
	}
	repoSegment, err := taskWorktreeRepoSegment(repo)
	if err != nil {
		return "", err
	}
	parentSegment, err := taskWorktreePathSegment(parentJobID, "parent job id")
	if err != nil {
		return "", err
	}
	delegationSegment, err := taskWorktreePathSegment(delegationID, "delegation id")
	if err != nil {
		return "", err
	}
	base := filepath.Join(home, "worktrees", repoSegment, "delegations", parentSegment, delegationSegment)
	if retryAttempt > 0 {
		base = filepath.Join(base, "retry", strconv.Itoa(retryAttempt))
	}
	return base, nil
}

func taskWorktreeRepoSegment(repo string) (string, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid task worktree repo %q", repo)
	}
	ownerSegment, err := taskWorktreePathSegment(owner, "repo owner")
	if err != nil {
		return "", err
	}
	nameSegment, err := taskWorktreePathSegment(name, "repo name")
	if err != nil {
		return "", err
	}
	return ownerSegment + "--" + nameSegment, nil
}

func taskWorktreePathSegment(value string, label string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	// Already a safe single segment -> return it unchanged so existing worktree
	// paths are byte-identical (backward-compatible: no in-flight worktree moves).
	if isSafeWorktreeSegment(value) {
		return value, nil
	}
	// The value contains characters that are not path-safe -- most importantly
	// '/', which legitimately appears in a coordinator's *continuation* parent job
	// id (e.g. "local-ask-lead-abc123/continuation/continuation"). Rejecting it
	// outright made it impossible to dispatch an implement / integration-worktree
	// delegation from any continuation deeper than the root job, which breaks the
	// multi-round Orchestra coordinator pattern. Deterministically sanitize
	// instead: collapse each run of unsafe characters to '_' and append a short
	// hash of the ORIGINAL value so distinct ids can never collide on one path.
	// The result is a single, path-safe, traversal-safe directory segment.
	var b strings.Builder
	prevSep := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-', char == '_', char == '.':
			b.WriteRune(char)
			prevSep = false
		default:
			if !prevSep {
				b.WriteByte('_')
				prevSep = true
			}
		}
	}
	sanitized := strings.Trim(b.String(), "_.")
	if sanitized == "" {
		sanitized = "seg"
	}
	sum := sha256.Sum256([]byte(value))
	return sanitized + "-" + hex.EncodeToString(sum[:])[:12], nil
}

// isSafeWorktreeSegment reports whether value is already a safe single path
// segment: non-empty, not "." or "..", and composed only of [A-Za-z0-9._-].
// Such values are used verbatim so existing worktree paths never move.
func isSafeWorktreeSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_' || char == '.':
		default:
			return false
		}
	}
	return true
}
