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

// WritableWorktreeLineageManager is implemented by the checkout-bound Git
// client. It keeps lineage checks and any stale-worktree replacement inside the
// same checkout mutation lock used for ordinary worktree allocation.
type WritableWorktreeLineageManager interface {
	FetchRemote(ctx context.Context, remote string) error
	HeadSHAAt(ctx context.Context, path string) (string, error)
	RevParse(ctx context.Context, rev string) (string, error)
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
	WorktreeCleanAt(ctx context.Context, path string) (bool, error)
	WorktreePristineAt(ctx context.Context, path string) (bool, error)
	WorktreeHeadReachableFromRemote(ctx context.Context, path string, branch string) (bool, error)
	RemoveWorktree(ctx context.Context, path string) error
	DeleteBranch(ctx context.Context, branch string) error
}

type TaskWorktreeReclaimClassification string

const (
	TaskWorktreeReclaimReclaimed       TaskWorktreeReclaimClassification = "reclaimed"
	TaskWorktreeReclaimAlreadyAbsent   TaskWorktreeReclaimClassification = "already_absent"
	TaskWorktreeReclaimNotTerminal     TaskWorktreeReclaimClassification = "not_terminal"
	TaskWorktreeReclaimPathMismatch    TaskWorktreeReclaimClassification = "path_mismatch"
	TaskWorktreeReclaimActiveOwner     TaskWorktreeReclaimClassification = "active_owner"
	TaskWorktreeReclaimLivenessUnknown TaskWorktreeReclaimClassification = "liveness_unknown"
	TaskWorktreeReclaimLiveProcess     TaskWorktreeReclaimClassification = "live_process"
	TaskWorktreeReclaimDirty           TaskWorktreeReclaimClassification = "dirty"
	TaskWorktreeReclaimHeadUnreachable TaskWorktreeReclaimClassification = "head_unreachable"
	TaskWorktreeReclaimUnremovable     TaskWorktreeReclaimClassification = "terminal_unremovable"
)

