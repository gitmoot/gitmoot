package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	VersionID      string
	VersionNumber  int
	VersionState   string
	ContentHash    string
	CreatedAt      string
	UpdatedAt      string
}

type AgentTemplateVersion struct {
	ID             string
	TemplateID     string
	VersionNumber  int
	State          string
	Name           string
	Description    string
	SourceRepo     string
	SourceRef      string
	SourcePath     string
	ResolvedCommit string
	ContentHash    string
	Content        string
	MetadataJSON   string
	CreatedAt      string
	UpdatedAt      string
	PromotedAt     string
}

type AgentTemplateCandidateReview struct {
	VersionID           string
	TemplateID          string
	BaseVersionID       string
	DiffArtifactID      string
	Score               *float64
	PreferenceSummary   string
	EvalReportJSON      string
	SummaryMetadataJSON string
	State               string
	DecisionReason      string
	CreatedAt           string
	UpdatedAt           string
	DecidedAt           string
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

type EvalArtifact struct {
	ID        string
	Hash      string
	MediaType string
	SizeBytes int64
	Driver    string
	CreatedAt string
}

type EvalRun struct {
	ID                string
	TemplateID        string
	TemplateVersionID string
	TargetRepo        string
	State             string
	Mode              string
	ExplorationLevel  string
	OptionsCount      int
	MetadataJSON      string
	CreatedAt         string
	UpdatedAt         string
}

type SkillOptTrainSession struct {
	ID                string
	TemplateID        string
	TemplateVersionID string
	TargetRepo        string
	WorkspaceRepo     string
	PreviewRepo       string
	RequestSummary    string
	TaskKind          string
	State             string
	MetadataJSON      string
	CreatedAt         string
	UpdatedAt         string
}

type SkillOptTrainIteration struct {
	ID                    string
	SessionID             string
	EvalRunID             string
	BaseTemplateVersionID string
	CandidateVersionID    string
	Mode                  string
	ExplorationLevel      string
	State                 string
	IssueRepo             string
	IssueNumber           int64
	IssueURL              string
	PullRequestRepo       string
	PullRequestNumber     int64
	PullRequestURL        string
	DecisionReason        string
	MetadataJSON          string
	CreatedAt             string
	UpdatedAt             string
}

type SkillOptReviewWatch struct {
	Repo                  string
	IssueNumber           int64
	RunID                 string
	ExpectedItemIDsJSON   string
	Status                string
	LastSeenCommentID     int64
	LastImportErrorHash   string
	StaleAfter            string
	StaleThresholdSeconds int64
	StaleNotified         bool
	MetadataJSON          string
	CreatedAt             string
	UpdatedAt             string
}

const (
	EvalRunModeExplore  = "explore"
	EvalRunModeRefine   = "refine"
	EvalRunModeDistill  = "distill"
	EvalRunModeValidate = "validate"

	ExplorationLevelHigh   = "high"
	ExplorationLevelMedium = "medium"
	ExplorationLevelLow    = "low"

	SkillOptReviewWatchStatusWatching      = "watching"
	SkillOptReviewWatchStatusImported      = "imported"
	SkillOptReviewWatchStatusClosed        = "closed"
	SkillOptReviewWatchStatusStaleNotified = "stale_notified"
	SkillOptReviewWatchStatusFailed        = "failed"
)

type EvalReviewItem struct {
	ID                  string
	RunID               string
	ItemID              string
	Title               string
	SourceArtifactID    string
	BaselineArtifactID  string
	CandidateArtifactID string
	PreviewArtifactID   string
	DiffArtifactID      string
	MetadataJSON        string
	CreatedAt           string
	UpdatedAt           string
}

type EvalReviewOption struct {
	ID           string
	RunID        string
	ItemID       string
	Label        string
	ArtifactID   string
	Role         string
	MetadataJSON string
	CreatedAt    string
	UpdatedAt    string
}

type EvalReviewGenerationWrite struct {
	ItemID     string
	ReviewItem *EvalReviewItem
	Artifacts  []EvalArtifact
	Options    []EvalReviewOption
}

type FeedbackEvent struct {
	ID        string
	RunID     string
	ItemID    string
	Choice    string
	Reasoning string
	Reviewer  string
	Source    string
	SourceURL string
	CreatedAt string
}

type RankedFeedbackEvent struct {
	ID                       string
	RunID                    string
	ItemID                   string
	RankingJSON              string
	Winner                   string
	UsefulTraitsJSON         string
	RejectedTraitsJSON       string
	RequiredImprovementsJSON string
	Quality                  string
	ContinueMode             string
	Promote                  string
	Reasoning                string
	Reviewer                 string
	Source                   string
	SourceURL                string
	CreatedAt                string
}

type PairwisePreference struct {
	RunID         string
	ItemID        string
	Preferred     string
	Rejected      string
	RankedEventID string
	Reviewer      string
	Source        string
	SourceURL     string
	CreatedAt     string
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
	ResourceKey   string
	OwnerJobID    string
	OwnerToken    string
	OwnerPID      int64
	OwnerHostname string
	CommandHash   string
	AcquiredAt    string
	UpdatedAt     string
	ExpiresAt     string
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	contentHash := templateContentHash(template.Content)
	current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, template.ID)
	if err != nil {
		return err
	}
	versionID := current.ID
	versionNumber := current.VersionNumber
	if !hasCurrent || current.ContentHash != contentHash {
		versionNumber, err = nextAgentTemplateVersionNumber(ctx, tx, template.ID)
		if err != nil {
			return err
		}
		versionID = agentTemplateVersionID(template.ID, versionNumber)
		if hasCurrent {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'superseded', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = 'current'`, current.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_template_versions(id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, promoted_at, updated_at)
			VALUES (?, ?, ?, 'current', ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			versionID, template.ID, versionNumber, template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, contentHash, template.Content, template.MetadataJSON); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions
			SET state = 'current',
				name = ?,
				description = ?,
				source_repo = ?,
				source_ref = ?,
				source_path = ?,
				resolved_commit = ?,
				content_hash = ?,
				content = ?,
				metadata_json = ?,
				updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`,
			template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, contentHash, template.Content, template.MetadataJSON, current.ID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_templates(id, name, description, source_repo, source_ref, source_path, resolved_commit, content, metadata_json, current_version_id, latest_version_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			source_repo = excluded.source_repo,
			source_ref = excluded.source_ref,
			source_path = excluded.source_path,
			resolved_commit = excluded.resolved_commit,
			content = excluded.content,
			metadata_json = excluded.metadata_json,
			current_version_id = excluded.current_version_id,
			latest_version_id = CASE
				WHEN agent_templates.current_version_id = excluded.current_version_id AND agent_templates.latest_version_id <> '' THEN agent_templates.latest_version_id
				ELSE excluded.latest_version_id
			END,
			updated_at = CURRENT_TIMESTAMP`,
		template.ID, template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, template.Content, template.MetadataJSON, versionID, versionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetAgentTemplate(ctx context.Context, id string) (AgentTemplate, error) {
	row := s.db.QueryRowContext(ctx, `SELECT t.id, t.name, t.description, t.source_repo, t.source_ref, t.source_path, t.resolved_commit, t.content, t.metadata_json,
			COALESCE(v.id, ''), COALESCE(v.version, 0), COALESCE(v.state, ''), COALESCE(NULLIF(v.content_hash, ''), ''), t.created_at, t.updated_at
		FROM agent_templates t
		LEFT JOIN agent_template_versions v ON v.id = t.current_version_id
		WHERE t.id = ?`, id)
	return scanAgentTemplateWithVersion(row)
}

func (s *Store) ListAgentTemplates(ctx context.Context) ([]AgentTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.name, t.description, t.source_repo, t.source_ref, t.source_path, t.resolved_commit, t.content, t.metadata_json,
			COALESCE(v.id, ''), COALESCE(v.version, 0), COALESCE(v.state, ''), COALESCE(NULLIF(v.content_hash, ''), ''), t.created_at, t.updated_at
		FROM agent_templates t
		LEFT JOIN agent_template_versions v ON v.id = t.current_version_id
		ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	templates := []AgentTemplate{}
	for rows.Next() {
		template, err := scanAgentTemplateWithVersion(rows)
		if err != nil {
			return nil, err
		}
		templates = append(templates, template)
	}
	return templates, rows.Err()
}

func (s *Store) GetAgentTemplateReference(ctx context.Context, ref string) (AgentTemplate, error) {
	templateID, versionRef := SplitAgentTemplateReference(ref)
	if versionRef == "" || versionRef == "current" {
		return s.GetAgentTemplate(ctx, templateID)
	}
	if versionRef == "latest" {
		return s.GetLatestAgentTemplateVersion(ctx, templateID)
	}
	version, err := s.GetAgentTemplateVersion(ctx, templateID, versionRef)
	if err != nil {
		return AgentTemplate{}, err
	}
	return agentTemplateFromVersion(version), nil
}

func (s *Store) GetLatestAgentTemplateVersion(ctx context.Context, templateID string) (AgentTemplate, error) {
	row := s.db.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at
		FROM agent_templates t
		JOIN agent_template_versions v ON v.id = t.latest_version_id
		WHERE t.id = ?`, strings.TrimSpace(templateID))
	version, err := scanAgentTemplateVersion(row)
	if err != nil {
		return AgentTemplate{}, err
	}
	return agentTemplateFromVersion(version), nil
}

func (s *Store) GetAgentTemplateVersion(ctx context.Context, templateID string, versionRef string) (AgentTemplateVersion, error) {
	templateID = strings.TrimSpace(templateID)
	versionRef = strings.TrimSpace(versionRef)
	if strings.HasPrefix(versionRef, "v") && len(versionRef) > 1 {
		versionRef = versionRef[1:]
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
		FROM agent_template_versions
		WHERE template_id = ? AND (id = ? OR CAST(version AS TEXT) = ?)`, templateID, templateID+"@v"+versionRef, versionRef)
	return scanAgentTemplateVersion(row)
}

func (s *Store) GetAgentTemplateVersionByID(ctx context.Context, versionID string) (AgentTemplateVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
		FROM agent_template_versions WHERE id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateVersion(row)
}

func (s *Store) ListAgentTemplateVersions(ctx context.Context, templateID string) ([]AgentTemplateVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
		FROM agent_template_versions WHERE template_id = ? ORDER BY version`, templateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []AgentTemplateVersion{}
	for rows.Next() {
		version, err := scanAgentTemplateVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

func (s *Store) ListPendingAgentTemplateVersions(ctx context.Context, templateID string) ([]AgentTemplateVersion, error) {
	templateID = strings.TrimSpace(templateID)
	query := `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
		FROM agent_template_versions WHERE state = 'pending'`
	args := []any{}
	if templateID != "" {
		query += ` AND template_id = ?`
		args = append(args, templateID)
	}
	query += ` ORDER BY template_id, version`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []AgentTemplateVersion{}
	for rows.Next() {
		version, err := scanAgentTemplateVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

func (s *Store) AddPendingAgentTemplateVersion(ctx context.Context, template AgentTemplate) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	_, versionNumber, err := addPendingAgentTemplateVersionTx(ctx, tx, template)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersion(ctx, template.ID, fmt.Sprintf("v%d", versionNumber))
}

func (s *Store) AddPendingAgentTemplateCandidate(ctx context.Context, template AgentTemplate, review AgentTemplateCandidateReview, artifacts []EvalArtifact) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	versionID, versionNumber, err := addPendingAgentTemplateVersionTx(ctx, tx, template)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	for _, artifact := range artifacts {
		if err := insertEvalArtifactTx(ctx, tx, artifact); err != nil {
			return AgentTemplateVersion{}, err
		}
	}
	review.VersionID = versionID
	if strings.TrimSpace(review.TemplateID) == "" {
		review.TemplateID = template.ID
	}
	if err := insertAgentTemplateCandidateReviewTx(ctx, tx, review); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersion(ctx, template.ID, fmt.Sprintf("v%d", versionNumber))
}

func addPendingAgentTemplateVersionTx(ctx context.Context, tx *sql.Tx, template AgentTemplate) (string, int, error) {
	versionNumber, err := nextAgentTemplateVersionNumber(ctx, tx, template.ID)
	if err != nil {
		return "", 0, err
	}
	versionID := agentTemplateVersionID(template.ID, versionNumber)
	contentHash := templateContentHash(template.Content)
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_template_versions(id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, updated_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		versionID, template.ID, versionNumber, template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, contentHash, template.Content, template.MetadataJSON); err != nil {
		return "", 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET latest_version_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, versionID, template.ID)
	if err != nil {
		return "", 0, err
	}
	if err := requireAffected(result, "agent template", template.ID); err != nil {
		return "", 0, err
	}
	return versionID, versionNumber, nil
}

func (s *Store) UpsertAgentTemplateCandidateReview(ctx context.Context, review AgentTemplateCandidateReview) error {
	return upsertAgentTemplateCandidateReview(ctx, s.db, review)
}

func upsertAgentTemplateCandidateReview(ctx context.Context, execer sqlExecer, review AgentTemplateCandidateReview) error {
	if strings.TrimSpace(review.VersionID) == "" {
		return errors.New("candidate review version id is required")
	}
	if strings.TrimSpace(review.TemplateID) == "" {
		return errors.New("candidate review template id is required")
	}
	if strings.TrimSpace(review.State) == "" {
		review.State = "pending"
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO agent_template_candidate_reviews(
			version_id, template_id, base_version_id, diff_artifact_id, score, preference_summary,
			eval_report_json, summary_metadata_json, state, decision_reason, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(version_id) DO UPDATE SET
			template_id = excluded.template_id,
			base_version_id = excluded.base_version_id,
			diff_artifact_id = excluded.diff_artifact_id,
			score = excluded.score,
			preference_summary = excluded.preference_summary,
			eval_report_json = excluded.eval_report_json,
			summary_metadata_json = excluded.summary_metadata_json,
			state = excluded.state,
			decision_reason = excluded.decision_reason,
			updated_at = CURRENT_TIMESTAMP`,
		strings.TrimSpace(review.VersionID),
		strings.TrimSpace(review.TemplateID),
		strings.TrimSpace(review.BaseVersionID),
		strings.TrimSpace(review.DiffArtifactID),
		review.Score,
		strings.TrimSpace(review.PreferenceSummary),
		strings.TrimSpace(review.EvalReportJSON),
		strings.TrimSpace(review.SummaryMetadataJSON),
		strings.TrimSpace(review.State),
		strings.TrimSpace(review.DecisionReason))
	return err
}

func insertAgentTemplateCandidateReviewTx(ctx context.Context, tx *sql.Tx, review AgentTemplateCandidateReview) error {
	return upsertAgentTemplateCandidateReview(ctx, tx, review)
}

func (s *Store) GetAgentTemplateCandidateReview(ctx context.Context, versionID string) (AgentTemplateCandidateReview, error) {
	row := s.db.QueryRowContext(ctx, `SELECT version_id, template_id, base_version_id, diff_artifact_id, score, preference_summary,
			eval_report_json, summary_metadata_json, state, decision_reason, created_at, updated_at, decided_at
		FROM agent_template_candidate_reviews WHERE version_id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateCandidateReview(row)
}

func (s *Store) PromoteAgentTemplateVersion(ctx context.Context, versionID string) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, versionID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if target.State != "pending" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not pending", target.ID, target.State)
	}
	current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if hasCurrent {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'superseded', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, current.ID); err != nil {
			return AgentTemplateVersion{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'current', promoted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, target.ID); err != nil {
		return AgentTemplateVersion{}, err
	}
	latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET
			name = ?, description = ?, source_repo = ?, source_ref = ?, source_path = ?, resolved_commit = ?,
			content = ?, metadata_json = ?, current_version_id = ?, latest_version_id = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		target.Name, target.Description, target.SourceRepo, target.SourceRef, target.SourcePath, target.ResolvedCommit,
		target.Content, target.MetadataJSON, target.ID, latestID, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := upsertAgentTemplateCandidateReviewDecisionTx(ctx, tx, target, "promoted", ""); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

func (s *Store) RejectAgentTemplateVersion(ctx context.Context, versionID string, reason string) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, versionID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if target.State != "pending" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not pending", target.ID, target.State)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'rejected', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, target.ID); err != nil {
		return AgentTemplateVersion{}, err
	}
	latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET latest_version_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, latestID, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := upsertAgentTemplateCandidateReviewDecisionTx(ctx, tx, target, "rejected", reason); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

func (s *Store) DecideSkillOptTrainCandidate(ctx context.Context, session SkillOptTrainSession, iteration SkillOptTrainIteration, candidateID string, decision string) (AgentTemplateVersion, error) {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	iteration, err = normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, candidateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if target.State != "pending" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not pending", target.ID, target.State)
	}
	switch strings.TrimSpace(decision) {
	case "promoted":
		current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, target.TemplateID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		if hasCurrent {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'superseded', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, current.ID); err != nil {
				return AgentTemplateVersion{}, err
			}
		}
		stateResult, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'current', promoted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = 'pending'`, target.ID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		if err := requireAffected(stateResult, "pending agent template version", target.ID); err != nil {
			return AgentTemplateVersion{}, err
		}
		latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET
				name = ?, description = ?, source_repo = ?, source_ref = ?, source_path = ?, resolved_commit = ?,
				content = ?, metadata_json = ?, current_version_id = ?, latest_version_id = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`,
			target.Name, target.Description, target.SourceRepo, target.SourceRef, target.SourcePath, target.ResolvedCommit,
			target.Content, target.MetadataJSON, target.ID, latestID, target.TemplateID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
			return AgentTemplateVersion{}, err
		}
		if err := upsertAgentTemplateCandidateReviewDecisionTx(ctx, tx, target, "promoted", ""); err != nil {
			return AgentTemplateVersion{}, err
		}
	case "rejected":
		stateResult, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'rejected', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = 'pending'`, target.ID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		if err := requireAffected(stateResult, "pending agent template version", target.ID); err != nil {
			return AgentTemplateVersion{}, err
		}
		latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET latest_version_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, latestID, target.TemplateID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
			return AgentTemplateVersion{}, err
		}
		if err := upsertAgentTemplateCandidateReviewDecisionTx(ctx, tx, target, "rejected", iteration.DecisionReason); err != nil {
			return AgentTemplateVersion{}, err
		}
	default:
		return AgentTemplateVersion{}, fmt.Errorf("candidate decision %q is not supported", decision)
	}
	if err := upsertSkillOptTrainSessionTx(ctx, tx, session); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := upsertSkillOptTrainIterationTx(ctx, tx, iteration); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
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

func (s *Store) DeleteAgentInstance(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM agent_instances WHERE name = ?`, strings.TrimSpace(name))
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

func (s *Store) UpsertEvalArtifact(ctx context.Context, artifact EvalArtifact) error {
	artifact, err := normalizeEvalArtifact(artifact)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO eval_artifacts(id, hash, media_type, size_bytes, driver, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			hash = excluded.hash,
			media_type = excluded.media_type,
			size_bytes = excluded.size_bytes,
			driver = excluded.driver`,
		artifact.ID, artifact.Hash, artifact.MediaType, artifact.SizeBytes, artifact.Driver)
	return err
}

func upsertEvalArtifactTx(ctx context.Context, tx *sql.Tx, artifact EvalArtifact) error {
	artifact, err := normalizeEvalArtifact(artifact)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO eval_artifacts(id, hash, media_type, size_bytes, driver, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			hash = excluded.hash,
			media_type = excluded.media_type,
			size_bytes = excluded.size_bytes,
			driver = excluded.driver`,
		artifact.ID, artifact.Hash, artifact.MediaType, artifact.SizeBytes, artifact.Driver)
	return err
}

func insertEvalArtifactTx(ctx context.Context, tx *sql.Tx, artifact EvalArtifact) error {
	artifact, err := normalizeEvalArtifact(artifact)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO eval_artifacts(id, hash, media_type, size_bytes, driver, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		artifact.ID, artifact.Hash, artifact.MediaType, artifact.SizeBytes, artifact.Driver)
	return err
}

func normalizeEvalArtifact(artifact EvalArtifact) (EvalArtifact, error) {
	if strings.TrimSpace(artifact.ID) == "" {
		artifact.ID = artifact.Hash
	}
	if strings.TrimSpace(artifact.Hash) == "" {
		return EvalArtifact{}, errors.New("eval artifact hash is required")
	}
	if artifact.SizeBytes < 0 {
		return EvalArtifact{}, errors.New("eval artifact size cannot be negative")
	}
	artifact.ID = strings.TrimSpace(artifact.ID)
	artifact.Hash = strings.TrimSpace(artifact.Hash)
	artifact.MediaType = strings.TrimSpace(artifact.MediaType)
	artifact.Driver = strings.TrimSpace(artifact.Driver)
	return artifact, nil
}

func (s *Store) GetEvalArtifact(ctx context.Context, id string) (EvalArtifact, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, hash, media_type, size_bytes, driver, created_at
		FROM eval_artifacts WHERE id = ?`, id)
	return scanEvalArtifact(row)
}

func (s *Store) UpsertEvalRun(ctx context.Context, run EvalRun) error {
	run, err := normalizeEvalRun(run)
	if err != nil {
		return err
	}
	return upsertEvalRunExec(ctx, s.db, run)
}

func upsertEvalRunExec(ctx context.Context, exec sqlExecer, run EvalRun) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO eval_runs(id, template_id, template_version_id, target_repo, state, mode, exploration_level, options_count, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			template_id = excluded.template_id,
			template_version_id = excluded.template_version_id,
			target_repo = excluded.target_repo,
			state = excluded.state,
			mode = excluded.mode,
			exploration_level = excluded.exploration_level,
			options_count = excluded.options_count,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		run.ID, run.TemplateID, run.TemplateVersionID, run.TargetRepo, run.State, run.Mode, run.ExplorationLevel, run.OptionsCount, run.MetadataJSON)
	return err
}

func normalizeEvalRun(run EvalRun) (EvalRun, error) {
	run.ID = strings.TrimSpace(run.ID)
	if run.ID == "" {
		return EvalRun{}, errors.New("eval run id is required")
	}
	run.TemplateID = strings.TrimSpace(run.TemplateID)
	run.TemplateVersionID = strings.TrimSpace(run.TemplateVersionID)
	run.TargetRepo = strings.TrimSpace(run.TargetRepo)
	run.State = strings.TrimSpace(run.State)
	if run.State == "" {
		run.State = "draft"
	}
	run.Mode = strings.TrimSpace(strings.ToLower(run.Mode))
	if run.Mode == "" {
		run.Mode = EvalRunModeValidate
	}
	switch run.Mode {
	case EvalRunModeExplore, EvalRunModeRefine, EvalRunModeDistill, EvalRunModeValidate:
	default:
		return EvalRun{}, fmt.Errorf("eval run mode %q is not supported", run.Mode)
	}
	run.ExplorationLevel = strings.TrimSpace(strings.ToLower(run.ExplorationLevel))
	if run.ExplorationLevel == "" {
		switch run.Mode {
		case EvalRunModeExplore:
			run.ExplorationLevel = ExplorationLevelHigh
		case EvalRunModeRefine:
			run.ExplorationLevel = ExplorationLevelMedium
		default:
			run.ExplorationLevel = ExplorationLevelLow
		}
	}
	switch run.ExplorationLevel {
	case ExplorationLevelHigh, ExplorationLevelMedium, ExplorationLevelLow:
	default:
		return EvalRun{}, fmt.Errorf("eval run exploration level %q is not supported", run.ExplorationLevel)
	}
	if run.OptionsCount == 0 {
		if run.Mode == EvalRunModeExplore {
			run.OptionsCount = 5
		} else {
			run.OptionsCount = 2
		}
	}
	if run.OptionsCount < 2 {
		return EvalRun{}, errors.New("eval run options count must be at least 2")
	}
	run.MetadataJSON = strings.TrimSpace(run.MetadataJSON)
	return run, nil
}

func (s *Store) GetEvalRun(ctx context.Context, id string) (EvalRun, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, template_version_id, target_repo, state, mode, exploration_level, options_count, metadata_json, created_at, updated_at
		FROM eval_runs WHERE id = ?`, id)
	return scanEvalRun(row)
}

func (s *Store) UpsertSkillOptTrainSession(ctx context.Context, session SkillOptTrainSession) error {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return err
	}
	return upsertSkillOptTrainSessionExec(ctx, s.db, session)
}

func upsertSkillOptTrainSessionTx(ctx context.Context, tx *sql.Tx, session SkillOptTrainSession) error {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return err
	}
	return upsertSkillOptTrainSessionExec(ctx, tx, session)
}

func upsertSkillOptTrainSessionExec(ctx context.Context, exec sqlExecer, session SkillOptTrainSession) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO skillopt_train_sessions(
			id, template_id, template_version_id, target_repo, workspace_repo, preview_repo,
			request_summary, task_kind, state, metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			template_id = excluded.template_id,
			template_version_id = excluded.template_version_id,
			target_repo = excluded.target_repo,
			workspace_repo = excluded.workspace_repo,
			preview_repo = excluded.preview_repo,
			request_summary = excluded.request_summary,
			task_kind = excluded.task_kind,
			state = excluded.state,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		session.ID, session.TemplateID, session.TemplateVersionID, session.TargetRepo, session.WorkspaceRepo,
		session.PreviewRepo, session.RequestSummary, session.TaskKind, session.State, session.MetadataJSON)
	return err
}

