package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

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

type jobPayloadProjection struct {
	WorkflowID             string          `json:"workflow_id"`
	Repo                   string          `json:"repo"`
	PullRequest            int             `json:"pull_request"`
	BlockerRetryAt         string          `json:"blocker_retry_at"`
	BlockerSuggestedAction string          `json:"blocker_suggested_action"`
	Result                 json.RawMessage `json:"result"`
}

func jobProjectionFromPayload(payload string) jobPayloadProjection {
	var p jobPayloadProjection
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return jobPayloadProjection{}
	}
	return p
}

func jobResultHashFromPayload(payload string) string {
	projection := jobProjectionFromPayload(payload)
	raw := bytes.TrimSpace(projection.Result)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		raw = compact.Bytes()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateJob(ctx context.Context, job Job) error {
	// root_id is a denormalized index of the engine's rootJobID() rule (#420):
	// bind the SAME COALESCE(NULLIF(?,''), ?) to (payload.RootJobID, job.ID) so
	// the invariant — payload root when set, else self-root — holds regardless of
	// caller. payload.RootJobID stays the value source of truth.
	projection := jobProjectionFromPayload(job.Payload)
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload, model, result_hash, parent_job_id, delegation_id, delegation_depth, delegated_by, root_id, workflow_id, repo, pull_request, blocker_retry_at, blocker_suggested_action, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), ?), ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		job.ID, job.Agent, job.Type, job.State, job.Payload, job.Model,
		jobResultHashFromPayload(job.Payload),
		job.ParentJobID, job.DelegationID, job.DelegationDepth, job.DelegatedBy,
		rootIDFromPayload(job.Payload), job.ID, projection.WorkflowID, projection.Repo, projection.PullRequest,
		projection.BlockerRetryAt, projection.BlockerSuggestedAction)
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
	projection := jobProjectionFromPayload(job.Payload)
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload, model, result_hash, parent_job_id, delegation_id, delegation_depth, delegated_by, root_id, workflow_id, repo, pull_request, blocker_retry_at, blocker_suggested_action, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), ?), ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		job.ID, job.Agent, job.Type, job.State, job.Payload, job.Model,
		jobResultHashFromPayload(job.Payload),
		job.ParentJobID, job.DelegationID, job.DelegationDepth, job.DelegatedBy,
		rootIDFromPayload(job.Payload), job.ID, projection.WorkflowID, projection.Repo, projection.PullRequest,
		projection.BlockerRetryAt, projection.BlockerSuggestedAction); err != nil {
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

// CreateExternallyDrivenJobWithEvent creates a session job (#657) whose execution
// happens OUTSIDE the engine: it sets externally_driven = 1 so the daemon's
// queued selector never claims it and the stuck-running reaper skips it. It
// mirrors CreateJobWithEvent (same root_id denormalization, same in-tx event) but
// stamps the extra column; the caller supplies job.State = 'running' so the job
// is created directly running, never queued. This is the ONLY insert site that
// sets externally_driven — every other job insert leaves it at its DEFAULT 0, so
// the normal dispatch path is byte-identical.
func (s *Store) CreateExternallyDrivenJobWithEvent(ctx context.Context, job Job, event JobEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	projection := jobProjectionFromPayload(job.Payload)
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload, model, result_hash, parent_job_id, delegation_id, delegation_depth, delegated_by, root_id, workflow_id, repo, pull_request, blocker_retry_at, blocker_suggested_action, externally_driven, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), ?), ?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP)`,
		job.ID, job.Agent, job.Type, job.State, job.Payload, job.Model,
		jobResultHashFromPayload(job.Payload),
		job.ParentJobID, job.DelegationID, job.DelegationDepth, job.DelegatedBy,
		rootIDFromPayload(job.Payload), job.ID, projection.WorkflowID, projection.Repo, projection.PullRequest,
		projection.BlockerRetryAt, projection.BlockerSuggestedAction); err != nil {
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
	row := s.db.QueryRowContext(ctx, `SELECT id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, workflow_id, root_killed, input_tokens, output_tokens, updated_at, created_at, externally_driven FROM jobs WHERE id = ?`, id)
	var job Job
	if err := row.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.WorkflowID, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt, &job.CreatedAt, &job.ExternallyDriven); err != nil {
		return Job{}, err
	}
	return job, nil
}

// jobColumns is the shared core projection ListJobs and ListJobsByType both
// read, kept as one const so their SELECT lists and scanJobs order cannot drift.
// Workflow-only scalar projections are selected by ListJobsByWorkflow.
const jobColumns = `id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, workflow_id, root_killed, input_tokens, output_tokens, updated_at, created_at, externally_driven`

// scanJobs reads every row of a *sql.Rows produced by a `SELECT `+jobColumns+`
// FROM jobs …` query into Jobs, in jobColumns order, and closes rows. Shared by
// ListJobs and ListJobsByType so their identical scan lives in exactly one place.
func scanJobs(rows *sql.Rows) ([]Job, error) {
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.WorkflowID, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt, &job.CreatedAt, &job.ExternallyDriven); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

