package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

// chatAskThreadSlug derives the deterministic, topic-path-safe slug for the chat
// thread auto-linked to a job paused at awaiting_human (#534). It is a pure
// function of the (coordinator) job id, so the link is idempotent: the same
// paused job always maps to the same thread and a re-advance reuses it rather
// than spawning a second thread. The `job-` prefix + lowercase hex keeps it a
// valid topic path ([a-z0-9-], no '+'/'#').
func chatAskThreadSlug(jobID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(jobID)))
	return "job-" + hex.EncodeToString(sum[:6])
}

// shortJobID returns the last 12 characters of a job id for a human-friendly
// thread name; short ids pass through unchanged.
func shortJobID(jobID string) string {
	jobID = strings.TrimSpace(jobID)
	if len(jobID) <= 12 {
		return jobID
	}
	return jobID[len(jobID)-12:]
}

// linkAskGateChatThread is the ask-gate answer channel (#534, keystone): when a
// job first pauses at awaiting_human it best-effort auto-creates (or idempotently
// reuses) a chat thread on the job's repo, records the human_questions (or the
// escalation reason) as a `system` message carrying an origin-qualified job ref,
// enrolls the paused job's agent as a participant, and stamps the thread id onto
// the paused (coordinator) job's payload so the answer-driven continuation
// inherits ThreadID and back-links its result into the same thread.
//
// EVERY failure is swallowed and recorded as a job event — a chat problem must
// NEVER affect the job (the pause remains durable via the escalation event +
// dashboard Attention regardless). It is called only on the FRESH-round path of
// pauseAwaitingHuman / pauseAwaitingHumanAnswer, so it runs at most once per
// round; the deterministic slug makes even a duplicate call idempotent.
func (e Engine) linkAskGateChatThread(ctx context.Context, jobID, repo, agentName, body string) {
	if e.Store == nil || strings.TrimSpace(jobID) == "" {
		return
	}
	repo = strings.TrimSpace(repo)
	slug := chatAskThreadSlug(jobID)

	thread, err := e.Store.GetChatThreadBySlug(ctx, repo, slug)
	if err != nil {
		created, cerr := e.Store.CreateChatThread(ctx, db.ChatThread{
			Slug:      slug,
			Name:      "Job " + shortJobID(jobID) + " — awaiting answer",
			Repo:      repo,
			CreatedBy: db.ChatAuthorKindSystem,
		})
		if cerr != nil {
			// Lost a UNIQUE(repo, slug) create race? re-read the winner.
			if existing, rerr := e.Store.GetChatThreadBySlug(ctx, repo, slug); rerr == nil {
				thread = existing
			} else {
				_ = e.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "chat_link_failed", Message: cerr.Error()})
				return
			}
		} else {
			thread = created
		}
	}

	msg, err := e.Store.AddChatMessage(ctx, db.ChatMessage{
		ThreadID:   thread.ID,
		AuthorKind: db.ChatAuthorKindSystem,
		AuthorName: "system",
		Kind:       db.ChatKindSystem,
		Body:       body,
		Refs:       []db.ChatRef{{Kind: "job", Repo: repo, ID: jobID}},
	})
	if err != nil {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "chat_link_failed", Message: err.Error()})
		return
	}

	if a := strings.TrimSpace(agentName); a != "" {
		_ = e.Store.AddChatMentions(ctx, []db.ChatMention{{
			MessageID: msg.ID,
			ThreadID:  thread.ID,
			Agent:     a,
			Resolved:  true,
			Unread:    true,
		}})
	}

	// Stamp the thread id onto the paused job's payload (only if unset) so the
	// answer-driven continuation inherits ThreadID and back-links its result
	// (#534 items 2/4). Best-effort: a marshal/write failure never fails the pause.
	if job, payload, perr := e.jobPayload(ctx, jobID); perr == nil && strings.TrimSpace(payload.ThreadID) == "" {
		payload.ThreadID = thread.ID
		payload.ChatMessageID = msg.ID
		if encoded, merr := marshalPayload(payload); merr == nil {
			_ = e.Store.UpdateJobPayload(ctx, job.ID, encoded)
		}
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "chat_thread_linked", Message: "linked ask-gate chat thread " + thread.Slug})
}
