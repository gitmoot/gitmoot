package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Repo struct {
	Owner         string
	Name          string
	DefaultBranch string
	RemoteURL     string
	CheckoutPath  string
	Enabled       bool
	PollInterval  string
	LastPollAt    string
	LastError     string
}

type Agent struct {
	Name           string
	Role           string
	Runtime        string
	RuntimeRef     string
	RepoScope      string
	TemplateID     string
	Capabilities   []string
	AutonomyPolicy string
	HealthStatus   string
}

type AgentTemplate struct {
	ID             string
	Name           string
	Description    string
	SourceRepo     string
	SourceRef      string
	SourcePath     string
	ResolvedCommit string
	Content        string
	MetadataJSON   string
	CreatedAt      string
	UpdatedAt      string
}

type AgentRepo struct {
	AgentName    string
	RepoFullName string
}

type AgentInstance struct {
	Name         string
	Type         string
	Runtime      string
	RuntimeRef   string
	RepoFullName string
	Role         string
	TemplateID   string
	Capabilities []string
	State        string
	CreatedAt    string
	LastUsedAt   string
	ExpiresAt    string
}

type Goal struct {
	ID     string
	Title  string
	Source string
	Status string
}

type Task struct {
	ID           string
	RepoFullName string
	GoalID       string
	Title        string
	State        string
	Branch       string
}

type PullRequest struct {
	RepoFullName   string
	Number         int64
	URL            string
	HeadBranch     string
	BaseBranch     string
	HeadSHA        string
	MergeCommitSHA string
	State          string
}

type Comment struct {
	RepoFullName string
	CommentID    int64
	PullRequest  int64
	Body         string
}

type Job struct {
	ID      string
	Agent   string
	Type    string
	State   string
	Payload string
}

type JobEvent struct {
	JobID   string
	Kind    string
	Message string
}

type BranchLock struct {
	RepoFullName string
	Branch       string
	Owner        string
}

type BranchLockEvent struct {
	RepoFullName string
	Branch       string
	Owner        string
	Kind         string
	Message      string
}

type ResourceLock struct {
	ResourceKey string
	OwnerJobID  string
	OwnerToken  string
	AcquiredAt  string
	UpdatedAt   string
	ExpiresAt   string
}

type MergeGate struct {
	RepoFullName string
	PullRequest  int64
	State        string
	Reason       string
}