type TaskWorktreeReclaimOutcome struct {
	Reclaimed      bool
	Classification TaskWorktreeReclaimClassification
	Path           string
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

// WorktreePruner removes stale worktree administrative entries after a forced
// TTL reclaim. The checkout-bound git client satisfies it; tests may omit it
// because pruning is cleanup of shared git metadata, not the safety gate.
type WorktreePruner interface {
	PruneWorktrees(ctx context.Context) error
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
	Home      string
	Repo      string
	TaskID    string
	GoalID    string
	TaskTitle string
	Branch    string
	// BaseBranch is the independently resolved lineage base for ordinary
	// allocation and reuse checks.
	BaseBranch string
	// LineageUnknown is set when no independently resolved base applies because
	// a more precise validation already proved an existing worktree correct,
	// such as an implicit PR fix-pass's exact branch and HEAD match. The
	// existing-worktree reuse path treats lineage as satisfied without deriving
	// a fallback base or performing another Git probe.
	LineageUnknown bool
	Owner          string
	Checkout       string
}

// ReconcileDirtyTaskWorktreeLineage distinguishes ordinary resumable dirtiness
// from a dirty worktree whose base has moved. Callers invoke it only after their
// existing dirty-worktree preflight has succeeded.
//
// A confirmed off-lineage worktree is blocked and journaled through the same
// path used by AllocateTaskWorktree. An intact or indeterminate lineage returns
// handled=false so callers preserve their existing recover-or-clean guidance.
func (e Engine) ReconcileDirtyTaskWorktreeLineage(ctx context.Context, manager WorktreeManager, task db.Task, path, baseRef string) (handled bool, blockErr error) {
	// Every probe/setup failure is deliberately indeterminate. This method may
	// only replace the caller's existing dirty-worktree behavior when it can
	// affirmatively prove the worktree is off-lineage.
	if err := e.validate(); err != nil {
		return false, nil
	}
	lineage, err := writableWorktreeLineageManager(manager)
	if err != nil {
		return false, nil
	}
	baseHead, err := fetchAndResolveLineageBase(ctx, lineage, baseRef)
	if err != nil {
		return false, nil
	}
	worktreeHead, err := lineage.HeadSHAAt(ctx, path)
	if err != nil {
		return false, nil
	}
	isAncestor, err := lineage.IsAncestor(ctx, baseHead, worktreeHead)
	if err != nil || isAncestor {
		return false, nil
	}
	result := worktreeLineageResult{DirtyBlocked: true, BaseHead: baseHead, OldHead: worktreeHead}
	reason := result.dirtyBlockedMessage(path)
	request := TaskWorktreeRequest{
		Repo:      task.RepoFullName,
		TaskID:    task.ID,
		GoalID:    task.GoalID,
		TaskTitle: task.Title,
		Branch:    task.Branch,
	}
	return true, blockTaskForDirtyWorktree(ctx, e.Store, task, request, path, reason)
}

// ReclaimTerminalTaskWorktreeOutcome removes a task-owned worktree only after
// terminal state, deterministic ownership, active-job absence, conclusive
// process liveness, and cleanliness are all revalidated under the same checkout
// mutation lock used by allocation. The task branch is deliberately preserved.
func (e Engine) ReclaimTerminalTaskWorktreeOutcome(ctx context.Context, home, checkout, taskID string, manager WritableWorktreeLineageManager) (TaskWorktreeReclaimOutcome, error) {
	outcome := TaskWorktreeReclaimOutcome{}
	if err := e.validate(); err != nil {
		return outcome, err
	}
	if strings.TrimSpace(taskID) == "" {
		return outcome, errors.New("task worktree task id is required")
	}
	if strings.TrimSpace(home) == "" {
		return outcome, errors.New("task worktree home is required")
	}
	if strings.TrimSpace(checkout) == "" {
		return outcome, errors.New("task worktree checkout is required")
	}
	if manager == nil {
		return outcome, errors.New("task worktree manager is required")
	}

	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return outcome, err
	}
	if !isTerminalTaskWorktreeState(task.State) {
		outcome.Classification = TaskWorktreeReclaimNotTerminal
		outcome.Path = strings.TrimSpace(task.WorktreePath)
		return outcome, nil
	}
	path := strings.TrimSpace(task.WorktreePath)
	outcome.Path = path
	expected, err := TaskWorktreePath(home, task.RepoFullName, task.ID)
	if err != nil || path == "" || filepath.Clean(path) != filepath.Clean(expected) {
		classified, classifyErr := e.Store.ClassifyTerminalTaskWorktreeUnremovable(ctx, task.ID, path)
		if classifyErr != nil {
			return outcome, classifyErr
		}
		if classified {
			outcome.Classification = TaskWorktreeReclaimPathMismatch
		}
		return outcome, nil
	}

	opCtx := context.WithoutCancel(ctx)
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(opCtx, e.Store, checkout, "task-worktree-reclaim:"+task.ID, time.Now().UTC())
	if err != nil {
		return outcome, fmt.Errorf("lock checkout for terminal task worktree reclaim: %w", err)
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()

	// Allocation and recovery can race the candidate scan. Re-read after taking
	// the checkout lock and require the same terminal task/path episode.
	task, err = e.Store.GetTask(opCtx, task.ID)
	if err != nil {
		return outcome, err
	}
	if !isTerminalTaskWorktreeState(task.State) {
		outcome.Classification = TaskWorktreeReclaimNotTerminal
		return outcome, nil
	}
	if strings.TrimSpace(task.WorktreePath) != path {
		outcome.Classification = TaskWorktreeReclaimPathMismatch
		return outcome, nil
	}
	active, err := e.taskWorktreeHasActiveOwner(opCtx, task, path)
	if err != nil {
		return outcome, fmt.Errorf("check active task worktree owner: %w", err)
	}
	if active {
		outcome.Classification = TaskWorktreeReclaimActiveOwner
		return outcome, nil
	}
	live, known := e.worktreeLiveness(path)
	if !known {
		outcome.Classification = TaskWorktreeReclaimLivenessUnknown
		return outcome, nil
	}
	if live {
		outcome.Classification = TaskWorktreeReclaimLiveProcess
		return outcome, nil
	}

	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		changed, finishErr := e.Store.CompleteTerminalTaskWorktreeReclaim(opCtx, task.ID, path)
		if finishErr != nil {
			return outcome, finishErr
		}
		outcome.Reclaimed = changed
		outcome.Classification = TaskWorktreeReclaimAlreadyAbsent
		return outcome, nil
	}
	if err != nil {
		return outcome, fmt.Errorf("inspect terminal task worktree %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		classified, classifyErr := e.Store.ClassifyTerminalTaskWorktreeUnremovable(opCtx, task.ID, path)
		if classifyErr != nil {
			return outcome, classifyErr
		}
		if classified {
			outcome.Classification = TaskWorktreeReclaimPathMismatch
		}
		return outcome, nil
	}
	clean, err := manager.WorktreePristineAt(opCtx, path)
	if err != nil {
		if isTerminalWorktreeRemovalError(err) {
			return e.classifyTerminalTaskWorktreeUnremovable(opCtx, task.ID, path, outcome)
		}
		return outcome, fmt.Errorf("prove terminal task worktree clean at %s: %w", path, err)
	}
	if !clean {
		outcome.Classification = TaskWorktreeReclaimDirty
		return outcome, nil
	}

	// Final guards minimize the status/liveness-to-unlink window. Git's
	// non-force removal remains the last line of defense against new dirtiness.
	task, err = e.Store.GetTask(opCtx, task.ID)
	if err != nil {
		return outcome, err
	}
	if !isTerminalTaskWorktreeState(task.State) || strings.TrimSpace(task.WorktreePath) != path {
		outcome.Classification = TaskWorktreeReclaimNotTerminal
		return outcome, nil
	}
	active, err = e.taskWorktreeHasActiveOwner(opCtx, task, path)
	if err != nil {
		return outcome, fmt.Errorf("recheck active task worktree owner: %w", err)
	}
	if active {
		outcome.Classification = TaskWorktreeReclaimActiveOwner
		return outcome, nil
	}
	live, known = e.worktreeLiveness(path)
	if !known {
		outcome.Classification = TaskWorktreeReclaimLivenessUnknown
		return outcome, nil
	}
	if live {
		outcome.Classification = TaskWorktreeReclaimLiveProcess
		return outcome, nil
	}
	clean, err = manager.WorktreePristineAt(opCtx, path)
	if err != nil {
		if isTerminalWorktreeRemovalError(err) {
			return e.classifyTerminalTaskWorktreeUnremovable(opCtx, task.ID, path, outcome)
		}
		return outcome, fmt.Errorf("recheck terminal task worktree clean at %s: %w", path, err)
	}
	if !clean {
		outcome.Classification = TaskWorktreeReclaimDirty
		return outcome, nil
	}
	reachable, err := taskWorktreeHeadReachableFromBranch(opCtx, task, path, manager)
	if err != nil {
		return outcome, fmt.Errorf("prove terminal task worktree head reachable from branch: %w", err)
	}
	if !reachable {
		outcome.Classification = TaskWorktreeReclaimHeadUnreachable
		return outcome, nil
	}
	if err := manager.RemoveWorktree(opCtx, path); err != nil {
		if isTerminalWorktreeRemovalError(err) {
			return e.classifyTerminalTaskWorktreeUnremovable(opCtx, task.ID, path, outcome)
		}
		return outcome, fmt.Errorf("remove terminal task worktree %s: %w", path, err)
	}
	changed, err := e.Store.CompleteTerminalTaskWorktreeReclaim(opCtx, task.ID, path)
	if err != nil {
		return outcome, err
	}
	outcome.Reclaimed = changed
	outcome.Classification = TaskWorktreeReclaimReclaimed
	return outcome, nil
}

func taskWorktreeHeadReachableFromBranch(ctx context.Context, task db.Task, path string, manager WritableWorktreeLineageManager) (bool, error) {
	branch := strings.TrimSpace(task.Branch)
	if branch == "" {
		return false, nil
	}
	head, err := manager.HeadSHAAt(ctx, path)
	if err != nil {
		return false, err
	}
	branchHead, err := manager.RevParse(ctx, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return manager.IsAncestor(ctx, head, branchHead)
}

func (e Engine) classifyTerminalTaskWorktreeUnremovable(ctx context.Context, taskID, path string, outcome TaskWorktreeReclaimOutcome) (TaskWorktreeReclaimOutcome, error) {
	classified, err := e.Store.ClassifyTerminalTaskWorktreeUnremovable(ctx, taskID, path)
	if err != nil {
		return outcome, err
	}
	if classified {
		outcome.Classification = TaskWorktreeReclaimUnremovable
	}
	return outcome, nil
}

func (e Engine) taskWorktreeHasActiveOwner(ctx context.Context, task db.Task, path string) (bool, error) {
	active, err := e.Store.TaskHasActiveWorktreeOwner(ctx, task.ID, path)
	if err != nil || active {
		return active, err
	}
	if strings.TrimSpace(task.Branch) == "" {
		return false, nil
	}
	if _, err := e.Store.GetBranchLock(ctx, task.RepoFullName, task.Branch); err == nil {
		return true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return false, nil
}

func isTerminalTaskWorktreeState(state string) bool {
	return db.IsTerminalTaskWorktreeState(state)
}

func isTerminalWorktreeRemovalError(err error) bool {
	var terminal interface {
		TerminalWorktreeRemoval() bool
	}
	return errors.As(err, &terminal) && terminal.TerminalWorktreeRemoval()
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
	claimPlanned := false
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return db.Task{}, err
		}
		task = db.Task{ID: request.TaskID, RepoFullName: request.Repo, State: string(TaskPlanned)}
	} else {
		claimPlanned = task.State == string(TaskPlanned)
	}
	if task.State == string(TaskDismissed) {
		return db.Task{}, fmt.Errorf("task %s is dismissed; recover it explicitly before allocating a worktree", request.TaskID)
	}
	if task.State == string(TaskSuperseded) || task.State == string(TaskStranded) {
		return db.Task{}, fmt.Errorf("task %s is %s; create a successor task before allocating a worktree", request.TaskID, task.State)
	}
	if task.State == string(TaskAwaitingHumanMerge) {
		return db.Task{}, fmt.Errorf("task %s is awaiting a human merge decision; resolve it before allocating a worktree", request.TaskID)
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
	if task.Branch == request.Branch && task.WorktreePath == path {
		// An implicit PR fix-pass may already have proved this exact branch and
		// HEAD through PR-specific validation, with no applicable ancestry base.
		if !request.LineageUnknown {
			lineage, err := ensureExistingWorktreeLineage(ctx, manager, request.Branch, path, request.BaseBranch)
			if err != nil {
				if createdLock {
					_, _ = e.Store.ReleaseLock(ctx, lock)
				}
				return db.Task{}, err
			}
			if lineage.DirtyBlocked {
				reason := lineage.dirtyBlockedMessage(path)
				blockErr := blockTaskForDirtyWorktree(ctx, e.Store, task, request, path, reason)
				if createdLock {
					_, _ = e.Store.ReleaseLock(ctx, lock)
				}
				return db.Task{}, blockErr
			}
			if lineage.Recut {
				if err := addTaskWorktreeLineageEvent(ctx, e.Store, task.ID, "stale_worktree_recut", lineage.message("stale task worktree detected and re-cut")); err != nil {
					if createdLock {
						_, _ = e.Store.ReleaseLock(ctx, lock)
					}
					return db.Task{}, err
				}
			}
		}
		if claimPlanned {
			if err := e.claimPlannedTaskForImplementation(ctx, task.ID); err != nil {
				if createdLock {
					_, _ = e.Store.ReleaseLock(ctx, lock)
				}
				return db.Task{}, err
			}
		}
		task.State = string(TaskImplementing)
		if err := e.Store.UpsertTask(ctx, task); err != nil {
			if createdLock {
				_, _ = e.Store.ReleaseLock(ctx, lock)
			}
			return db.Task{}, err
		}
		return task, nil
	}
	lineage, err := addTaskWorktree(ctx, manager, request.Branch, path, request.BaseBranch)
	if err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, err
	}
	if lineage.DirtyBlocked {
		reason := lineage.dirtyBlockedMessage(path)
		blockErr := blockTaskForDirtyWorktree(ctx, e.Store, task, request, path, reason)
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, blockErr
	}
	if lineage.Recut {
		if err := addTaskWorktreeLineageEvent(ctx, e.Store, task.ID, "stale_worktree_recut", lineage.message("stale task worktree detected and re-cut")); err != nil {
			if createdLock {
				_, _ = e.Store.ReleaseLock(ctx, lock)
			}
			return db.Task{}, err
		}
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
	if claimPlanned {
		// Close the planned_ttl/task-run race at the write boundary. A TTL
		// dismissal that won after the initial GetTask makes this CAS fail, so the
		// following legacy UpsertTask can never resurrect dismissed -> implementing.
		if err := e.claimPlannedTaskForImplementation(ctx, task.ID); err != nil {
			if cleaner, ok := manager.(ReadOnlyWorktreeManager); ok {
				_ = cleaner.RemoveWorktreeForce(context.Background(), path)
			}
			if createdLock {
				_, _ = e.Store.ReleaseLock(ctx, lock)
			}
			return db.Task{}, err
		}
	}
	if err := e.Store.UpsertTask(ctx, task); err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, err
	}
	return task, nil
}

func (e Engine) claimPlannedTaskForImplementation(ctx context.Context, taskID string) error {
	changed, current, err := e.Store.CompareAndSwapTaskState(ctx, taskID, string(TaskPlanned), string(TaskImplementing))
	if err != nil {
		return err
	}
	if changed {
		return nil
	}
	if current == string(TaskDismissed) {
		return fmt.Errorf("task %s was dismissed while task run was starting; recover it explicitly before retrying", taskID)
	}
	return fmt.Errorf("task %s left planned state while task run was starting (now %s); retry from its current lifecycle state", taskID, current)
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
	lineage, err := addTaskWorktree(ctx, manager, branch, path, request.BaseBranch)
	if err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return DelegationWorktreeResult{}, err
	}
	if lineage.DirtyBlocked {
		reason := lineage.dirtyBlockedMessage(path)
		if err := e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   request.ParentJobID,
			Kind:    "stale_worktree_dirty_blocked",
			Message: reason,
		}); err != nil {
			if createdLock {
				_, _ = e.Store.ReleaseLock(ctx, lock)
			}
			return DelegationWorktreeResult{}, err
		}
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return DelegationWorktreeResult{}, BlockedError{Reason: reason}
	}
	if lineage.Recut {
		if err := e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   request.ParentJobID,
			Kind:    "stale_worktree_recut",
			Message: lineage.message("stale delegation worktree detected and re-cut"),
		}); err != nil {
			if createdLock {
				_, _ = e.Store.ReleaseLock(ctx, lock)
			}
			return DelegationWorktreeResult{}, err
		}
	}
	return DelegationWorktreeResult{Path: path, Branch: branch}, nil
}

