package workflow

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	BranchExists(ctx context.Context, branch string) (bool, error)
	RemoteURL(ctx context.Context, remote string) (string, error)
	RefreshCloneProofRefs(ctx context.Context, path string, remoteURL string) error
	CloneOnlyCommit(ctx context.Context, path string) (string, error)
	VerifyPackIndex(ctx context.Context, indexPath string) error
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

const remoteWorktreeReachabilityTimeout = 2 * time.Minute

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
	if _, err := e.Store.CompleteTerminalTaskWorktreeReclaim(opCtx, task.ID, path); err != nil {
		return outcome, err
	}
	outcome.Reclaimed = true
	outcome.Classification = TaskWorktreeReclaimReclaimed
	return outcome, nil
}

func taskWorktreeHeadReachableFromBranch(ctx context.Context, task db.Task, path string, manager WritableWorktreeLineageManager) (bool, error) {
	branch := strings.TrimSpace(task.Branch)
	if branch == "" {
		return false, nil
	}
	// The preserved branch is the durable home for this worktree's commits. If it
	// is gone, safety is unprovable rather than broken: report it as unreachable so
	// the pass records head_unreachable instead of erroring on every tick forever.
	exists, err := manager.BranchExists(ctx, branch)
	if err != nil {
		return false, err
	}
	if !exists {
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
	// An absent path is evidence of a completed removal only when no
	// interrupted-removal quarantine of this clone survives. The TTL pass renames
	// the clone aside before deleting it, so "path absent, clone alive" is a real
	// state and marking the obligation removed here would retire the candidate
	// before the TTL pass can restore it. Absence is stat'd BEFORE the quarantine
	// scan: a rename needs the path to exist, so a path already seen absent cannot
	// acquire a quarantine afterwards.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		owned, ownErr := e.fixCloneFenceOwnership(opCtx, jobID)
		if ownErr != nil {
			return ownErr
		}
		quarantines, quarantineErr := FixCloneQuarantines(path, owned)
		if quarantineErr != nil {
			if persistErr := e.deferDelegationCleanupFailure(opCtx, jobID, path, "reclaim", quarantineErr); persistErr != nil {
				return errors.Join(quarantineErr, persistErr)
			}
			return fmt.Errorf("inspect fix clone quarantines beside %s: %w", path, quarantineErr)
		}
		if len(quarantines) > 0 {
			if err := e.deferDelegationCleanupObligation(opCtx, jobID, path, db.CleanupReasonTerminalDeferred); err != nil {
				return err
			}
			e.recordCleanupSkippedOnce(opCtx, jobID, payload, fmt.Sprintf("interrupted TTL removal left %s; awaiting restore", quarantines[0]))
			return nil
		}
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
//
// A fix clone is explicitly excluded. Fix payloads leave DelegationID empty
// today, so the shapes never overlapped in practice — but that was incidental,
// and an overlapping payload would take this branch first, deleting the clone
// without the published-object-database proof and releasing a branch lock the fix
// flow deliberately keeps. The exclusion makes the invariant enforced.
func isImplementDelegationWorktree(jobType string, payload JobPayload) bool {
	if isFixWorktree(jobType, payload) {
		return false
	}
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

func (e Engine) completeAgedTerminalFixWorktreeReclaim(ctx context.Context, jobID, path string) (bool, error) {
	opCtx := context.WithoutCancel(ctx)
	err := e.Store.AddJobEvent(opCtx, db.JobEvent{
		JobID: jobID, Kind: "delegation_worktree_reclaimed_ttl",
		Message: fmt.Sprintf("aged terminal fix worktree %s reconciled after TTL", path),
	})
	if err == nil {
		err = e.markDelegationCleanupRemoved(opCtx, jobID, path)
	}
	return err == nil, err
}

// fixCloneQuarantinePrefix names the sibling path a proven-disposable fix clone
// is renamed to before deletion. The rename is the atomicity boundary the proof
// needs: it is a single filesystem operation in the same directory, and once it
// returns no `git -C <original path>` can create a commit inside the clone that
// is about to be deleted. Anything that followed the directory instead is caught
// by repeating every proof on the quarantined copy.
//
// The suffix carries RANDOM bytes because the quarantine path is documented: a
// predictable name is a name a process can open between the final proof and the
// removal. Callers therefore discover quarantines by this prefix rather than
// reconstructing one name.
const fixCloneQuarantinePrefix = ".ttl-reclaiming-"

// fixCloneFenceRetention bounds how long a spent fence file is kept. It only has
// to outlive any process that could still name the quarantine it replaced; a day
// covers a daemon restart overlap and keeps the directory-entry cost per reclaimed
// job at zero in steady state.
const fixCloneFenceRetention = 24 * time.Hour

// FixCloneQuarantines lists the interrupted-removal siblings of a fix clone. The
// daemon and doctor need this too: an absent clone path is only evidence of a
// completed removal when no quarantine of it survives.
//
// The parent directory is READ rather than globbed, because a managed path may
// legitimately contain glob metacharacters (a repo or home directory named with
// brackets), and a pattern that silently matches nothing would be read as "no
// quarantine" by callers that then delete.
func FixCloneQuarantines(path string, owned FixCloneFenceOwnership) ([]string, error) {
	quarantines, _, err := classifyFixCloneQuarantineNames(path, owned)
	return quarantines, err
}

// FixCloneFences lists the spent quarantine names of a fix clone: zero-byte
// regular files left behind so a delayed writer can never recreate the name.
// Doctor and /api/health report them, because two directory entries per reclaimed
// job are otherwise invisible inode usage.
func FixCloneFences(path string, owned FixCloneFenceOwnership) ([]string, error) {
	return fixCloneFences(path, owned)
}

func fixCloneFences(path string, owned FixCloneFenceOwnership) ([]string, error) {
	_, fences, err := classifyFixCloneQuarantineNames(path, owned)
	return fences, err
}

// fixCloneFencePrefix opens the content of a spent-quarantine fence. What follows
// it is a random NONCE recorded durably in the job's event log at creation.
//
// The prefix alone is not provenance: it is public, so a same-user writer can type
// it. The nonce is what ties the file to a fence THIS daemon created — a writer
// that has not read the event log cannot produce one, and a fence whose nonce is
// not registered is treated as a survivor rather than as ours. Fences written
// before this record existed carry no nonce and are likewise treated as survivors:
// unproven, never pruned, visible to the operator.
const fixCloneFencePrefix = "gitmoot: spent fix-clone quarantine name; do not recreate\nnonce="

// JobEventFixCloneFenced is the durable record of a fence this code wrote. Its
// message is `<absolute fence path> <nonce>`, which is the evidence
// FixCloneFenceOwnership matches a file against.
const JobEventFixCloneFenced = "delegation_worktree_fence_written"

// FixCloneFenceOwnership is the set of fence nonces this host recorded, keyed by
// absolute path. Callers load it from the job event log; the zero value proves
// nothing, which makes every fence-shaped file a survivor.
type FixCloneFenceOwnership struct {
	nonceByPath map[string]string
}

// NewFixCloneFenceOwnership builds the registry from recorded fence events.
func NewFixCloneFenceOwnership(records []string) FixCloneFenceOwnership {
	owned := FixCloneFenceOwnership{nonceByPath: make(map[string]string, len(records))}
	for _, record := range records {
		path, nonce, ok := strings.Cut(strings.TrimSpace(record), " ")
		if !ok || strings.TrimSpace(path) == "" || strings.TrimSpace(nonce) == "" {
			continue
		}
		owned.nonceByPath[filepath.Clean(path)] = nonce
	}
	return owned
}

func (o FixCloneFenceOwnership) expected(path string) (string, bool) {
	if o.nonceByPath == nil {
		return "", false
	}
	nonce, ok := o.nonceByPath[filepath.Clean(path)]
	return nonce, ok
}

// proven reports whether the file at path is a fence this host recorded.
func (o FixCloneFenceOwnership) proven(path string, info fs.FileInfo) bool {
	nonce, ok := o.expected(path)
	if !ok || !info.Mode().IsRegular() || info.Size() != int64(len(fixCloneFencePrefix)+len(nonce)+1) {
		return false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return false
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Equal(content, fixCloneFenceContent(nonce))
}

func fixCloneFenceContent(nonce string) []byte {
	return []byte(fixCloneFencePrefix + nonce + "\n")
}

func newFixCloneFenceNonce() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate fix clone fence nonce: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// classifyFixCloneQuarantineNames splits the quarantine-named siblings of a clone
// into SURVIVORS and FENCES.
//
// Only a zero-byte REGULAR file is a fence. Anything else — a directory, a
// symlink (which a writer can point at a directory it still writes into), a
// device node, a non-empty file — is a survivor, because it can hold or reach
// content and something other than this code created it. Classifying by "not a
// directory" let a symlink win a fence name and then vanish from every scan.
func classifyFixCloneQuarantineNames(path string, owned FixCloneFenceOwnership) (survivors, fences []string, err error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return nil, nil, nil
	}
	parent, base := filepath.Dir(path), filepath.Base(path)
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	prefix := base + fixCloneQuarantinePrefix
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		candidate := filepath.Join(parent, entry.Name())
		// Type() reports the LSTAT type, so a symlink is never mistaken for its
		// target. An info error is reported rather than swallowed: a name that
		// cannot be classified must not be read as absent by a caller that
		// completes a removal on that basis.
		info, infoErr := entry.Info()
		if infoErr != nil {
			if os.IsNotExist(infoErr) {
				continue
			}
			return nil, nil, fmt.Errorf("classify fix clone quarantine %s: %w", candidate, infoErr)
		}
		// A fence is a file this code wrote: it carries the exact marker, is
		// exactly that long, and has ONE link. A zero-byte file, a hard link to
		// someone else's inode, a symlink, a directory or any other content is a
		// SURVIVOR — a writer can plant those, and treating them as owned made an
		// unproven name look like a completed removal.
		if owned.proven(candidate, info) {
			fences = append(fences, candidate)
			continue
		}
		survivors = append(survivors, candidate)
	}
	return survivors, fences, nil
}

// fenceFixCloneQuarantineName makes a quarantine name permanently unusable. A
// writer that learned the name while the proofs ran can otherwise recreate it
// AFTER the final survivor scan, and that late entry is invisible to every later
// pass because the reclaim completion event retires the job.
//
// The fence is a regular file carrying a fixed MARKER: `mkdir` on the name fails
// with EEXIST and any path BELOW it fails with ENOTDIR, so no content can ever be
// orphaned there. The marker is what makes the file PROVABLY ours — an empty file
// or a hard link a same-user writer plants at the same name carries no marker and
// is classified as a survivor, which blocks completion instead of forging one.
//
// It returns false when the name is already taken, which is the writer having won
// the race — the caller then retains instead of completing. O_NOFOLLOW makes the
// refusal explicit for an existing symlink rather than relying on O_EXCL alone.
func fenceFixCloneQuarantineName(path string) (bool, string, error) {
	nonce, err := newFixCloneFenceNonce()
	if err != nil {
		return false, "", err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o400)
	if err != nil {
		if os.IsExist(err) || errors.Is(err, syscall.ELOOP) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("fence fix clone quarantine name %s: %w", path, err)
	}
	if _, err := file.Write(fixCloneFenceContent(nonce)); err != nil {
		file.Close()
		_ = os.Remove(path)
		return false, "", fmt.Errorf("write fix clone fence %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, "", fmt.Errorf("close fix clone fence %s: %w", path, err)
	}
	return true, nonce, nil
}

// PruneFixCloneFences removes spent fences last modified before cutoff. A fence
// only has to outlive the writers that could still name it, and the pass that
// wrote it already proved no process held the clone. It never touches a survivor.
func PruneFixCloneFences(path string, cutoff time.Time, owned FixCloneFenceOwnership) (int, error) {
	fences, err := fixCloneFences(path, owned)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, fence := range fences {
		removed, err := removeOwnedFence(fence, cutoff, owned)
		if err != nil {
			return pruned, err
		}
		if removed {
			pruned++
		}
	}
	return pruned, nil
}

// PruneExpiredFixCloneFencesBounded is the SCHEDULED half of fence lifecycle: it
// sweeps fix-clone directories and removes retired fences older than cutoff,
// stopping after budget entries and resuming there next time.
//
// The in-pass prune cannot bound them on its own. It runs immediately after
// creating the current pass's fences, so those are always newer than any cutoff,
// and a completed reclaim never becomes a candidate again — so without this sweep
// two directory entries per reclaimed clone would persist forever. It is driven by
// the FILESYSTEM for that reason, and BOUNDED because that traversal is
// proportional to accumulated entries rather than to work in flight.
//
// It returns what it removed and what it looked at, so an operator can see the
// sweep making progress rather than inferring it.
func PruneExpiredFixCloneFencesBounded(home string, cutoff time.Time, budget int, ownership func(clone string) (FixCloneFenceOwnership, error)) (pruned int, scanned int, err error) {
	home = strings.TrimSpace(home)
	if home == "" || budget <= 0 || ownership == nil {
		return 0, 0, nil
	}
	repos, err := os.ReadDir(filepath.Join(home, "worktrees"))
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	resume := fixCloneFenceSweepResume(home)
	// Rotate: start after the repo the last bounded run stopped at, so a host with
	// more entries than the budget still reaches every repo across ticks.
	ordered := make([]string, 0, len(repos))
	for _, repo := range repos {
		if repo.IsDir() {
			ordered = append(ordered, repo.Name())
		}
	}
	sort.Strings(ordered)
	offset := 0
	for i, name := range ordered {
		if name >= resume {
			offset = i
			break
		}
	}
	for step := 0; step < len(ordered); step++ {
		name := ordered[(offset+step)%len(ordered)]
		fixes := filepath.Join(home, "worktrees", name, "fixes")
		entries, readErr := os.ReadDir(fixes)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return pruned, scanned, readErr
		}
		clones := map[string]struct{}{}
		for _, entry := range entries {
			scanned++
			base, _, found := strings.Cut(entry.Name(), fixCloneQuarantinePrefix)
			if !found || base == "" {
				continue
			}
			clones[filepath.Join(fixes, base)] = struct{}{}
		}
		for clone := range clones {
			owned, ownErr := ownership(clone)
			if ownErr != nil {
				return pruned, scanned, ownErr
			}
			count, pruneErr := PruneFixCloneFences(clone, cutoff, owned)
			pruned += count
			if pruneErr != nil {
				return pruned, scanned, pruneErr
			}
		}
		if scanned >= budget {
			setFixCloneFenceSweepResume(home, ordered[(offset+step+1)%len(ordered)])
			return pruned, scanned, nil
		}
	}
	setFixCloneFenceSweepResume(home, "")
	return pruned, scanned, nil
}

var fixCloneFenceSweepCursor = struct {
	sync.Mutex
	repoByHome map[string]string
}{repoByHome: map[string]string{}}

func fixCloneFenceSweepResume(home string) string {
	fixCloneFenceSweepCursor.Lock()
	defer fixCloneFenceSweepCursor.Unlock()
	return fixCloneFenceSweepCursor.repoByHome[home]
}

func setFixCloneFenceSweepResume(home, repo string) {
	fixCloneFenceSweepCursor.Lock()
	defer fixCloneFenceSweepCursor.Unlock()
	if repo == "" {
		delete(fixCloneFenceSweepCursor.repoByHome, home)
		return
	}
	fixCloneFenceSweepCursor.repoByHome[home] = repo
}

// FenceFixCloneQuarantineNameForTest exposes fence creation to the CLI package's
// accounting tests. Doctor has to distinguish a fence this code wrote from a file
// a writer planted, and a test that hand-rolls the marker would pass even if
// production stopped writing it.
func FenceFixCloneQuarantineNameForTest(path string) (bool, string, error) {
	return fenceFixCloneQuarantineName(path)
}

func newFixCloneQuarantinePath(path string) (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generate fix clone quarantine suffix: %w", err)
	}
	return filepath.Clean(path) + fixCloneQuarantinePrefix + hex.EncodeToString(suffix), nil
}

// pathPresent distinguishes "absent" from "cannot tell", so a stat failure is
// never read as an absence by a caller that deletes things.
func pathPresent(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, err
	}
}

