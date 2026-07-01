package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	Model          string
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
	// CanarySample is the active canary's sampled-traffic fraction in (0,1] (#484),
	// recorded only on a `canary`-state version; 0 (the column DEFAULT) for every
	// other state so existing rows read identically. The routing seam draws a
	// per-resolution random < CanarySample to route to the canary.
	CanarySample float64
	// CanaryStartedAt is the RFC3339 window-start of the active canary (#484), set
	// when a version transitions to `canary`; "" (the DEFAULT) otherwise. It bounds
	// the daemon's regression-window comparator.
	CanaryStartedAt string
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

// BanditArm is one persisted Beta-Bernoulli arm of the #473 Mode B
// champion-challenger bandit, keyed by (TemplateID, TemplateVersionID). Alpha
// and Beta are the Beta(1+wins, 1+losses) posterior under the uniform Beta(1,1)
// prior (so a missing row is exactly NewArm()); Pulls is the total win+loss
// count that drives the "over K samples" string and the low-traffic tiering
// floor. The arm key is the version id (which already encodes the agent and
// template), so champion and challenger are two distinct rows and promoting or
// rejecting a version naturally retires its arm.
type BanditArm struct {
	TemplateID        string
	TemplateVersionID string
	Alpha             float64
	Beta              float64
	Pulls             int
	UpdatedAt         string
}

// SkillOptJudgeOutcome records a single human promote/reject decision on a
// SkillOpt candidate alongside the LLM judge's signal for the same candidate,
// so judge calibration (agreement, Cohen's kappa, per-dimension drift) can be
// computed offline. The raw judge eval report is stored in JudgeScoreJSON so
// the derived Direction can always be recomputed from source.
type SkillOptJudgeOutcome struct {
	ID                 string
	CandidateVersionID string
	TemplateID         string
	JudgeScoreJSON     string
	JudgePromptVersion string
	JudgeEvaluatorID   string
	JudgePromptHash    string
	HumanDecision      string
	Direction          string
	Reason             string
	CreatedAt          string
}

// CockpitPane records one live Herdr pane opened for a delegation subagent's
// job (issue #357). Panes are keyed by (workspace_id, pane_key) so the cockpit
// can find/reuse a pane for a seat without splitting duplicates; root_job_id is
// derived from the job payload (not a jobs column) so all panes of one
// orchestration root can be listed and torn down together.
type CockpitPane struct {
	ID          string
	JobID       string
	PaneKey     string
	RootJobID   string
	PaneID      string
	WorkspaceID string
	Source      string
	CreatedAt   string
}

type AgentRepo struct {
	AgentName    string
	RepoFullName string
}

type AgentInstance struct {
	Name           string
	Type           string
	Runtime        string
	RuntimeRef     string
	RepoFullName   string
	Role           string
	TemplateID     string
	Model          string
	Capabilities   []string
	AutonomyPolicy string
	State          string
	CreatedAt      string
	LastUsedAt     string
	ExpiresAt      string
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
	WorktreePath string
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
	ID              string
	Agent           string
	Type            string
	State           string
	Payload         string
	ParentJobID     string
	DelegationID    string
	DelegationDepth int
	DelegatedBy     string
	// RootID is the id of the coordination tree's originating coordinator,
	// denormalized onto the row as an indexed column (idx_jobs_root_id, #420) so
	// root-scoped helpers can answer "which jobs belong to this run?" with one
	// indexed lookup instead of a full-table scan that unmarshals every payload.
	// It mirrors the engine's rootJobID() rule: payload.RootJobID when set, else
	// the job's own id (self-root). payload.RootJobID stays the value source of
	// truth; this column is a write-time denormalized index. It is populated by
	// the ListJobsByRoot projection (other readers may leave it empty).
	RootID string
	// RootKilled is the operator kill-switch flag (#341). When true, the
	// delegation tree rooted at this job has been killed: the engine's next
	// dispatch routes through the graceful finalize continuation instead of
	// dispatching new delegations, and the daemon skips queued children.
	RootKilled bool
	// InputTokens and OutputTokens record the runtime token usage captured for
	// this job (best-effort per runtime; see UpdateJobUsage). They default to 0
	// and are populated by every jobs SELECT so the per-root delegation token
	// budget (#338 Part B) can sum a tree's usage. A job whose runtime does not
	// report usage contributes 0 — the budget still works, it just under-counts.
	InputTokens  int
	OutputTokens int
	// UpdatedAt is populated by ListJobs (for age display in the dashboard);
	// other readers may leave it zero.
	UpdatedAt string
	// CreatedAt is the job's creation timestamp (SQLite UTC form,
	// "2006-01-02 15:04:05"). Populated by ListJobs/GetJob so the web dashboard
	// can stamp a node's StartedAt; other readers may leave it zero.
	CreatedAt string
}

type JobEvent struct {
	JobID   string
	Kind    string
	Message string
	// CreatedAt is the event's timestamp (SQLite UTC form). Populated by
	// ListJobEvents so the web dashboard can order a node's timeline by real
	// wall-clock time; other readers may leave it zero.
	CreatedAt string
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

type InteractivePrompt struct {
	ID            string   `json:"id"`
	Question      string   `json:"question"`
	Choices       []string `json:"choices,omitempty"`
	Default       string   `json:"default,omitempty"`
	Required      bool     `json:"required"`
	AnswerFormat  string   `json:"answer_format"`
	SourceCommand string   `json:"source_command"`
	State         string   `json:"state"`
	AnswerValue   string   `json:"answer_value,omitempty"`
	AnswerSource  string   `json:"answer_source,omitempty"`
	CreatedAt     string   `json:"created_at,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
	AnsweredAt    string   `json:"answered_at,omitempty"`
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

	InteractivePromptStatePending  = "pending"
	InteractivePromptStateResolved = "resolved"
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
	TieGroupsJSON            string
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
	RepoFullName           string
	Branch                 string
	Owner                  string
	SkipNativeReviewFanout bool
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

// 15s (raised from 5s): several repo-scoped daemons can share one ~/.gitmoot DB
// file, and WAL permits only one writer process at a time. A burst (e.g. a
// plan-by-N fan-out all writing results plus a coordinator continuation across
// processes) could exceed a 5s wait and surface "database is locked" (SQLITE_BUSY),
// which in turn made dependent reads return stale data. A longer wait lets the
// burst drain instead of erroring. (For many concurrent projects, also give each
// daemon its own home via GITMOOT_HOME_DIR so they do not share a DB at all.)
const sqliteBusyTimeoutMillis = 15000

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := configureWritableSQLite(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
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
	if err := configureReadOnlySQLite(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func configureWritableSQLite(ctx context.Context, db *sql.DB) error {
	if err := configureReadOnlySQLite(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("configure sqlite WAL: %w", err)
	}
	// synchronous=NORMAL is the WAL-recommended setting: it fsyncs only at WAL
	// checkpoints instead of on every commit (the FULL default), making the
	// per-item generation commits cheap. The bounded tradeoff is that an OS
	// crash or power loss can lose transactions committed since the last WAL
	// checkpoint (not merely the last commit); WAL still guarantees the database
	// is never corrupted. This is safe for generation because resume regenerates
	// any item whose commit did not survive. The wal_autocheckpoint default
	// (1000 pages) is left in place so long-lived read connections do not let the
	// WAL grow unbounded.
	if _, err := db.ExecContext(ctx, `PRAGMA synchronous=NORMAL`); err != nil {
		return fmt.Errorf("configure sqlite synchronous: %w", err)
	}
	return nil
}

func configureReadOnlySQLite(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout=%d`, sqliteBusyTimeoutMillis)); err != nil {
		return fmt.Errorf("configure sqlite busy timeout: %w", err)
	}
	return nil
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
	if err := s.backfillJobRootID(ctx); err != nil {
		return err
	}
	return nil
}

// backfillJobRootID populates the denormalized root_id column for any pre-#420
// jobs row that still has the migration's DEFAULT ” (every row inserted after
// #420 gets root_id at write time, so this only ever touches the historical
// backlog once). It is the Go-side equivalent of the spec's in-migration
// backfill SQL, chosen because modernc's json_extract raises a SQL error on a
// malformed payload — which would abort the migration — whereas unmarshalling in
// Go lets a malformed or root_job_id-less payload self-root to the job's own id,
// matching the engine's rootJobID() fallback exactly.
//
// It is idempotent: the WHERE root_id = ” filter means a second run touches
// nothing, and a job whose true root is genuinely "" is impossible because the
// fallback is always the non-empty job id. Done outside applyMigration so it can
// re-converge a partially-backfilled DB on any startup without bumping a version.
func (s *Store) backfillJobRootID(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, payload FROM jobs WHERE root_id = ''`)
	if err != nil {
		return err
	}
	type pending struct{ id, rootID string }
	var todo []pending
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			rows.Close()
			return err
		}
		rootID := rootIDFromPayload(payload)
		if strings.TrimSpace(rootID) == "" {
			rootID = id // malformed / root_job_id-less payload self-roots
		}
		todo = append(todo, pending{id: id, rootID: rootID})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(todo) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range todo {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET root_id = ? WHERE id = ? AND root_id = ''`, p.rootID, p.id); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, template_id, model, capabilities_json, autonomy_policy, health_status, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(name) DO UPDATE SET
				role = excluded.role,
				runtime = excluded.runtime,
				runtime_ref = excluded.runtime_ref,
				repo_scope = excluded.repo_scope,
				template_id = excluded.template_id,
				model = excluded.model,
				capabilities_json = excluded.capabilities_json,
				autonomy_policy = excluded.autonomy_policy,
				health_status = excluded.health_status,
				updated_at = CURRENT_TIMESTAMP`,
		agent.Name, agent.Role, agent.Runtime, agent.RuntimeRef, agent.RepoScope, agent.TemplateID, agent.Model, string(capabilities), agent.AutonomyPolicy, agent.HealthStatus); err != nil {
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
	row := s.db.QueryRowContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, template_id, model, capabilities_json, autonomy_policy, health_status
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
	row := s.db.QueryRowContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, template_id, model, capabilities_json, autonomy_policy, health_status
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
		Capabilities:   instance.Capabilities,
		AutonomyPolicy: policy,
		HealthStatus:   instance.State,
	}, nil
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, role, runtime, runtime_ref, repo_scope, template_id, model, capabilities_json, autonomy_policy, health_status
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
	row := s.db.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at, v.canary_sample, v.canary_started_at
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
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions
		WHERE template_id = ? AND (id = ? OR CAST(version AS TEXT) = ?)`, templateID, templateID+"@v"+versionRef, versionRef)
	return scanAgentTemplateVersion(row)
}

func (s *Store) GetAgentTemplateVersionByID(ctx context.Context, versionID string) (AgentTemplateVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions WHERE id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateVersion(row)
}

