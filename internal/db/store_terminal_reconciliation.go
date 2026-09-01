package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// BeginPullRequestTerminalReconciliation atomically pins the first canonical
// owner for an exact PR head and records every other current identity as
// settlement debt. Later callers may add identities, but cannot replace owner.
func (s *Store) BeginPullRequestTerminalReconciliation(ctx context.Context, proposed PullRequestTerminalReconciliation, taskIDs []string) (PullRequestTerminalReconciliation, error) {
	proposed.RepoFullName = strings.TrimSpace(proposed.RepoFullName)
	proposed.HeadSHA = strings.TrimSpace(proposed.HeadSHA)
	proposed.OwnerTaskID = strings.TrimSpace(proposed.OwnerTaskID)
	if proposed.RepoFullName == "" || proposed.PullRequest <= 0 || proposed.HeadSHA == "" || proposed.OwnerTaskID == "" {
		return PullRequestTerminalReconciliation{}, errors.New("terminal reconciliation requires repository, pull request, head SHA, and owner task")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRequestTerminalReconciliation{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pull_request_terminal_reconciliations(
		repo_full_name, pull_request, head_sha, owner_task_id, effects_completed, updated_at)
		VALUES (?, ?, ?, ?, 0, CURRENT_TIMESTAMP)`, proposed.RepoFullName, proposed.PullRequest, proposed.HeadSHA, proposed.OwnerTaskID); err != nil {
		return PullRequestTerminalReconciliation{}, err
	}

	var reconciliation PullRequestTerminalReconciliation
	var effectsCompleted int
	if err := tx.QueryRowContext(ctx, `SELECT repo_full_name, pull_request, head_sha, owner_task_id, effects_completed
		FROM pull_request_terminal_reconciliations
		WHERE repo_full_name = ? AND pull_request = ? AND head_sha = ?`, proposed.RepoFullName, proposed.PullRequest, proposed.HeadSHA).Scan(
		&reconciliation.RepoFullName, &reconciliation.PullRequest, &reconciliation.HeadSHA,
		&reconciliation.OwnerTaskID, &effectsCompleted); err != nil {
		return PullRequestTerminalReconciliation{}, err
	}
	reconciliation.EffectsCompleted = effectsCompleted != 0

	seen := make(map[string]struct{}, len(taskIDs))
	for _, taskID := range taskIDs {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" || taskID == reconciliation.OwnerTaskID {
			continue
		}
		if _, ok := seen[taskID]; ok {
			continue
		}
		seen[taskID] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pull_request_terminal_settlements(
			repo_full_name, pull_request, head_sha, task_id)
			VALUES (?, ?, ?, ?)`, reconciliation.RepoFullName, reconciliation.PullRequest, reconciliation.HeadSHA, taskID); err != nil {
			return PullRequestTerminalReconciliation{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PullRequestTerminalReconciliation{}, err
	}
	return reconciliation, nil
}

func (s *Store) CompletePullRequestTerminalEffects(ctx context.Context, reconciliation PullRequestTerminalReconciliation) error {
	result, err := s.db.ExecContext(ctx, `UPDATE pull_request_terminal_reconciliations
		SET effects_completed = 1, updated_at = CURRENT_TIMESTAMP
		WHERE repo_full_name = ? AND pull_request = ? AND head_sha = ? AND owner_task_id = ?`,
		reconciliation.RepoFullName, reconciliation.PullRequest, reconciliation.HeadSHA, reconciliation.OwnerTaskID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("terminal reconciliation owner %s for %s#%d at %s not found", reconciliation.OwnerTaskID, reconciliation.RepoFullName, reconciliation.PullRequest, reconciliation.HeadSHA)
	}
	return nil
}

func (s *Store) ListPullRequestTerminalSettlements(ctx context.Context, reconciliation PullRequestTerminalReconciliation) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id FROM pull_request_terminal_settlements
		WHERE repo_full_name = ? AND pull_request = ? AND head_sha = ?
		ORDER BY task_id`, reconciliation.RepoFullName, reconciliation.PullRequest, reconciliation.HeadSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			return nil, err
		}
		taskIDs = append(taskIDs, taskID)
	}
	return taskIDs, rows.Err()
}

func (s *Store) ResolvePullRequestTerminalSettlement(ctx context.Context, reconciliation PullRequestTerminalReconciliation, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pull_request_terminal_settlements
		WHERE repo_full_name = ? AND pull_request = ? AND head_sha = ? AND task_id = ?`,
		reconciliation.RepoFullName, reconciliation.PullRequest, reconciliation.HeadSHA, strings.TrimSpace(taskID))
	return err
}

func (s *Store) GetPullRequestTerminalReconciliation(ctx context.Context, repoFullName string, pullRequest int64, headSHA string) (PullRequestTerminalReconciliation, error) {
	var reconciliation PullRequestTerminalReconciliation
	var effectsCompleted int
	err := s.db.QueryRowContext(ctx, `SELECT repo_full_name, pull_request, head_sha, owner_task_id, effects_completed
		FROM pull_request_terminal_reconciliations
		WHERE repo_full_name = ? AND pull_request = ? AND head_sha = ?`, strings.TrimSpace(repoFullName), pullRequest, strings.TrimSpace(headSHA)).Scan(
		&reconciliation.RepoFullName, &reconciliation.PullRequest, &reconciliation.HeadSHA,
		&reconciliation.OwnerTaskID, &effectsCompleted)
	if err != nil {
		return PullRequestTerminalReconciliation{}, err
	}
	reconciliation.EffectsCompleted = effectsCompleted != 0
	return reconciliation, nil
}
