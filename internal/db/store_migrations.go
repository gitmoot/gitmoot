package db

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SchemaMigrationFingerprint identifies the exact ordered migration set. Test
// schema templates use it in their cache key so a migration edit cannot reuse
// a stale database from an earlier test run.
func SchemaMigrationFingerprint() string {
	return schemaMigrationFingerprint(migrations)
}

func schemaMigrationFingerprint(ordered []string) string {
	hash := sha256.New()
	var framing [8]byte
	binary.BigEndian.PutUint64(framing[:], uint64(len(ordered)))
	_, _ = hash.Write(framing[:])
	for _, migration := range ordered {
		binary.BigEndian.PutUint64(framing[:], uint64(len(migration)))
		_, _ = hash.Write(framing[:])
		_, _ = hash.Write([]byte(migration))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// SchemaMigrationCount returns the number of migrations in the current schema.
func SchemaMigrationCount() int {
	return len(migrations)
}

func (s *Store) Migrate(ctx context.Context) error {
	MigrateObserver()
	for version, migration := range migrations {
		if err := s.applyMigration(ctx, version+1, migration); err != nil {
			return err
		}
	}
	if err := s.backfillJobRootID(ctx); err != nil {
		return err
	}
	if err := s.backfillGhostSessionJobs(ctx); err != nil {
		return err
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
	// #596 merge-gate no-CI race: one row per PR recording the FIRST evaluation at
	// which the merge gate saw zero external commit-statuses AND zero check-runs at
	// a given head. The gate defers concluding "this repo has no CI" until a SECOND
	// consecutive zero-external observation at the SAME head, at least min_ci_wait
	// later — closing the window where a fresh head merges before GitHub Actions has
	// created its check run. A new head resets the observation. Pure additive append
	// (CREATE TABLE only); the table stays empty until a zero-external evaluation
	// occurs and is read only on the no-CI path, so every existing DB reads
	// identically.
	`
CREATE TABLE merge_gate_ci_observations (
	repo_full_name TEXT NOT NULL,
	pull_request INTEGER NOT NULL,
	head_sha TEXT NOT NULL DEFAULT '',
	first_zero_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(repo_full_name, pull_request)
);
	`,
	// #619 covering index for the per-tick job-event candidate GROUP BY queries
	// (JobIDsWithPendingAdvanceRetry / CommentRetry / DelegationWorktreeReclaim).
	// Those queries filter `kind IN (...)` and project only job_id + MAX(id), but
	// idx_job_events_kind covers only `kind`, so each candidate row still required a
	// table row fetch to read job_id/id (~23.67 MiB/call for the advance query on the
	// affected DB). Indexing (kind, job_id, id) lets the planner satisfy both the
	// outer filter and the MAX(id) GROUP BY index-only. EQP flips all three from
	// `SEARCH ... USING INDEX idx_job_events_kind (kind=? AND rowid=?)` (with row
	// fetches) to `SEARCH ... USING COVERING INDEX idx_job_events_kind_job_id
	// (kind=?)`; the GROUP BY temp b-tree remains (groups span kinds) but now runs
	// over index-only (job_id,id). job_events.id is INTEGER PRIMARY KEY (a rowid
	// alias) so id is covered. Result sets are byte-identical — pure additive index
	// (idx_job_events_kind is kept for pure kind= lookups), no renumber/alter of any
	// prior migration.
	`
CREATE INDEX idx_job_events_kind_job_id ON job_events(kind, job_id, id);
	`,
	// #619 partial index for the per-tick ListQueuedJobs poll. That query
	// (`WHERE state='queued' ORDER BY created_at, rowid`) had no supporting index,
	// so it full-scanned jobs and built a temp b-tree for the ORDER BY every worker
	// tick. A partial index on created_at over only the queued rows lets the planner
	// read them in created_at order directly (the partial index carries rowid as the
	// implicit tiebreaker, satisfying `created_at, rowid`) and indexes only the small
	// queued set, not the terminal-job backlog. ListQueuedJobs' text is unchanged.
	// EQP flips from `SCAN jobs` + `USE TEMP B-TREE FOR ORDER BY` to `SCAN jobs USING
	// INDEX idx_jobs_queued_created`. Pure additive index, no renumber/alter of any
	// prior migration.
	`
CREATE INDEX idx_jobs_queued_created ON jobs(created_at) WHERE state='queued';
	`,
	// #619 drop the now-redundant idx_job_events_kind. The prior migration added
	// idx_job_events_kind_job_id(kind, job_id, id); its leading column is `kind`, so
	// it is a strict superset of the single-column idx_job_events_kind(kind) for every
	// query that leads on kind — which is EVERY kind-filtered job_events query in the
	// codebase (the three per-tick candidate GROUP BYs, JobIDsWithEventKind, and
	// JobIDsWithOpenEscalation all filter `kind = ?` / `kind IN (...)`). SQLite serves
	// those from the composite (EQP verified against a copy of the production DB after
	// this drop), so idx_job_events_kind only cost write amplification on every
	// job_events insert. DROP INDEX IF EXISTS is idempotent and a pure removal — no
	// row reads differently — appended at the end so it does not renumber or alter any
	// prior migration.
	`
DROP INDEX IF EXISTS idx_job_events_kind;
	`,
	// #626 agent persistent memory (Phase 0 storage): the two-table evidence/
	// upsert split plus a standalone FTS5 index over confirmed content. A single
	// keyed-upsert table cannot both deduplicate and count witnesses, so pending
	// evidence (memory_observations) and injectable facts (confirmed_memories)
	// live apart. Owner identity is STRUCTURED (owner_kind/owner_ref/owner_version)
	// so template upgrades never inherit stale pools and role variants never
	// collide. repo is NULLABLE (NULL == a general-scope fact); partial unique
	// indexes enforce one keyed confirmed row per (owner, repo, key) with correct
	// NULL semantics. The FTS table is a PLAIN (non-external-content) fts5 table
	// managed transactionally from Go (UpsertConfirmedMemory keeps it in sync),
	// avoiding trigger-body parsing in the multi-statement migration string. This
	// is a pure additive append — CREATE TABLE/INDEX only, no ALTER/renumber of any
	// prior migration — and every table stays empty until an agent is enrolled in
	// [memory] (default off), so behavior is byte-identical when the feature is off.
	`
CREATE TABLE memory_observations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_kind TEXT NOT NULL,
	owner_ref TEXT NOT NULL,
	owner_version TEXT NOT NULL DEFAULT '',
	repo TEXT,
	scope TEXT NOT NULL,
	key TEXT NOT NULL,
	content TEXT NOT NULL,
	provenance TEXT NOT NULL DEFAULT '',
	trust_mark TEXT NOT NULL DEFAULT '',
	source_job TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_memory_obs_owner ON memory_observations(owner_kind, owner_ref, owner_version, key);

CREATE TABLE confirmed_memories (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_kind TEXT NOT NULL,
	owner_ref TEXT NOT NULL,
	owner_version TEXT NOT NULL DEFAULT '',
	repo TEXT,
	scope TEXT NOT NULL,
	key TEXT NOT NULL,
	content TEXT NOT NULL,
	provenance TEXT NOT NULL DEFAULT '',
	source_job TEXT NOT NULL DEFAULT '',
	first_confirmed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	superseded_by INTEGER
);
CREATE UNIQUE INDEX idx_confirmed_repo_key ON confirmed_memories(owner_kind, owner_ref, owner_version, repo, key) WHERE repo IS NOT NULL;
CREATE UNIQUE INDEX idx_confirmed_general_key ON confirmed_memories(owner_kind, owner_ref, owner_version, key) WHERE repo IS NULL;

CREATE VIRTUAL TABLE confirmed_memories_fts USING fts5(content, key, tokenize='porter');
	`,
	// #627 deterministic fixed-corpus replay-gate audit trail. Each row records one
	// terminal gate protocol run for a candidate: the champion it was compared
	// against, the corpus (path + version), the two aggregate corpus means, the
	// per-item deltas (JSON), the accept/reject verdict, and how many attempts (1 or
	// the single retry -> 2) it took. Pure additive append (CREATE TABLE only): the
	// table stays empty until a `gitmoot skillopt gate run` executes, and it is read
	// only by the gate-run history + the promotion guard, so every existing DB reads
	// identically. It NEVER promotes — promotion stays a separate, guarded action.
	`
CREATE TABLE skillopt_gate_runs (
	id TEXT PRIMARY KEY,
	template_id TEXT NOT NULL DEFAULT '',
	candidate_version_id TEXT NOT NULL DEFAULT '',
	champion_version_id TEXT NOT NULL DEFAULT '',
	corpus_path TEXT NOT NULL DEFAULT '',
	corpus_version INTEGER NOT NULL DEFAULT 0,
	corpus_items INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0,
	accepted INTEGER NOT NULL DEFAULT 0,
	champion_mean REAL NOT NULL DEFAULT 0,
	candidate_mean REAL NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT '',
	deltas_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_skillopt_gate_runs_candidate ON skillopt_gate_runs(candidate_version_id);
	`,
	// #657 session jobs: mark a job whose execution happens OUTSIDE the engine (the
	// "here"/prompt-import calling session drives the real work via `job open`/
	// `close`/`record`). A session job is created directly `running` and flagged so
	// (1) the daemon's queued selector never claims it, (2) the stuck-running reaper
	// skips it, and (3) it is closed via the CLI result path. Pure additive append —
	// ALTER TABLE ADD COLUMN with a NOT NULL DEFAULT 0, so SQLite backfills every
	// existing row to 0 and the whole normal dispatch/reaper path is byte-identical
	// unless the new commands are used.
	`
ALTER TABLE jobs ADD COLUMN externally_driven INTEGER NOT NULL DEFAULT 0;
	`,
	// #651 cross-boot process-liveness recovery: stamp the claiming process's
	// identity onto a running job (runner_pid for observability; runner_boot_id the
	// load-bearing cross-boot signal) and the acquiring process's boot id onto a
	// pid-backed resource lock. All three carry DEFAULTs (0 / '') so every existing
	// row reads identically and this is a pure additive append that does NOT
	// renumber or alter any prior migration — mirroring the owner_pid/owner_hostname
	// precedent above. A daemon upgraded mid-flight sees pre-upgrade running jobs as
	// identity-less ('' boot) and safely leaves them to the existing age/lease
	// recovery, then stamps identity on every subsequently-claimed job — no backfill.
	`
ALTER TABLE jobs ADD COLUMN runner_pid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN runner_boot_id TEXT NOT NULL DEFAULT '';
ALTER TABLE resource_locks ADD COLUMN owner_boot_id TEXT NOT NULL DEFAULT '';
	`,
	// #661 per-codex-session token delta tracking. codex reports turn.completed
	// usage as SESSION-CUMULATIVE on a resumed thread (the session's running
	// total, not the turn's), so attributing it to a single job needs the last-seen
	// cumulative counters per runtime session. This table stores them keyed by
	// runtime+ref; RecordRuntimeSessionUsageDelta reads the prior baseline, returns
	// max(0, cumulative_now - prev) as the job's usage, and upserts the new
	// baseline — all in one transaction. Pure additive append (CREATE TABLE only):
	// the table stays empty until a resumed codex delivery records usage, and every
	// existing DB reads identically. No cross-runtime use today (only codex sets
	// Result.CumulativeUsage). No GC/retention in v1 — orphan rows for dead threads
	// are tens of bytes; a bounded cleanup pass is a follow-up.
	`
CREATE TABLE runtime_session_usage (
	session_key TEXT PRIMARY KEY,
	input_cum INTEGER NOT NULL DEFAULT 0,
	output_cum INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	// #682 resumable blocked/needs gates. When a stage returns `blocked` with a
	// `needs` list, each need is persisted here as a gate attached to the blocked
	// job; when every gate is satisfied the blocked stage auto-re-runs via RetryJob.
	// UNIQUE(job_id, need) makes RecordJobGates' UPSERT idempotent and lets a
	// re-blocked job REOPEN a repeated need. Pure additive append (CREATE
	// TABLE/INDEX only, no ALTER/renumber of any prior migration): the table stays
	// empty until a blocked-with-needs result is recorded, so a blocked job with no
	// `needs` — and every existing DB — reads byte-identically. Rows are keyed by
	// job id (not FK-constrained) so a retried/cancelled job's history is retained;
	// there is no GC in v1 (a satisfied gate is tens of bytes).
	`
CREATE TABLE job_gates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id TEXT NOT NULL,
	need TEXT NOT NULL,
	satisfied INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	satisfied_at TEXT NOT NULL DEFAULT '',
	UNIQUE(job_id, need)
);
CREATE INDEX idx_job_gates_job ON job_gates(job_id);
	`,
	// #681 pipeline registry: one row per named pipeline holding the verbatim spec
	// YAML + its content hash (a run snapshots the hash it was created from), the
	// interval/jitter schedule fields (heartbeat idiom), and the durable schedule
	// state (last_run_at/next_due_at/last_run_id/last_status) that makes an
	// interval schedule restart-safe. name is the primary key and the stem of the
	// pipeline's hidden shell runner agent. Pure additive append (CREATE TABLE
	// only): the table stays empty until `gitmoot pipeline add` runs, so every
	// existing DB reads identically. The per-run and per-stage tables are separate
	// additive migrations appended by the run/advancer step.
	`
CREATE TABLE pipelines (
	name TEXT PRIMARY KEY,
	repo TEXT NOT NULL DEFAULT '',
	spec_yaml TEXT NOT NULL DEFAULT '',
	spec_hash TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 0,
	interval TEXT NOT NULL DEFAULT '',
	jitter TEXT NOT NULL DEFAULT '',
	last_run_at TEXT NOT NULL DEFAULT '',
	next_due_at TEXT NOT NULL DEFAULT '',
	last_run_id TEXT NOT NULL DEFAULT '',
	last_status TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	// #681 pipeline runs + stages: the per-run execution state the scan-based
	// advancer folds and drives. A pipeline_runs row is one execution of a
	// pipeline; it snapshots spec_hash so a run always executes the spec content it
	// was created from (the pipelines row's spec_yaml is resolved back and its hash
	// verified against this column). pipeline_run_stages holds one row per stage of
	// that run, keyed by (run_id, stage_id): the stage's advancement state, the job
	// id the advancer enqueued for it, the current attempt (deterministic stage job
	// ids embed it), the blocked needs persisted verbatim, and a short summary.
	// Pure additive append (CREATE TABLE/INDEX only): both tables stay empty until
	// `gitmoot pipeline run` creates a run, so every existing DB reads identically.
	// idx_pipeline_run_stages_run_id backs the per-run stage fold
	// (ListPipelineRunStages). Times are RFC3339Nano UTC text (empty == zero),
	// mirroring the pipelines/heartbeat_state schedule columns.
	`
CREATE TABLE pipeline_runs (
	id TEXT PRIMARY KEY,
	pipeline TEXT NOT NULL DEFAULT '',
	trigger TEXT NOT NULL DEFAULT 'manual',
	spec_hash TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'running',
	halt_stage TEXT NOT NULL DEFAULT '',
	halt_reason TEXT NOT NULL DEFAULT '',
	needs_json TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL DEFAULT '',
	finished_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE pipeline_run_stages (
	run_id TEXT NOT NULL,
	stage_id TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'pending',
	job_id TEXT NOT NULL DEFAULT '',
	attempt INTEGER NOT NULL DEFAULT 0,
	needs_json TEXT NOT NULL DEFAULT '',
	summary TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL DEFAULT '',
	finished_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (run_id, stage_id)
);

CREATE INDEX idx_pipeline_run_stages_run_id ON pipeline_run_stages(run_id);
	`,
	// #534 native agent chat (V1, local-only): a durable, repo-aware chat ledger
	// where registered agents + the human converse in threads, tag each other, and
	// later promote selected messages into real jobs. This is the ONLY net-new
	// storage the feature needs; promotion/mention-parsing/back-links all reuse
	// existing seams. Pure additive append (CREATE TABLE/INDEX only, no ALTER or
	// renumber of any prior migration): every table stays empty until `gitmoot
	// chat …` runs, so every existing DB reads byte-identically.
	//
	// The schema shape is deliberately federation-ready even though V1 is local-
	// only and zero-network (#705 is the parked bridge). These are column shapes
	// and naming rules, not features — they cost nothing at runtime:
	//   * `origin` columns on threads/messages/mentions and origin-qualified refs.
	//     V1 populates them with a generated stable per-DB home_id (chat_meta) — the
	//     "self"-equivalent — and NO code path assumes origin == "self". This is
	//     what makes `agent@machine-A` addressable from machine-B later and prevents
	//     bridge echo loops.
	//   * a structured author triple (author_kind|author_name|author_origin), never
	//     a bare agent name.
	//   * a versioned canonical envelope_json ({schema_version, kind, body,
	//     mentions[], refs[], reply_to}) — the deterministic self-describing unit a
	//     future bridge hashes/signs into opaque wire content. Additive-only.
	//   * topic-path-safe thread slugs ([a-z0-9-], no '+'/'#'), unique per repo, so
	//     a slug always derives a valid MQTT topic later.
	//   * an explicit (ts_ms, seq) ordering key (ts_ms is unix-millis); seq is the
	//     per-thread gapless LOCAL insertion order used as the deterministic
	//     same-timestamp tiebreak — a local rendering key, never a cross-origin
	//     federation assumption.
	//   * reserved NULLABLE content_hash/signature/signer_pubkey columns (content-
	//     addressing + signing land in the bridge, not here), with a partial UNIQUE
	//     index on non-NULL content_hash so a bridged content-addressed id can be
	//     stored verbatim and re-delivery is schema-enforced idempotent.
	//   * a fixed kind vocabulary chat|system|job_result|promotion_request, with
	//     promotion_request distinct and (per the interaction model) always locally
	//     re-authorized; job_result messages are non-promotable.
	`
CREATE TABLE chat_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE chat_threads (
	id TEXT PRIMARY KEY,
	slug TEXT NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	repo TEXT NOT NULL DEFAULT '',
	origin TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'open',
	created_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(repo, slug)
);

CREATE TABLE chat_messages (
	id TEXT PRIMARY KEY,
	origin TEXT NOT NULL DEFAULT '',
	thread_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	ts_ms INTEGER NOT NULL DEFAULT 0,
	author_kind TEXT NOT NULL DEFAULT '',
	author_name TEXT NOT NULL DEFAULT '',
	author_origin TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT 'chat',
	body TEXT NOT NULL DEFAULT '',
	envelope_json TEXT NOT NULL DEFAULT '',
	refs_json TEXT NOT NULL DEFAULT '',
	reply_to TEXT NOT NULL DEFAULT '',
	promoted_job_id TEXT NOT NULL DEFAULT '',
	content_hash TEXT,
	signature TEXT,
	signer_pubkey TEXT,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(thread_id, seq)
);
CREATE INDEX idx_chat_messages_thread_seq ON chat_messages(thread_id, seq);
CREATE INDEX idx_chat_messages_promoted_job ON chat_messages(promoted_job_id);
CREATE UNIQUE INDEX idx_chat_messages_content_hash ON chat_messages(content_hash) WHERE content_hash IS NOT NULL;

CREATE TABLE chat_mentions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id TEXT NOT NULL,
	thread_id TEXT NOT NULL,
	agent TEXT NOT NULL,
	agent_origin TEXT NOT NULL DEFAULT '',
	resolved INTEGER NOT NULL DEFAULT 1,
	unread INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_chat_mentions_agent_unread ON chat_mentions(agent, unread);
	`,
	// #525 BINEVAL binary evaluation: one row per (eval run, binary question)
	// recording the yes/no verdict + explanation and the dimension the question
	// belongs to. Keyed by (run_id, question_id) so re-running a question set
	// against the same run upserts each verdict in place (stable row count,
	// corrective overwrite). Pure additive append (CREATE TABLE/INDEX only, no
	// ALTER/renumber of any prior migration): the table stays empty until
	// `gitmoot skillopt binary run` executes, so every existing DB — and every
	// existing SkillOpt review/optimize flow — reads byte-identically.
	`
CREATE TABLE skillopt_binary_verdicts (
	run_id TEXT NOT NULL,
	question_id TEXT NOT NULL,
	dimension TEXT NOT NULL DEFAULT '',
	verdict TEXT NOT NULL DEFAULT 'no',
	explanation TEXT NOT NULL DEFAULT '',
	question_weight REAL NOT NULL DEFAULT 1,
	dimension_weight REAL NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (run_id, question_id)
);
CREATE INDEX idx_skillopt_binary_verdicts_run ON skillopt_binary_verdicts(run_id);
	`,
	// #535 Autodata-style synthetic SkillOpt review items. One row per ACCEPTED
	// synthetic item generated by `gitmoot skillopt synth` (an explicit, opt-in
	// command — NO daemon/auto integration). Every row is created
	// status='pending_human_approval' and is only ever moved to approved/rejected
	// by the explicit human gate (`synth approve`/`synth reject`); NOTHING in the
	// promotion/training path reads this table, so a pending item is structurally
	// incapable of affecting a promotion. Pure additive append (CREATE TABLE/INDEX
	// only): the table stays empty until `skillopt synth` accepts an item, so every
	// existing DB reads identically. Times are RFC3339Nano UTC text.
	// idx_skillopt_synth_items_status backs the status-filtered `synth list`.
	`
CREATE TABLE skillopt_synth_items (
	id TEXT PRIMARY KEY,
	template_id TEXT NOT NULL DEFAULT '',
	repo TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending_human_approval',
	context TEXT NOT NULL DEFAULT '',
	question TEXT NOT NULL DEFAULT '',
	rubric TEXT NOT NULL DEFAULT '',
	weak_agent TEXT NOT NULL DEFAULT '',
	strong_agent TEXT NOT NULL DEFAULT '',
	judge_agent TEXT NOT NULL DEFAULT '',
	weak_answer TEXT NOT NULL DEFAULT '',
	strong_answer TEXT NOT NULL DEFAULT '',
	weak_score REAL NOT NULL DEFAULT 0,
	strong_score REAL NOT NULL DEFAULT 0,
	gap REAL NOT NULL DEFAULT 0,
	rounds INTEGER NOT NULL DEFAULT 0,
	diagnostic TEXT NOT NULL DEFAULT '',
	out_path TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_skillopt_synth_items_status ON skillopt_synth_items(status);
	`,
	// #33 preset prompt delivery modes. Additive-only: the agents column carries a
	// 'full' DEFAULT so every existing row (and every agent that never opts in)
	// keeps delivering the whole preset exactly as before. preset_session_state
	// records, per (runtime, session_id, preset_id, preset_commit), that a resumed
	// session already received a preset at a specific commit; it stays EMPTY until
	// an agent set to referenced/auto completes a full delivery, so every existing
	// DB reads identically. The composite PK is the exact-match key the delivery
	// decision queries; a preset commit change simply fails to match (and
	// RecordPresetSessionState overwrites the prior commit row for the tuple).
	`
ALTER TABLE agents ADD COLUMN preset_delivery TEXT NOT NULL DEFAULT 'full';

CREATE TABLE preset_session_state (
	runtime TEXT NOT NULL,
	session_id TEXT NOT NULL,
	preset_id TEXT NOT NULL,
	preset_commit TEXT NOT NULL DEFAULT '',
	delivered_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (runtime, session_id, preset_id, preset_commit)
);
	`,
	// #530 execution-grounded routing telemetry: one row per job terminal
	// transition capturing which (action, runtime, model, template) combination ran
	// and how it turned out (state/decision/approval + coarse tests-run + duration +
	// tokens). Pure additive append (CREATE TABLE/INDEX only): the table stays empty
	// until a job finishes AFTER this migration, so every existing DB reads
	// identically, and the row write is best-effort/fail-safe (a telemetry error
	// never fails a job). Consumed read-only by `gitmoot router summary` and the
	// optional (off-by-default) coordinator context block; NOTHING reads it back to
	// change routing behavior in v1 — it is advisory only. The two indexes back the
	// summary's repo/action filters and the --since lower bound.
	`
CREATE TABLE routing_telemetry (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id TEXT NOT NULL DEFAULT '',
	repo TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	phase TEXT NOT NULL DEFAULT '',
	runtime TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	agent TEXT NOT NULL DEFAULT '',
	template_id TEXT NOT NULL DEFAULT '',
	template_commit TEXT NOT NULL DEFAULT '',
	job_state TEXT NOT NULL DEFAULT '',
	decision TEXT NOT NULL DEFAULT '',
	approved INTEGER NOT NULL DEFAULT 0,
	tests_run INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_routing_telemetry_repo_action ON routing_telemetry(repo, action);
CREATE INDEX idx_routing_telemetry_created ON routing_telemetry(created_at);
	`,
	// #526 result-check feed-forward stub: one row per FAILED deterministic
	// binary-checklist audit of a job's parsed gitmoot_result, stored so SkillOpt
	// can later consume them as structured feedback. Nothing reads this table
	// tonight beyond tests and the job-detail cross-check — there is NO SkillOpt
	// behavior change. Pure additive append (CREATE TABLE/INDEX only, no ALTER or
	// renumber of any prior migration): the table stays empty until [workflow]
	// result_checks is warn/block AND a result actually fails a check, so every
	// existing DB and every off-mode job reads byte-identically. Rows are keyed by
	// job id (not FK-constrained) so a retried/cancelled job's history is retained;
	// there is no GC in v1 (a failure row is tens of bytes).
	`
CREATE TABLE result_check_failures (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id TEXT NOT NULL,
	root_id TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	check_id TEXT NOT NULL DEFAULT '',
	question TEXT NOT NULL DEFAULT '',
	explanation TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_result_check_failures_job ON result_check_failures(job_id);
	`,
	// #534 V1.5 — `gitmoot moot`. A per-thread key/value side-table (mirroring the
	// chat_meta shape) carries moot metadata on a thread WITHOUT an ALTER of the V1
	// chat_threads table: a thread convened as a moot records moot='1' and
	// moot_message_cap='<N>' rows. It stays empty until `gitmoot moot` runs, so every
	// existing DB reads byte-identically. Pure additive append (CREATE TABLE only, no
	// ALTER/renumber of any prior migration).
	`
CREATE TABLE chat_thread_meta (
	thread_id TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(thread_id, key)
);
	`,
	// #737 P2 `memory vault import` — retirement columns for a confirmed memory
	// whose note the owner deleted from an exported vault (deletions ⇒ retirements).
	// Pure additive append: both columns carry a constant '' default, so SQLite
	// backfills every existing row to non-retired and the read paths that now filter
	// `retired_at = ''` (vault export lister + the injection query) are byte-identical
	// on any pre-migration DB. ALTER ADD COLUMN only — no renumber/ALTER of a prior
	// migration — mirroring the head_sha precedent above. superseded_by stays
	// RESERVED (still zero writers); retirement is a distinct, additive concept.
	`
ALTER TABLE confirmed_memories ADD COLUMN retired_at TEXT NOT NULL DEFAULT '';
ALTER TABLE confirmed_memories ADD COLUMN retired_reason TEXT NOT NULL DEFAULT '';
	`,
	// #763 Track A — emergent memory clusters. Two side-tables persist the
	// deterministic community detection over the fact-similarity graph so the CLI
	// and the dashboard bridge read a stable clustering without recomputing it on
	// every request. memory_clusters holds one row per detected community (plus the
	// reserved cluster_id 0 'unclustered' bucket): label is the computed
	// distinctive-term label, label_override is the owner's `memory cluster rename`
	// (override wins when non-empty), medoid_id anchors the label for stability.
	// memory_cluster_members maps each active confirmed fact to exactly one cluster
	// (memory_id PK ⇒ a fact is in at most one cluster). Pure additive append
	// (CREATE TABLE/INDEX only, no ALTER/renumber of any prior migration): both
	// tables stay empty until `gitmoot memory clusters recompute` runs, so every
	// existing DB reads byte-identically and the feature is inert when unused.
	`
CREATE TABLE memory_clusters (
	cluster_id INTEGER PRIMARY KEY,
	label TEXT NOT NULL DEFAULT '',
	label_override TEXT NOT NULL DEFAULT '',
	medoid_id INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE memory_cluster_members (
	memory_id INTEGER PRIMARY KEY,
	cluster_id INTEGER NOT NULL
);
CREATE INDEX idx_memory_cluster_members_cluster ON memory_cluster_members(cluster_id);
	`,
	// #784 auto cross-link confirmed memories. Links are stored in a dedicated side
	// table rather than mutating owner-authored fact content: the confirmed memory
	// row remains the source of truth for the fact, while this table records a
	// deterministic, capped similarity edge from one active fact to another. Pure
	// additive append (CREATE TABLE/INDEX only): the table stays empty until a
	// memory is confirmed or `gitmoot memory links backfill` runs, so every
	// existing read path is byte-identical unless it explicitly opts into links.
	`
CREATE TABLE memory_links (
	src_id INTEGER NOT NULL,
	dst_id INTEGER NOT NULL,
	score REAL NOT NULL,
	origin TEXT NOT NULL DEFAULT 'auto',
	created_at TEXT NOT NULL,
	UNIQUE(src_id, dst_id)
);
CREATE INDEX idx_memory_links_dst ON memory_links(dst_id);
	`,
	// #777 shared memory pool author preservation. Moving a confirmed fact into
	// the reserved shared pool changes owner_kind/owner_ref, but the dashboard and
	// vault still need to know who wrote the fact. author_ref is empty for legacy
	// and private rows, where author == owner_ref, and is populated only when the
	// author differs from the current pool owner. Observations get the same column
	// so `memory ingest --shared` can stage shared observations while preserving the
	// authoring agent. ALTER ADD COLUMN only; existing rows read byte-identically.
	`
ALTER TABLE confirmed_memories ADD COLUMN author_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_observations ADD COLUMN author_ref TEXT NOT NULL DEFAULT '';
	`,
	// #779 automatic memory-cluster hierarchy. parent_id=0 marks a top-level
	// cluster; child rows point to their top-level parent. Existing flat clusters
	// are therefore top-level after migration without a data rewrite. This is a
	// byte-appended migration only: no earlier migration is changed or renumbered.
	`
ALTER TABLE memory_clusters ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 0;
	`,
	// #804 stable ingest keys. Supersede-preserving auto-confirm updates and the
	// groom rekey / cross-pool actions must be able to keep MULTIPLE rows per
	// (owner, repo, key): the one live row plus archived superseded editions, and
	// a freshly rekeyed or promoted active row alongside retired same-key
	// siblings. The original unique indexes covered EVERY row, so an archival
	// insert or a promote-after-retire would abort on the constraint. Recreate
	// them as partial ACTIVE-ROW indexes: uniqueness still holds where it matters
	// (at most one injectable row per owner/repo/key), while superseded and
	// retired rows fall outside the constraint. UpsertConfirmedMemory's key
	// lookup orders active rows first (then newest) so key-matched upserts and
	// explicit resurrection stay deterministic when several inactive rows share a
	// key. Byte-appended migration only; no earlier migration changes.
	`
DROP INDEX idx_confirmed_repo_key;
DROP INDEX idx_confirmed_general_key;
CREATE UNIQUE INDEX idx_confirmed_repo_key ON confirmed_memories(owner_kind, owner_ref, owner_version, repo, key) WHERE repo IS NOT NULL AND superseded_by IS NULL AND retired_at = '';
CREATE UNIQUE INDEX idx_confirmed_general_key ON confirmed_memories(owner_kind, owner_ref, owner_version, key) WHERE repo IS NULL AND superseded_by IS NULL AND retired_at = '';
	`,
	// #797 per-agent reasoning effort. Mirrors the additive model columns: empty
	// defaults preserve every existing agent and managed instance unchanged.
	// Byte-appended migration only; no earlier migration changes.
	`
ALTER TABLE agents ADD COLUMN effort TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_instances ADD COLUMN effort TEXT NOT NULL DEFAULT '';
	`,
	// #831 durable repo checkout recovery. Existing rows lazily backfill this on
	// their next healthy registration, doctor pass, or dispatch touch.
	`
ALTER TABLE repos ADD COLUMN primary_checkout_path TEXT NOT NULL DEFAULT '';
	`,
	// #842 split-child subject inheritance. Empty context preserves every legacy
	// confirmed memory byte-for-byte; groom splits populate it with the parent key.
	`
ALTER TABLE confirmed_memories ADD COLUMN context TEXT NOT NULL DEFAULT '';
	`,
	// #842 Phase 2 LLM split verdict cache. Content hashes pin the exact trimmed
	// byte map, so both keep and split decisions replay without another model call.
	`
CREATE TABLE groom_llm_verdicts (
	content_hash TEXT PRIMARY KEY,
	verdict TEXT NOT NULL,
	cuts_json TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	// #843 external-coordinator workflow grouping and journal. workflow_id is
	// denormalized from the payload at every insert path; the partial index has no
	// write cost for legacy/unlabelled jobs. Notes are append-only journal entries.
	`
ALTER TABLE jobs ADD COLUMN workflow_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN repo TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN pull_request INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN blocker_retry_at TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN blocker_suggested_action TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_jobs_workflow_id ON jobs(workflow_id) WHERE workflow_id != '';

CREATE TABLE workflow_notes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	workflow_id TEXT NOT NULL,
	author TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL,
	repo TEXT NOT NULL DEFAULT '',
	memory_observation_id INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_workflow_notes_wid ON workflow_notes(workflow_id, created_at, id);
	`,
	// #854 operational-status staleness verdict cache. This is deliberately
	// separate from the split cache: its enum and lifecycle are independent.
	`
CREATE TABLE groom_stale_verdicts (
	content_hash TEXT PRIMARY KEY,
	verdict TEXT NOT NULL,
	residue TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	// #861 durable ownership of generated Activepieces trigger flows. Empty is
	// the exact legacy state; the JSON envelope is written only after a pipeline
	// declares a trigger. Additive ALTER only, preserving all existing rows.
	`
ALTER TABLE pipelines ADD COLUMN trigger_binding TEXT NOT NULL DEFAULT '';
	`,
	// #863 immutable external-input snapshot for pipeline runs. Existing and
	// non-bridge rows read as the canonical empty object.
	`
ALTER TABLE pipeline_runs ADD COLUMN payload_json TEXT NOT NULL DEFAULT '{}';
	`,
	// Dashboard redesign Wave 2 coordinator handoff metadata. This side table is
	// last-write-wins per explicit workflow label and leaves all existing workflow
	// jobs and notes untouched. A missing row is the canonical all-empty value.
	`
CREATE TABLE workflow_meta (
	workflow_id TEXT PRIMARY KEY,
	author TEXT NOT NULL DEFAULT '',
	pane TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	workdir TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	// #884 durable post-terminal insight harvest. result_hash denormalizes the
	// persisted payload.result fingerprint so the home-scoped daemon sweep can use
	// a limited receipt anti-join instead of ListJobs. The partial index contains
	// only settled states (blocked included; it may later produce a new result hash
	// on resume). Existing rows keep result_hash='' and the first enabled sweep
	// records the current row/time high-water mark, so enabling never backfills old
	// history silently. Receipts are append-only by (job_id,result_hash); state
	// transitions update only their processing metadata.
	`
ALTER TABLE jobs ADD COLUMN result_hash TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_jobs_memory_harvest_terminal ON jobs(updated_at, id)
	WHERE state IN ('succeeded', 'failed', 'blocked', 'cancelled');

CREATE TABLE memory_harvest_runs (
	job_id TEXT NOT NULL,
	result_hash TEXT NOT NULL,
	state TEXT NOT NULL CHECK(state IN ('claimed', 'started', 'done', 'skipped', 'uncertain')),
	claimed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	started_at TEXT NOT NULL DEFAULT '',
	finished_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	candidate_count INTEGER NOT NULL DEFAULT 0,
	detail TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(job_id, result_hash)
);
CREATE INDEX idx_memory_harvest_runs_state_updated ON memory_harvest_runs(state, updated_at);

CREATE TABLE memory_harvest_state (
	singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
	high_water_rowid INTEGER NOT NULL DEFAULT 0,
	high_water_updated_at TEXT NOT NULL DEFAULT '',
	initialized_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	// #888 general quality-audit verdict cache. This is a plain additive table;
	// content hashes pin classifications to the exact trimmed fact bytes.
	`
CREATE TABLE groom_quality_verdicts (
	content_hash TEXT PRIMARY KEY,
	verdict TEXT NOT NULL,
	confidence REAL NOT NULL,
	residue TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	// #896 one-line human summary for externally coordinated workflows. Empty is
	// the legacy/default state; note writes preserve it unless --summary is set.
	`
ALTER TABLE workflow_meta ADD COLUMN summary TEXT NOT NULL DEFAULT '';
	`,
	// #911 makes an empty poll_interval inherit the daemon's resolved --poll
	// cadence. Only the historical implicit default is migrated; operator-set
	// non-default intervals, including the production 3m0s values, survive.
	`
UPDATE repos SET poll_interval = '' WHERE poll_interval = '30s';
	`,
	// #913 task dismissal lifecycle audit. Task state is already unconstrained
	// TEXT, so the state itself needs no column migration; this append-only table
	// records every explicit manual, automatic, and recovery transition.
	`
CREATE TABLE IF NOT EXISTS task_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	from_state TEXT NOT NULL DEFAULT '',
	to_state TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_task_events_task_id_id ON task_events(task_id, id);
	`,
	// #922 once-per-upstream-run pipeline trigger state. The downstream pipeline
	// name is the durable identity; upstream is deliberately not foreign-keyed so
	// removing an upstream leaves its dependants dormant and re-creatable. cursor
	// stores the last observed/fired upstream run id, while armed_at is the no-
	// backfill boundary used when no upstream run existed at arm time.
	`
CREATE TABLE pipeline_trigger_states (
	downstream_pipeline TEXT PRIMARY KEY,
	upstream_pipeline TEXT NOT NULL,
	cursor TEXT NOT NULL DEFAULT '',
	armed_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_pipeline_trigger_states_upstream ON pipeline_trigger_states(upstream_pipeline);
		`,
	// Collapse unbounded advance_retry history. A terminal job whose post-delivery
	// advancement kept failing appended a fresh advance_retry event on EVERY ~1s
	// tick, so job_events grew without limit — a real install reached ~1.8M rows
	// (96% of the table), and the per-tick JobIDsWithPendingAdvanceRetry GROUP BY
	// over them (plus jobNeedsAdvanceRetry's per-job ListJobEvents) pinned a whole
	// core with zero jobs in flight. Only the LATEST advance_retry per job is ever
	// consulted (last-one-wins), so every earlier duplicate is dead weight: keep
	// the max-id row per job and drop the rest. The emission path is idempotent
	// now (recordAdvanceRetryOnce), so this is a one-time heal, not a recurring
	// clean-up. Candidate/predicate semantics are unchanged: the surviving row is
	// the newest advance_retry, so MAX(id) and last-one-wins both see the same
	// result they did before.
	`
DELETE FROM job_events
WHERE kind = 'advance_retry'
  AND id NOT IN (SELECT MAX(id) FROM job_events WHERE kind = 'advance_retry' GROUP BY job_id);
	`,
	// #958 stable workflow intent. Existing human summaries become the initial
	// description once; later writes keep the legacy summary column only as a
	// compatibility mirror.
	`
ALTER TABLE workflow_meta ADD COLUMN description TEXT NOT NULL DEFAULT '';
UPDATE workflow_meta SET description = summary WHERE description = '' AND summary != '';
	`,
	// #958 live workflow status plus the durable at-most-once guard for daemon PR
	// lifecycle breadcrumbs. The structured prefix through the first ] is the
	// stable (workflow, PR, transition) key; human-readable text after it may vary.
	`
ALTER TABLE workflow_meta ADD COLUMN status TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_workflow_notes_daemon_auto
	ON workflow_notes(workflow_id, substr(body, 1, instr(body, ']')))
	WHERE author = 'daemon' AND substr(body, 1, 9) = '[auto:pr:';
	`,
	// #874 named keycard registry metadata. Credential values remain exclusively
	// in the operator-owned keychain.env file; these tables record only delivery
	// mode and deny-by-default consumer grants. Foreign-key enforcement is not
	// enabled globally, so key and pipeline deletion clean grants explicitly in
	// the same transaction instead of relying on cascading constraints.
	`
CREATE TABLE keychain_keys (
	name TEXT PRIMARY KEY,
	mode TEXT NOT NULL CHECK(mode IN ('injected', 'proxied')),
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE keychain_grants (
	consumer_kind TEXT NOT NULL,
	consumer_id TEXT NOT NULL,
	key_name TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (consumer_kind, consumer_id, key_name)
);
CREATE INDEX idx_keychain_grants_key_name ON keychain_grants(key_name);
	`,
	// #874 fixed-upstream proxy metadata for proxied keycard entries. Existing
	// proxied rows remain deliberately unconfigured until `gitmoot key configure`
	// supplies all three fields; credential values remain outside SQLite.
	`
ALTER TABLE keychain_keys ADD COLUMN proxy_upstream TEXT;
ALTER TABLE keychain_keys ADD COLUMN proxy_auth_kind TEXT
	CHECK(proxy_auth_kind IS NULL OR proxy_auth_kind IN ('bearer', 'header'));
ALTER TABLE keychain_keys ADD COLUMN proxy_header TEXT;
`,
	// #988 append-only brain changelog. Events observe confirmed-memory and
	// cluster mutations in the same transaction; kind remains an open string so
	// future lifecycle actions do not require another schema change.
	`
CREATE TABLE memory_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at TEXT NOT NULL,
	kind TEXT NOT NULL,
	memory_id INTEGER,
	key TEXT NOT NULL DEFAULT '',
	owner_kind TEXT NOT NULL DEFAULT '',
	owner_ref TEXT NOT NULL DEFAULT '',
	repo TEXT,
	scope TEXT NOT NULL DEFAULT '',
	actor TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_memory_events_at ON memory_events(at);
CREATE INDEX idx_memory_events_memory_id ON memory_events(memory_id);
	`,
	// #998 P1 durable enqueue-time model snapshot. Existing jobs honestly remain
	// unknown; every mailbox-created job writes the selected override, agent
	// default, or runtime default into this additive scalar column.
	`
ALTER TABLE jobs ADD COLUMN model TEXT NOT NULL DEFAULT '';
	`,
	// #923 opt-in SkillOpt synth diversity/novelty audit metadata. Empty defaults
	// preserve every legacy discriminating item and keep unflagged reads identical.
	// The synth table predates these columns; both ALTERs are append-only.
	`
ALTER TABLE skillopt_synth_items ADD COLUMN kind TEXT NOT NULL DEFAULT '';
ALTER TABLE skillopt_synth_items ADD COLUMN injected_memory_key TEXT NOT NULL DEFAULT '';
	`,
	// #1011 opt-in pipeline service exposure + durable receipt metadata. Exposure
	// rows hold only a SHA-256 bearer-token digest; deletion is tied to the pipeline
	// declaration. Service-run receipts instead key to pipeline_runs, which already
	// survive pipeline removal, so disabling/rotating/removing an exposure cannot
	// erase an accepted run's receipt metadata. Foreign-key enforcement is not
	// enabled globally in this store, so DeletePipeline also removes its exposure
	// explicitly in the same transaction.
	`
CREATE TABLE pipeline_exposures (
	pipeline_name TEXT PRIMARY KEY,
	schema_version INTEGER NOT NULL,
	schema_json TEXT NOT NULL,
	schema_hash TEXT NOT NULL,
	token_hash BLOB NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	bucket_tokens REAL NOT NULL DEFAULT 0,
	bucket_updated_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (pipeline_name) REFERENCES pipelines(name) ON DELETE CASCADE
);

CREATE TABLE pipeline_service_runs (
	run_id TEXT PRIMARY KEY,
	pipeline_name TEXT NOT NULL,
	artifact_relpath TEXT NOT NULL DEFAULT '',
	artifact_sha256 TEXT NOT NULL DEFAULT '',
	proof_id TEXT NOT NULL DEFAULT '',
	proof_verified_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (run_id) REFERENCES pipeline_runs(id)
);
CREATE INDEX idx_pipeline_service_runs_pipeline ON pipeline_service_runs(pipeline_name, created_at, run_id);
	`,
	// #1059 passive organization-role presence. This is durable role activity,
	// distinct from workflow notes and intentionally contains no command output.
	`
CREATE TABLE org_role_presence (
	role TEXT PRIMARY KEY,
	last_seen_at TEXT NOT NULL,
	last_command TEXT NOT NULL DEFAULT ''
);
	`,
	// #1060 opt-in organization event rules. Rows are absent by default, so the
	// daemon's rule evaluator remains completely disabled until an operator adds
	// one explicitly. Filters are plain text rather than JSON so malformed
	// user-controlled payloads can never make modernc abort a query.
	`
CREATE TABLE event_rules (
	id TEXT PRIMARY KEY,
	on_kind TEXT NOT NULL,
	match_filter TEXT,
	wake_role TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL
);
	`,
	// #1060 durable de-duplication for synthesized blocked-since events. A row
	// represents one continuous blocked episode; leaving blocked deletes it so a
	// later episode can emit once again.
	`
CREATE TABLE org_blocked_episodes (
	subject TEXT PRIMARY KEY,
	blocked_since TEXT NOT NULL,
	emitted_at TEXT,
	updated_at TEXT NOT NULL
);
	`,
	// #1060 per-role consecutive missed-wake counters. A missing row means zero;
	// successful delivery deletes the row so the default path stays sparse.
	`
CREATE TABLE org_role_missed_wakes (
	role TEXT PRIMARY KEY,
	consecutive INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
	`,
	// #1078 per-fact usage telemetry. ADD-COLUMN-only and appended at the tail
	// AFTER #1086's org_role_missed_wakes entry: migration versions are positional,
	// so never renumber this or prior entries.
	`
ALTER TABLE confirmed_memories ADD COLUMN injected_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE confirmed_memories ADD COLUMN last_injected_at TEXT NOT NULL DEFAULT '';
ALTER TABLE confirmed_memories ADD COLUMN recalled_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE confirmed_memories ADD COLUMN last_recalled_at TEXT NOT NULL DEFAULT '';
	`,
	// #1061 durable de-duplication for recycle-overdue dispatch events. This is
	// deliberately parallel to org_blocked_episodes: the CLI ingress owns these
	// episodes and never mutates the daemon's blocked-since state. Appended AFTER
	// #1078's confirmed_memories telemetry entry — positional versions, never reorder.
	`
CREATE TABLE org_recycle_overdue_episodes (
	subject TEXT PRIMARY KEY,
	overdue_since TEXT NOT NULL,
	emitted_at TEXT,
	updated_at TEXT NOT NULL
);
	`,
	// #1105 last successful Herdr role snapshot. The daemon writes this only from
	// the existing blocked-role evaluator tick; dashboard readers remain SQLite-only.
	`
CREATE TABLE org_role_live_presence (
	role TEXT PRIMARY KEY,
	state TEXT NOT NULL,
	observed_at TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
	`,
	// #1134 blocked-job TTL sweeps filter by this exact state on every worker
	// tick. Keep the predicate exact so SQLite can use the small partial index.
	`
CREATE INDEX idx_jobs_blocked_updated_at ON jobs(updated_at, id) WHERE state = 'blocked';
	`,
	// #1066 stale-task reconciliation narrows task-liveness candidates by the
	// jobs table's denormalized repo column before decoding payloads.
	`
CREATE INDEX idx_jobs_repo ON jobs(repo);
	`,
	// #1066 jobs written before the denormalized repo column existed retained its
	// empty default even when payload.repo was populated. Current write paths keep
	// both representations synchronized; this idempotently repairs only that
	// historical backlog so indexed task-liveness lookups cannot miss live jobs.
	`
UPDATE jobs SET repo = json_extract(payload, '$.repo')
WHERE repo = '' AND json_valid(payload) AND COALESCE(json_extract(payload, '$.repo'), '') != '';
	`,
	// #1136 provider-declared per-role unavailability. This is deliberately
	// separate from org_blocked_episodes: the latter is an inferred Herdr-pane
	// signal, while this row records an explicit runtime quota wall with a known,
	// bounded expiry and a one-shot escalation claim.
	`
CREATE TABLE org_role_unavailable (
	role TEXT PRIMARY KEY,
	reason TEXT NOT NULL,
	unavailable_until TEXT NOT NULL,
	escalated_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);
CREATE INDEX idx_org_role_unavailable_until ON org_role_unavailable(unavailable_until);
	`,
	// #1136 provider identity for quota incidents. Successful work may clear an
	// incident only when it ran on the same runtime that reported the wall.
	// Existing rows remain unattributed and therefore expire naturally instead
	// of being cleared by an unrelated runtime.
	`
ALTER TABLE org_role_unavailable ADD COLUMN runtime TEXT NOT NULL DEFAULT '';
	`,
	// #1200/#1201 durable wake outbox. The source discriminator/id and caller-set
	// coalesce key keep the schema producer-agnostic across workflow notes and
	// chat messages. Per-row state makes never-attempted delivery detectable.
	// Appended at the tail: migration versions are positional, never reorder.
	`
CREATE TABLE IF NOT EXISTS wake_outbox (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source_kind TEXT NOT NULL,
	source_id TEXT NOT NULL,
	target_role TEXT NOT NULL,
	coalesce_key TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'pending'
		CHECK(state IN ('pending', 'attempted', 'delivered', 'stalled', 'failed')),
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	attempted_at TEXT,
	finished_at TEXT,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	UNIQUE(source_kind, source_id, target_role)
);
CREATE INDEX IF NOT EXISTS idx_wake_outbox_pending
	ON wake_outbox(target_role, coalesce_key, created_at, id)
	WHERE state = 'pending';

-- Keep the immediately preceding denormalized-repo repair effective for
-- databases upgraded directly through this new tail migration. The update is
-- idempotent and leaves already-projected jobs untouched.
UPDATE jobs SET repo = json_extract(payload, '$.repo')
WHERE repo = '' AND json_valid(payload) AND COALESCE(json_extract(payload, '$.repo'), '') != '';
	`,
	// #1200/#1201 explicit delivery-unknown resolution. An attempted wake can
	// survive a daemon crash after Herdr accepted the prompt but before the
	// terminal row update. Rebuilding the table extends the state constraint
	// without reordering the original outbox migration; the append-only event
	// written by the expiry transaction records why no blind retry occurred.
	`
ALTER TABLE wake_outbox RENAME TO wake_outbox_before_delivery_unknown;

CREATE TABLE wake_outbox (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source_kind TEXT NOT NULL,
	source_id TEXT NOT NULL,
	target_role TEXT NOT NULL,
	coalesce_key TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'pending'
		CHECK(state IN ('pending', 'attempted', 'delivered', 'stalled', 'failed', 'delivery_unknown')),
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	attempted_at TEXT,
	finished_at TEXT,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	UNIQUE(source_kind, source_id, target_role)
);

INSERT INTO wake_outbox(
	id, source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, last_error, created_at, attempted_at, finished_at, updated_at
)
SELECT
	id, source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, last_error, created_at, attempted_at, finished_at, updated_at
FROM wake_outbox_before_delivery_unknown;

DROP TABLE wake_outbox_before_delivery_unknown;

CREATE INDEX idx_wake_outbox_pending
	ON wake_outbox(target_role, coalesce_key, created_at, id)
	WHERE state = 'pending';
CREATE INDEX idx_wake_outbox_attempted
	ON wake_outbox(attempted_at, id)
	WHERE state = 'attempted';
	`,
	// #1246 event-rule routing scope. Existing rows are addressed rules: they
	// retain the reply addressee restriction that already ships. Observer is an
	// explicit opt-in exemption. Append-only tail; migrations are positional.
	`
ALTER TABLE event_rules ADD COLUMN scope TEXT NOT NULL DEFAULT 'addressed'
	CHECK(scope IN ('addressed', 'observer'));
	`,
	// #1246 observer ordering guard. Existing non-reply catch-all rules are the
	// global view of escalation/blocked and related undirected events. Promote
	// them before any later producer can address those kinds. Reply remains
	// addressed because it is already directed today, so changing it here would
	// not be behavior-neutral. Append-only tail; migrations are positional.
	`
UPDATE event_rules
SET scope = 'observer'
WHERE scope = 'addressed'
	AND on_kind <> 'reply'
	AND TRIM(COALESCE(match_filter, '')) = '';
	`,
	// #1206/#1304 restart-safe directive supervision. The directive remains the
	// original workflow_notes row; these additive columns persist evaluator
	// cadence across daemon restarts. -1 means inherit the global done TTL, while
	// 0 is an explicit per-directive disable.
	`
ALTER TABLE workflow_notes ADD COLUMN directive_done_ttl_seconds INTEGER NOT NULL DEFAULT -1
	CHECK(directive_done_ttl_seconds >= -1);
ALTER TABLE workflow_notes ADD COLUMN directive_nudge_count INTEGER NOT NULL DEFAULT 0
	CHECK(directive_nudge_count >= 0);
ALTER TABLE workflow_notes ADD COLUMN directive_last_nudged_at TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_workflow_notes_directive_oldest
	ON workflow_notes(created_at, id)
	WHERE substr(body, 1, length('[org:directive ')) = '[org:directive ';
	`,
	// #1250 org attribution. The branch lock is the ONE durable carrier of the
	// acting org role, read by BOTH PR-open triggers (in-process advance and the
	// daemon PR-watcher) so they cannot drift — the same role branch_locks already
	// plays for skip_native_review_fanout. Legacy rows backfill to '' , which IS
	// the unattributed polarity: readers degrade to today's behaviour rather than
	// failing, so a pre-migration lock never crashes or invents an attribution.
	`
ALTER TABLE branch_locks ADD COLUMN acting_org_role TEXT NOT NULL DEFAULT '';
	`,
	// #1352 directive nudge ladder. directive_nudge_count is CUMULATIVE and is
	// never reset at ack, so it cannot express a per-phase cap on its own — the
	// completion phase gets its own counter. directive_exhausted_at is the
	// TERMINAL, QUERYABLE state the ladder ends in: the row stays listed and
	// visible with a stamp, rather than the obligation vanishing into silence.
	// Both legacy defaults ('' and 0) mean "not yet", so pre-migration rows
	// behave exactly as they do today.
	`
ALTER TABLE workflow_notes ADD COLUMN directive_done_nudge_count INTEGER NOT NULL DEFAULT 0
	CHECK(directive_done_nudge_count >= 0);
ALTER TABLE workflow_notes ADD COLUMN directive_exhausted_at TEXT NOT NULL DEFAULT '';
	`,
	// #1368 durable awaited facts. A waiting row is the subscription; satisfied
	// and expired are terminal, queryable outcomes. The partial unique index
	// prevents duplicate live interests while allowing a later bounded wait for
	// the same subject after an earlier terminal outcome. Append-only tail;
	// migrations are positional.
	`
CREATE TABLE awaited_facts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	waiter_role TEXT NOT NULL,
	subject_kind TEXT NOT NULL,
	subject_key TEXT NOT NULL,
	deadline TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'waiting'
		CHECK(state IN ('waiting', 'satisfied', 'expired')),
	resolution_detail TEXT NOT NULL DEFAULT '',
	satisfied_at TEXT NOT NULL DEFAULT '',
	expired_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX idx_awaited_facts_live_subject
	ON awaited_facts(waiter_role, subject_kind, subject_key)
	WHERE state = 'waiting';
CREATE INDEX idx_awaited_facts_waiting_deadline
	ON awaited_facts(deadline, id)
	WHERE state = 'waiting';
	`,
	// #1344 evidence-based task disposal. The task columns keep the terminal
	// outcome and notification-routing result queryable; the episode columns cap
	// blocked alerts independently without disposing the task. Empty/zero defaults
	// preserve every pre-migration row. Append-only tail; migrations are positional
	// and this entry must never be inserted earlier in the slice.
	`
ALTER TABLE tasks ADD COLUMN disposal_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN disposal_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN disposal_at TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN disposal_escalation_role TEXT NOT NULL DEFAULT '';
ALTER TABLE org_blocked_episodes ADD COLUMN task_emit_count INTEGER NOT NULL DEFAULT 0
	CHECK(task_emit_count >= 0);
ALTER TABLE org_blocked_episodes ADD COLUMN task_exhausted_at TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_tasks_disposal_candidates
	ON tasks(repo_full_name, state, updated_at, id)
	WHERE state IN ('blocked', 'awaiting_human_merge');
	`,

	// lifecycle_generation is a MONOTONIC counter identifying one RUN of a job
	// (#1407). A job's state is a VALUE that recurs, so it cannot identify a run:
	// failed -> queued -> running -> failed returns the string to its old value,
	// and an in-flight advancement holding a result from the PREVIOUS run then
	// wins a compare-and-swap against a lifecycle it never observed. That is ABA,
	// and no amount of care at the reading sites repairs it, because the value
	// being compared is genuinely equal.
	//
	// Every transition INTO queued starts a new run, so the counter is bumped by
	// the state-writing UPDATEs themselves rather than by their callers. That is
	// the whole reason it lives here: the set of CALLERS that re-queue a job is
	// open and grows, while the set of SQL statements that write jobs.state is
	// closed and lives in one file. A guard defined against an open set decays
	// silently as the set widens; this one cannot.
	//
	// DEFAULT 0 preserves every pre-migration row: existing jobs start at
	// generation 0 and take their first bump on their next re-queue. Append-only
	// tail; migrations are positional and this entry must never be inserted
	// earlier in the slice.
	`
ALTER TABLE jobs ADD COLUMN lifecycle_generation INTEGER NOT NULL DEFAULT 0
	CHECK(lifecycle_generation >= 0);
	`,
	// #1484 permission-policy visibility. The singleton baseline lives beside the
	// live agent inventory it measures; CI has no home-scoped fleet and therefore
	// cannot honestly own this value. Warning claims make the per-agent-config
	// and capability window coalescing atomic across concurrent workers.
	`
CREATE TABLE permission_policy_observation_baseline (
	id INTEGER PRIMARY KEY CHECK(id = 1),
	affected_count INTEGER NOT NULL CHECK(affected_count >= 0),
	configs_json TEXT NOT NULL,
	recorded_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE permission_policy_warning_claims (
	agent TEXT NOT NULL,
	runtime TEXT NOT NULL,
	policy TEXT NOT NULL,
	capability TEXT NOT NULL,
	window_start TEXT NOT NULL,
	job_id TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(agent, runtime, policy, capability, window_start)
);
	`,
	// #1576 execution-backend attempt ledger. A reservation is durable before
	// provisioning begins: cost_reserved_usd is written in that same insert so
	// parallel children contend on one primary-keyed reservation. sandbox_id is
	// deliberately nullable during the provider-create/DB-write crash window, so
	// a bidirectional reaper can reconcile both ledger->provider and
	// provider-tags->ledger. daemon_fencing_token and boot_id are both required:
	// boot_id alone cannot distinguish a daemon crash-and-restart within one host
	// boot. orphaned is an explicit persisted state so reapers record their
	// conclusion instead of forcing callers to infer it.
	`
CREATE TABLE execbackend_attempts (
	job_id TEXT NOT NULL,
	attempt INTEGER NOT NULL,
	lifecycle_generation INTEGER,
	provider TEXT NOT NULL,
	sandbox_id TEXT,
	daemon_fencing_token TEXT NOT NULL,
	boot_id TEXT NOT NULL,
	ttl_expires_at TIMESTAMP NOT NULL,
	state TEXT NOT NULL CHECK(state IN (
		'reserved', 'provisioning', 'running', 'collecting', 'destroying',
		'destroyed', 'orphaned', 'failed'
	)),
	cost_reserved_usd REAL,
	cost_actual_usd REAL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (job_id, attempt, lifecycle_generation)
);
	`,
	// #1572 durable delegation-worktree cleanup obligations. Job events remain
	// audit output; this row is the restart-safe retry state. The resource id is
	// derived from (owner job, canonical expected path), while both values remain
	// explicit so an operator can prove identity before reopening a quarantine.
	// Removed is terminal; quarantined requires an explicit operator reopen.
	`
CREATE TABLE cleanup_obligations (
	resource_id TEXT PRIMARY KEY,
	resource_kind TEXT NOT NULL CHECK(resource_kind = 'delegation_worktree'),
	owner_job_id TEXT NOT NULL,
	expected_path TEXT NOT NULL,
	state TEXT NOT NULL CHECK(state IN ('pending', 'retryable', 'removed', 'quarantined')),
	reason TEXT NOT NULL CHECK(reason IN (
		'pending', 'removed', 'operator_reopened', 'terminal_cleanup_deferred',
		'context_interrupted', 'job_lookup', 'runner_resolution', 'checkout_lock',
		'identity_or_containment', 'unknown'
	)),
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
	next_attempt_at TEXT NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX idx_cleanup_obligations_owner_path
	ON cleanup_obligations(owner_job_id, expected_path);
CREATE INDEX idx_cleanup_obligations_due
	ON cleanup_obligations(state, next_attempt_at, owner_job_id)
	WHERE state IN ('pending', 'retryable');
	`,
	// #1496 durable event-rule deletion history. Wake-outbox rows deliberately
	// remain pending when no route matches; preserving the deleted rule semantics
	// lets health distinguish a route removed after a row existed from a route
	// that was never configured. Append-only tail; migrations are positional.
	`
CREATE TABLE event_rule_deletions (
	deletion_id INTEGER PRIMARY KEY AUTOINCREMENT,
	id TEXT NOT NULL,
	on_kind TEXT NOT NULL,
	match_filter TEXT,
	wake_role TEXT NOT NULL,
	scope TEXT NOT NULL,
	enabled INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	deleted_at TEXT NOT NULL
);
CREATE INDEX idx_event_rule_deletions_route
	ON event_rule_deletions(
		wake_role COLLATE NOCASE,
		on_kind COLLATE NOCASE,
		deleted_at,
		deletion_id
	)
	WHERE enabled = 1;
	`,
	// #1635 archive-agents: parked directive obligations. Parking SUSPENDS the
	// nudge ladder for a seat taken out of rotation — it is neither done (done
	// asserts a deliverable exists) nor cancel (cancel discards the obligation).
	// Suspension preserves #1418's exhaustion discrimination: an archived
	// seat's directives must not exhaust into the background rate, or
	// exhaustion stops distinguishing a real stall. Unpark clears the stamp
	// and resets the nudge anchor (directive_last_nudged_at) to unpark time so
	// a returning seat is not nagged immediately for time it spent archived.
	// Rebase note: this entry moved BEHIND #1496's event_rule_deletions when
	// main gained that tail first — append-only, a new tail, never reordered.
	`
ALTER TABLE workflow_notes ADD COLUMN directive_parked_at TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_notes ADD COLUMN directive_parked_reason TEXT NOT NULL DEFAULT '';
	`,
	// #1635 archive-agents ingest: the durable mirror of herdr-observed
	// archived seats. herdr is the SOLE authority (gitmoot never writes archive
	// state to herdr); these rows are a read cache with exactly one write site
	// (the daemon one-minute org lane) mutated only after a SUCCESSFUL
	// `herdr agent list` read. org_archive_poll records the last successful
	// poll so staleness can go LOUD (doctor) while the fail direction stays
	// preserved-exclusion: herdr-down never flips an archived seat back into
	// sweeps and nudges.
	`
CREATE TABLE org_role_archived (
	role TEXT PRIMARY KEY,
	archived_at TEXT NOT NULL,
	archived_by TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	parked_work TEXT NOT NULL DEFAULT '',
	observed_at TEXT NOT NULL
);
CREATE TABLE org_archive_poll (
	id INTEGER PRIMARY KEY CHECK(id = 1),
	last_success_at TEXT NOT NULL
);
	`,
	// #1643 round 4: the pending-observation ledger. An observed archived seat
	// is recorded HERE as the tick's FIRST write, then drained into the mirror
	// with per-row retry on every tick — so a transient failure creating the
	// first mirror row leaves durable retry state a later tick can act on even
	// when a valid list omits the role. A pending row is deleted only after
	// its mirror upsert succeeds. If this first write itself fails the tick
	// aborts with the poll stamp withheld — the failed-read equivalence, loud
	// via staleness rather than silent.
	`
CREATE TABLE org_archive_pending (
	role TEXT PRIMARY KEY,
	archived_at TEXT NOT NULL,
	archived_by TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	parked_work TEXT NOT NULL DEFAULT '',
	observed_at TEXT NOT NULL
);
	`,
	// #1634 closes the NULL-primary-key gap in the execution-backend ledger.
	// SQLite permits multiple NULL values in a composite non-rowid PRIMARY KEY,
	// so lifecycle_generation must be constrained by the schema rather than only
	// by Go callers. Rebuild at the append-only tail because SQLite cannot add a
	// NOT NULL constraint to an existing column in place. The INSERT deliberately
	// fails closed if an unexpected historical NULL exists; silently dropping or
	// coalescing such a row would erase evidence of an allocation attempt.
	`
ALTER TABLE execbackend_attempts RENAME TO execbackend_attempts_nullable_generation;
CREATE TABLE execbackend_attempts (
	job_id TEXT NOT NULL,
	attempt INTEGER NOT NULL,
	lifecycle_generation INTEGER NOT NULL CHECK(lifecycle_generation >= 0),
	provider TEXT NOT NULL,
	sandbox_id TEXT,
	daemon_fencing_token TEXT NOT NULL,
	boot_id TEXT NOT NULL,
	ttl_expires_at TIMESTAMP NOT NULL,
	state TEXT NOT NULL CHECK(state IN (
		'reserved', 'provisioning', 'running', 'collecting', 'destroying',
		'destroyed', 'orphaned', 'failed'
	)),
	cost_reserved_usd REAL,
	cost_actual_usd REAL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (job_id, attempt, lifecycle_generation)
);
INSERT INTO execbackend_attempts(
	job_id, attempt, lifecycle_generation, provider, sandbox_id,
	daemon_fencing_token, boot_id, ttl_expires_at, state,
	cost_reserved_usd, cost_actual_usd, created_at, updated_at
)
SELECT
	job_id, attempt, lifecycle_generation, provider, sandbox_id,
	daemon_fencing_token, boot_id, ttl_expires_at, state,
	cost_reserved_usd, cost_actual_usd, created_at, updated_at
FROM execbackend_attempts_nullable_generation;
DROP TABLE execbackend_attempts_nullable_generation;
	`,
	// #1679 directive receipts retire stale pending wakes. Superseded is a
	// terminal, non-delivery outcome: calling these rows delivered or failed
	// would falsify the durable audit. The rebuild extends SQLite's state CHECK;
	// the UPDATE cleans only already-terminal historical directives because an
	// acked, still-open pending row may be a legitimate completion nudge.
	`
ALTER TABLE wake_outbox RENAME TO wake_outbox_before_superseded;

CREATE TABLE wake_outbox (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source_kind TEXT NOT NULL,
	source_id TEXT NOT NULL,
	target_role TEXT NOT NULL,
	coalesce_key TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'pending'
		CHECK(state IN ('pending', 'attempted', 'delivered', 'stalled', 'failed', 'delivery_unknown', 'superseded')),
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	attempted_at TEXT,
	finished_at TEXT,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	UNIQUE(source_kind, source_id, target_role)
);

INSERT INTO wake_outbox(
	id, source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, last_error, created_at, attempted_at, finished_at, updated_at
)
SELECT
	id, source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, last_error, created_at, attempted_at, finished_at, updated_at
FROM wake_outbox_before_superseded;

DROP TABLE wake_outbox_before_superseded;

UPDATE wake_outbox
SET state = 'superseded',
	last_error = 'directive terminated before wake delivery',
	finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE state = 'pending' AND source_kind = 'workflow_note'
	AND coalesce_key LIKE 'directive:%'
	AND EXISTS (
		SELECT 1
		FROM workflow_notes d
		JOIN workflow_notes r ON r.workflow_id = d.workflow_id
		WHERE d.id = CAST(wake_outbox.source_id AS INTEGER)
			AND (
				substr(r.body, 1, length('[org:directive-cancel id=' || wake_outbox.source_id || ' ')) = '[org:directive-cancel id=' || wake_outbox.source_id || ' '
				OR substr(r.body, 1, length('[org:directive-done id=' || wake_outbox.source_id || ' ')) = '[org:directive-done id=' || wake_outbox.source_id || ' '
			)
	);

CREATE INDEX idx_wake_outbox_pending
	ON wake_outbox(target_role, coalesce_key, created_at, id)
	WHERE state = 'pending';
CREATE INDEX idx_wake_outbox_attempted
	ON wake_outbox(attempted_at, id)
	WHERE state = 'attempted';
	`,
	// #1686 per-PR auto-fix refusal. The scheduler reads this local durable
	// policy before resolving an owner or allocating a fix worktree.
	`
CREATE TABLE pull_request_auto_fix_policies (
	repo_full_name TEXT NOT NULL COLLATE NOCASE,
	pull_request INTEGER NOT NULL CHECK(pull_request > 0),
	disabled INTEGER NOT NULL CHECK(disabled IN (0, 1)),
	actor TEXT NOT NULL,
	reason TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	PRIMARY KEY (repo_full_name, pull_request)
);
	`,
	// #1714 current-head observation for the visible gitmoot/merge-gate status.
	// This state is intentionally separate from merge_gates: status bookkeeping
	// must never overwrite or gate the policy decision it describes.
	`
CREATE TABLE merge_gate_status_observations (
	repo_full_name TEXT NOT NULL COLLATE NOCASE,
	pull_request INTEGER NOT NULL CHECK(pull_request > 0),
	head_sha TEXT NOT NULL,
	kind TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	PRIMARY KEY (repo_full_name, pull_request)
);
	`,
	// #1562 durable merge-gate block classification. The gate already separates a
	// transient/infra block from an authoritative quality rejection, but that
	// classification lived only in the returned decision, so no exit path could
	// read it and a transient block became permanent. Existing rows default to 0
	// (MergeBlockNone), which is never selected for automatic re-evaluation.
	//
	// The CREATE is not redundant. A database can legitimately reach this version
	// without merge_gates: seeding schema_migrations forward past the migration
	// that created it leaves the table absent, and a bare ALTER then fails the
	// whole Migrate call, which is a refusal to start rather than a test artifact.
	// Both paths converge on the same shape, and this migration is APPENDED, never
	// reordered, so an already-migrated database still applies exactly this step.
	`
CREATE TABLE IF NOT EXISTS merge_gates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_full_name TEXT NOT NULL,
	pull_request INTEGER NOT NULL,
	state TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(repo_full_name, pull_request)
);
ALTER TABLE merge_gates ADD COLUMN block_class INTEGER NOT NULL DEFAULT 0;
	`,
	// Durable cross-process task-state claim for the irreversible native merge
	// boundary. SQLite triggers make every state writer participate, including
	// independent processes. An unresolved external outcome remains fenced
	// without expiry until an authoritative remote observation resolves it.
	`
CREATE TABLE task_state_claims (
	task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
	kind TEXT NOT NULL,
	token TEXT NOT NULL UNIQUE,
	expected_state TEXT NOT NULL,
	acquired_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX idx_task_state_claims_expires_at ON task_state_claims(expires_at);
CREATE TRIGGER guard_claimed_task_state_update
BEFORE UPDATE OF state ON tasks
WHEN OLD.state <> NEW.state
	AND EXISTS (
		SELECT 1 FROM task_state_claims
		WHERE task_id = OLD.id
			AND (kind = 'external_merge_uncertain'
				OR expires_at > CAST(strftime('%s', 'now') AS INTEGER))
	)
BEGIN
	SELECT RAISE(ABORT, 'task state is claimed for an external merge');
END;
CREATE TRIGGER guard_claimed_task_delete
BEFORE DELETE ON tasks
WHEN EXISTS (
	SELECT 1 FROM task_state_claims
	WHERE task_id = OLD.id
		AND (kind = 'external_merge_uncertain'
			OR expires_at > CAST(strftime('%s', 'now') AS INTEGER))
)
BEGIN
	SELECT RAISE(ABORT, 'task state is claimed for an external merge');
END;
	`,
	// Canonical terminal-effects ownership and secondary task-settlement debt
	// survive a daemon exit between the two phases. The exact head is part of
	// every key so a later push cannot inherit an earlier terminal decision.
	`
CREATE TABLE pull_request_terminal_reconciliations (
	repo_full_name TEXT NOT NULL COLLATE NOCASE,
	pull_request INTEGER NOT NULL CHECK(pull_request > 0),
	head_sha TEXT NOT NULL CHECK(length(trim(head_sha)) > 0),
	owner_task_id TEXT NOT NULL CHECK(length(trim(owner_task_id)) > 0),
	effects_completed INTEGER NOT NULL DEFAULT 0 CHECK(effects_completed IN (0, 1)),
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(repo_full_name, pull_request, head_sha)
);
CREATE TABLE pull_request_terminal_settlements (
	repo_full_name TEXT NOT NULL COLLATE NOCASE,
	pull_request INTEGER NOT NULL CHECK(pull_request > 0),
	head_sha TEXT NOT NULL CHECK(length(trim(head_sha)) > 0),
	task_id TEXT NOT NULL CHECK(length(trim(task_id)) > 0),
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(repo_full_name, pull_request, head_sha, task_id),
	FOREIGN KEY(repo_full_name, pull_request, head_sha)
		REFERENCES pull_request_terminal_reconciliations(repo_full_name, pull_request, head_sha)
		ON DELETE CASCADE
);
CREATE INDEX idx_pull_request_terminal_settlements_task
	ON pull_request_terminal_settlements(task_id);
	`,
	// #1684 disposable clones retained because their object database still holds
	// unpublished commits. SQLite cannot extend a CHECK in place, so the table is
	// rebuilt; every existing row keeps its state, reason and retry accounting.
	`
ALTER TABLE cleanup_obligations RENAME TO cleanup_obligations_before_unpublished;

CREATE TABLE cleanup_obligations (
	resource_id TEXT PRIMARY KEY,
	resource_kind TEXT NOT NULL CHECK(resource_kind = 'delegation_worktree'),
	owner_job_id TEXT NOT NULL,
	expected_path TEXT NOT NULL,
	state TEXT NOT NULL CHECK(state IN ('pending', 'retryable', 'removed', 'quarantined')),
	reason TEXT NOT NULL CHECK(reason IN (
		'pending', 'removed', 'operator_reopened', 'terminal_cleanup_deferred',
		'context_interrupted', 'job_lookup', 'runner_resolution', 'checkout_lock',
		'identity_or_containment', 'unpublished_commits', 'unknown'
	)),
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
	next_attempt_at TEXT NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO cleanup_obligations(
	resource_id, resource_kind, owner_job_id, expected_path, state, reason,
	attempt_count, next_attempt_at, last_error, created_at, updated_at
)
SELECT
	resource_id, resource_kind, owner_job_id, expected_path, state, reason,
	attempt_count, next_attempt_at, last_error, created_at, updated_at
FROM cleanup_obligations_before_unpublished;

DROP TABLE cleanup_obligations_before_unpublished;

CREATE UNIQUE INDEX idx_cleanup_obligations_owner_path
	ON cleanup_obligations(owner_job_id, expected_path);
CREATE INDEX idx_cleanup_obligations_due
	ON cleanup_obligations(state, next_attempt_at, owner_job_id)
	WHERE state IN ('pending', 'retryable');
	`,
	// #1752 removes the SkillOpt/evals/feedback optimization loop. This entry is
	// FORWARD-ONLY, like every one above it: the slice is APPEND-ONLY, so the
	// historical CREATE TABLEs stay exactly as written and this entry drops what they
	// built. Every table below was written only by the loop — the audit measured the
	// eval_*/skillopt_train_* tables last written 2026-07-08, feedback_events
	// 2026-06-01, and zero rows ever in skillopt_bandit_arms, skillopt_binary_verdicts,
	// skillopt_gate_runs and skillopt_judge_outcomes — and no surviving code reads or
	// writes any of them.
	//
	// ORDER MATTERS. The two candidate/canary columns on agent_template_versions can
	// only be dropped once no row still depends on the states that used them, so the
	// reconciliation runs FIRST:
	//
	//  1. Any agent_templates.latest_version_id pointing at a `pending`/`canary` row is
	//     repointed at that template's live `current` version. Without this the
	//     surviving read path (GetLatestAgentTemplateVersion, `@latest`) would keep
	//     resolving a version that is about to become terminal.
	//  2. Every remaining `pending`/`canary` version becomes `rejected`, NOT
	//     `superseded`: `superseded` is the one state RevertAgentTemplateVersion
	//     accepts, so reusing it here would let a never-reviewed candidate be reverted
	//     into `current` after the review layer that gated it is gone. `rejected` is
	//     terminal for every surviving path.
	//  3. current_version_id needs no reconciliation: a canary never held it (the
	//     champion stays current for the whole canary window) and a pending candidate
	//     never held it either.
	//
	// The partial index goes because the `canary` state it indexed no longer exists.
	// created_repos is deliberately NOT dropped: its rows name real GitHub repositories
	// gitmoot created for training runs, some of which may still exist un-cleaned, so
	// discarding that record is a separate decision from deleting the loop's code.
	`
UPDATE agent_templates
SET latest_version_id = COALESCE((
		SELECT v.id
		FROM agent_template_versions v
		WHERE v.template_id = agent_templates.id AND v.state = 'current'
		ORDER BY v.version DESC
		LIMIT 1
	), ''),
	updated_at = CURRENT_TIMESTAMP
WHERE latest_version_id IN (
	SELECT id FROM agent_template_versions WHERE state IN ('pending', 'canary')
);

UPDATE agent_template_versions
SET state = 'rejected',
	canary_sample = 0,
	canary_started_at = '',
	updated_at = CURRENT_TIMESTAMP
WHERE state IN ('pending', 'canary');

DROP INDEX IF EXISTS idx_atv_canary;

ALTER TABLE agent_template_versions DROP COLUMN canary_sample;
ALTER TABLE agent_template_versions DROP COLUMN canary_started_at;

DROP TABLE IF EXISTS agent_template_candidate_reviews;
DROP TABLE IF EXISTS skillopt_train_iterations;
DROP TABLE IF EXISTS skillopt_train_sessions;
DROP TABLE IF EXISTS skillopt_review_watches;
DROP TABLE IF EXISTS skillopt_judge_outcomes;
DROP TABLE IF EXISTS skillopt_bandit_arms;
DROP TABLE IF EXISTS skillopt_gate_runs;
DROP TABLE IF EXISTS skillopt_binary_verdicts;
DROP TABLE IF EXISTS skillopt_synth_items;
DROP TABLE IF EXISTS ranked_feedback_events;
DROP TABLE IF EXISTS feedback_events;
DROP TABLE IF EXISTS eval_review_options;
DROP TABLE IF EXISTS eval_review_items;
DROP TABLE IF EXISTS eval_runs;
DROP TABLE IF EXISTS eval_artifacts;
	`,

	// #1755 removes the retired Activepieces flow ownership state. Pipeline-chain
	// triggers remain in pipeline_trigger_states and are intentionally unaffected.
	`
ALTER TABLE pipelines DROP COLUMN trigger_binding;
	`,
	`
-- #1673: a human escalation round gets a DURABLE IDENTITY and a JOB-LEVEL
-- exclusive slot, so claim, effects and receipt are settled by identity rather
-- than by comparing aggregate event counts.
CREATE TABLE escalation_rounds (
	job_id TEXT NOT NULL CHECK(length(trim(job_id)) > 0),
	round_id TEXT NOT NULL CHECK(length(trim(round_id)) > 0),
	kind TEXT NOT NULL DEFAULT '',
	opened_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	-- resolved_at NULL => the round is LIVE (nobody has claimed its resolution).
	resolved_at TEXT,
	claim_verb TEXT NOT NULL DEFAULT '',
	claim_generation INTEGER,
	claim_payload TEXT NOT NULL DEFAULT '',
	-- effects_completed_at NULL => UNSETTLED. It is the slot predicate below, so a
	-- claimed-but-unfinished round keeps holding the coordinator's only slot: a
	-- stale replay must never be able to clear a NEWER round's live pause.
	effects_completed_at TEXT,
	-- settled_reason names WHY a settlement happened without applying effects: a
	-- Class I no-op release (the coordinator row is gone) or an operator supersede.
	settled_reason TEXT NOT NULL DEFAULT '',
	settled_by TEXT NOT NULL DEFAULT '',
	-- needs_repair is the terminal integrity state. It PRESERVES the claim and,
	-- because effects_completed_at stays NULL, keeps the slot held: no new round and
	-- no ordinary advance until an operator repairs or supersedes it.
	integrity_state TEXT NOT NULL DEFAULT '' CHECK(integrity_state IN ('', 'needs_repair')),
	integrity_cause TEXT NOT NULL DEFAULT '',
	integrity_at TEXT,
	recovery_attempts INTEGER NOT NULL DEFAULT 0,
	-- THE FENCE. Recovery is EXCLUSIVELY OWNED through effect commit: only the holder
	-- may run pre-effects, apply effects, park the round or settle it, and the fence is
	-- validated INSIDE the transaction that commits the effects. Because parking
	-- requires the fence and an operator supersede requires a parked round, a supersede
	-- and an in-flight replay are mutually exclusive rather than racing.
	recovery_owner TEXT NOT NULL DEFAULT '',
	recovery_lease_until TEXT,
	-- THE PRE-EFFECT RESOURCE RECORD. Allocating a delegation worktree and taking a
	-- branch lock are git/lock operations that cannot live inside a database
	-- transaction, so they run under the held fence BEFORE it and are recorded here.
	-- This row is what makes orphan release possible: when a round is superseded by an
	-- operator, or released because its coordinator is gone, these are the resources
	-- that must be handed back.
	preeffect_repo TEXT NOT NULL DEFAULT '',
	preeffect_branch TEXT NOT NULL DEFAULT '',
	preeffect_worktree_path TEXT NOT NULL DEFAULT '',
	preeffect_lock_owner TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(job_id, round_id)
);
-- THE EXCLUSION INVARIANT, as a schema constraint rather than a predicate a caller
-- can weaken: at most ONE unsettled round per coordinator job.
CREATE UNIQUE INDEX escalation_rounds_one_unsettled
	ON escalation_rounds(job_id) WHERE effects_completed_at IS NULL;
CREATE INDEX idx_escalation_rounds_unfinished
	ON escalation_rounds(job_id, integrity_state)
	WHERE resolved_at IS NOT NULL AND effects_completed_at IS NULL;
	`,

	// #1754 removes native chat/moot. Every reader of these tables is deleted in
	// the same change, so the rows have no surviving consumer; the escalation
	// answer path is retained through `gitmoot job answer`. Dropped children
	// first so no index or foreign-key-shaped reference outlives its parent.
	`
DROP INDEX IF EXISTS idx_chat_mentions_agent_unread;
DROP INDEX IF EXISTS idx_chat_messages_content_hash;
DROP INDEX IF EXISTS idx_chat_messages_promoted_job;
DROP INDEX IF EXISTS idx_chat_messages_thread_seq;

DELETE FROM wake_outbox WHERE source_kind = 'chat_message';

DROP TABLE IF EXISTS chat_thread_meta;
DROP TABLE IF EXISTS chat_mentions;
DROP TABLE IF EXISTS chat_messages;
DROP TABLE IF EXISTS chat_threads;
DROP TABLE IF EXISTS chat_meta;
	`,

	// #1756 removes the #33 preset delivery MODES. Measured before writing this:
	// preset_session_state held 0 rows and all 186 registered agents read
	// preset_delivery='full' on the live deployment, so `referenced`/`auto` were
	// never exercised and the only surviving behaviour is the pre-#33 one — always
	// inline the whole preset. That makes this a pure drop, not a data transition:
	// there is no non-'full' value to migrate and nothing reads the table.
	//
	// Order matters and is the reverse of the additive migration that created them:
	// the table goes first, then the column, so a failure between the two statements
	// leaves a column whose only legal value is already its DEFAULT rather than a
	// column referencing a table that no longer exists.
	//
	// The column drop is what makes this irreversible, and it is deliberate: a
	// column with one legal value is a configurable-but-behaviourless surface, which
	// is the defect this reduction exists to remove. ALTER TABLE ... DROP COLUMN is
	// the same mechanism #1752 used for the canary columns on agent_template_versions,
	// but precedent is not a measurement: both arms are proven for THIS migration by
	// TestPresetDeliveryRemovalMigration below (a fresh schema, and a pre-change
	// database seeded with real agent rows and a preset_session_state row, upgraded
	// through the real Open path).
	`
DROP TABLE IF EXISTS preset_session_state;

ALTER TABLE agents DROP COLUMN preset_delivery;
	`,

	// #1753 deleted the `gitmoot orchestrate --cockpit` pane wrapper and the
	// `gitmoot interactive` prompt command. Their tables have no remaining reader
	// or writer, so drop them rather than leaving three orphaned schemas behind.
	// Dropping a table drops its indexes with it, so
	// idx_cockpit_panes_job / idx_cockpit_panes_root /
	// idx_interactive_prompts_state need no separate statement.
	`
DROP TABLE IF EXISTS cockpit_panes;
DROP TABLE IF EXISTS cockpit_workspaces;
DROP TABLE IF EXISTS interactive_prompts;
	`,
	// #1822 findings ledger. One row per (finding, head) OBSERVATION. The key is
	// (finding_uid, head_sha) so the SAME finding at two heads is two rows and
	// nothing is ever silently carried; there is deliberately no column meaning
	// "still true". head_sha is CHECKed as 40 hex because an abbreviation reads as
	// proof it is not. finding_uid is minted by the store, never by the reviewer:
	// measured on #1783, reviewers number findings per round and F-1 named four
	// different defects across four rounds, so an obligation keyed on a
	// reviewer-supplied label is discharged by a coincidence of naming.
	// round_label keeps that label for humans and is matched on by nothing.
	`
CREATE TABLE review_finding_observations (
	finding_uid TEXT NOT NULL,
	repo TEXT NOT NULL,
	pull_request INTEGER NOT NULL DEFAULT 0,
	head_sha TEXT NOT NULL CHECK(length(head_sha) = 40 AND head_sha GLOB '[0-9a-f]*'),
	observed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	observer_job TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL CHECK(state IN ('open','answered','withdrawn','superseded')),
	severity TEXT NOT NULL DEFAULT '',
	round_label TEXT NOT NULL DEFAULT '',
	label_absent INTEGER NOT NULL DEFAULT 0,
	title TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	file TEXT NOT NULL DEFAULT '',
	line INTEGER NOT NULL DEFAULT 0,
	relevance_keys TEXT NOT NULL DEFAULT '[]',
	evidence_kind TEXT NOT NULL CHECK(evidence_kind IN ('EXECUTED','STATIC','QUOTED')),
	executed_commands TEXT NOT NULL DEFAULT '[]',
	executed_count INTEGER NOT NULL DEFAULT 0,
	evidence_locator TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL DEFAULT '',
	source_job TEXT NOT NULL DEFAULT '',
	withdraw_reason TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (finding_uid, head_sha)
);

CREATE INDEX idx_review_findings_pr ON review_finding_observations(repo, pull_request, observed_at);
	`,
	// #1850 round 2 F3. The previous head EDITED the #1822 migration in place, and
	// Migrate iterates POSITIONALLY and skips any version already recorded in
	// schema_migrations - so every database that ran the prior head kept the
	// pre-fix table forever, including every developer and reviewer daemon on this
	// box. The reviewer proved it: recreating the pre-fix table with the version
	// still marked applied and running a full Migrate left PRIMARY KEY
	// (finding_uid, head_sha) and the first-character-only hex CHECK in place, so
	// F8's second-reviewer fix and F9's CHECK fix reached nothing.
	//
	// This is the append-only remedy the file's own comments demand a dozen times.
	// It rebuilds the table with BOTH corrections and copies every existing row.
	// It is idempotent for a fresh database too: the additive migration above
	// created the old shape, this one replaces it, and the end state is identical
	// either way, so there is exactly ONE table definition a reader must trust.
	//
	// THE COPY IS FILTERED, AND THE PROOF IS WHY. My first version copied every
	// row, and TestReviewFindingRebuildUpgradesAPreFixDatabase failed with
	// 'CHECK constraint failed' - because the OLD check pinned only the first
	// character, so a pre-fix table can hold a head the NEW check rejects, and an
	// aborting migration leaves a daemon unable to start. That is strictly worse
	// than the defect being fixed, and I found it only because the directive
	// demanded an upgrade proof from a pre-fix database rather than a fresh one.
	//
	// A DROPPED ROW COULD NOT HAVE COME FROM THIS STORE: RecordReviewFindingObservation
	// has always rejected anything but a 40-hex head via headSHAPattern, on every
	// write, since the first version. So a non-conforming row can only have been
	// inserted by direct SQL, and preserving hand-written junk at the cost of
	// wedging startup is the wrong trade.
	`
CREATE TABLE review_finding_observations_1850 (
	finding_uid TEXT NOT NULL,
	repo TEXT NOT NULL,
	pull_request INTEGER NOT NULL DEFAULT 0,
	head_sha TEXT NOT NULL CHECK(length(head_sha) = 40 AND NOT head_sha GLOB '*[^0-9a-f]*'),
	observed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	observer_job TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL CHECK(state IN ('open','answered','withdrawn','superseded')),
	severity TEXT NOT NULL DEFAULT '',
	round_label TEXT NOT NULL DEFAULT '',
	label_absent INTEGER NOT NULL DEFAULT 0,
	title TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	file TEXT NOT NULL DEFAULT '',
	line INTEGER NOT NULL DEFAULT 0,
	relevance_keys TEXT NOT NULL DEFAULT '[]',
	evidence_kind TEXT NOT NULL CHECK(evidence_kind IN ('EXECUTED','STATIC','QUOTED')),
	executed_commands TEXT NOT NULL DEFAULT '[]',
	executed_count INTEGER NOT NULL DEFAULT 0,
	evidence_locator TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL DEFAULT '',
	source_job TEXT NOT NULL DEFAULT '',
	withdraw_reason TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (finding_uid, head_sha, observer_job)
);
INSERT INTO review_finding_observations_1850 SELECT
	finding_uid, repo, pull_request, head_sha, observed_at, observer_job, state,
	severity, round_label, label_absent, title, detail, file, line,
	relevance_keys, evidence_kind, executed_commands, executed_count,
	evidence_locator, rationale, source_job, withdraw_reason
FROM review_finding_observations
WHERE length(head_sha) = 40 AND NOT head_sha GLOB '*[^0-9a-f]*';
DROP TABLE review_finding_observations;
ALTER TABLE review_finding_observations_1850 RENAME TO review_finding_observations;
CREATE INDEX IF NOT EXISTS idx_review_findings_pr ON review_finding_observations(repo, pull_request, observed_at);
`,
}
