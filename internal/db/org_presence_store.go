package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type OrgRolePresence struct {
	Role        string `json:"role"`
	LastSeenAt  string `json:"last_seen_at"`
	LastCommand string `json:"last_command"`
}

func (s *Store) TouchOrgRolePresence(ctx context.Context, role, command string) error {
	// Canonicalize the key (trim+lower). Org role names are validated lowercase
	// (validOrgRoleName), but cfg.Role accepts case-insensitively, so a raw
	// --org-role like "OWNER" must not create a case-variant row the recycle
	// enforcement read (GetOrgRolePresence) would then miss.
	role = strings.ToLower(strings.TrimSpace(role))
	command = strings.TrimSpace(command)
	if role == "" {
		return errors.New("org role is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_role_presence(role, last_seen_at, last_command)
		VALUES (?, CURRENT_TIMESTAMP, ?)
		ON CONFLICT(role) DO UPDATE SET
			last_seen_at = CURRENT_TIMESTAMP,
			last_command = excluded.last_command`, role, command)
	return err
}

func (s *Store) GetOrgRolePresence(ctx context.Context, role string) (OrgRolePresence, bool, error) {
	var presence OrgRolePresence
	err := s.db.QueryRowContext(ctx, `SELECT role, last_seen_at, last_command
		FROM org_role_presence WHERE role = ?`, strings.ToLower(strings.TrimSpace(role))).Scan(&presence.Role, &presence.LastSeenAt, &presence.LastCommand)
	if errors.Is(err, sql.ErrNoRows) {
		return OrgRolePresence{}, false, nil
	}
	if err != nil {
		return OrgRolePresence{}, false, err
	}
	return presence, true, nil
}

func (s *Store) ListOrgRolePresence(ctx context.Context) ([]OrgRolePresence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, last_seen_at, last_command
		FROM org_role_presence ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []OrgRolePresence
	for rows.Next() {
		var presence OrgRolePresence
		if err := rows.Scan(&presence.Role, &presence.LastSeenAt, &presence.LastCommand); err != nil {
			return nil, err
		}
		result = append(result, presence)
	}
	return result, rows.Err()
}