// ReadOnlyWorktreeDispatchLockWaitBudget bounds how long the SCHEDULER-LOOP
// read-only worktree allocators (#739 dispatch-time isolation and the reactive
// pool-isolation dispatcher) wait for the checkout mutation lock. They run
// synchronously on the per-repo dispatch/poll loop, so the full
// checkoutMutationWaitTimeout (2m) would stall that repo's dispatch AND reap for up
// to two minutes whenever a same-repo merge gate holds the lock. Ask and pool
// isolation fail open because they are throughput optimizations; review fails
// closed because exact-head isolation is correctness. Both need a short bound.
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
// 2-minute wait would freeze that repo's whole dispatch+reap loop. Ask and pool
// isolation are fail-open throughput optimizations; exact-head review allocation
// uses the same bounded primitive but its caller fails the dispatch closed.
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

func isFixWorktree(jobType string, payload JobPayload) bool {
	return jobType == "implement" && payload.FixWorktree &&
		strings.TrimSpace(payload.WorktreePath) != ""
}

func (e Engine) prepareDelegationCleanupObligation(ctx context.Context, jobID, jobType string, payload JobPayload) (bool, error) {
	path := filepath.Clean(strings.TrimSpace(payload.WorktreePath))
	home := filepath.Clean(strings.TrimSpace(e.Home))
	if path == "." || path == "" {
		return false, errors.New("cleanup path is unavailable")
	}
	obligation, err := e.Store.EnsureCleanupObligation(context.WithoutCancel(ctx), jobID, path, time.Now().UTC())
	if err != nil {
		return false, err
	}
	switch obligation.State {
	case db.CleanupObligationQuarantined, db.CleanupObligationRemoved:
		return false, nil
	case db.CleanupObligationPending, db.CleanupObligationRetryable:
		// Actuation is permitted only for these two non-terminal states.
	default:
		return false, fmt.Errorf("cleanup obligation %s has unsupported state %q", obligation.ResourceID, obligation.State)
	}
	validator := ValidateDelegationCleanupTarget
	if e.cleanupTargetValidator != nil {
		validator = e.cleanupTargetValidator
	}
	if err := validator(home, jobID, jobType, payload); err != nil {
		return false, errors.Join(err, e.deferDelegationCleanupFailure(ctx, jobID, path, "identity", err))
	}
	return true, nil
}