func (s *Store) ListAgentTemplateVersions(ctx context.Context, templateID string) ([]AgentTemplateVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
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
	query := `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
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
	// A `pending` candidate promotes directly (#471); a `canary` candidate (#484)
	// GRADUATES through the SAME state-machine writes (supersede champion, become
	// current, clear the canary fraction/window). Any other state is a hard error.
	if target.State != "pending" && target.State != "canary" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not pending or canary", target.ID, target.State)
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
	// Clear canary_sample/canary_started_at on the target so a graduated canary
	// carries no stale window state; pending targets already have the 0/'' defaults,
	// so this is a no-op for the #471 path.
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'current', promoted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP, canary_sample = 0, canary_started_at = '' WHERE id = ?`, target.ID); err != nil {
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

// UpdateAgentTemplateMetadata replaces the stored metadata_json for an installed
// template (the agent_templates row) and, when present, its current version row,
// so both the template-level read path and version-referenced exports observe the
// change. Content, name, description, and version identity are untouched: this is
// a focused metadata write used by `skillopt judge promote` to fold an accepted
// judge prompt into the template's Evaluation map without minting a new version.
func (s *Store) UpdateAgentTemplateMetadata(ctx context.Context, templateID string, metadataJSON string) (AgentTemplate, error) {
	templateID = strings.TrimSpace(templateID)
	metadataJSON = strings.TrimSpace(metadataJSON)
	if templateID == "" {
		return AgentTemplate{}, errors.New("template id is required")
	}
	if metadataJSON == "" {
		return AgentTemplate{}, errors.New("metadata_json is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplate{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET metadata_json = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, metadataJSON, templateID)
	if err != nil {
		return AgentTemplate{}, err
	}
	if err := requireAffected(result, "agent template", templateID); err != nil {
		return AgentTemplate{}, err
	}
	if current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, templateID); err != nil {
		return AgentTemplate{}, err
	} else if hasCurrent {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET metadata_json = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, metadataJSON, current.ID); err != nil {
			return AgentTemplate{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplate{}, err
	}
	return s.GetAgentTemplate(ctx, templateID)
}

// RejectAgentTemplateVersion retires a pending (#471) or canary (#484) candidate.
// The returned changed bool reports whether THIS call performed the rejection
// transition: it is false in the idempotent already-rejected branch and true when
// the row actually moved to `rejected`. Callers that emit a one-shot side effect on
// rejection (the #484 candidate.rolled_back event) MUST gate it on changed so a
// concurrent / post-crash re-run does not double-fire.
func (s *Store) RejectAgentTemplateVersion(ctx context.Context, versionID string, reason string) (AgentTemplateVersion, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, versionID)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	// Idempotent: an already-rejected target is a no-op success (changed=false) so
	// the #484 canary auto-rollback (which rejects the regressing canary) can be
	// re-run safely after a crash — or raced by a concurrent harvest — without
	// erroring AND without re-firing the rolled_back event.
	if target.State == "rejected" {
		if err := tx.Commit(); err != nil {
			return AgentTemplateVersion{}, false, err
		}
		return target, false, nil
	}
	// A `pending` candidate rejects directly (#471); a `canary` candidate (#484) is
	// retired by the auto-rollback. A canary never holds current_version_id (the
	// champion stays current throughout the canary), so rejecting it leaves the
	// champion live — no current pointer changes. The canary fraction/window are
	// cleared so no stale routing state remains.
	if target.State != "pending" && target.State != "canary" {
		return AgentTemplateVersion{}, false, fmt.Errorf("agent template version %s is %s, not pending or canary", target.ID, target.State)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'rejected', updated_at = CURRENT_TIMESTAMP, canary_sample = 0, canary_started_at = '' WHERE id = ?`, target.ID); err != nil {
		return AgentTemplateVersion{}, false, err
	}
	latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET latest_version_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, latestID, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
		return AgentTemplateVersion{}, false, err
	}
	if err := upsertAgentTemplateCandidateReviewDecisionTx(ctx, tx, target, "rejected", reason); err != nil {
		return AgentTemplateVersion{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, false, err
	}
	version, err := s.GetAgentTemplateVersionByID(ctx, target.ID)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	return version, true, nil
}

// RevertAgentTemplateVersion makes a previously superseded version current
// again (a rollback). It mirrors PromoteAgentTemplateVersion's pointer/state
// writes but accepts a superseded target instead of a pending one, and records
// no candidate-review decision (reverts are not candidate decisions).
func (s *Store) RevertAgentTemplateVersion(ctx context.Context, templateID string, versionID string) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, versionID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if target.TemplateID != strings.TrimSpace(templateID) {
		return AgentTemplateVersion{}, fmt.Errorf("version %s belongs to template %s, not %s", target.ID, target.TemplateID, templateID)
	}
	if target.State != "superseded" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not superseded; only a previously current version can be reverted to", target.ID, target.State)
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
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

// CanaryPromoteAgentTemplateVersion transitions a PENDING candidate to the new
// `canary` state (#484) WITHOUT touching the template's current_version_id: the
// prior champion stays the live current version, so every non-sampled resolution
// is byte-identical and the routing seam (templateSnapshot) opts only a sampled
// fraction onto the canary. It records the active canary's sample fraction and
// window-start for the daemon regression comparator, and recomputes
// latest_version_id (which excludes the `canary` state) so the canary never leaks
// into latest_version_id. It enforces at most ONE active canary per template and
// requires an existing current champion (a canary only makes sense behind a live
// champion). It mirrors PromoteAgentTemplateVersion's state-machine writes but
// leaves the champion current — the defining safety property of the canary.
func (s *Store) CanaryPromoteAgentTemplateVersion(ctx context.Context, versionID string, sample float64) (AgentTemplateVersion, error) {
	if sample <= 0 || sample > 1 {
		return AgentTemplateVersion{}, fmt.Errorf("canary sample %v must be in (0,1]", sample)
	}
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
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not pending; only a pending candidate can become a canary", target.ID, target.State)
	}
	// A canary sits BEHIND a live champion; refuse if there is no current version so
	// non-sampled traffic always has a champion to resolve to.
	_, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if !hasCurrent {
		return AgentTemplateVersion{}, fmt.Errorf("template %s has no current champion; refusing to start a canary without one", target.TemplateID)
	}
	// At most one active canary per template: a second concurrent canary would make
	// routing ambiguous and the regression window unattributable.
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_template_versions WHERE template_id = ? AND state = 'canary'`, target.TemplateID).Scan(&existing); err != nil {
		return AgentTemplateVersion{}, err
	}
	if existing > 0 {
		return AgentTemplateVersion{}, fmt.Errorf("template %s already has an active canary; resolve it before starting another", target.TemplateID)
	}
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'canary', canary_sample = ?, canary_started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, sample, startedAt, target.ID); err != nil {
		return AgentTemplateVersion{}, err
	}
	// Recompute latest_version_id: latestSelectableVersionID only considers
	// current/pending, so the now-canary version is excluded and latest falls back
	// to the champion (or a higher pending) — the canary never becomes latest.
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
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

// GetActiveCanaryVersion returns the template's single active `canary`-state
// version (#484), or ok=false when none is canarying. It is the indexed lookup
// (idx_atv_canary) the routing seam and the daemon regression comparator use to
// discover the active canary. An unresolvable/empty template yields ok=false, not
// an error.
func (s *Store) GetActiveCanaryVersion(ctx context.Context, templateID string) (AgentTemplateVersion, bool, error) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return AgentTemplateVersion{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions
		WHERE template_id = ? AND state = 'canary'
		ORDER BY version DESC LIMIT 1`, templateID)
	version, err := scanAgentTemplateVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTemplateVersion{}, false, nil
	}
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	return version, true, nil
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
	// Snapshot the candidate's eval report inside the transaction so the
	// judge-outcome captured after commit reflects the report the decision was
	// made against. Best-effort: capture must never fail the decision.
	normalizedDecision := strings.TrimSpace(decision)
	var captureReason string
	if normalizedDecision == "rejected" {
		captureReason = iteration.DecisionReason
	}
	capturedEvalReport := candidateEvalReportJSONTx(ctx, tx, target.ID)
	switch normalizedDecision {
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
	// Capture the judge-vs-human outcome only after the decision is durably
	// committed, and never let a capture failure surface to the caller — the
	// human's promote/reject must stand regardless. (#345)
	if err := captureSkillOptJudgeOutcome(ctx, s.db, target, capturedEvalReport, normalizedDecision, captureReason); err != nil {
		log.Printf("skillopt: capture judge outcome for candidate %s failed: %v", target.ID, err)
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

func candidateEvalReportJSONTx(ctx context.Context, tx *sql.Tx, versionID string) string {
	var report string
	if err := tx.QueryRowContext(ctx, `SELECT eval_report_json FROM agent_template_candidate_reviews WHERE version_id = ?`, strings.TrimSpace(versionID)).Scan(&report); err != nil {
		return ""
	}
	return report
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_instances(name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			type = excluded.type,
			runtime = excluded.runtime,
			runtime_ref = excluded.runtime_ref,
			repo_full_name = excluded.repo_full_name,
			role = excluded.role,
			template_id = excluded.template_id,
			model = excluded.model,
			capabilities_json = excluded.capabilities_json,
			autonomy_policy = excluded.autonomy_policy,
			state = excluded.state,
			last_used_at = excluded.last_used_at,
			expires_at = excluded.expires_at`,
		instance.Name, instance.Type, instance.Runtime, instance.RuntimeRef, instance.RepoFullName, instance.Role, instance.TemplateID, instance.Model, string(capabilities), instance.AutonomyPolicy, instance.State, instance.CreatedAt, instance.LastUsedAt, instance.ExpiresAt)
	return err
}

func (s *Store) GetAgentInstance(ctx context.Context, name string) (AgentInstance, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at
		FROM agent_instances WHERE name = ?`, name)
	return scanAgentInstance(row)
}

func (s *Store) FindReusableAgentInstance(ctx context.Context, typ string, repo string, autonomyPolicy string, now time.Time) (AgentInstance, bool, error) {
	if strings.TrimSpace(autonomyPolicy) == "" {
		autonomyPolicy = "auto"
	}
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at
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
	row := s.db.QueryRowContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at
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
	rows, err := s.db.QueryContext(ctx, `SELECT name, type, runtime, runtime_ref, repo_full_name, role, template_id, model, capabilities_json, autonomy_policy, state, created_at, last_used_at, expires_at
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
	if err := row.Scan(&instance.Name, &instance.Type, &instance.Runtime, &instance.RuntimeRef, &instance.RepoFullName, &instance.Role, &instance.TemplateID, &instance.Model, &capabilities, &instance.AutonomyPolicy, &instance.State, &instance.CreatedAt, &instance.LastUsedAt, &instance.ExpiresAt); err != nil {
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

// rootIDFromPayload extracts the payload's root_job_id (the engine's rootJobID()
// value source of truth) from a job payload JSON string. A malformed or
// root_job_id-less payload yields "" — the caller's COALESCE(NULLIF(?,”), ?)
// then self-roots the row to job.ID, matching rootJobID()'s fallback (#420).
func rootIDFromPayload(payload string) string {
	var p struct {
		RootJobID string `json:"root_job_id"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	return p.RootJobID
}

func (s *Store) CreateJob(ctx context.Context, job Job) error {
	// root_id is a denormalized index of the engine's rootJobID() rule (#420):
	// bind the SAME COALESCE(NULLIF(?,''), ?) to (payload.RootJobID, job.ID) so
	// the invariant — payload root when set, else self-root — holds regardless of
	// caller. payload.RootJobID stays the value source of truth.
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), ?), CURRENT_TIMESTAMP)`,
		job.ID, job.Agent, job.Type, job.State, job.Payload,
		job.ParentJobID, job.DelegationID, job.DelegationDepth, job.DelegatedBy,
		rootIDFromPayload(job.Payload), job.ID)
	return err
}

func (s *Store) CreateJobWithEvent(ctx context.Context, job Job, event JobEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// See CreateJob: same COALESCE(NULLIF(?,''), ?) bound to (payload.RootJobID,
	// job.ID) denormalizes the rootJobID() rule onto the indexed root_id column.
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), ?), CURRENT_TIMESTAMP)`,
		job.ID, job.Agent, job.Type, job.State, job.Payload,
		job.ParentJobID, job.DelegationID, job.DelegationDepth, job.DelegatedBy,
		rootIDFromPayload(job.Payload), job.ID); err != nil {
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

// JobCreatedAt returns a job's created_at timestamp string (SQLite UTC format,
// "2006-01-02 15:04:05"). Used by the delegation wall-clock backstop to measure a
// tree's age from its root job. Returns sql.ErrNoRows if the job is unknown.
func (s *Store) JobCreatedAt(ctx context.Context, id string) (string, error) {
	var createdAt string
	row := s.db.QueryRowContext(ctx, `SELECT created_at FROM jobs WHERE id = ?`, id)
	if err := row.Scan(&createdAt); err != nil {
		return "", err
	}
	return createdAt, nil
}

// SetRootJobKilled marks a delegation tree's root job as killed by an operator
// (#341). Only the root row carries the flag; the engine and daemon scope to a
// tree by joining children's payload RootJobID back to this id. No-op-safe: an
// unknown id simply affects zero rows (the caller verifies existence).
func (s *Store) SetRootJobKilled(ctx context.Context, rootID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET root_killed = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, rootID)
	return err
}

// IsRootJobKilled reports whether the root job rootID has been killed by an
// operator (#341). An unknown id reads as not killed (COALESCE over no rows
// yields zero rows, so the scan defaults to false), so the backstop fails open
// and never blocks dispatch on a lookup miss.
func (s *Store) IsRootJobKilled(ctx context.Context, rootID string) (bool, error) {
	var killed bool
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(root_killed, 0) FROM jobs WHERE id = ?`, rootID)
	if err := row.Scan(&killed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return killed, nil
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens, updated_at, created_at FROM jobs WHERE id = ?`, id)
	var job Job
	if err := row.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt, &job.CreatedAt); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens, updated_at, created_at FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt, &job.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListJobsByParent returns the direct children of parentJobID (delegation
// children and the coordinator continuation job), ordered by delegation_id then
// id for a stable tree. It selects updated_at like ListJobs so callers can show
// child age; the idx_jobs_parent_job_id index backs the filter.
func (s *Store) ListJobsByParent(ctx context.Context, parentJobID string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens, updated_at
		FROM jobs WHERE parent_job_id = ? ORDER BY delegation_id, id`, parentJobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListJobsByRoot returns every job in the coordination tree rooted at rootID:
// the originating coordinator itself (its root_id self-roots to its own id) plus
// every child or continuation whose root_id points back at it (#420). It mirrors
// ListJobsByParent — same projection, populating RootID too — and is backed by
// idx_jobs_root_id for an indexed lookup instead of a full-table scan that
// unmarshals every payload. ORDER BY id is deterministic.
func (s *Store) ListJobsByRoot(ctx context.Context, rootID string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_id, root_killed, input_tokens, output_tokens, updated_at
		FROM jobs WHERE root_id = ? ORDER BY id`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootID, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// CountJobsByRoot returns the number of jobs in the coordination tree rooted at
// rootID via an indexed COUNT(*) on root_id (#420) — no row materialization, no
// payload unmarshal. It is the SQL form of the engine's countRootDelegationJobs;
// the grouping key (root_id) is identical to the old payload.RootJobID filter,
// so the count is byte-identical.
func (s *Store) CountJobsByRoot(ctx context.Context, rootID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE root_id = ?`, rootID).Scan(&count)
	return count, err
}

