package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReviewRequest is the durable claim behind one routed review (#2171). It is
// keyed by repo/PR/exact head/purpose so concurrent requesters resolve to a
// single reviewing job, and it outlives that job so a later requester can see
type ReviewRequest struct {
	SubjectKey string
	JobID      string
	Purpose    string
	Requester  string
	// OwnerPID, OwnerPIDStartTime and OwnerBootID identify the PROCESS holding
	// this claim through its dispatch window, before its job row exists. They
	// make takeover a liveness question rather than a timeout: a dispatch that
	// is merely slow keeps its claim, and one whose process is provably gone
	// releases it immediately. Empty for claims written by an older binary,
	// which then fall back to the age bound.
	OwnerPID          int
	OwnerPIDStartTime string
	OwnerBootID       string
	CreatedAt         string
	UpdatedAt         string
}

// ReviewRequestSubjectKey extends the review-verdict subject key with the
// review purpose. The head-bound prefix is produced by the same constructor the
// awaited-fact subscriptions use, so both identities agree by construction.
func ReviewRequestSubjectKey(repo string, pullRequest int, headSHA, purpose string) (string, error) {
	key, err := ReviewVerdictSubjectKey(repo, pullRequest, headSHA)
	if err != nil {
		return "", err
	}
	purpose = strings.ToLower(strings.TrimSpace(purpose))
	if purpose == "" || strings.ContainsAny(purpose, "|#@ \t\r\n") {
		return "", fmt.Errorf("review purpose is required")
	}
	return key + "|" + purpose, nil
}

// ReviewRequestOwner identifies the process taking a claim, so takeover can ask
// whether the holder is ALIVE instead of whether enough time has passed.
type ReviewRequestOwner struct {
	PID          int
	PIDStartTime string
	BootID       string
}

// ClaimReviewRequest inserts the claim for subjectKey standing on jobID. It
// returns the row that holds the claim after the call and whether this caller
// won it. A lost claim is not an error: the caller attaches to the returned job.
func (s *Store) ClaimReviewRequest(ctx context.Context, subjectKey, jobID, purpose, requester string, owner ReviewRequestOwner) (ReviewRequest, bool, error) {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO review_requests(subject_key, job_id, purpose, requester, owner_pid, owner_pid_start_time, owner_boot_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(subject_key) DO NOTHING`, subjectKey, jobID, strings.ToLower(strings.TrimSpace(purpose)), strings.TrimSpace(requester),
		owner.PID, strings.TrimSpace(owner.PIDStartTime), strings.TrimSpace(owner.BootID), stamp, stamp)
	if err != nil {
		return ReviewRequest{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return ReviewRequest{}, false, err
	}
	current, err := s.GetReviewRequest(ctx, subjectKey)
	if err != nil {
		return ReviewRequest{}, false, err
	}
	return current, inserted == 1 && current.JobID == jobID, nil
}

// ReplaceReviewRequestJob moves the claim from a job that ended without a
// verdict onto a new job. The compare-and-set on the old job id means two
// requesters that both observed the dead job cannot both dispatch.
func (s *Store) ReplaceReviewRequestJob(ctx context.Context, subjectKey, fromJobID, toJobID, requester string, owner ReviewRequestOwner) (bool, error) {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
UPDATE review_requests SET job_id = ?, requester = ?, owner_pid = ?, owner_pid_start_time = ?, owner_boot_id = ?, updated_at = ?
WHERE subject_key = ? AND job_id = ?`, toJobID, strings.TrimSpace(requester),
		owner.PID, strings.TrimSpace(owner.PIDStartTime), strings.TrimSpace(owner.BootID), stamp, subjectKey, fromJobID)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	return updated == 1, err
}

// ReleaseReviewRequest deletes a claim whose review will not produce a verdict:
// either enqueue/admission did not complete or cancellation revoked the accepted
// work. It is scoped to job id so a claim moved to another job survives.
func (s *Store) ReleaseReviewRequest(ctx context.Context, subjectKey, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM review_requests WHERE subject_key = ? AND job_id = ?`, subjectKey, jobID)
	return err
}

func (s *Store) GetReviewRequest(ctx context.Context, subjectKey string) (ReviewRequest, error) {
	var request ReviewRequest
	err := s.db.QueryRowContext(ctx, `
SELECT subject_key, job_id, purpose, requester, owner_pid, owner_pid_start_time, owner_boot_id, created_at, updated_at
FROM review_requests WHERE subject_key = ?`, subjectKey).Scan(
		&request.SubjectKey, &request.JobID, &request.Purpose, &request.Requester,
		&request.OwnerPID, &request.OwnerPIDStartTime, &request.OwnerBootID, &request.CreatedAt, &request.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, sql.ErrNoRows
	}
	return request, err
}
