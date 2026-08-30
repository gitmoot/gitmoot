package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PullRequestAutoFixPolicy is the durable operator decision the review
// scheduler reads before dispatching a changes-requested fix.
type PullRequestAutoFixPolicy struct {
	RepoFullName string
	PullRequest  int
	Disabled     bool
	Actor        string
	Reason       string
	CreatedAt    string
	UpdatedAt    string
}

func normalizeAutoFixPolicySubject(repo string, pullRequest int) (string, error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, "#@ \t\r\n") {
		return "", errors.New("auto-fix policy repo must be owner/repo")
	}
	if pullRequest <= 0 {
		return "", errors.New("auto-fix policy pull request must be positive")
	}
	return repo, nil
}

// SetPullRequestAutoFixPolicy records an explicit disable or re-enable decision.
// Rows are retained for both states so the latest operator decision remains
// auditable instead of disappearing when auto-fix is re-enabled.
func (s *Store) SetPullRequestAutoFixPolicy(ctx context.Context, repo string, pullRequest int, disabled bool, actor string, reason string) error {
	repo, err := normalizeAutoFixPolicySubject(repo, pullRequest)
	if err != nil {
		return err
	}
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if actor == "" {
		return errors.New("auto-fix policy actor is required")
	}
	if reason == "" {
		return errors.New("auto-fix policy reason is required")
	}
	disabledValue := 0
	if disabled {
		disabledValue = 1
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO pull_request_auto_fix_policies(repo_full_name, pull_request, disabled, actor, reason)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(repo_full_name, pull_request) DO UPDATE SET
	disabled = excluded.disabled,
	actor = excluded.actor,
	reason = excluded.reason,
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`, repo, pullRequest, disabledValue, actor, reason)
	if err != nil {
		return fmt.Errorf("set pull request auto-fix policy: %w", err)
	}
	return nil
}

// PullRequestAutoFixPolicyFor returns the latest explicit policy. configured is
// false when no operator has recorded a decision for this PR.
func (s *Store) PullRequestAutoFixPolicyFor(ctx context.Context, repo string, pullRequest int) (policy PullRequestAutoFixPolicy, configured bool, err error) {
	repo, err = normalizeAutoFixPolicySubject(repo, pullRequest)
	if err != nil {
		return PullRequestAutoFixPolicy{}, false, err
	}
	var disabled int
	err = s.db.QueryRowContext(ctx, `
SELECT repo_full_name, pull_request, disabled, actor, reason, created_at, updated_at
FROM pull_request_auto_fix_policies
WHERE repo_full_name = ? AND pull_request = ?`, repo, pullRequest).Scan(
		&policy.RepoFullName,
		&policy.PullRequest,
		&disabled,
		&policy.Actor,
		&policy.Reason,
		&policy.CreatedAt,
		&policy.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PullRequestAutoFixPolicy{}, false, nil
		}
		return PullRequestAutoFixPolicy{}, false, fmt.Errorf("get pull request auto-fix policy: %w", err)
	}
	policy.Disabled = disabled != 0
	return policy, true, nil
}
