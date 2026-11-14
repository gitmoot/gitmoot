package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (s *Store) UpsertAgent(ctx context.Context, agent Agent) error {
	capabilities, err := json.Marshal(agent.Capabilities)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, model, effort, capabilities_json, autonomy_policy, health_status, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(name) DO UPDATE SET
				role = excluded.role,
				runtime = excluded.runtime,
				runtime_ref = excluded.runtime_ref,
				repo_scope = excluded.repo_scope,
				model = excluded.model,
				effort = excluded.effort,
				capabilities_json = excluded.capabilities_json,
				autonomy_policy = excluded.autonomy_policy,
				health_status = excluded.health_status,
				updated_at = CURRENT_TIMESTAMP`,
		agent.Name, agent.Role, agent.Runtime, agent.RuntimeRef, agent.RepoScope, agent.Model, agent.Effort, string(capabilities), agent.AutonomyPolicy, agent.HealthStatus); err != nil {
		return err
	}
	if strings.TrimSpace(agent.RepoScope) != "" {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_repos(agent_name, repo_full_name, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP)`, agent.Name, agent.RepoScope); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateAgentRuntimeRef re-pins an agent's runtime_ref in place, updating only
// that column (#443). It is used by the self-heal path to record a freshly
// minted session id while preserving every other field. It returns an error if
// no agent row matched the name.
func (s *Store) UpdateAgentRuntimeRef(ctx context.Context, name, ref string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE agents SET runtime_ref = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`,
		strings.TrimSpace(ref), name)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("agent %q is not registered", name)
	}
	return nil
}

// ErrAgentPolicyBarrier reports that the caller's cross-plane barrier failed,
// so the transaction was rolled back and NO plane changed.
var ErrAgentPolicyBarrier = errors.New("agent policy barrier failed")

// ErrAgentPolicyCommit reports that the barrier succeeded but the commit did
// not. The caller's plane has moved and the database has not, so the caller
// MUST compensate.
var ErrAgentPolicyCommit = errors.New("agent policy commit failed")

// UpdateAgentAutonomyPolicy writes a REGISTERED agent's autonomy policy in
// place, without re-registering it (#2132).
//
// Deliberately targeted rather than an UpsertAgent round trip: re-registration
// replaces the runtime session, and the agents this exists for have live
// sessions and hundreds of completed jobs.
//
// #2134 F1 round two: beforeCommit IS A CROSS-PLANE BARRIER.
//
// The config plane is a TOML file, so no SQL transaction can span it, and
// review showed ordering alone cannot fix that: writing config first made a
// FAILED command widen the config plane, and writing the row first made it
// widen the row - the same defect pointed either way.
//
// A held transaction is the coordinator instead. The row updates, beforeCommit
// writes the non-SQL plane, and the commit lands only if it succeeded. The
// barrier failing rolls back, so nothing moved anywhere. Only a commit failure
// after a successful barrier leaves the caller's plane ahead, and that is
// reported distinctly as ErrAgentPolicyCommit so the caller can compensate,
// rather than folded into a generic error it can only print.
func (s *Store) UpdateAgentAutonomyPolicy(ctx context.Context, name, policy string, beforeCommit func() error) error {
	name = strings.TrimSpace(name)
	policy = strings.TrimSpace(policy)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE agents SET autonomy_policy = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`, policy, name)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("agent %q is not registered", name)
	}

	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			// Rollback is the deferred call: returning here leaves every plane
			// exactly as it was, which is the whole point of the barrier.
			return fmt.Errorf("%w: %w", ErrAgentPolicyBarrier, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %w", ErrAgentPolicyCommit, err)
	}
	return nil
}

func (s *Store) GetAgent(ctx context.Context, name string) (Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, model, effort, capabilities_json, autonomy_policy, health_status
		FROM agents WHERE name = ?`, name)
	return scanAgent(row)
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	// Returns NIL for zero rows, as before #1759 (see queryList). scanAgent takes
	// the DEFINED agentScanner interface, so it needs an adapter closure.
	return queryList(ctx, s.db, `SELECT name, role, runtime, runtime_ref, repo_scope, model, effort, capabilities_json, autonomy_policy, health_status
		FROM agents ORDER BY name`, nil,
		func(row rowScanner) (Agent, error) { return scanAgent(row) })
}

func (s *Store) RemoveAgent(ctx context.Context, name string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_repos WHERE agent_name = ?`, name); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, tx.Commit()
}