// SumJobTokensByRoot returns the summed runtime token usage (input + output)
// across the coordination tree rooted at rootID via an indexed SQL SUM on
// root_id (#420). COALESCE guards the empty-tree case (SUM over zero rows is
// NULL). It is the SQL form of sumRootDelegationTokens; same grouping key, so
// the total is byte-identical.
func (s *Store) SumJobTokensByRoot(ctx context.Context, rootID string) (int, error) {
	var total int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM jobs WHERE root_id = ?`, rootID).Scan(&total)
	return total, err
}

func (s *Store) ListQueuedJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens
		FROM jobs WHERE state = ? ORDER BY created_at, rowid`, "queued")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListRunningJobsUpdatedBefore(ctx context.Context, before time.Time) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens
		FROM jobs WHERE state = ? AND updated_at < ? ORDER BY id`, "running", before.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens); err != nil {
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

func (s *Store) DelegateQueuedJob(ctx context.Context, id string, fromAgent string, toAgent string, payload string, event JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// root_id is intentionally left untouched here (#420). This is an in-place
	// re-assignment of an already-queued job to a different agent; the job stays
	// in the same coordination tree, so its root_id (set at insert from the
	// original payload's RootJobID) remains correct. A re-delegation never carries
	// a new RootJobID, so re-binding the COALESCE would only re-derive the same id.
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET agent = ?, payload = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND agent = ? AND state = ?`,
		strings.TrimSpace(toAgent), payload, id, strings.TrimSpace(fromAgent), "queued")
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

// UpdateJobUsage records the runtime token usage for a job (best-effort capture
// from the runtime adapters; see internal/runtime). Negative values are clamped
// to 0 so a malformed runtime usage report can never push a tree's aggregate
// below zero (#338 Part B / ruflo design reference #380). It does not touch
// updated_at: usage is captured at delivery time alongside the existing payload
// write and is purely additive accounting, not a state transition.
func (s *Store) UpdateJobUsage(ctx context.Context, id string, inputTokens int, outputTokens int) error {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	// Accumulate rather than overwrite: a job can be delivered more than once (a
	// malformed-output repair retry re-delivers the same job.ID), so each delivery
	// adds its usage instead of clobbering the prior write. The per-delta clamp to
	// 0 above keeps the running total monotonic.
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET input_tokens = input_tokens + ?, output_tokens = output_tokens + ? WHERE id = ?`, inputTokens, outputTokens, id)
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
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, kind, message, created_at FROM job_events WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []JobEvent
	for rows.Next() {
		var event JobEvent
		if err := rows.Scan(&event.JobID, &event.Kind, &event.Message, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// LatestJobEvents returns the most recent event for every job that has one,
// keyed by job id, in a single query (the dashboard refresh would otherwise
// issue one ListJobEvents per job).
func (s *Store) LatestJobEvents(ctx context.Context) (map[string]JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, kind, message FROM job_events
		WHERE id IN (SELECT MAX(id) FROM job_events GROUP BY job_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := map[string]JobEvent{}
	for rows.Next() {
		var event JobEvent
		if err := rows.Scan(&event.JobID, &event.Kind, &event.Message); err != nil {
			return nil, err
		}
		events[event.JobID] = event
	}
	return events, rows.Err()
}

// JobIDsWithEventKind returns a map jobID -> message of the LATEST job_event of
// the given kind, one entry per job that has at least one such event. It is a
// single indexed query mirroring LatestJobEvents (but scoped to one kind) so a
// caller can surface, e.g., a delegation_preflight_failed reason in `job list`
// without an N-per-job lookup and regardless of whether that event is the job's
// overall latest event (a corrective continuation makes delegation_continuation_enqueued
// the latest event of the coordinator).
func (s *Store) JobIDsWithEventKind(ctx context.Context, kind string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, message FROM job_events
		WHERE kind = ? AND id IN (SELECT MAX(id) FROM job_events WHERE kind = ? GROUP BY job_id)`, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var jobID, message string
		if err := rows.Scan(&jobID, &message); err != nil {
			return nil, err
		}
		out[jobID] = message
	}
	return out, rows.Err()
}

