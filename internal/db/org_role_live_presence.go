package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RoleLivePresence is the latest Herdr lifecycle observation persisted for one
// configured organization role.
type RoleLivePresence struct {
	Role       string `json:"role"`
	State      string `json:"state"`
	ObservedAt string `json:"observed_at"`
}

func (s *Store) UpsertRoleLivePresence(ctx context.Context, role, state string, observedAt time.Time) error {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return errors.New("org role is required")
	}
	if observedAt.IsZero() {
		return errors.New("org live observed_at is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_role_live_presence(role, state, observed_at)
		VALUES (?, ?, ?)
		ON CONFLICT(role) DO UPDATE SET
			state = excluded.state,
			observed_at = excluded.observed_at,
			updated_at = CURRENT_TIMESTAMP`,
		role, state, observedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListRoleLivePresence(ctx context.Context) ([]RoleLivePresence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, state, observed_at
		FROM org_role_live_presence ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []RoleLivePresence{}
	for rows.Next() {
		var presence RoleLivePresence
		if err := rows.Scan(&presence.Role, &presence.State, &presence.ObservedAt); err != nil {
			return nil, err
		}
		result = append(result, presence)
	}
	return result, rows.Err()
}

// DeleteRoleLivePresenceExcept removes observations for roles absent from the
// latest complete provider snapshot.
func (s *Store) DeleteRoleLivePresenceExcept(ctx context.Context, roles []string) error {
	seen := make(map[string]struct{}, len(roles))
	normalized := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "" {
			continue
		}
		if _, ok := seen[role]; ok {
			continue
		}
		seen[role] = struct{}{}
		normalized = append(normalized, role)
	}
	if len(normalized) == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM org_role_live_presence`)
		return err
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	args := make([]any, len(normalized))
	for i := range normalized {
		args[i] = normalized[i]
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM org_role_live_presence WHERE role NOT IN (%s)`, placeholders), args...)
	return err
}
