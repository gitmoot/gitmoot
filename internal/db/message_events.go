package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// EnqueueMessageNotification is the trusted system producer. Source identity is
// independent of rendering and recipient policy; replay cannot rewrite mail.
func (s *Store) EnqueueMessageNotification(ctx context.Context, message Message, recipients []string) error {
	if message.SourceID == "" || message.SourcePayload == "" || (message.Kind != "review" && message.Kind != "notification") {
		return fmt.Errorf("system notification requires a source and notification kind")
	}
	if len(recipients) == 0 {
		return nil
	}
	ctx, cancel := s.durableWriteContext(ctx, 2)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Take the writer lock before checking the dedupe key. A read-first SQLite
	// transaction cannot wait safely while upgrading its read snapshot to write.
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET id=id WHERE source_kind=? AND source_id=?`, WakeOutboxSourceEvent, message.SourceID); err != nil {
		return err
	}
	for _, recipient := range recipients {
		recipient = strings.ToLower(strings.TrimSpace(recipient))
		if recipient == "" || recipient == MessageSystemSender {
			return fmt.Errorf("system notification requires a real recipient")
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE source_kind=? AND source_id=? AND recipient=?)`, WakeOutboxSourceEvent, message.SourceID, recipient).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		message.ID = 0
		message.ThreadID = 0
		message.ReplyTo = 0
		message.SourceKind = WakeOutboxSourceEvent
		message.Sender = MessageSystemSender
		message.Recipient = recipient
		if _, err := insertWorkflowNoteTx(ctx, tx, WorkflowNote{WorkflowID: message.WorkflowID, Author: MessageSystemSender, Body: "[system notification]\n" + message.Body, Inbox: &message}); err != nil {
			return err
		}
		if err := insertWakeOutboxTx(ctx, tx, WakeOutboxSourceEvent, message.SourceID, WakeOutboxKindEvent, recipient); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func populateMessageProvenance(message *Message) error {
	if message.SourceKind == WakeOutboxSourceAwaitedFact {
		var fact AwaitedFactWakePayload
		if err := json.Unmarshal([]byte(message.SourceID), &fact); err != nil {
			return err
		}
		message.SourceState = fact.State
		if fact.SubjectKind == AwaitedFactSubjectReviewVerdict {
			repo, pr, head, err := ParseReviewVerdictSubjectKey(fact.SubjectKey)
			if err != nil {
				return err
			}
			message.Repo = repo
			message.PullRequest = pr
			message.HeadSHA = head
			message.ReviewPurpose = ReviewVerdictKeyPurpose(fact.SubjectKey)
			message.SourceJobID = fact.JobID
		}
	} else if message.SourceKind == WakeOutboxSourceEvent {
		var source struct {
			Repo          string `json:"repo"`
			PullRequest   int    `json:"pull_request"`
			HeadSHA       string `json:"head_sha"`
			ReviewPurpose string `json:"review_purpose"`
			JobID         string `json:"job_id"`
			Status string `json:"status"`
			ReviewDecision string `json:"review_decision"`
		}
		if err := json.Unmarshal([]byte(message.SourcePayload), &source); err != nil {
			return err
		}
		message.Repo = source.Repo
		message.PullRequest = source.PullRequest
		message.HeadSHA = source.HeadSHA
		message.ReviewPurpose = source.ReviewPurpose
		message.SourceJobID = source.JobID
		message.SourceState = source.Status
		message.ReviewDecision = source.ReviewDecision
	}
	return nil
}

// A missing record is not an empty inbox and must not manufacture a source.
func messageSourceExistsTx(ctx context.Context, tx *sql.Tx, kind, id, recipient string) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE source_kind=? AND source_id=? AND recipient=?`, kind, id, recipient).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