type Pinger interface {
	Close() error
	Ping(ctx context.Context) error
}

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db}
	if err := store.Migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func OpenReadOnly(path string) (*Store, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Migrate(ctx context.Context) error {
	for version, migration := range migrations {
		if err := s.applyMigration(ctx, version+1, migration); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, version int, migration string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, migration); err != nil {
		return fmt.Errorf("apply migration %d: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, version, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpsertRepo(ctx context.Context, repo Repo) error {
	fullName := repo.Owner + "/" + repo.Name
	updatePollInterval := repo.PollInterval
	insertPollInterval := repo.PollInterval
	if strings.TrimSpace(insertPollInterval) == "" {
		insertPollInterval = "30s"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO repos(owner, name, full_name, default_branch, remote_url, checkout_path, enabled, poll_interval, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(full_name) DO UPDATE SET
			default_branch = CASE WHEN excluded.default_branch <> '' THEN excluded.default_branch ELSE repos.default_branch END,
			remote_url = CASE WHEN excluded.remote_url <> '' THEN excluded.remote_url ELSE repos.remote_url END,
			checkout_path = CASE WHEN excluded.checkout_path <> '' THEN excluded.checkout_path ELSE repos.checkout_path END,
			poll_interval = CASE WHEN ? <> '' THEN excluded.poll_interval ELSE repos.poll_interval END,
			updated_at = CURRENT_TIMESTAMP`,
		repo.Owner, repo.Name, fullName, repo.DefaultBranch, repo.RemoteURL, repo.CheckoutPath, insertPollInterval, updatePollInterval)
	return err
}

func (s *Store) GetRepo(ctx context.Context, fullName string) (Repo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT owner, name, default_branch, remote_url, checkout_path, enabled, poll_interval, last_poll_at, last_error
		FROM repos WHERE full_name = ?`, fullName)
	return scanRepo(row)
}

func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner, name, default_branch, remote_url, checkout_path, enabled, poll_interval, last_poll_at, last_error
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
	if err := row.Scan(&repo.Owner, &repo.Name, &repo.DefaultBranch, &repo.RemoteURL, &repo.CheckoutPath, &enabled, &repo.PollInterval, &repo.LastPollAt, &repo.LastError); err != nil {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, template_id, capabilities_json, autonomy_policy, health_status, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(name) DO UPDATE SET
				role = excluded.role,
				runtime = excluded.runtime,
				runtime_ref = excluded.runtime_ref,
				repo_scope = excluded.repo_scope,
				template_id = excluded.template_id,
				capabilities_json = excluded.capabilities_json,
				autonomy_policy = excluded.autonomy_policy,
				health_status = excluded.health_status,
				updated_at = CURRENT_TIMESTAMP`,
		agent.Name, agent.Role, agent.Runtime, agent.RuntimeRef, agent.RepoScope, agent.TemplateID, string(capabilities), agent.AutonomyPolicy, agent.HealthStatus); err != nil {
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

func (s *Store) GetAgent(ctx context.Context, name string) (Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, template_id, capabilities_json, autonomy_policy, health_status
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
	return Agent{
		Name:           instance.Name,
		Role:           instance.Role,
		Runtime:        instance.Runtime,
		RuntimeRef:     instance.RuntimeRef,
		RepoScope:      instance.RepoFullName,
		TemplateID:     instance.TemplateID,
		Capabilities:   instance.Capabilities,
		AutonomyPolicy: "auto",
		HealthStatus:   instance.State,
	}, nil
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, template_id, capabilities_json, autonomy_policy, health_status
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

func (s *Store) UpsertAgentTemplate(ctx context.Context, template AgentTemplate) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_templates(id, name, description, source_repo, source_ref, source_path, resolved_commit, content, metadata_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			source_repo = excluded.source_repo,
			source_ref = excluded.source_ref,
			source_path = excluded.source_path,
			resolved_commit = excluded.resolved_commit,
			content = excluded.content,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		template.ID, template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, template.Content, template.MetadataJSON)
	return err
}

func (s *Store) GetAgentTemplate(ctx context.Context, id string) (AgentTemplate, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, description, source_repo, source_ref, source_path, resolved_commit, content, metadata_json, created_at, updated_at
		FROM agent_templates WHERE id = ?`, id)
	return scanAgentTemplate(row)
}

func (s *Store) ListAgentTemplates(ctx context.Context) ([]AgentTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, description, source_repo, source_ref, source_path, resolved_commit, content, metadata_json, created_at, updated_at
		FROM agent_templates ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	templates := []AgentTemplate{}
	for rows.Next() {
		template, err := scanAgentTemplate(rows)
		if err != nil {
			return nil, err
		}
		templates = append(templates, template)
	}
	return templates, rows.Err()
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_instances(name, type, runtime, runtime_ref, repo_full_name, role, template_id, capabilities_json, state, created_at, last_used_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			type = excluded.type,
			runtime = excluded.runtime,
			runtime_ref = excluded.runtime_ref,
			repo_full_name = excluded.repo_full_name,
			role = excluded.role,
			template_id = excluded.template_id,
			capabilities_json = excluded.capabilities_json,
			state = excluded.state,
			last_used_at = excluded.last_used_at,
			expires_at = excluded.expires_at`,
		instance.Name, instance.Type, instance.Runtime, instance.RuntimeRef, instance.RepoFullName, instance.Role, instance.TemplateID, string(capabilities), instance.State, instance.CreatedAt, instance.LastUsedAt, instance.ExpiresAt)
	return err
}

func (s *Store) GetAgentInstance(ctx context.Context, name string) (AgentInstance, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, capabilities_json, state, created_at, last_used_at, expires_at
		FROM agent_instances WHERE name = ?`, name)
	return scanAgentInstance(row)
}

func (s *Store) FindReusableAgentInstance(ctx context.Context, typ string, repo string, now time.Time) (AgentInstance, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, capabilities_json, state, created_at, last_used_at, expires_at
		FROM agent_instances
		WHERE type = ? AND repo_full_name = ? AND expires_at > ?
			AND state = 'idle'
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.agent = agent_instances.name
					AND jobs.state IN ('queued', 'running')
			)
		ORDER BY last_used_at DESC, created_at DESC
		LIMIT 1`, typ, repo, formatResourceLockTime(now))
	instance, err := scanAgentInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentInstance{}, false, nil
	}
	if err != nil {
		return AgentInstance{}, false, err
	}
	return instance, true, nil
}

func (s *Store) CountActiveAgentInstances(ctx context.Context, typ string, now time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_instances
		WHERE type = ?
			AND (
				expires_at > ?
				OR EXISTS (
					SELECT 1 FROM jobs
					WHERE jobs.agent = agent_instances.name
						AND jobs.state IN ('queued', 'running')
				)
			)`, typ, formatResourceLockTime(now)).Scan(&count)
	return count, err
}

func (s *Store) FindActiveAgentInstance(ctx context.Context, typ string, repo string, now time.Time) (AgentInstance, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, capabilities_json, state, created_at, last_used_at, expires_at
		FROM agent_instances
		WHERE type = ? AND repo_full_name = ?
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
		LIMIT 1`, typ, repo, formatResourceLockTime(now), formatResourceLockTime(now))
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
	rows, err := s.db.QueryContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, capabilities_json, state, created_at, last_used_at, expires_at
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

func scanAgentInstance(row interface{ Scan(dest ...any) error }) (AgentInstance, error) {
	var instance AgentInstance
	var capabilities string
	if err := row.Scan(&instance.Name, &instance.Type, &instance.Runtime, &instance.RuntimeRef, &instance.RepoFullName, &instance.Role, &instance.TemplateID, &capabilities, &instance.State, &instance.CreatedAt, &instance.LastUsedAt, &instance.ExpiresAt); err != nil {
		return AgentInstance{}, err
	}
	if strings.TrimSpace(capabilities) != "" {
		if err := json.Unmarshal([]byte(capabilities), &instance.Capabilities); err != nil {
			return AgentInstance{}, err
		}
	}
	return instance, nil
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

func upsertTask(ctx context.Context, execer sqlExecer, task Task) error {
	_, err := execer.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			repo_full_name = excluded.repo_full_name,
			goal_id = excluded.goal_id,
			title = excluded.title,
			state = excluded.state,
			branch = excluded.branch,
			updated_at = CURRENT_TIMESTAMP`,
		task.ID, task.RepoFullName, task.GoalID, task.Title, task.State, task.Branch)
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
	_, err := execer.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET
				repo_full_name = CASE
					WHEN excluded.repo_full_name <> '' THEN excluded.repo_full_name
					ELSE tasks.repo_full_name
				END,
				goal_id = excluded.goal_id,
				title = excluded.title,
				state = tasks.state,
			branch = tasks.branch,
			updated_at = CURRENT_TIMESTAMP`,
		task.ID, task.RepoFullName, task.GoalID, task.Title, task.State, task.Branch)
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, repo_full_name, goal_id, title, state, branch FROM tasks WHERE id = ?`, id)
	var task Task
	if err := row.Scan(&task.ID, &task.RepoFullName, &task.GoalID, &task.Title, &task.State, &task.Branch); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) GetTaskByBranch(ctx context.Context, branch string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, repo_full_name, goal_id, title, state, branch
		FROM tasks WHERE branch = ? ORDER BY updated_at DESC, id LIMIT 1`, branch)
	return scanTask(row)
}

func (s *Store) GetTaskByRepoBranch(ctx context.Context, repoFullName string, branch string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, repo_full_name, goal_id, title, state, branch
		FROM tasks
		WHERE branch = ? AND (repo_full_name = ? OR repo_full_name = '')
		ORDER BY CASE WHEN repo_full_name = ? THEN 0 ELSE 1 END, updated_at DESC, id
		LIMIT 1`, branch, repoFullName, repoFullName)
	return scanTask(row)
}

