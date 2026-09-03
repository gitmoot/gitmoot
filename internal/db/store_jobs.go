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

func (s *Store) CreateJobWithEvent(ctx context.Context, job Job, event JobEvent, additionalEvents ...JobEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := createJobWithEventTx(ctx, tx, s, job, event, additionalEvents...); err != nil {
		return err
	}
	return tx.Commit()
}

// createJobWithEventTx is the shared insert body, so the ownership-bound variant
// below cannot drift from the plain one.
//
// See CreateJob: the same COALESCE(NULLIF(?,”), ?) bound to (payload.RootJobID,
// job.ID) denormalizes the rootJobID() rule onto the indexed root_id column.
func createJobWithEventTx(ctx context.Context, tx *sql.Tx, s *Store, job Job, event JobEvent, additionalEvents ...JobEvent) error {
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
	for _, additional := range additionalEvents {
		additional.JobID = job.ID
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, additional.JobID, additional.Kind, additional.Message); err != nil {
			return err
		}
	}
	return resolveAwaitedReviewFactTx(ctx, tx, job.ID, job.Agent, job.Type, job.State, job.Payload, s.blockingSeverityFor, time.Now().UTC())
}

// ErrAdvanceOwnershipLost is returned when an irreversible write is refused because
// the supersession advance that requested it no longer owns the job.
var ErrAdvanceOwnershipLost = errors.New("supersede advance no longer owns this job")

// CreateJobWithEventIfAdvanceOwned is CreateJobWithEvent with live advance ownership
// asserted INSIDE the insert's own transaction (#1673).
//
// Enqueueing a job is irreversible, so the binding cannot be a preceding read or a
// pre-write heartbeat: the gap between such a check and the insert is exactly where
// a lease loss or a lifecycle rollover lands. Checking in the transaction that
// carries the insert means either both the ownership predicate held and the job
// exists, or neither.
func (s *Store) CreateJobWithEventIfAdvanceOwned(ctx context.Context, job Job, own AdvanceOwnership, now time.Time, event JobEvent, additionalEvents ...JobEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	live, err := advanceOwnershipLiveTx(ctx, tx, own, now)
	if err != nil {
		return err
	}
	if !live {
		return fmt.Errorf("%w: job %s at generation %d", ErrAdvanceOwnershipLost, own.OwnerJobID, own.AtGeneration)
	}
	if err := createJobWithEventTx(ctx, tx, s, job, event, additionalEvents...); err != nil {
		return err
	}
	return tx.Commit()
}