// ListActiveJobs returns only queued/running jobs in stable id order. Keeping
// the state predicate in SQLite avoids decoding the terminal job backlog (and
// its potentially large payloads) on the merge gate's final branch-activity
// check, reducing both database work and the check-to-merge interval.
//
// "Active" is deliberately queued+running (jobs that are executing or about to),
// NOT blocked: a blocked job is settled and may never be resumed, so holding the
// merge gate for one would let an abandoned blocked job livelock a PR's merge
// forever. Resuming a blocked job re-enqueues a fresh queued job, which this
// predicate then catches — so the only exposure is the narrow window while a
// stale blocked job sits un-retried, and there the visible resume failure is
// preferable to an indefinite merge hold.
func (s *Store) ListActiveJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE state IN (?, ?) ORDER BY id`, "queued", "running")
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

func (s *Store) ListTranscriptJobs(ctx context.Context) ([]TranscriptJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, state, updated_at, created_at FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []TranscriptJob
	for rows.Next() {
		var job TranscriptJob
		if err := rows.Scan(&job.ID, &job.State, &job.UpdatedAt, &job.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListJobsByType returns every job of the given type, with the SAME 14-column
// projection and ORDER BY id as ListJobs, filtered in SQL by `type = ?`. The PR
// poll path (#619) only ever needs review jobs, but was calling ListJobs and
// discarding non-review rows in Go — materializing the whole 37.8MB payload column
// (including one 11MB implement payload) on every open PR every sweep. Because the
// `type` column precedes `payload` in the row record, SQLite's `type = ?` filter
// decides to keep a row before it ever touches the payload's overflow pages, so
// this decodes payload only for matching rows (~237KB of review payloads vs 37.8MB
// total on the affected DB). Behavior is byte-identical to ListJobs + a Go type
// filter; only the avoided payload reads differ. EQP: `SCAN jobs USING INDEX
// sqlite_autoindex_jobs_1` over the same rows — a dedicated jobs(type) index was
// not worth its per-insert write cost and is deferred.
func (s *Store) ListJobsByType(ctx context.Context, jobType string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE type = ? ORDER BY id`, jobType)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

// ListJobsByParent returns the direct children of parentJobID (delegation
// children and the coordinator continuation job), ordered by delegation_id then
// id for a stable tree. It selects updated_at like ListJobs so callers can show
// child age; the idx_jobs_parent_job_id index backs the filter.
func (s *Store) ListJobsByParent(ctx context.Context, parentJobID string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens, updated_at
		FROM jobs WHERE parent_job_id = ? ORDER BY delegation_id, id`, parentJobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, root_id, workflow_id, repo, pull_request, root_killed, input_tokens, output_tokens, updated_at, created_at, result_hash
		FROM jobs WHERE root_id = ? ORDER BY id`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootID, &job.WorkflowID, &job.Repo, &job.PullRequest, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt, &job.CreatedAt, &job.ResultHash); err != nil {
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

// listQueuedJobsSQL is ListQueuedJobs' exact query, exported as a package-level const
// so the plan test (TestListQueuedJobsUsesQueuedIndex) can EXPLAIN QUERY PLAN the
// PRODUCTION text rather than a hand-copied duplicate — a change to this query is then
// what the test actually asserts a plan for.
const listQueuedJobsSQL = `SELECT id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens
		FROM jobs WHERE state = 'queued' AND externally_driven = 0 ORDER BY created_at, rowid`

// ListQueuedJobs returns the queued jobs in created_at (then rowid) order. The
// state predicate is the SQL literal 'queued' — not a bound parameter — so SQLite
// can prove it matches the partial index idx_jobs_queued_created (WHERE
// state='queued') and read the queued rows in order directly; a bound `state = ?`
// leaves the planner unable to prove the partial predicate, so it full-scans jobs
// and builds a temp b-tree for the ORDER BY every worker tick (#619, verified by
// EXPLAIN QUERY PLAN). 'queued' is a fixed constant, so inlining it is safe.
//
// The `externally_driven = 0` predicate defends the session-job invariant (#657):
// a "here"/prompt-import job is executed by the calling session, never the engine,
// so it must never be claimed off the queue and Delivered to a runtime even if some
// path forced an externally_driven row to state='queued'. Session jobs are created
// directly running so they normally never reach the queue at all; this is the
// belt-and-suspenders guard mirroring the reaper's exemption below. It is a residual
// filter on top of the partial-index scan, so the query plan is unchanged.
func (s *Store) ListQueuedJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, listQueuedJobsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// CountQueuedJobsForRepo returns the queued engine-owned jobs targeting repo.
// The literal queued predicate lets SQLite use idx_jobs_queued_created; repo is
// then a residual filter over the normally small queued set.
func (s *Store) CountQueuedJobsForRepo(ctx context.Context, repo string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs
		WHERE state = 'queued' AND externally_driven = 0 AND repo = ?`, strings.TrimSpace(repo)).Scan(&count)
	return count, err
}

const countCurrentJobsByOrgRoleRunningSQL = `SELECT json_extract(payload, '$.acting_org_role') AS role, COUNT(*)
	FROM jobs INDEXED BY idx_jobs_running_updated_at
	WHERE state = 'running' AND json_valid(payload)
	AND json_type(payload, '$.acting_org_role') = 'text'
	AND json_extract(payload, '$.acting_org_role') <> ''
	GROUP BY role`