func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_full_name, goal_id, title, state, branch FROM tasks ORDER BY id`)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_full_name, goal_id, title, state, branch
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_full_name, goal_id, title, state, branch
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

func scanTask(row interface{ Scan(dest ...any) error }) (Task, error) {
	var task Task
	if err := row.Scan(&task.ID, &task.RepoFullName, &task.GoalID, &task.Title, &task.State, &task.Branch); err != nil {
		return Task{}, err
	}
	return task, nil
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

func (s *Store) CreateJob(ctx context.Context, job Job) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`, job.ID, job.Agent, job.Type, job.State, job.Payload)
	return err
}

func (s *Store) CreateJobWithEvent(ctx context.Context, job Job, event JobEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`, job.ID, job.Agent, job.Type, job.State, job.Payload); err != nil {
		return err
	}
	if event.JobID == "" {
		event.JobID = job.ID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, agent, type, state, payload FROM jobs WHERE id = ?`, id)
	var job Job
	if err := row.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListQueuedJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload
		FROM jobs WHERE state = ? ORDER BY created_at, rowid`, "queued")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListRunningJobsUpdatedBefore(ctx context.Context, before time.Time) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload
		FROM jobs WHERE state = ? AND updated_at < ? ORDER BY id`, "running", before.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) UpdateJobState(ctx context.Context, id string, state string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, state, id)
	if err != nil {
		return err
	}
	return requireAffected(result, "job", id)
}

