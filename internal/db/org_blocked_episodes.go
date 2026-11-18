package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// BlockedEpisodeTimeLayout is the fixed-width UTC layout for the stored
// blocked_since / emitted_at / updated_at timestamps. It is EXPORTED so the cli
// evaluator parses exactly what this package formats — one shared round-trip
// contract rather than two constants that could silently drift apart.
const BlockedEpisodeTimeLayout = "2006-01-02T15:04:05.000000000Z"

// BlockedEpisode tracks one continuous condition episode. Subjects use distinct
// prefixes for task blocks, organization-role blocks, and role input-pending
// dialogs. EmittedAt is empty until the first synthesized event, then carries
// the LAST-emitted instant so the evaluator can re-nudge at most once per
// interval while the condition persists (self-healing against a dropped wake)
// rather than firing a single durable one-shot.
type BlockedEpisode struct {
	Subject         string `json:"subject"`
	BlockedSince    string `json:"blocked_since"`
	EmittedAt       string `json:"emitted_at,omitempty"`
	TaskEmitCount   int    `json:"task_emit_count,omitempty"`
	TaskExhaustedAt string `json:"task_exhausted_at,omitempty"`
	// UpdatedAt is the last instant the subject matched its condition (refreshed on
	// every UpsertBlockedEpisode). The role evaluator reaps an episode whose
	// UpdatedAt has gone stale — the subject stopped matching — which
	// distinguishes a genuinely ended block (or a role gone for good) from a
	// transient unknown/absent snapshot blip, without leaking rows forever.
	UpdatedAt string `json:"updated_at,omitempty"`
}

// UpsertBlockedEpisode opens an episode at blockedSince, or refreshes updated_at
// to `now` for an existing one while deliberately retaining its first
// blocked_since instant. `now` must be the evaluator's clock so the staleness
// reap (which compares UpdatedAt against the evaluator's now) is deterministic.
func (s *Store) UpsertBlockedEpisode(ctx context.Context, subject string, blockedSince, now time.Time) error {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return errors.New("blocked episode subject is required")
	}
	stamp := now.UTC().Format(BlockedEpisodeTimeLayout)
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_blocked_episodes(subject, blocked_since, emitted_at, updated_at)
		VALUES (?, ?, NULL, ?)
		ON CONFLICT(subject) DO UPDATE SET updated_at = excluded.updated_at`,
		subject, blockedSince.UTC().Format(BlockedEpisodeTimeLayout), stamp)
	return err
}

// MarkBlockedEpisodeEmitted records the instant a synthesized event was emitted
// for the episode (the LAST-emitted time, used by the once-per-interval re-nudge
// gate). `at` must be the evaluator's clock so the gate is deterministic and
// testable. Marking a missing episode is an idempotent no-op.
func (s *Store) MarkBlockedEpisodeEmitted(ctx context.Context, subject string, at time.Time) error {
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	_, err := s.db.ExecContext(ctx, `UPDATE org_blocked_episodes SET emitted_at = ?, updated_at = ? WHERE subject = ?`,
		stamp, stamp, strings.TrimSpace(subject))
	return err
}

// MarkBlockedTaskEpisodeEmitted advances the finite alert ladder for a task
// episode. The count and exhaustion stamp are durable so daemon restarts cannot
// reset the ladder. A previously exhausted or concurrently changed row is an
// idempotent no-op.
func (s *Store) MarkBlockedTaskEpisodeEmitted(ctx context.Context, subject string, expectedCount, maxEmits int, at time.Time) (int, bool, error) {
	if maxEmits <= 0 {
		return expectedCount, false, errors.New("blocked task max emits must be positive")
	}
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	result, err := s.db.ExecContext(ctx, `UPDATE org_blocked_episodes
		SET emitted_at = ?, updated_at = ?, task_emit_count = task_emit_count + 1,
			task_exhausted_at = CASE WHEN task_emit_count + 1 >= ? THEN ? ELSE task_exhausted_at END
		WHERE subject = ? AND task_emit_count = ? AND task_exhausted_at = ''`,
		stamp, stamp, maxEmits, stamp, strings.TrimSpace(subject), expectedCount)
	if err != nil {
		return expectedCount, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return expectedCount, false, err
	}
	newCount := expectedCount + 1
	return newCount, newCount >= maxEmits, nil
}

// ClearBlockedEpisode closes a blocked episode. Deleting a missing row is an
// idempotent no-op.
func (s *Store) ClearBlockedEpisode(ctx context.Context, subject string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_blocked_episodes WHERE subject = ?`, strings.TrimSpace(subject))
	return err
}