// ValidateDelegationCleanupTarget independently derives the managed path for a
// cleanup owner and resolves symlinks before accepting filesystem containment.
func ValidateDelegationCleanupTarget(home, jobID, jobType string, payload JobPayload) error {
	path := filepath.Clean(strings.TrimSpace(payload.WorktreePath))
	home = filepath.Clean(strings.TrimSpace(home))
	if !filepath.IsAbs(path) {
		return fmt.Errorf("cleanup path %q is not absolute", path)
	}
	if home == "." || home == "" {
		return errors.New("cleanup worktree home is unavailable")
	}
	expected, err := expectedDelegationCleanupPaths(home, jobID, jobType, payload)
	if err != nil {
		return err
	}
	matched := false
	for _, candidate := range expected {
		if path == filepath.Clean(candidate) {
			matched = true
			break
		}
	}
	if !matched {
		return fmt.Errorf("cleanup path %q does not match the job's derived managed path", path)
	}
	root := filepath.Join(home, "worktrees")
	if err := cleanupPathContained(root, path); err != nil {
		return err
	}
	return nil
}

func expectedDelegationCleanupPaths(home, jobID, jobType string, payload JobPayload) ([]string, error) {
	if isFixWorktree(jobType, payload) {
		path, err := FixWorktreePath(home, payload.Repo, jobID)
		return []string{path}, err
	}
	// Match isReadOnlyDelegationWorktree's precedence: the explicit marker is a
	// top-level dispatch/pipeline/pool allocation even if legacy payload metadata
	// also carries a DelegationID.
	if payload.ReadOnlyWorktree {
		segments := []string{"readonly-seat", "pipeline-service-stage", "pipeline-stage", "pool-isolation"}
		paths := make([]string, 0, len(segments))
		for _, segment := range segments {
			path, err := DelegationWorktreePath(home, payload.Repo, jobID, segment, 0)
			if err != nil {
				return nil, err
			}
			paths = append(paths, path)
		}
		return paths, nil
	}
	if strings.TrimSpace(payload.DelegationID) != "" {
		parentID := strings.TrimSpace(payload.ParentJobID)
		delegationID := strings.TrimSpace(payload.DelegationID)
		if parentID == "" {
			return nil, errors.New("cleanup owner is missing its parent job id")
		}
		expectedJobID := parentID + "/delegation/" + delegationID
		if payload.RetryCount > 0 {
			expectedJobID += "/retry/" + strconv.Itoa(payload.RetryCount)
		}
		if strings.TrimSpace(jobID) != expectedJobID {
			return nil, fmt.Errorf("cleanup owner job %q does not match parent %q and delegation %q", jobID, parentID, delegationID)
		}
		path, err := DelegationWorktreePath(home, payload.Repo, parentID, delegationID, payload.RetryCount)
		if err != nil {
			return nil, err
		}
		paths := []string{path}
		if readOnlyDelegationAction(jobType) {
			integration, err := DelegationWorktreePath(home, payload.Repo, parentID, "integration-"+delegationID, payload.RetryCount)
			if err != nil {
				return nil, err
			}
			paths = append(paths, integration)
		}
		return paths, nil
	}
	return nil, errors.New("cleanup owner has no managed worktree identity")
}

func cleanupPathContained(root, path string) error {
	resolvedRoot, err := evalPathWithMissingTail(root)
	if err != nil {
		return fmt.Errorf("resolve managed cleanup root %q: %w", root, err)
	}
	resolvedPath, err := evalPathWithMissingTail(path)
	if err != nil {
		return fmt.Errorf("resolve cleanup path %q: %w", path, err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("cleanup path %q resolves outside managed root %q", path, root)
	}
	return nil
}

func evalPathWithMissingTail(path string) (string, error) {
	current := filepath.Clean(path)
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if info, lerr := os.Lstat(current); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path component %q is a dangling symlink", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		tail = append(tail, filepath.Base(current))
		current = parent
	}
}

func (e Engine) deferDelegationCleanupObligation(ctx context.Context, jobID, path string, reason db.CleanupObligationReason) error {
	now := time.Now().UTC()
	_, err := e.Store.DeferCleanupObligation(context.WithoutCancel(ctx), jobID, path, reason, now, now.Add(time.Minute))
	return err
}

func (e Engine) deferDelegationCleanupFailure(ctx context.Context, jobID, path, phase string, err error) error {
	return e.deferDelegationCleanupObligation(ctx, jobID, path, db.ClassifyCleanupObligationFailure(phase, err))
}

func (e Engine) markDelegationCleanupRemoved(ctx context.Context, jobID, path string) error {
	_, err := e.Store.MarkCleanupObligationRemoved(context.WithoutCancel(ctx), jobID, path, time.Now().UTC())
	return err
}

