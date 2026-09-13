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
// which job answered (or failed to answer) the same question.
type ReviewRequest struct {
	SubjectKey string
	JobID      string
	Purpose    string
	Requester  string
	CreatedAt  string
	UpdatedAt  string
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

// ClaimReviewRequest inserts the claim for subjectKey standing on jobID. It
// returns the row that holds the claim after the call and whether this caller
// won it. A lost claim is not an error: the caller attaches to the returned job.
func (s *Store) ClaimReviewRequest(ctx context.Context, subjectKey, jobID, purpose, requester string) (ReviewRequest, bool, error) {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO review_requests(subject_key, job_id, purpose, requester, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(subject_key) DO NOTHING`, subjectKey, jobID, strings.ToLower(strings.TrimSpace(purpose)), strings.TrimSpace(requester), stamp, stamp)
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
func (s *Store) ReplaceReviewRequestJob(ctx context.Context, subjectKey, fromJobID, toJobID, requester string) (bool, error) {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
UPDATE review_requests SET job_id = ?, requester = ?, updated_at = ?
WHERE subject_key = ? AND job_id = ?`, toJobID, strings.TrimSpace(requester), stamp, subjectKey, fromJobID)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	return updated == 1, err
}

// ReleaseReviewRequest deletes a claim whose job was never enqueued. It is
// scoped to the job id so a claim that has since moved to another job survives.
func (s *Store) ReleaseReviewRequest(ctx context.Context, subjectKey, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM review_requests WHERE subject_key = ? AND job_id = ?`, subjectKey, jobID)
	return err
}

func (s *Store) GetReviewRequest(ctx context.Context, subjectKey string) (ReviewRequest, error) {
	var request ReviewRequest
	err := s.db.QueryRowContext(ctx, `
SELECT subject_key, job_id, purpose, requester, created_at, updated_at
FROM review_requests WHERE subject_key = ?`, subjectKey).Scan(
		&request.SubjectKey, &request.JobID, &request.Purpose, &request.Requester, &request.CreatedAt, &request.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, sql.ErrNoRows
	}
	return request, err
}
