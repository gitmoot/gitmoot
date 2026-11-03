package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// OrgSeatAgentIdentity is the last registered agent an "agent:NAME" seat
// binding resolved to (#2502). AlertedAt is set once the seat's unresolved
// binding has been reported, and cleared when it resolves again.
type OrgSeatAgentIdentity struct {
	Role       string
	AgentName  string
	PaneID     string
	Session    string
	ObservedAt string
	AlertedAt  string
}

// GetOrgSeatAgentIdentity returns the recorded identity; ok is false when none
// is recorded.
func (s *Store) GetOrgSeatAgentIdentity(ctx context.Context, role string) (OrgSeatAgentIdentity, bool, error) {
	var id OrgSeatAgentIdentity
	err := s.db.QueryRowContext(ctx, `
SELECT role, agent_name, pane_id, session, observed_at, alerted_at
FROM org_seat_agent_identity WHERE role = ?`, role).Scan(
		&id.Role, &id.AgentName, &id.PaneID, &id.Session, &id.ObservedAt, &id.AlertedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OrgSeatAgentIdentity{}, false, nil
	}
	return id, err == nil, err
}

// RecordOrgSeatAgentIdentity stores the agent a seat resolved to now and
// clears any unresolved-binding alert.
func (s *Store) RecordOrgSeatAgentIdentity(ctx context.Context, role, agentName, paneID, session string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO org_seat_agent_identity (role, agent_name, pane_id, session, observed_at, alerted_at)
VALUES (?, ?, ?, ?, ?, '')
ON CONFLICT(role) DO UPDATE SET agent_name = excluded.agent_name, pane_id = excluded.pane_id,
	session = excluded.session, observed_at = excluded.observed_at, alerted_at = ''`,
		role, agentName, paneID, session, now.UTC().Format(time.RFC3339Nano))
	return err
}

// MarkOrgSeatIdentityAlerted records that the seat's unresolved binding was
// reported. It returns false when it was already reported, so the alert is
// sent once per unresolved episode.
func (s *Store) MarkOrgSeatIdentityAlerted(ctx context.Context, role string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE org_seat_agent_identity SET alerted_at = ? WHERE role = ? AND alerted_at = ''`,
		now.UTC().Format(time.RFC3339Nano), role)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}