func normalizeSkillOptTrainSession(session SkillOptTrainSession) (SkillOptTrainSession, error) {
	session.ID = strings.TrimSpace(session.ID)
	if session.ID == "" {
		return SkillOptTrainSession{}, errors.New("skillopt train session id is required")
	}
	session.TemplateID = strings.TrimSpace(session.TemplateID)
	if session.TemplateID == "" {
		return SkillOptTrainSession{}, errors.New("skillopt train session template id is required")
	}
	session.TemplateVersionID = strings.TrimSpace(session.TemplateVersionID)
	session.TargetRepo = strings.TrimSpace(session.TargetRepo)
	session.WorkspaceRepo = strings.TrimSpace(session.WorkspaceRepo)
	session.PreviewRepo = strings.TrimSpace(session.PreviewRepo)
	session.RequestSummary = strings.TrimSpace(session.RequestSummary)
	session.TaskKind = strings.TrimSpace(strings.ToLower(session.TaskKind))
	if session.TaskKind == "" {
		session.TaskKind = "custom"
	}
	session.State = strings.TrimSpace(session.State)
	if session.State == "" {
		session.State = "request_confirmed"
	}
	session.MetadataJSON = strings.TrimSpace(session.MetadataJSON)
	return session, nil
}

func (s *Store) GetSkillOptTrainSession(ctx context.Context, id string) (SkillOptTrainSession, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, template_version_id, target_repo, workspace_repo, preview_repo,
			request_summary, task_kind, state, metadata_json, created_at, updated_at
		FROM skillopt_train_sessions WHERE id = ?`, strings.TrimSpace(id))
	return scanSkillOptTrainSession(row)
}

func (s *Store) UpsertSkillOptTrainIteration(ctx context.Context, iteration SkillOptTrainIteration) error {
	iteration, err := normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return err
	}
	return upsertSkillOptTrainIterationExec(ctx, s.db, iteration)
}

func upsertSkillOptTrainIterationTx(ctx context.Context, tx *sql.Tx, iteration SkillOptTrainIteration) error {
	iteration, err := normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return err
	}
	return upsertSkillOptTrainIterationExec(ctx, tx, iteration)
}

func upsertSkillOptTrainIterationExec(ctx context.Context, exec sqlExecer, iteration SkillOptTrainIteration) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO skillopt_train_iterations(
			id, session_id, eval_run_id, base_template_version_id, candidate_version_id,
			mode, exploration_level, state, issue_repo, issue_number, issue_url,
			pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			session_id = excluded.session_id,
			eval_run_id = excluded.eval_run_id,
			base_template_version_id = excluded.base_template_version_id,
			candidate_version_id = excluded.candidate_version_id,
			mode = excluded.mode,
			exploration_level = excluded.exploration_level,
			state = excluded.state,
			issue_repo = excluded.issue_repo,
			issue_number = excluded.issue_number,
			issue_url = excluded.issue_url,
			pull_request_repo = excluded.pull_request_repo,
			pull_request_number = excluded.pull_request_number,
			pull_request_url = excluded.pull_request_url,
			decision_reason = excluded.decision_reason,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		iteration.ID, iteration.SessionID, iteration.EvalRunID, iteration.BaseTemplateVersionID, iteration.CandidateVersionID,
		iteration.Mode, iteration.ExplorationLevel, iteration.State, iteration.IssueRepo, iteration.IssueNumber, iteration.IssueURL,
		iteration.PullRequestRepo, iteration.PullRequestNumber, iteration.PullRequestURL, iteration.DecisionReason, iteration.MetadataJSON)
	return err
}

func (s *Store) UpsertSkillOptTrainSessionAndIteration(ctx context.Context, session SkillOptTrainSession, iteration SkillOptTrainIteration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertSkillOptTrainSessionTx(ctx, tx, session); err != nil {
		return err
	}
	if err := upsertSkillOptTrainIterationTx(ctx, tx, iteration); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpsertSkillOptTrainSessionIterationAndReviewWatch(ctx context.Context, session SkillOptTrainSession, iteration SkillOptTrainIteration, watch SkillOptReviewWatch) error {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return err
	}
	iteration, err = normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return err
	}
	watch, err = normalizeSkillOptReviewWatch(watch)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertSkillOptTrainSessionExec(ctx, tx, session); err != nil {
		return err
	}
	if err := upsertSkillOptTrainIterationExec(ctx, tx, iteration); err != nil {
		return err
	}
	if err := upsertSkillOptReviewWatchExec(ctx, tx, watch); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpsertSkillOptTrainNextIteration(ctx context.Context, session SkillOptTrainSession, iteration SkillOptTrainIteration, run EvalRun, items []EvalReviewItem) error {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return err
	}
	iteration, err = normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return err
	}
	run, err = normalizeEvalRun(run)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertSkillOptTrainSessionExec(ctx, tx, session); err != nil {
		return err
	}
	if err := upsertSkillOptTrainIterationExec(ctx, tx, iteration); err != nil {
		return err
	}
	if err := upsertEvalRunExec(ctx, tx, run); err != nil {
		return err
	}
	for _, item := range items {
		if err := upsertEvalReviewItemExec(ctx, tx, item); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func normalizeSkillOptTrainIteration(iteration SkillOptTrainIteration) (SkillOptTrainIteration, error) {
	iteration.ID = strings.TrimSpace(iteration.ID)
	if iteration.ID == "" {
		return SkillOptTrainIteration{}, errors.New("skillopt train iteration id is required")
	}
	iteration.SessionID = strings.TrimSpace(iteration.SessionID)
	if iteration.SessionID == "" {
		return SkillOptTrainIteration{}, errors.New("skillopt train iteration session id is required")
	}
	iteration.EvalRunID = strings.TrimSpace(iteration.EvalRunID)
	iteration.BaseTemplateVersionID = strings.TrimSpace(iteration.BaseTemplateVersionID)
	iteration.CandidateVersionID = strings.TrimSpace(iteration.CandidateVersionID)
	iteration.Mode = strings.TrimSpace(strings.ToLower(iteration.Mode))
	if iteration.Mode == "" {
		iteration.Mode = EvalRunModeExplore
	}
	switch iteration.Mode {
	case EvalRunModeExplore, EvalRunModeRefine, EvalRunModeDistill, EvalRunModeValidate:
	default:
		return SkillOptTrainIteration{}, fmt.Errorf("skillopt train iteration mode %q is not supported", iteration.Mode)
	}
	iteration.ExplorationLevel = strings.TrimSpace(strings.ToLower(iteration.ExplorationLevel))
	if iteration.ExplorationLevel == "" {
		switch iteration.Mode {
		case EvalRunModeExplore:
			iteration.ExplorationLevel = ExplorationLevelHigh
		case EvalRunModeRefine:
			iteration.ExplorationLevel = ExplorationLevelMedium
		default:
			iteration.ExplorationLevel = ExplorationLevelLow
		}
	}
	switch iteration.ExplorationLevel {
	case ExplorationLevelHigh, ExplorationLevelMedium, ExplorationLevelLow:
	default:
		return SkillOptTrainIteration{}, fmt.Errorf("skillopt train iteration exploration level %q is not supported", iteration.ExplorationLevel)
	}
	iteration.State = strings.TrimSpace(iteration.State)
	if iteration.State == "" {
		iteration.State = "request_confirmed"
	}
	iteration.IssueRepo = strings.TrimSpace(iteration.IssueRepo)
	iteration.IssueURL = strings.TrimSpace(iteration.IssueURL)
	iteration.PullRequestRepo = strings.TrimSpace(iteration.PullRequestRepo)
	iteration.PullRequestURL = strings.TrimSpace(iteration.PullRequestURL)
	iteration.DecisionReason = strings.TrimSpace(iteration.DecisionReason)
	iteration.MetadataJSON = strings.TrimSpace(iteration.MetadataJSON)
	return iteration, nil
}

func (s *Store) GetSkillOptTrainIteration(ctx context.Context, id string) (SkillOptTrainIteration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, eval_run_id, base_template_version_id,
			candidate_version_id, mode, exploration_level, state, issue_repo, issue_number,
			issue_url, pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json,
			created_at, updated_at
		FROM skillopt_train_iterations WHERE id = ?`, strings.TrimSpace(id))
	return scanSkillOptTrainIteration(row)
}

