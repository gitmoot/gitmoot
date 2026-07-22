package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

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
}