func (s *Store) TransitionJobState(ctx context.Context, id string, from string, to string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`, to, id, from)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) TransitionJobStateWithEvent(ctx context.Context, id string, from string, to string, event JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`, to, id, from)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, tx.Commit()
	}
	if event.JobID == "" {
		event.JobID = id
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) TransitionJobStatePayloadWithEvent(ctx context.Context, id string, from string, to string, payload string, event JobEvent, extraEvents ...JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, payload = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`, to, payload, id, from)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, tx.Commit()
	}
	if event.JobID == "" {
		event.JobID = id
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message); err != nil {
		return false, err
	}
	for _, extra := range extraEvents {
		if extra.JobID == "" {
			extra.JobID = id
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, extra.JobID, extra.Kind, extra.Message); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *Store) UpdateJobPayload(ctx context.Context, id string, payload string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET payload = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, payload, id)
	if err != nil {
		return err
	}
	return requireAffected(result, "job", id)
}

func (s *Store) AddJobEvent(ctx context.Context, event JobEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message)
	return err
}

func (s *Store) ListJobEvents(ctx context.Context, jobID string) ([]JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, kind, message FROM job_events WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []JobEvent
	for rows.Next() {
		var event JobEvent
		if err := rows.Scan(&event.JobID, &event.Kind, &event.Message); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) AcquireResourceLock(ctx context.Context, lock ResourceLock, now time.Time) (bool, error) {
	resourceKey := strings.TrimSpace(lock.ResourceKey)
	ownerJobID := strings.TrimSpace(lock.OwnerJobID)
	ownerToken := strings.TrimSpace(lock.OwnerToken)
	if resourceKey == "" {
		return false, errors.New("resource lock key is required")
	}
	if ownerJobID == "" {
		return false, errors.New("resource lock owner job id is required")
	}
	if ownerToken == "" {
		return false, errors.New("resource lock owner token is required")
	}
	expiresAt := strings.TrimSpace(lock.ExpiresAt)
	if expiresAt == "" {
		return false, errors.New("resource lock expiry is required")
	}
	parsedExpiresAt, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return false, fmt.Errorf("resource lock expiry must be RFC3339: %w", err)
	}
	expiresAt = formatResourceLockTime(parsedExpiresAt)
	nowText := formatResourceLockTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE resource_key = ?
			AND expires_at <= ?
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			)`, resourceKey, nowText); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO resource_locks(resource_key, owner_job_id, owner_token, acquired_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, resourceKey, ownerJobID, ownerToken, nowText, nowText, expiresAt)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 1 {
		return true, tx.Commit()
	}
	return false, tx.Commit()
}

func (s *Store) GetResourceLock(ctx context.Context, resourceKey string) (ResourceLock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT resource_key, owner_job_id, owner_token, acquired_at, updated_at, expires_at FROM resource_locks WHERE resource_key = ?`, resourceKey)
	var lock ResourceLock
	if err := row.Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken, &lock.AcquiredAt, &lock.UpdatedAt, &lock.ExpiresAt); err != nil {
		return ResourceLock{}, err
	}
	return lock, nil
}

func (s *Store) ReleaseResourceLock(ctx context.Context, resourceKey string, ownerJobID string, ownerToken string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks WHERE resource_key = ? AND owner_job_id = ? AND owner_token = ?`, resourceKey, ownerJobID, ownerToken)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) DeleteExpiredResourceLocks(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE expires_at <= ?
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			)`, formatResourceLockTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func formatResourceLockTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func normalizeStoredTime(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return formatResourceLockTime(parsed)
	}
	return value
}

func (s *Store) AcquireLock(ctx context.Context, lock BranchLock) (bool, error) {
	created, err := s.CreateLock(ctx, lock)
	if err != nil {
		return false, err
	}
	if created {
		return true, nil
	}

	var owner string
	err = s.db.QueryRowContext(ctx, `SELECT owner FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, lock.RepoFullName, lock.Branch).Scan(&owner)
	if err != nil {
		return false, err
	}
	return owner == lock.Owner, nil
}