// cleanupFixWorktree removes an engine-dispatched review fix's independent
// writable clone. It deliberately does not use RemoveWorktreeForce, delete the
// payload branch, or release its task branch lock: the clone has its own .git and
// payload.Branch is the real lane branch that the finalizer just pushed.
func (e Engine) cleanupFixWorktree(ctx context.Context, jobID string, jobType string, payload JobPayload) error {
	if !isFixWorktree(jobType, payload) {
		return nil
	}
	if strings.TrimSpace(e.Home) == "" {
		return nil
	}
	path := strings.TrimSpace(payload.WorktreePath)
	actuate, err := e.prepareDelegationCleanupObligation(ctx, jobID, jobType, payload)
	if err != nil {
		return err
	}
	if !actuate {
		return nil
	}
	if skip, reason := e.cleanupBlockedByLiveOwner(ctx, jobID, payload); skip {
		if err := e.deferDelegationCleanupObligation(ctx, jobID, path, db.CleanupReasonTerminalDeferred); err != nil {
			return err
		}
		e.recordCleanupSkippedOnce(ctx, jobID, payload, reason)
		return nil
	}
	opCtx := context.WithoutCancel(ctx)
	expected, err := FixWorktreePath(e.Home, payload.Repo, jobID)
	if err != nil || filepath.Clean(path) != filepath.Clean(expected) {
		cleanupErr := fmt.Errorf("refusing unmanaged fix worktree cleanup %s", path)
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "identity", cleanupErr); persistErr != nil {
			return errors.Join(cleanupErr, persistErr)
		}
		e.recordCleanupSkippedOnce(opCtx, jobID, payload, "path is not the job's managed fix-worktree path")
		return cleanupErr
	}
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return e.markDelegationCleanupRemoved(opCtx, jobID, path)
	} else if statErr != nil {
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", statErr); persistErr != nil {
			return errors.Join(statErr, persistErr)
		}
		e.recordCleanupSkippedOnce(opCtx, jobID, payload, fmt.Sprintf("inspect failed: %v", statErr))
		return fmt.Errorf("inspect fix worktree %s: %w", path, statErr)
	}
	if err := os.RemoveAll(path); err != nil {
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		e.recordCleanupSkippedOnce(opCtx, jobID, payload, fmt.Sprintf("remove failed: %v", err))
		return fmt.Errorf("remove fix worktree %s: %w", path, err)
	}
	_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_removed", Message: fmt.Sprintf("fix worktree %s removed", path)})
	return e.markDelegationCleanupRemoved(opCtx, jobID, path)
}

// cleanupReadOnlyDelegationWorktree disposes the detached worktree allocated for
// a read-only delegation child once the child job is terminal. It is best-effort
// and idempotent: a missing worktree (already removed on a prior advance, or
// never allocated) is logged, not fatal. Removal mutates the shared .git, so it
// holds the checkout mutation lock like allocation does.
func (e Engine) cleanupReadOnlyDelegationWorktree(ctx context.Context, jobID string, jobType string, payload JobPayload) error {
	if !isReadOnlyDelegationWorktree(jobType, payload) {
		return nil
	}
	// Detach from the caller's cancellation: this runs on the child's terminal
	// AdvanceJob, which may carry a job context already cancelled by a run timeout.
	// The worktree must still be disposed, so keep context values but drop the
	// deadline/cancel.
	opCtx := context.WithoutCancel(ctx)
	path := strings.TrimSpace(payload.WorktreePath)
	actuate, err := e.prepareDelegationCleanupObligation(opCtx, jobID, jobType, payload)
	if err != nil {
		return err
	}
	if !actuate {
		return nil
	}
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	if !ok || manager == nil {
		cleanupErr := errors.New("delegation worktree manager cannot force-remove worktrees")
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", cleanupErr); persistErr != nil {
			return errors.Join(cleanupErr, persistErr)
		}
		e.recordReadOnlyCleanupSkippedOnce(opCtx, jobID, path, "delegation worktree manager is unavailable")
		return cleanupErr
	}
	// Idempotent: AdvanceJob can run more than once for a job (re-advance / retry
	// passes). If the worktree directory is already gone, do not re-lock or emit a
	// spurious cleanup-failed event.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return e.markDelegationCleanupRemoved(opCtx, jobID, path)
	} else if statErr != nil {
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", statErr); persistErr != nil {
			return errors.Join(statErr, persistErr)
		}
		e.recordReadOnlyCleanupSkippedOnce(opCtx, jobID, path, fmt.Sprintf("inspect failed: %v", statErr))
		return fmt.Errorf("inspect read-only worktree %s: %w", path, statErr)
	}
	if e.BeforeReadOnlyWorktreeCleanup != nil {
		if err := e.BeforeReadOnlyWorktreeCleanup(opCtx, jobID, jobType, payload); err != nil {
			_ = e.Store.AddJobEvent(opCtx, db.JobEvent{
				JobID: jobID, Kind: "readonly_worktree_precleanup_failed",
				Message: fmt.Sprintf("pre-cleanup hook failed before worktree disposal: %s", RedactCommentText(err.Error())),
			})
		}
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(opCtx, e.Store, e.DelegationCheckout, "worktree-cleanup:"+jobID, time.Now().UTC())
	if err != nil {
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "lock", err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		// A transient failure (lock contention) must NOT be terminal: emit the same
		// reclaim marker the daemon's reclaimSkippedDelegationWorktrees pass keys on,
		// so ReclaimTerminalDelegationWorktree re-fires this cleanup on a later tick
		// rather than leaking the worktree. A bare delegation_worktree_cleanup_failed
		// is never re-selected by any pass (it is not the latest advance marker, and
		// the reclaim SQL only picks _skipped), so it would leak permanently (#739 review).
		e.recordReadOnlyCleanupSkippedOnce(opCtx, jobID, path, fmt.Sprintf("could not lock checkout: %v", err))
		return fmt.Errorf("lock checkout for read-only worktree cleanup: %w", err)
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	if err := manager.RemoveWorktreeForce(opCtx, path); err != nil {
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		e.recordReadOnlyCleanupSkippedOnce(opCtx, jobID, path, fmt.Sprintf("force-remove failed: %v", err))
		return fmt.Errorf("force-remove read-only worktree %s: %w", path, err)
	}
	_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_removed", Message: fmt.Sprintf("read-only worktree %s removed", path)})
	return e.markDelegationCleanupRemoved(opCtx, jobID, path)
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
func (e Engine) cleanupImplementDelegationWorktree(ctx context.Context, jobID string, jobType string, payload JobPayload) error {
	if !isImplementDelegationWorktree(jobType, payload) {
		return nil
	}
	// #332 guard: a succeeded implement leg's branch is merged into a dependent
	// integration worktree. Do
	// NOT delete a succeeded leg whose branch a sibling lists in Deps, or a
	// pending integration would fail to merge it. Failed/blocked legs are never
	// merged, so they are always safe to clean.
	if payload.Result != nil && payload.Result.Decision == "implemented" &&
		e.implementLegBranchMayBeMerged(ctx, payload) {
		return nil
	}
	path := strings.TrimSpace(payload.WorktreePath)
	actuate, err := e.prepareDelegationCleanupObligation(ctx, jobID, jobType, payload)
	if err != nil {
		return err
	}
	if !actuate {
		return nil
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
		if err := e.deferDelegationCleanupObligation(ctx, jobID, path, db.CleanupReasonTerminalDeferred); err != nil {
			return err
		}
		e.recordCleanupSkippedOnce(ctx, jobID, payload, reason)
		return nil
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
		cleanupErr := errors.New("delegation worktree manager cannot force-remove worktrees")
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", cleanupErr); persistErr != nil {
			return errors.Join(cleanupErr, persistErr)
		}
		e.recordCleanupSkippedOnce(opCtx, jobID, payload, "delegation worktree manager is unavailable")
		return cleanupErr
	}
	deleter, _ := e.DelegationWorktrees.(BranchDeleter)
	checker, _ := e.DelegationWorktrees.(BranchExistenceChecker)
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
		return e.markDelegationCleanupRemoved(opCtx, jobID, path)
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(opCtx, e.Store, e.DelegationCheckout, "worktree-cleanup:"+jobID, time.Now().UTC())
	if err != nil {
		if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "lock", err); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("implement worktree %s cleanup could not lock checkout: %v", path, err)})
		return fmt.Errorf("lock checkout for implement worktree cleanup: %w", err)
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	if !worktreeGone {
		if err := manager.RemoveWorktreeForce(opCtx, path); err != nil {
			if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", err); persistErr != nil {
				return errors.Join(err, persistErr)
			}
			_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("implement worktree %s force-remove failed: %v", path, err)})
			return fmt.Errorf("force-remove implement worktree %s: %w", path, err)
		}
	}
	branchDeleted := false
	if deleter != nil && !branchKnownGone {
		if err := deleter.DeleteBranch(opCtx, branch); err != nil {
			if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", err); persistErr != nil {
				return errors.Join(err, persistErr)
			}
			_ = e.Store.AddJobEvent(opCtx, db.JobEvent{JobID: jobID, Kind: "delegation_worktree_cleanup_failed", Message: fmt.Sprintf("implement branch %s delete failed: %v", branch, err)})
			return fmt.Errorf("delete implement worktree branch %s: %w", branch, err)
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
	return e.markDelegationCleanupRemoved(opCtx, jobID, path)
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
	_, err := e.ReclaimTerminalDelegationWorktreeOutcome(ctx, jobID)
	return err
}

