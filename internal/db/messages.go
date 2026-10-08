package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Message is immutable conversation data. Status describes the notification,
// never whether the recipient read, accepted or completed anything.
type Message struct {
	ID                 int64  `json:"id"`
	ThreadID           int64  `json:"thread_id"`
	ReplyTo            int64  `json:"reply_to,omitempty"`
	Sender             string `json:"from"`
	Recipient          string `json:"to"`
	Body               string `json:"message"`
	WorkflowID         string `json:"workflow,omitempty"`
	CreatedAt          string `json:"created_at"`
	Status             string `json:"notification_status"`
	Kind               string `json:"kind"`
	SourceKind         string `json:"source_kind"`
	SourceID           string `json:"source_id"`
	Historical         bool   `json:"historical,omitempty"`
	NotificationReason string `json:"notification_reason,omitempty"`
	Lifecycle          string `json:"lifecycle,omitempty"`
	SourcePayload      string `json:"-"`
	Repo               string `json:"repo,omitempty"`
	PullRequest        int    `json:"pull_request,omitempty"`
	HeadSHA            string `json:"head_sha,omitempty"`
	ReviewPurpose      string `json:"review_purpose,omitempty"`
	SourceJobID        string `json:"source_job_id,omitempty"`
	SourceState        string `json:"source_state,omitempty"`
	ReviewDecision     string `json:"review_decision,omitempty"`
}

const messageSelect = `SELECT m.id,m.thread_id,COALESCE(m.reply_to,0),m.sender,m.recipient,m.body,
 n.workflow_id,n.created_at,COALESCE(w.state,CASE WHEN m.recipient='@system' THEN 'not_required' ELSE 'not_notified' END),
 m.kind,m.source_kind,m.source_id,m.historical,COALESCE(w.last_error,''),
 CASE root.kind
 WHEN 'directive' THEN CASE
 WHEN EXISTS(SELECT 1 FROM messages receipt JOIN workflow_notes r ON r.id=receipt.id WHERE receipt.thread_id=root.id AND receipt.kind='directive_receipt' AND r.body LIKE '[org:directive-done id='||root.id||' %') THEN 'completed'
 WHEN EXISTS(SELECT 1 FROM messages receipt JOIN workflow_notes r ON r.id=receipt.id WHERE receipt.thread_id=root.id AND receipt.kind='directive_receipt' AND r.body LIKE '[org:directive-cancel id='||root.id||' %') THEN 'cancelled'
 WHEN EXISTS(SELECT 1 FROM messages receipt JOIN workflow_notes r ON r.id=receipt.id WHERE receipt.thread_id=root.id AND receipt.kind='directive_receipt' AND r.body LIKE '[org:directive-ack id='||root.id||' %') THEN 'accepted'
 ELSE 'pending' END
 WHEN 'escalation' THEN CASE WHEN EXISTS(SELECT 1 FROM messages receipt JOIN workflow_notes r ON r.id=receipt.id WHERE receipt.thread_id=root.id AND receipt.kind='escalation_receipt' AND r.body LIKE '[org:escalate-resolved id='||root.id||' %') THEN 'resolved' ELSE 'open' END
 ELSE '' END, m.source_payload
 FROM messages m JOIN messages root ON root.id=m.thread_id
 JOIN workflow_notes n ON n.id=m.id
 LEFT JOIN wake_outbox w ON w.source_kind=m.source_kind AND w.source_id=m.source_id AND w.target_role=m.recipient`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.ThreadID, &m.ReplyTo, &m.Sender, &m.Recipient, &m.Body, &m.WorkflowID, &m.CreatedAt, &m.Status, &m.Kind, &m.SourceKind, &m.SourceID, &m.Historical, &m.NotificationReason, &m.Lifecycle, &m.SourcePayload)
	if err == nil {
		err = populateMessageProvenance(&m)
	}
	switch m.Status {
	case WakeOutboxStatePending:
		m.Status = "queued"
	case WakeOutboxStateAttempted:
		m.Status = "submitting"
	case WakeOutboxStateDelivered:
		m.Status = "submitted"
	case WakeOutboxStateDeliveryUnknown:
		m.Status = "uncertain"
	case WakeOutboxStateSuperseded:
		m.Status = "resolved"
	}
	return m, err
}