func (s *Store) CreateLock(ctx context.Context, lock BranchLock) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO branch_locks(repo_full_name, branch, owner, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, lock.RepoFullName, lock.Branch, lock.Owner)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) GetBranchLock(ctx context.Context, repoFullName string, branch string) (BranchLock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, branch, owner FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, repoFullName, branch)
	var lock BranchLock
	if err := row.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner); err != nil {
		return BranchLock{}, err
	}
	return lock, nil
}

func (s *Store) ListBranchLocks(ctx context.Context, repoFullName string) ([]BranchLock, error) {
	query := `SELECT repo_full_name, branch, owner FROM branch_locks`
	args := []any{}
	if strings.TrimSpace(repoFullName) != "" {
		query += ` WHERE repo_full_name = ?`
		args = append(args, repoFullName)
	}
	query += ` ORDER BY repo_full_name, branch`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []BranchLock
	for rows.Next() {
		var lock BranchLock
		if err := rows.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner); err != nil {
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, rows.Err()
}

func (s *Store) ReleaseLock(ctx context.Context, lock BranchLock) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM branch_locks WHERE repo_full_name = ? AND branch = ? AND owner = ?`, lock.RepoFullName, lock.Branch, lock.Owner)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) ReleaseLockWithEvent(ctx context.Context, lock BranchLock, event BranchLockEvent) (bool, error) {
	return s.releaseLockWithEvent(ctx, lock, false, event)
}

func (s *Store) ForceReleaseLockWithEvent(ctx context.Context, repoFullName string, branch string, event BranchLockEvent) (BranchLock, bool, error) {
	lock, err := s.GetBranchLock(ctx, repoFullName, branch)
	if errors.Is(err, sql.ErrNoRows) {
		return BranchLock{}, false, nil
	}
	if err != nil {
		return BranchLock{}, false, err
	}
	released, err := s.releaseLockWithEvent(ctx, lock, true, event)
	if err != nil {
		return BranchLock{}, false, err
	}
	if !released {
		return BranchLock{}, false, nil
	}
	return lock, true, nil
}

func (s *Store) releaseLockWithEvent(ctx context.Context, lock BranchLock, force bool, event BranchLockEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	current := lock
	if force || strings.TrimSpace(current.Owner) == "" {
		row := tx.QueryRowContext(ctx, `SELECT repo_full_name, branch, owner FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, lock.RepoFullName, lock.Branch)
		if err := row.Scan(&current.RepoFullName, &current.Branch, &current.Owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, tx.Commit()
			}
			return false, err
		}
	}

	query := `DELETE FROM branch_locks WHERE repo_full_name = ? AND branch = ? AND owner = ?`
	args := []any{current.RepoFullName, current.Branch, current.Owner}
	if force {
		query = `DELETE FROM branch_locks WHERE repo_full_name = ? AND branch = ?`
		args = []any{current.RepoFullName, current.Branch}
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, tx.Commit()
	}

	event.RepoFullName = current.RepoFullName
	event.Branch = current.Branch
	event.Owner = current.Owner
	if strings.TrimSpace(event.Kind) == "" {
		event.Kind = "released"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lock_events(repo_full_name, branch, owner, kind, message)
		VALUES (?, ?, ?, ?, ?)`, event.RepoFullName, event.Branch, event.Owner, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) ListBranchLockEvents(ctx context.Context, repoFullName string, branch string) ([]BranchLockEvent, error) {
	query := `SELECT repo_full_name, branch, owner, kind, message FROM lock_events`
	args := []any{}
	conditions := []string{}
	if strings.TrimSpace(repoFullName) != "" {
		conditions = append(conditions, "repo_full_name = ?")
		args = append(args, repoFullName)
	}
	if strings.TrimSpace(branch) != "" {
		conditions = append(conditions, "branch = ?")
		args = append(args, branch)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY rowid`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []BranchLockEvent
	for rows.Next() {
		var event BranchLockEvent
		if err := rows.Scan(&event.RepoFullName, &event.Branch, &event.Owner, &event.Kind, &event.Message); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) UpsertMergeGate(ctx context.Context, gate MergeGate) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO merge_gates(repo_full_name, pull_request, state, reason, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name, pull_request) DO UPDATE SET
			state = excluded.state,
			reason = excluded.reason,
			updated_at = CURRENT_TIMESTAMP`,
		gate.RepoFullName, gate.PullRequest, gate.State, gate.Reason)
	return err
}

func (s *Store) GetMergeGate(ctx context.Context, repoFullName string, pullRequest int64) (MergeGate, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, pull_request, state, reason
		FROM merge_gates WHERE repo_full_name = ? AND pull_request = ?`,
		repoFullName, pullRequest)
	var gate MergeGate
	if err := row.Scan(&gate.RepoFullName, &gate.PullRequest, &gate.State, &gate.Reason); err != nil {
		return MergeGate{}, err
	}
	return gate, nil
}