// LatestJobEventsOfKinds returns, per job, the LATEST job_event whose kind is one
// of the given kinds, keyed by job id, in a single indexed query. It mirrors
// LatestJobEvents but restricts the candidate set to the caller's "reason" kinds
// so a stuck-job surface (issue #552) can find the most recent event that
// actually explains why a queued/blocked job is waiting — ignoring benign
// lifecycle events (queued, route_selected, delegation_continuation_enqueued)
// that would otherwise be the overall latest event and mask the real reason. An
// empty kinds slice returns an empty map without querying.
func (s *Store) LatestJobEventsOfKinds(ctx context.Context, kinds []string) (map[string]JobEvent, error) {
	if len(kinds) == 0 {
		return map[string]JobEvent{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, 0, len(kinds)*2)
	for _, kind := range kinds {
		args = append(args, kind)
	}
	// The IN list appears twice (outer filter + inner MAX(id) subquery), so the
	// bound args are the kinds repeated.
	args = append(args, args...)
	query := `SELECT job_id, kind, message FROM job_events
		WHERE kind IN (` + placeholders + `) AND id IN (
			SELECT MAX(id) FROM job_events WHERE kind IN (` + placeholders + `) GROUP BY job_id)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]JobEvent{}
	for rows.Next() {
		var event JobEvent
		if err := rows.Scan(&event.JobID, &event.Kind, &event.Message); err != nil {
			return nil, err
		}
		out[event.JobID] = event
	}
	return out, rows.Err()
}

// JobIDsWithPendingDelegationWorktreeReclaim returns the IDs of jobs whose most
// recent terminal delegation-worktree cleanup outcome was a PRESERVE
// (delegation_worktree_cleanup_skipped) NOT yet followed by a removal
// (delegation_worktree_removed) — i.e. their per-delegation worktree and
// gitmoot-delegation-* branch are still on disk awaiting reclaim (#536/#549).
//
// This is a single indexed query so the daemon's reclaim pass can iterate only
// the (small) set of jobs that genuinely carry an unreconciled preserve marker,
// instead of listing EVERY terminal job and re-reading each one's FULL event
// history (ListJobEvents) on every 1s supervisor tick — which was O(jobs ×
// events) and burned a core once a few hundred terminal jobs had accumulated.
//
// The order of the two cleanup-outcome kinds matters (a worktree can be
// preserved, then later removed), so the LAST one wins: a job is "pending" iff
// the highest-id event among the two kinds is a skip. The MAX(id) subquery picks
// that latest outcome per job and the outer filter keeps only the jobs where it
// is a skip — exactly mirroring the per-job lastCleanupOutcomeIsSkip walk, but
// set-at-once.
func (s *Store) JobIDsWithPendingDelegationWorktreeReclaim(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id FROM job_events
		WHERE kind = 'delegation_worktree_cleanup_skipped'
		  AND id IN (
			SELECT MAX(id) FROM job_events
			WHERE kind IN ('delegation_worktree_cleanup_skipped', 'delegation_worktree_removed')
			GROUP BY job_id
		)
		ORDER BY job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobIDs []string
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			return nil, err
		}
		jobIDs = append(jobIDs, jobID)
	}
	return jobIDs, rows.Err()
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

// ListSkillOptTrainSessions returns all SkillOpt train sessions, most recently
// updated first.
func (s *Store) ListSkillOptTrainSessions(ctx context.Context) ([]SkillOptTrainSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_id, template_version_id, target_repo, workspace_repo, preview_repo,
			request_summary, task_kind, state, metadata_json, created_at, updated_at
		FROM skillopt_train_sessions ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []SkillOptTrainSession
	for rows.Next() {
		session, err := scanSkillOptTrainSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
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

// DeleteSkillOptTrainSession removes a train session and everything keyed by it
// in one transaction: iterations, each iteration's eval run with its review
// items/options and (ranked) feedback events, review watches, and the session's
// resource locks (matched as a whole colon-delimited key segment). It refuses
// while a non-expired lock exists for the session so an in-flight generation or
// optimizer is never pulled out from under its worker. Interactive prompt
// records carry no session linkage (train-init prompts are workspace-scoped and
// deleted by their own flows), so none are touched here.
func (s *Store) DeleteSkillOptTrainSession(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("train session id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM skillopt_train_sessions WHERE id = ?`, sessionID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("train session %q not found", sessionID)
	}

	// Collect lock keys referencing the session as a whole segment, and refuse
	// while any of them is still live.
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	lockRows, err := tx.QueryContext(ctx, `SELECT resource_key, expires_at FROM resource_locks`)
	if err != nil {
		return err
	}
	sessionLockKeys := []string{}
	for lockRows.Next() {
		var key, expiresAt string
		if err := lockRows.Scan(&key, &expiresAt); err != nil {
			lockRows.Close()
			return err
		}
		matched := false
		for _, segment := range strings.Split(key, ":") {
			if segment == sessionID {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if expiresAt > now {
			lockRows.Close()
			return fmt.Errorf("train session %s has an active resource lock (%s); wait for it to finish or recover it first", sessionID, key)
		}
		sessionLockKeys = append(sessionLockKeys, key)
	}
	if err := lockRows.Err(); err != nil {
		lockRows.Close()
		return err
	}
	lockRows.Close()

	runRows, err := tx.QueryContext(ctx, `SELECT eval_run_id FROM skillopt_train_iterations WHERE session_id = ? AND eval_run_id <> ''`, sessionID)
	if err != nil {
		return err
	}
	runIDs := []string{}
	for runRows.Next() {
		var runID string
		if err := runRows.Scan(&runID); err != nil {
			runRows.Close()
			return err
		}
		runIDs = append(runIDs, runID)
	}
	if err := runRows.Err(); err != nil {
		runRows.Close()
		return err
	}
	runRows.Close()

	for _, runID := range runIDs {
		for _, stmt := range []string{
			`DELETE FROM eval_review_items WHERE run_id = ?`,
			`DELETE FROM eval_review_options WHERE run_id = ?`,
			`DELETE FROM feedback_events WHERE run_id = ?`,
			`DELETE FROM ranked_feedback_events WHERE run_id = ?`,
			`DELETE FROM skillopt_review_watches WHERE run_id = ?`,
			`DELETE FROM eval_runs WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, runID); err != nil {
				return err
			}
		}
	}
	for _, key := range sessionLockKeys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_locks WHERE resource_key = ?`, key); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM skillopt_train_iterations WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM skillopt_train_sessions WHERE id = ?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreatedRepo records a GitHub repository gitmoot itself created, so cleanup
// flows can offer deletion of exactly those repos and never others.
type CreatedRepo struct {
	Repo      string `json:"repo"`
	Purpose   string `json:"purpose,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// RecordCreatedRepo remembers that gitmoot created the repo. A repo can only be
// created once, so on conflict the ORIGINAL creator linkage is preserved (a
// later session re-recording the same name must not steal the cleanup offer).
func (s *Store) RecordCreatedRepo(ctx context.Context, record CreatedRepo) error {
	record.Repo = strings.TrimSpace(record.Repo)
	if record.Repo == "" {
		return errors.New("created repo name is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO created_repos(repo, purpose, session_id, created_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo) DO NOTHING`,
		record.Repo, record.Purpose, record.SessionID)
	return err
}

// ListCreatedReposForSession returns the repos gitmoot created for a session.
// AdoptCreatedRepoRecords links repos recorded before a session existed (a
// setup form creates them with an empty session id) to the session, so the
// session-delete cleanup offer includes them. Rows already owned by another
// session are left alone.
func (s *Store) AdoptCreatedRepoRecords(ctx context.Context, sessionID string, repos []string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session id is required")
	}
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE created_repos SET session_id = ? WHERE repo = ? AND TRIM(COALESCE(session_id,'')) = ''`, sessionID, repo); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListCreatedReposForSession(ctx context.Context, sessionID string) ([]CreatedRepo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, purpose, session_id, created_at FROM created_repos WHERE session_id = ? ORDER BY created_at`, strings.TrimSpace(sessionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []CreatedRepo{}
	for rows.Next() {
		var record CreatedRepo
		if err := rows.Scan(&record.Repo, &record.Purpose, &record.SessionID, &record.CreatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// DeleteCreatedRepoRecord forgets a created-repo record (after the repo itself
// was deleted, or to stop offering cleanup for it).
func (s *Store) DeleteCreatedRepoRecord(ctx context.Context, repo string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM created_repos WHERE repo = ?`, strings.TrimSpace(repo))
	return err
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

func (s *Store) UpsertInteractivePrompt(ctx context.Context, prompt InteractivePrompt) error {
	prompt, err := normalizeInteractivePrompt(prompt)
	if err != nil {
		return err
	}
	choicesJSON, err := json.Marshal(prompt.Choices)
	if err != nil {
		return fmt.Errorf("encode prompt choices: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO interactive_prompts(
			id, question, choices_json, default_value, required, answer_format, source_command,
			state, answer_value, answer_source, answered_at, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			question = excluded.question,
			choices_json = excluded.choices_json,
			default_value = excluded.default_value,
			required = excluded.required,
			answer_format = excluded.answer_format,
			source_command = excluded.source_command,
			state = CASE
				WHEN interactive_prompts.state = 'resolved' AND excluded.state = 'pending' THEN interactive_prompts.state
				ELSE excluded.state
			END,
			answer_value = CASE
				WHEN interactive_prompts.state = 'resolved' AND excluded.state = 'pending' THEN interactive_prompts.answer_value
				ELSE excluded.answer_value
			END,
			answer_source = CASE
				WHEN interactive_prompts.state = 'resolved' AND excluded.state = 'pending' THEN interactive_prompts.answer_source
				ELSE excluded.answer_source
			END,
			answered_at = CASE
				WHEN interactive_prompts.state = 'resolved' AND excluded.state = 'pending' THEN interactive_prompts.answered_at
				ELSE excluded.answered_at
			END,
			updated_at = CURRENT_TIMESTAMP`,
		prompt.ID, prompt.Question, string(choicesJSON), prompt.Default, boolInt(prompt.Required), prompt.AnswerFormat, prompt.SourceCommand,
		prompt.State, prompt.AnswerValue, prompt.AnswerSource, prompt.AnsweredAt)
	return err
}

func normalizeInteractivePrompt(prompt InteractivePrompt) (InteractivePrompt, error) {
	prompt.ID = strings.TrimSpace(prompt.ID)
	if prompt.ID == "" {
		return InteractivePrompt{}, errors.New("interactive prompt id is required")
	}
	prompt.Question = strings.TrimSpace(prompt.Question)
	if prompt.Question == "" {
		return InteractivePrompt{}, errors.New("interactive prompt question is required")
	}
	prompt.Choices = trimUniqueStrings(prompt.Choices)
	prompt.Default = strings.TrimSpace(prompt.Default)
	if prompt.Default != "" && len(prompt.Choices) > 0 && !stringInSlice(prompt.Default, prompt.Choices) {
		return InteractivePrompt{}, fmt.Errorf("interactive prompt default %q is not one of the allowed choices", prompt.Default)
	}
	prompt.AnswerFormat = strings.TrimSpace(prompt.AnswerFormat)
	if prompt.AnswerFormat == "" {
		if len(prompt.Choices) > 0 {
			prompt.AnswerFormat = "choice"
		} else {
			prompt.AnswerFormat = "text"
		}
	}
	prompt.SourceCommand = strings.TrimSpace(prompt.SourceCommand)
	prompt.State = strings.TrimSpace(strings.ToLower(prompt.State))
	if prompt.State == "" {
		prompt.State = InteractivePromptStatePending
	}
	switch prompt.State {
	case InteractivePromptStatePending, InteractivePromptStateResolved:
	default:
		return InteractivePrompt{}, fmt.Errorf("interactive prompt state %q is not supported", prompt.State)
	}
	prompt.AnswerValue = strings.TrimSpace(prompt.AnswerValue)
	prompt.AnswerSource = strings.TrimSpace(prompt.AnswerSource)
	prompt.AnsweredAt = strings.TrimSpace(prompt.AnsweredAt)
	if prompt.State == InteractivePromptStateResolved {
		value, err := validateInteractivePromptAnswer(prompt, prompt.AnswerValue)
		if err != nil {
			return InteractivePrompt{}, err
		}
		prompt.AnswerValue = value
		if prompt.AnswerSource == "" {
			prompt.AnswerSource = "unknown"
		}
		if prompt.AnsweredAt == "" {
			prompt.AnsweredAt = time.Now().UTC().Format(time.RFC3339)
		}
	}
	return prompt, nil
}

func (s *Store) ListInteractivePrompts(ctx context.Context, state string) ([]InteractivePrompt, error) {
	state = strings.TrimSpace(strings.ToLower(state))
	var rows *sql.Rows
	var err error
	if state == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT id, question, choices_json, default_value, required, answer_format,
				source_command, state, answer_value, answer_source, created_at, updated_at, answered_at
			FROM interactive_prompts
			ORDER BY created_at, id`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT id, question, choices_json, default_value, required, answer_format,
				source_command, state, answer_value, answer_source, created_at, updated_at, answered_at
			FROM interactive_prompts
			WHERE state = ?
			ORDER BY created_at, id`, state)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prompts := []InteractivePrompt{}
	for rows.Next() {
		prompt, err := scanInteractivePrompt(rows)
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, prompt)
	}
	return prompts, rows.Err()
}

func (s *Store) GetInteractivePrompt(ctx context.Context, id string) (InteractivePrompt, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, question, choices_json, default_value, required, answer_format,
			source_command, state, answer_value, answer_source, created_at, updated_at, answered_at
		FROM interactive_prompts
		WHERE id = ?`, strings.TrimSpace(id))
	return scanInteractivePrompt(row)
}

func (s *Store) AnswerInteractivePrompt(ctx context.Context, id string, value string, source string) (InteractivePrompt, error) {
	prompt, err := s.GetInteractivePrompt(ctx, id)
	if err != nil {
		return InteractivePrompt{}, err
	}
	if prompt.State == InteractivePromptStateResolved {
		return InteractivePrompt{}, fmt.Errorf("interactive prompt %s is already resolved", prompt.ID)
	}
	value, err = validateInteractivePromptAnswer(prompt, value)
	if err != nil {
		return InteractivePrompt{}, err
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "cli"
	}
	answeredAt := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx, `UPDATE interactive_prompts
		SET state = ?, answer_value = ?, answer_source = ?, answered_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND state = ?`,
		InteractivePromptStateResolved, value, source, answeredAt, prompt.ID, InteractivePromptStatePending)
	if err != nil {
		return InteractivePrompt{}, err
	}
	if err := requireAffected(result, "interactive prompt", prompt.ID); err != nil {
		return InteractivePrompt{}, err
	}
	return s.GetInteractivePrompt(ctx, prompt.ID)
}

// ErrInteractivePromptNotFound is returned by DeleteInteractivePrompt when no
// prompt with the given id exists. Callers performing best-effort cleanup of a
// prompt that may already be gone can ignore it via errors.Is.
var ErrInteractivePromptNotFound = errors.New("interactive prompt not found")

func (s *Store) DeleteInteractivePrompt(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("interactive prompt id is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM interactive_prompts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("interactive prompt %q: %w", id, ErrInteractivePromptNotFound)
	}
	return nil
}

// DeleteInteractivePromptsByState deletes every prompt in the given state and
// returns the number removed. An empty state deletes all prompts regardless of
// state.
func (s *Store) DeleteInteractivePromptsByState(ctx context.Context, state string) (int64, error) {
	var result sql.Result
	var err error
	if state == "" {
		result, err = s.db.ExecContext(ctx, `DELETE FROM interactive_prompts`)
	} else {
		result, err = s.db.ExecContext(ctx, `DELETE FROM interactive_prompts WHERE state = ?`, state)
	}
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func validateInteractivePromptAnswer(prompt InteractivePrompt, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" && strings.TrimSpace(prompt.Default) != "" {
		value = strings.TrimSpace(prompt.Default)
	}
	if value == "" && prompt.Required {
		return "", fmt.Errorf("interactive prompt %s requires an answer", prompt.ID)
	}
	if value != "" && len(prompt.Choices) > 0 && !stringInSlice(value, prompt.Choices) {
		return "", fmt.Errorf("interactive prompt %s answer %q is not one of: %s", prompt.ID, value, strings.Join(prompt.Choices, ", "))
	}
	return value, nil
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

// normalizeEvalReviewGenerationWrite validates and canonicalizes a single
// generation write, scoping its review item and options to runID, normalizing
// each artifact/option, and rejecting duplicate option labels for the item.
func normalizeEvalReviewGenerationWrite(runID string, write EvalReviewGenerationWrite) (EvalReviewGenerationWrite, error) {
	itemID := strings.TrimSpace(write.ItemID)
	if itemID == "" && write.ReviewItem != nil {
		itemID = strings.TrimSpace(write.ReviewItem.ItemID)
	}
	if itemID == "" {
		return EvalReviewGenerationWrite{}, errors.New("eval review generation item id is required")
	}
	next := EvalReviewGenerationWrite{ItemID: itemID}
	for _, artifact := range write.Artifacts {
		artifact, err := normalizeEvalArtifact(artifact)
		if err != nil {
			return EvalReviewGenerationWrite{}, err
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
			return EvalReviewGenerationWrite{}, errors.New("eval review item run id is required")
		}
		if strings.TrimSpace(item.ItemID) == "" {
			return EvalReviewGenerationWrite{}, errors.New("eval review item id is required")
		}
		next.ReviewItem = &item
	}
	seen := map[string]struct{}{}
	for _, option := range write.Options {
		option.RunID = runID
		option.ItemID = itemID
		option, err := normalizeEvalReviewOption(option)
		if err != nil {
			return EvalReviewGenerationWrite{}, err
		}
		if _, ok := seen[option.Label]; ok {
			return EvalReviewGenerationWrite{}, fmt.Errorf("eval review option label %q is duplicated for item %q", option.Label, itemID)
		}
		seen[option.Label] = struct{}{}
		next.Options = append(next.Options, option)
	}
	return next, nil
}

// writeGeneratedEvalReviewArtifactsTx persists one normalized generation write
// inside tx: upserts artifacts, upserts the review item, and replaces the item's
// options (DELETE-then-INSERT scoped to run_id/item_id). The caller owns the
// transaction lifecycle. The write must already be normalized.
func writeGeneratedEvalReviewArtifactsTx(ctx context.Context, tx *sql.Tx, runID string, write EvalReviewGenerationWrite) error {
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
		return nil
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
	return nil
}

func (s *Store) ReplaceGeneratedEvalReviewArtifacts(ctx context.Context, runID string, writes []EvalReviewGenerationWrite) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("eval review generation run id is required")
	}
	normalized := make([]EvalReviewGenerationWrite, 0, len(writes))
	for _, write := range writes {
		next, err := normalizeEvalReviewGenerationWrite(runID, write)
		if err != nil {
			return err
		}
		normalized = append(normalized, next)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, write := range normalized {
		if err := writeGeneratedEvalReviewArtifactsTx(ctx, tx, runID, write); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceGeneratedEvalReviewArtifactsForItem atomically persists the generated
// artifacts, review item, and options for a single item in one transaction so a
// completed item is durable on its own (artifacts + item row + options commit
// together). Options are replaced DELETE-then-INSERT scoped to (run_id,item_id),
// so re-writing the same item is idempotent and writing one item leaves others
// untouched.
func (s *Store) ReplaceGeneratedEvalReviewArtifactsForItem(ctx context.Context, runID string, write EvalReviewGenerationWrite) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("eval review generation run id is required")
	}
	normalized, err := normalizeEvalReviewGenerationWrite(runID, write)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeGeneratedEvalReviewArtifactsTx(ctx, tx, runID, normalized); err != nil {
		return err
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

func scanInteractivePrompt(row interface{ Scan(dest ...any) error }) (InteractivePrompt, error) {
	var prompt InteractivePrompt
	var choicesJSON string
	var required int
	if err := row.Scan(&prompt.ID, &prompt.Question, &choicesJSON, &prompt.Default, &required, &prompt.AnswerFormat,
		&prompt.SourceCommand, &prompt.State, &prompt.AnswerValue, &prompt.AnswerSource, &prompt.CreatedAt, &prompt.UpdatedAt, &prompt.AnsweredAt); err != nil {
		return InteractivePrompt{}, err
	}
	if strings.TrimSpace(choicesJSON) != "" {
		if err := json.Unmarshal([]byte(choicesJSON), &prompt.Choices); err != nil {
			return InteractivePrompt{}, fmt.Errorf("decode interactive prompt choices: %w", err)
		}
	}
	prompt.Choices = trimUniqueStrings(prompt.Choices)
	prompt.Required = required != 0
	return prompt, nil
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

func trimUniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	output := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		output = append(output, value)
	}
	return output
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
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
			id, run_id, item_id, ranking_json, tie_groups_json, winner, useful_traits_json, rejected_traits_json,
			required_improvements_json, quality, continue_mode, promote, reasoning, reviewer, source, source_url, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, item_id, reviewer, source, source_url) DO UPDATE SET
			id = excluded.id,
			ranking_json = excluded.ranking_json,
			tie_groups_json = excluded.tie_groups_json,
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
		event.ID, event.RunID, event.ItemID, event.RankingJSON, event.TieGroupsJSON, event.Winner, event.UsefulTraitsJSON, event.RejectedTraitsJSON, event.RequiredImprovementsJSON,
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
	tieGroups, err := rankedFeedbackTieGroups(event)
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
		if len(tieGroups) == 0 || !stringSliceContains(tieGroups[0], event.Winner) {
			return fmt.Errorf("ranked feedback item %s winner %q is not in first ranked group", event.ItemID, event.Winner)
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
	tieGroups, err := rankedFeedbackTieGroups(event)
	if err != nil {
		return RankedFeedbackEvent{}, err
	}
	if strings.TrimSpace(event.TieGroupsJSON) != "" {
		encoded, err := json.Marshal(tieGroups)
		if err != nil {
			return RankedFeedbackEvent{}, err
		}
		event.TieGroupsJSON = string(encoded)
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

func rankedFeedbackTieGroups(event RankedFeedbackEvent) ([][]string, error) {
	ranking, err := rankedFeedbackRanking(event)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(event.TieGroupsJSON) == "" {
		groups := make([][]string, 0, len(ranking))
		for _, label := range ranking {
			groups = append(groups, []string{label})
		}
		return groups, nil
	}
	var groups [][]string
	if err := json.Unmarshal([]byte(event.TieGroupsJSON), &groups); err != nil {
		return nil, fmt.Errorf("ranked feedback tie_groups_json must be a JSON array of option label arrays: %w", err)
	}
	flattened := make([]string, 0, len(ranking))
	seen := map[string]struct{}{}
	for groupIndex, group := range groups {
		if len(group) == 0 {
			return nil, fmt.Errorf("ranked feedback tie group %d is empty", groupIndex+1)
		}
		for labelIndex, label := range group {
			normalized := normalizeOptionLabel(label)
			if normalized == "" {
				return nil, fmt.Errorf("ranked feedback tie group %d contains empty option label at position %d", groupIndex+1, labelIndex+1)
			}
			if _, ok := seen[normalized]; ok {
				return nil, fmt.Errorf("ranked feedback tie groups contain duplicate option label %q", normalized)
			}
			seen[normalized] = struct{}{}
			groups[groupIndex][labelIndex] = normalized
			flattened = append(flattened, normalized)
		}
	}
	if len(flattened) != len(ranking) {
		return nil, fmt.Errorf("ranked feedback tie groups include %d options, want %d ranking options", len(flattened), len(ranking))
	}
	for index, label := range ranking {
		if flattened[index] != label {
			return nil, fmt.Errorf("ranked feedback tie groups do not match ranking order at position %d", index+1)
		}
	}
	return groups, nil
}

func (s *Store) ListRankedFeedbackEvents(ctx context.Context, runID string) ([]RankedFeedbackEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, item_id, ranking_json, tie_groups_json, winner, useful_traits_json, rejected_traits_json,
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
	if err := row.Scan(&event.ID, &event.RunID, &event.ItemID, &event.RankingJSON, &event.TieGroupsJSON, &event.Winner, &event.UsefulTraitsJSON, &event.RejectedTraitsJSON,
		&event.RequiredImprovementsJSON, &event.Quality, &event.ContinueMode, &event.Promote, &event.Reasoning, &event.Reviewer, &event.Source, &event.SourceURL, &event.CreatedAt); err != nil {
		return RankedFeedbackEvent{}, err
	}
	return event, nil
}

// GetBanditArm reads the #473 Mode B bandit arm for a (templateID,
// versionID) variant. A missing row is NOT an error: it returns ok=false with a
// zero arm, and the caller treats that as the uniform Beta(1,1) prior
// (skillopt.NewArm). This keeps the table off-by-default — no row exists until
// the manual A/B records its first pick.
func (s *Store) GetBanditArm(ctx context.Context, templateID, versionID string) (BanditArm, bool, error) {
	templateID = strings.TrimSpace(templateID)
	versionID = strings.TrimSpace(versionID)
	if versionID == "" {
		return BanditArm{}, false, errors.New("bandit arm version id is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT template_id, template_version_id, alpha, beta, pulls, updated_at
		FROM skillopt_bandit_arms WHERE template_id = ? AND template_version_id = ?`, templateID, versionID)
	var arm BanditArm
	if err := row.Scan(&arm.TemplateID, &arm.TemplateVersionID, &arm.Alpha, &arm.Beta, &arm.Pulls, &arm.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BanditArm{}, false, nil
		}
		return BanditArm{}, false, err
	}
	return arm, true, nil
}

// UpsertBanditArm writes (or replaces) a bandit arm's full posterior. The alpha/
// beta/pulls are the caller's authoritative counters; this never increments, it
// stores exactly what it is given.
func (s *Store) UpsertBanditArm(ctx context.Context, arm BanditArm) error {
	arm.TemplateVersionID = strings.TrimSpace(arm.TemplateVersionID)
	if arm.TemplateVersionID == "" {
		return errors.New("bandit arm version id is required")
	}
	arm.TemplateID = strings.TrimSpace(arm.TemplateID)
	if arm.Alpha <= 0 {
		arm.Alpha = 1
	}
	if arm.Beta <= 0 {
		arm.Beta = 1
	}
	if arm.Pulls < 0 {
		arm.Pulls = 0
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO skillopt_bandit_arms(template_id, template_version_id, alpha, beta, pulls, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(template_id, template_version_id) DO UPDATE SET
			alpha = excluded.alpha,
			beta = excluded.beta,
			pulls = excluded.pulls,
			updated_at = CURRENT_TIMESTAMP`,
		arm.TemplateID, arm.TemplateVersionID, arm.Alpha, arm.Beta, arm.Pulls)
	return err
}

// HeartbeatState is the persisted state of one named agent heartbeat schedule
// (#533): when it last ran, when it is next due, and the id/status of its most
// recent job. The next_due + last_status are what make a heartbeat restart-safe
// (a restart re-reads next_due rather than firing immediately).
type HeartbeatState struct {
	Agent      string
	Name       string
	LastRunAt  time.Time
	NextDueAt  time.Time
	LastJobID  string
	LastStatus string
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

// CountActiveJobsByFingerprint counts queued/running jobs whose stored payload
// carries the given fingerprint — the heartbeat overlap guard (#533), a cousin of
// countActiveJobsTx that filters on the payload fingerprint instead of the agent
// column. The fingerprint lives in the JSON payload, and modernc's json_extract
// raises a SQL error on a malformed payload (see backfillJobRootID), so this
// counts Go-side: it scans the small set of active rows and tolerates a malformed
// payload (skipped) rather than aborting. An empty fingerprint matches nothing.
func (s *Store) CountActiveJobsByFingerprint(ctx context.Context, fingerprint string) (int, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM jobs WHERE state IN ('queued', 'running')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return 0, err
		}
		var probe struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			continue
		}
		if strings.TrimSpace(probe.Fingerprint) == fingerprint {
			count++
		}
	}
	return count, rows.Err()
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

// IncrementBanditArm atomically applies ONE pairwise outcome to a (templateID,
// versionID) arm in a single transaction: a win bumps alpha, a loss bumps beta,
// and either way pulls increments. A first-ever pull seeds the row from the
// Beta(1,1) prior. It returns the post-update arm so the caller can recompute
// P(challenger>champion) immediately. The two arms of one A/B are incremented
// with two calls (winner=+win, loser=+loss).
func (s *Store) IncrementBanditArm(ctx context.Context, templateID, versionID string, win bool) (BanditArm, error) {
	templateID = strings.TrimSpace(templateID)
	versionID = strings.TrimSpace(versionID)
	if versionID == "" {
		return BanditArm{}, errors.New("bandit arm version id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BanditArm{}, err
	}
	defer tx.Rollback()

	arm := BanditArm{TemplateID: templateID, TemplateVersionID: versionID, Alpha: 1, Beta: 1, Pulls: 0}
	row := tx.QueryRowContext(ctx, `SELECT template_id, alpha, beta, pulls FROM skillopt_bandit_arms
		WHERE template_id = ? AND template_version_id = ?`, templateID, versionID)
	if err := row.Scan(&arm.TemplateID, &arm.Alpha, &arm.Beta, &arm.Pulls); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return BanditArm{}, err
		}
		// No row yet: keep the Beta(1,1) prior seeded above.
		arm.TemplateID = templateID
	}
	if win {
		arm.Alpha++
	} else {
		arm.Beta++
	}
	arm.Pulls++
	if _, err := tx.ExecContext(ctx, `INSERT INTO skillopt_bandit_arms(template_id, template_version_id, alpha, beta, pulls, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(template_id, template_version_id) DO UPDATE SET
			alpha = excluded.alpha,
			beta = excluded.beta,
			pulls = excluded.pulls,
			updated_at = CURRENT_TIMESTAMP`,
		arm.TemplateID, arm.TemplateVersionID, arm.Alpha, arm.Beta, arm.Pulls); err != nil {
		return BanditArm{}, err
	}
	if err := tx.Commit(); err != nil {
		return BanditArm{}, err
	}
	return arm, nil
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
	tieGroups, err := rankedFeedbackTieGroups(event)
	if err != nil {
		return nil, err
	}
	preferences := []PairwisePreference{}
	for preferredGroupIndex, preferredGroup := range tieGroups {
		for _, rejectedGroup := range tieGroups[preferredGroupIndex+1:] {
			for _, preferred := range preferredGroup {
				for _, rejected := range rejectedGroup {
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
		}
	}
	return preferences, nil
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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

// ListResourceLocks returns all held resource locks, ordered by resource key.
func (s *Store) ListResourceLocks(ctx context.Context) ([]ResourceLock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, command_hash, acquired_at, updated_at, expires_at FROM resource_locks ORDER BY resource_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locks []ResourceLock
	for rows.Next() {
		var lock ResourceLock
		if err := rows.Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken, &lock.OwnerPID, &lock.OwnerHostname, &lock.CommandHash, &lock.AcquiredAt, &lock.UpdatedAt, &lock.ExpiresAt); err != nil {
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, rows.Err()
}

func (s *Store) ListExpiredRuntimeSessionLocks(ctx context.Context, now time.Time) ([]ResourceLock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, command_hash, acquired_at, updated_at, expires_at
		FROM resource_locks
		WHERE resource_key LIKE ? AND expires_at <= ?
		ORDER BY resource_key`, RuntimeSessionLockKeyPrefix+"%", formatResourceLockTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locks []ResourceLock
	for rows.Next() {
		var lock ResourceLock
		if err := rows.Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken, &lock.OwnerPID, &lock.OwnerHostname, &lock.CommandHash, &lock.AcquiredAt, &lock.UpdatedAt, &lock.ExpiresAt); err != nil {
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, rows.Err()
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

// DeleteResourceLocksByOwner releases every resource lock held by ownerJobID,
// regardless of token/expiry — used when a job is cancelled and can no longer
// renew its locks. Returns the number released.
func (s *Store) DeleteResourceLocksByOwner(ctx context.Context, ownerJobID string) (int64, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks WHERE owner_job_id = ?`, ownerJobID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteResourceLocksByOwnerIfNotRunning releases an owner's resource locks
// only while that owner job is not currently running, evaluated atomically in
// the DELETE itself. This mirrors DeleteExpiredResourceLocks's
// `NOT EXISTS (... jobs.state='running')` guard and closes the TOCTOU race in
// the delegation-kill cleanup path (#479): a child that raced queued->running
// after a stale snapshot was read keeps its live runtime-session / checkout
// lock instead of having it deleted out from under its in-flight process.
func (s *Store) DeleteResourceLocksByOwnerIfNotRunning(ctx context.Context, ownerJobID string) (int64, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE owner_job_id = ?
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			)`, ownerJobID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteExpiredResourceLocks reaps lock rows whose lease has elapsed.
//
// The owner_pid<=0 clause keeps the historical conservatism for NON-runtime locks
// (e.g. skillopt-train-generation): a lock that records a live-process owner PID is
// reclaimed by a PID-liveness check elsewhere, not by blind expiry. Runtime-session
// locks (resource_key LIKE 'runtime:%') are the explicit exception: their recorded
// PID is the gitmoot DAEMON's, not the spawned worker's, so it is meaningless after
// a daemon restart and must NOT keep an expired lease alive forever. Without this
// exception an expired runtime-session lock (which always sets owner_pid>0) would
// NEVER be reaped here — stranding the job's recovery and worktree cleanup (#536
// finding 2). Once a runtime lease expires, the daemon may requeue the running
// owner job from the same recovery tick.
func (s *Store) DeleteExpiredResourceLocks(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE expires_at <= ?
			AND (owner_pid <= 0 OR resource_key LIKE 'runtime:%')
			AND (resource_key LIKE 'runtime:%' OR NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			))`, formatResourceLockTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteExpiredResourceLocksExcludingOwners is DeleteExpiredResourceLocks minus
// locks held by the given owner job IDs (#562): the daemon's recovery tick skips
// reaping a lock whose owner job is in flight in this very process (its worker
// goroutine is alive — e.g. a ctx-deaf runtime overrunning its lease), because
// deleting it would let a second run of the same session start beside the live
// one. An empty exclusion list is byte-identical to DeleteExpiredResourceLocks.
func (s *Store) DeleteExpiredResourceLocksExcludingOwners(ctx context.Context, now time.Time, excludeOwners []string) (int64, error) {
	if len(excludeOwners) == 0 {
		return s.DeleteExpiredResourceLocks(ctx, now)
	}
	placeholders := strings.Repeat("?,", len(excludeOwners))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(excludeOwners)+1)
	args = append(args, formatResourceLockTime(now))
	for _, owner := range excludeOwners {
		args = append(args, owner)
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE expires_at <= ?
			AND (owner_pid <= 0 OR resource_key LIKE 'runtime:%')
			AND (resource_key LIKE 'runtime:%' OR NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			))
			AND owner_job_id NOT IN (`+placeholders+`)`, args...)
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
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, branch, owner, skip_native_review_fanout FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, repoFullName, branch)
	var lock BranchLock
	if err := row.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner, &lock.SkipNativeReviewFanout); err != nil {
		return BranchLock{}, err
	}
	return lock, nil
}

func (s *Store) ListBranchLocks(ctx context.Context, repoFullName string) ([]BranchLock, error) {
	query := `SELECT repo_full_name, branch, owner, skip_native_review_fanout FROM branch_locks`
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
		if err := rows.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner, &lock.SkipNativeReviewFanout); err != nil {
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, rows.Err()
}

// SetBranchLockReviewFanout persists the skip_native_review_fanout flag onto the
// branch lock for (repoFullName, branch). It is a no-op when no lock exists for
// the pair. The flag is never written at lock creation (CreateLock defaults it to
// 0); only the implement-job advancement path sets it so the daemon's PR-watcher
// can read whether the native review fanout should be skipped.
func (s *Store) SetBranchLockReviewFanout(ctx context.Context, repoFullName string, branch string, skip bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE branch_locks SET skip_native_review_fanout = ?, updated_at = CURRENT_TIMESTAMP WHERE repo_full_name = ? AND branch = ?`, skip, repoFullName, branch)
	return err
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
	if err := scanner.Scan(&agent.Name, &agent.Role, &agent.Runtime, &agent.RuntimeRef, &agent.RepoScope, &agent.TemplateID, &agent.Model, &capabilities, &agent.AutonomyPolicy, &agent.HealthStatus); err != nil {
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
	if err := scanner.Scan(&version.ID, &version.TemplateID, &version.VersionNumber, &version.State, &version.Name, &version.Description, &version.SourceRepo, &version.SourceRef, &version.SourcePath, &version.ResolvedCommit, &version.ContentHash, &version.Content, &version.MetadataJSON, &version.CreatedAt, &version.UpdatedAt, &version.PromotedAt, &version.CanarySample, &version.CanaryStartedAt); err != nil {
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

// SkillOpt judge-outcome direction buckets. The four directions are the cells
// of the human-vs-judge confusion matrix.
const (
	// SkillOptJudgeDirectionAgreeAccept: human promoted and judge accepted.
	SkillOptJudgeDirectionAgreeAccept = "agree_accept"
	// SkillOptJudgeDirectionAgreeReject: human rejected and judge rejected.
	SkillOptJudgeDirectionAgreeReject = "agree_reject"
	// SkillOptJudgeDirectionJudgeAcceptHumanReject: judge accepted but the human
	// rejected (a judge false positive relative to the human).
	SkillOptJudgeDirectionJudgeAcceptHumanReject = "judge_accept_human_reject"
	// SkillOptJudgeDirectionJudgeRejectHumanAccept: judge rejected but the human
	// promoted (a judge false negative relative to the human).
	SkillOptJudgeDirectionJudgeRejectHumanAccept = "judge_reject_human_accept"
)

func (s *Store) InsertSkillOptJudgeOutcome(ctx context.Context, outcome SkillOptJudgeOutcome) error {
	id := strings.TrimSpace(outcome.ID)
	if id == "" {
		generated, err := newSkillOptJudgeOutcomeID()
		if err != nil {
			return err
		}
		id = generated
	}
	if strings.TrimSpace(outcome.CandidateVersionID) == "" {
		return errors.New("judge outcome candidate_version_id is required")
	}
	if strings.TrimSpace(outcome.HumanDecision) == "" {
		return errors.New("judge outcome human_decision is required")
	}
	if strings.TrimSpace(outcome.Direction) == "" {
		return errors.New("judge outcome direction is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO skillopt_judge_outcomes(
			id, candidate_version_id, template_id, judge_score_json, judge_prompt_version,
			judge_evaluator_id, judge_prompt_hash, human_decision, direction, reason
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		strings.TrimSpace(outcome.CandidateVersionID),
		strings.TrimSpace(outcome.TemplateID),
		strings.TrimSpace(outcome.JudgeScoreJSON),
		strings.TrimSpace(outcome.JudgePromptVersion),
		strings.TrimSpace(outcome.JudgeEvaluatorID),
		strings.TrimSpace(outcome.JudgePromptHash),
		strings.TrimSpace(outcome.HumanDecision),
		strings.TrimSpace(outcome.Direction),
		strings.TrimSpace(outcome.Reason))
	return err
}

func (s *Store) ListSkillOptJudgeOutcomes(ctx context.Context, templateID string) ([]SkillOptJudgeOutcome, error) {
	query := `SELECT id, candidate_version_id, template_id, judge_score_json, judge_prompt_version,
			judge_evaluator_id, judge_prompt_hash, human_decision, direction, reason, created_at
		FROM skillopt_judge_outcomes`
	var (
		rows *sql.Rows
		err  error
	)
	if templateID = strings.TrimSpace(templateID); templateID != "" {
		query += ` WHERE template_id = ? ORDER BY created_at, id`
		rows, err = s.db.QueryContext(ctx, query, templateID)
	} else {
		query += ` ORDER BY created_at, id`
		rows, err = s.db.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var outcomes []SkillOptJudgeOutcome
	for rows.Next() {
		outcome, err := scanSkillOptJudgeOutcome(rows)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, rows.Err()
}

func (s *Store) GetSkillOptJudgeOutcome(ctx context.Context, id string) (SkillOptJudgeOutcome, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, candidate_version_id, template_id, judge_score_json, judge_prompt_version,
			judge_evaluator_id, judge_prompt_hash, human_decision, direction, reason, created_at
		FROM skillopt_judge_outcomes WHERE id = ?`, strings.TrimSpace(id))
	return scanSkillOptJudgeOutcome(row)
}

func scanSkillOptJudgeOutcome(row interface{ Scan(dest ...any) error }) (SkillOptJudgeOutcome, error) {
	var outcome SkillOptJudgeOutcome
	if err := row.Scan(&outcome.ID, &outcome.CandidateVersionID, &outcome.TemplateID, &outcome.JudgeScoreJSON,
		&outcome.JudgePromptVersion, &outcome.JudgeEvaluatorID, &outcome.JudgePromptHash, &outcome.HumanDecision,
		&outcome.Direction, &outcome.Reason, &outcome.CreatedAt); err != nil {
		return SkillOptJudgeOutcome{}, err
	}
	return outcome, nil
}

func newSkillOptJudgeOutcomeID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "judge-outcome-" + hex.EncodeToString(raw[:]), nil
}

// InsertCockpitPane records a live Herdr pane for a delegation subagent's job
// (issue #357). An empty ID is auto-generated. The (workspace_id, pane_key)
// pair is UNIQUE, so a duplicate open for the same seat surfaces as an error the
// cockpit can treat as "pane already exists, reuse it".
func (s *Store) InsertCockpitPane(ctx context.Context, pane CockpitPane) error {
	id := strings.TrimSpace(pane.ID)
	if id == "" {
		generated, err := newCockpitPaneID()
		if err != nil {
			return err
		}
		id = generated
	}
	if strings.TrimSpace(pane.JobID) == "" {
		return errors.New("cockpit pane job_id is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO cockpit_panes(
			id, job_id, pane_key, root_job_id, pane_id, workspace_id, source
		)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id,
		strings.TrimSpace(pane.JobID),
		strings.TrimSpace(pane.PaneKey),
		strings.TrimSpace(pane.RootJobID),
		strings.TrimSpace(pane.PaneID),
		strings.TrimSpace(pane.WorkspaceID),
		strings.TrimSpace(pane.Source))
	return err
}

// GetCockpitPaneByJob returns the pane recorded for a job, if any.
func (s *Store) GetCockpitPaneByJob(ctx context.Context, jobID string) (CockpitPane, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, job_id, pane_key, root_job_id, pane_id, workspace_id, source, created_at
		FROM cockpit_panes WHERE job_id = ?`, strings.TrimSpace(jobID))
	return scanCockpitPane(row)
}

// GetCockpitPaneByKey returns the live pane for a (workspace_id, pane_key) seat,
// if one exists (issue #357, seat mode). The bool reports found; a not-found row
// is (CockpitPane{}, false, nil) — sql.ErrNoRows is never surfaced — so the
// seat-reuse fail-open path can treat "no pane yet" as a clean miss rather than an
// error. The (workspace_id, pane_key) pair is UNIQUE, so at most one row matches.
func (s *Store) GetCockpitPaneByKey(ctx context.Context, workspaceID, paneKey string) (CockpitPane, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, job_id, pane_key, root_job_id, pane_id, workspace_id, source, created_at
		FROM cockpit_panes WHERE workspace_id = ? AND pane_key = ?`,
		strings.TrimSpace(workspaceID), strings.TrimSpace(paneKey))
	pane, err := scanCockpitPane(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CockpitPane{}, false, nil
	}
	if err != nil {
		return CockpitPane{}, false, err
	}
	return pane, true, nil
}