const countCurrentJobsByOrgRoleQueuedSQL = `SELECT json_extract(payload, '$.acting_org_role') AS role, COUNT(*)
	FROM jobs INDEXED BY idx_jobs_queued_created
	WHERE state = 'queued' AND json_valid(payload)
	AND json_type(payload, '$.acting_org_role') = 'text'
	AND json_extract(payload, '$.acting_org_role') <> ''
	GROUP BY role`

// CountCurrentJobsByOrgRole returns queued/running counts for every attributed
// role. It reads only the two partial active-state indexes rather than the
// historical jobs table.
func (s *Store) CountCurrentJobsByOrgRole(ctx context.Context) (map[string]map[string]int, error) {
	counts := map[string]map[string]int{}
	for _, query := range []struct {
		state string
		sql   string
	}{
		{state: "running", sql: countCurrentJobsByOrgRoleRunningSQL},
		{state: "queued", sql: countCurrentJobsByOrgRoleQueuedSQL},
	} {
		rows, err := s.db.QueryContext(ctx, query.sql)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var role string
			var count int
			if err := rows.Scan(&role, &count); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if counts[role] == nil {
				counts[role] = map[string]int{}
			}
			counts[role][query.state] = count
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return counts, nil
}

// CountJobsByOrgRoleSince returns one grouped state-count projection for all
// attributed org roles. A zero since includes the full job history.
func (s *Store) CountJobsByOrgRoleSince(ctx context.Context, since time.Time) (map[string]map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT json_extract(payload, '$.acting_org_role') AS role, state, COUNT(*)
		FROM jobs
		WHERE json_extract(payload, '$.acting_org_role') IS NOT NULL
		AND json_extract(payload, '$.acting_org_role') <> ''
		AND created_at >= ?
		GROUP BY role, state`, since.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]map[string]int{}
	for rows.Next() {
		var role, state string
		var count int
		if err := rows.Scan(&role, &state, &count); err != nil {
			return nil, err
		}
		if counts[role] == nil {
			counts[role] = map[string]int{}
		}
		counts[role][state] = count
	}
	return counts, rows.Err()
}

// listRunningJobsUpdatedBeforeSQL is ListRunningJobsUpdatedBefore's exact query,
// exported as a package-level const so the plan test (TestListRunningJobsUpdatedBeforeOrder)
// EXPLAINs the PRODUCTION text (binding the threshold as its `?` parameter) instead of
// a hand-copied duplicate.
// The `externally_driven = 0` predicate exempts session jobs (#657) from the
// stuck-running reaper: a "here"/prompt-import job is created directly running and
// held open by the calling session for as long as the work takes, so the daemon
// must never time it out and requeue it (which would try to Deliver work the
// engine never owned). Engine-driven jobs default externally_driven = 0, so their
// reaping is byte-identical.
const listRunningJobsUpdatedBeforeSQL = `SELECT id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens
		FROM jobs WHERE state = 'running' AND externally_driven = 0 AND updated_at < ? ORDER BY updated_at`

// ListRunningJobsUpdatedBefore returns the running jobs whose updated_at predates
// the crash-backstop threshold. It orders by updated_at (not id) so the partial
// index idx_jobs_running_updated_at (WHERE state='running') satisfies both the
// filter and the ordering — an `ORDER BY id` defeated that index and forced a full
// primary-key scan of the whole jobs table each worker tick (#619). The state
// predicate is the SQL literal 'running' (not a bound parameter) for the same
// reason ListQueuedJobs inlines 'queued': SQLite only applies a partial index when
// it can prove the query's WHERE implies the index's predicate, which a bound
// `state = ?` prevents (EXPLAIN QUERY PLAN falls back to a scan + temp b-tree).
// The sole caller (recoverRunningJobsBeforeForRepoSkipping) processes each row
// independently and does not depend on the ordering; updated_at is fixed-width
// 'YYYY-MM-DD HH:MM:SS' so lexical order equals chronological order.
func (s *Store) ListRunningJobsUpdatedBefore(ctx context.Context, before time.Time) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, listRunningJobsUpdatedBeforeSQL, before.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens); err != nil {
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

// ClaimRunningJob is TransitionJobStateWithEvent specialized for the queued->
// running CLAIM, additionally stamping the claiming process's identity
// (runner_pid, runner_boot_id) in the SAME atomic UPDATE (#651). runner_pid is
// recorded for observability only — cross-boot recovery keys on runner_boot_id,
// never on the pid, because the daemon runs jobs in-process so the pid is the
// daemon's own, not the reparented worker's. runnerBootID is db.BootID(): "" off
// Linux, in which case the row is identity-less and only the existing age/lease
// recovery applies. It returns false (no event written) when the row was not in
// `from` state, exactly like TransitionJobStateWithEvent.
func (s *Store) ClaimRunningJob(ctx context.Context, id string, from string, to string, event JobEvent, runnerPID int, runnerBootID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, runner_pid = ?, runner_boot_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`, to, runnerPID, strings.TrimSpace(runnerBootID), id, from)
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

// RequeueRunningJobsFromForeignBoot requeues (running->queued) every running job
// whose recorded runner_boot_id proves it was claimed on a PREVIOUS boot of this
// host (#651): the host has since rebooted, so its in-process worker is
// definitively dead and the job must be re-dispatched immediately — regardless of
// any unexpired runtime-session lease, which survives a reboot in the DB and would
// otherwise keep the job "held" until it expired (the AC2 gap this closes). Each
// requeue appends a job_event for the audit trail. It returns the ids actually
// requeued so the daemon can log them.
//
// It is a STRICT no-op when currentBootID is "" (a non-Linux host or a boot id
// that could not be read): with no boot identity there is nothing to compare
// against, so behavior is byte-identical to before this feature. The `!= ”`
// guard likewise never touches identity-less legacy rows (pre-upgrade running
// jobs), leaving them to the existing age/lease recovery. It NEVER touches a
// same-boot job — same-boot liveness stays governed by the age/lease gate, so no
// live in-process worker is ever double-run.
func (s *Store) RequeueRunningJobsFromForeignBoot(ctx context.Context, currentBootID string) ([]string, error) {
	currentBootID = strings.TrimSpace(currentBootID)
	if currentBootID == "" {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM jobs
		WHERE state = 'running' AND runner_boot_id != '' AND runner_boot_id != ?
		ORDER BY id`, currentBootID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	requeued := make([]string, 0, len(ids))
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'queued', updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND state = 'running' AND runner_boot_id != '' AND runner_boot_id != ?`, id, currentBootID)
		if err != nil {
			return nil, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
			id, "queued", "recovered running job claimed on a previous boot (host rebooted)"); err != nil {
			return nil, err
		}
		requeued = append(requeued, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return requeued, nil
}

func (s *Store) TransitionJobStatePayloadWithEvent(ctx context.Context, id string, from string, to string, payload string, event JobEvent, extraEvents ...JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ? AND workflow_id = ?`,
		to, payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
		projection.BlockerSuggestedAction, id, from, projection.WorkflowID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		if err := rejectWorkflowIDMismatch(ctx, tx, id, projection.WorkflowID); err != nil {
			return false, err
		}
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

// TransitionJobStatePayloadWithEventAndTaskTransition is the retry path's
// cross-row transaction: a dismissed task is explicitly recovered before its
// job is re-queued, and either both lifecycle events commit or neither does.
func (s *Store) TransitionJobStatePayloadWithEventAndTaskTransition(ctx context.Context, id string, from string, to string, payload string, event JobEvent, taskID string, taskFrom string, taskTo string, taskKind string, taskReason string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	taskResult, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`,
		taskTo, taskID, taskFrom)
	if err != nil {
		return false, err
	}
	taskAffected, err := taskResult.RowsAffected()
	if err != nil {
		return false, err
	}
	if taskAffected != 1 {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&current); err != nil {
			return false, err
		}
		return false, fmt.Errorf("task %s is %s; retry requires explicit recovery from %s", taskID, current, taskFrom)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
		VALUES (?, ?, ?, ?, ?)`, taskID, taskKind, taskFrom, taskTo, taskReason); err != nil {
		return false, err
	}

	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ? AND workflow_id = ?`,
		to, payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
		projection.BlockerSuggestedAction, id, from, projection.WorkflowID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		if err := rejectWorkflowIDMismatch(ctx, tx, id, projection.WorkflowID); err != nil {
			return false, err
		}
		return false, nil
	}
	if event.JobID == "" {
		event.JobID = id
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message); err != nil {
		return false, err
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
	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET agent = ?, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND agent = ? AND state = ? AND workflow_id = ?`,
		strings.TrimSpace(toAgent), payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest,
		projection.BlockerRetryAt, projection.BlockerSuggestedAction, id, strings.TrimSpace(fromAgent), "queued", projection.WorkflowID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		if err := rejectWorkflowIDMismatch(ctx, tx, id, projection.WorkflowID); err != nil {
			return false, err
		}
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND workflow_id = ?`,
		payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
		projection.BlockerSuggestedAction, id, projection.WorkflowID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		if err := rejectWorkflowIDMismatch(ctx, tx, id, projection.WorkflowID); err != nil {
			return err
		}
		return requireAffected(result, "job", id)
	}
	return tx.Commit()
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func rejectWorkflowIDMismatch(ctx context.Context, q queryRower, id, incoming string) error {
	var stored string
	err := q.QueryRowContext(ctx, `SELECT workflow_id FROM jobs WHERE id = ?`, id).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if stored != incoming {
		return fmt.Errorf("job %q workflow id is immutable: stored %q, incoming %q", id, stored, incoming)
	}
	return nil
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

// RecordRuntimeSessionUsageDelta converts a runtime session's CUMULATIVE token
// counters into this delivery's per-job usage. codex reports turn.completed usage
// as the session's running total on a resumed thread (#661), so a job's real cost
// is the increase since the last delivery on that session. It reads the prior
// baseline for sessionKey (0 if none), returns deltaInput/deltaOutput =
// max(0, cumulative_now - prev), and upserts the new cumulative as the baseline —
// all inside ONE transaction so two daemon workers racing on the same session
// cannot interleave the read and the write (the store also caps the pool at a
// single connection). A counter that went backwards (codex session
// compaction/rollover resets it) yields a 0 delta for the crossing job AND resyncs
// the baseline to the new lower cumulative: a safe under-count rather than a
// spurious huge delta. Negative inputs are clamped to 0 so a malformed usage
// report can never corrupt the baseline.
func (s *Store) RecordRuntimeSessionUsageDelta(ctx context.Context, sessionKey string, cumInput, cumOutput int) (deltaInput int, deltaOutput int, err error) {
	if cumInput < 0 {
		cumInput = 0
	}
	if cumOutput < 0 {
		cumOutput = 0
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	var prevInput, prevOutput int
	err = tx.QueryRowContext(ctx, `SELECT input_cum, output_cum FROM runtime_session_usage WHERE session_key = ?`, sessionKey).Scan(&prevInput, &prevOutput)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, err
	}

	if deltaInput = cumInput - prevInput; deltaInput < 0 {
		deltaInput = 0
	}
	if deltaOutput = cumOutput - prevOutput; deltaOutput < 0 {
		deltaOutput = 0
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_session_usage(session_key, input_cum, output_cum, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(session_key) DO UPDATE SET
			input_cum = excluded.input_cum,
			output_cum = excluded.output_cum,
			updated_at = excluded.updated_at`,
		sessionKey, cumInput, cumOutput); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return deltaInput, deltaOutput, nil
}

func (s *Store) AddJobEvent(ctx context.Context, event JobEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message)
	return err
}

// AddJobEventIfAbsent atomically inserts one event for a job/kind pair. The
// existence check and insert share one SQLite statement, so concurrent callers
// cannot both pass a check-then-insert window.
func (s *Store) AddJobEventIfAbsent(ctx context.Context, event JobEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message)
		SELECT ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM job_events WHERE job_id = ? AND kind = ?
		)`, event.JobID, event.Kind, event.Message, event.JobID, event.Kind)
	return err
}

// ClaimJobEvent atomically inserts an exact job/kind/message event and reports
// whether this caller won. The NOT EXISTS guard and insert share one SQLite
// statement, so concurrent pipeline scans cannot both claim the same external
// write identified by the event content.
func (s *Store) ClaimJobEvent(ctx context.Context, event JobEvent) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message)
		SELECT ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM job_events WHERE job_id = ? AND kind = ? AND message = ?
		)`, event.JobID, event.Kind, event.Message, event.JobID, event.Kind, event.Message)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// UpsertLatestJobEvent keeps one mutable latest-only row for a job/event kind.
// The write is guarded by jobs.state='running' in the same transaction, so a
// delayed best-effort progress tick that races terminalization becomes a no-op.
// This intentionally touches only job_events: the stuck-running reaper keys on
// the jobs row (including runner_boot_id), so progress cannot refresh or mask
// job liveness and cannot perturb pipeline_run_stages advancement invariants.
func (s *Store) UpsertLatestJobEvent(ctx context.Context, event JobEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE job_events
		SET message = ?, created_at = CURRENT_TIMESTAMP
		WHERE job_id = ? AND kind = ?
		  AND EXISTS (SELECT 1 FROM jobs WHERE id = ? AND state = 'running')`,
		event.Message, event.JobID, event.Kind, event.JobID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message)
			SELECT ?, ?, ? WHERE EXISTS (
				SELECT 1 FROM jobs WHERE id = ? AND state = 'running'
			)`, event.JobID, event.Kind, event.Message, event.JobID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetLatestJobEventByKind returns the newest event of kind for jobID. The
// idx_job_events_kind_job_id covering index serves the lookup.
func (s *Store) GetLatestJobEventByKind(ctx context.Context, jobID, kind string) (JobEvent, bool, error) {
	var event JobEvent
	err := s.db.QueryRowContext(ctx, `SELECT job_id, kind, message, created_at
		FROM job_events WHERE kind = ? AND job_id = ? ORDER BY id DESC LIMIT 1`,
		kind, jobID).Scan(&event.JobID, &event.Kind, &event.Message, &event.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return JobEvent{}, false, nil
	}
	return event, err == nil, err
}

// GetEarliestJobEventByKind returns the oldest event of kind for jobID. The
// idx_job_events_kind_job_id covering index serves the lookup.
func (s *Store) GetEarliestJobEventByKind(ctx context.Context, jobID, kind string) (JobEvent, bool, error) {
	var event JobEvent
	err := s.db.QueryRowContext(ctx, `SELECT job_id, kind, message, created_at
		FROM job_events WHERE kind = ? AND job_id = ? ORDER BY id ASC LIMIT 1`,
		kind, jobID).Scan(&event.JobID, &event.Kind, &event.Message, &event.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return JobEvent{}, false, nil
	}
	return event, err == nil, err
}

// LatestAdvancementMarker returns the most recent post-delivery advancement
// marker kind for jobID (or "" when the job has none). The kind set is exactly
// the one jobNeedsAdvanceRetry (daemon.go) evaluates last-one-wins, so this is a
// cheap way for the emitter to stay idempotent: a job already sitting on
// advance_retry must not append another advance_retry on every ~1s tick — that
// unbounded growth is what pinned a core once job_events reached hundreds of
// thousands of rows (JobIDsWithPendingAdvanceRetry's per-tick GROUP BY and
// jobNeedsAdvanceRetry's per-job ListJobEvents both scale with those rows). The
// job stays a retry candidate regardless (last-one-wins is unchanged); only the
// duplicate row is elided.
// RefreshLatestAdvanceRetry rewrites the surviving advance_retry row's message
// and timestamp so the #552 why-stuck surface keeps showing the CURRENT failure
// while recordAdvanceRetryOnce keeps the row count bounded at one per stuck job.
// The write is skipped when the message is unchanged, so a stable failure costs
// zero writes per tick. Reported updated=false covers both no-row and no-change.
func (s *Store) RefreshLatestAdvanceRetry(ctx context.Context, jobID string, message string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE job_events
		SET message = ?, created_at = CURRENT_TIMESTAMP
		WHERE id = (SELECT id FROM job_events WHERE job_id = ? AND kind = 'advance_retry' ORDER BY id DESC LIMIT 1)
		  AND message != ?`, message, jobID, message)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Store) LatestAdvancementMarker(ctx context.Context, jobID string) (string, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM job_events
		WHERE job_id = ? AND kind IN (
			'advance_started', 'advance_retry', 'advance_completed',
			'advance_retried', 'advance_blocked', 'advance_retry_skipped', 'retry_queued')
		ORDER BY id DESC LIMIT 1`, jobID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return kind, err
}

// GetLatestJobEventsByKind returns the newest event of kind for each requested
// job in one indexed query. Empty input returns an initialized empty map.
func (s *Store) GetLatestJobEventsByKind(ctx context.Context, jobIDs []string, kind string) (map[string]JobEvent, error) {
	out := map[string]JobEvent{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(jobIDs)), ",")
	args := make([]any, 0, 2*(len(jobIDs)+1))
	args = append(args, kind)
	for _, jobID := range jobIDs {
		args = append(args, jobID)
	}
	args = append(args, kind)
	for _, jobID := range jobIDs {
		args = append(args, jobID)
	}
	query := `SELECT job_id, kind, message, created_at FROM job_events
		WHERE kind = ? AND job_id IN (` + placeholders + `)
		  AND id IN (SELECT MAX(id) FROM job_events
			WHERE kind = ? AND job_id IN (` + placeholders + `) GROUP BY job_id)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var event JobEvent
		if err := rows.Scan(&event.JobID, &event.Kind, &event.Message, &event.CreatedAt); err != nil {
			return nil, err
		}
		out[event.JobID] = event
	}
	return out, rows.Err()
}

