package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

func (s *Store) UpsertRepo(ctx context.Context, repo Repo) error {
	return s.upsertRepo(ctx, repo, false)
}

// UpsertRepoForce deliberately bypasses linked-worktree overwrite protection.
// It is reserved for the explicit `repo add --force` operator path.
func (s *Store) UpsertRepoForce(ctx context.Context, repo Repo) error {
	return s.upsertRepo(ctx, repo, true)
}

func (s *Store) upsertRepo(ctx context.Context, repo Repo, force bool) error {
	fullName := repo.Owner + "/" + repo.Name
	if strings.TrimSpace(repo.CheckoutPath) != "" && strings.TrimSpace(repo.PrimaryCheckoutPath) == "" {
		if primary, err := (gitutil.Client{Dir: repo.CheckoutPath}).PrimaryWorktree(ctx); err == nil {
			repo.PrimaryCheckoutPath = primary
		}
	}
	if !force && strings.TrimSpace(repo.CheckoutPath) != "" {
		if existing, err := s.GetRepo(ctx, fullName); err == nil && shouldProtectRepoCheckout(existing, repo.CheckoutPath) {
			if linked, linkErr := (gitutil.Client{Dir: repo.CheckoutPath}).IsLinkedWorktree(ctx); linkErr == nil && linked {
				log.Printf("WARNING: keeping registered checkout for %s at %s; refusing linked worktree %s (use gitmoot repo add --force to override)", fullName, existing.CheckoutPath, repo.CheckoutPath)
				repo.CheckoutPath = ""
				repo.PrimaryCheckoutPath = ""
			}
		}
	}
	updatePollInterval := repo.PollInterval
	insertPollInterval := repo.PollInterval
	_, err := s.db.ExecContext(ctx, `INSERT INTO repos(owner, name, full_name, default_branch, remote_url, checkout_path, primary_checkout_path, enabled, poll_interval, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(full_name) DO UPDATE SET
			default_branch = CASE WHEN excluded.default_branch <> '' THEN excluded.default_branch ELSE repos.default_branch END,
			remote_url = CASE WHEN excluded.remote_url <> '' THEN excluded.remote_url ELSE repos.remote_url END,
			checkout_path = CASE WHEN excluded.checkout_path <> '' THEN excluded.checkout_path ELSE repos.checkout_path END,
			primary_checkout_path = CASE WHEN excluded.primary_checkout_path <> '' THEN excluded.primary_checkout_path ELSE repos.primary_checkout_path END,
			poll_interval = CASE WHEN ? <> '' THEN excluded.poll_interval ELSE repos.poll_interval END,
			updated_at = CURRENT_TIMESTAMP`,
		repo.Owner, repo.Name, fullName, repo.DefaultBranch, repo.RemoteURL, repo.CheckoutPath, repo.PrimaryCheckoutPath, insertPollInterval, updatePollInterval)
	return err
}

func shouldProtectRepoCheckout(existing Repo, incoming string) bool {
	if sameRepoCheckoutPath(existing.CheckoutPath, incoming) {
		return false
	}
	if info, err := os.Stat(strings.TrimSpace(existing.CheckoutPath)); err == nil && info.IsDir() {
		return true
	}
	return strings.TrimSpace(existing.PrimaryCheckoutPath) != "" && sameRepoCheckoutPath(existing.CheckoutPath, existing.PrimaryCheckoutPath)
}

