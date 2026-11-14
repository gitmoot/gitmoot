package db

import (
	"context"
	"strconv"
)

// DashboardTaskRow is the task-list projection consumed by the web dashboard.
// CI is deliberately absent: Gitmoot does not persist a current CI conclusion,
// and dashboard reads must never turn into per-request GitHub calls.
type DashboardTaskRow struct {
	ID        string
	Title     string
	Repo      string
	State     string
	Agent     string
	PRNumber  int
	UpdatedAt string
}

// DashboardJobRow is a bounded projection used by the overview summaries.
// Payload is populated only by ListDashboardNotableJobs.
type DashboardJobRow struct {
	ID           string
	Agent        string
	State        string
	WorkflowID   string
	Repo         string
	Payload      string
	InputTokens  int
	OutputTokens int
	CreatedAt    string
	UpdatedAt    string
	Reason       string
}

// DashboardJobSummaryRow is the payload-slim projection consumed by the web
// dashboard's /api/jobs response. It deliberately carries only the scalar job
// columns and four payload values that JobSummary needs. RegisteredRuntime is
// joined from agents; EphemeralRuntime is the fallback for unregistered inline
// workers.
type DashboardJobSummaryRow struct {
	ID                string
	Agent             string
	Type              string
	State             string
	Model             string
	Instructions      string
	ParentJobID       string
	DelegationDepth   int
	RegisteredRuntime string
	EphemeralRuntime  string
	Repo              string
	PullRequest       int
	InputTokens       int
	OutputTokens      int
	UpdatedAt         string
	CreatedAt         string
}

type DashboardFleetCount struct {
	Agent     string
	Running   int
	JobsToday int
}

// DashboardTerminalBucket is one rolling-hour/state aggregate. AgeHours is 0
// for the current trailing hour and 23 for the oldest hour in the 24h window.
type DashboardTerminalBucket struct {
	State        string
	AgeHours     int
	Jobs         int
	InputTokens  int
	OutputTokens int
}

const dashboardChangeCursorSQL = `SELECT
	COALESCE((SELECT MAX(id) FROM job_events), 0),
	COALESCE((SELECT MAX(id) FROM workflow_notes), 0),
	COALESCE((SELECT MAX(id) FROM task_events), 0)`

// DashboardChangeCursor returns the monotonic row ids that invalidate
// dashboard views. All maxima are read in one statement so a poll is one cheap
// SQLite round trip and an empty store has the stable all-zero cursor
// (job events, workflow notes, task events). A 4th component read
// memory_events until #2202 retired the brain and dropped that table — the term
// had to go with it, because a dropped table makes the whole statement fail.
func (s *Store) DashboardChangeCursor(ctx context.Context) (jobEventID, workflowNoteID, taskEventID int64, err error) {
	err = s.db.QueryRowContext(ctx, dashboardChangeCursorSQL).Scan(&jobEventID, &workflowNoteID, &taskEventID)
	return jobEventID, workflowNoteID, taskEventID, err
}

