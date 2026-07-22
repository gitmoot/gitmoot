package db

import (
	"context"
	"errors"
	"strings"
	"time"
)

const blockedEpisodeTimeLayout = "2006-01-02T15:04:05.000000000Z"

// BlockedEpisode tracks one continuous task or organization-role blocked
// episode. EmittedAt is empty until its synthesized blocked event is emitted.
type BlockedEpisode struct {
	Subject      string `json:"subject"`
	BlockedSince string `json:"blocked_since"`
	EmittedAt    string `json:"emitted_at,omitempty"`
}

// UpsertBlockedEpisode opens an episode at blockedSince. Repeated observations
// refresh updated_at but deliberately retain the first blocked_since instant.
func (s *Store) UpsertBlockedEpisode(ctx context.Context, subject string, blockedSince time.Time) error {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return errors.New("blocked episode subject is required")
	}
	now := time.Now().UTC().Format(blockedEpisodeTimeLayout)
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_blocked_episodes(subject, blocked_since, emitted_at, updated_at)
		VALUES (?, ?, NULL, ?)
		ON CONFLICT(subject) DO UPDATE SET updated_at = excluded.updated_at`,
		subject, blockedSince.UTC().Format(blockedEpisodeTimeLayout), now)
	return err
}

// MarkBlockedEpisodeEmitted records that the episode's one synthesized event
// has been emitted. Marking a missing episode is an idempotent no-op.
func (s *Store) MarkBlockedEpisodeEmitted(ctx context.Context, subject string) error {
	now := time.Now().UTC().Format(blockedEpisodeTimeLayout)
	_, err := s.db.ExecContext(ctx, `UPDATE org_blocked_episodes SET emitted_at = ?, updated_at = ? WHERE subject = ?`,
		now, now, strings.TrimSpace(subject))
	return err
}

// ClearBlockedEpisode closes a blocked episode. Deleting a missing row is an
// idempotent no-op.
func (s *Store) ClearBlockedEpisode(ctx context.Context, subject string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_blocked_episodes WHERE subject = ?`, strings.TrimSpace(subject))
	return err
}

// ListBlockedEpisodes returns every open episode in stable subject order.
func (s *Store) ListBlockedEpisodes(ctx context.Context) ([]BlockedEpisode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, blocked_since, COALESCE(emitted_at, '')
		FROM org_blocked_episodes ORDER BY subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []BlockedEpisode{}
	for rows.Next() {
		var episode BlockedEpisode
		if err := rows.Scan(&episode.Subject, &episode.BlockedSince, &episode.EmittedAt); err != nil {
			return nil, err
		}
		result = append(result, episode)
	}
	return result, rows.Err()
}