// GetEarliestJobEventsByKind returns the oldest event of kind for each requested
// job in one indexed query. Empty input returns an initialized empty map.
func (s *Store) GetEarliestJobEventsByKind(ctx context.Context, jobIDs []string, kind string) (map[string]JobEvent, error) {
	out := map[string]JobEvent{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(jobIDs)), ",")
	args := make([]any, 0, 2*(len(jobIDs)+1))
	args = append(args, kind)
	for _, jobID := range jobIDs {
		args = append(args, jobID)
	}
	args = append(args, kind)
	for _, jobID := range jobIDs {
		args = append(args, jobID)
	}
	query := `SELECT job_id, kind, message, created_at FROM job_events
		WHERE kind = ? AND job_id IN (` + placeholders + `)
		  AND id IN (SELECT MIN(id) FROM job_events
			WHERE kind = ? AND job_id IN (` + placeholders + `) GROUP BY job_id)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var event JobEvent
		if err := rows.Scan(&event.JobID, &event.Kind, &event.Message, &event.CreatedAt); err != nil {
			return nil, err
		}
		out[event.JobID] = event
	}
	return out, rows.Err()
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

// jobIDsByQuery runs a query whose rows are each a single job_id column and
// collects them into a slice. The bounded-candidate passes (#598) and the
// delegation-worktree reclaim pass all share this identical rows-scan boilerplate;
// each supplies its own SELECT and defers the row handling here.
func (s *Store) jobIDsByQuery(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
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

// The three per-tick candidate GROUP BY queries are exported as package-level consts
// so the covering-index plan test (TestJobEventsKindJobIDCoveringIndexPlan) EXPLAINs
// the PRODUCTION query text — not a hand-copied duplicate — for each. A change to any
// of these queries is then exactly what the covering-index test asserts a plan for.
const (
	jobIDsWithPendingDelegationWorktreeReclaimSQL = `SELECT job_id FROM job_events
		WHERE kind = 'delegation_worktree_cleanup_skipped'
		  AND id IN (
			SELECT MAX(id) FROM job_events
			WHERE kind IN ('delegation_worktree_cleanup_skipped', 'delegation_worktree_removed')
			GROUP BY job_id
		)
		ORDER BY job_id`

	jobIDsWithPendingAdvanceRetrySQL = `SELECT job_id FROM job_events
		WHERE kind IN ('advance_started', 'advance_retry')
		  AND id IN (
			SELECT MAX(id) FROM job_events
			WHERE kind IN ('advance_started', 'advance_retry', 'advance_completed',
			               'advance_retried', 'advance_blocked', 'advance_retry_skipped', 'retry_queued')
			GROUP BY job_id
		)
		ORDER BY job_id`

	jobIDsWithPendingCommentRetrySQL = `SELECT job_id FROM job_events
		WHERE kind = 'comment_post_failed'
		  AND id IN (
			SELECT MAX(id) FROM job_events
			WHERE kind IN ('comment_post_failed', 'comment_posted', 'retry_queued')
			GROUP BY job_id
		)
		ORDER BY job_id`
)

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
	return s.jobIDsByQuery(ctx, jobIDsWithPendingDelegationWorktreeReclaimSQL)
}

// JobIDsWithAgedTerminalDelegationWorktree returns final jobs whose recorded
// delegation/read-only worktree has outlived cutoff and has no successful
// cleanup event. Unlike the pending-marker query above, this deliberately finds
// crash-window leftovers that never got far enough to emit a cleanup-skipped
// marker. Blocked is excluded: it is resumable and therefore still owns its
// worktree. The payload predicates exclude ordinary task worktrees.
func (s *Store) JobIDsWithAgedTerminalDelegationWorktree(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.id
		FROM jobs j
		WHERE j.state IN ('succeeded', 'failed', 'cancelled')
		  AND unixepoch(COALESCE(NULLIF(j.updated_at, ''), j.created_at)) <= unixepoch(?)
		  AND COALESCE(json_extract(j.payload, '$.worktree_path'), '') <> ''
		  AND (COALESCE(json_extract(j.payload, '$.delegation_id'), '') <> ''
		       OR COALESCE(json_extract(j.payload, '$.read_only_worktree'), 0) = 1)
		  AND NOT EXISTS (
			SELECT 1 FROM jobs owner
			WHERE owner.id <> j.id
			  AND json_extract(owner.payload, '$.worktree_path') = json_extract(j.payload, '$.worktree_path')
			  AND (owner.state NOT IN ('succeeded', 'failed', 'cancelled')
			       OR unixepoch(COALESCE(NULLIF(owner.updated_at, ''), owner.created_at)) IS NULL
			       OR unixepoch(COALESCE(NULLIF(owner.updated_at, ''), owner.created_at)) > unixepoch(?))
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM job_events e
			WHERE e.job_id = j.id
			  AND e.kind IN ('delegation_worktree_removed', 'delegation_worktree_reclaimed_ttl')
		  )
		ORDER BY j.id`, cutoff.UTC().Format("2006-01-02 15:04:05"), cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// JobIDsWithPendingAdvanceRetry returns the IDs of jobs whose LATEST post-delivery
