package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const MessageSystemSender = "@system"

func insertInboxMessageTx(ctx context.Context, tx *sql.Tx, m Message) error {
	if m.ThreadID == 0 {
		m.ThreadID = m.ID
	}
	if m.Kind == "" {
		m.Kind = "message"
	}
	if m.SourceKind == "" {
		m.SourceKind = WakeOutboxSourceWorkflowNote
	}
	if m.SourceID == "" {
		m.SourceID = strconv.FormatInt(m.ID, 10)
	}
	var parent any
	if m.ReplyTo != 0 {
		parent = m.ReplyTo
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO messages(id,thread_id,reply_to,sender,recipient,body,kind,source_kind,source_id,historical,source_payload) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, m.ID, m.ThreadID, parent, m.Sender, m.Recipient, m.Body, m.Kind, m.SourceKind, m.SourceID, m.Historical, m.SourcePayload)
	return err
}

// Headers are decoded only from pre-existing typed journal/outbox sources.
// CreateMessage supplies structured metadata first and wraps conversation text,
// so quoted directive or approval text never enters this path as authority.
func inboxNoteHeader(body string) (string, map[string]string, string) {
	end := strings.IndexByte(body, ']')
	if !strings.HasPrefix(body, "[org:") || end < 0 {
		return "", nil, body
	}
	fields := strings.Fields(body[1:end])
	if len(fields) == 0 {
		return "", nil, body
	}
	attrs := make(map[string]string, len(fields)-1)
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			attrs[key] = value
		}
	}
	return strings.TrimPrefix(fields[0], "org:"), attrs, strings.TrimSpace(body[end+1:])
}

func inboxMessageForNote(note WorkflowNote, recipient string) Message {
	kind, attrs, body := inboxNoteHeader(note.Body)
	m := Message{ID: note.ID, Sender: MessageSystemSender, Recipient: recipient, Body: note.Body, Kind: "notification", WorkflowID: note.WorkflowID, CreatedAt: note.CreatedAt}
	// Legacy self-addressed notes are journal notifications, not conversations.
	// Keep their original body and source identity without claiming a participant.
	if note.Author != recipient && attrs["to"] == recipient && ((attrs["from"] != "" && attrs["from"] == note.Author) || (kind == "escalate" && attrs["from"] == "")) {
		switch kind {
		case "message", "escalate", "directive":
			if attrs["from"] != "" {
				m.Sender = note.Author
			}
			m.Kind = kind
			if kind == "escalate" {
				m.Kind = "escalation"
			}
			m.Body = body
		}
	}
	return m
}

