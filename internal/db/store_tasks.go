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
		if primary, err := (gitutil.NewHostClient(repo.CheckoutPath)).PrimaryWorktree(ctx); err == nil {
			repo.PrimaryCheckoutPath = primary
		}
	}
	if !force && strings.TrimSpace(repo.CheckoutPath) != "" {
		if existing, err := s.GetRepo(ctx, fullName); err == nil && shouldProtectRepoCheckout(existing, repo.CheckoutPath) {
			if linked, linkErr := (gitutil.NewHostClient(repo.CheckoutPath)).IsLinkedWorktree(ctx); linkErr == nil && linked {
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

// UpsertTaskUnlessStates applies the normal task upsert unless an existing row
// is currently in a forbidden state. The predicate lives on the conflict UPDATE so
// callers that must not resurrect a terminal task remain safe if its state
// changes after their initial read.
func (s *Store) UpsertTaskUnlessStates(ctx context.Context, task Task, forbiddenStates []string) (bool, error) {
	return s.upsertTaskUnlessStates(ctx, s.db, task, forbiddenStates)
}

// UpsertTaskUnlessStatesIfAdvanceOwned is UpsertTaskUnlessStates with live advance
// ownership asserted in the SAME transaction as the task write (#1673).
//
// An escalate_human pause moves the parent task to awaiting_human, which is an
// irreversible parent effect: it stops the tree and calls a human. The barrier that
// decided the policy cannot protect it, because the pass can stall between that
// check and this write while its lease lapses and a retry re-queues the child.
func (s *Store) UpsertTaskUnlessStatesIfAdvanceOwned(ctx context.Context, task Task, forbiddenStates []string, own AdvanceOwnership, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	live, err := advanceOwnershipLiveTx(ctx, tx, own, now)
	if err != nil {
		return false, err
	}
	if !live {
		return false, fmt.Errorf("%w: task %s write for job %s at generation %d",
			ErrAdvanceOwnershipLost, task.ID, own.OwnerJobID, own.AtGeneration)
	}
	written, err := s.upsertTaskUnlessStates(ctx, tx, task, forbiddenStates)
	if err != nil {
		return false, err
	}
	return written, tx.Commit()
}

// HumanRoundOpen names one attempt to open a human round: the round's durable
// identity, the pause target, and the round-open event that records it.
type HumanRoundOpen struct {
	JobID           string
	RoundID         string
	Kind            string
	Task            Task
	ForbiddenStates []string
	Event           JobEvent
}

// OpenHumanRound opens a human round as ONE transaction that settles three facts
// together (#1673):
//
//	SLOT    the FIRST statement takes the coordinator's only unsettled-round slot in
//	        escalation_rounds. Exclusion is the partial unique index
//	        escalation_rounds_one_unsettled, not a predicate over the caller's own
//	        identity: two concurrent openers mint DIFFERENT round ids, so an
//	        identity-scoped predicate would let both through. The slot is held until
//	        SETTLEMENT, so a claimed round whose effects have not landed still blocks
//	        a new round — a stale replay must never clear a newer round's live pause.
//	PAUSED  the guarded task transition commits WITH the requested event. If the
//	        transition is REFUSED (a merged row forbids awaiting_human, or the row
//	        became disposed after the caller's pre-read) the whole transaction rolls
//	        back, releasing the slot: a round event without its pause is a lie, and
//	        announcing an unopened round calls a human about nothing.
//	QUIET   the caller learns the outcome BEFORE any announcement, so notifier, event
//	        sink and chat link fire only for the winner that actually paused.
//
// A LOSER writes nothing at all — no round row, no event, no task write, and
// therefore no classification and no audit row. That is why a concurrent loser can
// never produce a false landed-work refusal.
func (s *Store) OpenHumanRound(ctx context.Context, round HumanRoundOpen, now time.Time) (EscalationRoundOutcome, error) {
	return s.openHumanRound(ctx, round, nil, now)
}

// OpenHumanRoundIfAdvanceOwned is OpenHumanRound with live advance ownership
// asserted in the same transaction, so a superseded pass can neither open a round
// nor announce one.
func (s *Store) OpenHumanRoundIfAdvanceOwned(ctx context.Context, round HumanRoundOpen, own AdvanceOwnership, now time.Time) (EscalationRoundOutcome, error) {
	return s.openHumanRound(ctx, round, &own, now)
}

func (s *Store) openHumanRound(ctx context.Context, round HumanRoundOpen, own *AdvanceOwnership, now time.Time) (EscalationRoundOutcome, error) {
	jobID := strings.TrimSpace(round.JobID)
	if jobID == "" {
		return EscalationRoundBlocked, errors.New("escalation round job id is required")
	}
	if strings.TrimSpace(round.RoundID) == "" {
		return EscalationRoundBlocked, errors.New("escalation round id is required")
	}
	if strings.TrimSpace(round.Event.Kind) == "" {
		return EscalationRoundBlocked, errors.New("round-open event kind is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EscalationRoundBlocked, err
	}
	defer tx.Rollback()

	took, err := insertEscalationRoundTx(ctx, tx, jobID, round.RoundID, round.Kind, now)
	if err != nil {
		return EscalationRoundBlocked, err
	}
	if !took {
		// The coordinator already has an unsettled round: idempotent, and silent.
		return EscalationRoundBlocked, tx.Commit()
	}
	if own != nil {
		live, ownErr := advanceOwnershipLiveTx(ctx, tx, *own, now)
		if ownErr != nil {
			return EscalationRoundBlocked, ownErr
		}
		if !live {
			return EscalationRoundBlocked, fmt.Errorf("%w: round-open for job %s at generation %d",
				ErrAdvanceOwnershipLost, own.OwnerJobID, own.AtGeneration)
		}
	}
	if strings.TrimSpace(round.Task.ID) != "" {
		written, werr := s.upsertTaskUnlessStates(ctx, tx, round.Task, round.ForbiddenStates)
		if werr != nil {
			return EscalationRoundBlocked, werr
		}
		if !written {
			// REFUSED: roll back the slot and the event with the pause. The caller
			// classifies the winning row; a loser never reaches this statement.
			return EscalationRoundRefused, nil
		}
	}
	event := round.Event
	if strings.TrimSpace(event.JobID) == "" {
		event.JobID = jobID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
		event.JobID, event.Kind, event.Message); err != nil {
		return EscalationRoundBlocked, err
	}
	return EscalationRoundOpened, tx.Commit()
}

// CloseHumanRound claims a round's resolution and appends its resolved event in ONE
// transaction, keyed by round identity (#1673). rows=1 on the claim UPDATE is the
// winner and the only caller allowed to run the verb's irreversible effects; a human
// resume and the TTL sweep contend on that single statement rather than on two
// independent pre-checks.
//
// It does NOT release the slot: settlement does, after the effects land.
func (s *Store) CloseHumanRound(ctx context.Context, jobID string, roundID string, verb string, generation int64, payload string, event JobEvent, now time.Time) (bool, error) {
	jobID = strings.TrimSpace(jobID)
	roundID = strings.TrimSpace(roundID)
	if jobID == "" || roundID == "" {
		return false, errors.New("round-close requires a job id and a round id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE escalation_rounds
		SET resolved_at = ?, claim_verb = ?, claim_generation = ?, claim_payload = ?
		WHERE job_id = ? AND round_id = ? AND resolved_at IS NULL AND effects_completed_at IS NULL`,
		formatResourceLockTime(now), strings.TrimSpace(verb), generation, payload, jobID, roundID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != 1 {
		return false, tx.Commit()
	}
	if strings.TrimSpace(event.JobID) == "" {
		event.JobID = jobID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
		event.JobID, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) upsertTaskUnlessStates(ctx context.Context, execer sqlExecer, task Task, forbiddenStates []string) (bool, error) {
	if len(forbiddenStates) == 0 {
		return false, errors.New("at least one forbidden task state is required")
	}
	placeholders := make([]string, 0, len(forbiddenStates))
	args := []any{task.ID, task.RepoFullName, task.GoalID, task.Title, task.State, task.Branch, task.WorktreePath}
	for _, state := range forbiddenStates {
		placeholders = append(placeholders, "?")
		args = append(args, strings.TrimSpace(state))
	}
	result, err := execer.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch, worktree_path, updated_at)
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
		WHERE tasks.state NOT IN (`+strings.Join(placeholders, ",")+`)`, args...)
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

var (
	ErrTaskStateConflict = errors.New("task state changed before guarded transition")
	ErrTaskStateClaimed  = errors.New("task state is claimed for an external merge")
)

const (
	TaskStateClaimKindExternalMerge          = "external_merge"
	TaskStateClaimKindExternalMergeUncertain = "external_merge_uncertain"
)

// BlockTaskWithEvent atomically compares the caller's observed state, persists
// the blocked task row, and appends the event that owns that block. blocked is
// true only after both writes commit.
func (s *Store) BlockTaskWithEvent(ctx context.Context, task Task, event TaskEvent) (blocked bool, err error) {
	return s.blockTaskWithEvent(ctx, task, event, nil, time.Time{})
}

// BlockTaskWithEventIfAdvanceOwned is BlockTaskWithEvent with live advance ownership
// asserted in the SAME transaction as the block (#1673).
//
// A block_parent failure policy is an irreversible parent effect: it moves the
// parent task and writes its attribution event. Checking ownership at the barrier
// that decided the policy is not enough — the pass can stall between that check and
// this write, its lease can lapse, and a retry can legally re-queue the child at
// generation N+1. Binding here means the block either commits while this pass still
// owns the advance, or does not happen at all.
func (s *Store) BlockTaskWithEventIfAdvanceOwned(ctx context.Context, task Task, event TaskEvent, own AdvanceOwnership, now time.Time) (blocked bool, err error) {
	return s.blockTaskWithEvent(ctx, task, event, &own, now)
}

func (s *Store) blockTaskWithEvent(ctx context.Context, task Task, event TaskEvent, own *AdvanceOwnership, now time.Time) (blocked bool, err error) {
	task.ID = strings.TrimSpace(task.ID)
	if task.ID == "" {
		return false, errors.New("blocked task id is required")
	}
	if strings.TrimSpace(task.State) != "blocked" {
		return false, fmt.Errorf("blocked task %s has state %q", task.ID, task.State)
	}
	event.TaskID = task.ID
	event.ToState = "blocked"

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if own != nil {
		live, ownErr := advanceOwnershipLiveTx(ctx, tx, *own, now)
		if ownErr != nil {
			return false, ownErr
		}
		if !live {
			return false, fmt.Errorf("%w: task %s block for job %s at generation %d",
				ErrAdvanceOwnershipLost, task.ID, own.OwnerJobID, own.AtGeneration)
		}
	}

	var activeClaim string
	claimErr := tx.QueryRowContext(ctx, `SELECT token FROM task_state_claims
		WHERE task_id = ? AND expires_at > CAST(strftime('%s', 'now') AS INTEGER)`, task.ID).Scan(&activeClaim)
	if claimErr == nil {
		return false, fmt.Errorf("%w: task %s", ErrTaskStateClaimed, task.ID)
	}
	if !errors.Is(claimErr, sql.ErrNoRows) {
		return false, claimErr
	}

	var currentState string
	stateErr := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, task.ID).Scan(&currentState)
	switch {
	case errors.Is(stateErr, sql.ErrNoRows):
		if strings.TrimSpace(event.FromState) != "" {
			return false, fmt.Errorf("%w: task %s no longer exists", ErrTaskStateConflict, task.ID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch, worktree_path, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
			task.ID, task.RepoFullName, task.GoalID, task.Title, task.State, task.Branch, task.WorktreePath); err != nil {
			return false, err
		}
	case stateErr != nil:
		return false, stateErr
	default:
		switch currentState {
		case "merged", "dismissed", "superseded", "stranded":
			return false, fmt.Errorf("%w: task %s is terminal in state %q",
				ErrTaskStateConflict, task.ID, currentState)
		}
		expectedState := strings.TrimSpace(event.FromState)
		if expectedState == "" || currentState != expectedState {
			return false, fmt.Errorf("%w: task %s expected %q, current %q",
				ErrTaskStateConflict, task.ID, expectedState, currentState)
		}
		preserveAge := false
		if event.Kind == "merge_gate_blocked" && expectedState == "ready_to_merge" {
			var latestKind string
			if err := tx.QueryRowContext(ctx, `SELECT kind FROM task_events
				WHERE task_id = ? ORDER BY id DESC LIMIT 1`, task.ID).Scan(&latestKind); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			} else {
				preserveAge = latestKind == "merge_gate_transient_retry"
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET
				repo_full_name = ?, goal_id = ?, title = ?, state = ?, branch = ?,
				worktree_path = CASE WHEN ? <> '' THEN ? ELSE worktree_path END,
				updated_at = CASE WHEN ? THEN updated_at ELSE CURRENT_TIMESTAMP END
			WHERE id = ? AND state = ?`,
			task.RepoFullName, task.GoalID, task.Title, task.State, task.Branch,
			task.WorktreePath, task.WorktreePath, preserveAge, task.ID, expectedState)
		if err != nil {
			return false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if affected != 1 {
			return false, fmt.Errorf("%w: task %s expected %q", ErrTaskStateConflict, task.ID, expectedState)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
		VALUES (?, ?, ?, ?, ?)`, task.ID, event.Kind, event.FromState, event.ToState, event.Reason); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
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

// RevalidateTaskState performs a single-statement compare without changing the
// task or refreshing updated_at. It is used immediately before an external side
// effect when an earlier read selected the task by identity.
func (s *Store) RevalidateTaskState(ctx context.Context, taskID string, expected string) (matched bool, currentState string, err error) {
	taskID = strings.TrimSpace(taskID)
	expected = strings.TrimSpace(expected)
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET state = state WHERE id = ? AND state = ?`, taskID, expected)
	if err != nil {
		return false, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, "", err
	}
	if affected > 0 {
		return true, expected, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&currentState); err != nil {
		return false, "", err
	}
	return false, currentState, nil
}

// HasTaskStateClaim reports whether a task has a durable external-merge claim.
// Terminal PR-group reconciliation uses it to select the one claim-owning task
// that may execute per-PR post-merge effects.
func (s *Store) HasTaskStateClaim(ctx context.Context, taskID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM task_state_claims WHERE task_id = ?`,
		strings.TrimSpace(taskID)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ClaimTaskState durably fences every state-changing SQL writer across Store
// handles and processes while an irreversible external operation is in flight.
// The schema trigger enforces the claim; the token restricts completion to its
// owner. Expired claims are reclaimed at acquisition.
func (s *Store) ClaimTaskState(ctx context.Context, taskID, expectedState, kind string, ttl time.Duration) (token string, claimed bool, currentState string, err error) {
	taskID = strings.TrimSpace(taskID)
	expectedState = strings.TrimSpace(expectedState)
	kind = strings.TrimSpace(kind)
	if taskID == "" || expectedState == "" || kind == "" {
		return "", false, "", errors.New("task claim requires task id, expected state, and kind")
	}
	ttlSeconds, err := taskStateClaimTTLSeconds(ttl)
	if err != nil {
		return "", false, "", err
	}
	token, err = newTaskStateClaimToken()
	if err != nil {
		return "", false, "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_state_claims
		WHERE task_id = ? AND kind <> ? AND expires_at <= CAST(strftime('%s', 'now') AS INTEGER)`,
		taskID, TaskStateClaimKindExternalMergeUncertain); err != nil {
		return "", false, "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&currentState); err != nil {
		return "", false, "", err
	}
	if currentState != expectedState {
		return "", false, currentState, tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_state_claims(
			task_id, kind, token, expected_state, acquired_at, expires_at
		) VALUES (?, ?, ?, ?, CAST(strftime('%s', 'now') AS INTEGER),
			CAST(strftime('%s', 'now') AS INTEGER) + ?)`,
		taskID, kind, token, expectedState, ttlSeconds)
	if err != nil {
		var existing int
		if queryErr := tx.QueryRowContext(ctx, `SELECT 1 FROM task_state_claims
			WHERE task_id = ? AND (kind = ? OR expires_at > CAST(strftime('%s', 'now') AS INTEGER))`,
			taskID, TaskStateClaimKindExternalMergeUncertain).Scan(&existing); queryErr == nil {
			return "", false, currentState, fmt.Errorf("%w: task %s", ErrTaskStateClaimed, taskID)
		}
		return "", false, currentState, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, currentState, err
	}
	return token, true, currentState, nil
}

func taskStateClaimTTLSeconds(ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		return 0, errors.New("task claim ttl must be positive")
	}
	seconds := int64(ttl / time.Second)
	if ttl%time.Second != 0 {
		seconds++
	}
	if seconds == 0 {
		seconds = 1
	}
	return seconds, nil
}

// ReleaseTaskStateClaim abandons an uncompleted external operation. The token
// prevents one claimant from releasing a later claimant's lease.
func (s *Store) ReleaseTaskStateClaim(ctx context.Context, taskID, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM task_state_claims WHERE task_id = ? AND token = ?`,
		strings.TrimSpace(taskID), strings.TrimSpace(token))
	return err
}

// RenewTaskStateClaim extends an active claim from the database clock. A claim
// that already expired or changed owners is never revived.
func (s *Store) RenewTaskStateClaim(ctx context.Context, taskID, token string, ttl time.Duration) (bool, error) {
	ttlSeconds, err := taskStateClaimTTLSeconds(ttl)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE task_state_claims
		SET expires_at = CAST(strftime('%s', 'now') AS INTEGER) + ?
		WHERE task_id = ? AND token = ? AND kind <> ?
			AND expires_at > CAST(strftime('%s', 'now') AS INTEGER)`,
		ttlSeconds, strings.TrimSpace(taskID), strings.TrimSpace(token),
		TaskStateClaimKindExternalMergeUncertain)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// RetainTaskStateClaim marks an unresolved external outcome as non-expiring.
// The state check and kind change share one SQL statement: either a writer won
// after lease expiry, or every later writer observes the retained claim.
func (s *Store) RetainTaskStateClaim(ctx context.Context, taskID, token, expectedState, retainedKind string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE task_state_claims
		SET kind = ?
		WHERE task_id = ? AND token = ? AND expected_state = ?
			AND EXISTS (SELECT 1 FROM tasks WHERE id = ? AND state = ?)`,
		strings.TrimSpace(retainedKind), strings.TrimSpace(taskID), strings.TrimSpace(token),
		strings.TrimSpace(expectedState), strings.TrimSpace(taskID), strings.TrimSpace(expectedState))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// ReleaseRetainedTaskStateClaim resolves a retained claim after the caller has
// authoritatively observed that the remote operation cannot still occur.
// The expected-state check prevents clearing ownership after a local transition.
func (s *Store) ReleaseRetainedTaskStateClaim(ctx context.Context, taskID, expectedState, retainedKind string) (released bool, currentState string, err error) {
	taskID = strings.TrimSpace(taskID)
	expectedState = strings.TrimSpace(expectedState)
	retainedKind = strings.TrimSpace(retainedKind)
	if taskID == "" || expectedState == "" || retainedKind == "" {
		return false, "", errors.New("retained task claim release requires task id, expected state, and claim kind")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&currentState); err != nil {
		return false, "", err
	}
	if currentState != expectedState {
		return false, currentState, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM task_state_claims WHERE task_id = ? AND kind = ?`,
		taskID, retainedKind)
	if err != nil {
		return false, currentState, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, currentState, err
	}
	if err := tx.Commit(); err != nil {
		return false, currentState, err
	}
	return affected == 1, currentState, nil
}

// CompleteTaskStateClaim atomically removes the caller's claim, applies the
// claimed state transition, and records its event.
func (s *Store) CompleteTaskStateClaim(ctx context.Context, taskID, token, to, kind, reason string) (changed bool, currentState string, err error) {
	taskID = strings.TrimSpace(taskID)
	token = strings.TrimSpace(token)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	var claimToken, expectedState string
	if err := tx.QueryRowContext(ctx, `SELECT token, expected_state FROM task_state_claims WHERE task_id = ?`,
		taskID).Scan(&claimToken, &expectedState); err != nil {
		return false, "", err
	}
	if claimToken != token {
		return false, "", fmt.Errorf("%w: task %s claim token changed", ErrTaskStateClaimed, taskID)
	}
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&currentState); err != nil {
		return false, "", err
	}
	if currentState != expectedState {
		return false, currentState, fmt.Errorf("%w: task %s expected %q, current %q",
			ErrTaskStateConflict, taskID, expectedState, currentState)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_state_claims WHERE task_id = ? AND token = ?`, taskID, token); err != nil {
		return false, currentState, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`,
		strings.TrimSpace(to), taskID, expectedState)
	if err != nil {
		return false, currentState, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, currentState, err
	}
	if affected != 1 {
		return false, currentState, fmt.Errorf("%w: task %s expected %q", ErrTaskStateConflict, taskID, expectedState)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
		VALUES (?, ?, ?, ?, ?)`, taskID, strings.TrimSpace(kind), expectedState, strings.TrimSpace(to), strings.TrimSpace(reason)); err != nil {
		return false, currentState, err
	}
	if err := tx.Commit(); err != nil {
		return false, currentState, err
	}
	return true, strings.TrimSpace(to), nil
}

// RecoverClaimedTaskState applies a remotely observed terminal fact after a
// claimant crashed between the external operation and local finalization. Any
// surviving claim is removed in the same transaction as the reconciled state.
func (s *Store) RecoverClaimedTaskState(ctx context.Context, taskID, to, kind, reason string) (changed bool, currentState string, err error) {
	taskID = strings.TrimSpace(taskID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	var claimToken string
	if err := tx.QueryRowContext(ctx, `SELECT token FROM task_state_claims WHERE task_id = ?`, taskID).Scan(&claimToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if stateErr := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&currentState); stateErr != nil {
				return false, "", stateErr
			}
			return false, currentState, tx.Commit()
		}
		return false, "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&currentState); err != nil {
		return false, "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_state_claims WHERE task_id = ?`, taskID); err != nil {
		return false, currentState, err
	}
	to = strings.TrimSpace(to)
	if currentState == to {
		return false, currentState, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`,
		to, taskID, currentState)
	if err != nil {
		return false, currentState, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, currentState, err
	}
	if affected != 1 {
		return false, currentState, fmt.Errorf("%w: task %s changed during claim recovery", ErrTaskStateConflict, taskID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
		VALUES (?, ?, ?, ?, ?)`, taskID, strings.TrimSpace(kind), currentState, to, strings.TrimSpace(reason)); err != nil {
		return false, currentState, err
	}
	if err := tx.Commit(); err != nil {
		return false, currentState, err
	}
	return true, to, nil
}

// TransitionTaskStateWithEvent atomically compares and moves a task state and
// appends its audit event. A failed comparison writes no event and returns the
// current state so callers can distinguish idempotence from a conflicting move.
func (s *Store) TransitionTaskStateWithEvent(ctx context.Context, taskID string, fromStates []string, to string, kind string, reason string) (changed bool, currentState string, err error) {
	changed, _, currentState, err = s.transitionTaskStateWithEvent(ctx, taskID, fromStates, to, kind, reason, false, false, time.Time{})
	return changed, currentState, err
}

// TransitionTaskStateWithEventPreserveAgeAt records a retry transition without
// refreshing the task's lifecycle age. The explicit event timestamp lets a
// daemon clock enforce retry cadence deterministically.
func (s *Store) TransitionTaskStateWithEventPreserveAgeAt(ctx context.Context, taskID string, fromStates []string, to string, kind string, reason string, at time.Time) (changed bool, currentState string, err error) {
	changed, _, currentState, err = s.transitionTaskStateWithEvent(ctx, taskID, fromStates, to, kind, reason, false, true, at)
	return changed, currentState, err
}

// TransitionTaskStateWithEventObserved is the same atomic state/event CAS as
// TransitionTaskStateWithEvent, but also returns the state the transaction
// observed before it attempted the update. Callers that need follow-up cleanup
// tied to the actual transition, rather than a stale pre-read, should use it.
func (s *Store) TransitionTaskStateWithEventObserved(ctx context.Context, taskID string, fromStates []string, to string, kind string, reason string) (changed bool, observedState string, currentState string, err error) {
	return s.transitionTaskStateWithEvent(ctx, taskID, fromStates, to, kind, reason, false, false, time.Time{})
}

// TransitionTaskStateWithEventIfNoActiveJob adds a queued/running job guard to
// the same transaction as the task CAS. Callers perform any broader liveness
// checks before entering this transaction; this guard closes the window in
// which a newly queued/running job could acquire the task.
func (s *Store) TransitionTaskStateWithEventIfNoActiveJob(ctx context.Context, taskID string, fromStates []string, to string, kind string, reason string) (changed bool, currentState string, err error) {
	changed, _, currentState, err = s.transitionTaskStateWithEvent(ctx, taskID, fromStates, to, kind, reason, true, false, time.Time{})
	return changed, currentState, err
}

// DisposeTask atomically records an evidence-based terminal task disposition,
// its audit event, and (when routable) the single durable terminal escalation.
// Notification routing never controls whether the row terminates: an empty
// escalation role or failed escalation write is recorded on the task, and the
// state transition still commits.
func (s *Store) DisposeTask(ctx context.Context, taskID string, fromStates []string, to, tier, reason, escalationRole, escalationEvent string, at time.Time) (bool, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()

	var current string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, strings.TrimSpace(taskID)).Scan(&current); err != nil {
		return false, "", err
	}
	allowed := false
	for _, state := range fromStates {
		if current == strings.TrimSpace(state) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, current, tx.Commit()
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE tasks
		SET state = ?, disposal_tier = ?, disposal_reason = ?, disposal_at = ?,
			disposal_escalation_role = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND state = ?`,
		strings.TrimSpace(to), strings.TrimSpace(tier), strings.TrimSpace(reason), stamp,
		strings.ToLower(strings.TrimSpace(escalationRole)), strings.TrimSpace(taskID), current)
	if err != nil {
		return false, current, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, current, err
	}
	if affected != 1 {
		return false, current, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
		VALUES (?, ?, ?, ?, ?)`, strings.TrimSpace(taskID), "task_disposed_"+strings.TrimSpace(tier), current, strings.TrimSpace(to), strings.TrimSpace(reason)); err != nil {
		return false, current, err
	}
	if role := strings.ToLower(strings.TrimSpace(escalationRole)); role != "" {
		var escalationErr error
		if strings.TrimSpace(escalationEvent) == "" {
			escalationErr = errors.New("task disposal escalation event is required for a routed escalation")
		} else {
			escalationErr = insertTaskDisposalEscalationTx(ctx, tx, escalationEvent, role)
		}
		if escalationErr != nil {
			failureReason := strings.TrimSpace(reason) + "; escalation write failed: " + escalationErr.Error()
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET disposal_reason = ? WHERE id = ?`, failureReason, strings.TrimSpace(taskID)); err != nil {
				return false, current, err
			}
			if err := tx.Commit(); err != nil {
				return false, current, err
			}
			return true, strings.TrimSpace(to), escalationErr
		}
	}
	if err := tx.Commit(); err != nil {
		return false, current, err
	}
	return true, strings.TrimSpace(to), nil
}

func insertTaskDisposalEscalationTx(ctx context.Context, tx *sql.Tx, event, role string) error {
	const savepoint = "task_disposal_escalation"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("start task disposal escalation savepoint: %w", err)
	}
	if err := insertWakeOutboxTx(ctx, tx, WakeOutboxSourceEscalation, event, WakeOutboxKindEscalation, role); err != nil {
		if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rollbackErr != nil {
			return fmt.Errorf("insert task disposal escalation: %v; rollback savepoint: %w", err, rollbackErr)
		}
		if _, releaseErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); releaseErr != nil {
			return fmt.Errorf("insert task disposal escalation: %v; release savepoint: %w", err, releaseErr)
		}
		return fmt.Errorf("insert task disposal escalation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("release task disposal escalation savepoint: %w", err)
	}
	return nil
}

func (s *Store) transitionTaskStateWithEvent(ctx context.Context, taskID string, fromStates []string, to string, kind string, reason string, rejectActiveJob bool, preserveUpdatedAt bool, eventAt time.Time) (changed bool, observedState string, currentState string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", "", err
	}
	defer tx.Rollback()

	var repoFullName, branch string
	if err := tx.QueryRowContext(ctx, `SELECT state, repo_full_name, branch FROM tasks WHERE id = ?`, strings.TrimSpace(taskID)).Scan(&observedState, &repoFullName, &branch); err != nil {
		return false, "", "", err
	}
	allowed := false
	for _, state := range fromStates {
		if observedState == strings.TrimSpace(state) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, observedState, observedState, tx.Commit()
	}
	if rejectActiveJob {
		jobID, active, err := activeJobMatchingTaskTx(ctx, tx, strings.TrimSpace(taskID), repoFullName, branch)
		if err != nil {
			return false, observedState, observedState, err
		}
		if active {
			return false, observedState, observedState, fmt.Errorf("%w: %s", ErrTaskHasActiveJob, jobID)
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks
		SET state = ?, updated_at = CASE WHEN ? THEN updated_at ELSE CURRENT_TIMESTAMP END
		WHERE id = ? AND state = ?`,
		strings.TrimSpace(to), preserveUpdatedAt, strings.TrimSpace(taskID), observedState)
	if err != nil {
		return false, observedState, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, observedState, "", err
	}
	if affected == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, strings.TrimSpace(taskID)).Scan(&currentState); err != nil {
			return false, observedState, "", err
		}
		return false, observedState, currentState, tx.Commit()
	}
	if eventAt.IsZero() {
		_, err = tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
			VALUES (?, ?, ?, ?, ?)`, strings.TrimSpace(taskID), strings.TrimSpace(kind), observedState, strings.TrimSpace(to), strings.TrimSpace(reason))
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, strings.TrimSpace(taskID), strings.TrimSpace(kind), observedState, strings.TrimSpace(to), strings.TrimSpace(reason), eventAt.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		return false, observedState, "", err
	}
	if err := tx.Commit(); err != nil {
		return false, observedState, "", err
	}
	return true, observedState, strings.TrimSpace(to), nil
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
	if err := row.Scan(&task.ID, &task.RepoFullName, &task.GoalID, &task.Title, &task.State, &task.Branch, &task.WorktreePath,
		&task.UpdatedAt, &task.DisposalTier, &task.DisposalReason, &task.DisposedAt, &task.DisposalEscalationRole); err != nil {
		return Task{}, err
	}
	return task, nil
}

func taskSelectSQL() string {
	return `SELECT id, repo_full_name, goal_id, title, state, branch, worktree_path, updated_at,
		disposal_tier, disposal_reason, disposal_at, disposal_escalation_role`
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
