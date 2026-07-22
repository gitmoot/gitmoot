package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, template_id, model, effort, capabilities_json, autonomy_policy, health_status, preset_delivery, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(name) DO UPDATE SET
				role = excluded.role,
				runtime = excluded.runtime,
				runtime_ref = excluded.runtime_ref,
				repo_scope = excluded.repo_scope,
				template_id = excluded.template_id,
				model = excluded.model,
				effort = excluded.effort,
				capabilities_json = excluded.capabilities_json,
				autonomy_policy = excluded.autonomy_policy,
				health_status = excluded.health_status,
				preset_delivery = excluded.preset_delivery,
				updated_at = CURRENT_TIMESTAMP`,
		agent.Name, agent.Role, agent.Runtime, agent.RuntimeRef, agent.RepoScope, agent.TemplateID, agent.Model, agent.Effort, string(capabilities), agent.AutonomyPolicy, agent.HealthStatus, normalizePresetDeliveryStored(agent.PresetDelivery)); err != nil {
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

// UpdateAgentRuntime switches a registered agent's runtime (codex or claude),
// preserving its role, capabilities, repo scope, template, and policy. The warm
// runtime_ref is cleared because it is bound to the old runtime — the next job
// starts a fresh session for the new runtime. The old agent_instance, if any,
// idle-expires on its own.
func (s *Store) UpdateAgentRuntime(ctx context.Context, name, runtime string) error {
	runtime = strings.TrimSpace(runtime)
	if runtime != "codex" && runtime != "claude" && runtime != "kimi" {
		return fmt.Errorf("unknown runtime %q (want codex, claude, or kimi)", runtime)
	}
	row := s.db.QueryRowContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, template_id, model, effort, capabilities_json, autonomy_policy, health_status, preset_delivery
		FROM agents WHERE name = ?`, name)
	agent, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("agent %q is not registered", name)
	}
	if err != nil {
		return err
	}
	agent.Runtime = runtime
	agent.RuntimeRef = ""
	return s.UpsertAgent(ctx, agent)
}

// UpdateAgentRuntimeRef re-pins an agent's runtime_ref in place, updating only
// that column (#443). Unlike UpdateAgentRuntime — which switches runtimes and
// deliberately CLEARS runtime_ref — this is used by the self-heal path to record
// a freshly minted session id while preserving every other field. It returns an
// error if no agent row matched the name.
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

func (s *Store) GetAgent(ctx context.Context, name string) (Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, template_id, model, effort, capabilities_json, autonomy_policy, health_status, preset_delivery
		FROM agents WHERE name = ?`, name)
	agent, err := scanAgent(row)
	if err == nil {
		return agent, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Agent{}, err
	}
	instance, err := s.GetAgentInstance(ctx, name)
	if err != nil {
		return Agent{}, err
	}
	policy := strings.TrimSpace(instance.AutonomyPolicy)
	if policy == "" {
		policy = "auto"
	}
	return Agent{
		Name:           instance.Name,
		Role:           instance.Role,
		Runtime:        instance.Runtime,
		RuntimeRef:     instance.RuntimeRef,
		RepoScope:      instance.RepoFullName,
		TemplateID:     instance.TemplateID,
		Model:          instance.Model,
		Effort:         instance.Effort,
		Capabilities:   instance.Capabilities,
		AutonomyPolicy: policy,
		HealthStatus:   instance.State,
		// Ephemeral/temp-worker instances have no preset_delivery column; they
		// always deliver the full preset (#33), matching the 'full' default.
		PresetDelivery: PresetDeliveryFull,
	}, nil
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, template_id, model, effort, capabilities_json, autonomy_policy, health_status, preset_delivery
		FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM keychain_grants WHERE consumer_kind = 'agent' AND consumer_id = ?`, name); err != nil {
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

// ErrAgentHasActiveJobs is the sentinel DeleteAgentChecked wraps when it refuses
// an agent that still has queued/running jobs. Callers (e.g. the dashboard's
// bulk delete) classify "skip vs hard error" with errors.Is rather than matching
// the message text.
var ErrAgentHasActiveJobs = errors.New("agent has queued or running jobs")

// rowQuerier is the QueryRowContext shape shared by *sql.DB and *sql.Tx, so
// countActiveJobsTx can run on either a plain connection or inside a transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// countActiveJobsTx is the single source of the queued/running busy count for an
// agent. Both AgentActiveJobCount (own connection) and DeleteAgentChecked (inside
// its delete transaction) call it, so the SQL and the ('queued','running') state
// list live in exactly one place and can't drift.
func countActiveJobsTx(ctx context.Context, q rowQuerier, name string) (int, error) {
	var active int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE agent = ? AND state IN ('queued', 'running')`, name).Scan(&active); err != nil {
		return 0, err
	}
	return active, nil
}

// AgentActiveJobCount returns how many queued or running jobs reference the
// named agent. It is the restart rebind's busy pre-flight; it shares its query
// with DeleteAgentChecked via countActiveJobsTx so both refuse an agent with
// in-flight work identically (callers wrap ErrAgentHasActiveJobs to classify the
// refusal).
func (s *Store) AgentActiveJobCount(ctx context.Context, name string) (int, error) {
	return countActiveJobsTx(ctx, s.db, name)
}

// DeleteAgentChecked removes an agent (and its instances) unless queued or
// running jobs still reference it, in which case it refuses (wrapping
// ErrAgentHasActiveJobs) so in-flight work is never orphaned.
func (s *Store) DeleteAgentChecked(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("agent name is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Same query as AgentActiveJobCount, but run on tx so the check + the deletes
	// below stay in one transaction (atomic). countActiveJobsTx is the shared
	// source of the SQL/state-list.
	active, err := countActiveJobsTx(ctx, tx, name)
	if err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("agent %s has %d queued or running job(s); cancel them first: %w", name, active, ErrAgentHasActiveJobs)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_repos WHERE agent_name = ?`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keychain_grants WHERE consumer_kind = 'agent' AND consumer_id = ?`, name); err != nil {
		return err
	}
	// agent_instances are NOT deleted: their `type` column references a managed
	// agent type, not this agents.name, so deleting by name could remove another
	// type's instances. Instances are ephemeral (expiry-reaped) either way.
	result, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("agent %q not found", name)
	}
	return tx.Commit()
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
	if err != nil || count > 0 {
		return count > 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_instances WHERE name = ? AND repo_full_name = ?`, agentName, repoFullName).Scan(&count)
	return count > 0, err
}