// ListCockpitPanesByRoot returns every pane opened under one orchestration root,
// oldest first, so the cockpit can tear them all down on root finalize.
func (s *Store) ListCockpitPanesByRoot(ctx context.Context, rootJobID string) ([]CockpitPane, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, pane_key, root_job_id, pane_id, workspace_id, source, created_at
		FROM cockpit_panes WHERE root_job_id = ? ORDER BY created_at, rowid`, strings.TrimSpace(rootJobID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var panes []CockpitPane
	for rows.Next() {
		pane, err := scanCockpitPane(rows)
		if err != nil {
			return nil, err
		}
		panes = append(panes, pane)
	}
	return panes, rows.Err()
}

// ListAllCockpitPanes returns every recorded pane across all roots, oldest first.
// The reconcile GC uses it to find orphaned rows (pane gone from herdr + owning
// root terminal) without scanning per-root.
func (s *Store) ListAllCockpitPanes(ctx context.Context) ([]CockpitPane, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, pane_key, root_job_id, pane_id, workspace_id, source, created_at
		FROM cockpit_panes ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var panes []CockpitPane
	for rows.Next() {
		pane, err := scanCockpitPane(rows)
		if err != nil {
			return nil, err
		}
		panes = append(panes, pane)
	}
	return panes, rows.Err()
}

// DeleteCockpitPane removes a pane record by ID. Deleting a missing row is a
// no-op (best-effort teardown should not fail on a stale record). It stays
// available for reconcile/GC, which addresses panes by their generated id.
func (s *Store) DeleteCockpitPane(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cockpit_panes WHERE id = ?`, strings.TrimSpace(id))
	return err
}

