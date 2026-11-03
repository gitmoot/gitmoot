package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OrgSeatAgentIdentity is the last registered agent an "agent:NAME" seat
// binding resolved to (#2502). Session may be empty when Herdr reported none;
// an empty session is never proof of identity. AlertedBinding is the binding
// whose unresolved state was already reported to the seat's parent.
type OrgSeatAgentIdentity struct {
	Role           string
	AgentName      string
	PaneID         string
	Session        string
	ObservedAt     string
	AlertedBinding string
}

// GetOrgSeatAgentIdentity returns the recorded identity; ok is false when none
// is recorded.
func (s *Store) GetOrgSeatAgentIdentity(ctx context.Context, role string) (OrgSeatAgentIdentity, bool, error) {
	var id OrgSeatAgentIdentity
	err := s.db.QueryRowContext(ctx, `
SELECT role, agent_name, pane_id, session, observed_at, alerted_binding
FROM org_seat_agent_identity WHERE role = ?`, role).Scan(
		&id.Role, &id.AgentName, &id.PaneID, &id.Session, &id.ObservedAt, &id.AlertedBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return OrgSeatAgentIdentity{}, false, nil
	}
	return id, err == nil, err
}

// RecordOrgSeatAgentIdentity stores what a seat's binding resolved to now,
// replacing any older proof (an empty session replaces it with no proof), and
// ends any unresolved-binding alert episode.
func (s *Store) RecordOrgSeatAgentIdentity(ctx context.Context, role, agentName, paneID, session string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO org_seat_agent_identity (role, agent_name, pane_id, session, observed_at, alerted_binding)
VALUES (?, ?, ?, ?, ?, '')
ON CONFLICT(role) DO UPDATE SET agent_name = excluded.agent_name, pane_id = excluded.pane_id,
	session = excluded.session, observed_at = excluded.observed_at, alerted_binding = ''`,
		role, agentName, paneID, session, now.UTC().Format(time.RFC3339Nano))
	return err
}

// AlertOrgSeatUnreachableOnce sends the system message body to recipient
// unless the seat's unresolved binding was already reported, marking it
// reported in the SAME transaction as the message, so a failure leaves the
// alert pending for the next tick instead of consumed. It works with no
// recorded identity. It returns whether the message was sent.
func (s *Store) AlertOrgSeatUnreachableOnce(ctx context.Context, role, binding, recipient, body string, now time.Time) (bool, error) {
	recipient = strings.TrimSpace(recipient)
	if role == "" || binding == "" || recipient == "" || recipient == MessageSystemSender || strings.TrimSpace(body) == "" {
		return false, fmt.Errorf("seat alert needs a role, binding, recipient and body")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var alerted string
	err = tx.QueryRowContext(ctx, `SELECT alerted_binding FROM org_seat_agent_identity WHERE role = ?`, role).Scan(&alerted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if alerted == binding {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO org_seat_agent_identity (role, agent_name, pane_id, session, observed_at, alerted_binding)
VALUES (?, '', '', '', ?, ?)
ON CONFLICT(role) DO UPDATE SET alerted_binding = excluded.alerted_binding`,
		role, now.UTC().Format(time.RFC3339Nano), binding); err != nil {
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