// ListDashboardJobSummaries avoids materializing the full jobs.payload corpus
// for /api/jobs. Current rows use the denormalized repo/pull_request columns;
// pre-column rows fall back to their legacy payload values. Every json_extract
// is guarded because modernc SQLite rejects malformed JSON, which the legacy Go
// parser tolerated by returning an empty payload.
func (s *Store) ListDashboardJobSummaries(ctx context.Context) ([]DashboardJobSummaryRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		j.id, j.agent, j.type, j.state, j.model,
		CASE WHEN json_valid(j.payload) AND json_type(j.payload, '$.instructions') = 'text'
			THEN json_extract(j.payload, '$.instructions') ELSE '' END,
		j.parent_job_id, j.delegation_depth, COALESCE(a.runtime, ''),
		CASE WHEN json_valid(j.payload) AND json_type(j.payload, '$.ephemeral.runtime') = 'text'
			THEN json_extract(j.payload, '$.ephemeral.runtime') ELSE '' END,
		COALESCE(NULLIF(j.repo, ''), CASE
			WHEN json_valid(j.payload) AND json_type(j.payload, '$.repo') = 'text'
			THEN json_extract(j.payload, '$.repo') ELSE '' END),
		CASE WHEN j.pull_request != 0 THEN j.pull_request
			WHEN json_valid(j.payload) AND json_type(j.payload, '$.pull_request') = 'integer'
			THEN json_extract(j.payload, '$.pull_request') ELSE 0 END,
		j.input_tokens, j.output_tokens, j.updated_at, j.created_at
	FROM jobs j LEFT JOIN agents a ON a.name = j.agent
	ORDER BY j.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardJobSummaryRow{}
	for rows.Next() {
		var item DashboardJobSummaryRow
		if err := rows.Scan(
			&item.ID, &item.Agent, &item.Type, &item.State, &item.Model, &item.Instructions,
			&item.ParentJobID, &item.DelegationDepth, &item.RegisteredRuntime,
			&item.EphemeralRuntime, &item.Repo, &item.PullRequest,
			&item.InputTokens, &item.OutputTokens, &item.UpdatedAt, &item.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListDashboardTasks reads every visible task with its latest PR number and the
// PR implementer's acting org role in one query. The owner column is the same
// pullRequestActingRoleSelector that PullRequestActingRole runs, evaluated once
// per distinct in-scope (repo, PR) rather than per task: a task without a PR is
// never looked up, and the PR-number subquery is materialized before the
// selector runs, so the planner cannot re-evaluate it for every candidate job
// (#2639: 847k jobs row visits and ~30 s per request on the live store).
func (s *Store) ListDashboardTasks(ctx context.Context, mergedSince string) ([]DashboardTaskRow, error) {
	rows, err := s.db.QueryContext(ctx, `WITH task_scope AS MATERIALIZED (
		SELECT t.id, t.title, t.repo_full_name AS repo, t.state, t.updated_at,
			(SELECT pr.number FROM pull_requests pr
				WHERE pr.repo_full_name = t.repo_full_name AND pr.head_branch = t.branch
				ORDER BY pr.number DESC LIMIT 1) AS pull_request
		FROM tasks t
		WHERE t.state != 'dismissed'
			AND (t.state != 'merged' OR julianday(t.updated_at) >= julianday(?))
	), pr_owner AS MATERIALIZED (
		SELECT acting_scope.repo, acting_scope.pull_request,
			`+pullRequestActingRoleSelector+` AS owner
		FROM (SELECT DISTINCT repo, pull_request FROM task_scope
			WHERE pull_request IS NOT NULL) acting_scope
	)
	SELECT task_scope.id, task_scope.title, task_scope.repo, task_scope.state,
		COALESCE(pr_owner.owner, ''),
		COALESCE(task_scope.pull_request, 0),
		task_scope.updated_at
	FROM task_scope LEFT JOIN pr_owner
		ON pr_owner.repo = task_scope.repo AND pr_owner.pull_request = task_scope.pull_request
	ORDER BY task_scope.id`, mergedSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardTaskRow{}
	for rows.Next() {
		var item DashboardTaskRow
		if err := rows.Scan(&item.ID, &item.Title, &item.Repo, &item.State, &item.Agent, &item.PRNumber, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListDashboardActiveJobs reads the queued and running jobs oldest first. Each
// state is its own arm so the queued and running partial indexes bound the
// read; a single IN predicate matches neither and scans every job (#2639).
func (s *Store) ListDashboardActiveJobs(ctx context.Context) ([]DashboardJobRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, state, workflow_id, repo, created_at, updated_at
	FROM jobs WHERE state = 'queued'
	UNION ALL
	SELECT id, agent, state, workflow_id, repo, created_at, updated_at
	FROM jobs WHERE state = 'running'
	ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardJobRow{}
	for rows.Next() {
		var item DashboardJobRow
		if err := rows.Scan(&item.ID, &item.Agent, &item.State, &item.WorkflowID, &item.Repo, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListDashboardUnlabeledJobs reads only recent rows whose denormalized workflow
// label is empty. Callers parse payload only to apply the sender/delegation
// eligibility predicate, avoiding an overview-wide payload scan.
func (s *Store) ListDashboardUnlabeledJobs(ctx context.Context, since string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workflow_id, payload, created_at
	FROM jobs WHERE workflow_id = '' AND created_at >= ? ORDER BY created_at, id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var item Job
		if err := rows.Scan(&item.ID, &item.WorkflowID, &item.Payload, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// PhaseProfileEventKind is the diagnostic job event carrying a review's
// command-phase accounting (#1824). It lives here rather than in internal/cli
// because the operator-facing fallbacks that must EXCLUDE it are SQL in this
// package: a diagnostic row appended after a job's terminal event would
// otherwise be shown to an operator as the reason a job is blocked.
const PhaseProfileEventKind = "phase_profile"

// diagnosticJobEventKinds are event kinds that describe a job's MEASUREMENT
// rather than its state. Any query that shows an operator "the latest thing
// that happened" must exclude them, or accounting JSON is displayed where an
// actionable blocker belongs (#1824 review F6: a blocked-review probe returned
// the profile JSON as the Needs You card's reason).
const diagnosticJobEventKinds = `('` + PhaseProfileEventKind + `')`

func (s *Store) ListDashboardBlockedJobs(ctx context.Context) ([]DashboardJobRow, error) {
	out, err := queryList(ctx, s.db, `SELECT j.id, j.agent, j.state, j.workflow_id, j.repo,
		j.payload, j.created_at, j.updated_at,
		COALESCE(NULLIF(j.blocker_suggested_action, ''),
			(SELECT e.message FROM job_events e WHERE e.job_id = j.id
				AND e.kind NOT IN `+diagnosticJobEventKinds+`
				ORDER BY e.id DESC LIMIT 1), '')
	FROM jobs j WHERE j.state = 'blocked' ORDER BY j.updated_at DESC, j.id`, nil,
		func(row rowScanner) (DashboardJobRow, error) {
			var item DashboardJobRow
			err := row.Scan(&item.ID, &item.Agent, &item.State, &item.WorkflowID, &item.Repo,
				&item.Payload, &item.CreatedAt, &item.UpdatedAt, &item.Reason)
			return item, err
		})
	// emptyIfNil (query_list.go) is the whole contract, including the QUERY-time
	// divergence this justification used to omit: on an early failure these
	// methods return a non-nil len-0 slice with the error, where the pre-#1759
	// bodies returned nil. Stated once there rather than restated here, because
	// six copies of a justification drift and the previous six were already
	// silent about half of it (#1795 review N4).
	return emptyIfNil(out), err
}

// ListDashboardTerminalBuckets aggregates terminal jobs per state and trailing
// hour. The window predicates here and in the notable/fleet reads below compare
// text: jobs.created_at and jobs.updated_at are always written as SQLite
// CURRENT_TIMESTAMP ("YYYY-MM-DD HH:MM:SS"), the format the dashboard binds, so
// the order is the one julianday() gives, but idx_jobs_terminal_state_updated_at
// and idx_jobs_agent_state_created can serve them instead of a full jobs scan
// (#2639).
func (s *Store) ListDashboardTerminalBuckets(ctx context.Context, since, now string) ([]DashboardTerminalBucket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state,
		CAST((julianday(?) - julianday(updated_at)) * 24 AS INTEGER) AS age_hours,
		COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0)
	FROM jobs
	WHERE state IN ('succeeded', 'failed', 'cancelled')
		AND updated_at >= ? AND updated_at <= ?
	GROUP BY state, age_hours ORDER BY age_hours DESC, state`, now, since, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardTerminalBucket{}
	for rows.Next() {
		var item DashboardTerminalBucket
		if err := rows.Scan(&item.State, &item.AgeHours, &item.Jobs, &item.InputTokens, &item.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListDashboardNotableJobs(ctx context.Context, since string, limit int) ([]DashboardJobRow, error) {
	if limit <= 0 {
		return []DashboardJobRow{}, nil
	}
	// The inner select picks the newest ids from the covering state index; only
	// those rows are read from the table, so the payload of every other job in
	// the window is never materialized.
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, state, payload, created_at, updated_at
	FROM jobs
	WHERE id IN (SELECT id FROM jobs
		WHERE state IN ('succeeded', 'failed', 'cancelled') AND updated_at >= ?
		ORDER BY updated_at DESC, id DESC LIMIT ?)
	ORDER BY updated_at DESC, id DESC`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardJobRow{}
	for rows.Next() {
		var item DashboardJobRow
		if err := rows.Scan(&item.ID, &item.Agent, &item.State, &item.Payload, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListDashboardFleetCounts(ctx context.Context, since string) ([]DashboardFleetCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent,
		SUM(CASE WHEN state = 'running' THEN 1 ELSE 0 END),
		SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END)
	FROM jobs WHERE agent != '' GROUP BY agent ORDER BY agent`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardFleetCount{}
	for rows.Next() {
		var item DashboardFleetCount
		if err := rows.Scan(&item.Agent, &item.Running, &item.JobsToday); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// dashboardPayloadTextSQL projects one top-level text field of jobs.payload, or
// the empty string when the payload is not valid JSON or the field is absent or
// not text.
// modernc SQLite rejects json_extract over malformed JSON, which the Go payload
// decoder tolerated by returning an empty payload, so every extract is guarded.
func dashboardPayloadTextSQL(field string) string {
	return `CASE WHEN json_valid(payload) AND json_type(payload, '$.` + field + `') = 'text'
		THEN json_extract(payload, '$.` + field + `') ELSE '' END`
}

// dashboardInstructionsPrefixRunes bounds the instructions text the dashboard
// reads per job. Titles are only the first non-empty line capped at 60 runes,
// so the prefix after leading ASCII whitespace is enough, and the rest of a
// multi-kilobyte prompt never crosses into Go.
const dashboardInstructionsPrefixRunes = 1024

var dashboardInstructionsPrefixSQL = `substr(ltrim(` + dashboardPayloadTextSQL("instructions") + `, ' ' || char(9, 10, 11, 12, 13)), 1, ` + strconv.Itoa(dashboardInstructionsPrefixRunes) + `)`

// CountDashboardJobsByState returns the job count per raw state in one
// aggregate over a covering index (idx_jobs_agent_state_created); no row or
// payload is read.
func (s *Store) CountDashboardJobsByState(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		out[state] += count
	}
	return out, rows.Err()
}

// DashboardJobTitleRow is a payload-free job projection for dashboard lists:
// Instructions is only the leading dashboardInstructionsPrefixRunes of the
// prompt, enough for a title. Repo is the denormalized column, falling back to
// the payload repo for rows written before the column existed.
type DashboardJobTitleRow struct {
	ID           string
	Agent        string
	Repo         string
	Instructions string
	UpdatedAt    string
	CreatedAt    string
}

// ListDashboardRecentFailures returns the newest limit failed jobs by
// (updated_at, id) descending, read backwards along
// idx_jobs_terminal_state_updated_at, so only the returned rows touch the table.
// The redundant IN term is that partial index's WHERE clause, which SQLite
// only uses when the query repeats it.
func (s *Store) ListDashboardRecentFailures(ctx context.Context, limit int) ([]DashboardJobTitleRow, error) {
	if limit <= 0 {
		return []DashboardJobTitleRow{}, nil
	}
	out, err := queryList(ctx, s.db, `SELECT id, agent,
		COALESCE(NULLIF(repo, ''), `+dashboardPayloadTextSQL("repo")+`),
		`+dashboardInstructionsPrefixSQL+`,
		updated_at, created_at
	FROM jobs WHERE state = 'failed' AND state IN ('succeeded', 'failed', 'cancelled')
	ORDER BY updated_at DESC, id DESC LIMIT ?`, []any{limit},
		func(row rowScanner) (DashboardJobTitleRow, error) {
			var item DashboardJobTitleRow
			err := row.Scan(&item.ID, &item.Agent, &item.Repo, &item.Instructions, &item.UpdatedAt, &item.CreatedAt)
			return item, err
		})
	return emptyIfNil(out), err
}

// DashboardGraphJobRow is the galaxy graph's per-job projection: scalar columns
// plus the two payload fields a node title can use. It never carries the
// payload itself.
type DashboardGraphJobRow struct {
	ID           string
	Agent        string
	Type         string
	State        string
	ParentJobID  string
	DelegationID string
	WorkflowID   string
	Repo         string
	InputTokens  int
	OutputTokens int
	TaskTitle    string
	Instructions string
}

// ListDashboardGraphJobs returns every job created at or after since (a
// "YYYY-MM-DD HH:MM:SS" bound, compared as text), in (created_at, id) order so
// idx_jobs_created_at_repo bounds the read to the window; an id-ordered scan
// would read every row's created_at past its payload. Titles come from
// json_extract in SQL rather than a Go payload decode (#2639).
func (s *Store) ListDashboardGraphJobs(ctx context.Context, since string) ([]DashboardGraphJobRow, error) {
	out, err := queryList(ctx, s.db, `SELECT id, agent, type, state, parent_job_id, delegation_id,
		workflow_id, repo, input_tokens, output_tokens,
		`+dashboardPayloadTextSQL("task_title")+`,
		`+dashboardInstructionsPrefixSQL+`
	FROM jobs WHERE created_at >= ?
	ORDER BY created_at, id`, []any{since},
		func(row rowScanner) (DashboardGraphJobRow, error) {
			var item DashboardGraphJobRow
			err := row.Scan(&item.ID, &item.Agent, &item.Type, &item.State, &item.ParentJobID, &item.DelegationID,
				&item.WorkflowID, &item.Repo, &item.InputTokens, &item.OutputTokens, &item.TaskTitle, &item.Instructions)
			return item, err
		})
	return emptyIfNil(out), err
}

// DashboardJobRepoExists reports whether any job's denormalized repo is repo,
// one seek on idx_jobs_repo.
func (s *Store) DashboardJobRepoExists(ctx context.Context, repo string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE repo = ?)`, repo).Scan(&exists)
	return exists, err
}
