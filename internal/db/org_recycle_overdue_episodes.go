package db

import (
	"context"
	"errors"
	"strings"
	"time"
)

// RecycleOverdueEpisode tracks one continuous organization-role recycle
// overdue episode. EmittedAt carries the last emitted instant so the CLI ingress
// can re-nudge at most once per recycle interval while the role stays overdue.
type RecycleOverdueEpisode struct {
	Subject      string `json:"subject"`
	OverdueSince string `json:"overdue_since"`
	EmittedAt    string `json:"emitted_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// UpsertRecycleOverdueEpisode opens an episode at overdueSince, or refreshes
// updated_at for an existing one while retaining its first overdue_since.
func (s *Store) UpsertRecycleOverdueEpisode(ctx context.Context, subject string, overdueSince, now time.Time) error {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return errors.New("recycle overdue episode subject is required")
	}
	stamp := now.UTC().Format(BlockedEpisodeTimeLayout)
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_recycle_overdue_episodes(subject, overdue_since, emitted_at, updated_at)
		VALUES (?, ?, NULL, ?)
		ON CONFLICT(subject) DO UPDATE SET updated_at = excluded.updated_at`,
		subject, overdueSince.UTC().Format(BlockedEpisodeTimeLayout), stamp)
	return err
}

// MarkRecycleOverdueEpisodeEmitted records the last emitted instant for the
// episode. Marking a missing episode is an idempotent no-op.
func (s *Store) MarkRecycleOverdueEpisodeEmitted(ctx context.Context, subject string, at time.Time) error {
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	_, err := s.db.ExecContext(ctx, `UPDATE org_recycle_overdue_episodes SET emitted_at = ?, updated_at = ? WHERE subject = ?`,
		stamp, stamp, strings.TrimSpace(subject))
	return err
}

// ClearRecycleOverdueEpisode closes an episode. Deleting a missing row is an
// idempotent no-op.
func (s *Store) ClearRecycleOverdueEpisode(ctx context.Context, subject string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_recycle_overdue_episodes WHERE subject = ?`, strings.TrimSpace(subject))
	return err
}

// ListRecycleOverdueEpisodes returns every open episode in stable subject order.
func (s *Store) ListRecycleOverdueEpisodes(ctx context.Context) ([]RecycleOverdueEpisode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, overdue_since, COALESCE(emitted_at, ''), updated_at
		FROM org_recycle_overdue_episodes ORDER BY subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RecycleOverdueEpisode{}
	for rows.Next() {
		var episode RecycleOverdueEpisode
		if err := rows.Scan(&episode.Subject, &episode.OverdueSince, &episode.EmittedAt, &episode.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, episode)
	}
	return result, rows.Err()
}