func (s *Store) ListSkillOptTrainIterations(ctx context.Context, sessionID string) ([]SkillOptTrainIteration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, eval_run_id, base_template_version_id,
			candidate_version_id, mode, exploration_level, state, issue_repo, issue_number,
			issue_url, pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json,
			created_at, updated_at
		FROM skillopt_train_iterations
		WHERE session_id = ?
		ORDER BY rowid`, strings.TrimSpace(sessionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var iterations []SkillOptTrainIteration
	for rows.Next() {
		iteration, err := scanSkillOptTrainIteration(rows)
		if err != nil {
			return nil, err
		}
		iterations = append(iterations, iteration)
	}
	return iterations, rows.Err()
}

func (s *Store) GetLatestSkillOptTrainIteration(ctx context.Context, sessionID string) (SkillOptTrainIteration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, eval_run_id, base_template_version_id,
			candidate_version_id, mode, exploration_level, state, issue_repo, issue_number,
			issue_url, pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json,
			created_at, updated_at
		FROM skillopt_train_iterations
		WHERE session_id = ?
		ORDER BY rowid DESC
		LIMIT 1`, strings.TrimSpace(sessionID))
	return scanSkillOptTrainIteration(row)
}

func (s *Store) GetSkillOptTrainIterationByEvalRun(ctx context.Context, evalRunID string) (SkillOptTrainIteration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, eval_run_id, base_template_version_id,
			candidate_version_id, mode, exploration_level, state, issue_repo, issue_number,
			issue_url, pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json,
			created_at, updated_at
		FROM skillopt_train_iterations
		WHERE eval_run_id = ?
		ORDER BY rowid DESC
		LIMIT 1`, strings.TrimSpace(evalRunID))
	return scanSkillOptTrainIteration(row)
}

func (s *Store) UpsertSkillOptReviewWatch(ctx context.Context, watch SkillOptReviewWatch) error {
	watch, err := normalizeSkillOptReviewWatch(watch)
	if err != nil {
		return err
	}
	return upsertSkillOptReviewWatchExec(ctx, s.db, watch)
}

func upsertSkillOptReviewWatchExec(ctx context.Context, exec sqlExecer, watch SkillOptReviewWatch) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO skillopt_review_watches(
			repo, issue_number, run_id, expected_item_ids_json, status, last_seen_comment_id,
			last_import_error_hash, stale_after, stale_threshold_seconds, stale_notified,
			metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(repo, issue_number) DO UPDATE SET
			run_id = excluded.run_id,
			expected_item_ids_json = excluded.expected_item_ids_json,
			status = excluded.status,
			last_seen_comment_id = excluded.last_seen_comment_id,
			last_import_error_hash = excluded.last_import_error_hash,
			stale_after = excluded.stale_after,
			stale_threshold_seconds = excluded.stale_threshold_seconds,
			stale_notified = excluded.stale_notified,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		watch.Repo, watch.IssueNumber, watch.RunID, watch.ExpectedItemIDsJSON, watch.Status, watch.LastSeenCommentID,
		watch.LastImportErrorHash, watch.StaleAfter, watch.StaleThresholdSeconds, boolInt(watch.StaleNotified), watch.MetadataJSON)
	return err
}

func normalizeSkillOptReviewWatch(watch SkillOptReviewWatch) (SkillOptReviewWatch, error) {
	watch.Repo = strings.TrimSpace(watch.Repo)
	if watch.Repo == "" {
		return SkillOptReviewWatch{}, errors.New("skillopt review watch repo is required")
	}
	if watch.IssueNumber <= 0 {
		return SkillOptReviewWatch{}, errors.New("skillopt review watch issue number is required")
	}
	watch.RunID = strings.TrimSpace(watch.RunID)
	if watch.RunID == "" {
		return SkillOptReviewWatch{}, errors.New("skillopt review watch run id is required")
	}
	watch.ExpectedItemIDsJSON = strings.TrimSpace(watch.ExpectedItemIDsJSON)
	watch.Status = strings.TrimSpace(strings.ToLower(watch.Status))
	if watch.Status == "" {
		watch.Status = SkillOptReviewWatchStatusWatching
	}
	switch watch.Status {
	case SkillOptReviewWatchStatusWatching, SkillOptReviewWatchStatusImported, SkillOptReviewWatchStatusClosed, SkillOptReviewWatchStatusStaleNotified, SkillOptReviewWatchStatusFailed:
	default:
		return SkillOptReviewWatch{}, fmt.Errorf("skillopt review watch status %q is not supported", watch.Status)
	}
	watch.LastImportErrorHash = strings.TrimSpace(watch.LastImportErrorHash)
	watch.StaleAfter = strings.TrimSpace(watch.StaleAfter)
	if watch.StaleThresholdSeconds < 0 {
		return SkillOptReviewWatch{}, errors.New("skillopt review watch stale threshold must not be negative")
	}
	watch.MetadataJSON = strings.TrimSpace(watch.MetadataJSON)
	return watch, nil
}

func (s *Store) GetSkillOptReviewWatch(ctx context.Context, repo string, issueNumber int64) (SkillOptReviewWatch, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo, issue_number, run_id, expected_item_ids_json, status,
			last_seen_comment_id, last_import_error_hash, stale_after, stale_threshold_seconds,
			stale_notified, metadata_json, created_at, updated_at
		FROM skillopt_review_watches
		WHERE repo = ? AND issue_number = ?`, strings.TrimSpace(repo), issueNumber)
	return scanSkillOptReviewWatch(row)
}

