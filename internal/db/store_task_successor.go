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
	root, err := s.taskChainRoot(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return latestTaskInChain(ctx, s.db, root)
}

// taskChainRoot is id's chain root. An id that only LOOKS like a successor
// (ends in -successor-<n>) but was never recorded as one is its own root, so
// a foreign task id is never re-rooted onto an unrelated task (#2278 review).
func (s *Store) taskChainRoot(ctx context.Context, id string) (string, error) {
	id = strings.TrimSpace(id)
	if _, err := s.GetTask(ctx, id); err != nil {
		return "", err
	}
	root := TaskSuccessorRoot(id)
	if root == id {
		return id, nil
	}
	var recorded int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE task_id = ? AND kind = ?`, id, TaskEventCreatedAsSuccessor).Scan(&recorded); err != nil {
		return "", err
	}
	if recorded == 0 {
		return id, nil
	}
	return root, nil
}

func latestTaskInChain(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, root string) (Task, error) {
	latest, err := scanTask(q.QueryRowContext(ctx, taskSelectSQL()+` FROM tasks WHERE id = ?`, root))
	if err != nil {
		return Task{}, err
	}
	for n := 2; n <= maxTaskSuccessors; n++ {
		// Only a row created AS a successor continues the chain; a foreign task
		// that merely shares the name does not.
		next, err := scanTask(q.QueryRowContext(ctx, taskSelectSQL()+` FROM tasks WHERE id = ? AND EXISTS (
			SELECT 1 FROM task_events WHERE task_events.task_id = tasks.id AND task_events.kind = ?)`,
			taskSuccessorID(root, n), TaskEventCreatedAsSuccessor))
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
// It also TAKES OVER the disposed task's branch: a repository branch belongs to
// one task (idx_tasks_repo_branch_unique), and every lookup by branch (review
// dispatch, workflow advancement, recovery) must reach the live work, not the
// disposed row, or the successor's verdict could never advance its task (#2278
// review). The disposed row keeps its state; the transfer is recorded in both
// tasks' events. The worktree is not copied, since disposal may have reclaimed
// it. The successor starts in initialState. When the newest task is already a
// live successor it is returned with created=false, so a repeated or concurrent
// call is harmless. A chain whose root was never disposed is refused with
// ErrTaskNotDisposed. Everything commits in one IMMEDIATE transaction.
func (s *Store) CreateTaskSuccessor(ctx context.Context, id string, disposedStates []string, initialState, reason string) (Task, bool, error) {
	root, err := s.taskChainRoot(ctx, id)
	if err != nil {
		return Task{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Take the write lock BEFORE reading the chain (a no-op write on the root),
	// so concurrent callers, in this process or another, serialize: the second
	// sees the first's successor and returns it instead of colliding on its id.
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET id = id WHERE id = ?`, root); err != nil {
		return Task{}, false, err
	}
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
	// Count from the chain root, never from a root that merely looks like a
	// successor itself.
	next := 2
	if latest.ID != root {
		n, err := strconv.Atoi(strings.TrimPrefix(latest.ID, root+taskSuccessorSeparator))
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
		Branch:       latest.Branch,
	}
	if latest.Branch != "" {
		// Hand the branch over before inserting, so the unique index never sees
		// two owners. Only the branch column changes on the disposed row.
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET branch = '' WHERE id = ? AND state = ?`, latest.ID, latest.State); err != nil {
			return Task{}, false, err
		}
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
		{TaskID: latest.ID, Kind: TaskEventSuccessorCreated, Reason: "successor " + successor.ID + " created" + suffixIfSet(", taking over branch ", latest.Branch) + suffixIfSet("; ", reason)},
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
