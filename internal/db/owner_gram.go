package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Owner Gram outcomes. The owner has no pane or seat; wakes addressed to the
// owner role are delivered as one Herdr Gram per drain (owner decision
// 2026-10-08). A Gram that Herdr accepted is NOT proof that the owner read it.
const (
	// OwnerGramAccepted: Herdr returned a gram id for the send.
	OwnerGramAccepted = "accepted"
	// OwnerGramRefused: the send provably did not happen (Herdr rejected the
	// request, the arguments were refused, or the binary could not start).
	OwnerGramRefused = "refused"
	// OwnerGramUnknown: the send may or may not have reached Herdr (timeout,
	// broken connection, success without a readable gram id). Never resent.
	OwnerGramUnknown = "unknown"
)

// OwnerGramRefusedPrefix starts the last_error of an owner wake whose Gram was
// provably not sent. WakeProvenUnsent recognizes it, so an operator can retry
// the row after fixing Herdr.
const OwnerGramRefusedPrefix = "owner gram refused: "

// OwnerGramReceipt is the durable record of one owner Gram attempt for one
// wake row. Every row listed in a Gram gets its own receipt carrying the same
// gram id.
type OwnerGramReceipt struct {
	WakeOutboxID int64
	Attempt      int
	Outcome      string
	GramID       string
	Detail       string
	SentAt       string
}

// OwnerAlertHealth counts owner wakes that ended without an accepted Gram and
// that no operator has resolved yet. `gitmoot org status` flags the owner row
// with it, so a failed owner send cannot go unnoticed again.
type OwnerAlertHealth struct {
	Failed  int `json:"failed"`
	Unknown int `json:"unknown"`
	Stalled int `json:"stalled"`
}

// Total is every unresolved undelivered owner alert.
func (h OwnerAlertHealth) Total() int { return h.Failed + h.Unknown + h.Stalled }

