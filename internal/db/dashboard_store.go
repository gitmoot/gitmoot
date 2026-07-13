package db

import (
	"context"
	"strings"
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

// DashboardAutoWorkflow is one synthetic adhoc workflow group built from
// scalar columns on unlabeled, non-pipeline jobs. Repos is already sorted and
// de-duplicated by the SQL projection.
type DashboardAutoWorkflow struct {
	Summary WorkflowSummary
	Repos   []string
}

const dashboardChangeCursorSQL = `SELECT
	COALESCE((SELECT MAX(id) FROM job_events), 0),
	COALESCE((SELECT MAX(id) FROM workflow_notes), 0)`

// DashboardChangeCursor returns the two monotonic row ids that invalidate
// dashboard views. Both maxima are read in one statement so a poll is one cheap
// SQLite round trip and an empty store has the stable cursor components 0, 0.
func (s *Store) DashboardChangeCursor(ctx context.Context) (jobEventID, workflowNoteID int64, err error) {
	err = s.db.QueryRowContext(ctx, dashboardChangeCursorSQL).Scan(&jobEventID, &workflowNoteID)
	return jobEventID, workflowNoteID, err
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
	WHERE t.state != 'merged' OR julianday(t.updated_at) >= julianday(?)
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

// ListDashboardAutoWorkflows groups unlabeled, non-pipeline jobs by assigned
// agent without reading payloads. Pipeline stage jobs are excluded entirely.
func (s *Store) ListDashboardAutoWorkflows(ctx context.Context) ([]DashboardAutoWorkflow, error) {
	rows, err := s.db.QueryContext(ctx, `WITH pipeline_jobs AS (
		SELECT prs.job_id, MIN(pr.pipeline) AS pipeline
		FROM pipeline_run_stages prs
		JOIN pipeline_runs pr ON pr.id = prs.run_id
		WHERE prs.job_id != '' AND pr.pipeline != ''
		GROUP BY prs.job_id
	), auto_jobs AS (
		SELECT 'adhoc/' || j.agent AS workflow_id,
		j.state, j.input_tokens, j.output_tokens, j.created_at, j.updated_at, j.repo
		FROM jobs j
		LEFT JOIN pipeline_jobs pj ON pj.job_id = j.id
		WHERE j.workflow_id = '' AND pj.pipeline IS NULL AND j.agent != ''
	)
	SELECT workflow_id,
		COUNT(*),
		SUM(CASE WHEN state = 'queued' THEN 1 ELSE 0 END),
		SUM(CASE WHEN state = 'running' THEN 1 ELSE 0 END),
		SUM(CASE WHEN state = 'succeeded' THEN 1 ELSE 0 END),
		SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END),
		SUM(CASE WHEN state = 'blocked' THEN 1 ELSE 0 END),
		SUM(CASE WHEN state = 'cancelled' THEN 1 ELSE 0 END),
		COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		MIN(created_at), MAX(updated_at),
		COALESCE(MAX(CASE WHEN state IN ('failed', 'blocked') THEN updated_at END), ''),
		COALESCE(GROUP_CONCAT(DISTINCT NULLIF(repo, '')), '')
	FROM auto_jobs GROUP BY workflow_id ORDER BY workflow_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardAutoWorkflow{}
	for rows.Next() {
		var item DashboardAutoWorkflow
		var repos string
		if err := rows.Scan(&item.Summary.WorkflowID, &item.Summary.JobCount,
			&item.Summary.Queued, &item.Summary.Running, &item.Summary.Succeeded,
			&item.Summary.Failed, &item.Summary.Blocked, &item.Summary.Cancelled,
			&item.Summary.InputTokens, &item.Summary.OutputTokens,
			&item.Summary.FirstAt, &item.Summary.LastAt, &item.Summary.LastFailureAt,
			&repos); err != nil {
			return nil, err
		}
		if repos != "" {
			item.Repos = strings.Split(repos, ",")
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
