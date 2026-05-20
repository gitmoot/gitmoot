package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
}

type Agent struct {
	Name           string
	Role           string
	Runtime        string
	RuntimeRef     string
	RepoScope      string
	Capabilities   []string
	AutonomyPolicy string
	HealthStatus   string
}

type Goal struct {
	ID     string
	Title  string
	Source string
	Status string
}

type Task struct {
	ID     string
	GoalID string
	Title  string
	State  string
	Branch string
}

type PullRequest struct {
	RepoFullName string
	Number       int64
	URL          string
	HeadBranch   string
	BaseBranch   string
	HeadSHA      string
	State        string
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

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	store := &Store{db: db}
	if err := store.Migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO repos(owner, name, full_name, default_branch, remote_url, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(full_name) DO UPDATE SET
			default_branch = excluded.default_branch,
			remote_url = excluded.remote_url,
			updated_at = CURRENT_TIMESTAMP`,
		repo.Owner, repo.Name, fullName, repo.DefaultBranch, repo.RemoteURL)
	return err
}

func (s *Store) UpsertAgent(ctx context.Context, agent Agent) error {
	capabilities, err := json.Marshal(agent.Capabilities)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, capabilities_json, autonomy_policy, health_status, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET
			role = excluded.role,
			runtime = excluded.runtime,
			runtime_ref = excluded.runtime_ref,
			repo_scope = excluded.repo_scope,
			capabilities_json = excluded.capabilities_json,
			autonomy_policy = excluded.autonomy_policy,
			health_status = excluded.health_status,
			updated_at = CURRENT_TIMESTAMP`,
		agent.Name, agent.Role, agent.Runtime, agent.RuntimeRef, agent.RepoScope, string(capabilities), agent.AutonomyPolicy, agent.HealthStatus)
	return err
}

func (s *Store) GetAgent(ctx context.Context, name string) (Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, capabilities_json, autonomy_policy, health_status
		FROM agents WHERE name = ?`, name)
	return scanAgent(row)
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, capabilities_json, autonomy_policy, health_status
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
	result, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Store) InsertGoal(ctx context.Context, goal Goal) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO goals(id, title, source, status) VALUES (?, ?, ?, ?)`, goal.ID, goal.Title, goal.Source, goal.Status)
	return err
}

func (s *Store) UpsertTask(ctx context.Context, task Task) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id, goal_id, title, state, branch, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			goal_id = excluded.goal_id,
			title = excluded.title,
			state = excluded.state,
			branch = excluded.branch,
			updated_at = CURRENT_TIMESTAMP`,
		task.ID, task.GoalID, task.Title, task.State, task.Branch)
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, goal_id, title, state, branch FROM tasks WHERE id = ?`, id)
	var task Task
	if err := row.Scan(&task.ID, &task.GoalID, &task.Title, &task.State, &task.Branch); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) GetTaskByBranch(ctx context.Context, branch string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, goal_id, title, state, branch
		FROM tasks WHERE branch = ? ORDER BY updated_at DESC, id LIMIT 1`, branch)
	var task Task
	if err := row.Scan(&task.ID, &task.GoalID, &task.Title, &task.State, &task.Branch); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) UpsertPullRequest(ctx context.Context, pr PullRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pull_requests(repo_full_name, number, url, head_branch, base_branch, head_sha, state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name, number) DO UPDATE SET
			url = excluded.url,
			head_branch = excluded.head_branch,
			base_branch = excluded.base_branch,
			head_sha = excluded.head_sha,
			state = excluded.state,
			updated_at = CURRENT_TIMESTAMP`,
		pr.RepoFullName, pr.Number, pr.URL, pr.HeadBranch, pr.BaseBranch, pr.HeadSHA, pr.State)
	return err
}

func (s *Store) GetPullRequest(ctx context.Context, repoFullName string, number int64) (PullRequest, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, number, url, head_branch, base_branch, head_sha, state
		FROM pull_requests WHERE repo_full_name = ? AND number = ?`, repoFullName, number)
	var pr PullRequest
	if err := row.Scan(&pr.RepoFullName, &pr.Number, &pr.URL, &pr.HeadBranch, &pr.BaseBranch, &pr.HeadSHA, &pr.State); err != nil {
		return PullRequest{}, err
	}
	return pr, nil
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

func (s *Store) TransitionJobStatePayloadWithEvent(ctx context.Context, id string, from string, to string, payload string, event JobEvent) (bool, error) {
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

func (s *Store) AcquireLock(ctx context.Context, lock BranchLock) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO branch_locks(repo_full_name, branch, owner, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, lock.RepoFullName, lock.Branch, lock.Owner)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 1 {
		return affected == 1, err
	}

	var owner string
	err = s.db.QueryRowContext(ctx, `SELECT owner FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, lock.RepoFullName, lock.Branch).Scan(&owner)
	if err != nil {
		return false, err
	}
	return owner == lock.Owner, nil
}

func (s *Store) GetBranchLock(ctx context.Context, repoFullName string, branch string) (BranchLock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, branch, owner FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, repoFullName, branch)
	var lock BranchLock
	if err := row.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner); err != nil {
		return BranchLock{}, err
	}
	return lock, nil
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
	if err := scanner.Scan(&agent.Name, &agent.Role, &agent.Runtime, &agent.RuntimeRef, &agent.RepoScope, &capabilities, &agent.AutonomyPolicy, &agent.HealthStatus); err != nil {
		return Agent{}, err
	}
	if err := json.Unmarshal([]byte(capabilities), &agent.Capabilities); err != nil {
		return Agent{}, err
	}
	return agent, nil
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
}