func sameRepoCheckoutPath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func (s *Store) GetRepo(ctx context.Context, fullName string) (Repo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT owner, name, default_branch, remote_url, checkout_path, primary_checkout_path, enabled, poll_interval, last_poll_at, last_error
		FROM repos WHERE full_name = ?`, fullName)
	return scanRepo(row)
}

func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner, name, default_branch, remote_url, checkout_path, primary_checkout_path, enabled, poll_interval, last_poll_at, last_error
		FROM repos ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []Repo{}
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

// HealRepoCheckout atomically replaces a repo checkout only when it still has
// the path the caller observed. The compare guard prevents a concurrent,
// deliberate re-registration from being overwritten by a stale healer.
func (s *Store) HealRepoCheckout(ctx context.Context, fullName, expectedCheckoutPath, checkoutPath, primaryCheckoutPath string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE repos
		SET checkout_path = ?, primary_checkout_path = ?, updated_at = CURRENT_TIMESTAMP
		WHERE full_name = ? AND checkout_path = ?`,
		strings.TrimSpace(checkoutPath), strings.TrimSpace(primaryCheckoutPath), strings.TrimSpace(fullName), strings.TrimSpace(expectedCheckoutPath))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) SetRepoEnabled(ctx context.Context, fullName string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE repos SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE full_name = ?`, value, fullName)
	if err != nil {
		return err
	}
	return requireAffected(result, "repo", fullName)
}

// SetRepoPollInterval sets a repository's explicit poll interval. An empty
// value is the inherit sentinel and falls back to the daemon --poll interval.
func (s *Store) SetRepoPollInterval(ctx context.Context, fullName string, interval string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE repos SET poll_interval = ?, updated_at = CURRENT_TIMESTAMP WHERE full_name = ?`, strings.TrimSpace(interval), strings.TrimSpace(fullName))
	if err != nil {
		return err
	}
	return requireAffected(result, "repo", fullName)
}

func (s *Store) UpdateRepoPollResult(ctx context.Context, fullName string, lastPollAt string, lastError string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE repos SET last_poll_at = ?, last_error = ?, updated_at = CURRENT_TIMESTAMP WHERE full_name = ?`, lastPollAt, lastError, fullName)
	if err != nil {
		return err
	}
	return requireAffected(result, "repo", fullName)
}

func (s *Store) RemoveRepo(ctx context.Context, fullName string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE full_name = ?`, fullName)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func scanRepo(row interface{ Scan(dest ...any) error }) (Repo, error) {
	var repo Repo
	var enabled int
	if err := row.Scan(&repo.Owner, &repo.Name, &repo.DefaultBranch, &repo.RemoteURL, &repo.CheckoutPath, &repo.PrimaryCheckoutPath, &enabled, &repo.PollInterval, &repo.LastPollAt, &repo.LastError); err != nil {
		return Repo{}, err
	}
	repo.Enabled = enabled != 0
	return repo, nil
}

func (r Repo) FullName() string {
	if r.Owner == "" || r.Name == "" {
		return ""
	}
	return r.Owner + "/" + r.Name
}

func (s *Store) InsertGoal(ctx context.Context, goal Goal) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO goals(id, title, source, status) VALUES (?, ?, ?, ?)`, goal.ID, goal.Title, goal.Source, goal.Status)
	return err
}

func (s *Store) UpsertGoal(ctx context.Context, goal Goal) error {
	return upsertGoal(ctx, s.db, goal)
}

func upsertGoal(ctx context.Context, execer sqlExecer, goal Goal) error {
	_, err := execer.ExecContext(ctx, `INSERT INTO goals(id, title, source, status, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			source = excluded.source,
			status = excluded.status,
			updated_at = CURRENT_TIMESTAMP`,
		goal.ID, goal.Title, goal.Source, goal.Status)
	return err
}

func (s *Store) ListGoals(ctx context.Context) ([]Goal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, source, status FROM goals ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	goals := []Goal{}
	for rows.Next() {
		var goal Goal
		if err := rows.Scan(&goal.ID, &goal.Title, &goal.Source, &goal.Status); err != nil {
			return nil, err
		}
		goals = append(goals, goal)
	}
	return goals, rows.Err()
}

func (s *Store) UpsertTask(ctx context.Context, task Task) error {
	return upsertTask(ctx, s.db, task)
}

// UpsertTaskUnlessState applies the normal task upsert unless an existing row
// is currently in forbiddenState. The predicate lives on the conflict UPDATE so
// callers that must not resurrect a terminal task remain safe if its state
// changes after their initial read.
func (s *Store) UpsertTaskUnlessState(ctx context.Context, task Task, forbiddenState string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch, worktree_path, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			repo_full_name = excluded.repo_full_name,
			goal_id = excluded.goal_id,
			title = excluded.title,
			state = excluded.state,
			branch = excluded.branch,
			worktree_path = CASE
				WHEN excluded.worktree_path <> '' THEN excluded.worktree_path
				ELSE tasks.worktree_path
			END,
			updated_at = CURRENT_TIMESTAMP
		WHERE tasks.state <> ?`,
		task.ID, task.RepoFullName, task.GoalID, task.Title, task.State, task.Branch, task.WorktreePath,
		strings.TrimSpace(forbiddenState))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *Store) ClearTaskWorktreePath(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET worktree_path = '', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}