func (s *Store) ListSkillOptReviewWatches(ctx context.Context, status string) ([]SkillOptReviewWatch, error) {
	status = strings.TrimSpace(strings.ToLower(status))
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT repo, issue_number, run_id, expected_item_ids_json, status,
				last_seen_comment_id, last_import_error_hash, stale_after, stale_threshold_seconds,
				stale_notified, metadata_json, created_at, updated_at
			FROM skillopt_review_watches
			ORDER BY rowid`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT repo, issue_number, run_id, expected_item_ids_json, status,
				last_seen_comment_id, last_import_error_hash, stale_after, stale_threshold_seconds,
				stale_notified, metadata_json, created_at, updated_at
			FROM skillopt_review_watches
			WHERE status = ?
			ORDER BY rowid`, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	watches := []SkillOptReviewWatch{}
	for rows.Next() {
		watch, err := scanSkillOptReviewWatch(rows)
		if err != nil {
			return nil, err
		}
		watches = append(watches, watch)
	}
	return watches, rows.Err()
}

func (s *Store) UpsertEvalReviewItem(ctx context.Context, item EvalReviewItem) error {
	return upsertEvalReviewItemExec(ctx, s.db, item)
}

func upsertEvalReviewItemExec(ctx context.Context, exec sqlExecer, item EvalReviewItem) error {
	if strings.TrimSpace(item.ID) == "" {
		item.ID = item.RunID + "/" + item.ItemID
	}
	if strings.TrimSpace(item.RunID) == "" {
		return errors.New("eval review item run id is required")
	}
	if strings.TrimSpace(item.ItemID) == "" {
		return errors.New("eval review item id is required")
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO eval_review_items(
			id, run_id, item_id, title, source_artifact_id, baseline_artifact_id, candidate_artifact_id,
			preview_artifact_id, diff_artifact_id, metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			run_id = excluded.run_id,
			item_id = excluded.item_id,
			title = excluded.title,
			source_artifact_id = excluded.source_artifact_id,
			baseline_artifact_id = excluded.baseline_artifact_id,
			candidate_artifact_id = excluded.candidate_artifact_id,
			preview_artifact_id = excluded.preview_artifact_id,
			diff_artifact_id = excluded.diff_artifact_id,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		item.ID, item.RunID, item.ItemID, item.Title, item.SourceArtifactID, item.BaselineArtifactID, item.CandidateArtifactID,
		item.PreviewArtifactID, item.DiffArtifactID, item.MetadataJSON)
	return err
}

func (s *Store) ReplaceGeneratedEvalReviewArtifacts(ctx context.Context, runID string, writes []EvalReviewGenerationWrite) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("eval review generation run id is required")
	}
	normalized := make([]EvalReviewGenerationWrite, 0, len(writes))
	for _, write := range writes {
		itemID := strings.TrimSpace(write.ItemID)
		if itemID == "" && write.ReviewItem != nil {
			itemID = strings.TrimSpace(write.ReviewItem.ItemID)
		}
		if itemID == "" {
			return errors.New("eval review generation item id is required")
		}
		next := EvalReviewGenerationWrite{ItemID: itemID}
		for _, artifact := range write.Artifacts {
			artifact, err := normalizeEvalArtifact(artifact)
			if err != nil {
				return err
			}
			next.Artifacts = append(next.Artifacts, artifact)
		}
		if write.ReviewItem != nil {
			item := *write.ReviewItem
			item.RunID = runID
			item.ItemID = itemID
			if strings.TrimSpace(item.ID) == "" {
				item.ID = item.RunID + "/" + item.ItemID
			}
			if strings.TrimSpace(item.RunID) == "" {
				return errors.New("eval review item run id is required")
			}
			if strings.TrimSpace(item.ItemID) == "" {
				return errors.New("eval review item id is required")
			}
			next.ReviewItem = &item
		}
		seen := map[string]struct{}{}
		for _, option := range write.Options {
			option.RunID = runID
			option.ItemID = itemID
			option, err := normalizeEvalReviewOption(option)
			if err != nil {
				return err
			}
			if _, ok := seen[option.Label]; ok {
				return fmt.Errorf("eval review option label %q is duplicated for item %q", option.Label, itemID)
			}
			seen[option.Label] = struct{}{}
			next.Options = append(next.Options, option)
		}
		normalized = append(normalized, next)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, write := range normalized {
		for _, artifact := range write.Artifacts {
			if err := upsertEvalArtifactTx(ctx, tx, artifact); err != nil {
				return err
			}
		}
		if write.ReviewItem != nil {
			item := *write.ReviewItem
			if _, err := tx.ExecContext(ctx, `INSERT INTO eval_review_items(
					id, run_id, item_id, title, source_artifact_id, baseline_artifact_id, candidate_artifact_id,
					preview_artifact_id, diff_artifact_id, metadata_json, created_at, updated_at
				)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
				ON CONFLICT(id) DO UPDATE SET
					run_id = excluded.run_id,
					item_id = excluded.item_id,
					title = excluded.title,
					source_artifact_id = excluded.source_artifact_id,
					baseline_artifact_id = excluded.baseline_artifact_id,
					candidate_artifact_id = excluded.candidate_artifact_id,
					preview_artifact_id = excluded.preview_artifact_id,
					diff_artifact_id = excluded.diff_artifact_id,
					metadata_json = excluded.metadata_json,
					updated_at = CURRENT_TIMESTAMP`,
				item.ID, item.RunID, item.ItemID, item.Title, item.SourceArtifactID, item.BaselineArtifactID, item.CandidateArtifactID,
				item.PreviewArtifactID, item.DiffArtifactID, item.MetadataJSON); err != nil {
				return err
			}
		}
		if len(write.Options) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM eval_review_options WHERE run_id = ? AND item_id = ?`, runID, write.ItemID); err != nil {
			return err
		}
		for _, option := range write.Options {
			if _, err := tx.ExecContext(ctx, `INSERT INTO eval_review_options(
					id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
				)
				VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
				option.ID, option.RunID, option.ItemID, option.Label, option.ArtifactID, option.Role, option.MetadataJSON); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) ListEvalReviewItems(ctx context.Context, runID string) ([]EvalReviewItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, item_id, title, source_artifact_id, baseline_artifact_id,
			candidate_artifact_id, preview_artifact_id, diff_artifact_id, metadata_json, created_at, updated_at
		FROM eval_review_items WHERE run_id = ? ORDER BY item_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []EvalReviewItem
	for rows.Next() {
		item, err := scanEvalReviewItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) UpsertEvalReviewOption(ctx context.Context, option EvalReviewOption) error {
	option, err := normalizeEvalReviewOption(option)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO eval_review_options(
			id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(run_id, item_id, label) DO UPDATE SET
			id = excluded.id,
			artifact_id = excluded.artifact_id,
			role = excluded.role,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		option.ID, option.RunID, option.ItemID, option.Label, option.ArtifactID, option.Role, option.MetadataJSON)
	return err
}

func (s *Store) ReplaceEvalReviewOptions(ctx context.Context, runID string, itemID string, options []EvalReviewOption) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("eval review option run id is required")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return errors.New("eval review option item id is required")
	}
	normalized := make([]EvalReviewOption, 0, len(options))
	seen := map[string]struct{}{}
	for _, option := range options {
		option.RunID = runID
		option.ItemID = itemID
		option, err := normalizeEvalReviewOption(option)
		if err != nil {
			return err
		}
		if _, ok := seen[option.Label]; ok {
			return fmt.Errorf("eval review option label %q is duplicated for item %q", option.Label, itemID)
		}
		seen[option.Label] = struct{}{}
		normalized = append(normalized, option)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM eval_review_options WHERE run_id = ? AND item_id = ?`, runID, itemID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, option := range normalized {
		if _, err := tx.ExecContext(ctx, `INSERT INTO eval_review_options(
				id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			option.ID, option.RunID, option.ItemID, option.Label, option.ArtifactID, option.Role, option.MetadataJSON); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func normalizeEvalReviewOption(option EvalReviewOption) (EvalReviewOption, error) {
	option.RunID = strings.TrimSpace(option.RunID)
	if option.RunID == "" {
		return EvalReviewOption{}, errors.New("eval review option run id is required")
	}
	option.ItemID = strings.TrimSpace(option.ItemID)
	if option.ItemID == "" {
		return EvalReviewOption{}, errors.New("eval review option item id is required")
	}
	option.Label = normalizeOptionLabel(option.Label)
	if option.Label == "" {
		return EvalReviewOption{}, errors.New("eval review option label is required")
	}
	option.ArtifactID = strings.TrimSpace(option.ArtifactID)
	if option.ArtifactID == "" {
		return EvalReviewOption{}, errors.New("eval review option artifact id is required")
	}
	option.Role = strings.TrimSpace(strings.ToLower(option.Role))
	if option.ID == "" {
		option.ID = option.RunID + "/" + option.ItemID + "/" + option.Label
	}
	option.MetadataJSON = strings.TrimSpace(option.MetadataJSON)
	return option, nil
}

func (s *Store) ListEvalReviewOptions(ctx context.Context, runID string, itemID string) ([]EvalReviewOption, error) {
	runID = strings.TrimSpace(runID)
	itemID = strings.TrimSpace(itemID)
	var (
		rows *sql.Rows
		err  error
	)
	if itemID == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
			FROM eval_review_options WHERE run_id = ? ORDER BY item_id, label`, runID)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
			FROM eval_review_options WHERE run_id = ? AND item_id = ? ORDER BY label`, runID, itemID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var options []EvalReviewOption
	for rows.Next() {
		option, err := scanEvalReviewOption(rows)
		if err != nil {
			return nil, err
		}
		options = append(options, option)
	}
	return options, rows.Err()
}

func scanEvalArtifact(row interface{ Scan(dest ...any) error }) (EvalArtifact, error) {
	var artifact EvalArtifact
	if err := row.Scan(&artifact.ID, &artifact.Hash, &artifact.MediaType, &artifact.SizeBytes, &artifact.Driver, &artifact.CreatedAt); err != nil {
		return EvalArtifact{}, err
	}
	return artifact, nil
}

func scanEvalRun(row interface{ Scan(dest ...any) error }) (EvalRun, error) {
	var run EvalRun
	if err := row.Scan(&run.ID, &run.TemplateID, &run.TemplateVersionID, &run.TargetRepo, &run.State, &run.Mode, &run.ExplorationLevel, &run.OptionsCount, &run.MetadataJSON, &run.CreatedAt, &run.UpdatedAt); err != nil {
		return EvalRun{}, err
	}
	return run, nil
}

func scanSkillOptTrainSession(row interface{ Scan(dest ...any) error }) (SkillOptTrainSession, error) {
	var session SkillOptTrainSession
	if err := row.Scan(&session.ID, &session.TemplateID, &session.TemplateVersionID, &session.TargetRepo, &session.WorkspaceRepo, &session.PreviewRepo,
		&session.RequestSummary, &session.TaskKind, &session.State, &session.MetadataJSON, &session.CreatedAt, &session.UpdatedAt); err != nil {
		return SkillOptTrainSession{}, err
	}
	return session, nil
}

func scanSkillOptTrainIteration(row interface{ Scan(dest ...any) error }) (SkillOptTrainIteration, error) {
	var iteration SkillOptTrainIteration
	if err := row.Scan(&iteration.ID, &iteration.SessionID, &iteration.EvalRunID, &iteration.BaseTemplateVersionID,
		&iteration.CandidateVersionID, &iteration.Mode, &iteration.ExplorationLevel, &iteration.State, &iteration.IssueRepo,
		&iteration.IssueNumber, &iteration.IssueURL, &iteration.PullRequestRepo, &iteration.PullRequestNumber,
		&iteration.PullRequestURL, &iteration.DecisionReason, &iteration.MetadataJSON, &iteration.CreatedAt, &iteration.UpdatedAt); err != nil {
		return SkillOptTrainIteration{}, err
	}
	return iteration, nil
}

func scanSkillOptReviewWatch(row interface{ Scan(dest ...any) error }) (SkillOptReviewWatch, error) {
	var watch SkillOptReviewWatch
	var staleNotified int
	if err := row.Scan(&watch.Repo, &watch.IssueNumber, &watch.RunID, &watch.ExpectedItemIDsJSON, &watch.Status,
		&watch.LastSeenCommentID, &watch.LastImportErrorHash, &watch.StaleAfter, &watch.StaleThresholdSeconds,
		&staleNotified, &watch.MetadataJSON, &watch.CreatedAt, &watch.UpdatedAt); err != nil {
		return SkillOptReviewWatch{}, err
	}
	watch.StaleNotified = staleNotified != 0
	return watch, nil
}

func scanEvalReviewItem(row interface{ Scan(dest ...any) error }) (EvalReviewItem, error) {
	var item EvalReviewItem
	if err := row.Scan(&item.ID, &item.RunID, &item.ItemID, &item.Title, &item.SourceArtifactID, &item.BaselineArtifactID,
		&item.CandidateArtifactID, &item.PreviewArtifactID, &item.DiffArtifactID, &item.MetadataJSON, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return EvalReviewItem{}, err
	}
	return item, nil
}

func scanEvalReviewOption(row interface{ Scan(dest ...any) error }) (EvalReviewOption, error) {
	var option EvalReviewOption
	if err := row.Scan(&option.ID, &option.RunID, &option.ItemID, &option.Label, &option.ArtifactID, &option.Role, &option.MetadataJSON, &option.CreatedAt, &option.UpdatedAt); err != nil {
		return EvalReviewOption{}, err
	}
	return option, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) UpsertFeedbackEvent(ctx context.Context, event FeedbackEvent) error {
	if strings.TrimSpace(event.ID) == "" {
		event.ID = feedbackEventID(event)
	}
	if strings.TrimSpace(event.RunID) == "" {
		return errors.New("feedback event run id is required")
	}
	if strings.TrimSpace(event.ItemID) == "" {
		return errors.New("feedback event item id is required")
	}
	if strings.TrimSpace(event.Choice) == "" {
		return errors.New("feedback event choice is required")
	}
	if strings.TrimSpace(event.Reviewer) == "" {
		return errors.New("feedback event reviewer is required")
	}
	if strings.TrimSpace(event.Source) == "" {
		return errors.New("feedback event source is required")
	}
	if strings.TrimSpace(event.CreatedAt) == "" {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO feedback_events(id, run_id, item_id, choice, reasoning, reviewer, source, source_url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, item_id, reviewer, source, source_url) DO UPDATE SET
			id = excluded.id,
			choice = excluded.choice,
			reasoning = excluded.reasoning,
			reviewer = excluded.reviewer,
			source = excluded.source,
			source_url = excluded.source_url,
			created_at = excluded.created_at`,
		event.ID, event.RunID, event.ItemID, event.Choice, event.Reasoning, event.Reviewer, event.Source, event.SourceURL, event.CreatedAt)
	return err
}

func (s *Store) ListFeedbackEvents(ctx context.Context, runID string) ([]FeedbackEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, item_id, choice, reasoning, reviewer, source, source_url, created_at
		FROM feedback_events WHERE run_id = ? ORDER BY item_id, reviewer, source, source_url`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []FeedbackEvent
	for rows.Next() {
		event, err := scanFeedbackEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func scanFeedbackEvent(row interface{ Scan(dest ...any) error }) (FeedbackEvent, error) {
	var event FeedbackEvent
	if err := row.Scan(&event.ID, &event.RunID, &event.ItemID, &event.Choice, &event.Reasoning, &event.Reviewer, &event.Source, &event.SourceURL, &event.CreatedAt); err != nil {
		return FeedbackEvent{}, err
	}
	return event, nil
}

func (s *Store) UpsertRankedFeedbackEvent(ctx context.Context, event RankedFeedbackEvent) error {
	event, err := normalizeRankedFeedbackEvent(event)
	if err != nil {
		return err
	}
	if err := s.validateRankedFeedbackEventOptions(ctx, event); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO ranked_feedback_events(
			id, run_id, item_id, ranking_json, winner, useful_traits_json, rejected_traits_json,
			required_improvements_json, quality, continue_mode, promote, reasoning, reviewer, source, source_url, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, item_id, reviewer, source, source_url) DO UPDATE SET
			id = excluded.id,
			ranking_json = excluded.ranking_json,
			winner = excluded.winner,
			useful_traits_json = excluded.useful_traits_json,
			rejected_traits_json = excluded.rejected_traits_json,
			required_improvements_json = excluded.required_improvements_json,
			quality = excluded.quality,
			continue_mode = excluded.continue_mode,
			promote = excluded.promote,
			reasoning = excluded.reasoning,
			reviewer = excluded.reviewer,
			source = excluded.source,
			source_url = excluded.source_url,
			created_at = excluded.created_at`,
		event.ID, event.RunID, event.ItemID, event.RankingJSON, event.Winner, event.UsefulTraitsJSON, event.RejectedTraitsJSON, event.RequiredImprovementsJSON,
		event.Quality, event.ContinueMode, event.Promote, event.Reasoning, event.Reviewer, event.Source, event.SourceURL, event.CreatedAt)
	return err
}

func (s *Store) validateRankedFeedbackEventOptions(ctx context.Context, event RankedFeedbackEvent) error {
	run, err := s.GetEvalRun(ctx, event.RunID)
	if err != nil {
		return err
	}
	options, err := s.ListEvalReviewOptions(ctx, event.RunID, event.ItemID)
	if err != nil {
		return err
	}
	if len(options) == 0 {
		return fmt.Errorf("ranked feedback item %s has no registered review options", event.ItemID)
	}
	ranking, err := rankedFeedbackRanking(event)
	if err != nil {
		return err
	}
	expectedOptions := len(options)
	if run.OptionsCount > 0 {
		expectedOptions = run.OptionsCount
		if len(options) != expectedOptions {
			return fmt.Errorf("ranked feedback item %s has %d registered options, want %d run options", event.ItemID, len(options), expectedOptions)
		}
	}
	if len(ranking) != expectedOptions {
		return fmt.Errorf("ranked feedback item %s ranking includes %d options, want %d options", event.ItemID, len(ranking), expectedOptions)
	}
	known := make(map[string]struct{}, len(options))
	for _, option := range options {
		known[normalizeOptionLabel(option.Label)] = struct{}{}
	}
	ranked := make(map[string]struct{}, len(ranking))
	for _, label := range ranking {
		if _, ok := known[label]; !ok {
			return fmt.Errorf("ranked feedback item %s references unknown option %q", event.ItemID, label)
		}
		ranked[label] = struct{}{}
	}
	for label := range known {
		if _, ok := ranked[label]; !ok {
			return fmt.Errorf("ranked feedback item %s missing registered option %q", event.ItemID, label)
		}
	}
	if event.Winner != "" {
		if _, ok := known[event.Winner]; !ok {
			return fmt.Errorf("ranked feedback item %s winner references unknown option %q", event.ItemID, event.Winner)
		}
		if event.Winner != ranking[0] {
			return fmt.Errorf("ranked feedback item %s winner %q does not match first ranked option %q", event.ItemID, event.Winner, ranking[0])
		}
	}
	for _, traits := range []struct {
		name string
		json string
	}{
		{name: "useful_traits_json", json: event.UsefulTraitsJSON},
		{name: "rejected_traits_json", json: event.RejectedTraitsJSON},
	} {
		if strings.TrimSpace(traits.json) == "" {
			continue
		}
		var decoded map[string][]string
		if err := json.Unmarshal([]byte(traits.json), &decoded); err != nil {
			return fmt.Errorf("ranked feedback %s must be a JSON object keyed by option label: %w", traits.name, err)
		}
		for label := range decoded {
			normalized := normalizeOptionLabel(label)
			if normalized == "" {
				return fmt.Errorf("ranked feedback %s contains an empty option label", traits.name)
			}
			if _, ok := known[normalized]; !ok {
				return fmt.Errorf("ranked feedback %s references unknown option %q", traits.name, normalized)
			}
		}
	}
	return nil
}

func normalizeRankedFeedbackEvent(event RankedFeedbackEvent) (RankedFeedbackEvent, error) {
	event.RunID = strings.TrimSpace(event.RunID)
	if event.RunID == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback run id is required")
	}
	event.ItemID = strings.TrimSpace(event.ItemID)
	if event.ItemID == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback item id is required")
	}
	event.Winner = normalizeOptionLabel(event.Winner)
	event.RankingJSON = strings.TrimSpace(event.RankingJSON)
	if event.RankingJSON == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback ranking_json is required")
	}
	if _, err := rankedFeedbackRanking(event); err != nil {
		return RankedFeedbackEvent{}, err
	}
	event.UsefulTraitsJSON = strings.TrimSpace(event.UsefulTraitsJSON)
	event.RejectedTraitsJSON = strings.TrimSpace(event.RejectedTraitsJSON)
	event.RequiredImprovementsJSON = strings.TrimSpace(event.RequiredImprovementsJSON)
	if event.RequiredImprovementsJSON != "" {
		var decoded any
		if err := json.Unmarshal([]byte(event.RequiredImprovementsJSON), &decoded); err != nil {
			return RankedFeedbackEvent{}, fmt.Errorf("ranked feedback required_improvements_json must be valid JSON: %w", err)
		}
	}
	event.Quality = normalizeRankedFeedbackQuality(event.Quality)
	if event.Quality == "__invalid__" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback quality must be one of poor, acceptable, or strong")
	}
	event.ContinueMode = normalizeRankedFeedbackContinueMode(event.ContinueMode)
	if event.ContinueMode == "__invalid__" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback continue_mode must be one of explore, refine, distill, or validate")
	}
	event.Promote = normalizeRankedFeedbackPromote(event.Promote)
	if event.Promote == "__invalid__" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback promote must be yes or no")
	}
	event.Reasoning = strings.TrimSpace(event.Reasoning)
	event.Reviewer = strings.TrimSpace(event.Reviewer)
	if event.Reviewer == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback reviewer is required")
	}
	event.Source = strings.TrimSpace(event.Source)
	if event.Source == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback source is required")
	}
	event.SourceURL = strings.TrimSpace(event.SourceURL)
	if strings.TrimSpace(event.CreatedAt) == "" {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(event.ID) == "" {
		event.ID = rankedFeedbackEventID(event)
	}
	return event, nil
}

func normalizeRankedFeedbackQuality(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "":
		return ""
	case "poor", "acceptable", "strong":
		return strings.TrimSpace(strings.ToLower(value))
	}
	return "__invalid__"
}

func normalizeRankedFeedbackContinueMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "":
		return ""
	case EvalRunModeExplore, EvalRunModeRefine, EvalRunModeDistill, EvalRunModeValidate:
		return strings.TrimSpace(strings.ToLower(value))
	}
	return "__invalid__"
}

func normalizeRankedFeedbackPromote(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "":
		return ""
	case "yes", "y", "true":
		return "yes"
	case "no", "n", "false":
		return "no"
	}
	return "__invalid__"
}

func rankedFeedbackRanking(event RankedFeedbackEvent) ([]string, error) {
	var ranking []string
	if err := json.Unmarshal([]byte(event.RankingJSON), &ranking); err != nil {
		return nil, fmt.Errorf("ranked feedback ranking_json must be a JSON array of option labels: %w", err)
	}
	if len(ranking) < 2 {
		return nil, errors.New("ranked feedback ranking must include at least two options")
	}
	seen := map[string]struct{}{}
	for index, label := range ranking {
		normalized := normalizeOptionLabel(label)
		if normalized == "" {
			return nil, fmt.Errorf("ranked feedback ranking contains empty option label at position %d", index+1)
		}
		if _, ok := seen[normalized]; ok {
			return nil, fmt.Errorf("ranked feedback ranking contains duplicate option label %q", normalized)
		}
		seen[normalized] = struct{}{}
		ranking[index] = normalized
	}
	return ranking, nil
}

func (s *Store) ListRankedFeedbackEvents(ctx context.Context, runID string) ([]RankedFeedbackEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, item_id, ranking_json, winner, useful_traits_json, rejected_traits_json,
			required_improvements_json, quality, continue_mode, promote, reasoning, reviewer, source, source_url, created_at
		FROM ranked_feedback_events WHERE run_id = ? ORDER BY item_id, reviewer, source, source_url`, strings.TrimSpace(runID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []RankedFeedbackEvent
	for rows.Next() {
		event, err := scanRankedFeedbackEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func scanRankedFeedbackEvent(row interface{ Scan(dest ...any) error }) (RankedFeedbackEvent, error) {
	var event RankedFeedbackEvent
	if err := row.Scan(&event.ID, &event.RunID, &event.ItemID, &event.RankingJSON, &event.Winner, &event.UsefulTraitsJSON, &event.RejectedTraitsJSON,
		&event.RequiredImprovementsJSON, &event.Quality, &event.ContinueMode, &event.Promote, &event.Reasoning, &event.Reviewer, &event.Source, &event.SourceURL, &event.CreatedAt); err != nil {
		return RankedFeedbackEvent{}, err
	}
	return event, nil
}

func (s *Store) ListPairwisePreferences(ctx context.Context, runID string) ([]PairwisePreference, error) {
	events, err := s.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		return nil, err
	}
	preferences := []PairwisePreference{}
	for _, event := range events {
		eventPreferences, err := PairwisePreferencesForRankedFeedback(event)
		if err != nil {
			return nil, err
		}
		preferences = append(preferences, eventPreferences...)
	}
	return preferences, nil
}

func PairwisePreferencesForRankedFeedback(event RankedFeedbackEvent) ([]PairwisePreference, error) {
	ranking, err := rankedFeedbackRanking(event)
	if err != nil {
		return nil, err
	}
	preferences := make([]PairwisePreference, 0, len(ranking)*(len(ranking)-1)/2)
	for preferredIndex, preferred := range ranking {
		for _, rejected := range ranking[preferredIndex+1:] {
			preferences = append(preferences, PairwisePreference{
				RunID:         event.RunID,
				ItemID:        event.ItemID,
				Preferred:     preferred,
				Rejected:      rejected,
				RankedEventID: event.ID,
				Reviewer:      event.Reviewer,
				Source:        event.Source,
				SourceURL:     event.SourceURL,
				CreatedAt:     event.CreatedAt,
			})
		}
	}
	return preferences, nil
}

func feedbackEventID(event FeedbackEvent) string {
	parts := []string{event.RunID, event.ItemID, event.Reviewer, event.Source, event.SourceURL}
	for index, part := range parts {
		parts[index] = strings.TrimSpace(part)
	}
	content, _ := json.Marshal(parts)
	sum := sha256.Sum256(content)
	return "feedback:" + hex.EncodeToString(sum[:])
}

func rankedFeedbackEventID(event RankedFeedbackEvent) string {
	parts := []string{event.RunID, event.ItemID, event.Reviewer, event.Source, event.SourceURL}
	for index, part := range parts {
		parts[index] = strings.TrimSpace(part)
	}
	content, _ := json.Marshal(parts)
	sum := sha256.Sum256(content)
	return "ranked-feedback:" + hex.EncodeToString(sum[:])
}

func normalizeOptionLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
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
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO resource_locks(resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, command_hash, acquired_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, resourceKey, ownerJobID, ownerToken, lock.OwnerPID, strings.TrimSpace(lock.OwnerHostname), strings.TrimSpace(lock.CommandHash), nowText, nowText, expiresAt)
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
	row := s.db.QueryRowContext(ctx, `SELECT resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, command_hash, acquired_at, updated_at, expires_at FROM resource_locks WHERE resource_key = ?`, resourceKey)
	var lock ResourceLock
	if err := row.Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken, &lock.OwnerPID, &lock.OwnerHostname, &lock.CommandHash, &lock.AcquiredAt, &lock.UpdatedAt, &lock.ExpiresAt); err != nil {
		return ResourceLock{}, err
	}
	return lock, nil
}

func (s *Store) HeartbeatResourceLock(ctx context.Context, resourceKey string, ownerJobID string, ownerToken string, now time.Time, expiresAt time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE resource_locks
		SET updated_at = ?, expires_at = ?
		WHERE resource_key = ? AND owner_job_id = ? AND owner_token = ?`,
		formatResourceLockTime(now), formatResourceLockTime(expiresAt), strings.TrimSpace(resourceKey), strings.TrimSpace(ownerJobID), strings.TrimSpace(ownerToken))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
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
			AND owner_pid <= 0
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
	template.ContentHash = templateContentHash(template.Content)
	template.VersionState = "current"
	return template, nil
}

func scanAgentTemplateWithVersion(scanner agentTemplateScanner) (AgentTemplate, error) {
	var template AgentTemplate
	if err := scanner.Scan(&template.ID, &template.Name, &template.Description, &template.SourceRepo, &template.SourceRef, &template.SourcePath, &template.ResolvedCommit, &template.Content, &template.MetadataJSON, &template.VersionID, &template.VersionNumber, &template.VersionState, &template.ContentHash, &template.CreatedAt, &template.UpdatedAt); err != nil {
		return AgentTemplate{}, err
	}
	if template.ContentHash == "" {
		template.ContentHash = templateContentHash(template.Content)
	}
	if template.VersionState == "" {
		template.VersionState = "current"
	}
	return template, nil
}

func scanAgentTemplateVersion(scanner agentTemplateScanner) (AgentTemplateVersion, error) {
	var version AgentTemplateVersion
	if err := scanner.Scan(&version.ID, &version.TemplateID, &version.VersionNumber, &version.State, &version.Name, &version.Description, &version.SourceRepo, &version.SourceRef, &version.SourcePath, &version.ResolvedCommit, &version.ContentHash, &version.Content, &version.MetadataJSON, &version.CreatedAt, &version.UpdatedAt, &version.PromotedAt); err != nil {
		return AgentTemplateVersion{}, err
	}
	if version.ContentHash == "" {
		version.ContentHash = templateContentHash(version.Content)
	}
	return version, nil
}

func scanAgentTemplateCandidateReview(scanner agentTemplateScanner) (AgentTemplateCandidateReview, error) {
	var review AgentTemplateCandidateReview
	var score sql.NullFloat64
	if err := scanner.Scan(&review.VersionID, &review.TemplateID, &review.BaseVersionID, &review.DiffArtifactID, &score, &review.PreferenceSummary, &review.EvalReportJSON, &review.SummaryMetadataJSON, &review.State, &review.DecisionReason, &review.CreatedAt, &review.UpdatedAt, &review.DecidedAt); err != nil {
		return AgentTemplateCandidateReview{}, err
	}
	if score.Valid {
		review.Score = &score.Float64
	}
	return review, nil
}

func agentTemplateFromVersion(version AgentTemplateVersion) AgentTemplate {
	return AgentTemplate{
		ID:             version.TemplateID,
		Name:           version.Name,
		Description:    version.Description,
		SourceRepo:     version.SourceRepo,
		SourceRef:      version.SourceRef,
		SourcePath:     version.SourcePath,
		ResolvedCommit: version.ResolvedCommit,
		Content:        version.Content,
		MetadataJSON:   version.MetadataJSON,
		VersionID:      version.ID,
		VersionNumber:  version.VersionNumber,
		VersionState:   version.State,
		ContentHash:    version.ContentHash,
		CreatedAt:      version.CreatedAt,
		UpdatedAt:      version.UpdatedAt,
	}
}

func SplitAgentTemplateReference(ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}
	if index := strings.LastIndex(ref, "@"); index > 0 {
		return strings.TrimSpace(ref[:index]), strings.TrimSpace(ref[index+1:])
	}
	return ref, ""
}

func getCurrentAgentTemplateVersion(ctx context.Context, tx *sql.Tx, templateID string) (AgentTemplateVersion, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at
		FROM agent_templates t
		JOIN agent_template_versions v ON v.id = t.current_version_id
		WHERE t.id = ?`, templateID)
	version, err := scanAgentTemplateVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTemplateVersion{}, false, nil
	}
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	return version, true, nil
}

func getAgentTemplateVersionByIDTx(ctx context.Context, tx *sql.Tx, versionID string) (AgentTemplateVersion, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
		FROM agent_template_versions WHERE id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateVersion(row)
}

func latestSelectableVersionID(ctx context.Context, tx *sql.Tx, templateID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM agent_template_versions
		WHERE template_id = ? AND state IN ('current', 'pending')
		ORDER BY version DESC LIMIT 1`, templateID).Scan(&id)
	return id, err
}

func upsertAgentTemplateCandidateReviewDecisionTx(ctx context.Context, tx *sql.Tx, version AgentTemplateVersion, state string, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_template_candidate_reviews(
			version_id, template_id, state, decision_reason, decided_at, updated_at
		)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(version_id) DO UPDATE SET
			state = excluded.state,
			decision_reason = excluded.decision_reason,
			decided_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP`,
		version.ID,
		version.TemplateID,
		strings.TrimSpace(state),
		strings.TrimSpace(reason))
	return err
}

func nextAgentTemplateVersionNumber(ctx context.Context, tx *sql.Tx, templateID string) (int, error) {
	var current sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM agent_template_versions WHERE template_id = ?`, templateID).Scan(&current); err != nil {
		return 0, err
	}
	if !current.Valid {
		return 1, nil
	}
	return int(current.Int64) + 1, nil
}

func agentTemplateVersionID(templateID string, version int) string {
	return fmt.Sprintf("%s@v%d", strings.TrimSpace(templateID), version)
}

func templateContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
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
	`
CREATE TABLE agent_template_versions (
	id TEXT PRIMARY KEY,
	template_id TEXT NOT NULL,
	version INTEGER NOT NULL,
	state TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	source_repo TEXT NOT NULL,
	source_ref TEXT NOT NULL,
	source_path TEXT NOT NULL,
	resolved_commit TEXT NOT NULL,
	content_hash TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	metadata_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	promoted_at TEXT NOT NULL DEFAULT '',
	UNIQUE(template_id, version)
);

INSERT OR REPLACE INTO agent_template_versions(id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at)
SELECT id || '@v1', id, 1, 'current', name, description, source_repo, source_ref, source_path, resolved_commit, '', content, metadata_json, created_at, updated_at, updated_at
FROM agent_templates;

ALTER TABLE agent_templates ADD COLUMN current_version_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_templates ADD COLUMN latest_version_id TEXT NOT NULL DEFAULT '';

UPDATE agent_templates
SET current_version_id = id || '@v1',
	latest_version_id = id || '@v1'
WHERE current_version_id = '';
	`,
	`
CREATE TABLE eval_artifacts (
	id TEXT PRIMARY KEY,
	hash TEXT NOT NULL,
	media_type TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL DEFAULT 0,
	driver TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE eval_runs (
	id TEXT PRIMARY KEY,
	template_id TEXT NOT NULL DEFAULT '',
	template_version_id TEXT NOT NULL DEFAULT '',
	target_repo TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'draft',
	metadata_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE eval_review_items (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	item_id TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	source_artifact_id TEXT NOT NULL DEFAULT '',
	baseline_artifact_id TEXT NOT NULL DEFAULT '',
	candidate_artifact_id TEXT NOT NULL DEFAULT '',
	preview_artifact_id TEXT NOT NULL DEFAULT '',
	diff_artifact_id TEXT NOT NULL DEFAULT '',
	metadata_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(run_id, item_id)
);
	`,
	`
CREATE TABLE feedback_events (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	item_id TEXT NOT NULL,
	choice TEXT NOT NULL,
	reasoning TEXT NOT NULL DEFAULT '',
	reviewer TEXT NOT NULL,
	source TEXT NOT NULL,
	source_url TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(run_id, item_id, reviewer, source, source_url)
);
	`,
	`
CREATE TABLE agent_template_candidate_reviews (
	version_id TEXT PRIMARY KEY,
	template_id TEXT NOT NULL,
	base_version_id TEXT NOT NULL DEFAULT '',
	diff_artifact_id TEXT NOT NULL DEFAULT '',
	score REAL,
	preference_summary TEXT NOT NULL DEFAULT '',
	eval_report_json TEXT NOT NULL DEFAULT '',
	summary_metadata_json TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'pending',
	decision_reason TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	decided_at TEXT NOT NULL DEFAULT ''
);
	`,
	`
ALTER TABLE eval_runs ADD COLUMN mode TEXT NOT NULL DEFAULT 'validate';
ALTER TABLE eval_runs ADD COLUMN exploration_level TEXT NOT NULL DEFAULT 'low';
ALTER TABLE eval_runs ADD COLUMN options_count INTEGER NOT NULL DEFAULT 2;

CREATE TABLE eval_review_options (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	item_id TEXT NOT NULL,
	label TEXT NOT NULL,
	artifact_id TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT '',
	metadata_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(run_id, item_id, label)
);

CREATE TABLE ranked_feedback_events (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	item_id TEXT NOT NULL,
	ranking_json TEXT NOT NULL,
	winner TEXT NOT NULL DEFAULT '',
	useful_traits_json TEXT NOT NULL DEFAULT '',
	rejected_traits_json TEXT NOT NULL DEFAULT '',
	reasoning TEXT NOT NULL DEFAULT '',
	reviewer TEXT NOT NULL,
	source TEXT NOT NULL,
	source_url TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(run_id, item_id, reviewer, source, source_url)
);
	`,
	`
CREATE TABLE skillopt_train_sessions (
	id TEXT PRIMARY KEY,
	template_id TEXT NOT NULL,
	template_version_id TEXT NOT NULL DEFAULT '',
	target_repo TEXT NOT NULL DEFAULT '',
	workspace_repo TEXT NOT NULL DEFAULT '',
	preview_repo TEXT NOT NULL DEFAULT '',
	request_summary TEXT NOT NULL DEFAULT '',
	task_kind TEXT NOT NULL DEFAULT 'custom',
	state TEXT NOT NULL DEFAULT 'request_confirmed',
	metadata_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE skillopt_train_iterations (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	eval_run_id TEXT NOT NULL DEFAULT '',
	base_template_version_id TEXT NOT NULL DEFAULT '',
	candidate_version_id TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT 'explore',
	exploration_level TEXT NOT NULL DEFAULT 'high',
	state TEXT NOT NULL DEFAULT 'request_confirmed',
	issue_repo TEXT NOT NULL DEFAULT '',
	issue_number INTEGER NOT NULL DEFAULT 0,
	issue_url TEXT NOT NULL DEFAULT '',
	pull_request_repo TEXT NOT NULL DEFAULT '',
	pull_request_number INTEGER NOT NULL DEFAULT 0,
	pull_request_url TEXT NOT NULL DEFAULT '',
	decision_reason TEXT NOT NULL DEFAULT '',
	metadata_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(session_id, id)
);
	`,
	`
ALTER TABLE ranked_feedback_events ADD COLUMN quality TEXT NOT NULL DEFAULT '';
ALTER TABLE ranked_feedback_events ADD COLUMN continue_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE ranked_feedback_events ADD COLUMN promote TEXT NOT NULL DEFAULT '';
	`,
	`
ALTER TABLE ranked_feedback_events ADD COLUMN required_improvements_json TEXT NOT NULL DEFAULT '';
	`,
	`
ALTER TABLE resource_locks ADD COLUMN owner_pid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resource_locks ADD COLUMN owner_hostname TEXT NOT NULL DEFAULT '';
ALTER TABLE resource_locks ADD COLUMN command_hash TEXT NOT NULL DEFAULT '';
	`,
	`
CREATE TABLE skillopt_review_watches (
	repo TEXT NOT NULL,
	issue_number INTEGER NOT NULL,
	run_id TEXT NOT NULL,
	expected_item_ids_json TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'watching',
	last_seen_comment_id INTEGER NOT NULL DEFAULT 0,
	last_import_error_hash TEXT NOT NULL DEFAULT '',
	stale_after TEXT NOT NULL DEFAULT '',
	stale_threshold_seconds INTEGER NOT NULL DEFAULT 0,
	stale_notified INTEGER NOT NULL DEFAULT 0,
	metadata_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(repo, issue_number)
);

CREATE INDEX idx_skillopt_review_watches_status ON skillopt_review_watches(status);
CREATE INDEX idx_skillopt_review_watches_run_id ON skillopt_review_watches(run_id);
	`,
}
