package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A disposed task (stranded, superseded, dismissed) is never resurrected: work
// that must continue gets a successor task, named <root>-successor-<n>. The
// suffix keeps a review task's "review-pr-<number>-<hash>" prefix parseable.
const (
	TaskEventSuccessorCreated   = "successor_created"
	TaskEventCreatedAsSuccessor = "created_as_successor"

	taskSuccessorSeparator = "-successor-"
	maxTaskSuccessors      = 1000
)

var taskSuccessorSuffix = regexp.MustCompile(`-successor-([0-9]+)$`)

// ErrTaskNotDisposed refuses a successor for a task that can still be used.
var ErrTaskNotDisposed = errors.New("task is not disposed")

// TaskSuccessorRoot returns the first task of id's successor chain.
func TaskSuccessorRoot(id string) string {
	id = strings.TrimSpace(id)
	if loc := taskSuccessorSuffix.FindStringIndex(id); loc != nil {
		return id[:loc[0]]
	}
	return id
}

func taskSuccessorID(root string, n int) string {
	return root + taskSuccessorSeparator + strconv.Itoa(n)
}

// LatestTaskInChain returns the newest task in id's successor chain: id's root
// when it has no successor. It returns sql.ErrNoRows when the root is missing.
func (s *Store) LatestTaskInChain(ctx context.Context, id string) (Task, error) {
	return latestTaskInChain(ctx, s.db, TaskSuccessorRoot(id))
}

func latestTaskInChain(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, root string) (Task, error) {
	latest, err := scanTask(q.QueryRowContext(ctx, taskSelectSQL()+` FROM tasks WHERE id = ?`, root))
	if err != nil {
		return Task{}, err
	}
	for n := 2; n <= maxTaskSuccessors; n++ {
		next, err := scanTask(q.QueryRowContext(ctx, taskSelectSQL()+` FROM tasks WHERE id = ?`, taskSuccessorID(root, n)))
		if errors.Is(err, sql.ErrNoRows) {
			return latest, nil
		}
		if err != nil {
			return Task{}, err
		}
		latest = next
	}
	return Task{}, fmt.Errorf("task %s has more than %d successors", root, maxTaskSuccessors)
}

// CreateTaskSuccessor creates the next successor of id's chain when the newest
// task in it is in one of disposedStates, copying repository, goal and title.
// The branch stays with the disposed task (a repository branch belongs to one
// task: idx_tasks_repo_branch_unique), so dispatch by branch finds the disposed
// task and follows its chain; the worktree is not copied either, since disposal
// may have reclaimed it. The successor
// starts in initialState. When the newest task is already a live successor it
// is returned with created=false, so a repeated call is harmless. A chain whose
// root was never disposed is refused with ErrTaskNotDisposed. The new row and
// the events on both tasks commit together.
func (s *Store) CreateTaskSuccessor(ctx context.Context, id string, disposedStates []string, initialState, reason string) (Task, bool, error) {
	root := TaskSuccessorRoot(id)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback()
	latest, err := latestTaskInChain(ctx, tx, root)
	if err != nil {
		return Task{}, false, err
	}
	if !slices.Contains(disposedStates, latest.State) {
		if latest.ID == root {
			return Task{}, false, fmt.Errorf("%w: task %s is %s", ErrTaskNotDisposed, latest.ID, latest.State)
		}
		return latest, false, tx.Commit()
	}
	next := 2
	if match := taskSuccessorSuffix.FindStringSubmatch(latest.ID); match != nil {
		n, err := strconv.Atoi(match[1])
		if err != nil {
			return Task{}, false, err
		}
		next = n + 1
	}
	successor := Task{
		ID:           taskSuccessorID(root, next),
		RepoFullName: latest.RepoFullName,
		GoalID:       latest.GoalID,
		Title:        latest.Title,
		State:        initialState,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch, worktree_path, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '', CURRENT_TIMESTAMP)`,
		successor.ID, successor.RepoFullName, successor.GoalID, successor.Title, successor.State, successor.Branch); err != nil {
		return Task{}, false, err
	}
	reason = strings.TrimSpace(reason)
	why := fmt.Sprintf("successor of %s (%s", latest.ID, latest.State)
	if latest.Branch != "" {
		why = fmt.Sprintf("successor of %s on branch %s (%s", latest.ID, latest.Branch, latest.State)
	}
	if latest.DisposalReason != "" {
		why += ": " + latest.DisposalReason
	}
	why += ")"
	if reason != "" {
		why += "; " + reason
	}
	for _, event := range []TaskEvent{
		// Informational on the predecessor: it does not move, so FromState and
		// ToState stay empty per the TaskEvent contract.
		{TaskID: latest.ID, Kind: TaskEventSuccessorCreated, Reason: "successor " + successor.ID + " created" + suffixIfSet("; ", reason)},
		{TaskID: successor.ID, Kind: TaskEventCreatedAsSuccessor, ToState: successor.State, Reason: why},
	} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason) VALUES (?, ?, ?, ?, ?)`,
			event.TaskID, event.Kind, event.FromState, event.ToState, event.Reason); err != nil {
			return Task{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Task{}, false, err
	}
	created, err := s.GetTask(ctx, successor.ID)
	return created, true, err
}

func suffixIfSet(separator, value string) string {
	if value == "" {
		return ""
	}
	return separator + value
}