// CreateMessage commits the immutable message, its journal representation and
// existing notification obligation together. Role authorization is the CLI's
// responsibility; participant routing is derived here rather than caller text.
func (s *Store) CreateMessage(ctx context.Context, input Message) (Message, error) {
	input.Sender = strings.TrimSpace(input.Sender)
	input.Recipient = strings.TrimSpace(input.Recipient)
	if input.Sender == "" || strings.TrimSpace(input.Body) == "" || len(input.Body) > 16000 {
		return Message{}, fmt.Errorf("message requires a sender and 1–16000 bytes of text")
	}
	if input.ReplyTo < 0 {
		return Message{}, fmt.Errorf("invalid reply id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	if input.ReplyTo > 0 {
		parent, err := scanMessage(tx.QueryRowContext(ctx, messageSelect+` WHERE m.id=? AND (m.sender=? OR m.recipient=?)`, input.ReplyTo, input.Sender, input.Sender))
		if err != nil {
			return Message{}, fmt.Errorf("reply parent unavailable to this role: %w", err)
		}
		if input.Recipient != "" || input.WorkflowID != "" {
			return Message{}, fmt.Errorf("reply recipient and workflow are inherited")
		}
		input.Recipient = parent.Sender
		if input.Sender == parent.Sender {
			input.Recipient = parent.Recipient
		}
		input.ThreadID = parent.ThreadID
		input.WorkflowID = parent.WorkflowID
	} else {
		if input.ThreadID != 0 {
			return Message{}, fmt.Errorf("thread id is assigned by the store")
		}
		if input.WorkflowID != "" {
			var exists bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_meta WHERE workflow_id=?) OR EXISTS(SELECT 1 FROM jobs WHERE workflow_id=?) OR EXISTS(SELECT 1 FROM workflow_notes WHERE workflow_id=?)`, input.WorkflowID, input.WorkflowID, input.WorkflowID).Scan(&exists)
			if err != nil {
				return Message{}, err
			}
			if !exists {
				return Message{}, fmt.Errorf("workflow %q is not registered", input.WorkflowID)
			}
		}
	}
	if input.Recipient == "" || input.Recipient == input.Sender {
		return Message{}, fmt.Errorf("message requires a distinct recipient")
	}
	input.Kind = "message"
	input.SourceKind = WakeOutboxSourceWorkflowNote
	input.SourceID = ""
	input.SourcePayload = ""
	input.Historical = false
	target := input.Recipient
	if target == MessageSystemSender {
		target = ""
	}
	// The journal wrapper prevents directive-looking conversation text from
	// becoming a formal control note. The structured row retains the exact body.
	noteID, err := insertWorkflowNoteTx(ctx, tx, WorkflowNote{
		WorkflowID: input.WorkflowID, Author: input.Sender,
		Body:            fmt.Sprintf("[message from=%s to=%s]\n%s", input.Sender, input.Recipient, input.Body),
		AddressedTarget: target, Inbox: &input,
	})
	if err != nil {
		return Message{}, err
	}

	if err = tx.Commit(); err != nil {
		return Message{}, err
	}
	return s.GetMessage(ctx, noteID, input.Sender)
}

func (s *Store) GetMessage(ctx context.Context, id int64, role string) (Message, error) {
	if id <= 0 || role == "" {
		return Message{}, sql.ErrNoRows
	}
	return scanMessage(s.db.QueryRowContext(ctx, messageSelect+` WHERE m.id=? AND (m.sender=? OR m.recipient=?)`, id, role, role))
}

// ListMessages is an inbox, not a read receipt. A before-id cursor remains stable
// as newer messages arrive. The caller can request the next older page.
func (s *Store) ListMessages(ctx context.Context, role string, before int64, limit int) ([]Message, error) {
	if role == "" || before < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("inbox requires a role, nonnegative cursor and limit 1–100")
	}
	query := messageSelect + ` WHERE m.recipient=?`
	args := []any{role}
	if before > 0 {
		query += ` AND m.id<?`
		args = append(args, before)
	}
	query += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