func isHexObjectName(value string, lengths ...int) bool {
	validLength := false
	for _, length := range lengths {
		if len(value) == length {
			validLength = true
			break
		}
	}
	if !validLength {
		return false
	}
	for i := 0; i < len(value); i++ {
		if (value[i] < '0' || value[i] > '9') &&
			(value[i] < 'a' || value[i] > 'f') &&
			(value[i] < 'A' || value[i] > 'F') {
			return false
		}
	}
	return true
}

// looseGitObjectDatabase reports the object database owning a loose object.
//
// Neither the hex-fanout NAME nor a plausible header is evidence: ordinary
// ignored content-addressed build output uses the same layout, and a truncated or
// synthetic Git-shaped cache entry can carry a well-formed header. The candidate
// is accepted only when its decompressed bytes hash to the name it is stored
// under, which is the same property Git itself relies on — so a real object is
// always recognised and a corrupt or fabricated one never is.
func looseGitObjectDatabase(absolute, rel string, entry fs.DirEntry) (string, error) {
	if entry.IsDir() || !isHexObjectName(entry.Name(), 38, 62) {
		return "", nil
	}
	fanout := filepath.Base(filepath.Dir(rel))
	if !isHexObjectName(fanout, 2) {
		return "", nil
	}
	file, err := os.Open(absolute)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect possible loose Git object %s: %w", absolute, err)
	}
	defer file.Close()
	reader, err := zlib.NewReader(file)
	if err != nil {
		return "", nil // not zlib: ordinary content-addressed content
	}
	defer reader.Close()
	if !looseObjectHashMatchesName(reader, strings.ToLower(fanout+entry.Name())) {
		return "", nil
	}
	return filepath.Dir(filepath.Dir(rel)), nil
}

