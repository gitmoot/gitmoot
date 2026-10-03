package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) ListMessageThread(ctx context.Context, id int64, role string, before int64, limit int) ([]Message, error) {
	if before < 0 || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid thread page")
	}
	message, err := s.GetMessage(ctx, id, role)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, messageSelect+` WHERE m.thread_id=? AND (root.sender=? OR root.recipient=? OR m.sender=? OR m.recipient=?) AND (?=0 OR m.id<?) ORDER BY m.id DESC LIMIT ?`, message.ThreadID, role, role, role, role, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (s *Store) MessageForNotification(ctx context.Context, kind, id, role string) (Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, messageSelect+` WHERE m.source_kind=? AND m.source_id=? AND m.recipient=?`, kind, id, role))
}

// ResolveMessageEscalation serializes closure with any competing resolver.
// Repeating a resolution returns the original receipt, without another wake.
func (s *Store) ResolveMessageEscalation(ctx context.Context, id int64, by, answer string, answerID int64) (WorkflowNote, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowNote{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_notes SET workflow_id=workflow_id WHERE id=?`, id); err != nil {
		return WorkflowNote{}, err
	}
	root, err := scanMessage(tx.QueryRowContext(ctx, messageSelect+` WHERE m.id=? AND m.kind='escalation'`, id))
	if err != nil {
		return WorkflowNote{}, err
	}
	if by != root.Sender && by != root.Recipient {
		return WorkflowNote{}, fmt.Errorf("only the requester or addressed coordinator may resolve this escalation")
	}
	prefix := fmt.Sprintf("[org:escalate-resolved id=%d ", id)
	var existing WorkflowNote
	err = tx.QueryRowContext(ctx, `SELECT r.id,r.workflow_id,r.author,r.body,r.created_at FROM workflow_notes r JOIN messages m ON m.id=r.id WHERE m.thread_id=? AND m.kind='escalation_receipt' AND substr(r.body,1,length(?))=? ORDER BY r.id LIMIT 1`, root.ThreadID, prefix, prefix).Scan(&existing.ID, &existing.WorkflowID, &existing.Author, &existing.Body, &existing.CreatedAt)
	if err == nil {
		_, attrs, _ := inboxNoteHeader(existing.Body)
		if existing.Author != by || (answerID != 0 && attrs["note"] != fmt.Sprint(answerID)) {
			return WorkflowNote{}, fmt.Errorf("escalation already resolved with a different resolver or answer")
		}
		if answer != "" {
			var savedAnswer string
			if err := tx.QueryRowContext(ctx, `SELECT body FROM workflow_notes WHERE id=?`, attrs["note"]).Scan(&savedAnswer); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return WorkflowNote{}, err
			}
			if savedAnswer != "[message resolution answer]\n"+answer {
				return WorkflowNote{}, fmt.Errorf("escalation already resolved with a different answer")
			}
		}
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkflowNote{}, err
	}
	recipient := root.Sender
	if by == recipient {
		recipient = root.Recipient
	}
	var repo string
	if err := tx.QueryRowContext(ctx, `SELECT repo FROM workflow_notes WHERE id=?`, id).Scan(&repo); err != nil {
		return WorkflowNote{}, err
	}
	var citedAnswer, citedAuthor string
	if answerID != 0 {
		err := tx.QueryRowContext(ctx, `SELECT n.body,n.author FROM workflow_notes n
			WHERE n.id=? AND (
				EXISTS(SELECT 1 FROM messages m WHERE m.id=n.id AND m.thread_id=? AND (m.sender=? OR m.recipient=?))
				OR (?<>'' AND n.workflow_id=? AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.id=n.id))
			)`, answerID, root.ThreadID, by, by, root.WorkflowID, root.WorkflowID).Scan(&citedAnswer, &citedAuthor)
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowNote{}, fmt.Errorf("answer note is not in the escalation thread or associated journal")
		}
		if err != nil {
			return WorkflowNote{}, err
		}
	}
	if answer != "" {
		answerID, err = insertWorkflowNoteTx(ctx, tx, WorkflowNote{WorkflowID: root.WorkflowID, Repo: repo, Author: by, Body: "[message resolution answer]\n" + answer, Inbox: &Message{Sender: by, Recipient: recipient, Body: answer, ThreadID: root.ThreadID, ReplyTo: id}})
		if err != nil {
			return WorkflowNote{}, err
		}
	}
	body := fmt.Sprintf("[org:escalate-resolved id=%d by=%s", id, by)
	if answerID != 0 {
		body += fmt.Sprintf(" note=%d", answerID)
	}
	body += "] resolved"
	note := WorkflowNote{WorkflowID: root.WorkflowID, Repo: repo, Author: by, Body: body}
	if err := linkMessageReceiptTx(ctx, tx, &note, id, false); err != nil {
		return WorkflowNote{}, err
	}
	if answer != "" {
		note.Inbox.Body = "Resolved: " + answer
	}
	if citedAnswer != "" {
		note.Inbox.Body = fmt.Sprintf("Resolved using note %d (%s):\n%s", answerID, citedAuthor, citedAnswer)
	}
	note.ID, err = insertWorkflowNoteTx(ctx, tx, note)
	if err != nil {
		return WorkflowNote{}, err
	}
	// A pending question is obsolete after its resolution; uncertain input is
	// reconciled, never retried. Keep both immutable notes as the audit trail.
	if _, err := tx.ExecContext(ctx, `UPDATE wake_outbox SET state='superseded',last_error='escalation resolved before notification',finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE source_kind='workflow_note' AND source_id=CAST(? AS TEXT) AND state IN ('pending','failed','stalled','delivery_unknown')`, id); err != nil {
		return WorkflowNote{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowNote{}, err
	}
	return s.GetWorkflowNote(ctx, note.ID)
}
