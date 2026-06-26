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

	"github.com/jerryfane/gitmoot/internal/db"
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

// AllocateReadOnlyDelegationWorktree creates a detached, branch-lock-free git
// worktree for a read-only (ask/review) delegation child so it does not
// serialize with its same-repo siblings on the shared repo checkout key. It
// reuses the deterministic DelegationWorktreePath and the checkout mutation lock
// (a detached `git worktree add` mutates the shared .git) but takes no branch
// lock and creates no branch: a read-only child owns nothing to merge. The
// worktree is disposed by cleanupReadOnlyDelegationWorktree once the child job
// reaches a terminal state.
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
	path, err := DelegationWorktreePath(request.Home, request.Repo, request.ParentJobID, request.DelegationID, request.RetryAttempt)
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(request.BaseBranch)
	if ref == "" {
		ref = "HEAD"
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, request.Checkout, "worktree:"+request.ParentJobID+"/"+request.DelegationID, time.Now().UTC())
	if err != nil {
		return "", err
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	if err := manager.AddDetachedWorktree(ctx, path, ref); err != nil {
		return "", err
	}
	return path, nil
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

// isReadOnlyDelegationWorktree reports whether a job ran in a detached read-only
// delegation worktree that must be disposed. Only read-only delegation children
// allocate one; implement children carry a branch and are cleaned through the
// merge gate, so they are excluded.
func isReadOnlyDelegationWorktree(jobType string, payload JobPayload) bool {
	return strings.TrimSpace(payload.DelegationID) != "" &&
		strings.TrimSpace(payload.WorktreePath) != "" &&
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
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(opCtx, e.Store, e.DelegationCheckout, "worktree-cleanup:"+jobID, time.Now().UTC())
	if err != nil {
		_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("read-only worktree %s cleanup could not lock checkout: %v", path, err)})
		return
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	if err := manager.RemoveWorktreeForce(opCtx, path); err != nil {
		_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("read-only worktree %s force-remove failed: %v", path, err)})
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

// cleanupImplementDelegationWorktree disposes the per-delegation worktree AND
// deletes the gitmoot-delegation-* branch allocated for an implement delegation
// child once the child job is terminal, so they do not accumulate in the shared
// checkout and mislead a later coordinator (#478). It is best-effort and
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
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager) // RemoveWorktreeForce
	if !ok || manager == nil {
		return
	}
	deleter, _ := e.DelegationWorktrees.(BranchDeleter)
	checker, _ := e.DelegationWorktrees.(BranchExistenceChecker)
	// Detach from the caller's cancellation: this runs on the child's terminal
	// AdvanceJob, which may carry a job context already cancelled by a run timeout.
	// The worktree must still be disposed, so keep context values but drop the
	// deadline/cancel.
	opCtx := context.WithoutCancel(ctx)
	path := strings.TrimSpace(payload.WorktreePath)
	branch := strings.TrimSpace(payload.Branch)
	// Idempotency: if the worktree dir is already gone AND the branch is already
	// gone (prior advance cleaned it), do nothing (no lock, no spurious event).
	_, statErr := os.Stat(path)
	worktreeGone := os.IsNotExist(statErr)
	branchGone := false
	if checker != nil {
		if exists, err := checker.BranchExists(opCtx, branch); err == nil {
			branchGone = !exists
		}
	}
	if worktreeGone && branchGone {
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
	if deleter != nil && !branchGone {
		if err := deleter.DeleteBranch(opCtx, branch); err != nil {
			_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("implement branch %s delete failed: %v", branch, err)})
			return
		}
	}
	_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_removed", Message: fmt.Sprintf("implement worktree %s and branch %s removed", path, branch)})
}

// implementLegBranchMayBeMerged reports whether some sibling delegation lists
// this leg's delegation id in its Deps, meaning a dependent integration step
// (#332) may still merge the leg's branch. It reads the parent's result.
func (e Engine) implementLegBranchMayBeMerged(ctx context.Context, payload JobPayload) bool {
	parentID := strings.TrimSpace(payload.ParentJobID)
	if parentID == "" {
		return false
	}
	_, parentPayload, err := e.jobPayload(ctx, parentID)
	if err != nil || parentPayload.Result == nil {
		return false // best-effort; don't block cleanup on a fetch error
	}
	for _, sib := range parentPayload.Result.Delegations {
		for _, dep := range sib.Deps {
			if strings.TrimSpace(dep) == strings.TrimSpace(payload.DelegationID) {
				return true
			}
		}
	}
	return false
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