func (s *Store) HasTable(ctx context.Context, name string) (bool, error) {
	if strings.ContainsAny(name, "'\"`;") {
		return false, fmt.Errorf("unsafe table name: %s", name)
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count)
	return count == 1, err
}

type agentScanner interface {
	Scan(dest ...any) error
}

func scanAgent(scanner agentScanner) (Agent, error) {
	var agent Agent
	var capabilities string
	if err := scanner.Scan(&agent.Name, &agent.Role, &agent.Runtime, &agent.RuntimeRef, &agent.RepoScope, &agent.TemplateID, &capabilities, &agent.AutonomyPolicy, &agent.HealthStatus); err != nil {
		return Agent{}, err
	}
	if err := json.Unmarshal([]byte(capabilities), &agent.Capabilities); err != nil {
		return Agent{}, err
	}
	return agent, nil
}

type agentTemplateScanner interface {
	Scan(dest ...any) error
}

func scanAgentTemplate(scanner agentTemplateScanner) (AgentTemplate, error) {
	var template AgentTemplate
	if err := scanner.Scan(&template.ID, &template.Name, &template.Description, &template.SourceRepo, &template.SourceRef, &template.SourcePath, &template.ResolvedCommit, &template.Content, &template.MetadataJSON, &template.CreatedAt, &template.UpdatedAt); err != nil {
		return AgentTemplate{}, err
	}
	return template, nil
}

func requireAffected(result sql.Result, subject string, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%s %q not found", subject, id)
	}
	return nil
}

