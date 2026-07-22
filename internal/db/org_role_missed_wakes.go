package db

import (
	"context"
	"errors"
	"strings"
	"time"
)

type RoleMissedWake struct {
	Role        string `json:"role"`
	Consecutive int    `json:"consecutive"`
	UpdatedAt   string `json:"updated_at"`
}

func (s *Store) IncrementRoleMissedWake(ctx context.Context, role string, at time.Time) error {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return errors.New("org role is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_role_missed_wakes(role, consecutive, updated_at)
		VALUES (?, 1, ?)
		ON CONFLICT(role) DO UPDATE SET
			consecutive = consecutive + 1,
			updated_at = excluded.updated_at`, role, at.UTC().Format(BlockedEpisodeTimeLayout))
	return err
}

func (s *Store) ResetRoleMissedWake(ctx context.Context, role string) error {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return errors.New("org role is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_role_missed_wakes WHERE role = ?`, role)
	return err
}

func (s *Store) ListRoleMissedWakes(ctx context.Context) ([]RoleMissedWake, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, consecutive, updated_at
		FROM org_role_missed_wakes ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RoleMissedWake
	for rows.Next() {
		var missed RoleMissedWake
		if err := rows.Scan(&missed.Role, &missed.Consecutive, &missed.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, missed)
	}
	return result, rows.Err()
}