// ReclaimTerminalDelegationWorktreeOutcome is the reporting form used by the
// daemon reclaim pass. reclaimed is true only when this call performed cleanup;
// a liveness gate or already-clean candidate reports false without an error.
func (e Engine) ReclaimTerminalDelegationWorktreeOutcome(ctx context.Context, jobID string) (reclaimed bool, err error) {
	if err := e.validate(); err != nil {
		return false, err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return false, err
	}
	var cleanupErr error
	for _, err := range []error{
		e.cleanupImplementDelegationWorktree(ctx, jobID, job.Type, payload),
		e.cleanupReadOnlyDelegationWorktree(ctx, jobID, job.Type, payload),
		e.cleanupFixWorktree(ctx, jobID, job.Type, payload),
	} {
		if cleanupErr == nil && err != nil {
			cleanupErr = err
		}
	}
	if cleanupErr != nil {
		return false, cleanupErr
	}
	outcome, err := e.Store.LatestDelegationWorktreeCleanupOutcome(context.WithoutCancel(ctx), jobID)
	if err != nil {
		return false, err
	}
	return outcome == "delegation_worktree_removed", nil
}

// ReclaimAgedTerminalDelegationWorktree force-removes a delegation/read-only/fix
// worktree only when the owning job is FINAL and its terminal updated_at is at
// or before cutoff. This is the crash-window backstop: unlike the ordinary
// cleanup path it intentionally bypasses dirty/unprovable-content and stale
// runtime-owner preservation after the 72h default grace period. Blocked jobs
// are resumable (not final), so they can never pass this gate.
func (e Engine) ReclaimAgedTerminalDelegationWorktree(ctx context.Context, jobID string, cutoff time.Time) error {
	_, err := e.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, cutoff)
	return err
}

