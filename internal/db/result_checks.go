package db

import "context"

// ResultCheckFailure is one failed deterministic result check to persist (#526).
// It is the per-check payload of RecordResultCheckFailures; the job/root/action
// context is passed alongside so a caller never has to repeat it per row.
type ResultCheckFailure struct {
	CheckID     string
	Question    string
	Explanation string
}

// ResultCheckFailureRow is a persisted result-check failure as read back
// (RecordResultCheckFailures writes one row per failed check). It is the
// feed-forward record SkillOpt may later consume as structured feedback; nothing
// consumes it tonight beyond tests and the job-detail cross-check.
type ResultCheckFailureRow struct {
	ID          int64
	JobID       string
	RootID      string
	Action      string
	CheckID     string
	Question    string
	Explanation string
	CreatedAt   string
}

// RecordResultCheckFailures persists the given failed checks for a job as one row
// per check (#526, the SkillOpt feed-forward stub). It is a pure additive write:
// the result_check_failures table is empty until [workflow] result_checks is
// warn/block AND a result fails a check, so every existing DB and every off-mode
// job is byte-identical. An empty slice is a no-op. Best-effort by convention at
// the call site — the audit must never fail a job — but the error is still
// returned so tests can assert the write.
func (s *Store) RecordResultCheckFailures(ctx context.Context, jobID, rootID, action string, checks []ResultCheckFailure) error {
	if len(checks) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range checks {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO result_check_failures(job_id, root_id, action, check_id, question, explanation)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			jobID, rootID, action, c.CheckID, c.Question, c.Explanation); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListResultCheckFailures returns the persisted result-check failures for a job
// in insertion order. It is used by tests and is the read seam a future SkillOpt
// consumer would build on.
func (s *Store) ListResultCheckFailures(ctx context.Context, jobID string) ([]ResultCheckFailureRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, root_id, action, check_id, question, explanation, created_at
		 FROM result_check_failures WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResultCheckFailureRow
	for rows.Next() {
		var r ResultCheckFailureRow
		if err := rows.Scan(&r.ID, &r.JobID, &r.RootID, &r.Action, &r.CheckID, &r.Question, &r.Explanation, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