func upsertTask(ctx context.Context, execer sqlExecer, task Task) error {
	_, err := execer.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch, worktree_path, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			repo_full_name = excluded.repo_full_name,
			goal_id = excluded.goal_id,
			title = excluded.title,
			state = excluded.state,
			branch = excluded.branch,
			worktree_path = CASE
				WHEN excluded.worktree_path <> '' THEN excluded.worktree_path
				ELSE tasks.worktree_path
			END,
			updated_at = CURRENT_TIMESTAMP`,
		task.ID, task.RepoFullName, task.GoalID, task.Title, task.State, task.Branch, task.WorktreePath)
	return err
}

func (s *Store) UpsertGoalWithTasks(ctx context.Context, goal Goal, tasks []Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := upsertGoal(ctx, tx, goal); err != nil {
		return err
	}
	for _, task := range tasks {
		if err := upsertImportedTask(ctx, tx, task); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func upsertImportedTask(ctx context.Context, execer sqlExecer, task Task) error {
	_, err := execer.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch, worktree_path, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET
				repo_full_name = CASE
					WHEN excluded.repo_full_name <> '' THEN excluded.repo_full_name
					ELSE tasks.repo_full_name
				END,
				goal_id = excluded.goal_id,
				title = excluded.title,
				state = tasks.state,
			branch = tasks.branch,
			worktree_path = tasks.worktree_path,
			updated_at = CURRENT_TIMESTAMP`,
		task.ID, task.RepoFullName, task.GoalID, task.Title, task.State, task.Branch, task.WorktreePath)
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	row := s.db.QueryRowContext(ctx, taskSelectSQL()+` FROM tasks WHERE id = ?`, id)
	return scanTask(row)
}

func (s *Store) GetTaskByBranch(ctx context.Context, branch string) (Task, error) {
	row := s.db.QueryRowContext(ctx, taskSelectSQL()+`
		FROM tasks WHERE branch = ? ORDER BY updated_at DESC, id LIMIT 1`, branch)
	return scanTask(row)
}

func (s *Store) GetTaskByRepoBranch(ctx context.Context, repoFullName string, branch string) (Task, error) {
	row := s.db.QueryRowContext(ctx, taskSelectSQL()+`
		FROM tasks
		WHERE branch = ? AND (repo_full_name = ? OR repo_full_name = '')
		ORDER BY CASE WHEN repo_full_name = ? THEN 0 ELSE 1 END, updated_at DESC, id
		LIMIT 1`, branch, repoFullName, repoFullName)
	return scanTask(row)
}

