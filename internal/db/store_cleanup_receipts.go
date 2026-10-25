package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"
)

// ErrCleanupEvidenceChanged rejects publication from an obsolete terminal
// snapshot without charging the replacement lifecycle a cleanup failure.
var ErrCleanupEvidenceChanged = errors.New("cleanup owner evidence changed during publication")

// CleanupReceipt preserves the terminal payload and the producer's eligibility
// evidence independently of the disposable resource and mutable jobs row.
// Only outcome fields may change: completed effects or an explicit transfer to
// a newer lifecycle, never a rewrite of the original evidence.
type CleanupReceipt struct {
	ResourceID             string `json:"resource_id"`
	LifecycleGeneration    int64  `json:"lifecycle_generation"`
	OwnerJobID             string `json:"owner_job_id"`
	ExpectedPath           string `json:"expected_path"`
	TerminalState          string `json:"terminal_state"`
	OwnerPayload           string `json:"owner_payload"`
	Evidence               string `json:"evidence"`
	EligibilityToken       string `json:"eligibility_token"`
	ReceiptID              string `json:"receipt_id"`
	CreatedAt              string `json:"created_at"`
	RemovedAt              string `json:"removed_at"`
	SupersededAt           string `json:"superseded_at,omitempty"`
	SupersededByGeneration int64  `json:"superseded_by_generation,omitempty"`
}

const cleanupReceiptColumns = `resource_id, lifecycle_generation, owner_job_id,
	expected_path, terminal_state, owner_payload, evidence, eligibility_token,
	receipt_id, created_at, removed_at, superseded_at, superseded_by_generation`

func (s *Store) requireCleanupLifecycle(ctx context.Context) error {
	lease, ok := ctx.Value(jobLifecycleFenceKey{}).(*jobLifecycleLease)
	if !ok || lease.store != s || !lease.exclusive || lease.refs.Load() <= 0 {
		return errors.New("cleanup receipt mutation requires the exclusive lifecycle fence")
	}
	return nil
}

// PrepareCleanupReceipt must complete before filesystem deletion. Replaying an
// interrupted episode may reuse exactly the same evidence, never overwrite it.
func (s *Store) PrepareCleanupReceipt(ctx context.Context, receipt CleanupReceipt, now time.Time) (CleanupReceipt, error) {
	if err := s.requireCleanupLifecycle(ctx); err != nil {
		return CleanupReceipt{}, err
	}
	if receipt.OwnerJobID == "" || !filepath.IsAbs(receipt.ExpectedPath) ||
		receipt.ResourceID != CleanupObligationResourceID(receipt.OwnerJobID, receipt.ExpectedPath) ||
		receipt.LifecycleGeneration < 0 || receipt.EligibilityToken == "" || !json.Valid([]byte(receipt.Evidence)) {
		return CleanupReceipt{}, errors.New("cleanup receipt requires resource identity, generation, token and valid evidence")
	}
	job, err := s.GetJob(ctx, receipt.OwnerJobID)
	if err != nil {
		return CleanupReceipt{}, err
	}
	if job.LifecycleGeneration != receipt.LifecycleGeneration || job.State != receipt.TerminalState {
		return CleanupReceipt{}, errors.New("cleanup owner lifecycle changed")
	}
	switch job.State {
	case "succeeded", "failed", "cancelled":
	default:
		return CleanupReceipt{}, errors.New("cleanup owner is not terminal")
	}
	receipt.OwnerPayload = job.Payload
	receipt.ReceiptID, receipt.CreatedAt, receipt.RemovedAt = "", "", ""
	receipt.SupersededAt, receipt.SupersededByGeneration = "", 0
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return CleanupReceipt{}, err
	}
	sum := sha256.Sum256(encoded)
	receipt.ReceiptID = "cleanup-receipt:" + hex.EncodeToString(sum[:])
	receipt.CreatedAt = cleanupObligationTime(now)
	_, err = s.db.ExecContext(ctx, `INSERT INTO cleanup_receipts (`+cleanupReceiptColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', 0)
		ON CONFLICT(resource_id, lifecycle_generation) DO NOTHING`,
		receipt.ResourceID, receipt.LifecycleGeneration, receipt.OwnerJobID,
		receipt.ExpectedPath, receipt.TerminalState, receipt.OwnerPayload,
		receipt.Evidence, receipt.EligibilityToken, receipt.ReceiptID, receipt.CreatedAt)
	if err != nil {
		return CleanupReceipt{}, err
	}
	stored, err := s.GetCleanupReceipt(ctx, receipt.ResourceID, receipt.LifecycleGeneration)
	if err != nil {
		return CleanupReceipt{}, err
	}
	if stored.ReceiptID != receipt.ReceiptID {
		return CleanupReceipt{}, errors.New("cleanup terminal evidence changed within the same lifecycle generation")
	}
	return stored, nil
}

func scanCleanupReceipt(row interface{ Scan(...any) error }) (CleanupReceipt, error) {
	var receipt CleanupReceipt
	err := row.Scan(&receipt.ResourceID, &receipt.LifecycleGeneration, &receipt.OwnerJobID,
		&receipt.ExpectedPath, &receipt.TerminalState, &receipt.OwnerPayload,
		&receipt.Evidence, &receipt.EligibilityToken, &receipt.ReceiptID,
		&receipt.CreatedAt, &receipt.RemovedAt, &receipt.SupersededAt, &receipt.SupersededByGeneration)
	return receipt, err
}

func (s *Store) GetCleanupReceipt(ctx context.Context, resourceID string, generation int64) (CleanupReceipt, error) {
	return scanCleanupReceipt(s.db.QueryRowContext(ctx, `SELECT `+cleanupReceiptColumns+`
		FROM cleanup_receipts WHERE resource_id=? AND lifecycle_generation=?`, resourceID, generation))
}

// CleanupReceiptForToken lets a completed request return its historical receipt
// without consulting or deleting a resource recreated by a later generation.
func (s *Store) CleanupReceiptForToken(ctx context.Context, token string) (CleanupReceipt, error) {
	return scanCleanupReceipt(s.db.QueryRowContext(ctx, `SELECT `+cleanupReceiptColumns+`
		FROM cleanup_receipts WHERE eligibility_token=?`, token))
}

func (s *Store) CompleteCleanupReceipt(ctx context.Context, resourceID string, generation int64, receiptID string, now time.Time) (CleanupReceipt, error) {
	if err := s.requireCleanupLifecycle(ctx); err != nil {
		return CleanupReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CleanupReceipt{}, err
	}
	defer tx.Rollback()
	stamp := cleanupObligationTime(now)
	result, err := tx.ExecContext(ctx, `UPDATE cleanup_obligations
		SET state='removed', reason='removed', next_attempt_at=?, last_error='', updated_at=?
		WHERE resource_id=? AND lifecycle_generation=? AND state <> 'quarantined'`,
		stamp, stamp, resourceID, generation)
	if err != nil {
		return CleanupReceipt{}, err
	}
	if err := requireAffected(result, "cleanup obligation", resourceID); err != nil {
		return CleanupReceipt{}, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE cleanup_receipts
		SET removed_at=CASE WHEN removed_at='' THEN ? ELSE removed_at END
		WHERE resource_id=? AND lifecycle_generation=? AND receipt_id=? AND superseded_at=''`,
		cleanupObligationTime(now), resourceID, generation, receiptID)
	if err != nil {
		return CleanupReceipt{}, err
	}
	if err := requireAffected(result, "cleanup receipt", receiptID); err != nil {
		return CleanupReceipt{}, fmt.Errorf("complete cleanup receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CleanupReceipt{}, err
	}
	return s.GetCleanupReceipt(ctx, resourceID, generation)
}