// advancement event (by id) is still an unreconciled attempt marker
// (advance_started/advance_retry) — exactly jobWorker.jobNeedsAdvanceRetry's
// last-one-wins rule (daemon.go), evaluated set-at-once so the retry pass iterates
// only genuine candidates instead of a full ListJobs + a per-terminal-job
// ListJobEvents on every 1s worker tick — which was O(jobs × events) and burned a
// core once a few hundred terminal jobs had accumulated (#598). The pass
// re-verifies each candidate with the Go predicate, so this only has to be a
// superset; it is in fact exact.
//
// The order of the tracked kinds matters (a job can be started, then completed,
// then retried), so the LAST one wins: a job is a candidate iff the highest-id
// event among exactly the predicate's tracked kinds is a positive attempt marker.
// The MAX(id) subquery is restricted to those tracked kinds only (mirroring the Go
// switch's default: no-op for every other kind) and the outer filter keeps a job
// iff that latest tracked event is positive. One row per job (its unique MAX id).
func (s *Store) JobIDsWithPendingAdvanceRetry(ctx context.Context) ([]string, error) {
	return s.jobIDsByQuery(ctx, jobIDsWithPendingAdvanceRetrySQL)
}

// JobIDsWithPendingCommentRetry returns the IDs of jobs whose LATEST comment event
// (by id) is comment_post_failed with no subsequent comment_posted or retry_queued
// — exactly jobWorker.jobNeedsCommentRetry's last-one-wins rule (daemon.go),
// evaluated set-at-once so the comment-retry pass iterates only genuine candidates
// instead of a full ListJobs + per-terminal-job ListJobEvents on every 1s worker
// tick (#598). The pass re-verifies each candidate with the Go predicate, so this
// only has to be a superset; it is in fact exact.
func (s *Store) JobIDsWithPendingCommentRetry(ctx context.Context) ([]string, error) {
	return s.jobIDsByQuery(ctx, jobIDsWithPendingCommentRetrySQL)
}

