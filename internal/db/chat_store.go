package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file holds the store layer for the native agent chat ledger (#534, V1
// local-only). It owns the chat_threads / chat_messages / chat_mentions tables
// plus the tiny chat_meta key/value table that carries the generated stable
// per-DB home_id. It is pure SQL: slug validation and mention parsing live in
// internal/mention and the CLI layer; this layer only persists and reads.
//
// Federation-readiness (the #705 bridge is parked, V1 is zero-network) is
// enforced here as discipline, not features: every write stamps the DB's
// home_id into the origin/author_origin/agent_origin columns (the "self"-
// equivalent) and builds a versioned canonical envelope. No read path assumes
// origin == "self".

// ChatThreadState is the lifecycle of a chat thread.
const (
	ChatThreadStateOpen     = "open"
	ChatThreadStateArchived = "archived"
)

// Chat message author kinds (the "kind" of the author triple).
const (
	ChatAuthorKindHuman  = "human"
	ChatAuthorKindAgent  = "agent"
	ChatAuthorKindSystem = "system"
)

// Chat message kinds — the fixed V1 vocabulary. promotion_request is distinct
// from chat and (per the interaction model) always re-authorized locally;
// job_result messages are non-promotable.
const (
	ChatKindChat             = "chat"
	ChatKindSystem           = "system"
	ChatKindJobResult        = "job_result"
	ChatKindPromotionRequest = "promotion_request"
)

// chatEnvelopeSchemaVersion is the version stamped into every message's
// canonical envelope_json. Bump only on a breaking envelope-shape change;
// additive/omitempty evolution does NOT bump it.
const chatEnvelopeSchemaVersion = 1

// validChatKinds is the fixed message-kind vocabulary AddChatMessage accepts.
var validChatKinds = map[string]bool{
	ChatKindChat:             true,
	ChatKindSystem:           true,
	ChatKindJobResult:        true,
	ChatKindPromotionRequest: true,
}

// ChatThread is one durable, repo-scoped conversation. slug is the topic-path-
// safe stable handle (unique per repo); name is the human display title.
type ChatThread struct {
	ID        string
	Slug      string
	Name      string
	Repo      string
	Origin    string
	State     string
	CreatedBy string
	CreatedAt string
	UpdatedAt string
}

// ChatRef is an origin-qualified opaque handle to a Gitmoot entity (job, PR,
// artifact, …). The id/url are machine-local and must never be dereferenced by a
// non-origin machine — origin marks whose namespace id lives in.
type ChatRef struct {
	Kind   string `json:"kind"`
	Origin string `json:"origin"`
	Repo   string `json:"repo"`
	ID     string `json:"id"`
	URL    string `json:"url"`
}

// ChatMessage is one durable message in a thread. The seq is a per-thread gapless
// LOCAL insertion counter; the rendering order key is (TsMs, Seq) — ts_ms is the
// chronological key and seq is the deterministic same-timestamp tiebreak (never
// the random ID). seq is a local ordering key, not a cross-origin federation
// assumption. Mentions/Refs/ReplyTo are the logical fields; they are mirrored into
// the canonical EnvelopeJSON on write.
type ChatMessage struct {
	ID            string
	Origin        string
	ThreadID      string
	Seq           int64
	TsMs          int64
	AuthorKind    string
	AuthorName    string
	AuthorOrigin  string
	Kind          string
	Body          string
	Mentions      []string
	Refs          []ChatRef
	ReplyTo       string
	PromotedJobID string
	EnvelopeJSON  string
	CreatedAt     string
}

// ChatMention is one inbox-index row: agent A was tagged in message M of thread
// T. resolved marks whether the mention named a known registered agent; unread
// is the per-agent read state.
type ChatMention struct {
	MessageID   string
	ThreadID    string
	Agent       string
	AgentOrigin string
	Resolved    bool
	Unread      bool
	CreatedAt   string
}

// ChatInboxEntry is one row of an agent's inbox: the mention joined with its
// message and thread so a caller can render "who said what, where" without
// N-per-row lookups.
type ChatInboxEntry struct {
	ThreadID   string
	ThreadSlug string
	ThreadName string
	Repo       string
	MessageID  string
	Seq        int64
	TsMs       int64
	AuthorKind string
	AuthorName string
	Kind       string
	Body       string
	Unread     bool
	CreatedAt  string
}

// chatEnvelope is the versioned canonical, self-describing unit of a message. It
// is marshaled with encoding/json, which emits struct fields in declaration
// order, so the serialization is deterministic (a future bridge hashes/signs
// this verbatim). Mentions/Refs are always non-nil so the JSON is stable.
type chatEnvelope struct {
	SchemaVersion int       `json:"schema_version"`
	Kind          string    `json:"kind"`
	Body          string    `json:"body"`
	Mentions      []string  `json:"mentions"`
	Refs          []ChatRef `json:"refs"`
	ReplyTo       string    `json:"reply_to"`
}

// HomeID returns this DB's generated stable home identity, creating it on first
// use (crypto/rand, hex). It is the "self"-equivalent stamped into every origin
// column — never the literal string "self" — so a later bridge can distinguish
// this machine's namespace from a peer's. Idempotent and race-safe (INSERT with
// ON CONFLICT DO NOTHING, then re-read).
func (s *Store) HomeID(ctx context.Context) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM chat_meta WHERE key = 'home_id'`).Scan(&value)
	if err == nil && strings.TrimSpace(value) != "" {
		return value, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	generated := hex.EncodeToString(raw[:])
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_meta(key, value) VALUES ('home_id', ?) ON CONFLICT(key) DO NOTHING`, generated); err != nil {
		return "", err
	}
	// Re-read so a concurrent creator's value wins deterministically.
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM chat_meta WHERE key = 'home_id'`).Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

// CreateChatThread inserts a new thread. slug must already be validated as
// topic-path-safe by the caller (internal/mention/CLI); the store enforces
// non-emptiness and the UNIQUE(repo, slug) constraint. origin is stamped with
// this DB's home_id.
func (s *Store) CreateChatThread(ctx context.Context, thread ChatThread) (ChatThread, error) {
	thread.Slug = strings.TrimSpace(thread.Slug)
	if thread.Slug == "" {
		return ChatThread{}, errors.New("chat thread slug is required")
	}
	thread.Repo = strings.TrimSpace(thread.Repo)
	thread.Name = strings.TrimSpace(thread.Name)
	if thread.Name == "" {
		thread.Name = thread.Slug
	}
	thread.State = strings.TrimSpace(thread.State)
	if thread.State == "" {
		thread.State = ChatThreadStateOpen
	}
	if thread.State != ChatThreadStateOpen && thread.State != ChatThreadStateArchived {
		return ChatThread{}, fmt.Errorf("chat thread state %q is not supported", thread.State)
	}
	if strings.TrimSpace(thread.ID) == "" {
		id, err := newChatID("chat")
		if err != nil {
			return ChatThread{}, err
		}
		thread.ID = id
	}
	origin, err := s.HomeID(ctx)
	if err != nil {
		return ChatThread{}, err
	}
	thread.Origin = origin
	if _, err := s.db.ExecContext(ctx, `INSERT INTO chat_threads(
			id, slug, name, repo, origin, state, created_by, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		thread.ID, thread.Slug, thread.Name, thread.Repo, thread.Origin, thread.State, strings.TrimSpace(thread.CreatedBy)); err != nil {
		// Translate the raw UNIQUE(repo, slug) driver error into a friendly message
		// so re-creating an existing room reads clearly instead of leaking SQL.
		if isUniqueConstraintErr(err) {
			return ChatThread{}, fmt.Errorf("a chat thread %q already exists in %s", thread.Slug, thread.Repo)
		}
		return ChatThread{}, err
	}
	return s.GetChatThreadByID(ctx, thread.ID)
}

