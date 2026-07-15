package db

import "context"

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

// DashboardChangeCursor returns the three monotonic row ids that invalidate
// dashboard views. All maxima are read in one statement so a poll is one cheap
// SQLite round trip and an empty store has the stable cursor 0, 0, 0.
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
		j.id, j.agent, j.type, j.state,
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
			&item.ID, &item.Agent, &item.Type, &item.State, &item.Instructions,
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

func (s *Store) ListDashboardTasks(ctx context.Context, mergedSince string) ([]DashboardTaskRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.title, t.repo_full_name, t.state,
		COALESCE((SELECT bl.owner FROM branch_locks bl
			WHERE bl.repo_full_name = t.repo_full_name AND bl.branch = t.branch LIMIT 1), ''),
		COALESCE((SELECT pr.number FROM pull_requests pr
			WHERE pr.repo_full_name = t.repo_full_name AND pr.head_branch = t.branch
			ORDER BY pr.number DESC LIMIT 1), 0),
		t.updated_at
	FROM tasks t
	WHERE t.state != 'dismissed'
		AND (t.state != 'merged' OR julianday(t.updated_at) >= julianday(?))
	ORDER BY t.id`, mergedSince)
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

func (s *Store) ListDashboardActiveJobs(ctx context.Context) ([]DashboardJobRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, state, workflow_id, repo, created_at, updated_at
	FROM jobs WHERE state IN ('queued', 'running') ORDER BY created_at, id`)
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

func (s *Store) ListDashboardBlockedJobs(ctx context.Context) ([]DashboardJobRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.id, j.agent, j.state, j.workflow_id, j.repo,
		j.payload, j.created_at, j.updated_at,
		COALESCE(NULLIF(j.blocker_suggested_action, ''),
			(SELECT e.message FROM job_events e WHERE e.job_id = j.id ORDER BY e.id DESC LIMIT 1), '')
	FROM jobs j WHERE j.state = 'blocked' ORDER BY j.updated_at DESC, j.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardJobRow{}
	for rows.Next() {
		var item DashboardJobRow
		if err := rows.Scan(&item.ID, &item.Agent, &item.State, &item.WorkflowID, &item.Repo,
			&item.Payload, &item.CreatedAt, &item.UpdatedAt, &item.Reason); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListDashboardTerminalBuckets(ctx context.Context, since, now string) ([]DashboardTerminalBucket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state,
		CAST((julianday(?) - julianday(updated_at)) * 24 AS INTEGER) AS age_hours,
		COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0)
	FROM jobs
	WHERE state IN ('succeeded', 'failed', 'cancelled')
		AND julianday(updated_at) >= julianday(?) AND julianday(updated_at) <= julianday(?)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, state, payload, created_at, updated_at
	FROM jobs
	WHERE state IN ('succeeded', 'failed', 'cancelled') AND julianday(updated_at) >= julianday(?)
	ORDER BY updated_at DESC, id DESC LIMIT ?`, since, limit)
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
		SUM(CASE WHEN julianday(created_at) >= julianday(?) THEN 1 ELSE 0 END)
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