// AddJobEventIfAdvanceOwned appends a job event only while the advance that asked
// for it still owns the job, asserted in the append's own transaction (#1673). It is
// the taskless counterpart of the round-open write: a coordinator with no task still
// owes a durable round record, and a superseded pass still must not write one.
func (s *Store) AddJobEventIfAdvanceOwned(ctx context.Context, event JobEvent, own AdvanceOwnership, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	live, err := advanceOwnershipLiveTx(ctx, tx, own, now)
	if err != nil {
		return false, err
	}
	if !live {
		return false, fmt.Errorf("%w: event %q for job %s at generation %d",
			ErrAdvanceOwnershipLost, event.Kind, own.OwnerJobID, own.AtGeneration)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
		event.JobID, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// CreateExternallyDrivenJobWithEvent creates a session job (#657) whose execution
// happens OUTSIDE the engine: it sets externally_driven = 1 so the daemon's
// queued selector never claims it and the engine lease reaper skips it. It
// mirrors CreateJobWithEvent (same root_id denormalization, same in-tx event) but
// stamps the extra column; the caller supplies job.State = 'running' so the job
// is created directly running, never queued. This is the ONLY insert site that
// sets externally_driven — every other job insert leaves it at its DEFAULT 0, so
// the normal dispatch path is byte-identical.
func (s *Store) CreateExternallyDrivenJobWithEvent(ctx context.Context, job Job, event JobEvent, extraEvents ...JobEvent) error {
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
	for _, extra := range extraEvents {
		if extra.JobID == "" {
			extra.JobID = job.ID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, extra.JobID, extra.Kind, extra.Message); err != nil {
			return err
		}
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
	row := s.db.QueryRowContext(ctx, `SELECT id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, workflow_id, root_killed, input_tokens, output_tokens, updated_at, created_at, externally_driven, lifecycle_generation FROM jobs WHERE id = ?`, id)
	var job Job
	if err := row.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.WorkflowID, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt, &job.CreatedAt, &job.ExternallyDriven, &job.LifecycleGeneration); err != nil {
		return Job{}, err
	}
	return job, nil
}

// ResolvePullRequestOwner returns the strongest stored org owner for one pull
// request task. Implement-job acting-role attribution wins, followed by the
// branch lock, then the earliest implementing agent. An unresolved owner is
// ("", nil).
func (s *Store) ResolvePullRequestOwner(
	ctx context.Context,
	repo string,
	branch string,
	pullRequest int,
	taskID string,
) (string, error) {
	repo = strings.TrimSpace(repo)
	branch = strings.TrimSpace(branch)
	taskID = strings.TrimSpace(taskID)
	if repo == "" || pullRequest <= 0 {
		return "", nil
	}

	var owner string
	err := s.db.QueryRowContext(ctx, `
SELECT trim(json_extract(payload, '$.acting_org_role'))
FROM jobs
WHERE repo = ? AND pull_request = ?
	AND type = 'implement'
	AND json_valid(payload)
	AND json_type(payload, '$.acting_org_role') = 'text'
	AND trim(json_extract(payload, '$.acting_org_role')) != ''
	AND (? = '' OR COALESCE(json_extract(payload, '$.task_id'), '') = ?)
ORDER BY created_at, id
LIMIT 1`, repo, pullRequest, taskID, taskID).Scan(&owner)
	if err == nil {
		return strings.TrimSpace(owner), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	if branch != "" {
		lock, err := s.GetBranchLock(ctx, repo, branch)
		switch {
		case err == nil && strings.TrimSpace(lock.ActingOrgRole) != "":
			return strings.TrimSpace(lock.ActingOrgRole), nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	}

	err = s.db.QueryRowContext(ctx, `
SELECT agent
FROM jobs
WHERE repo = ? AND pull_request = ? AND type = 'implement'
	AND (? = '' OR (
		json_valid(payload)
		AND COALESCE(json_extract(payload, '$.task_id'), '') = ?
	))
ORDER BY created_at, id
LIMIT 1`, repo, pullRequest, taskID, taskID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(owner), nil
}

// JobWorktreePath returns the worktree path recorded in one job payload without
// scanning the rest of the job row. The daemon's best-effort reclaim pass uses
// this narrow lookup to distinguish a candidate-local GetJob scan failure from a
// store-wide failure: if this lookup also fails, the pass still escalates.
func (s *Store) JobWorktreePath(ctx context.Context, id string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(payload, '$.worktree_path'), '')
		FROM jobs WHERE id = ? AND json_valid(payload)`, id).Scan(&path)
	return path, err
}

// LatestDelegationWorktreeCleanupOutcome returns the newest cleanup outcome for
// one job. The compound kind/job index makes this a bounded point lookup.
func (s *Store) LatestDelegationWorktreeCleanupOutcome(ctx context.Context, jobID string) (string, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM job_events
		WHERE job_id = ? AND kind IN ('delegation_worktree_cleanup_skipped', 'delegation_worktree_removed')
		ORDER BY id DESC LIMIT 1`, jobID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return kind, err
}

// jobColumns is the shared core projection ListJobs and ListJobsByType both
// read, kept as one const so their SELECT lists and scanJobs order cannot drift.
// Workflow-only scalar projections are selected by ListJobsByWorkflow.
const jobColumns = `id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, workflow_id, root_killed, input_tokens, output_tokens, updated_at, created_at, externally_driven, lifecycle_generation`

const listJobsByStateSQL = `SELECT ` + jobColumns + ` FROM jobs WHERE state = ? ORDER BY updated_at, id`

// scanJobs reads every row of a *sql.Rows produced by a `SELECT `+jobColumns+`
// FROM jobs …` query into Jobs, in jobColumns order, and closes rows. Shared by
// ListJobs and ListJobsByType so their identical scan lives in exactly one place.
func scanJobs(rows *sql.Rows) ([]Job, error) {
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.WorkflowID, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt, &job.CreatedAt, &job.ExternallyDriven, &job.LifecycleGeneration); err != nil {
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

func (s *Store) ListJobsByState(ctx context.Context, state string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, listJobsByStateSQL, state)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

func (s *Store) ListJobsByRepo(ctx context.Context, repo string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE repo = ? ORDER BY id`, repo)
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

// listQueuedJobsForRepoSQL mirrors listQueuedJobsSQL with a repo residual and the
// denormalized repo/pull_request columns projected, so a repo-scoped consumer can
// select its own candidates WITHOUT decoding another repo's payload.
//
// INDEXED BY is not decoration. MEASURED with EXPLAIN QUERY PLAN: left to itself the
// planner picks idx_jobs_repo (repo=?) and then builds a TEMP B-TREE for the ORDER BY,
// which walks every job this repo has ever had and sorts them. The queued set is tiny
// by construction — that is the premise of the whole sweep — so scanning the partial
// index in created_at order with repo as the residual is strictly cheaper and needs no
// sort. The literal 'queued' predicate is what makes the partial index usable at all;
// the hint follows the existing precedent at countCurrentJobsByOrgRoleRunningSQL.
// The repo residual is CASE-INSENSITIVE because forge repository identity is: a row
// projected `Gitmoot/Gitmoot` against a daemon registered as `gitmoot/gitmoot` would
// otherwise be invisible to every poll, forever. It costs nothing here — repo is a
// residual over the queued rows, not the indexed term.
const listQueuedJobsForRepoSQL = `SELECT id, agent, type, state, payload, model, parent_job_id, delegation_id, delegation_depth, delegated_by, root_killed, input_tokens, output_tokens, repo, pull_request, lifecycle_generation
		FROM jobs INDEXED BY idx_jobs_queued_created
		WHERE state = 'queued' AND externally_driven = 0 AND lower(repo) = lower(?) ORDER BY created_at, rowid`

// ListQueuedJobsForRepo returns this repo's queued engine-owned jobs in created_at
// (then rowid) order, with the repo, pull_request and lifecycle_generation
// projections populated.
//
// lifecycle_generation rides along because a sweep that decides a job is
// terminable has formed that verdict about the run it OBSERVED. Settling on the
// state string alone cannot tell "still that run" from "it completed, was
// re-queued, and is queued again", so a slow sweep could cancel a newer
// lifecycle. Every settlement from this list must be anchored on the generation
// returned here.
//
// It exists because ListQueuedJobs is HOME-WIDE and projects neither column, so a
// repo-scoped caller had to decode every payload in the home just to discover whose
// row it was — which meant one undecodable payload in ANY repo could fail EVERY
// watched repo's poll, permanently, since that condition is not transient. Filtering
// in SQL means a foreign row is never read at all.
func (s *Store) ListQueuedJobsForRepo(ctx context.Context, repo string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, listQueuedJobsForRepoSQL, strings.TrimSpace(repo))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID, &job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.Repo, &job.PullRequest, &job.LifecycleGeneration); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
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
		WHERE json_valid(payload)
		AND json_extract(payload, '$.acting_org_role') IS NOT NULL
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

// bumpLifecycleGenerationSQL is a SET-clause fragment that advances a job's
// lifecycle generation whenever the write moves it INTO queued from some other
// state (#1407). It takes ONE bound argument: the destination state, which every
// caller already has to hand.
//
// It is a shared fragment rather than seven hand-written CASE expressions for the
// same reason the counter lives in SQL rather than in the callers: a rule copied
// N times is N chances to copy it wrong, and the copies drift independently. Every
// statement below that assigns jobs.state embeds this fragment, with no per-site
// reasoning about whether that particular statement "can" reach queued -- deciding
// per site is exactly how a policy ends up documented at five call sites and
// violated at three of them.
//
// `state` inside the CASE reads the row's PRE-UPDATE value: SQLite evaluates
// SET-clause expressions against the original row, so the comparison sees the
// state being left, not the one being written. The `state <> 'queued'` term makes
// the bump idempotent -- a queued -> queued write is not a new run and must not
// invalidate an in-flight advancement's anchor.
const bumpLifecycleGenerationSQL = `lifecycle_generation = lifecycle_generation + (CASE WHEN ? = 'queued' AND state <> 'queued' THEN 1 ELSE 0 END)`

func (s *Store) UpdateJobState(ctx context.Context, id string, state string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, state, state, id)
	if err != nil {
		return err
	}
	return requireAffected(result, "job", id)
}

func (s *Store) TransitionJobState(ctx context.Context, id string, from string, to string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`, to, to, id, from)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// TransitionJobStateAtGeneration is TransitionJobState with the LIFECYCLE
// GENERATION pinned and NO event written.
//
// It exists for a recovery pass that must convert a check-then-act into a
// compare-and-swap without leaving an audit row per attempt. A caller re-asserts
// (state, generation) as a self-transition to claim the exact run it decided
// about; a retry queued since that decision moves the generation, so the claim
// fails. Writing an event here instead would append one row per poll for as long
// as a fault persists, which is the unbounded job_events growth the advance-retry
// markers were reshaped to avoid (#598).
func (s *Store) TransitionJobStateAtGeneration(ctx context.Context, id string, from string, fromGeneration int64, to string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ? AND lifecycle_generation = ?`, to, to, id, from, fromGeneration)
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

	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`, to, to, id, from)
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

// TransitionJobStateWithEventAtGeneration is TransitionJobStateWithEvent whose
// compare-and-swap additionally pins the job's LIFECYCLE GENERATION (#1407).
//
// TransitionJobStateWithEvent anchors on the state string alone, which cannot
// distinguish "still the run I observed" from "a later run that happens to have
// reached the same state again". A caller settling a verdict it formed about a
// SPECIFIC run must use this instead: the generation only ever moves forward, so
// a stale settlement loses the CAS rather than overwriting a live lifecycle.
//
// It returns false, with no event written, when the row is not in `from` state OR
// not at `fromGeneration` -- the caller cannot tell the two apart from the return
// value alone and should re-read the row to classify what happened.
//
// additionalEvents land in the SAME transaction as the state write. A caller that
// owes follow-up work after the transition (cleanup, a synthetic result, a parent
// advance) uses one to record that debt durably, so a crash between the
// transition and the follow-up leaves a marker a later sweep can rediscover
// rather than a silently half-finished settlement.
func (s *Store) TransitionJobStateWithEventAtGeneration(ctx context.Context, id string, from string, fromGeneration int64, to string, event JobEvent, additionalEvents ...JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ? AND lifecycle_generation = ?`, to, to, id, from, fromGeneration)
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
	for _, additional := range additionalEvents {
		if additional.JobID == "" {
			additional.JobID = id
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, additional.JobID, additional.Kind, additional.Message); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// TransitionJobStatePayloadWithEventAtGeneration atomically settles one
// observed lifecycle while replacing its payload and recording its required
// event plus any additional events. As the payload-writing counterpart of
// TransitionJobStateWithEventAtGeneration, a stale run loses the compare-and-swap
// instead of rewriting the result of a newer lifecycle.
func (s *Store) TransitionJobStatePayloadWithEventAtGeneration(ctx context.Context, id string, from string, fromGeneration int64, to string, payload string, event JobEvent, additionalEvents ...JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ? AND lifecycle_generation = ? AND workflow_id = ?`,
		to, to, payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
		projection.BlockerSuggestedAction, id, from, fromGeneration, projection.WorkflowID)
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
	for _, additional := range additionalEvents {
		if additional.JobID == "" {
			additional.JobID = id
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, additional.JobID, additional.Kind, additional.Message); err != nil {
			return false, err
		}
	}
	var agent, jobType string
	if err := tx.QueryRowContext(ctx, `SELECT agent, type FROM jobs WHERE id = ?`, id).Scan(&agent, &jobType); err != nil {
		return false, err
	}
	if err := resolveAwaitedReviewFactTx(ctx, tx, id, agent, jobType, to, payload, s.blockingSeverityFor, time.Now().UTC()); err != nil {
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

	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, runner_pid = ?, runner_boot_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`, to, to, runnerPID, strings.TrimSpace(runnerBootID), id, from)
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

// ListRunningJobIDsFromForeignBoot returns running jobs whose recorded runner
// belongs to a previous host boot. It does not change state: daemon recovery
// owns the terminal policy and records the evidence-bearing transition.
func (s *Store) ListRunningJobIDsFromForeignBoot(ctx context.Context, currentBootID string) ([]string, error) {
	currentBootID = strings.TrimSpace(currentBootID)
	if currentBootID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs
		WHERE state = 'running' AND runner_boot_id != '' AND runner_boot_id != ?
		ORDER BY id`, currentBootID)
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

// ErrSupersedeAdvanceOwned is returned when a retry loses the ownership predicate:
// a supersession recovery owns this job's parent advance right now, and re-queueing
// it would roll the lifecycle over mid-advance.
var ErrSupersedeAdvanceOwned = errors.New("supersession parent-advance owns this job")

// TransitionJobStatePayloadWithEventUnlessAdvanceOwned is
// TransitionJobStatePayloadWithEvent with the advance-ownership exclusion in the
// SAME statement. RetryJob uses it so claim absence and the terminal-to-queued
// transition commit together; a separate pre-check loses to a recovery pass that
// acquires ownership in between.
func (s *Store) TransitionJobStatePayloadWithEventUnlessAdvanceOwned(ctx context.Context, id string, from string, to string, payload string, now time.Time, event JobEvent, extraEvents ...JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND state = ? AND workflow_id = ?`+noLiveSupersedeAdvanceLockSQL,
		to, to, payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
		projection.BlockerSuggestedAction, id, from, projection.WorkflowID, formatResourceLockTime(now))
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
		owned, ownErr := advanceOwnershipHeldAnyTokenTx(ctx, tx, SupersedeAdvanceLockKeyPrefix+id, now)
		if ownErr != nil {
			return false, ownErr
		}
		if owned {
			return false, fmt.Errorf("%w: job %s", ErrSupersedeAdvanceOwned, id)
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

func (s *Store) TransitionJobStatePayloadWithEvent(ctx context.Context, id string, from string, to string, payload string, event JobEvent, extraEvents ...JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ? AND workflow_id = ?`,
		to, to, payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
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
	var agent, jobType string
	if err := tx.QueryRowContext(ctx, `SELECT agent, type FROM jobs WHERE id = ?`, id).Scan(&agent, &jobType); err != nil {
		return false, err
	}
	if err := resolveAwaitedReviewFactTx(ctx, tx, id, agent, jobType, to, payload, s.blockingSeverityFor, time.Now().UTC()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// TransitionJobStatePayloadUsageWithEvent atomically closes externally driven
// work with the model and token evidence supplied by its caller. It mirrors
// TransitionJobStatePayloadWithEvent's state/payload/event behavior while keeping
// the usage write in the same transaction. Negative token values are clamped to
// zero defensively; the CLI rejects them before reaching this layer.
func (s *Store) TransitionJobStatePayloadUsageWithEvent(ctx context.Context, id string, from string, to string, payload string, model string, inputTokens int, outputTokens int, event JobEvent, extraEvents ...JobEvent) (bool, error) {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, payload = ?, model = ?, input_tokens = ?, output_tokens = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ? AND workflow_id = ?`,
		to, to, payload, strings.TrimSpace(model), inputTokens, outputTokens, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest,
		projection.BlockerRetryAt, projection.BlockerSuggestedAction, id, from, projection.WorkflowID)
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
	var agent, jobType string
	if err := tx.QueryRowContext(ctx, `SELECT agent, type FROM jobs WHERE id = ?`, id).Scan(&agent, &jobType); err != nil {
		return false, err
	}
	if err := resolveAwaitedReviewFactTx(ctx, tx, id, agent, jobType, to, payload, s.blockingSeverityFor, time.Now().UTC()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// TransitionJobStatePayloadWithEventAndTaskTransition is the retry path's
// cross-row transaction: a dismissed task is explicitly recovered before its
// job is re-queued, and either both lifecycle events commit or neither does.
// now is required because the job transition carries the advance-ownership
// exclusion in its own statement: this arm re-queues exactly like the plain one,
// so it must refuse for the same reason (#1673) rather than being the hole the
// other arm's guard leaves open.
func (s *Store) TransitionJobStatePayloadWithEventAndTaskTransition(ctx context.Context, id string, from string, to string, payload string, now time.Time, event JobEvent, taskID string, taskFrom string, taskTo string, taskKind string, taskReason string) (bool, error) {
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
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND state = ? AND workflow_id = ?`+noLiveSupersedeAdvanceLockSQL,
		to, to, payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
		projection.BlockerSuggestedAction, id, from, projection.WorkflowID, formatResourceLockTime(now))
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
		owned, ownErr := advanceOwnershipHeldAnyTokenTx(ctx, tx, SupersedeAdvanceLockKeyPrefix+id, now)
		if ownErr != nil {
			return false, ownErr
		}
		if owned {
			return false, fmt.Errorf("%w: job %s", ErrSupersedeAdvanceOwned, id)
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

// UpdateJobPayloadAtGeneration is UpdateJobPayload with the job's LIFECYCLE
// GENERATION folded into the same UPDATE (#1620).
//
// UpdateJobPayload predicates on (id, workflow_id) only. A caller that reads the
// row, decides the generation it observed is still current, and only THEN writes
// therefore has a window between the check and the write: a retry claiming the
// row inside it bumps the generation, and the now-stale writer overwrites the NEW
// run's payload with the old run's. Making the generation part of the WHERE turns
// that read-decide-write into one compare-and-swap, so the check cannot go stale
// before the write it guards.
//
// Losing the CAS returns false with the row untouched, and is NOT an error: it
// means the payload being written describes a superseded run, and writing nothing
// is the correct outcome. Callers must not retry it. As with
// TransitionJobStatePayloadWithEventAtGeneration, false alone cannot say whether
// the state, the generation, or the row's existence is what failed to match; a
// caller that needs to know re-reads the row.
//
// UpdateJobPayload itself is deliberately left alone: its callers write payloads
// that are not anchored to an observed run, and giving them a CAS they never
// consented to would silently start dropping their writes.
func (s *Store) UpdateJobPayloadAtGeneration(ctx context.Context, id string, payload string, atGeneration int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND workflow_id = ? AND lifecycle_generation = ?`,
		payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
		projection.BlockerSuggestedAction, id, projection.WorkflowID, atGeneration)
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
	return true, tx.Commit()
}

func (s *Store) UpdateJobPayloadAndStateWithEvent(ctx context.Context, id string, payload string, state string, event JobEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	projection := jobProjectionFromPayload(payload)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND workflow_id = ?`,
		state, state, payload, jobResultHashFromPayload(payload), projection.Repo, projection.PullRequest, projection.BlockerRetryAt,
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
	if event.JobID == "" {
		event.JobID = id
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message); err != nil {
		return err
	}
	var agent, jobType string
	if err := tx.QueryRowContext(ctx, `SELECT agent, type FROM jobs WHERE id = ?`, id).Scan(&agent, &jobType); err != nil {
		return err
	}
	if err := resolveAwaitedReviewFactTx(ctx, tx, id, agent, jobType, state, payload, s.blockingSeverityFor, time.Now().UTC()); err != nil {
		return err
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

// ReleaseJobEventClaim deletes an exact job/kind/message claim so a later
// caller can win it again. It is for a claim whose external write PROVABLY did
// not happen: the pipeline auto-merge gate claims before calling Merge, and a
// reconciliation hold is returned before any GitHub mutation, which left the
// claim consumed and the merge unrepeatable for the life of the run.
//
// It reports whether a row was removed, so a caller that loses the race with a
// concurrent scan can tell it did not release someone else's claim.
func (s *Store) ReleaseJobEventClaim(ctx context.Context, event JobEvent) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM job_events
		WHERE job_id = ? AND kind = ? AND message = ?`, event.JobID, event.Kind, event.Message)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
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
			'advance_retried', 'advance_blocked', 'advance_retry_skipped', 'retry_queued',
			'review_loop_detected')
		ORDER BY id DESC LIMIT 1`, jobID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return kind, err
}

// GetLatestJobEventsByKind returns the newest event of kind for each requested
// job in one indexed query. Empty input returns an initialized empty map.
func (s *Store) GetLatestJobEventsByKind(ctx context.Context, jobIDs []string, kind string) (map[string]JobEvent, error) {
	return s.jobEventsByKindExtreme(ctx, jobIDs, kind, jobEventPickNewest)
}

// GetEarliestJobEventsByKind returns the oldest event of kind for each requested
// job in one indexed query. Empty input returns an initialized empty map.
func (s *Store) GetEarliestJobEventsByKind(ctx context.Context, jobIDs []string, kind string) (map[string]JobEvent, error) {
	return s.jobEventsByKindExtreme(ctx, jobIDs, kind, jobEventPickOldest)
}

// jobEventPick names the aggregate that selects one event per job. It is a
// closed set of package-private constants rather than a string parameter
// precisely because its value is INTERPOLATED into the statement below: a caller
// able to supply that text would be supplying SQL. The two exported methods
// above are the only way in, and each names a constant.
type jobEventPick string

const (
	jobEventPickNewest jobEventPick = "MAX"
	jobEventPickOldest jobEventPick = "MIN"
)

// jobEventsByKindExtreme is the shared body of GetLatestJobEventsByKind and
// GetEarliestJobEventsByKind, which were line-for-line identical apart from
// MAX(id) versus MIN(id) (#1759). Both exported names are kept: they are called
// from the daemon's hot path and from the escalation ladder, and renaming them
// would be a behaviour change dressed as deduplication.
//
// Returns an INITIALIZED EMPTY MAP for empty input, as both twins did - a nil
// map would still read as empty but would panic on assignment for any future
// caller that tried to add to the result.
func (s *Store) jobEventsByKindExtreme(ctx context.Context, jobIDs []string, kind string, pick jobEventPick) (map[string]JobEvent, error) {
	out := map[string]JobEvent{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(jobIDs)), ",")
	args := make([]any, 0, 2*(len(jobIDs)+1))
	for range 2 {
		args = append(args, kind)
		for _, jobID := range jobIDs {
			args = append(args, jobID)
		}
	}
	query := `SELECT job_id, kind, message, created_at FROM job_events
		WHERE kind = ? AND job_id IN (` + placeholders + `)
		  AND id IN (SELECT ` + string(pick) + `(id) FROM job_events
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

// LatestSuccessfulRuntimeSessionUse returns when agentName most recently
// completed a succeeded job on the exact runtimeName/runtimeRef session.
// Runtime selection is dispatch evidence, so the match comes from the newest
// effective_runtime/runtime_override event for each succeeded job. Restricting
// each job to its newest selection excludes failed same-row attempts that were
// followed by a successful retry on another session.
func (s *Store) LatestSuccessfulRuntimeSessionUse(ctx context.Context, agentName, runtimeName, runtimeRef string) (time.Time, bool, error) {
	runtimeName = strings.TrimSpace(runtimeName)
	ref := strings.TrimSpace(runtimeRef)
	if runtimeName == "" || ref == "" {
		return time.Time{}, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.message, j.updated_at
		FROM jobs j
		JOIN job_events e ON e.job_id = j.id
		WHERE j.agent = ?
		  AND j.state = 'succeeded'
		  AND e.kind IN ('effective_runtime', 'runtime_override')
		  AND e.id = (SELECT MAX(latest.id)
			FROM job_events latest
			WHERE latest.job_id = j.id
			  AND latest.kind IN ('effective_runtime', 'runtime_override'))
		ORDER BY j.updated_at DESC, e.id DESC`, strings.TrimSpace(agentName))
	if err != nil {
		return time.Time{}, false, err
	}
	defer rows.Close()

	for rows.Next() {
		var message, updatedAt string
		if err := rows.Scan(&message, &updatedAt); err != nil {
			return time.Time{}, false, err
		}
		eventRuntime, eventRef := runtimeEventSession(message)
		if eventRuntime == runtimeName && eventRef == ref {
			return parseStoredTimestamp(updatedAt), true, nil
		}
	}
	return time.Time{}, false, rows.Err()
}

func runtimeEventSession(message string) (string, string) {
	const marker = "; session lock "
	index := strings.LastIndex(message, marker)
	if index < 0 {
		return "", ""
	}
	lockKey := strings.TrimSpace(message[index+len(marker):])
	const prefix = "runtime:"
	if !strings.HasPrefix(lockKey, prefix) {
		return "", ""
	}
	runtimeAndRef := strings.TrimPrefix(lockKey, prefix)
	separator := strings.IndexByte(runtimeAndRef, ':')
	if separator < 0 {
		return "", ""
	}
	runtimeName := strings.TrimSpace(runtimeAndRef[:separator])
	ref := strings.TrimSpace(runtimeAndRef[separator+1:])
	if runtimeName == "" || ref == "" {
		return "", ""
	}
	return runtimeName, ref
}

// JobIDsWithEventKind returns a map jobID -> message of the LATEST job_event of
// the given kind, one entry per job that has at least one such event. It is a
// single indexed query scoped to one kind so a
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
// of the given kinds, keyed by job id, in a single indexed query. It restricts
// the candidate set to the caller's "reason" kinds
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
func (s *Store) jobIDsByQuery(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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
const delegationWorktreeReclaimCandidateLimit = 256

const (
	jobIDsWithPendingDelegationWorktreeReclaimSQL = `SELECT e.job_id FROM job_events e
		JOIN jobs j ON j.id = e.job_id
		LEFT JOIN cleanup_obligations o
		  ON o.owner_job_id = e.job_id
		 AND o.expected_path = CASE WHEN json_valid(j.payload)
			THEN json_extract(j.payload, '$.worktree_path') ELSE '' END
		WHERE e.kind = 'delegation_worktree_cleanup_skipped'
		  AND e.id IN (
			SELECT MAX(id) FROM job_events
			WHERE kind IN ('delegation_worktree_cleanup_skipped', 'delegation_worktree_removed')
			GROUP BY job_id
		)
		  AND (o.resource_id IS NULL OR (
			o.state IN ('pending', 'retryable')
			AND unixepoch(o.next_attempt_at) <= unixepoch('now')
		  ))
		ORDER BY COALESCE(unixepoch(o.next_attempt_at), 0), e.job_id
		LIMIT ?`

	jobIDsWithPendingAdvanceRetrySQL = `SELECT job_id FROM job_events
		WHERE kind IN ('advance_started', 'advance_retry')
		  AND id IN (
			SELECT MAX(id) FROM job_events
			WHERE kind IN ('advance_started', 'advance_retry', 'advance_completed',
			               'advance_retried', 'advance_blocked', 'advance_retry_skipped', 'retry_queued',
			               'review_loop_detected')
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

	jobIDsWithPendingSupersedeFinalizationSQL = `SELECT job_id FROM job_events
		WHERE kind = 'supersede_finalize_pending'
		  AND id IN (
			SELECT MAX(id) FROM job_events
			WHERE kind IN ('supersede_finalize_pending', 'supersede_finalize_completed')
			GROUP BY job_id
		)
		ORDER BY job_id`

	// supersedeFinalizationClosureSQL appends the completion marker only while the
	// pending marker it claims is STILL the latest of the two marker kinds. The
	// candidate query above is last-one-wins by event id, so an unconditional
	// append written after a newer pending marker silently clears a debt nobody
	// paid. Predicate and insert are one statement, so no retry can land between
	// them.
	//
	// %s is the pending marker's identity test. The marker's MESSAGE IS THE
	// GENERATION, written as a canonical decimal and nothing else, because the
	// classification has to mean the same thing in SQL and in Go. The previous
	// `generation=<n>: <reason>` prefix could not: a malformed prefix
	// (`generation=abc: …`) parsed as unanchored in Go while SQL's
	// `NOT LIKE 'generation=%'` rejected it as anchored-shaped, and a non-canonical
	// spelling (`generation=01: …`) parsed as 1 in Go while the anchored LIKE missed
	// it — both left a debt that no path could ever close.
	//
	// CAST(CAST(message AS INTEGER) AS TEXT) = message is exactly "canonical decimal
	// integer", which is the same predicate strconv.ParseInt plus a FormatInt
	// round-trip applies in Go. So anchored is an equality on that canonical form and
	// unanchored is its complement, and every shape lands in exactly one of them.
	supersedeFinalizationClosureSQL = `INSERT INTO job_events(job_id, kind, message)
		SELECT ?, 'supersede_finalize_completed', ?
		WHERE EXISTS (
			SELECT 1 FROM job_events pending
			WHERE pending.job_id = ?
			  AND pending.kind = 'supersede_finalize_pending'
			  AND %s
			  AND pending.id = (
				SELECT MAX(id) FROM job_events latest
				WHERE latest.job_id = ?
				  AND latest.kind IN ('supersede_finalize_pending', 'supersede_finalize_completed')
			  )
		)`

	supersedeFinalizationAnchoredPendingSQL = `pending.message = CAST(? AS TEXT)
			  AND CAST(CAST(pending.message AS INTEGER) AS TEXT) = pending.message`
	supersedeFinalizationUnanchoredPendingSQL = `CAST(CAST(pending.message AS INTEGER) AS TEXT) <> pending.message`
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
	return s.jobIDsByQuery(ctx, jobIDsWithPendingDelegationWorktreeReclaimSQL, delegationWorktreeReclaimCandidateLimit)
}

// JobIDsWithAgedTerminalDelegationWorktree returns final jobs whose recorded
// delegation/read-only/fix worktree has outlived cutoff and has no successful
// cleanup event. Unlike the pending-marker query above, this deliberately finds
// crash-window leftovers that never got far enough to emit a cleanup-skipped
// marker. Blocked is excluded: it is resumable and therefore still owns its
// worktree. The payload predicates exclude ordinary task worktrees.
//
// The `job_wt` MATERIALIZED CTE extracts each job's worktree_path (and the other
// payload fields) EXACTLY ONCE. The prior form correlated the owner NOT EXISTS
// with json_extract(owner.payload,...) = json_extract(j.payload,...), so SQLite
// re-parsed every owner row's full JSON payload for every outer candidate row —
// O(jobs^2) JSON reparses. On this host's live store (~3500 jobs, some payloads
// >10MB) that made a single run take ~100s, and it runs every ~1s reclaim tick,
// pinning a core in modernc's VDBE/JSON1 (#1111). Materializing the extract once
// turns it into O(jobs) parses + an equality join on the plain column: same rows,
// ~700x faster (verified against a live snapshot). The MATERIALIZED hint is
// required — without it SQLite inlines the CTE and re-parses per reference.
//
// The `WHERE json_valid(payload)` guard is load-bearing: the CTE extracts eagerly
// over every row, and json_extract ERRORS on a non-JSON payload — and ” is the
// column's schema default (payload TEXT NOT NULL DEFAULT ”), a tolerated store
// state (the write path swallows marshal errors). Without the guard a single
// bad-payload row would fail this query on every reclaim tick. Invalid-payload
// rows have no worktree_path, so they can never be a candidate or an owner: the
// guard is a no-op on the result set and matches the sibling json_valid guards
// (store_jobs.go json*Reclaim queries).
func (s *Store) JobIDsWithAgedTerminalDelegationWorktree(ctx context.Context, cutoff time.Time) ([]string, error) {
	cutoffStr := cutoff.UTC().Format("2006-01-02 15:04:05")
	rows, err := s.db.QueryContext(ctx, `
		WITH job_wt AS MATERIALIZED (
			SELECT id, state,
			       unixepoch(COALESCE(NULLIF(updated_at, ''), created_at)) AS ts,
			       json_extract(payload, '$.worktree_path') AS worktree_path,
			       COALESCE(json_extract(payload, '$.delegation_id'), '') AS delegation_id,
			       COALESCE(json_extract(payload, '$.read_only_worktree'), 0) AS read_only_worktree,
			       COALESCE(json_extract(payload, '$.fix_worktree'), 0) AS fix_worktree
			FROM jobs
			WHERE json_valid(payload)
		)
		SELECT j.id
		FROM job_wt j
		LEFT JOIN cleanup_obligations obligation
		  ON obligation.owner_job_id = j.id
		 AND obligation.expected_path = j.worktree_path
		WHERE j.state IN ('succeeded', 'failed', 'cancelled')
		  AND j.ts <= unixepoch(?)
		  AND COALESCE(j.worktree_path, '') <> ''
		  AND (j.delegation_id <> '' OR j.read_only_worktree = 1 OR j.fix_worktree = 1)
		  AND NOT EXISTS (
			SELECT 1 FROM job_wt owner
			WHERE owner.id <> j.id
			  AND owner.worktree_path = j.worktree_path
			  AND (owner.state NOT IN ('succeeded', 'failed', 'cancelled')
			       OR owner.ts IS NULL
			       OR owner.ts > unixepoch(?))
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM job_events e
			WHERE e.job_id = j.id
			  AND e.kind IN ('delegation_worktree_removed', 'delegation_worktree_reclaimed_ttl')
		  )
		  AND (obligation.resource_id IS NULL OR (
			obligation.state IN ('pending', 'retryable')
			AND unixepoch(obligation.next_attempt_at) <= unixepoch('now')
		  ))
		ORDER BY COALESCE(unixepoch(obligation.next_attempt_at), j.ts), j.id
		LIMIT ?`, cutoffStr, cutoffStr, delegationWorktreeReclaimCandidateLimit)
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

// JobIDsWithPendingSupersedeFinalization returns the IDs of jobs whose closed-PR
// supersession recorded its follow-up debt (supersede_finalize_pending) in the
// same transaction as the terminal state write and has not yet recorded that the
// debt was paid (supersede_finalize_completed).
//
// It exists because the terminal write moves the job OUT of queued, and the
// closed-PR sweep selects only queued jobs: a crash or error after the
// transition but before the cleanups, the synthetic result and the parent
// advance left a child that no sweep could ever rediscover, stranding its
// coordinator forever. The marker makes that window recoverable — this is the
// set a later poll re-drives.
//
// Same last-one-wins shape as the other bounded marker queries: the highest-id
// event among exactly the two tracked kinds decides, so a job that was finalized,
// re-queued and superseded again is a candidate again.
func (s *Store) JobIDsWithPendingSupersedeFinalization(ctx context.Context) ([]string, error) {
	return s.jobIDsByQuery(ctx, jobIDsWithPendingSupersedeFinalizationSQL)
}

// CloseSupersedeFinalizationDebtAtGeneration appends the completion marker for
// ONE lifecycle's debt, conditionally and atomically: the pending marker the
// payment claimed must still be the latest of the two marker kinds.
//
// Reading the latest marker and then appending are two statements, and between
// them a retry plus a fresh supersession can append pending generation N+1. The
// old completion then becomes the latest marker and erases N+1 from
// JobIDsWithPendingSupersedeFinalization — a debt that is now invisible and was
// never paid. Evaluating the predicate inside the INSERT removes that window.
//
// closed reports whether the marker was written. False means a newer debt is
// outstanding (or the debt was already closed); the caller leaves it for the next
// poll rather than treating it as paid.
func (s *Store) CloseSupersedeFinalizationDebtAtGeneration(ctx context.Context, jobID string, message string, atGeneration int64, anchored bool) (bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return false, nil
	}
	var (
		statement string
		args      []any
	)
	if anchored {
		statement = fmt.Sprintf(supersedeFinalizationClosureSQL, supersedeFinalizationAnchoredPendingSQL)
		args = []any{jobID, message, jobID, atGeneration, jobID}
	} else {
		statement = fmt.Sprintf(supersedeFinalizationClosureSQL, supersedeFinalizationUnanchoredPendingSQL)
		args = []any{jobID, message, jobID, jobID}
	}
	result, err := s.db.ExecContext(ctx, statement, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// AddJobEventAtGeneration appends an event only while the job is still the settled
// lifecycle the caller claimed: same lifecycle_generation, still in one of the two
// terminal states a supersession writes.
//
// Predicate and insert are ONE statement, so it is the write-side equivalent of a
// compare-and-swap for the event log. Recovery uses it wherever an unconditional
// append would attribute a superseded run's work to a retry that now owns the row:
// the read-only worktree reclaim marker (which would hand a LIVE run's worktree to
// the reclaim pass, since the path is derived from the job id and is therefore the
// same one the retry is using), and the parent-advance bracket.
//
// written reports whether the row landed. False means the lifecycle moved; the
// caller must not treat the step as done.
func (s *Store) AddJobEventAtGeneration(ctx context.Context, event JobEvent, atGeneration int64) (bool, error) {
	jobID := strings.TrimSpace(event.JobID)
	if jobID == "" {
		return false, errors.New("job event requires a job id")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message)
		SELECT ?, ?, ?
		WHERE EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.id = ?
			  AND jobs.lifecycle_generation = ?
			  AND jobs.state IN ('cancelled', 'failed')
		)`, jobID, event.Kind, event.Message, jobID, atGeneration)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
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

// JobIDsWithUnfinishedEscalationResolution returns coordinator jobs whose escalation
// resolution was CLAIMED but whose effects never finished: strictly more
// delegation_escalation_resolved than delegation_escalation_effects_completed events.
//
// The claim is committed before the verb's irreversible effects so that only one
// resolver runs them (#1673). This query is what stops that ordering from turning a
// crash into a permanent stall: a closed round is no candidate for any other sweep,
// so the receipt gap is the only remaining evidence that work is owed.
func (s *Store) JobIDsWithUnfinishedEscalationResolution(ctx context.Context) ([]string, error) {
	return s.jobIDsByQuery(ctx, `SELECT job_id FROM job_events
		WHERE kind IN ('delegation_escalation_resolved', 'delegation_escalation_effects_completed')
		GROUP BY job_id
		HAVING SUM(CASE WHEN kind = 'delegation_escalation_resolved' THEN 1 ELSE 0 END)
		     > SUM(CASE WHEN kind = 'delegation_escalation_effects_completed' THEN 1 ELSE 0 END)
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

// TransitionJobStateWithEvents is TransitionJobStateWithEvent for a caller that must
// land the transition AND a durable OBLIGATION in the same commit (#1673).
//
// The single-event form leaves a window: a caller that transitions a job and then
// writes its re-drive marker separately can crash in between, and a settled job with
// no marker is invisible to every sweep that would have re-driven it. Passing both
// events here closes that window by construction rather than by ordering luck.
func (s *Store) TransitionJobStateWithEvents(ctx context.Context, id string, from string, to string, events ...JobEvent) (bool, error) {
	return s.TransitionJobStateWithPayloadAndEvents(ctx, id, from, to, nil, events...)
}

// TransitionJobStateWithPayloadAndEvents additionally rewrites the job's PAYLOAD in
// the same commit (#1673).
//
// THE CLASS, not the site: a caller that terminalizes a job and then writes the
// payload its recovery path needs has merely MOVED the non-atomicity one step down the
// sequence. The state it leaves - a settled job carrying a re-drive marker but no
// result - is unrepairable: the actuator rejects the nil result, re-stamps the marker
// and repeats forever, and no sweep selects it because the sweeps list QUEUED jobs.
// Two durable facts that must agree are written by one statement here.
//
// A nil payload leaves the stored payload untouched, so the events-only wrapper above
// is byte-equivalent to the original single-event primitive.
func (s *Store) TransitionJobStateWithPayloadAndEvents(ctx context.Context, id string, from string, to string, payload []byte, events ...JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var result sql.Result
	if payload == nil {
		result, err = tx.ExecContext(ctx, `UPDATE jobs SET state = ?, `+bumpLifecycleGenerationSQL+`, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`, to, to, id, from)
	} else {
		// DERIVED FIELDS TRAVEL WITH THE PAYLOAD. jobs.result_hash and the payload
		// projections are not decoration: the hash is the terminal result's
		// proof-integrity receipt and its memory-harvest key, and writing a payload that
		// installs a result while leaving the old (or empty) hash behind breaks that
		// invariant silently. Every other payload writer recomputes them in the same
		// UPDATE; so does this one (#1673).
		projection := jobProjectionFromPayload(string(payload))
		result, err = tx.ExecContext(ctx, `UPDATE jobs SET state = ?, payload = ?, result_hash = ?, repo = ?, pull_request = ?, blocker_retry_at = ?, blocker_suggested_action = ?, `+bumpLifecycleGenerationSQL+`, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = ?`,
			to, payload, jobResultHashFromPayload(string(payload)), projection.Repo, projection.PullRequest,
			projection.BlockerRetryAt, projection.BlockerSuggestedAction, to, id, from)
	}
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
	for _, event := range events {
		if event.JobID == "" {
			event.JobID = id
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// maxJobEventIDSQL is the change cursor for the candidate queries whose result is
// a pure function of job_events. It is the same monotonic maximum the dashboard
// already polls (dashboardChangeCursorSQL) and SQLite answers it with a single
// descent of the integer primary key, so it costs the same whether the table
// holds a hundred rows or a hundred thousand.
const maxJobEventIDSQL = `SELECT COALESCE(MAX(id), 0) FROM job_events`

// MaxJobEventID returns the highest job_events row id, or 0 when the table is
// empty.
//
// It is a sound change cursor for JobIDsWithPendingAdvanceRetry and
// JobIDsWithPendingCommentRetry specifically, because those two queries read only
// the (id, kind, job_id) columns of job_events and NOTHING else — no other table,
// no clock. Production only ever APPENDS to job_events (the sole DELETE is a
// one-time migration that runs before any tick) and its in-place updates —
// RefreshLatestAdvanceRetry, the running-job progress refresh, and the claimed
// permission-policy warning — all SET message/created_at only, never id, kind or
// job_id. So an unmoved maximum id proves those two result sets are unchanged.
//
// It is NOT a sound cursor for the delegation/aged/task reclaim candidates: those
// join jobs and cleanup_obligations and compare against unixepoch('now'), so their
// results change with the clock alone.
func (s *Store) MaxJobEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, maxJobEventIDSQL).Scan(&id)
	return id, err
}
