package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
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
