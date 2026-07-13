package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MemoryHarvestClaimed   = "claimed"
	MemoryHarvestStarted   = "started"
	MemoryHarvestDone      = "done"
	MemoryHarvestSkipped   = "skipped"
	MemoryHarvestUncertain = "uncertain"
)

// MemoryHarvestCandidate is the narrow terminal-job projection consumed by the
// home-scoped insight sweep. It deliberately carries no unrelated job columns.
type MemoryHarvestCandidate struct {
	JobID      string
	Agent      string
	AgentRole  string
	JobType    string
	State      string
	Payload    string
	ResultHash string
}

type MemoryHarvestRun struct {
	JobID          string
	ResultHash     string
	State          string
	ClaimedAt      string
	StartedAt      string
	FinishedAt     string
	UpdatedAt      string
	CandidateCount int
	Detail         string
}

// ListMemoryHarvestCandidatesSQL is exported for the query-plan regression
// test. INDEXED BY makes a future accidental full jobs scan fail loudly.
const ListMemoryHarvestCandidatesSQL = `
SELECT j.id, j.agent, COALESCE(a.role, ''), j.type, j.state, j.payload,
	j.result_hash
FROM jobs j INDEXED BY idx_jobs_memory_harvest_terminal
JOIN memory_harvest_state s ON s.singleton = 1
LEFT JOIN agents a ON a.name = j.agent
LEFT JOIN memory_harvest_runs r
	ON r.job_id = j.id AND r.result_hash = j.result_hash
WHERE j.state IN ('succeeded', 'failed', 'blocked', 'cancelled')
	AND j.result_hash <> ''
	AND (j.rowid > s.high_water_rowid OR j.updated_at > s.high_water_updated_at)
	AND (r.job_id IS NULL OR (r.state = 'claimed' AND r.updated_at <= ?))
ORDER BY j.updated_at, j.id
LIMIT ?`

func memoryHarvestDBTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// InitializeMemoryHarvestState records the current terminal high-water mark.
// The caller returns without processing when initialized=true, so the backlog
// that predates first enable is never harvested silently.
func (s *Store) InitializeMemoryHarvestState(ctx context.Context) (initialized bool, err error) {
	result, err := s.db.ExecContext(ctx, `
INSERT INTO memory_harvest_state(singleton, high_water_rowid, high_water_updated_at)
SELECT 1, COALESCE(MAX(rowid), 0), COALESCE(MAX(updated_at), '')
FROM jobs
WHERE state IN ('succeeded', 'failed', 'blocked', 'cancelled')
ON CONFLICT(singleton) DO NOTHING`)
	if err != nil {
		return false, fmt.Errorf("initialize memory harvest high-water: %w", err)
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *Store) ListMemoryHarvestCandidates(ctx context.Context, claimExpiredBefore time.Time, limit int) ([]MemoryHarvestCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, ListMemoryHarvestCandidatesSQL, memoryHarvestDBTime(claimExpiredBefore), limit)
	if err != nil {
		return nil, fmt.Errorf("list memory harvest candidates: %w", err)
	}
	defer rows.Close()
	var out []MemoryHarvestCandidate
	for rows.Next() {
		var c MemoryHarvestCandidate
		if err := rows.Scan(&c.JobID, &c.Agent, &c.AgentRole, &c.JobType, &c.State, &c.Payload, &c.ResultHash); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimMemoryHarvestRun inserts a fresh receipt or reclaims an expired receipt
// that never advanced beyond claimed. A started receipt is never reclaimed.
func (s *Store) ClaimMemoryHarvestRun(ctx context.Context, jobID, resultHash string, now, claimExpiredBefore time.Time) (bool, error) {
	stamp := memoryHarvestDBTime(now)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO memory_harvest_runs(job_id, result_hash, state, claimed_at, updated_at)
VALUES (?, ?, 'claimed', ?, ?)
ON CONFLICT(job_id, result_hash) DO UPDATE SET
	claimed_at = excluded.claimed_at,
	updated_at = excluded.updated_at,
	detail = ''
WHERE memory_harvest_runs.state = 'claimed'
	AND memory_harvest_runs.updated_at <= ?`,
		strings.TrimSpace(jobID), strings.TrimSpace(resultHash), stamp, stamp, memoryHarvestDBTime(claimExpiredBefore))
	if err != nil {
		return false, fmt.Errorf("claim memory harvest receipt: %w", err)
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *Store) StartMemoryHarvestRun(ctx context.Context, jobID, resultHash string, now time.Time) (bool, error) {
	stamp := memoryHarvestDBTime(now)
	result, err := s.db.ExecContext(ctx, `UPDATE memory_harvest_runs
SET state = 'started', started_at = ?, updated_at = ?
WHERE job_id = ? AND result_hash = ? AND state = 'claimed'`, stamp, stamp, jobID, resultHash)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *Store) SkipMemoryHarvestRun(ctx context.Context, jobID, resultHash, detail string, now time.Time) error {
	stamp := memoryHarvestDBTime(now)
	result, err := s.db.ExecContext(ctx, `UPDATE memory_harvest_runs
SET state = 'skipped', finished_at = ?, updated_at = ?, detail = ?
WHERE job_id = ? AND result_hash = ? AND state IN ('claimed', 'started')`,
		stamp, stamp, strings.TrimSpace(detail), jobID, resultHash)
	if err != nil {
		return err
	}
	return requireAffected(result, "memory harvest receipt", jobID+"/"+resultHash)
}

func (s *Store) MarkMemoryHarvestUncertain(ctx context.Context, jobID, resultHash, detail string, now time.Time) error {
	stamp := memoryHarvestDBTime(now)
	result, err := s.db.ExecContext(ctx, `UPDATE memory_harvest_runs
SET state = 'uncertain', finished_at = ?, updated_at = ?, detail = ?
WHERE job_id = ? AND result_hash = ? AND state = 'started'`,
		stamp, stamp, strings.TrimSpace(detail), jobID, resultHash)
	if err != nil {
		return err
	}
	return requireAffected(result, "memory harvest receipt", jobID+"/"+resultHash)
}

// ExpireStartedMemoryHarvestRuns moves abandoned started receipts to uncertain.
// They are surfaced but never retried because provider calls have no idempotency
// token.
func (s *Store) ExpireStartedMemoryHarvestRuns(ctx context.Context, expiredBefore, now time.Time) ([]MemoryHarvestRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT job_id, result_hash FROM memory_harvest_runs
WHERE state = 'started' AND updated_at <= ? ORDER BY updated_at, job_id`, memoryHarvestDBTime(expiredBefore))
	if err != nil {
		return nil, err
	}
	var expired []MemoryHarvestRun
	for rows.Next() {
		var r MemoryHarvestRun
		if err := rows.Scan(&r.JobID, &r.ResultHash); err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	stamp := memoryHarvestDBTime(now)
	for _, r := range expired {
		if _, err := tx.ExecContext(ctx, `UPDATE memory_harvest_runs
SET state='uncertain', finished_at=?, updated_at=?, detail='started receipt expired; provider outcome is uncertain'
WHERE job_id=? AND result_hash=? AND state='started'`, stamp, stamp, r.JobID, r.ResultHash); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return expired, nil
}

// CompleteMemoryHarvestRun commits every staged observation and the done receipt
// in one transaction. The transaction is intentionally entered only after any
// LLM call has completed.
func (s *Store) CompleteMemoryHarvestRun(ctx context.Context, jobID, resultHash string, observations []MemoryObservation, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, obs := range observations {
		if _, err := insertMemoryObservationTx(ctx, tx, obs); err != nil {
			return err
		}
	}
	stamp := memoryHarvestDBTime(now)
	result, err := tx.ExecContext(ctx, `UPDATE memory_harvest_runs
SET state='done', finished_at=?, updated_at=?, candidate_count=?, detail=''
WHERE job_id=? AND result_hash=? AND state IN ('claimed', 'started')`,
		stamp, stamp, len(observations), jobID, resultHash)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("memory harvest receipt %q not claimable", jobID+"/"+resultHash)
	}
	return tx.Commit()
}

func (s *Store) GetMemoryHarvestRun(ctx context.Context, jobID, resultHash string) (MemoryHarvestRun, bool, error) {
	var r MemoryHarvestRun
	err := s.db.QueryRowContext(ctx, `SELECT job_id, result_hash, state, claimed_at, started_at,
	finished_at, updated_at, candidate_count, detail
FROM memory_harvest_runs WHERE job_id=? AND result_hash=?`, jobID, resultHash).Scan(
		&r.JobID, &r.ResultHash, &r.State, &r.ClaimedAt, &r.StartedAt,
		&r.FinishedAt, &r.UpdatedAt, &r.CandidateCount, &r.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return MemoryHarvestRun{}, false, nil
	}
	return r, err == nil, err
}

func (s *Store) CountMemoryHarvestRunsByState(ctx context.Context, state string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_harvest_runs WHERE state=?`, strings.TrimSpace(state)).Scan(&count)
	return count, err
}