// DeleteCockpitPaneByJob removes the pane record for a job. The cockpit opens a
// pane without knowing its generated primary key, so teardown deletes by job_id;
// this also lets a re-run of the same job reclaim its (workspace_id, pane_key)
// slot. Deleting a missing row is a no-op.
func (s *Store) DeleteCockpitPaneByJob(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cockpit_panes WHERE job_id = ?`, strings.TrimSpace(jobID))
	return err
}

// GetOrCreateWorkspaceForRoot returns the single Herdr workspace id bound to an
// orchestration root, creating it via create exactly once. Concurrent callers
// for the same root serialize on the cockpit_workspaces primary key: the first
// inserter wins and create runs once; losers read the winner's id back without
// calling create. create is invoked outside the row insert (it shells out to
// herdr), and a racing insert that loses simply re-reads the committed id.
// create returns BOTH the new workspace id and the id of its root pane: herdr's
// `pane split` requires a PANE id as the split parent (the workspace id is not a
// valid target), so the root pane id is persisted alongside the workspace and
// returned on every reuse so subsequent children split off it.
func (s *Store) GetOrCreateWorkspaceForRoot(ctx context.Context, rootJobID string, create func() (workspaceID string, rootPaneID string, err error)) (workspaceID string, rootPaneID string, err error) {
	rootJobID = strings.TrimSpace(rootJobID)
	if rootJobID == "" {
		return "", "", errors.New("cockpit workspace root_job_id is required")
	}
	if create == nil {
		return "", "", errors.New("cockpit workspace create func is required")
	}
	// Fast path: an existing registration short-circuits without calling create.
	if existingWS, existingRP, err := s.lookupWorkspaceForRoot(ctx, rootJobID); err != nil {
		return "", "", err
	} else if existingWS != "" {
		return existingWS, existingRP, nil
	}
	workspaceID, rootPaneID, err = create()
	if err != nil {
		return "", "", err
	}
	workspaceID = strings.TrimSpace(workspaceID)
	rootPaneID = strings.TrimSpace(rootPaneID)
	if workspaceID == "" {
		return "", "", errors.New("cockpit workspace create returned an empty workspace id")
	}
	// INSERT OR IGNORE: if a concurrent caller already bound this root, our row
	// is dropped and we fall through to re-read the winning id (our freshly
	// created workspace is then orphaned, which the cockpit reaper handles).
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO cockpit_workspaces(root_job_id, workspace_id, root_pane_id) VALUES (?, ?, ?)`,
		rootJobID, workspaceID, rootPaneID); err != nil {
		return "", "", err
	}
	storedWS, storedRP, err := s.lookupWorkspaceForRoot(ctx, rootJobID)
	if err != nil {
		return "", "", err
	}
	if storedWS == "" {
		// Should not happen: we just inserted-or-ignored. Treat as our ids.
		return workspaceID, rootPaneID, nil
	}
	return storedWS, storedRP, nil
}