// FinishOwnerGramWakeOutbox records the outcome of one owner Gram for every
// claimed (attempted) row it covered, in one transaction: a receipt per row
// stamped with `at`, the time the send finished, and the row's terminal state (accepted -> delivered, refused -> failed,
// unknown -> delivery_unknown). A refused or unknown send also writes the same
// wake_delivery_failed job event pane deliveries write.
func (s *Store) FinishOwnerGramWakeOutbox(ctx context.Context, ids []int64, outcome, gramID, detail string, at time.Time) error {
	if len(ids) == 0 {
		return errors.New("owner gram outcome requires at least one wake id")
	}
	var state string
	switch outcome {
	case OwnerGramAccepted:
		state = WakeOutboxStateDelivered
		if strings.TrimSpace(gramID) == "" {
			return errors.New("accepted owner gram requires a gram id")
		}
	case OwnerGramRefused:
		state = WakeOutboxStateFailed
		detail = OwnerGramRefusedPrefix + strings.TrimSpace(detail)
	case OwnerGramUnknown:
		state = WakeOutboxStateDeliveryUnknown
	default:
		return fmt.Errorf("invalid owner gram outcome %q", outcome)
	}
	detail = strings.TrimSpace(detail)
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	// #1911: contended waits are the first write and the COMMIT.
	writeCtx, cancel := s.durableWriteContext(ctx, 2)
	defer cancel()
	tx, err := s.db.BeginTx(writeCtx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		// The attempt number comes from the row itself, so a receipt can only be
		// written once per claim: a second writer for the same attempt hits the
		// primary key, and the whole outcome rolls back.
		if _, err := tx.ExecContext(writeCtx, `
INSERT INTO owner_gram_receipts(wake_outbox_id, attempt, outcome, gram_id, detail, sent_at)
SELECT id, attempt_count, ?, ?, ?, ? FROM wake_outbox WHERE id = ? AND state = 'attempted'`,
			outcome, strings.TrimSpace(gramID), detail, stamp, id,
		); err != nil {
			return fmt.Errorf("record owner gram receipt for wake %d: %w", id, err)
		}
	}
	if err := finishWakeOutboxRowsTx(writeCtx, tx, ids, state, detail, at); err != nil {
		return err
	}
	// The spacing interval runs from when the send actually finished, not from
	// when the drain reserved the slot, so a slow send cannot shorten it.
	if _, err := tx.ExecContext(writeCtx,
		`UPDATE owner_gram_spacing SET last_send_at = ? WHERE id = 1 AND last_send_at < ?`, stamp, stamp,
	); err != nil {
		return err
	}
	if outcome != OwnerGramAccepted {
		message := fmt.Sprintf("wake delivery failed for owner: owner gram %s: %s", outcome, detail)
		for _, id := range ids {
			if _, err := tx.ExecContext(writeCtx,
				`INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
				fmt.Sprintf("wake-outbox:%d", id), WakeOutboxDeliveryFailedEventKind, message,
			); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ClaimOwnerGramWakeOutbox claims the rows of one owner Gram AND reserves the
// owner Gram slot, in one transaction. The slot (owner_gram_spacing) holds the
// time of the latest owner Gram send, so two drainers with disjoint batches
// cannot both send inside minInterval: the second one finds the slot taken and
// claims nothing. It returns false, with nothing changed, when the slot is not
// free or any row is no longer pending.
func (s *Store) ClaimOwnerGramWakeOutbox(ctx context.Context, ids []int64, at time.Time, minInterval time.Duration) (bool, error) {
	if len(ids) == 0 {
		return false, errors.New("owner gram claim requires at least one wake id")
	}
	query, args, err := wakeOutboxIDUpdate(`
UPDATE wake_outbox
SET state = 'attempted', attempt_count = attempt_count + 1,
	attempted_at = ?, finished_at = NULL, last_error = '', updated_at = ?
WHERE state = 'pending' AND id IN (`, ids, at)
	if err != nil {
		return false, err
	}
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	freeBy := at.UTC().Add(-minInterval).Format(BlockedEpisodeTimeLayout)
	// #1911: contended waits are the first write and the COMMIT.
	writeCtx, cancel := s.durableWriteContext(ctx, 2)
	defer cancel()
	tx, err := s.db.BeginTx(writeCtx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// The layout is fixed-width, so the stamps compare correctly as text.
	result, err := tx.ExecContext(writeCtx, `
INSERT INTO owner_gram_spacing(id, last_send_at) VALUES (1, ?)
ON CONFLICT(id) DO UPDATE SET last_send_at = excluded.last_send_at
WHERE owner_gram_spacing.last_send_at <= ?`, stamp, freeBy)
	if err != nil {
		return false, fmt.Errorf("reserve owner gram slot: %w", err)
	}
	if reserved, err := result.RowsAffected(); err != nil {
		return false, err
	} else if reserved != 1 {
		return false, tx.Rollback()
	}
	result, err = tx.ExecContext(writeCtx, query, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != int64(len(ids)) {
		return false, tx.Rollback()
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// OwnerGramLastSendAt is the latest owner Gram send time (its reservation, or
// its recorded outcome once known), or the zero time when none was sent.
func (s *Store) OwnerGramLastSendAt(ctx context.Context) (time.Time, error) {
	var stamp string
	err := s.db.QueryRowContext(ctx, `SELECT last_send_at FROM owner_gram_spacing WHERE id = 1`).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && stamp == "") {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(BlockedEpisodeTimeLayout, stamp)
}

// ListOwnerGramReceipts returns every owner Gram receipt, oldest first.
func (s *Store) ListOwnerGramReceipts(ctx context.Context) ([]OwnerGramReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT wake_outbox_id, attempt, outcome, gram_id, detail, sent_at
FROM owner_gram_receipts ORDER BY sent_at, wake_outbox_id, attempt`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var receipts []OwnerGramReceipt
	for rows.Next() {
		var receipt OwnerGramReceipt
		if err := rows.Scan(&receipt.WakeOutboxID, &receipt.Attempt, &receipt.Outcome, &receipt.GramID, &receipt.Detail, &receipt.SentAt); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

// OwnerAlertHealth counts wakes addressed to ownerRole that are in a terminal
// undelivered state an operator has not resolved: failed, delivery_unknown or
// stalled. Superseding or retrying a row (gitmoot org wake) clears it.
func (s *Store) OwnerAlertHealth(ctx context.Context, ownerRole string) (OwnerAlertHealth, error) {
	var health OwnerAlertHealth
	err := s.db.QueryRowContext(ctx, `
SELECT
	COALESCE(SUM(state = 'failed'), 0),
	COALESCE(SUM(state = 'delivery_unknown'), 0),
	COALESCE(SUM(state = 'stalled'), 0)
FROM wake_outbox
WHERE target_role = ? AND state IN ('failed', 'delivery_unknown', 'stalled')`,
		strings.ToLower(strings.TrimSpace(ownerRole)),
	).Scan(&health.Failed, &health.Unknown, &health.Stalled)
	return health, err
}

// PullRequestForHead finds the pull request of repo whose recorded head is
// head. The owner Gram uses it to link a gate escalation, which names only the
// head; it never looks outside the alert's own repository.
func (s *Store) PullRequestForHead(ctx context.Context, repo, head string) (int, bool, error) {
	repo, head = strings.TrimSpace(repo), strings.TrimSpace(head)
	if repo == "" || head == "" {
		return 0, false, nil
	}
	var number int
	err := s.db.QueryRowContext(ctx, `
SELECT number FROM pull_requests
WHERE repo_full_name = ? AND head_sha = ? ORDER BY updated_at DESC, id DESC LIMIT 1`, repo, head).Scan(&number)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return number, true, nil
}