func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectSQL()+` FROM tasks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) ListTasksByRepo(ctx context.Context, repoFullName string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectSQL()+`
		FROM tasks
		WHERE repo_full_name = ? OR repo_full_name = ''
		ORDER BY CASE WHEN repo_full_name = ? THEN 0 ELSE 1 END, id`, repoFullName, repoFullName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) ListTasksByRepoState(ctx context.Context, repoFullName string, state string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectSQL()+`
		FROM tasks
		WHERE repo_full_name = ? AND state = ?
		ORDER BY id`, repoFullName, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// ListStaleTaskCandidates returns the oldest tasks in one repo whose state is
// in states and whose conservative updated_at activity proxy predates before.
func (s *Store) ListStaleTaskCandidates(ctx context.Context, repoFullName string, states []string, before time.Time, limit int) ([]StaleTaskCandidate, error) {
	if len(states) == 0 || limit <= 0 {
		return []StaleTaskCandidate{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := make([]any, 0, len(states)+3)
	args = append(args, strings.TrimSpace(repoFullName))
	for _, state := range states {
		args = append(args, strings.TrimSpace(state))
	}
	args = append(args, before.UTC().Format("2006-01-02 15:04:05"), limit)
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_full_name, state, branch, updated_at
		FROM tasks
		WHERE repo_full_name = ? AND state IN (`+placeholders+`) AND updated_at < ?
		ORDER BY updated_at, id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StaleTaskCandidate{}
	for rows.Next() {
		var candidate StaleTaskCandidate
		if err := rows.Scan(&candidate.ID, &candidate.RepoFullName, &candidate.State, &candidate.Branch, &candidate.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func (s *Store) AddTaskEvent(ctx context.Context, event TaskEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
		VALUES (?, ?, ?, ?, ?)`, event.TaskID, event.Kind, event.FromState, event.ToState, event.Reason)
	return err
}

func (s *Store) ListTaskEvents(ctx context.Context, taskID string) ([]TaskEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, kind, from_state, to_state, reason, created_at
		FROM task_events WHERE task_id = ? ORDER BY id`, strings.TrimSpace(taskID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskEvent{}
	for rows.Next() {
		var event TaskEvent
		if err := rows.Scan(&event.ID, &event.TaskID, &event.Kind, &event.FromState, &event.ToState, &event.Reason, &event.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

var ErrTaskHasActiveJob = errors.New("task has a queued or running job")

// CompareAndSwapTaskState atomically moves one task state without adding an
// audit event. It exists for legacy lifecycle writers that already changed state
// without task_events but need write-time exclusion against a concurrent
// terminal transition. New audited lifecycle transitions should use
// TransitionTaskStateWithEvent instead.
func (s *Store) CompareAndSwapTaskState(ctx context.Context, taskID, from, to string) (changed bool, currentState string, err error) {
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`,
		strings.TrimSpace(to), strings.TrimSpace(taskID), strings.TrimSpace(from))
	if err != nil {
		return false, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, "", err
	}
	if affected > 0 {
		return true, strings.TrimSpace(to), nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, strings.TrimSpace(taskID)).Scan(&currentState); err != nil {
		return false, "", err
	}
	return false, currentState, nil
}

// TransitionTaskStateWithEvent atomically compares and moves a task state and
// appends its audit event. A failed comparison writes no event and returns the
// current state so callers can distinguish idempotence from a conflicting move.
func (s *Store) TransitionTaskStateWithEvent(ctx context.Context, taskID string, fromStates []string, to string, kind string, reason string) (changed bool, currentState string, err error) {
	return s.transitionTaskStateWithEvent(ctx, taskID, fromStates, to, kind, reason, false)
}

// TransitionTaskStateWithEventIfNoActiveJob adds a queued/running job guard to
// the same transaction as the task CAS. It is reserved for dismissal: broader
// liveness (pending advancement and unsettled cancellation) is checked by the
// caller before entering this transaction, while this guard closes the window
// in which a newly queued/running job could acquire the task.
func (s *Store) TransitionTaskStateWithEventIfNoActiveJob(ctx context.Context, taskID string, fromStates []string, to string, kind string, reason string) (changed bool, currentState string, err error) {
	return s.transitionTaskStateWithEvent(ctx, taskID, fromStates, to, kind, reason, true)
}