// systemAlertSubjectPrefix is the episode-row namespace that
// SendThrottledSystemAlert owns. Callers pass only their alert key, so a
// throttle row can never collide with a blocked_since episode ("role:",
// "task:", "input-pending:") that an evaluator would read or clear.
const systemAlertSubjectPrefix = "system-alert:"

// SendThrottledSystemAlert sends recipient one system message for key unless
// the key already sent one within interval, and reports whether it sent. The
// throttle is the key's episode row (under systemAlertSubjectPrefix), claimed in
// the same transaction that inserts the message: a restart cannot resend, and a
// failed send stays owed.
func (s *Store) SendThrottledSystemAlert(ctx context.Context, key, recipient, body string, now time.Time, interval time.Duration) (bool, error) {
	key, recipient = strings.TrimSpace(key), strings.TrimSpace(recipient)
	if key == "" || recipient == "" || strings.TrimSpace(body) == "" || interval <= 0 {
		return false, errors.New("throttled system alert needs a key, a recipient, a body and a positive interval")
	}
	subject := systemAlertSubjectPrefix + key
	writeCtx, cancel := s.durableWriteContext(ctx, 2)
	defer cancel()
	sent := false
	err := retryNotificationWrite(writeCtx, func() error {
		var err error
		sent, err = s.sendThrottledSystemAlertOnce(writeCtx, subject, recipient, body, now, interval)
		return err
	})
	return sent, err
}

func (s *Store) sendThrottledSystemAlertOnce(ctx context.Context, subject, recipient, body string, now time.Time, interval time.Duration) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(BlockedEpisodeTimeLayout)
	result, err := tx.ExecContext(ctx, `INSERT INTO org_blocked_episodes(subject, blocked_since, emitted_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(subject) DO UPDATE SET emitted_at = excluded.emitted_at, updated_at = excluded.updated_at
		WHERE org_blocked_episodes.emitted_at IS NULL OR org_blocked_episodes.emitted_at <= ?`,
		subject, stamp, stamp, stamp, now.Add(-interval).UTC().Format(BlockedEpisodeTimeLayout))
	if err != nil {
		return false, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return false, err
	}
	message := Message{Sender: MessageSystemSender, Recipient: recipient, Body: body, Kind: "message", SourceKind: WakeOutboxSourceWorkflowNote}
	if _, err := insertWorkflowNoteTx(ctx, tx, WorkflowNote{
		Author:          MessageSystemSender,
		Body:            fmt.Sprintf("[message from=%s to=%s]\n%s", MessageSystemSender, recipient, body),
		AddressedTarget: recipient, Inbox: &message,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListBlockedEpisodes returns every open episode in stable subject order.
func (s *Store) ListBlockedEpisodes(ctx context.Context) ([]BlockedEpisode, error) {
	return s.queryBlockedEpisodes(ctx, `SELECT subject, blocked_since, COALESCE(emitted_at, ''),
			task_emit_count, task_exhausted_at, updated_at
		FROM org_blocked_episodes ORDER BY subject`)
}

func (s *Store) queryBlockedEpisodes(ctx context.Context, query string, args ...any) ([]BlockedEpisode, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []BlockedEpisode{}
	for rows.Next() {
		var episode BlockedEpisode
		if err := rows.Scan(&episode.Subject, &episode.BlockedSince, &episode.EmittedAt,
			&episode.TaskEmitCount, &episode.TaskExhaustedAt, &episode.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, episode)
	}
	return result, rows.Err()
}