func (s *Store) UpsertAgentInstance(ctx context.Context, instance AgentInstance) error {
	capabilities, err := json.Marshal(instance.Capabilities)
	if err != nil {
		return err
	}
	instance.CreatedAt = normalizeStoredTime(instance.CreatedAt)
	instance.LastUsedAt = normalizeStoredTime(instance.LastUsedAt)
	instance.ExpiresAt = normalizeStoredTime(instance.ExpiresAt)
	if strings.TrimSpace(instance.AutonomyPolicy) == "" {
		instance.AutonomyPolicy = "auto"
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_instances(name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, effort, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			type = excluded.type,
			runtime = excluded.runtime,
			runtime_ref = excluded.runtime_ref,
			repo_full_name = excluded.repo_full_name,
			role = excluded.role,
			template_id = excluded.template_id,
			model = excluded.model,
			effort = excluded.effort,
			capabilities_json = excluded.capabilities_json,
			autonomy_policy = excluded.autonomy_policy,
			state = excluded.state,
			last_used_at = excluded.last_used_at,
			expires_at = excluded.expires_at`,
		instance.Name, instance.Type, instance.Runtime, instance.RuntimeRef, instance.RepoFullName, instance.Role, instance.TemplateID, instance.Model, instance.Effort, string(capabilities), instance.AutonomyPolicy, instance.State, instance.CreatedAt, instance.LastUsedAt, instance.ExpiresAt)
	return err
}

func (s *Store) GetAgentInstance(ctx context.Context, name string) (AgentInstance, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, effort, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at
		FROM agent_instances WHERE name = ?`, name)
	return scanAgentInstance(row)
}

func (s *Store) FindReusableAgentInstance(ctx context.Context, typ string, repo string, autonomyPolicy string, now time.Time) (AgentInstance, bool, error) {
	if strings.TrimSpace(autonomyPolicy) == "" {
		autonomyPolicy = "auto"
	}
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, effort, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at
		FROM agent_instances
		WHERE type = ? AND repo_full_name = ? AND autonomy_policy = ? AND expires_at > ?
			AND state = 'idle'
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.agent = agent_instances.name
					AND jobs.state IN ('queued', 'running')
			)
		ORDER BY last_used_at DESC, created_at DESC
		LIMIT 1`, typ, repo, autonomyPolicy, formatResourceLockTime(now))
	instance, err := scanAgentInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentInstance{}, false, nil
	}
	if err != nil {
		return AgentInstance{}, false, err
	}
	return instance, true, nil
}

func (s *Store) CountActiveAgentInstances(ctx context.Context, typ string, autonomyPolicy string, now time.Time) (int, error) {
	if strings.TrimSpace(autonomyPolicy) == "" {
		autonomyPolicy = "auto"
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_instances
		WHERE type = ? AND autonomy_policy = ?
			AND (
				expires_at > ?
				OR EXISTS (
					SELECT 1 FROM jobs
					WHERE jobs.agent = agent_instances.name
						AND jobs.state IN ('queued', 'running')
				)
			)`, typ, autonomyPolicy, formatResourceLockTime(now)).Scan(&count)
	return count, err
}