// JobIDsWithOpenEscalation returns the IDs of coordinator jobs with an OPEN
// escalation round — strictly more delegation_escalation_requested than
// delegation_escalation_resolved events — the same requested>resolved rule
// Engine.escalationOpen applies per job (engine.go), evaluated set-at-once over
// idx_job_events_kind. It lets AutoFinalizeExpiredEscalations iterate only the
// (small) set of trees currently paused awaiting a human instead of listing EVERY
// job and re-reading each one's full event history TWICE on every repo poll — which
// sustained ~37MB/s and ~1108 queries per poll x18 repos (#598/#340). Zero rows =>
// the caller returns with no further per-job queries.
//
// Both ask-gate (#445) and failure escalations ride these same two kinds, and the
// per-job candidate gate is purely count-based on them (never branching on the
// record's Kind), so this count reproduces the exact candidate set the original
// loadEscalation(exists) + !escalationResolved(open) gates passed. The literal
// strings MUST equal the escalationRequestedEvent/escalationResolvedEvent constants;
// a store test pins this.
func (s *Store) JobIDsWithOpenEscalation(ctx context.Context) ([]string, error) {
	return s.jobIDsByQuery(ctx, `SELECT job_id FROM job_events
		WHERE kind IN ('delegation_escalation_requested', 'delegation_escalation_resolved')
		GROUP BY job_id
		HAVING SUM(CASE WHEN kind = 'delegation_escalation_requested' THEN 1 ELSE 0 END)
		     > SUM(CASE WHEN kind = 'delegation_escalation_resolved' THEN 1 ELSE 0 END)
		ORDER BY job_id`)
}