// GetChatThreadByID returns one thread by id.
func (s *Store) GetChatThreadByID(ctx context.Context, id string) (ChatThread, error) {
	return scanChatThread(s.db.QueryRowContext(ctx, chatThreadSelect+` WHERE id = ?`, strings.TrimSpace(id)))
}

// GetChatThreadBySlug returns one thread by (repo, slug).
func (s *Store) GetChatThreadBySlug(ctx context.Context, repo, slug string) (ChatThread, error) {
	return scanChatThread(s.db.QueryRowContext(ctx, chatThreadSelect+` WHERE repo = ? AND slug = ?`,
		strings.TrimSpace(repo), strings.TrimSpace(slug)))
}

// ListChatThreads returns threads for a repo (empty repo == all repos),
// optionally filtered by state (empty state == all states), newest activity
// first.
func (s *Store) ListChatThreads(ctx context.Context, repo, state string) ([]ChatThread, error) {
	query := chatThreadSelect
	var args []any
	var clauses []string
	if r := strings.TrimSpace(repo); r != "" {
		clauses = append(clauses, "repo = ?")
		args = append(args, r)
	}
	if st := strings.TrimSpace(state); st != "" {
		clauses = append(clauses, "state = ?")
		args = append(args, st)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY updated_at DESC, id DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var threads []ChatThread
	for rows.Next() {
		thread, err := scanChatThread(rows)
		if err != nil {
			return nil, err
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// RenameChatThread updates a thread's human display name. The slug is immutable
// (it is the topic-path-safe stable handle), so a rename never re-slugs.
func (s *Store) RenameChatThread(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("chat thread name is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_threads SET name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, name, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	return chatRowsAffected(res, "chat thread")
}

// SetChatThreadState archives (close) or re-opens (reopen) a thread.
func (s *Store) SetChatThreadState(ctx context.Context, id, state string) error {
	state = strings.TrimSpace(state)
	if state != ChatThreadStateOpen && state != ChatThreadStateArchived {
		return fmt.Errorf("chat thread state %q is not supported", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_threads SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, state, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	return chatRowsAffected(res, "chat thread")
}

// maxSeqAssignRetries bounds the AddChatMessage seq-assignment retry loop (see
// below). A handful of retries is ample: a per-thread seq collision only occurs
// when two SEPARATE processes (e.g. the daemon back-link + a human `chat send`)
// race to append to the same thread, which is rare and self-limiting.
const maxSeqAssignRetries = 8

// AddChatMessage appends a message to a thread. It assigns the per-thread seq as
// SELECT COALESCE(MAX(seq),0)+1 inside the insert transaction and inserts against
// UNIQUE(thread_id, seq). SetMaxOpenConns(1) serializes appends within ONE Store,
// but the DB is shared across processes (the daemon and a human `chat send` hold
// independent WAL connections), so two processes can compute the same seq: the
// loser's INSERT fails the UNIQUE constraint (or trips SQLITE_BUSY_SNAPSHOT when
// its read snapshot went stale before the write — which busy_timeout does NOT
// retry). We wrap the whole read-modify-write tx in a bounded retry that recomputes
// the seq on such a conflict, so a cross-process race is resolved transparently
// instead of dropping the daemon's back-link or spuriously failing a human send.
// It also stamps ts_ms with unix-millis and origin/author_origin with this DB's
// home_id, and builds the deterministic canonical envelope_json. The returned
// message carries the assigned id/seq/ts_ms/origin/envelope.
func (s *Store) AddChatMessage(ctx context.Context, msg ChatMessage) (ChatMessage, error) {
	msg.ThreadID = strings.TrimSpace(msg.ThreadID)
	if msg.ThreadID == "" {
		return ChatMessage{}, errors.New("chat message thread id is required")
	}
	msg.Kind = strings.TrimSpace(msg.Kind)
	if msg.Kind == "" {
		msg.Kind = ChatKindChat
	}
	if !validChatKinds[msg.Kind] {
		return ChatMessage{}, fmt.Errorf("chat message kind %q is not supported", msg.Kind)
	}
	msg.AuthorKind = strings.TrimSpace(msg.AuthorKind)
	if msg.AuthorKind == "" {
		msg.AuthorKind = ChatAuthorKindHuman
	}
	msg.AuthorName = strings.TrimSpace(msg.AuthorName)
	if msg.AuthorName == "" {
		return ChatMessage{}, errors.New("chat message author name is required")
	}
	if msg.Mentions == nil {
		msg.Mentions = []string{}
	}
	if msg.Refs == nil {
		msg.Refs = []ChatRef{}
	}

	// Resolve home_id BEFORE opening the tx: the store runs on a single DB
	// connection (SetMaxOpenConns(1)), so any s.db call while a tx holds that
	// connection would deadlock. HomeID uses s.db, so it must run first.
	origin, err := s.HomeID(ctx)
	if err != nil {
		return ChatMessage{}, err
	}
	msg.Origin = origin
	if strings.TrimSpace(msg.AuthorOrigin) == "" {
		msg.AuthorOrigin = origin
	}
	// Origin-qualify any refs that did not carry their own origin.
	for i := range msg.Refs {
		if strings.TrimSpace(msg.Refs[i].Origin) == "" {
			msg.Refs[i].Origin = origin
		}
	}
	if strings.TrimSpace(msg.ID) == "" {
		id, err := newChatID("msg")
		if err != nil {
			return ChatMessage{}, err
		}
		msg.ID = id
	}
	msg.TsMs = time.Now().UTC().UnixMilli()

	envelope, err := json.Marshal(chatEnvelope{
		SchemaVersion: chatEnvelopeSchemaVersion,
		Kind:          msg.Kind,
		Body:          msg.Body,
		Mentions:      msg.Mentions,
		Refs:          msg.Refs,
		ReplyTo:       strings.TrimSpace(msg.ReplyTo),
	})
	if err != nil {
		return ChatMessage{}, fmt.Errorf("encode chat envelope: %w", err)
	}
	msg.EnvelopeJSON = string(envelope)
	refsJSON, err := json.Marshal(msg.Refs)
	if err != nil {
		return ChatMessage{}, fmt.Errorf("encode chat refs: %w", err)
	}

	// A sentinel a retryable seq-collision wraps so the outer loop can distinguish it
	// from a genuine (non-retryable) failure returned by the tx body.
	var lastConflict error
	for attempt := 0; attempt < maxSeqAssignRetries; attempt++ {
		commitErr := func() error {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()

			// The thread must exist (also gives a clear error rather than an orphan row).
			var threadState string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM chat_threads WHERE id = ?`, msg.ThreadID).Scan(&threadState); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("chat thread %q not found", msg.ThreadID)
				}
				return err
			}

			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(seq), 0) + 1 FROM chat_messages WHERE thread_id = ?`, msg.ThreadID).Scan(&msg.Seq); err != nil {
				return err
			}

			if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(
					id, origin, thread_id, seq, ts_ms, author_kind, author_name, author_origin,
					kind, body, envelope_json, refs_json, reply_to, promoted_job_id, created_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
				msg.ID, msg.Origin, msg.ThreadID, msg.Seq, msg.TsMs, msg.AuthorKind, msg.AuthorName, msg.AuthorOrigin,
				msg.Kind, msg.Body, msg.EnvelopeJSON, string(refsJSON), strings.TrimSpace(msg.ReplyTo), strings.TrimSpace(msg.PromotedJobID)); err != nil {
				return err
			}

			// Bump the thread's activity clock so ListChatThreads orders by recency.
			if _, err := tx.ExecContext(ctx,
				`UPDATE chat_threads SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, msg.ThreadID); err != nil {
				return err
			}

			return tx.Commit()
		}()
		if commitErr == nil {
			return msg, nil
		}
		if isRetryableSeqConflict(commitErr) {
			lastConflict = commitErr
			continue
		}
		return ChatMessage{}, commitErr
	}
	return ChatMessage{}, fmt.Errorf("assign chat message seq after %d attempts: %w", maxSeqAssignRetries, lastConflict)
}

// isRetryableSeqConflict reports whether err is a transient per-thread seq
// collision that a fresh MAX(seq)+1 retry can resolve: a cross-process
// UNIQUE(thread_id, seq) race, or a WAL snapshot/busy conflict (a write against a
// read snapshot that went stale — SQLITE_BUSY_SNAPSHOT, which busy_timeout does
// not retry). It is deliberately narrow (the UNIQUE match requires the seq column)
// so it never masks a genuine uniqueness bug on some other column.
func isRetryableSeqConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, "seq") {
		return true
	}
	// WAL snapshot / busy conflicts that a retry (re-reading MAX(seq)) resolves.
	return strings.Contains(msg, "SQLITE_BUSY_SNAPSHOT") ||
		strings.Contains(msg, "(517)") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// isUniqueConstraintErr reports whether err is any SQLite UNIQUE-constraint
// violation, used to translate a raw driver error into a friendly message.
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ListChatMessages returns a thread's messages ordered by (ts_ms, seq): ts_ms
// (unix-millis) is the chronological key, and the per-thread gapless `seq` is the
// deterministic same-timestamp tiebreak. seq is assigned locally on insert for
// EVERY message in the thread (so it is a total local insertion order), which is
// why it is the correct tiebreak — ordering by the random message id instead would
// render same-millisecond same-author messages out of insertion order. limit <= 0
// returns all messages; a positive limit returns the most recent `limit` messages
// (still in ascending order).
func (s *Store) ListChatMessages(ctx context.Context, threadID string, limit int) ([]ChatMessage, error) {
	threadID = strings.TrimSpace(threadID)
	if limit > 0 {
		// Take the newest `limit` by the ordering key, then re-sort ascending.
		rows, err := s.db.QueryContext(ctx, chatMessageSelect+`
			WHERE thread_id = ? ORDER BY ts_ms DESC, seq DESC LIMIT ?`, threadID, limit)
		if err != nil {
			return nil, err
		}
		msgs, err := scanChatMessages(rows)
		if err != nil {
			return nil, err
		}
		// Reverse to ascending order.
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
		return msgs, nil
	}
	rows, err := s.db.QueryContext(ctx, chatMessageSelect+`
		WHERE thread_id = ? ORDER BY ts_ms ASC, seq ASC`, threadID)
	if err != nil {
		return nil, err
	}
	return scanChatMessages(rows)
}

// AddChatMentions inserts inbox-index rows for the mentions in a message. Each
// mention's agent_origin is stamped with this DB's home_id when empty. Unknown
// (unresolved) mentions are still recorded (resolved=0) for audit; the inbox
// read path filters to resolved rows.
func (s *Store) AddChatMentions(ctx context.Context, mentions []ChatMention) error {
	if len(mentions) == 0 {
		return nil
	}
	origin, err := s.HomeID(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, m := range mentions {
		agent := strings.TrimSpace(m.Agent)
		if agent == "" {
			continue
		}
		agentOrigin := strings.TrimSpace(m.AgentOrigin)
		if agentOrigin == "" {
			agentOrigin = origin
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_mentions(
				message_id, thread_id, agent, agent_origin, resolved, unread, created_at
			) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
			strings.TrimSpace(m.MessageID), strings.TrimSpace(m.ThreadID), agent, agentOrigin,
			boolInt(m.Resolved), boolInt(m.Unread)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// InboxForAgent returns an agent's inbox: resolved mentions joined with their
// message + thread, newest first. unreadOnly restricts to unread mentions. The
// query is scoped to this DB's home_id (agent_origin) so a mention delivered for
// `agent@other-home` (once the #705 bridge lands) never surfaces in the LOCAL
// agent's inbox — the "no code path may assume origin == self" constraint. In V1
// every local mention carries agent_origin == home_id, so this is a no-op filter.
func (s *Store) InboxForAgent(ctx context.Context, agent string, unreadOnly bool) ([]ChatInboxEntry, error) {
	origin, err := s.HomeID(ctx)
	if err != nil {
		return nil, err
	}
	query := `SELECT t.id, t.slug, t.name, t.repo, m.id, m.seq, m.ts_ms,
			m.author_kind, m.author_name, m.kind, m.body, mn.unread, mn.created_at
		FROM chat_mentions mn
		JOIN chat_messages m ON m.id = mn.message_id
		JOIN chat_threads t ON t.id = mn.thread_id
		WHERE mn.agent = ? AND mn.agent_origin = ? AND mn.resolved = 1`
	args := []any{strings.TrimSpace(agent), origin}
	if unreadOnly {
		query += " AND mn.unread = 1"
	}
	query += " ORDER BY m.ts_ms DESC, m.id DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []ChatInboxEntry
	for rows.Next() {
		var e ChatInboxEntry
		var unread int
		if err := rows.Scan(&e.ThreadID, &e.ThreadSlug, &e.ThreadName, &e.Repo, &e.MessageID, &e.Seq, &e.TsMs,
			&e.AuthorKind, &e.AuthorName, &e.Kind, &e.Body, &unread, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Unread = unread != 0
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// SetChatMessagePromotedJob records the job id a promotion_request message was
// promoted into (#534). The CLI writes it after `chat task` enqueues the job, so
// `chat show` can render the promotion → job back-reference.
func (s *Store) SetChatMessagePromotedJob(ctx context.Context, messageID, jobID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_messages SET promoted_job_id = ? WHERE id = ?`,
		strings.TrimSpace(jobID), strings.TrimSpace(messageID))
	if err != nil {
		return err
	}
	return chatRowsAffected(res, "chat message")
}

// RecentPromotionRequestExists reports whether an identical promotion_request
// (same thread, same body) that ACTUALLY produced a job was recorded within the
// last windowMs milliseconds — the structural anti-ping-pong fingerprint dedupe
// (#534). A promotion body carries the "@agent message" verbatim, so an identical
// (thread, body) is an identical (thread, agent, body) promotion. windowMs <= 0
// disables the check.
//
// Only rows with a non-empty promoted_job_id count: `chat task` records the
// promotion_request BEFORE dispatch (so the intent is durable even if dispatch
// fails) and back-links promoted_job_id only AFTER dispatch succeeds. Counting a
// row whose dispatch failed (promoted_job_id still empty) would let one failed
// attempt poison the window and block a legitimate retry of an identical task even
// though no job was ever created.
func (s *Store) RecentPromotionRequestExists(ctx context.Context, threadID, body string, windowMs int64) (bool, error) {
	if windowMs <= 0 {
		return false, nil
	}
	cutoff := time.Now().UTC().UnixMilli() - windowMs
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM chat_messages WHERE thread_id = ? AND kind = ? AND body = ? AND ts_ms >= ? AND promoted_job_id != ''`,
		strings.TrimSpace(threadID), ChatKindPromotionRequest, body, cutoff).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// MarkThreadRead clears the unread flag on all of an agent's mentions in a
// thread (the "I've seen this conversation" action). Returns the number of
// mentions cleared. Scoped to this DB's home_id (agent_origin) so a local agent
// never marks read a mention addressed to `agent@other-home` (the origin != self
// constraint); in V1 every local mention carries agent_origin == home_id.
func (s *Store) MarkThreadRead(ctx context.Context, agent, threadID string) (int64, error) {
	origin, err := s.HomeID(ctx)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_mentions SET unread = 0 WHERE agent = ? AND agent_origin = ? AND thread_id = ? AND unread = 1`,
		strings.TrimSpace(agent), origin, strings.TrimSpace(threadID))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ChatThreadMessageStat is one thread's message rollup: the total message count
// plus the fields of its most-recent message (the greatest (ts_ms, seq)). It lets
// the dashboard render the threads list — count + a last-message preview per
// thread — without loading every thread's full history (no per-thread N+1).
type ChatThreadMessageStat struct {
	ThreadID       string
	MessageCount   int
	LastSeq        int64
	LastTsMs       int64
	LastAuthorKind string
	LastAuthorName string
	LastKind       string
	LastBody       string
}

// ListChatThreadMessageStats returns, for every thread that has at least one
// message, its message count and its last message in ONE bounded query (window
// functions over chat_messages): a threads-list rollup with no per-thread N+1.
// The "last" message is the greatest (ts_ms DESC, seq DESC) — the same ordering
// key ListChatMessages renders by, so the list preview and the detail's final
// message never disagree. Threads with no messages are simply absent (the caller
// falls back to the thread row's updated_at).
func (s *Store) ListChatThreadMessageStats(ctx context.Context) ([]ChatThreadMessageStat, error) {
	const query = `SELECT thread_id, seq, ts_ms, author_kind, author_name, kind, body, cnt FROM (
			SELECT thread_id, seq, ts_ms, author_kind, author_name, kind, body,
				ROW_NUMBER() OVER (PARTITION BY thread_id ORDER BY ts_ms DESC, seq DESC) AS rn,
				COUNT(*) OVER (PARTITION BY thread_id) AS cnt
			FROM chat_messages
		) WHERE rn = 1
		ORDER BY thread_id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatThreadMessageStat
	for rows.Next() {
		var st ChatThreadMessageStat
		if err := rows.Scan(&st.ThreadID, &st.LastSeq, &st.LastTsMs, &st.LastAuthorKind,
			&st.LastAuthorName, &st.LastKind, &st.LastBody, &st.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ChatThreadMentionCount is one thread's count of resolved, unread mentions,
// scoped to this DB's home_id (agent_origin).
type ChatThreadMentionCount struct {
	ThreadID string
	Count    int
}

// CountUnreadMentionsByThread returns, per thread, the number of resolved+unread
// @mentions in ONE GROUP BY over chat_mentions. It is scoped to this DB's home_id
// (agent_origin) — the same "no read path assumes origin == self" discipline
// InboxForAgent uses — so a mention delivered for `agent@other-home` (once the
// #705 bridge lands) never inflates the local thread's unread badge. In V1 every
// local mention carries agent_origin == home_id, so this is a no-op filter.
func (s *Store) CountUnreadMentionsByThread(ctx context.Context) ([]ChatThreadMentionCount, error) {
	origin, err := s.HomeID(ctx)
	if err != nil {
		return nil, err
	}
	const query = `SELECT thread_id, COUNT(*) FROM chat_mentions
		WHERE agent_origin = ? AND resolved = 1 AND unread = 1
		GROUP BY thread_id ORDER BY thread_id`
	rows, err := s.db.QueryContext(ctx, query, origin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatThreadMentionCount
	for rows.Next() {
		var c ChatThreadMentionCount
		if err := rows.Scan(&c.ThreadID, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChatThreadParticipant is one (thread, participant-name) pair used to derive a
// thread's participant set.
type ChatThreadParticipant struct {
	ThreadID string
	Name     string
}

// ListChatThreadParticipants returns, per thread, the distinct participant names
// in ONE bounded UNION query: real message authors (human/agent) unioned with
// resolved @mention targets (an agent tagged but not yet a poster is still a
// participant). `system` messages are excluded by author_kind — the authorless
// ask-gate post is a system event, not a participant (chat_link.go stamps it with
// author_name "system", so a plain non-empty filter would leak a bogus "system"
// chip). The mention side is scoped to this DB's home_id (agent_origin) — the same
// "no read path assumes origin == self" discipline CountUnreadMentionsByThread /
// InboxForAgent / MarkThreadRead use — so a mention delivered for `agent@other-home`
// (once the #705 bridge lands) never joins a local thread's participant set. In V1
// every local mention carries agent_origin == home_id, so it is a no-op filter.
// Rows come sorted by (thread_id, name); the caller groups them per thread. No
// per-thread N+1.
func (s *Store) ListChatThreadParticipants(ctx context.Context) ([]ChatThreadParticipant, error) {
	origin, err := s.HomeID(ctx)
	if err != nil {
		return nil, err
	}
	const query = `SELECT DISTINCT thread_id, author_name AS name FROM chat_messages
			WHERE TRIM(author_name) != '' AND author_kind != ?
		UNION
		SELECT DISTINCT thread_id, agent AS name FROM chat_mentions
			WHERE agent_origin = ? AND resolved = 1 AND TRIM(agent) != ''
		ORDER BY thread_id, name`
	rows, err := s.db.QueryContext(ctx, query, ChatAuthorKindSystem, origin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatThreadParticipant
	for rows.Next() {
		var p ChatThreadParticipant
		if err := rows.Scan(&p.ThreadID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountUnreadMentionsForThread returns ONE thread's resolved+unread mention count
// with a thread-scoped WHERE (no full-corpus GROUP BY), for the single-thread
// detail path. Origin-scoped like CountUnreadMentionsByThread, so the detail badge
// equals the list badge.
func (s *Store) CountUnreadMentionsForThread(ctx context.Context, threadID string) (int, error) {
	origin, err := s.HomeID(ctx)
	if err != nil {
		return 0, err
	}
	const query = `SELECT COUNT(*) FROM chat_mentions
		WHERE thread_id = ? AND agent_origin = ? AND resolved = 1 AND unread = 1`
	var n int
	if err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(threadID), origin).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListChatThreadParticipantsForThread returns ONE thread's distinct participant
// names with a thread-scoped WHERE (no full-corpus UNION scan), for the
// single-thread detail path. It derives the set the SAME way
// ListChatThreadParticipants does — real authors (system excluded) ∪ resolved,
// origin-scoped @mention targets — so the detail and the list agree. Rows come
// sorted by name.
func (s *Store) ListChatThreadParticipantsForThread(ctx context.Context, threadID string) ([]string, error) {
	origin, err := s.HomeID(ctx)
	if err != nil {
		return nil, err
	}
	tid := strings.TrimSpace(threadID)
	const query = `SELECT author_name AS name FROM chat_messages
			WHERE thread_id = ? AND TRIM(author_name) != '' AND author_kind != ?
		UNION
		SELECT agent AS name FROM chat_mentions
			WHERE thread_id = ? AND agent_origin = ? AND resolved = 1 AND TRIM(agent) != ''
		ORDER BY name`
	rows, err := s.db.QueryContext(ctx, query, tid, ChatAuthorKindSystem, tid, origin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ---- helpers ---------------------------------------------------------------

const chatThreadSelect = `SELECT id, slug, name, repo, origin, state, created_by, created_at, updated_at FROM chat_threads`

const chatMessageSelect = `SELECT id, origin, thread_id, seq, ts_ms, author_kind, author_name, author_origin,
	kind, body, envelope_json, refs_json, reply_to, promoted_job_id, created_at FROM chat_messages`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanChatThread(row rowScanner) (ChatThread, error) {
	var t ChatThread
	if err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.Repo, &t.Origin, &t.State, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return ChatThread{}, err
	}
	return t, nil
}

func scanChatMessages(rows *sql.Rows) ([]ChatMessage, error) {
	defer rows.Close()
	var msgs []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var refsJSON string
		if err := rows.Scan(&m.ID, &m.Origin, &m.ThreadID, &m.Seq, &m.TsMs, &m.AuthorKind, &m.AuthorName, &m.AuthorOrigin,
			&m.Kind, &m.Body, &m.EnvelopeJSON, &refsJSON, &m.ReplyTo, &m.PromotedJobID, &m.CreatedAt); err != nil {
			return nil, err
		}
		if strings.TrimSpace(refsJSON) != "" {
			if err := json.Unmarshal([]byte(refsJSON), &m.Refs); err != nil {
				return nil, fmt.Errorf("decode chat refs: %w", err)
			}
		}
		if m.Refs == nil {
			m.Refs = []ChatRef{}
		}
		// Mentions have no dedicated column — the canonical envelope is their
		// source of truth — so recover them from envelope_json for rendering.
		if strings.TrimSpace(m.EnvelopeJSON) != "" {
			var env chatEnvelope
			if err := json.Unmarshal([]byte(m.EnvelopeJSON), &env); err != nil {
				return nil, fmt.Errorf("decode chat envelope: %w", err)
			}
			m.Mentions = env.Mentions
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func chatRowsAffected(res sql.Result, entity string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s not found", entity)
	}
	return nil
}

func newChatID(prefix string) (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(raw[:]), nil
}