// looseObjectHashMatchesName streams the decompressed object through the Git hash
// its storage name implies and compares the two.
//
// The header read is BOUNDED. A Git object header is `<type> <size>\x00` and the
// longest legal one is well under 32 bytes, so a candidate whose decompressed
// stream carries no NUL inside that window is rejected without buffering it — an
// unbounded ReadBytes(0) on a malformed multi-gigabyte cache entry could otherwise
// exhaust the daemon. Valid objects of any size still STREAM: only the header is
// bounded, the content flows through the hasher below.
func looseObjectHashMatchesName(reader io.Reader, name string) bool {
	buffered := bufio.NewReader(reader)
	header, err := readBoundedGitObjectHeader(buffered)
	if err != nil || !isGitObjectHeader(header) {
		return false
	}
	declared, err := strconv.ParseInt(string(header[bytes.IndexByte(header, ' ')+1:len(header)-1]), 10, 64)
	if err != nil || declared < 0 {
		return false
	}
	var digest hash.Hash
	switch len(name) {
	case 40:
		digest = sha1.New()
	case 64:
		digest = sha256.New()
	default:
		return false
	}
	digest.Write(header)
	// The hash is what decides; this bound is why the decision is cheap. Reading
	// one byte past the declared size caps the work an adversarial header can ask
	// for, and a length mismatch short-circuits before the comparison.
	copied, err := io.Copy(digest, io.LimitReader(buffered, declared+1))
	if err != nil || copied != declared {
		return false
	}
	return hex.EncodeToString(digest.Sum(nil)) == name
}