func (s *Store) FindActiveAgentInstance(ctx context.Context, typ string, repo string, autonomyPolicy string, now time.Time) (AgentInstance, bool, error) {
	if strings.TrimSpace(autonomyPolicy) == "" {
		autonomyPolicy = "auto"
	}
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, effort, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at
		FROM agent_instances
		WHERE type = ? AND repo_full_name = ? AND autonomy_policy = ?
			AND (
				expires_at > ?
				OR EXISTS (
					SELECT 1 FROM jobs
					WHERE jobs.agent = agent_instances.name
						AND jobs.state IN ('queued', 'running')
				)
			)
		ORDER BY
			CASE WHEN expires_at > ? THEN 0 ELSE 1 END,
			last_used_at DESC,
			created_at DESC
		LIMIT 1`, typ, repo, autonomyPolicy, formatResourceLockTime(now), formatResourceLockTime(now))
	instance, err := scanAgentInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentInstance{}, false, nil
	}
	if err != nil {
		return AgentInstance{}, false, err
	}
	return instance, true, nil
}

func (s *Store) ListAgentInstances(ctx context.Context) ([]AgentInstance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, effort, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at
		FROM agent_instances ORDER BY type, repo_full_name, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	instances := []AgentInstance{}
	for rows.Next() {
		instance, err := scanAgentInstance(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	return instances, rows.Err()
}

func (s *Store) TouchAgentInstance(ctx context.Context, name string, now time.Time, idleTimeout time.Duration) error {
	result, err := s.db.ExecContext(ctx, `UPDATE agent_instances SET state = 'idle', last_used_at = ?, expires_at = ? WHERE name = ?`,
		formatResourceLockTime(now), formatResourceLockTime(now.Add(idleTimeout)), name)
	if err != nil {
		return err
	}
	return requireAffected(result, "agent instance", name)
}

func (s *Store) MarkAgentInstanceRunning(ctx context.Context, name string, now time.Time, jobTimeout time.Duration) error {
	result, err := s.db.ExecContext(ctx, `UPDATE agent_instances SET state = 'running', last_used_at = ?, expires_at = ? WHERE name = ?`,
		formatResourceLockTime(now), formatResourceLockTime(now.Add(jobTimeout)), name)
	if err != nil {
		return err
	}
	return requireAffected(result, "agent instance", name)
}

func (s *Store) DeleteAgentInstance(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM agent_instances WHERE name = ?`, strings.TrimSpace(name))
	if err != nil {
		return err
	}
	return requireAffected(result, "agent instance", name)
}

// StopAgentInstance removes a runtime session (warm agent_instance) by name. It
// refuses a session that is mid-job (state "running") so an in-flight job is
// never orphaned — the caller cancels the job first. A missing session errors.
func (s *Store) StopAgentInstance(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	instance, err := s.GetAgentInstance(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session %q not found", name)
	}
	if err != nil {
		return err
	}
	if instance.State == "running" {
		return fmt.Errorf("session %q is running a job; cancel it from Jobs first", name)
	}
	return s.DeleteAgentInstance(ctx, name)
}

