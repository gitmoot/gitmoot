package workflow

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jerryfane/gitmoot/internal/db"
)

type BranchCreator interface {
	CreateBranch(ctx context.Context, branch string, base string) error
}

type TaskBranchRequest struct {
	Repo       string
	GoalID     string
	TaskID     string
	TaskTitle  string
	Branch     string
	BaseBranch string
	Owner      string
	Checkout   string
}

func (e Engine) StartTaskBranch(ctx context.Context, request TaskBranchRequest, brancher BranchCreator) (db.Task, error) {
	if err := e.validate(); err != nil {
		return db.Task{}, err
	}
	if brancher == nil {
		return db.Task{}, errors.New("branch creator is required")
	}
	if err := validateTaskBranchRequest(request); err != nil {
		return db.Task{}, err
	}
	existing, err := e.Store.GetTaskByRepoBranch(ctx, request.Repo, request.Branch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return db.Task{}, err
	}
	if err == nil && existing.ID != request.TaskID {
		return db.Task{}, errors.New("task branch is already assigned to another task")
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLock(ctx, e.Store, request.Checkout, "task:"+request.TaskID, time.Now().UTC())
	if err != nil {
		return db.Task{}, err
	}
	defer func() {
		if releaseCheckoutLock != nil {
			_ = releaseCheckoutLock(context.Background())
		}
	}()
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

	if err := brancher.CreateBranch(ctx, request.Branch, request.BaseBranch); err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, err
	}
	taskGoalID := request.GoalID
	taskTitle := request.TaskTitle
	existingTask, err := e.Store.GetTask(ctx, request.TaskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, err
	}
	if err == nil {
		if taskGoalID == "" {
			taskGoalID = existingTask.GoalID
		}
		if taskTitle == "" {
			taskTitle = existingTask.Title
		}
	}
	task := db.Task{
		ID:           request.TaskID,
		RepoFullName: request.Repo,
		GoalID:       taskGoalID,
		Title:        taskTitle,
		State:        string(TaskImplementing),
		Branch:       request.Branch,
	}
	if err := e.Store.UpsertTask(ctx, task); err != nil {
		if createdLock {
			_, _ = e.Store.ReleaseLock(ctx, lock)
		}
		return db.Task{}, err
	}
	return task, nil
}

// delegationBranchName derives the git branch for a delegated implement job.
// It prefers a slugged form of the delegation's requested worktree label and
// otherwise falls back to a deterministic gitmoot-delegation-<parent-short>-<id>
// name so sibling delegations from the same parent never collide.
func delegationBranchName(d Delegation, parentJobID string, delegationID string) string {
	if hint := slug(d.Worktree); hint != "" {
		return hint
	}
	return fmt.Sprintf("gitmoot-delegation-%s-%s", parentShort(parentJobID), delegationID)
}

// slug normalizes an arbitrary label into a lowercase, dash-separated token
// safe for use in branch names. Mirrors internal/cli/workflow.go::slug.
func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash && builder.Len() > 0 {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

// parentShort returns the first eight hex characters of the SHA-1 of the parent
// job id, giving a stable short identifier for delegation branch names.
func parentShort(parentJobID string) string {
	sum := sha1.Sum([]byte(parentJobID))
	return hex.EncodeToString(sum[:])[:8]
}

func validateTaskBranchRequest(request TaskBranchRequest) error {
	switch {
	case strings.TrimSpace(request.Repo) == "":
		return errors.New("task branch repo is required")
	case strings.TrimSpace(request.TaskID) == "":
		return errors.New("task branch task id is required")
	case strings.TrimSpace(request.Branch) == "":
		return errors.New("task branch name is required")
	case strings.TrimSpace(request.Owner) == "":
		return errors.New("task branch owner is required")
	}
	return nil
}