var migrations = []string{
	`
CREATE TABLE repos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner TEXT NOT NULL,
	name TEXT NOT NULL,
	full_name TEXT NOT NULL UNIQUE,
	default_branch TEXT NOT NULL DEFAULT '',
	remote_url TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE agents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	role TEXT NOT NULL,
	runtime TEXT NOT NULL,
	runtime_ref TEXT NOT NULL,
	repo_scope TEXT NOT NULL,
	capabilities_json TEXT NOT NULL DEFAULT '[]',
	autonomy_policy TEXT NOT NULL DEFAULT 'auto',
	health_status TEXT NOT NULL DEFAULT 'unknown',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE goals (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'planned',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE tasks (
	id TEXT PRIMARY KEY,
	goal_id TEXT NOT NULL,
	title TEXT NOT NULL,
	state TEXT NOT NULL,
	branch TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE pull_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_full_name TEXT NOT NULL,
	number INTEGER NOT NULL,
	url TEXT NOT NULL,
	head_branch TEXT NOT NULL,
	base_branch TEXT NOT NULL,
	state TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(repo_full_name, number)
);

CREATE TABLE seen_comments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_full_name TEXT NOT NULL,
	comment_id INTEGER NOT NULL,
	pull_request INTEGER NOT NULL,
	body TEXT NOT NULL,
	seen_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(repo_full_name, comment_id)
);

CREATE TABLE jobs (
	id TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	type TEXT NOT NULL,
	state TEXT NOT NULL,
	payload TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE job_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	message TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE branch_locks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_full_name TEXT NOT NULL,
	branch TEXT NOT NULL,
	owner TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(repo_full_name, branch)
);

CREATE TABLE merge_gates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_full_name TEXT NOT NULL,
	pull_request INTEGER NOT NULL,
	state TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(repo_full_name, pull_request)
);
`,
	`
ALTER TABLE pull_requests ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
	`,
	`
ALTER TABLE tasks ADD COLUMN repo_full_name TEXT NOT NULL DEFAULT '';

WITH ranked_tasks AS (
	SELECT rowid AS task_rowid,
		ROW_NUMBER() OVER (PARTITION BY repo_full_name, branch ORDER BY updated_at DESC, id) AS branch_rank
	FROM tasks
	WHERE branch <> ''
)
UPDATE tasks
SET branch = ''
WHERE rowid IN (SELECT task_rowid FROM ranked_tasks WHERE branch_rank > 1);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_repo_branch_unique ON tasks(repo_full_name, branch) WHERE branch <> '';
	`,
	`
ALTER TABLE pull_requests ADD COLUMN merge_commit_sha TEXT NOT NULL DEFAULT '';
	`,
	`
ALTER TABLE repos ADD COLUMN checkout_path TEXT NOT NULL DEFAULT '';
ALTER TABLE repos ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE repos ADD COLUMN poll_interval TEXT NOT NULL DEFAULT '30s';
ALTER TABLE repos ADD COLUMN last_poll_at TEXT NOT NULL DEFAULT '';
ALTER TABLE repos ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
	`,
	`
CREATE TABLE agent_repos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	agent_name TEXT NOT NULL,
	repo_full_name TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(agent_name, repo_full_name)
);

INSERT OR IGNORE INTO agent_repos(agent_name, repo_full_name)
SELECT name, repo_scope FROM agents WHERE repo_scope <> '';
	`,
	`
CREATE TABLE IF NOT EXISTS lock_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_full_name TEXT NOT NULL,
	branch TEXT NOT NULL,
	owner TEXT NOT NULL,
	kind TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	`
CREATE TABLE presets (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	source_repo TEXT NOT NULL,
	source_ref TEXT NOT NULL,
	source_path TEXT NOT NULL,
	resolved_commit TEXT NOT NULL,
	content TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE agents ADD COLUMN preset_id TEXT NOT NULL DEFAULT '';
	`,
	`
CREATE TABLE resource_locks (
	resource_key TEXT PRIMARY KEY,
	owner_job_id TEXT NOT NULL,
	acquired_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
	`,
	`
ALTER TABLE resource_locks ADD COLUMN owner_token TEXT NOT NULL DEFAULT '';
	`,
	`
CREATE TABLE agent_instances (
	name TEXT PRIMARY KEY,
	type TEXT NOT NULL,
	runtime TEXT NOT NULL,
	runtime_ref TEXT NOT NULL,
	repo_full_name TEXT NOT NULL,
	role TEXT NOT NULL,
	preset_id TEXT NOT NULL DEFAULT '',
	capabilities_json TEXT NOT NULL DEFAULT '[]',
	state TEXT NOT NULL,
	created_at TEXT NOT NULL,
	last_used_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
	`,
	`
CREATE TABLE agent_templates (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	source_repo TEXT NOT NULL,
	source_ref TEXT NOT NULL,
	source_path TEXT NOT NULL,
	resolved_commit TEXT NOT NULL,
	content TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT OR REPLACE INTO agent_templates(id, name, description, source_repo, source_ref, source_path, resolved_commit, content, created_at, updated_at)
SELECT id, name, description, source_repo, source_ref, source_path, resolved_commit, content, created_at, updated_at
FROM presets;

DROP TABLE presets;

ALTER TABLE agents ADD COLUMN template_id TEXT NOT NULL DEFAULT '';
UPDATE agents SET template_id = preset_id WHERE template_id = '' AND preset_id <> '';

ALTER TABLE agent_instances ADD COLUMN template_id TEXT NOT NULL DEFAULT '';
UPDATE agent_instances SET template_id = preset_id WHERE template_id = '' AND preset_id <> '';
	`,
	`
ALTER TABLE agent_templates ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '';
	`,
}
