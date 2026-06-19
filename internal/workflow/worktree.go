package workflow

import (
	"context"
	"database/sql"
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

// IntegrationWorktreeManager builds a detached worktree off the parent base and
// merges the per-delegation branches of succeeded implement legs into it, so a
// dependent verify/review step sees the legs' combined work instead of the base
// checkout (issue #332). The detached worktree carries no branch and no branch
// lock, so it is disposed by the same read-only cleanup as fan-out worktrees.
type IntegrationWorktreeManager interface {
	AddDetachedWorktree(ctx context.Context, path string, ref string) error
	MergeBranches(ctx context.Context, dir string, branches []string, message string) error
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
	if value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return "", fmt.Errorf("%s %q is not safe for a worktree path", label, value)
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_' || char == '.':
		default:
			return "", fmt.Errorf("%s %q contains unsupported path characters", label, value)
		}
	}
	return value, nil
}