func (s *Store) transitionTaskStateWithEvent(ctx context.Context, taskID string, fromStates []string, to string, kind string, reason string, rejectActiveJob bool) (changed bool, currentState string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()

	var repoFullName, branch string
	if err := tx.QueryRowContext(ctx, `SELECT state, repo_full_name, branch FROM tasks WHERE id = ?`, strings.TrimSpace(taskID)).Scan(&currentState, &repoFullName, &branch); err != nil {
		return false, "", err
	}
	allowed := false
	for _, state := range fromStates {
		if currentState == strings.TrimSpace(state) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, currentState, tx.Commit()
	}
	if rejectActiveJob {
		jobID, active, err := activeJobMatchingTaskTx(ctx, tx, strings.TrimSpace(taskID), repoFullName, branch)
		if err != nil {
			return false, currentState, err
		}
		if active {
			return false, currentState, fmt.Errorf("%w: %s", ErrTaskHasActiveJob, jobID)
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`,
		strings.TrimSpace(to), strings.TrimSpace(taskID), currentState)
	if err != nil {
		return false, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, "", err
	}
	if affected == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, strings.TrimSpace(taskID)).Scan(&currentState); err != nil {
			return false, "", err
		}
		return false, currentState, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
		VALUES (?, ?, ?, ?, ?)`, strings.TrimSpace(taskID), strings.TrimSpace(kind), currentState, strings.TrimSpace(to), strings.TrimSpace(reason)); err != nil {
		return false, "", err
	}
	if err := tx.Commit(); err != nil {
		return false, "", err
	}
	return true, strings.TrimSpace(to), nil
}

func activeJobMatchingTaskTx(ctx context.Context, tx *sql.Tx, taskID string, repoFullName string, branch string) (string, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, payload FROM jobs WHERE state IN ('queued', 'running') ORDER BY id`)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	branch = strings.TrimSpace(branch)
	repoFullName = strings.TrimSpace(repoFullName)
	for rows.Next() {
		var jobID, rawPayload string
		if err := rows.Scan(&jobID, &rawPayload); err != nil {
			return "", false, err
		}
		var payload struct {
			TaskID string `json:"task_id"`
			Repo   string `json:"repo"`
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
			continue
		}
		if strings.TrimSpace(payload.TaskID) == taskID ||
			(branch != "" && strings.TrimSpace(payload.Repo) == repoFullName && strings.TrimSpace(payload.Branch) == branch) {
			return jobID, true, nil
		}
	}
	return "", false, rows.Err()
}

func scanTask(row interface{ Scan(dest ...any) error }) (Task, error) {
	var task Task
	if err := row.Scan(&task.ID, &task.RepoFullName, &task.GoalID, &task.Title, &task.State, &task.Branch, &task.WorktreePath); err != nil {
		return Task{}, err
	}
	return task, nil
}

func taskSelectSQL() string {
	return `SELECT id, repo_full_name, goal_id, title, state, branch, worktree_path`
}

func (s *Store) UpsertPullRequest(ctx context.Context, pr PullRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pull_requests(repo_full_name, number, url, head_branch, base_branch, head_sha, merge_commit_sha, state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name, number) DO UPDATE SET
			url = excluded.url,
			head_branch = excluded.head_branch,
			base_branch = excluded.base_branch,
			head_sha = excluded.head_sha,
			merge_commit_sha = excluded.merge_commit_sha,
			state = excluded.state,
			updated_at = CURRENT_TIMESTAMP`,
		pr.RepoFullName, pr.Number, pr.URL, pr.HeadBranch, pr.BaseBranch, pr.HeadSHA, pr.MergeCommitSHA, pr.State)
	return err
}

