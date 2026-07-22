package db

import (
	"context"
	"errors"
	"strings"
	"time"
)

// BlockedEpisodeTimeLayout is the fixed-width UTC layout for the stored
// blocked_since / emitted_at / updated_at timestamps. It is EXPORTED so the cli
// evaluator parses exactly what this package formats — one shared round-trip
// contract rather than two constants that could silently drift apart.
const BlockedEpisodeTimeLayout = "2006-01-02T15:04:05.000000000Z"

// BlockedEpisode tracks one continuous task or organization-role blocked
// episode. EmittedAt is empty until the first synthesized blocked event, then
// carries the LAST-emitted instant so the evaluator can re-nudge at most once per
// interval while the subject stays blocked (self-healing against a dropped wake)
// rather than firing a single durable one-shot.
type BlockedEpisode struct {
	Subject      string `json:"subject"`
	BlockedSince string `json:"blocked_since"`
	EmittedAt    string `json:"emitted_at,omitempty"`
	// UpdatedAt is the last instant the subject was observed blocked (refreshed on
	// every UpsertBlockedEpisode). The role evaluator reaps an episode whose
	// UpdatedAt has gone stale — the subject stopped being observed blocked — which
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

// ClearBlockedEpisode closes a blocked episode. Deleting a missing row is an
// idempotent no-op.
func (s *Store) ClearBlockedEpisode(ctx context.Context, subject string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_blocked_episodes WHERE subject = ?`, strings.TrimSpace(subject))
	return err
}

// ListBlockedEpisodes returns every open episode in stable subject order.
func (s *Store) ListBlockedEpisodes(ctx context.Context) ([]BlockedEpisode, error) {
	return s.queryBlockedEpisodes(ctx, `SELECT subject, blocked_since, COALESCE(emitted_at, ''), updated_at
		FROM org_blocked_episodes ORDER BY subject`)
}

// ListBlockedEpisodesByPrefix returns only episodes whose subject starts with
// prefix (e.g. "task:owner/repo:" or "role:"), so a per-repo tick reads only the
// rows it can act on instead of scanning the whole table every tick.
func (s *Store) ListBlockedEpisodesByPrefix(ctx context.Context, prefix string) ([]BlockedEpisode, error) {
	return s.queryBlockedEpisodes(ctx, `SELECT subject, blocked_since, COALESCE(emitted_at, ''), updated_at
		FROM org_blocked_episodes WHERE subject LIKE ? ESCAPE '\' ORDER BY subject`, likeLiteralPrefix(prefix))
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
		if err := rows.Scan(&episode.Subject, &episode.BlockedSince, &episode.EmittedAt, &episode.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, episode)
	}
	return result, rows.Err()
}

// likeLiteralPrefix escapes the LIKE metacharacters in a literal prefix (a repo
// name can contain '_') so it matches by prefix rather than as a wildcard.
func likeLiteralPrefix(prefix string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return escaped + "%"
}