// indexWakeMessageTx shares the producer's transaction. Source identity is
// stable across observation/restart and directive nags never add inbox mail.
func indexWakeMessageTx(ctx context.Context, tx *sql.Tx, sourceKind, sourceID, recipient string, historical bool) error {
	exists, err := messageSourceExistsTx(ctx, tx, sourceKind, sourceID, recipient)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if sourceKind == WakeOutboxSourceEvent {
		return fmt.Errorf("event notification must persist its immutable inbox source before enqueue")
	}
	m := Message{Sender: MessageSystemSender, Recipient: recipient, Body: sourceID, Kind: "notification", SourceKind: sourceKind, SourceID: sourceID, Historical: historical}
	if sourceKind == WakeOutboxSourceWorkflowNote {
		var note WorkflowNote
		err := tx.QueryRowContext(ctx, `SELECT id,workflow_id,author,body,repo,created_at FROM workflow_notes WHERE id=?`, sourceID).Scan(&note.ID, &note.WorkflowID, &note.Author, &note.Body, &note.Repo, &note.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		m = inboxMessageForNote(note, recipient)
		m.SourceKind = sourceKind
		m.SourceID = sourceID
		m.Historical = historical
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=?)`, note.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return insertInboxMessageTx(ctx, tx, m)
		}
		// An observer of an addressed note receives a separate notification, not
		// authority or participant access to the original recipient's thread.
		m.ID = 0
		m.Kind = "notification"
		m.Sender = MessageSystemSender
	}
	if sourceKind == WakeOutboxSourceAwaitedFact {
		var fact AwaitedFactWakePayload
		if json.Unmarshal([]byte(sourceID), &fact) == nil {
			m.Kind = "fact"
			if fact.SubjectKind == AwaitedFactSubjectReviewVerdict {
				m.Kind = "review"
			}
			m.Body = fmt.Sprintf("%s %s: %s\n%s", fact.SubjectKind, fact.SubjectKey, fact.State, fact.Detail)
			if fact.JobID != "" {
				m.Body += "\nReview job: " + fact.JobID
			}
			var root int64
			err := tx.QueryRowContext(ctx, `SELECT thread_id FROM messages WHERE source_kind=? AND recipient=? AND CASE WHEN json_valid(source_id) THEN json_extract(source_id,'$.subject_kind') END=? AND CASE WHEN json_valid(source_id) THEN json_extract(source_id,'$.subject_key') END=? ORDER BY id LIMIT 1`, sourceKind, recipient, fact.SubjectKind, fact.SubjectKey).Scan(&root)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				m.ThreadID = root
				m.ReplyTo = root
			}
		}
	} else if sourceKind != WakeOutboxSourceWorkflowNote {
		var event struct {
			Detail string `json:"detail"`
			Type   string `json:"type"`
			JobID  string `json:"job_id"`
		}
		if json.Unmarshal([]byte(sourceID), &event) == nil && event.Detail != "" {
			m.Body = event.Type + ": " + event.Detail
			if event.JobID != "" {
				m.Body += "\nJob: " + event.JobID
			}
		}
	}
	if historical && m.CreatedAt == "" {
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM wake_outbox WHERE source_kind=? AND source_id=? AND target_role=? ORDER BY id LIMIT 1`, sourceKind, sourceID, recipient).Scan(&m.CreatedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO workflow_notes(workflow_id,author,body,created_at) VALUES(?,?,?,COALESCE(NULLIF(?,''),strftime('%Y-%m-%d %H:%M:%S','now')))`, m.WorkflowID, MessageSystemSender, "[inbox notification]\n"+m.Body, m.CreatedAt)
	if err != nil {
		return err
	}
	m.ID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	return insertInboxMessageTx(ctx, tx, m)
}

// Backfill is one atomic, recorded pass and only builds inbox projections. It
// never calls an outbox writer, changes delivery evidence or replays history.
func backfillMessageInboxTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT source_kind,source_id,target_role FROM wake_outbox ORDER BY id`)
	if err != nil {
		return err
	}
	type source struct{ kind, id, role string }
	var sources []source
	for rows.Next() {
		var v source
		if err := rows.Scan(&v.kind, &v.id, &v.role); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,workflow_id,author,body FROM workflow_notes WHERE body LIKE '[org:%' ORDER BY id`)
	if err != nil {
		return err
	}
	var notes []WorkflowNote
	for rows.Next() {
		var n WorkflowNote
		if err := rows.Scan(&n.ID, &n.WorkflowID, &n.Author, &n.Body); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, note := range notes {
		kind, attrs, _ := inboxNoteHeader(note.Body)
		if kind != "message" && kind != "escalate" && kind != "directive" {
			continue
		}
		if (attrs["from"] != note.Author && !(kind == "escalate" && attrs["from"] == "")) || attrs["to"] == "" || attrs["to"] == note.Author {
			continue
		}
		if err := indexWakeMessageTx(ctx, tx, WakeOutboxSourceWorkflowNote, strconv.FormatInt(note.ID, 10), attrs["to"], true); err != nil {
			return err
		}
	}
	// Receipt notes predate typed inbox metadata. Attach them without waking
	// anyone; their immutable bodies remain the lifecycle authority.
	for _, note := range notes {
		kind, attrs, _ := inboxNoteHeader(note.Body)
		if kind != "directive-ack" && kind != "directive-done" && kind != "directive-cancel" && kind != "escalate-resolved" {
			continue
		}
		parent, err := strconv.ParseInt(attrs["id"], 10, 64)
		if err != nil {
			continue
		}
		if err := linkMessageReceiptTx(ctx, tx, &note, parent, true); err != nil {
			return err
		}
	}
	for _, v := range sources {
		if err := indexWakeMessageTx(ctx, tx, v.kind, v.id, v.role, true); err != nil {
			return err
		}
	}
	return nil
}

func linkMessageReceiptTx(ctx context.Context, tx *sql.Tx, note *WorkflowNote, parentID int64, historical bool) error {
	var root Message
	err := tx.QueryRowContext(ctx, `SELECT m.thread_id,m.sender,m.recipient,m.kind,n.workflow_id FROM messages m JOIN workflow_notes n ON n.id=m.id WHERE m.id=?`, parentID).Scan(&root.ThreadID, &root.Sender, &root.Recipient, &root.Kind, &root.WorkflowID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	kind, attrs, _ := inboxNoteHeader(note.Body)
	expectedKind := "directive"
	if kind == "escalate-resolved" {
		expectedKind = "escalation"
	} else if kind != "directive-ack" && kind != "directive-done" && kind != "directive-cancel" {
		return nil
	}
	if root.Kind != expectedKind || attrs["by"] != note.Author || root.WorkflowID != note.WorkflowID {
		return nil
	}
	recipient := root.Sender
	if recipient == note.Author {
		recipient = root.Recipient
	}
	m := Message{ID: note.ID, ThreadID: root.ThreadID, ReplyTo: parentID, Sender: note.Author, Recipient: recipient, Body: note.Body, Kind: root.Kind + "_receipt", Historical: historical}
	if historical {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=?)`, note.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			_, err := tx.ExecContext(ctx, `UPDATE messages SET thread_id=?,reply_to=?,sender=?,recipient=?,body=?,kind=?,historical=1 WHERE id=?`,
				m.ThreadID, m.ReplyTo, m.Sender, m.Recipient, m.Body, m.Kind, m.ID)
			return err
		}
		return insertInboxMessageTx(ctx, tx, m)
	}
	note.Inbox = &m
	note.AddressedTarget = recipient
	if recipient == MessageSystemSender {
		note.AddressedTarget = ""
	}
	return nil
}