func (s *Store) lookupWorkspaceForRoot(ctx context.Context, rootJobID string) (workspaceID string, rootPaneID string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT workspace_id, root_pane_id FROM cockpit_workspaces WHERE root_job_id = ?`, rootJobID).Scan(&workspaceID, &rootPaneID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(workspaceID), strings.TrimSpace(rootPaneID), nil
}

// GetWorkspaceForRoot returns the Herdr workspace id registered for an
// orchestration root, if one exists. The bool reports found; a not-found root is
// ("", false, nil) — never a surfaced sql.ErrNoRows — so the cockpit's fail-open
// finalize path can treat "no workspace" as a clean miss rather than an error.
// FinalizeRoot uses it to close the per-root workspace even when no pane rows
// remain (job mode deletes pane rows per-Deliver, so the registry is the only
// remaining handle on the workspace at root-terminal).
func (s *Store) GetWorkspaceForRoot(ctx context.Context, rootJobID string) (string, bool, error) {
	rootJobID = strings.TrimSpace(rootJobID)
	if rootJobID == "" {
		return "", false, nil
	}
	workspaceID, _, err := s.lookupWorkspaceForRoot(ctx, rootJobID)
	if err != nil {
		return "", false, err
	}
	if workspaceID == "" {
		return "", false, nil
	}
	return workspaceID, true, nil
}

// DeleteWorkspaceForRoot removes the cockpit_workspaces registry row for a root.
// FinalizeRoot calls it after closing the workspace so a second finalize finds
// nothing and no-ops (idempotent). Deleting a missing row is a no-op, not an error.
func (s *Store) DeleteWorkspaceForRoot(ctx context.Context, rootJobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cockpit_workspaces WHERE root_job_id = ?`, strings.TrimSpace(rootJobID))
	return err
}

func scanCockpitPane(row interface{ Scan(dest ...any) error }) (CockpitPane, error) {
	var pane CockpitPane
	if err := row.Scan(&pane.ID, &pane.JobID, &pane.PaneKey, &pane.RootJobID,
		&pane.PaneID, &pane.WorkspaceID, &pane.Source, &pane.CreatedAt); err != nil {
		return CockpitPane{}, err
	}
	return pane, nil
}

func newCockpitPaneID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "cockpit-pane-" + hex.EncodeToString(raw[:]), nil
}