// RecordJobGates persists one resumable gate per non-blank need for a blocked job
// (#682). It is an UPSERT keyed on (job_id, need): a fresh need inserts an OPEN
// gate; a re-blocked job that repeats a need REOPENS the existing row (resets
// satisfied to 0 and clears satisfied_at) so a stage that blocks → clears →
// re-runs → blocks again does not strand a permanently-satisfied gate. It returns
// the number of needs written. An empty (or all-blank) list is a no-op that
// writes nothing, so a blocked job with no `needs` is byte-identical to before
// this feature (no rows, no behavior change).
func (s *Store) RecordJobGates(ctx context.Context, jobID string, needs []string) (int, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return 0, fmt.Errorf("job id is required")
	}
	written := 0
	for _, need := range needs {
		need = strings.TrimSpace(need)
		if need == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO job_gates(job_id, need, satisfied, created_at, satisfied_at)
			VALUES (?, ?, 0, CURRENT_TIMESTAMP, '')
			ON CONFLICT(job_id, need) DO UPDATE SET satisfied = 0, satisfied_at = ''`,
			jobID, need); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// ListJobGates returns every gate recorded for a job, oldest-first by insertion
// id. Zero rows (the common case) yields an empty slice.
func (s *Store) ListJobGates(ctx context.Context, jobID string) ([]JobGate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, need, satisfied, created_at, satisfied_at
		FROM job_gates WHERE job_id = ? ORDER BY id`, strings.TrimSpace(jobID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gates []JobGate
	for rows.Next() {
		var g JobGate
		var satisfied int
		if err := rows.Scan(&g.ID, &g.JobID, &g.Need, &satisfied, &g.CreatedAt, &g.SatisfiedAt); err != nil {
			return nil, err
		}
		g.Satisfied = satisfied != 0
		gates = append(gates, g)
	}
	return gates, rows.Err()
}

// ListOpenJobGates returns every still-open (unsatisfied) job gate across all
// jobs, oldest-first by insertion id, so the dashboard "Needs a human" view
// (#528) can list every job parked on a human decision in one query. It is a
// read-only projection; a home with no gates yields an empty slice.
func (s *Store) ListOpenJobGates(ctx context.Context) ([]JobGate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, need, satisfied, created_at, satisfied_at
		FROM job_gates WHERE satisfied = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gates []JobGate
	for rows.Next() {
		var g JobGate
		var satisfied int
		if err := rows.Scan(&g.ID, &g.JobID, &g.Need, &satisfied, &g.CreatedAt, &g.SatisfiedAt); err != nil {
			return nil, err
		}
		g.Satisfied = satisfied != 0
		gates = append(gates, g)
	}
	return gates, rows.Err()
}

// SatisfyJobGate marks a single open gate satisfied by exact need text. It returns
// whether an OPEN gate with that need existed and was cleared (false when no such
// need is recorded or it was already satisfied), so the caller can report an
// unknown --need rather than silently succeeding.
func (s *Store) SatisfyJobGate(ctx context.Context, jobID, need string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE job_gates SET satisfied = 1, satisfied_at = CURRENT_TIMESTAMP
		WHERE job_id = ? AND need = ? AND satisfied = 0`,
		strings.TrimSpace(jobID), strings.TrimSpace(need))
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// SatisfyAllJobGates marks every still-open gate for a job satisfied and returns
// how many it cleared.
func (s *Store) SatisfyAllJobGates(ctx context.Context, jobID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE job_gates SET satisfied = 1, satisfied_at = CURRENT_TIMESTAMP
		WHERE job_id = ? AND satisfied = 0`, strings.TrimSpace(jobID))
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// CountJobGates returns (total, open) gate counts for a job in one query. open is
// the number still unsatisfied; total==0 means the job never recorded any gate.
func (s *Store) CountJobGates(ctx context.Context, jobID string) (total int, open int, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(CASE WHEN satisfied = 0 THEN 1 ELSE 0 END)
		FROM job_gates WHERE job_id = ?`, strings.TrimSpace(jobID))
	var openNull sql.NullInt64
	if err := row.Scan(&total, &openNull); err != nil {
		return 0, 0, err
	}
	return total, int(openNull.Int64), nil
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