// ReclaimAgedTerminalDelegationWorktreeOutcome is the reporting form used by
// the daemon reclaim pass. reclaimed is false for every revalidation no-op and
// true only after the path cleanup and reclaim event complete.
func (e Engine) ReclaimAgedTerminalDelegationWorktreeOutcome(ctx context.Context, jobID string, cutoff time.Time) (reclaimed bool, err error) {
	if err := e.validate(); err != nil {
		return false, err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return false, err
	}
	if !IsFinalJobState(job.State) {
		return false, fmt.Errorf("refusing TTL worktree reclaim for non-final job %s in state %s", jobID, job.State)
	}
	terminalAt, ok := parseStoredJobTime(job.UpdatedAt)
	if !ok {
		terminalAt, ok = parseStoredJobTime(job.CreatedAt)
	}
	if !ok || terminalAt.After(cutoff.UTC()) {
		return false, nil
	}
	readOnly := isReadOnlyDelegationWorktree(job.Type, payload)
	implement := isImplementDelegationWorktree(job.Type, payload)
	fix := isFixWorktree(job.Type, payload)
	if !readOnly && !implement && !fix {
		return false, nil
	}
	// A deterministic path can appear in more than one historical row. Never let
	// an aged row reclaim it out from under a newer or resumable owner.
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, other := range jobs {
		if other.ID == job.ID {
			continue
		}
		otherPayload, err := ParseJobPayload(other.Payload)
		if err != nil || filepath.Clean(strings.TrimSpace(otherPayload.WorktreePath)) != filepath.Clean(strings.TrimSpace(payload.WorktreePath)) {
			continue
		}
		if !IsFinalJobState(other.State) {
			return false, nil
		}
		otherAt, ok := parseStoredJobTime(other.UpdatedAt)
		if !ok {
			otherAt, ok = parseStoredJobTime(other.CreatedAt)
		}
		if !ok || otherAt.After(cutoff.UTC()) {
			return false, nil
		}
	}
	path := strings.TrimSpace(payload.WorktreePath)
	actuate, err := e.prepareDelegationCleanupObligation(ctx, jobID, job.Type, payload)
	if err != nil {
		return false, err
	}
	if !actuate {
		return false, nil
	}
	if fix {
		expected, err := FixWorktreePath(e.Home, payload.Repo, jobID)
		if err != nil || filepath.Clean(path) != filepath.Clean(expected) {
			return false, fmt.Errorf("refusing TTL reclaim for unmanaged fix worktree %s", path)
		}
		live, known := e.worktreeLiveness(path)
		if !known || live {
			return false, nil
		}
		manager, ok := e.DelegationWorktrees.(WritableWorktreeLineageManager)
		if !ok || manager == nil {
			return false, errors.New("delegation worktree manager cannot prove fix worktree lineage")
		}
		if strings.TrimSpace(payload.Branch) == "" {
			return false, errors.New("terminal fix worktree payload has no branch")
		}
		clean, err := manager.WorktreePristineAt(ctx, path)
		if err != nil {
			return false, fmt.Errorf("prove aged terminal fix worktree clean: %w", err)
		}
		if !clean {
			return false, nil
		}
		reachable, err := manager.WorktreeHeadReachableFromRemote(ctx, path, payload.Branch)
		if err != nil {
			return false, fmt.Errorf("prove aged terminal fix worktree head reachable from remote: %w", err)
		}
		if !reachable {
			return false, nil
		}
		live, known = e.worktreeLiveness(path)
		if !known || live {
			return false, nil
		}
		clean, err = manager.WorktreePristineAt(ctx, path)
		if err != nil {
			return false, fmt.Errorf("recheck aged terminal fix worktree clean: %w", err)
		}
		if !clean {
			return false, nil
		}
		if err := os.RemoveAll(path); err != nil {
			return false, fmt.Errorf("remove aged terminal fix worktree %s: %w", path, err)
		}
		err = e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{
			JobID: jobID, Kind: "delegation_worktree_reclaimed_ttl",
			Message: fmt.Sprintf("aged terminal fix worktree %s removed after TTL", path),
		})
		if err == nil {
			err = e.markDelegationCleanupRemoved(ctx, jobID, path)
		}
		return err == nil, err
	}
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	if !ok || manager == nil {
		return false, errors.New("delegation worktree manager cannot force-remove worktrees")
	}

	opCtx := context.WithoutCancel(ctx)
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(opCtx, e.Store, e.DelegationCheckout, "worktree-ttl-reclaim:"+jobID, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("lock checkout for TTL worktree reclaim: %w", err)
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()

	if _, statErr := os.Stat(path); statErr == nil {
		if err := manager.RemoveWorktreeForce(opCtx, path); err != nil {
			return false, fmt.Errorf("force-remove aged terminal worktree %s: %w", path, err)
		}
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("inspect aged terminal worktree %s: %w", path, statErr)
	}

	branch := strings.TrimSpace(payload.Branch)
	if implement {
		if _, err := releaseDelegationBranchLock(opCtx, e.Store, job.Type, payload); err != nil {
			return false, fmt.Errorf("release delegation branch lock %s: %w", branch, err)
		}
		if deleter, ok := e.DelegationWorktrees.(BranchDeleter); ok && deleter != nil {
			shouldDelete := true
			if checker, ok := e.DelegationWorktrees.(BranchExistenceChecker); ok && checker != nil {
				exists, err := checker.BranchExists(opCtx, branch)
				if err != nil {
					return false, fmt.Errorf("inspect delegation branch %s: %w", branch, err)
				}
				shouldDelete = exists
			}
			if shouldDelete {
				if err := deleter.DeleteBranch(opCtx, branch); err != nil {
					return false, fmt.Errorf("force-delete aged terminal branch %s: %w", branch, err)
				}
			}
		}
	}
	if pruner, ok := e.DelegationWorktrees.(WorktreePruner); ok && pruner != nil {
		if err := pruner.PruneWorktrees(opCtx); err != nil {
			return false, fmt.Errorf("prune worktree metadata after TTL reclaim: %w", err)
		}
	}
	err = e.Store.AddJobEvent(opCtx, db.JobEvent{
		JobID: jobID, Kind: "delegation_worktree_reclaimed_ttl",
		Message: fmt.Sprintf("aged terminal delegation worktree %s force-removed after TTL", path),
	})
	if err == nil {
		err = e.markDelegationCleanupRemoved(opCtx, jobID, path)
	}
	return err == nil, err
}

func parseStoredJobTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// implementLegBranchMayBeMerged reports whether a succeeded implement leg's
// branch must be PRESERVED because a dependent integration step (#332) may still
// merge it. A sibling delegation that lists this leg in its Deps consumes the
// leg's branch after that leg succeeds, but only
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
// another live consumer) proceeds. It is idempotent for already-cleaned legs and
// fails closed when durable cleanup accounting cannot be persisted.
func (e Engine) cleanupConsumedImplementLegWorktrees(ctx context.Context, payload JobPayload) error {
	parentID := strings.TrimSpace(payload.ParentJobID)
	deps := compactStrings(payload.Deps)
	if parentID == "" || len(deps) == 0 {
		return nil
	}
	children, err := e.childDelegationJobs(ctx, parentID)
	if err != nil {
		return err
	}
	var cleanupErr error
	for _, dep := range deps {
		legJob, ok := children[strings.TrimSpace(dep)]
		if !ok {
			continue
		}
		legPayload, err := unmarshalPayload(legJob.Payload)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		if err := e.cleanupImplementDelegationWorktree(ctx, legJob.ID, legJob.Type, legPayload); err != nil {
			return errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

type worktreeLineageResult struct {
	Recut        bool
	DirtyBlocked bool
	BaseHead     string
	OldHead      string
	NewHead      string
}

func (r worktreeLineageResult) message(prefix string) string {
	return fmt.Sprintf("%s: base=%s old_head=%s new_head=%s", prefix, r.BaseHead, r.OldHead, r.NewHead)
}

func (r worktreeLineageResult) dirtyBlockedMessage(path string) string {
	return fmt.Sprintf("worktree at %s is stale (HEAD %s is not a descendant of resolved base %s) and has uncommitted changes; manually salvage, commit, stash, or clean them before retrying so the worktree can be re-cut", path, r.OldHead, r.BaseHead)
}

func addTaskWorktree(ctx context.Context, manager WorktreeManager, branch string, path string, base string) (worktreeLineageResult, error) {
	if checker, ok := manager.(BranchExistenceChecker); ok {
		exists, err := checker.BranchExists(ctx, branch)
		if err != nil {
			return worktreeLineageResult{}, err
		}
		if exists {
			existingManager, ok := manager.(ExistingBranchWorktreeManager)
			if !ok {
				return worktreeLineageResult{}, errors.New("existing branch worktree manager is required")
			}
			return reuseExistingBranchWorktree(ctx, manager, existingManager, branch, path, base)
		}
	}
	return worktreeLineageResult{}, manager.AddWorktree(ctx, branch, path, base)
}

func ensureExistingWorktreeLineage(ctx context.Context, manager WorktreeManager, branch, path, base string) (worktreeLineageResult, error) {
	lineage, err := writableWorktreeLineageManager(manager)
	if err != nil {
		return worktreeLineageResult{}, err
	}
	baseHead, err := fetchAndResolveLineageBase(ctx, lineage, base)
	if err != nil {
		return worktreeLineageResult{}, err
	}
	oldHead, err := lineage.HeadSHAAt(ctx, path)
	if err != nil {
		return worktreeLineageResult{}, fmt.Errorf("resolve existing worktree head: %w", err)
	}
	isAncestor, err := lineage.IsAncestor(ctx, baseHead, oldHead)
	if err != nil {
		return worktreeLineageResult{}, fmt.Errorf("verify existing worktree lineage: %w", err)
	}
	if isAncestor {
		return worktreeLineageResult{BaseHead: baseHead, OldHead: oldHead, NewHead: oldHead}, nil
	}
	clean, err := lineage.WorktreeCleanAt(ctx, path)
	if err != nil {
		return worktreeLineageResult{}, fmt.Errorf("inspect stale worktree for uncommitted changes: %w", err)
	}
	if !clean {
		return worktreeLineageResult{DirtyBlocked: true, BaseHead: baseHead, OldHead: oldHead}, nil
	}
	if err := lineage.RemoveWorktree(ctx, path); err != nil {
		return worktreeLineageResult{}, fmt.Errorf("remove stale worktree: %w", err)
	}
	if err := lineage.DeleteBranch(ctx, branch); err != nil {
		return worktreeLineageResult{}, fmt.Errorf("delete stale worktree branch: %w", err)
	}
	if err := manager.AddWorktree(ctx, branch, path, baseHead); err != nil {
		return worktreeLineageResult{}, fmt.Errorf("re-create stale worktree: %w", err)
	}
	newHead, err := lineage.HeadSHAAt(ctx, path)
	if err != nil {
		return worktreeLineageResult{}, fmt.Errorf("resolve re-created worktree head: %w", err)
	}
	return worktreeLineageResult{Recut: true, BaseHead: baseHead, OldHead: oldHead, NewHead: newHead}, nil
}

func reuseExistingBranchWorktree(ctx context.Context, manager WorktreeManager, existing ExistingBranchWorktreeManager, branch, path, base string) (worktreeLineageResult, error) {
	lineage, err := writableWorktreeLineageManager(manager)
	if err != nil {
		return worktreeLineageResult{}, err
	}
	baseHead, err := fetchAndResolveLineageBase(ctx, lineage, base)
	if err != nil {
		return worktreeLineageResult{}, err
	}
	pathExists := false
	if _, statErr := os.Stat(path); statErr == nil {
		pathExists = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return worktreeLineageResult{}, fmt.Errorf("inspect existing worktree path: %w", statErr)
	}
	var oldHead string
	if pathExists {
		oldHead, err = lineage.HeadSHAAt(ctx, path)
	} else {
		oldHead, err = lineage.RevParse(ctx, branch)
	}
	if err != nil {
		return worktreeLineageResult{}, fmt.Errorf("resolve existing worktree branch head: %w", err)
	}
	isAncestor, err := lineage.IsAncestor(ctx, baseHead, oldHead)
	if err != nil {
		return worktreeLineageResult{}, fmt.Errorf("verify existing worktree branch lineage: %w", err)
	}
	if isAncestor {
		if pathExists {
			return worktreeLineageResult{BaseHead: baseHead, OldHead: oldHead, NewHead: oldHead}, nil
		}
		return worktreeLineageResult{}, existing.AddExistingBranchWorktree(ctx, branch, path)
	}
	if pathExists {
		clean, err := lineage.WorktreeCleanAt(ctx, path)
		if err != nil {
			return worktreeLineageResult{}, fmt.Errorf("inspect stale worktree for uncommitted changes: %w", err)
		}
		if !clean {
			return worktreeLineageResult{DirtyBlocked: true, BaseHead: baseHead, OldHead: oldHead}, nil
		}
		if err := lineage.RemoveWorktree(ctx, path); err != nil {
			return worktreeLineageResult{}, fmt.Errorf("remove stale worktree: %w", err)
		}
	}
	if err := lineage.DeleteBranch(ctx, branch); err != nil {
		return worktreeLineageResult{}, fmt.Errorf("delete stale worktree branch: %w", err)
	}
	if err := manager.AddWorktree(ctx, branch, path, baseHead); err != nil {
		return worktreeLineageResult{}, fmt.Errorf("re-create stale worktree branch: %w", err)
	}
	newHead, err := lineage.HeadSHAAt(ctx, path)
	if err != nil {
		return worktreeLineageResult{}, fmt.Errorf("resolve re-created worktree head: %w", err)
	}
	return worktreeLineageResult{Recut: true, BaseHead: baseHead, OldHead: oldHead, NewHead: newHead}, nil
}

func writableWorktreeLineageManager(manager WorktreeManager) (WritableWorktreeLineageManager, error) {
	lineage, ok := manager.(WritableWorktreeLineageManager)
	if !ok {
		return nil, errors.New("writable worktree lineage manager is required")
	}
	return lineage, nil
}

func fetchAndResolveLineageBase(ctx context.Context, manager WritableWorktreeLineageManager, base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "HEAD"
	}
	// HEAD and full object IDs already identify the prepared checkout's exact
	// local commit; fetching cannot change what either resolves to and would make
	// local-only repositories fail a purely local retry. Mutable base refs still
	// refresh origin before resolution.
	if base != "HEAD" && !isFullGitObjectID(base) {
		if err := manager.FetchRemote(ctx, "origin"); err != nil {
			return "", fmt.Errorf("fetch origin before worktree lineage check: %w", err)
		}
	}
	head, err := manager.RevParse(ctx, base)
	if err != nil {
		return "", fmt.Errorf("resolve worktree lineage base %q: %w", base, err)
	}
	return head, nil
}

func isFullGitObjectID(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for _, char := range ref {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

func addTaskWorktreeLineageEvent(ctx context.Context, store *db.Store, taskID, kind, reason string) error {
	return store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID: taskID,
		Kind:   kind,
		Reason: reason,
	})
}

func blockTaskForDirtyWorktree(ctx context.Context, store *db.Store, task db.Task, request TaskWorktreeRequest, path, reason string) error {
	fromState := task.State
	task.State = string(TaskBlocked)
	task.Branch = request.Branch
	task.WorktreePath = path
	if task.RepoFullName == "" {
		task.RepoFullName = request.Repo
	}
	if task.GoalID == "" {
		task.GoalID = request.GoalID
	}
	if task.Title == "" {
		task.Title = request.TaskTitle
	}
	if err := store.UpsertTask(ctx, task); err != nil {
		return err
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID:    task.ID,
		Kind:      "stale_worktree_dirty_blocked",
		FromState: fromState,
		ToState:   string(TaskBlocked),
		Reason:    reason,
	}); err != nil {
		return err
	}
	return BlockedError{Reason: reason}
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

// FixWorktreePath builds the deterministic path for an engine-dispatched review
// fix's independent writable clone. The job id makes ownership per-job while the
// dedicated "fixes" segment keeps cleanup distinct from linked delegation
// worktrees, whose synthetic branches are deleted at terminal state.
func FixWorktreePath(home string, repo string, jobID string) (string, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return "", errors.New("fix worktree home is required")
	}
	repoSegment, err := taskWorktreeRepoSegment(repo)
	if err != nil {
		return "", err
	}
	jobSegment, err := taskWorktreePathSegment(jobID, "job id")
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "worktrees", repoSegment, "fixes", jobSegment), nil
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