// rowQuerier is the QueryRowContext shape shared by *sql.DB and *sql.Tx, so a
// helper can run on either a plain connection or inside a transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ErrAgentHasActiveJobs is wrapped into the refusal when an agent that still has
// queued or running jobs would be disturbed. Its only producer today is
// `agent restart` (cli/agent.go), and it currently has NO errors.Is consumer:
// #1753 deleted the dashboard TUI's bulk delete, which was the one classifier.
// It is retained for the wrap so a future caller can classify skip-vs-hard-error
// without re-deriving the condition from message text - but do not read the
// wrap as an exercised contract, because nothing observes it (#1787 review
// round 3, F3, which proved it with a %w -> %v mutant the whole suite ignored).
var ErrAgentHasActiveJobs = errors.New("agent has queued or running jobs")

// AgentActiveJobCount returns how many queued or running jobs reference the
// named agent. It is the busy pre-flight for `agent restart`'s rebind refusal.
// BOTH states in the list are load-bearing - a queued job is work that has not
// started, so rebinding past it orphans it - and
// TestAgentActiveJobCountCountsQueuedAndRunning pins that, the CLI-level test of
// the guard seeding only "running".
func (s *Store) AgentActiveJobCount(ctx context.Context, name string) (int, error) {
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE agent = ? AND state IN ('queued', 'running')`, name).Scan(&active); err != nil {
		return 0, err
	}
	return active, nil
}

func (s *Store) AllowAgentRepo(ctx context.Context, agentName string, repoFullName string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE agents
		SET repo_scope = CASE WHEN repo_scope = '' THEN ? ELSE repo_scope END,
			updated_at = CURRENT_TIMESTAMP
		WHERE name = ?`, repoFullName, agentName)
	if err != nil {
		return err
	}
	if err := requireAffected(result, "agent", agentName); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO agent_repos(agent_name, repo_full_name, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)`, agentName, repoFullName)
	return err
}

func (s *Store) DenyAgentRepo(ctx context.Context, agentName string, repoFullName string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM agent_repos WHERE agent_name = ? AND repo_full_name = ?`, agentName, repoFullName)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET repo_scope = '', updated_at = CURRENT_TIMESTAMP WHERE name = ? AND repo_scope = ?`, agentName, repoFullName); err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, tx.Commit()
}

func (s *Store) ReplaceAgentRepos(ctx context.Context, agentName string, repoFullNames []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	repoScope := ""
	if len(repoFullNames) > 0 {
		repoScope = repoFullNames[0]
	}
	result, err := tx.ExecContext(ctx, `UPDATE agents SET repo_scope = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`, repoScope, agentName)
	if err != nil {
		return err
	}
	if err := requireAffected(result, "agent", agentName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_repos WHERE agent_name = ?`, agentName); err != nil {
		return err
	}
	for _, repo := range repoFullNames {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_repos(agent_name, repo_full_name, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP)`, agentName, repo); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListAgentRepos(ctx context.Context, agentName string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_full_name FROM agent_repos WHERE agent_name = ? ORDER BY repo_full_name`, agentName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []string{}
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

func (s *Store) AgentCanAccessRepo(ctx context.Context, agentName string, repoFullName string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_repos WHERE agent_name = ? AND repo_full_name = ?`, agentName, repoFullName).Scan(&count)
	return count > 0, err
}

func scanAgent(scanner agentScanner) (Agent, error) {
	var agent Agent
	var capabilities string
	if err := scanner.Scan(&agent.Name, &agent.Role, &agent.Runtime, &agent.RuntimeRef, &agent.RepoScope, &agent.Model, &agent.Effort, &capabilities, &agent.AutonomyPolicy, &agent.HealthStatus); err != nil {
		return Agent{}, err
	}
	if err := json.Unmarshal([]byte(capabilities), &agent.Capabilities); err != nil {
		return Agent{}, err
	}
	return agent, nil
}