func (s *Store) DeleteExpiredAgentInstances(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM agent_instances
		WHERE state = 'idle'
			AND expires_at <= ?
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.agent = agent_instances.name
					AND jobs.state IN ('queued', 'running', 'failed', 'blocked', 'cancelled')
			)`, formatResourceLockTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ReconcileOrphanedRunningInstances resets to 'idle' any agent_instance left at
// state='running' whose lease (expires_at) has already elapsed and that has NO
// active (queued/running) job (#505 gap 2). Such a row is a phantom: a daemon
// that died mid-job never ran its deferred TouchAgentInstance, so the instance is
// stuck advertising a runtime session that no longer exists and the existing
// idle-only GC never reclaims it. It never disturbs a genuinely live session: an
// in-flight job within its timeout keeps a FUTURE expires_at (set to
// now+jobTimeout by MarkAgentInstanceRunning), and the active-job guard protects
// queued/running work — so this is safe to call from any number of concurrent
// daemons. Resetting (rather than deleting) keeps the row reusable and lets the
// normal idle GC reclaim it. Returns the number of rows reconciled.
func (s *Store) ReconcileOrphanedRunningInstances(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE agent_instances
		SET state = 'idle'
		WHERE state = 'running'
			AND expires_at <= ?
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.agent = agent_instances.name
					AND jobs.state IN ('queued', 'running')
			)`, formatResourceLockTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func scanAgentInstance(row interface{ Scan(dest ...any) error }) (AgentInstance, error) {
	var instance AgentInstance
	var capabilities string
	if err := row.Scan(&instance.Name, &instance.Type, &instance.Runtime, &instance.RuntimeRef, &instance.RepoFullName, &instance.Role, &instance.TemplateID, &instance.Model, &instance.Effort, &capabilities, &instance.AutonomyPolicy, &instance.State, &instance.CreatedAt, &instance.LastUsedAt, &instance.ExpiresAt); err != nil {
		return AgentInstance{}, err
	}
	if strings.TrimSpace(instance.AutonomyPolicy) == "" {
		instance.AutonomyPolicy = "auto"
	}
	if strings.TrimSpace(capabilities) != "" {
		if err := json.Unmarshal([]byte(capabilities), &instance.Capabilities); err != nil {
			return AgentInstance{}, err
		}
	}
	return instance, nil
}

// GetHeartbeatState returns the persisted state for one (agent, name) heartbeat.
// A missing row is NOT an error: it returns ok=false with a zero state, which the
// daemon treats as "due now" (a zero next_due is in the past). This keeps the
// table off-by-default — no row exists until a heartbeat first fires.
func (s *Store) GetHeartbeatState(ctx context.Context, agent, name string) (HeartbeatState, bool, error) {
	agent = strings.TrimSpace(agent)
	name = strings.TrimSpace(name)
	if agent == "" || name == "" {
		return HeartbeatState{}, false, errors.New("heartbeat agent and name are required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT agent, name, last_run_at, next_due_at, last_job_id, last_status
		FROM heartbeat_state WHERE agent = ? AND name = ?`, agent, name)
	var (
		state            HeartbeatState
		lastRun, nextDue string
	)
	if err := row.Scan(&state.Agent, &state.Name, &lastRun, &nextDue, &state.LastJobID, &state.LastStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HeartbeatState{}, false, nil
		}
		return HeartbeatState{}, false, err
	}
	state.LastRunAt = parseHeartbeatTime(lastRun)
	state.NextDueAt = parseHeartbeatTime(nextDue)
	return state, true, nil
}

// UpsertHeartbeatState writes (or replaces) a heartbeat's full state. Times are
// stored as RFC3339Nano UTC text (a zero time becomes empty text, mirroring the
// table's DEFAULT ”).
func (s *Store) UpsertHeartbeatState(ctx context.Context, state HeartbeatState) error {
	state.Agent = strings.TrimSpace(state.Agent)
	state.Name = strings.TrimSpace(state.Name)
	if state.Agent == "" || state.Name == "" {
		return errors.New("heartbeat agent and name are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO heartbeat_state(agent, name, last_run_at, next_due_at, last_job_id, last_status)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent, name) DO UPDATE SET
			last_run_at = excluded.last_run_at,
			next_due_at = excluded.next_due_at,
			last_job_id = excluded.last_job_id,
			last_status = excluded.last_status`,
		state.Agent, state.Name,
		formatHeartbeatTime(state.LastRunAt), formatHeartbeatTime(state.NextDueAt),
		strings.TrimSpace(state.LastJobID), strings.TrimSpace(state.LastStatus))
	return err
}

func formatHeartbeatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseHeartbeatTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func scanAgent(scanner agentScanner) (Agent, error) {
	var agent Agent
	var capabilities string
	if err := scanner.Scan(&agent.Name, &agent.Role, &agent.Runtime, &agent.RuntimeRef, &agent.RepoScope, &agent.TemplateID, &agent.Model, &agent.Effort, &capabilities, &agent.AutonomyPolicy, &agent.HealthStatus, &agent.PresetDelivery); err != nil {
		return Agent{}, err
	}
	if err := json.Unmarshal([]byte(capabilities), &agent.Capabilities); err != nil {
		return Agent{}, err
	}
	return agent, nil
}