// SupersedeCleanupReceipt closes an interrupted historical episode without
// touching the replacement resource or the current physical-resource ledger.
// It is not a successful removal receipt: remaining effects belong to the new
// lifecycle and may no longer be authorized by the historical request.
func (s *Store) SupersedeCleanupReceipt(ctx context.Context, receipt CleanupReceipt, generation int64, now time.Time) (CleanupReceipt, error) {
	if err := s.requireCleanupLifecycle(ctx); err != nil {
		return CleanupReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CleanupReceipt{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE cleanup_receipts
		SET superseded_at=?, superseded_by_generation=?
		WHERE resource_id=? AND lifecycle_generation=? AND receipt_id=?
		  AND removed_at='' AND superseded_at='' AND ? > lifecycle_generation
		  AND EXISTS (SELECT 1 FROM jobs WHERE id=cleanup_receipts.owner_job_id AND lifecycle_generation=?)`,
		cleanupObligationTime(now), generation, receipt.ResourceID, receipt.LifecycleGeneration, receipt.ReceiptID, generation, generation)
	if err != nil {
		return CleanupReceipt{}, err
	}
	if err := requireAffected(result, "superseded cleanup receipt", receipt.ReceiptID); err != nil {
		return CleanupReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
		receipt.OwnerJobID, "delegation_worktree_cleanup_superseded",
		fmt.Sprintf("cleanup receipt %s generation %d superseded by generation %d; replacement resource and ledger preserved",
			receipt.ReceiptID, receipt.LifecycleGeneration, generation)); err != nil {
		return CleanupReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return CleanupReceipt{}, err
	}
	return s.GetCleanupReceipt(ctx, receipt.ResourceID, receipt.LifecycleGeneration)
}

// RecordCleanupArtifactSnapshot attaches preserved outputs only to the exact
// terminal snapshot that was collected. Publication runs outside a SQL
// transaction; a same-generation payload change must not be overwritten.
func (s *Store) RecordCleanupArtifactSnapshot(ctx context.Context, owner Job, snapshot string) (bool, error) {
	ctx, release, err := s.AcquireJobLifecycle(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	result, err := s.db.ExecContext(ctx, `UPDATE jobs
		SET payload=json_set(payload, '$.cleanup_artifacts', json(?)), updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND lifecycle_generation=? AND state=? AND payload=?
		  AND state IN ('succeeded', 'failed', 'cancelled')`,
		snapshot, owner.ID, owner.LifecycleGeneration, owner.State, owner.Payload)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// RecordReadOnlyCleanupDiff updates only captured metadata and only while the
// terminal owner snapshot remains unchanged. In particular, a concurrent retry
// cannot receive a diff captured from its previous generation.
func (s *Store) RecordReadOnlyCleanupDiff(ctx context.Context, owner Job, diff string, truncated bool, captureError string) (bool, error) {
	ctx, release, err := s.AcquireJobLifecycle(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET payload=json_set(payload,
		'$.read_only_worktree_diff', ?,
		'$.read_only_worktree_diff_truncated', json(?),
		'$.read_only_worktree_diff_error', ?), updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND lifecycle_generation=? AND state=? AND payload=?
		  AND state IN ('succeeded', 'failed', 'cancelled')`,
		diff, strconv.FormatBool(truncated), captureError, owner.ID,
		owner.LifecycleGeneration, owner.State, owner.Payload)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}