func (s *Store) GetPullRequest(ctx context.Context, repoFullName string, number int64) (PullRequest, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, number, url, head_branch, base_branch, head_sha, merge_commit_sha, state
		FROM pull_requests WHERE repo_full_name = ? AND number = ?`, repoFullName, number)
	var pr PullRequest
	if err := row.Scan(&pr.RepoFullName, &pr.Number, &pr.URL, &pr.HeadBranch, &pr.BaseBranch, &pr.HeadSHA, &pr.MergeCommitSHA, &pr.State); err != nil {
		return PullRequest{}, err
	}
	return pr, nil
}

func (s *Store) GetPullRequestByRepoBranch(ctx context.Context, repoFullName string, branch string) (PullRequest, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, number, url, head_branch, base_branch, head_sha, merge_commit_sha, state
		FROM pull_requests WHERE repo_full_name = ? AND head_branch = ? ORDER BY number DESC LIMIT 1`, repoFullName, branch)
	var pr PullRequest
	if err := row.Scan(&pr.RepoFullName, &pr.Number, &pr.URL, &pr.HeadBranch, &pr.BaseBranch, &pr.HeadSHA, &pr.MergeCommitSHA, &pr.State); err != nil {
		return PullRequest{}, err
	}
	return pr, nil
}

func (s *Store) ListPullRequests(ctx context.Context, repoFullName string) ([]PullRequest, error) {
	query := `SELECT repo_full_name, number, url, head_branch, base_branch, head_sha, merge_commit_sha, state FROM pull_requests`
	args := []any{}
	if strings.TrimSpace(repoFullName) != "" {
		query += ` WHERE repo_full_name = ?`
		args = append(args, repoFullName)
	}
	query += ` ORDER BY repo_full_name, number`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prs := []PullRequest{}
	for rows.Next() {
		var pr PullRequest
		if err := rows.Scan(&pr.RepoFullName, &pr.Number, &pr.URL, &pr.HeadBranch, &pr.BaseBranch, &pr.HeadSHA, &pr.MergeCommitSHA, &pr.State); err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

func (s *Store) MarkCommentSeen(ctx context.Context, comment Comment) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO seen_comments(repo_full_name, comment_id, pull_request, body)
		VALUES (?, ?, ?, ?)`, comment.RepoFullName, comment.CommentID, comment.PullRequest, comment.Body)
	return err
}

func (s *Store) HasCommentSeen(ctx context.Context, repoFullName string, commentID int64) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM seen_comments WHERE repo_full_name = ? AND comment_id = ?`, repoFullName, commentID).Scan(&count)
	return count > 0, err
}

func (s *Store) MarkCommentSeenIfNew(ctx context.Context, comment Comment) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO seen_comments(repo_full_name, comment_id, pull_request, body)
		VALUES (?, ?, ?, ?)`, comment.RepoFullName, comment.CommentID, comment.PullRequest, comment.Body)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// GetIssueCommentPollCursor returns the newest issue/PR comment updated_at the
// --watch-issues poller has persisted for a repo (#566). A missing row is NOT an
// error: it returns ok=false with a zero time, which the daemon treats as a
// first-ever poll and seeds a bounded window from `now` (no history backfill).
func (s *Store) GetIssueCommentPollCursor(ctx context.Context, repoFullName string) (time.Time, bool, error) {
	repoFullName = strings.TrimSpace(repoFullName)
	if repoFullName == "" {
		return time.Time{}, false, errors.New("repo full name is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT last_seen_comment_at FROM issue_comment_poll_state WHERE repo_full_name = ?`, repoFullName)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		// A malformed persisted cursor is treated as "no cursor" rather than a hard
		// error so a poison row can't wedge the poller; the next write repairs it.
		return time.Time{}, false, nil
	}
	return parsed, true, nil
}

// UpsertIssueCommentPollCursor persists the newest observed comment updated_at for
// a repo (#566). The time is stored as RFC3339Nano UTC text.
func (s *Store) UpsertIssueCommentPollCursor(ctx context.Context, repoFullName string, lastSeen time.Time) error {
	repoFullName = strings.TrimSpace(repoFullName)
	if repoFullName == "" {
		return errors.New("repo full name is required")
	}
	raw := ""
	if !lastSeen.IsZero() {
		raw = lastSeen.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO issue_comment_poll_state(repo_full_name, last_seen_comment_at, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name) DO UPDATE SET
			last_seen_comment_at = excluded.last_seen_comment_at,
			updated_at = CURRENT_TIMESTAMP`,
		repoFullName, raw)
	return err
}