// captureSkillOptJudgeOutcome records one judge-vs-human outcome row for a
// candidate decision. It is best-effort: capture must never fail the decision,
// so callers log and continue on error.
//
// The judge accept/reject signal is read generically from the candidate
// review's eval_report_json (never from new typed struct fields) using
// skillOptJudgeAcceptFromReport, and the four-way Direction is derived from the
// human decision crossed with that signal. The raw eval report is persisted in
// JudgeScoreJSON so Direction can be recomputed later if the heuristic evolves.
func captureSkillOptJudgeOutcome(ctx context.Context, execer sqlExecer, version AgentTemplateVersion, evalReportJSON string, humanDecision string, reason string) error {
	id, err := newSkillOptJudgeOutcomeID()
	if err != nil {
		return err
	}
	judgeAccept, hasSignal, promptVersion, evaluatorID, promptHash := skillOptJudgeAcceptFromReport(evalReportJSON)
	if !hasSignal {
		// No recognizable judge signal in the eval report (missing/empty/
		// unrecognized): skip rather than record a misleading "judge rejected"
		// outcome that would pollute the agreement dataset. Calibration excludes
		// no-data decisions.
		return nil
	}
	humanPromoted := strings.TrimSpace(humanDecision) == "promoted"
	direction := skillOptJudgeDirection(humanPromoted, judgeAccept)
	_, err = execer.ExecContext(ctx, `INSERT INTO skillopt_judge_outcomes(
			id, candidate_version_id, template_id, judge_score_json, judge_prompt_version,
			judge_evaluator_id, judge_prompt_hash, human_decision, direction, reason
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		strings.TrimSpace(version.ID),
		strings.TrimSpace(version.TemplateID),
		strings.TrimSpace(evalReportJSON),
		promptVersion,
		evaluatorID,
		promptHash,
		strings.TrimSpace(humanDecision),
		direction,
		strings.TrimSpace(reason))
	return err
}

func skillOptJudgeDirection(humanPromoted bool, judgeAccept bool) string {
	switch {
	case humanPromoted && judgeAccept:
		return SkillOptJudgeDirectionAgreeAccept
	case !humanPromoted && !judgeAccept:
		return SkillOptJudgeDirectionAgreeReject
	case !humanPromoted && judgeAccept:
		return SkillOptJudgeDirectionJudgeAcceptHumanReject
	default: // humanPromoted && !judgeAccept
		return SkillOptJudgeDirectionJudgeRejectHumanAccept
	}
}

// skillOptJudgeAcceptFromReport derives a judge accept/reject signal plus the
// judge prompt/evaluator identity from a candidate's eval report JSON, reading
// everything generically (map[string]any) so it does not depend on any typed
// struct fields that may be absent on older reports. The eval report may carry
// the judge fields at the top level or nested under "evaluator_score" (the
// EvaluatorScore object in internal/skillopt/contract.go), so both locations
// are inspected.
//
// Judge-accept heuristic (most authoritative field wins):
//  1. explicit boolean "promotable" — true => accept, false => reject;
//  2. a recommendation string ("recommendation"/"recommended_action"):
//     "promote"/"accept"/"approve"/"pass" => accept, "reject"/"decline"/"fail"
//     => reject;
//  3. a quality/contract status string ("quality_status"/"contract_status"):
//     "pass"/"passed"/"promote"/"ok"/"accept"/"approved" => accept,
//     "fail"/"failed"/"reject"/"rejected" => reject (other statuses like
//     "not_run" are inconclusive and skipped);
//  4. fall back to a soft/selection score — "soft", or the landing-page
//     profile's "best_selection_soft"/"best_selection_hard" — first present
//     wins: score >= 0.5 => accept.
//
// hasSignal reports whether any of the above produced a verdict. When it is
// false (missing/empty/unrecognized report), callers should SKIP recording the
// outcome rather than treat the absence of data as a "judge rejected" verdict,
// which would pollute the calibration dataset.
func skillOptJudgeAcceptFromReport(evalReportJSON string) (accept bool, hasSignal bool, promptVersion string, evaluatorID string, promptHash string) {
	evalReportJSON = strings.TrimSpace(evalReportJSON)
	if evalReportJSON == "" {
		return false, false, "", "", ""
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(evalReportJSON), &report); err != nil {
		return false, false, "", "", ""
	}
	// Search the report root and the nested evaluator_score object, preferring
	// whichever supplies the most authoritative signal.
	sources := []map[string]any{report}
	if nested, ok := report["evaluator_score"].(map[string]any); ok {
		sources = append(sources, nested)
	}

	promptVersion = firstSkillOptJudgeString(sources, "judge_prompt_version", "prompt_version")
	evaluatorID = firstSkillOptJudgeString(sources, "judge_evaluator_id", "evaluator_id", "profile_id")
	promptHash = firstSkillOptJudgeString(sources, "judge_prompt_hash", "prompt_hash")

	// 1) explicit promotable boolean.
	for _, source := range sources {
		if value, ok := source["promotable"].(bool); ok {
			return value, true, promptVersion, evaluatorID, promptHash
		}
	}
	// 2) explicit recommendation.
	if recommendation := firstSkillOptJudgeString(sources, "recommendation", "recommended_action"); recommendation != "" {
		switch strings.ToLower(recommendation) {
		case "promote", "accept", "approve", "pass":
			return true, true, promptVersion, evaluatorID, promptHash
		case "reject", "decline", "fail":
			return false, true, promptVersion, evaluatorID, promptHash
		}
	}
	// 3) quality / contract status.
	if status := firstSkillOptJudgeString(sources, "quality_status", "contract_status"); status != "" {
		switch strings.ToLower(status) {
		case "pass", "passed", "promote", "ok", "accept", "approved":
			return true, true, promptVersion, evaluatorID, promptHash
		case "fail", "failed", "reject", "rejected":
			return false, true, promptVersion, evaluatorID, promptHash
		}
	}
	// 4) soft/selection-score fallback. Real optimizer reports vary by evaluator
	// profile: the generic profile sets a top-level "promotable" (handled above);
	// the landing-page profile instead reports the best candidate's selection-gate
	// scores ("best_selection_soft"/"best_selection_hard") with no "promotable".
	// Treat the first such score present as the judge's confidence: >= 0.5 => accept.
	for _, source := range sources {
		for _, key := range []string{"soft", "best_selection_soft", "best_selection_hard"} {
			if score, ok := skillOptJudgeFloat(source[key]); ok {
				return score >= 0.5, true, promptVersion, evaluatorID, promptHash
			}
		}
	}
	return false, false, promptVersion, evaluatorID, promptHash
}

func firstSkillOptJudgeString(sources []map[string]any, keys ...string) string {
	for _, source := range sources {
		for _, key := range keys {
			if value, ok := source[key].(string); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func skillOptJudgeFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
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
	row := tx.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at, v.canary_sample, v.canary_started_at
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
	row := tx.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
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
	`
ALTER TABLE ranked_feedback_events ADD COLUMN tie_groups_json TEXT NOT NULL DEFAULT '';
	`,
	`
ALTER TABLE agent_instances ADD COLUMN autonomy_policy TEXT NOT NULL DEFAULT 'auto';
	`,
	`
ALTER TABLE tasks ADD COLUMN worktree_path TEXT NOT NULL DEFAULT '';
	`,
	`
CREATE TABLE interactive_prompts (
	id TEXT PRIMARY KEY,
	question TEXT NOT NULL,
	choices_json TEXT NOT NULL DEFAULT '[]',
	default_value TEXT NOT NULL DEFAULT '',
	required INTEGER NOT NULL DEFAULT 1,
	answer_format TEXT NOT NULL DEFAULT 'text',
	source_command TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'pending',
	answer_value TEXT NOT NULL DEFAULT '',
	answer_source TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	answered_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_interactive_prompts_state ON interactive_prompts(state);
	`,
	`
CREATE TABLE created_repos (
	repo TEXT PRIMARY KEY,
	purpose TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_created_repos_session ON created_repos(session_id);
	`,
	`
ALTER TABLE jobs ADD COLUMN parent_job_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN delegation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN delegation_depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN delegated_by TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_jobs_parent_job_id ON jobs(parent_job_id);
CREATE INDEX idx_jobs_delegation_id ON jobs(delegation_id);
	`,
	`
ALTER TABLE agents ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_instances ADD COLUMN model TEXT NOT NULL DEFAULT '';
	`,
	`
CREATE TABLE skillopt_judge_outcomes (
	id TEXT PRIMARY KEY,
	candidate_version_id TEXT NOT NULL,
	template_id TEXT NOT NULL DEFAULT '',
	judge_score_json TEXT NOT NULL DEFAULT '',
	judge_prompt_version TEXT NOT NULL DEFAULT '',
	judge_evaluator_id TEXT NOT NULL DEFAULT '',
	judge_prompt_hash TEXT NOT NULL DEFAULT '',
	human_decision TEXT NOT NULL,
	direction TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_skillopt_judge_outcomes_template ON skillopt_judge_outcomes(template_id);
CREATE INDEX idx_skillopt_judge_outcomes_candidate ON skillopt_judge_outcomes(candidate_version_id);
	`,
	`
CREATE TABLE cockpit_panes (
	id TEXT PRIMARY KEY,
	job_id TEXT NOT NULL DEFAULT '',
	pane_key TEXT NOT NULL DEFAULT '',
	root_job_id TEXT NOT NULL DEFAULT '',
	pane_id TEXT NOT NULL DEFAULT '',
	workspace_id TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(workspace_id, pane_key)
);

CREATE INDEX idx_cockpit_panes_job ON cockpit_panes(job_id);
CREATE INDEX idx_cockpit_panes_root ON cockpit_panes(root_job_id);

CREATE TABLE cockpit_workspaces (
	root_job_id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	`
ALTER TABLE cockpit_workspaces ADD COLUMN root_pane_id TEXT NOT NULL DEFAULT '';
	`,
	`
ALTER TABLE branch_locks ADD COLUMN skip_native_review_fanout INTEGER NOT NULL DEFAULT 0;
	`,
	`
ALTER TABLE jobs ADD COLUMN root_killed INTEGER NOT NULL DEFAULT 0;
	`,
	`
ALTER TABLE jobs ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
	`,
	// #420: denormalize the coordination-tree root onto an indexed root_id column
	// so root-scoped helpers do one indexed lookup instead of a full-table scan
	// that unmarshals every payload. New DEFAULT '' rows are then backfilled by
	// backfillJobRootID (a Go-side, idempotent, malformed-JSON-safe pass run after
	// migrations), not by in-migration json_extract: modernc's json_extract raises
	// a SQL error on malformed payloads, which would abort the whole migration —
	// the Go pass instead self-roots a malformed row, matching rootJobID().
	`
ALTER TABLE jobs ADD COLUMN root_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_jobs_root_id ON jobs(root_id);
	`,
	// #473 Mode B: per-(template, version) Beta-Bernoulli bandit arm. alpha/beta
	// are the Beta(1+wins, 1+losses) posterior under the uniform Beta(1,1) prior,
	// so the row is the sufficient statistic and the posterior is reconstructable.
	// pulls is wins+losses (the "over K samples" / tiering count). The table is
	// dedicated so these MUTABLE counters never overload the immutable contract
	// rows (ranked_feedback_events). Off-by-default: no rows exist unless the
	// manual `skillopt ab` A/B runs.
	`
CREATE TABLE skillopt_bandit_arms (
	template_id TEXT NOT NULL,
	template_version_id TEXT NOT NULL,
	alpha REAL NOT NULL DEFAULT 1,
	beta REAL NOT NULL DEFAULT 1,
	pulls INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (template_id, template_version_id)
);
	`,
	// #484 canary promotion: a new `canary` version state plus two columns that
	// record the active canary's sampled-traffic fraction and window-start so the
	// routing seam and the daemon regression comparator can find/parametrize it.
	// The state column is already free-text TEXT, so no structural change is needed;
	// these columns carry DEFAULTs (0 / '') so every existing row reads identically
	// and this migration is a pure additive append (it does not renumber or alter
	// any prior migration). The partial index makes the "active canary for this
	// template" lookup a single indexed probe (at most one canary row per template
	// at a time) and indexes no non-canary rows.
	`
ALTER TABLE agent_template_versions ADD COLUMN canary_sample REAL NOT NULL DEFAULT 0;
ALTER TABLE agent_template_versions ADD COLUMN canary_started_at TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_atv_canary ON agent_template_versions(template_id) WHERE state = 'canary';
	`,
	// #549: index job_events so per-job and per-kind lookups stop full-scanning
	// the table. job_events had NO index, so every ListJobEvents(jobID) (one per
	// job in several daemon passes) and the cleanup-marker queries scanned the
	// whole table. idx_job_events_job_id covers the WHERE job_id=? ORDER BY id
	// read; idx_job_events_kind covers the kind-filtered marker queries
	// (JobIDsWithEventKind / JobIDsWithPendingDelegationWorktreeReclaim). Both are
	// pure additive indexes — no row reads differently, only faster.
	`
CREATE INDEX idx_job_events_job_id ON job_events(job_id);
CREATE INDEX idx_job_events_kind ON job_events(kind);
	`,
	// #533 agent heartbeat schedules: one row per (agent, named heartbeat) tracking
	// the schedule's persisted state so a daemon restart never duplicates an active
	// run (the next_due_at + the active-job check are the restart-safe dedup). This
	// is a pure additive append — CREATE TABLE only, no ALTER/renumber of any prior
	// migration — and the table stays empty unless a heartbeat is configured AND
	// fires, so every existing DB reads identically.
	`
CREATE TABLE heartbeat_state (
	agent TEXT NOT NULL,
	name TEXT NOT NULL,
	last_run_at TEXT NOT NULL DEFAULT '',
	next_due_at TEXT NOT NULL DEFAULT '',
	last_job_id TEXT NOT NULL DEFAULT '',
	last_status TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (agent, name)
);
	`,
	// Running-job stale recovery queries `state = running AND updated_at < ?` on
	// every daemon worker tick. Index only running rows so long-lived databases do
	// not scan terminal jobs once per second.
	`
CREATE INDEX idx_jobs_running_updated_at ON jobs(updated_at) WHERE state = 'running';
	`,
	// #566 --watch-issues bounded polling: one row per repo tracking the newest
	// issue/PR comment updated_at the issue-comment watcher has observed. The
	// daemon passes it (minus a small overlap) as the `since` bound to the repo-wide
	// comment endpoint, collapsing the former O(open-issues) per-issue comment
	// fan-out into a single since-bounded call per repo per tick. Pure additive
	// append (CREATE TABLE only); the table stays empty until --watch-issues runs,
	// so every existing DB reads identically.
	`
CREATE TABLE issue_comment_poll_state (
	repo_full_name TEXT PRIMARY KEY,
	last_seen_comment_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
}