// gitObjectHeaderLimit bounds the header read. "commit " plus a 20-digit size plus
// the NUL is 28 bytes; 32 leaves room without letting a malformed stream dictate
// the allocation.
const gitObjectHeaderLimit = 32

// readBoundedGitObjectHeader reads up to gitObjectHeaderLimit bytes looking for
// the header's NUL terminator, consuming only what it returns so the caller can go
// on streaming the content.
func readBoundedGitObjectHeader(reader *bufio.Reader) ([]byte, error) {
	header := make([]byte, 0, gitObjectHeaderLimit)
	for len(header) < gitObjectHeaderLimit {
		next, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		header = append(header, next)
		if next == 0 {
			return header, nil
		}
	}
	return nil, errors.New("loose object header is not NUL-terminated within its bound")
}

func isGitObjectHeader(header []byte) bool {
	if len(header) < 4 || header[len(header)-1] != 0 {
		return false
	}
	space := bytes.IndexByte(header, ' ')
	if space <= 0 {
		return false
	}
	switch string(header[:space]) {
	case "commit", "tree", "blob", "tag":
	default:
		return false
	}
	size := header[space+1 : len(header)-1]
	if len(size) == 0 || (len(size) > 1 && size[0] == '0') {
		return false
	}
	for _, digit := range size {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// packedGitObjectDatabase accepts a pack only when its CHECKSUMS verify, not on
// naming, size or a readable header: an ordinary cache entry can carry all three,
// and retaining on them keeps every clone with such a cache forever.
//
// Git closes every pack and index with a trailing digest over the bytes that
// precede it, and the index also records the pack's digest. Recomputing those is
// the same integrity test Git applies when it opens the pair, and it is decided
// entirely by content: a fabricated or truncated file cannot satisfy it, and a
// genuine pack of any size always does. It is done in-process rather than by
// shelling out to `git verify-pack` because this pass runs while holding the
// checkout mutation lock and must not depend on a Git binary or spend a subprocess
// per candidate file.
func packedGitObjectDatabase(ctx context.Context, absolute, rel string, entry fs.DirEntry, verify packIndexVerifier) (string, error) {
	if entry.IsDir() || filepath.Base(filepath.Dir(rel)) != "pack" {
		return "", nil
	}
	name := entry.Name()
	if !strings.HasPrefix(name, "pack-") || !strings.HasSuffix(name, ".pack") {
		return "", nil
	}
	hashName := strings.TrimSuffix(strings.TrimPrefix(name, "pack-"), ".pack")
	if !isHexObjectName(hashName, 40, 64) {
		return "", nil
	}
	packDigest, ok, err := verifyGitPack(absolute, len(hashName)/2)
	if err != nil || !ok {
		return "", err
	}
	// The index must verify too, and must name THIS pack: Git records the pack's
	// trailing digest inside the index, immediately before the index's own.
	indexPath := strings.TrimSuffix(absolute, ".pack") + ".idx"
	indexed, err := verifyGitPackIndex(indexPath, packDigest)
	if err != nil || !indexed {
		return "", err
	}
	// Checksums prove the FILES are intact; they do not prove the pair holds
	// readable objects. A fabricated but self-consistent PACK/idx satisfies every
	// digest above, so the last word belongs to Git itself, which walks the entries,
	// offsets and CRCs before it will read from the pair.
	if verify != nil {
		if err := verify(ctx, indexPath); err != nil {
			return "", nil
		}
	}
	return filepath.Dir(filepath.Dir(rel)), nil
}

// verifyGitPack checks a pack file's structure and its trailing self-checksum,
// returning that digest so the index can be cross-checked against it.
func verifyGitPack(path string, checksumSize int) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect possible Git pack %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect possible Git pack %s: %w", path, err)
	}
	// header (12) + at least one object byte + the trailing checksum.
	if info.Size() < int64(12+1+checksumSize) {
		return nil, false, nil
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, false, nil
	}
	if string(header[:4]) != "PACK" {
		return nil, false, nil
	}
	if version := binary.BigEndian.Uint32(header[4:8]); version != 2 && version != 3 {
		return nil, false, nil
	}
	if binary.BigEndian.Uint32(header[8:12]) == 0 {
		return nil, false, nil // a pack of nothing holds nothing to lose
	}
	digest := newGitDigest(checksumSize)
	if digest == nil {
		return nil, false, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	if _, err := io.Copy(digest, io.LimitReader(file, info.Size()-int64(checksumSize))); err != nil {
		return nil, false, err
	}
	trailer := make([]byte, checksumSize)
	if _, err := io.ReadFull(file, trailer); err != nil {
		return nil, false, nil
	}
	if !bytes.Equal(digest.Sum(nil), trailer) {
		return nil, false, nil
	}
	return trailer, true, nil
}

// verifyGitPackIndex checks a v2 pack index's magic, version and trailing
// self-checksum, and that the pack digest it records is the one just verified.
func verifyGitPackIndex(path string, packDigest []byte) (bool, error) {
	checksumSize := len(packDigest)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // no index: Git cannot read the pack either
		}
		return false, fmt.Errorf("inspect Git pack index %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect Git pack index %s: %w", path, err)
	}
	// magic (4) + version (4) + fanout (1024) + the two trailing digests.
	if info.Size() < int64(8+1024+2*checksumSize) {
		return false, nil
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(file, header); err != nil {
		return false, nil
	}
	if !bytes.Equal(header[:4], []byte{0xff, 0x74, 0x4f, 0x63}) || binary.BigEndian.Uint32(header[4:8]) != 2 {
		return false, nil
	}
	digest := newGitDigest(checksumSize)
	if digest == nil {
		return false, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	if _, err := io.Copy(digest, io.LimitReader(file, info.Size()-int64(checksumSize))); err != nil {
		return false, err
	}
	trailer := make([]byte, checksumSize)
	if _, err := file.ReadAt(trailer, info.Size()-int64(checksumSize)); err != nil {
		return false, nil
	}
	if !bytes.Equal(digest.Sum(nil), trailer) {
		return false, nil
	}
	recorded := make([]byte, checksumSize)
	if _, err := file.ReadAt(recorded, info.Size()-int64(2*checksumSize)); err != nil {
		return false, nil
	}
	return bytes.Equal(recorded, packDigest), nil
}

func newGitDigest(checksumSize int) hash.Hash {
	switch checksumSize {
	case sha1.Size:
		return sha1.New()
	case sha256.Size:
		return sha256.New()
	default:
		return nil
	}
}

// nestedGitObjectDatabase returns the first Git repository or object database
// below a clone's root object database. CloneOnlyCommit proves only the root
// repository: ignored nested repositories, initialized submodules, and bare
// repositories carry separate commit graphs that disappear with the clone.
func nestedGitObjectDatabase(ctx context.Context, path string, verify packIndexVerifier) (string, error) {
	path = filepath.Clean(path)
	var nested string
	err := filepath.WalkDir(path, func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(path, candidate)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		// Do not walk the root repository's proven object database. Everything
		// else under .git remains visible: submodules and tool-managed nested
		// repositories can keep separate object databases there.
		if entry.IsDir() && rel == filepath.Join(".git", "objects") {
			return fs.SkipDir
		}
		if rel != ".git" && entry.Name() == ".git" {
			nested = rel
			return fs.SkipAll
		}
		objectDB, err := looseGitObjectDatabase(candidate, rel, entry)
		if err != nil {
			return err
		}
		if objectDB == "" {
			if objectDB, err = packedGitObjectDatabase(ctx, candidate, rel, entry, verify); err != nil {
				return err
			}
		}
		if objectDB != "" {
			nested = objectDB
			return fs.SkipAll
		}
		if !entry.IsDir() || entry.Name() != "objects" {
			return nil
		}
		info, infoErr := os.Stat(filepath.Join(candidate, "info"))
		pack, packErr := os.Stat(filepath.Join(candidate, "pack"))
		if infoErr == nil && packErr == nil && info.IsDir() && pack.IsDir() {
			nested = rel
			return fs.SkipAll
		}
		if (infoErr != nil && !os.IsNotExist(infoErr)) || (packErr != nil && !os.IsNotExist(packErr)) {
			return fmt.Errorf("inspect possible Git object database %s: info: %v; pack: %v", candidate, infoErr, packErr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return nested, nil
}

// fixCloneInventory is the exact set of entries a removal is authorised to unlink,
// keyed by path relative to the clone root.
type fixCloneInventory map[string]fixCloneEntry

// errFixCloneTreeChanged aborts a removal whose tree no longer matches the proof.
var errFixCloneTreeChanged = errors.New("fix clone changed after its removal proof")

// scanFixCloneTree walks a proven clone ONCE, returning both answers the removal
// needs: the first nested Git object database (empty when there is none) and, when
// there is none, the inventory the unlink phase is allowed to act on.
// packIndexVerifier is Git's own pack validation, injected so this package keeps
// no Git dependency of its own. A nil verifier falls back to checksum-only
// recognition, which is strictly more conservative (it retains more).
type packIndexVerifier func(ctx context.Context, indexPath string) error

func scanFixCloneTree(ctx context.Context, path string, verify packIndexVerifier) (string, fixCloneInventory, error) {
	path = filepath.Clean(path)
	var nested string
	inventory := fixCloneInventory{}
	err := filepath.WalkDir(path, func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(path, candidate)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		inventory[rel] = fixCloneEntryFrom(entry, info)

		// Do not walk the root repository's proven object database. Everything
		// else under .git remains visible: submodules and tool-managed nested
		// repositories can keep separate object databases there. Its CONTENTS are
		// still inventoried below, because the unlink phase has to remove them.
		if entry.IsDir() && rel == filepath.Join(".git", "objects") {
			return inventoryUnscannedSubtree(ctx, path, rel, inventory)
		}
		if rel != ".git" && entry.Name() == ".git" {
			nested = rel
			return fs.SkipAll
		}
		objectDB, err := looseGitObjectDatabase(candidate, rel, entry)
		if err != nil {
			return err
		}
		if objectDB == "" {
			if objectDB, err = packedGitObjectDatabase(ctx, candidate, rel, entry, verify); err != nil {
				return err
			}
		}
		if objectDB != "" {
			nested = objectDB
			return fs.SkipAll
		}
		if !entry.IsDir() || entry.Name() != "objects" {
			return nil
		}
		info, infoErr := os.Stat(filepath.Join(candidate, "info"))
		pack, packErr := os.Stat(filepath.Join(candidate, "pack"))
		if infoErr == nil && packErr == nil && info.IsDir() && pack.IsDir() {
			nested = rel
			return fs.SkipAll
		}
		if (infoErr != nil && !os.IsNotExist(infoErr)) || (packErr != nil && !os.IsNotExist(packErr)) {
			return fmt.Errorf("inspect possible Git object database %s: info: %v; pack: %v", candidate, infoErr, packErr)
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	if nested != "" {
		return nested, nil, nil
	}
	return "", inventory, nil
}

// inventoryUnscannedSubtree records a subtree the nested-database scan skips. The
// scan skips the root object database because it is already proved by
// CloneOnlyCommit, but every entry under it still has to be inventoried or the
// unlink phase would refuse to remove the clone at all.
func inventoryUnscannedSubtree(ctx context.Context, root, rel string, inventory fixCloneInventory) error {
	err := filepath.WalkDir(filepath.Join(root, rel), func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		child, err := filepath.Rel(root, candidate)
		if err != nil {
			return err
		}
		if child == rel {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		inventory[child] = fixCloneEntryFrom(entry, info)
		return nil
	})
	if err != nil {
		return err
	}
	return fs.SkipDir
}

func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// restoreChangedFixClone puts a partially removed clone back at its managed path
// after a concurrent write aborted the removal. The remaining tree holds the
// writer's new content plus whatever the proof had not yet unlinked, and the next
// pass re-proves all of it from scratch.
func restoreChangedFixClone(quarantine, path string) (bool, error) {
	if err := os.Rename(quarantine, path); err != nil {
		return false, fmt.Errorf("restore fix clone %s to %s after a concurrent write: %w", quarantine, path, err)
	}
	return false, nil
}

// fixCloneFenceOwnership loads the durable fence records for one job. A fence is
// only ours if its nonce is here, so a caller with no records proves nothing and
// every fence-shaped file stays a survivor.
func (e Engine) fixCloneFenceOwnership(ctx context.Context, jobID string) (FixCloneFenceOwnership, error) {
	events, err := e.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return FixCloneFenceOwnership{}, err
	}
	records := make([]string, 0, 2)
	for _, event := range events {
		if event.Kind == JobEventFixCloneFenced {
			records = append(records, event.Message)
		}
	}
	return NewFixCloneFenceOwnership(records), nil
}

// fenceFixCloneQuarantineNameForJob writes a fence and records its nonce in the
// same job's event log, which is what later passes match the file against.
func (e Engine) fenceFixCloneQuarantineNameForJob(ctx context.Context, jobID, path string) (bool, error) {
	fenced, nonce, err := fenceFixCloneQuarantineName(path)
	if err != nil || !fenced {
		return false, err
	}
	if err := e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{
		JobID:   jobID,
		Kind:    JobEventFixCloneFenced,
		Message: fmt.Sprintf("%s %s", path, nonce),
	}); err != nil {
		// A fence whose record failed to persist would be indistinguishable from a
		// writer's file forever: remove it and let the caller retain instead.
		_ = os.Remove(path)
		return false, fmt.Errorf("record fix clone fence %s: %w", path, err)
	}
	return true, nil
}

// reclaimAgedTerminalFixClone removes a terminal fix worktree's independent
// clone only after proving that its object database holds nothing unpublished.
// A fix worktree is a standalone clone, so removal takes its objects with it:
// HEAD ancestry is not a sufficient proof, because side branches, tags, stashes
// and reflog-only commits all survive a clean `git status` and all die with the
// directory.
func (e Engine) reclaimAgedTerminalFixClone(ctx context.Context, jobID string, payload JobPayload, path string) (bool, error) {
	expected, err := FixWorktreePath(e.Home, payload.Repo, jobID)
	if err != nil || filepath.Clean(path) != filepath.Clean(expected) {
		return false, fmt.Errorf("refusing TTL reclaim for unmanaged fix worktree %s", path)
	}
	// Absence is observed FIRST, then the quarantines. That order is what makes the
	// inference sound against a concurrent pass: a rename requires the path to
	// exist, so a path already seen absent cannot acquire a quarantine afterwards,
	// while the reverse order lets a rename slip between the scan and the stat and
	// makes this pass record a completed removal for a clone that is still alive.
	present, err := pathPresent(path)
	if err != nil {
		return false, fmt.Errorf("inspect aged terminal fix worktree %s: %w", path, err)
	}
	preOwned, err := e.fixCloneFenceOwnership(ctx, jobID)
	if err != nil {
		return false, err
	}
	quarantines, err := FixCloneQuarantines(path, preOwned)
	if err != nil {
		return false, fmt.Errorf("inspect fix clone quarantines beside %s: %w", path, err)
	}
	switch {
	case len(quarantines) > 1:
		return false, fmt.Errorf("refusing TTL reclaim: %d quarantined fix clones survive beside %s: %s", len(quarantines), path, strings.Join(quarantines, ", "))
	case len(quarantines) == 1 && present:
		return false, fmt.Errorf("refusing TTL reclaim: quarantined fix clone %s from an interrupted removal still exists beside %s", quarantines[0], path)
	case len(quarantines) == 1:
		// A survivor is only restorable when it is a DIRECTORY this code could have
		// renamed aside. A symlink or a file at that name was created by something
		// else: restoring it would install a foreign object as the clone and then
		// re-prove it as one, so the pass refuses and leaves it for an operator.
		info, statErr := os.Lstat(quarantines[0])
		if statErr != nil {
			return false, fmt.Errorf("inspect quarantined fix clone %s: %w", quarantines[0], statErr)
		}
		if !info.IsDir() {
			return false, fmt.Errorf("refusing TTL reclaim: quarantine name %s beside %s is a %s created by another writer, not an interrupted removal", quarantines[0], path, info.Mode().Type())
		}
		// Interrupted removal: restore the clone and let a later pass re-prove it
		// from scratch. The earlier proofs died with the interrupted pass.
		if renameErr := os.Rename(quarantines[0], path); renameErr != nil {
			return false, fmt.Errorf("restore interrupted quarantined fix clone %s to %s: %w", quarantines[0], path, renameErr)
		}
		return false, e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{
			JobID: jobID, Kind: "delegation_worktree_quarantine_restored",
			Message: fmt.Sprintf("restored fix clone %s after an interrupted TTL removal; safety will be re-proven", path),
		})
	case !present:
		return e.completeAgedTerminalFixWorktreeReclaim(ctx, jobID, path)
	}
	// A live process wins before any manager capability or probe matters: the
	// cheapest gate is also the one that must never be skipped. An INCONCLUSIVE
	// probe retains the clone too, but it is recorded: an unreadable process table
	// makes the whole feature inert, and a silent keep is indistinguishable from a
	// worker that is genuinely still running.
	if live, known := e.worktreeLiveness(path); !known {
		return false, e.recordFixCloneLivenessUnknown(ctx, jobID, path)
	} else if live {
		return false, e.recordFixCloneRetainedLive(ctx, jobID, path)
	}
	manager, ok := e.DelegationWorktrees.(WritableWorktreeLineageManager)
	if !ok || manager == nil {
		return false, errors.New("delegation worktree manager cannot prove fix worktree lineage")
	}
	// Cleanliness here means NO UNSAVED WORK — tracked modifications and untracked
	// files. Ignored build output is allowed only after the separate scan below
	// proves that it contains no nested Git repository or object database. The
	// root object-database proof cannot see commits stored in those databases.
	clean, err := manager.WorktreeCleanAt(ctx, path)
	if err != nil {
		return false, fmt.Errorf("prove aged terminal fix worktree clean: %w", err)
	}
	if !clean {
		return false, e.recordFixCloneRetainedDirty(ctx, jobID, path)
	}
	nested, err := nestedGitObjectDatabase(ctx, path, manager.VerifyPackIndex)
	if err != nil {
		return false, fmt.Errorf("inspect aged terminal fix worktree for nested Git object databases: %w", err)
	}
	if nested != "" {
		return false, e.retainFixCloneWithNestedRepository(ctx, jobID, path, nested)
	}
	// The clone's own `origin` is writable by whatever ran inside it, so it is not
	// evidence: the registered repository checkout supplies the trusted URL.
	remoteURL, err := manager.RemoteURL(ctx, "origin")
	if err != nil {
		return false, fmt.Errorf("resolve trusted remote url for aged terminal fix worktree: %w", err)
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, remoteWorktreeReachabilityTimeout)
	err = manager.RefreshCloneProofRefs(probeCtx, path, remoteURL)
	cancelProbe()
	if err != nil {
		return false, fmt.Errorf("refresh trusted remote refs for aged terminal fix worktree: %w", err)
	}
	unpublished, err := manager.CloneOnlyCommit(ctx, path)
	if err != nil {
		return false, fmt.Errorf("prove aged terminal fix worktree holds no unpublished commits: %w", err)
	}
	if unpublished != "" {
		return false, e.retainFixCloneWithUnpublishedCommits(ctx, jobID, path, unpublished)
	}
	if live, known := e.worktreeLiveness(path); !known {
		return false, e.recordFixCloneLivenessUnknown(ctx, jobID, path)
	} else if live {
		return false, e.recordFixCloneRetainedLive(ctx, jobID, path)
	}
	// Serialise only the MUTATION window — rename, re-proof, delete — against a
	// second reclaimer on this host (the documented `gitmoot daemon restart`
	// double-daemon overlap). The read-only proofs and the remote fetch above stay
	// outside the lock: holding a shared checkout lock across a two-minute network
	// timeout would delay every other pass that takes the same key.
	//
	// An empty checkout path makes that lock a silent no-op, so the removal refuses
	// rather than proceeding unserialised: an unenforceable claim is worse than a
	// retained directory.
	if strings.TrimSpace(e.DelegationCheckout) == "" {
		return false, errors.New("refusing TTL fix clone removal: no registered checkout to serialise reclaimers on")
	}
	lockCtx := context.WithoutCancel(ctx)
	releaseCheckoutLock, _, err := acquireCheckoutMutationLockWithWait(lockCtx, e.Store, e.DelegationCheckout, "worktree-ttl-reclaim:"+jobID, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("lock checkout for TTL fix clone reclaim: %w", err)
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
	quarantine, err := newFixCloneQuarantinePath(path)
	if err != nil {
		return false, err
	}
	if err := os.Rename(path, quarantine); err != nil {
		return false, fmt.Errorf("quarantine aged terminal fix worktree %s: %w", path, err)
	}
	restore := func(reason error) (bool, error) {
		if renameErr := os.Rename(quarantine, path); renameErr != nil {
			if reason != nil {
				return false, fmt.Errorf("restore quarantined fix clone %s to %s: %w (while retaining after %v)", quarantine, path, renameErr, reason)
			}
			return false, fmt.Errorf("restore quarantined fix clone %s to %s: %w", quarantine, path, renameErr)
		}
		return false, reason
	}
	if live, known := e.worktreeLiveness(quarantine); !known {
		if _, restoreErr := restore(nil); restoreErr != nil {
			return false, restoreErr
		}
		return false, e.recordFixCloneLivenessUnknown(ctx, jobID, path)
	} else if live {
		if _, restoreErr := restore(nil); restoreErr != nil {
			return false, restoreErr
		}
		return false, e.recordFixCloneRetainedLive(ctx, jobID, path)
	}
	clean, err = manager.WorktreeCleanAt(ctx, quarantine)
	if err != nil {
		return restore(fmt.Errorf("recheck quarantined fix clone clean: %w", err))
	}
	if !clean {
		if _, restoreErr := restore(nil); restoreErr != nil {
			return false, restoreErr
		}
		return false, e.recordFixCloneRetainedDirty(ctx, jobID, path)
	}
	unpublished, err = manager.CloneOnlyCommit(ctx, quarantine)
	if err != nil {
		return restore(fmt.Errorf("recheck quarantined fix clone for unpublished commits: %w", err))
	}
	if unpublished != "" {
		if _, restoreErr := restore(nil); restoreErr != nil {
			return false, restoreErr
		}
		return false, e.retainFixCloneWithUnpublishedCommits(ctx, jobID, path, unpublished)
	}
	// Seal the proof path before the final content scan. CloneOnlyCommit is the
	// last operation that legitimately knows the first random quarantine name, so
	// after this rename a path-based writer can only act on names we then FENCE.
	sealed, err := newFixCloneQuarantinePath(path)
	if err != nil {
		return restore(err)
	}
	if err := os.Rename(quarantine, sealed); err != nil {
		return restore(fmt.Errorf("seal proved fix clone %s as %s: %w", quarantine, sealed, err))
	}
	freed := quarantine
	quarantine = sealed
	// The rename freed the first name, and a writer that learned it can recreate
	// it at any later instant — including after the final survivor scan, where no
	// later pass would ever look again. Fencing it with a zero-byte file makes that
	// impossible rather than merely unobserved.
	if fenced, err := e.fenceFixCloneQuarantineNameForJob(ctx, jobID, freed); err != nil {
		return restore(err)
	} else if !fenced {
		if _, restoreErr := restore(nil); restoreErr != nil {
			return false, restoreErr
		}
		return false, e.retainFixCloneWithRacingQuarantineWriter(ctx, jobID, path, freed)
	}
	// ONE walk answers both remaining questions: is there a nested object database,
	// and exactly which entries is the removal authorised to unlink.
	nested, inventory, err := scanFixCloneTree(ctx, quarantine, manager.VerifyPackIndex)
	if err != nil {
		return restore(fmt.Errorf("recheck sealed fix clone for nested Git object databases: %w", err))
	}
	if nested != "" {
		if _, restoreErr := restore(nil); restoreErr != nil {
			return false, restoreErr
		}
		return false, e.retainFixCloneWithNestedRepository(ctx, jobID, path, nested)
	}
	// An existing cwd or open file descriptor follows the second rename. Refuse
	// removal unless /proc conclusively shows that no such writer survives. This is
	// a cheap early exit, NOT the guarantee: the guarantee is the inventoried
	// unlink below, which the kernel enforces at the moment of removal.
	if live, known := e.worktreeWriterLiveness(quarantine); !known {
		if _, restoreErr := restore(nil); restoreErr != nil {
			return false, restoreErr
		}
		return false, e.recordFixCloneLivenessUnknown(ctx, jobID, path)
	} else if live {
		if _, restoreErr := restore(nil); restoreErr != nil {
			return false, restoreErr
		}
		return false, e.recordFixCloneRetainedLive(ctx, jobID, path)
	}
	// Remove EXACTLY what the scan above proved. Content a writer created between
	// that scan and this unlink is not in the inventory, so it is never removed and
	// its parent's rmdir fails instead — the removal aborts and the clone is put
	// back with the new content intact.
	if err := removeInventoriedTree(quarantine, inventory); err != nil {
		if errors.Is(err, errFixCloneTreeChanged) {
			if _, restoreErr := restoreChangedFixClone(quarantine, path); restoreErr != nil {
				return false, restoreErr
			}
			return false, e.retainFixCloneWithConcurrentWrite(ctx, jobID, path, err)
		}
		return false, fmt.Errorf("remove quarantined fix clone %s: %w", quarantine, err)
	}
	// Fence the sealed name too: between RemoveAll and this call it is the only
	// other name a writer could have observed. Losing the name means something
	// else now owns it — a directory, or a SYMLINK aimed at a directory it keeps
	// writing into — so the reclaim records why it did not complete instead of
	// returning silently, which previously let the next pass see no survivor and
	// retire the job.
	if fenced, err := e.fenceFixCloneQuarantineNameForJob(ctx, jobID, quarantine); err != nil {
		return false, err
	} else if !fenced {
		return false, e.retainFixCloneWithRacingQuarantineWriter(ctx, jobID, path, quarantine)
	}
	owned, err := e.fixCloneFenceOwnership(ctx, jobID)
	if err != nil {
		return false, err
	}
	if survivors, err := FixCloneQuarantines(path, owned); err != nil {
		return false, fmt.Errorf("recheck fix clone quarantine siblings after removal: %w", err)
	} else if len(survivors) != 0 {
		return false, e.retainFixCloneWithRacingQuarantineWriter(ctx, jobID, path, strings.Join(survivors, ", "))
	}
	// Spent fences are bounded here rather than kept forever: this pass has just
	// proved no process holds a working directory or descriptor inside the clone,
	// so a fence from an earlier pass can no longer be protecting anything.
	if _, err := PruneFixCloneFences(path, time.Now().UTC().Add(-fixCloneFenceRetention), owned); err != nil {
		return false, err
	}
	// A concurrent actor may have restored this clone to its original path while we
	// held the quarantine name. Completing the reclaim then would record a removal
	// for a clone that is alive again.
	if present, err := pathPresent(path); err != nil {
		return false, fmt.Errorf("recheck aged terminal fix worktree %s after removal: %w", path, err)
	} else if present {
		return false, nil
	}
	return e.completeAgedTerminalFixWorktreeReclaim(ctx, jobID, path)
}

func (e Engine) retainFixCloneWithUnpublishedCommits(ctx context.Context, jobID, path, sha string) error {
	if err := e.recordFixCloneRetention(ctx, jobID, "delegation_worktree_retained_unpublished",
		fmt.Sprintf("fix clone %s retained after TTL: commit %s is in no trusted remote ref", path, sha)); err != nil {
		return err
	}
	return e.deferDelegationCleanupObligation(context.WithoutCancel(ctx), jobID, path, db.CleanupReasonUnpublishedCommits)
}

func (e Engine) retainFixCloneWithNestedRepository(ctx context.Context, jobID, path, nested string) error {
	if err := e.recordFixCloneRetention(ctx, jobID, "delegation_worktree_retained_unpublished",
		fmt.Sprintf("fix clone %s retained after TTL: nested Git object database %s has unproved recoverability", path, nested)); err != nil {
		return err
	}
	return e.deferDelegationCleanupObligation(context.WithoutCancel(ctx), jobID, path, db.CleanupReasonUnpublishedCommits)
}

// retainFixCloneWithRacingQuarantineWriter records the one outcome the fence can
// observe: something else created a directory at a quarantine name of this clone
// while the proofs ran. Its content is unproven, so the clone is restored and the
// obligation stays open rather than the racing directory being deleted.
func (e Engine) retainFixCloneWithRacingQuarantineWriter(ctx context.Context, jobID, path, quarantine string) error {
	if err := e.recordFixCloneRetention(ctx, jobID, "delegation_worktree_retained_unpublished",
		fmt.Sprintf("fix clone %s retained after TTL: another writer created quarantine %s during the removal proof", path, quarantine)); err != nil {
		return err
	}
	return e.deferDelegationCleanupObligation(context.WithoutCancel(ctx), jobID, path, db.CleanupReasonUnpublishedCommits)
}

// retainFixCloneWithConcurrentWrite records the retention an aborted removal
// takes: the tree gained or changed content between the proof and the unlink, so
// the removal stopped with that content on disk. It is the observable half of the
// inventory guard — without it, a clone that keeps losing this race would look
// like a pass that simply never runs.
func (e Engine) retainFixCloneWithConcurrentWrite(ctx context.Context, jobID, path string, cause error) error {
	if err := e.recordFixCloneRetention(ctx, jobID, "delegation_worktree_retained_unpublished",
		fmt.Sprintf("fix clone %s retained after TTL: %v, so the proved removal was abandoned with its content intact", path, cause)); err != nil {
		return err
	}
	return e.deferDelegationCleanupObligation(context.WithoutCancel(ctx), jobID, path, db.CleanupReasonUnpublishedCommits)
}

// recordFixCloneLivenessUnknown makes an inert deployment visible. A process
// table that cannot be read (a non-root daemon seeing EACCES on another user's
// process, or a host with no /proc at all) retains every clone forever, and
// without this event that is indistinguishable from clones that are genuinely
// still in use.
func (e Engine) recordFixCloneLivenessUnknown(ctx context.Context, jobID, path string) error {
	return e.recordFixCloneRetention(ctx, jobID, "delegation_worktree_liveness_unknown",
		fmt.Sprintf("fix clone %s retained after TTL: process liveness could not be proven", path))
}

// recordFixCloneRetainedDirty records the retention a clone with UNSAVED WORK
// hits: tracked modifications or untracked files. Ignored content never reaches
// this branch — the gate is WorktreeCleanAt, which does not consult it — because
// the repository declares ignored paths regenerable and demanding a pristine tree
// made the whole pass inert on any repo with build output.
func (e Engine) recordFixCloneRetainedDirty(ctx context.Context, jobID, path string) error {
	return e.recordFixCloneRetention(ctx, jobID, "delegation_worktree_retained_dirty",
		fmt.Sprintf("fix clone %s retained after TTL: working tree holds unsaved work (tracked or untracked content)", path))
}

// recordFixCloneRetainedLive records the ordinary, expected retention: something
// on this host still has its working directory inside the clone. It is recorded
// for the same reason as the others — every outcome of this pass should be
// attributable from the job log alone.
func (e Engine) recordFixCloneRetainedLive(ctx context.Context, jobID, path string) error {
	return e.recordFixCloneRetention(ctx, jobID, "delegation_worktree_retained_live",
		fmt.Sprintf("fix clone %s retained after TTL: a live process holds a working directory inside it", path))
}

// recordFixCloneRetention appends a retention event once PER REASON PER JOB.
//
// It dedupes on the event KIND, because a later retention for the same reason
// naming a different commit is the same standing fact; on the event LOG rather
// than the cleanup obligation, because the obligation reason is rewritten by the
// daemon's generic per-pass deferral and a crash between two writes must not
// re-announce; and in ONE statement, because a read-then-write pair lets two
// concurrent reclaimers both see no event and both insert.
func (e Engine) recordFixCloneRetention(ctx context.Context, jobID, kind, message string) error {
	return e.Store.AddJobEventIfAbsent(context.WithoutCancel(ctx), db.JobEvent{
		JobID: jobID, Kind: kind, Message: message,
	})
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
		return e.reclaimAgedTerminalFixClone(ctx, jobID, payload, path)
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
