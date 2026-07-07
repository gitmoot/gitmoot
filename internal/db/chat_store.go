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

// ChatMessage is one durable message in a thread. The seq is a LOCAL rendering
// convenience (per-thread gapless counter); the authoritative ordering key is
// (TsMs, AuthorName, ID). Mentions/Refs/ReplyTo are the logical fields; they are
// mirrored into the canonical EnvelopeJSON on write.
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

// AddChatMessage appends a message to a thread. It assigns the per-thread seq
// inside the insert transaction (SELECT COALESCE(MAX(seq),0)+1) so concurrent
// appends never collide on UNIQUE(thread_id, seq); it stamps ts_ms with
// unix-millis and origin/author_origin with this DB's home_id, and builds the
// deterministic canonical envelope_json. The returned message carries the
// assigned id/seq/ts_ms/origin/envelope.
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChatMessage{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// The thread must exist (also gives a clear error rather than an orphan row).
	var threadState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM chat_threads WHERE id = ?`, msg.ThreadID).Scan(&threadState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChatMessage{}, fmt.Errorf("chat thread %q not found", msg.ThreadID)
		}
		return ChatMessage{}, err
	}

	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM chat_messages WHERE thread_id = ?`, msg.ThreadID).Scan(&msg.Seq); err != nil {
		return ChatMessage{}, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(
			id, origin, thread_id, seq, ts_ms, author_kind, author_name, author_origin,
			kind, body, envelope_json, refs_json, reply_to, promoted_job_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		msg.ID, msg.Origin, msg.ThreadID, msg.Seq, msg.TsMs, msg.AuthorKind, msg.AuthorName, msg.AuthorOrigin,
		msg.Kind, msg.Body, msg.EnvelopeJSON, string(refsJSON), strings.TrimSpace(msg.ReplyTo), strings.TrimSpace(msg.PromotedJobID)); err != nil {
		return ChatMessage{}, err
	}

	// Bump the thread's activity clock so ListChatThreads orders by recency.
	if _, err := tx.ExecContext(ctx,
		`UPDATE chat_threads SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, msg.ThreadID); err != nil {
		return ChatMessage{}, err
	}

	if err := tx.Commit(); err != nil {
		return ChatMessage{}, err
	}
	return msg, nil
}

// ListChatMessages returns a thread's messages in the authoritative ordering key
// (ts_ms, author_name, id) — NOT by the local seq, which is only a rendering
// convenience. limit <= 0 returns all messages; a positive limit returns the
// most recent `limit` messages (still in ascending order).
func (s *Store) ListChatMessages(ctx context.Context, threadID string, limit int) ([]ChatMessage, error) {
	threadID = strings.TrimSpace(threadID)
	if limit > 0 {
		// Take the newest `limit` by the ordering key, then re-sort ascending.
		rows, err := s.db.QueryContext(ctx, chatMessageSelect+`
			WHERE thread_id = ? ORDER BY ts_ms DESC, author_name DESC, id DESC LIMIT ?`, threadID, limit)
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
		WHERE thread_id = ? ORDER BY ts_ms ASC, author_name ASC, id ASC`, threadID)
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
// message + thread, newest first. unreadOnly restricts to unread mentions.
func (s *Store) InboxForAgent(ctx context.Context, agent string, unreadOnly bool) ([]ChatInboxEntry, error) {
	query := `SELECT t.id, t.slug, t.name, t.repo, m.id, m.seq, m.ts_ms,
			m.author_kind, m.author_name, m.kind, m.body, mn.unread, mn.created_at
		FROM chat_mentions mn
		JOIN chat_messages m ON m.id = mn.message_id
		JOIN chat_threads t ON t.id = mn.thread_id
		WHERE mn.agent = ? AND mn.resolved = 1`
	args := []any{strings.TrimSpace(agent)}
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
// (same thread, same body) was recorded within the last windowMs milliseconds —
// the structural anti-ping-pong fingerprint dedupe (#534). A promotion body
// carries the "@agent message" verbatim, so an identical (thread, body) is an
// identical (thread, agent, body) promotion. windowMs <= 0 disables the check.
func (s *Store) RecentPromotionRequestExists(ctx context.Context, threadID, body string, windowMs int64) (bool, error) {
	if windowMs <= 0 {
		return false, nil
	}
	cutoff := time.Now().UTC().UnixMilli() - windowMs
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM chat_messages WHERE thread_id = ? AND kind = ? AND body = ? AND ts_ms >= ?`,
		strings.TrimSpace(threadID), ChatKindPromotionRequest, body, cutoff).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// MarkThreadRead clears the unread flag on all of an agent's mentions in a
// thread (the "I've seen this conversation" action). Returns the number of
// mentions cleared.
func (s *Store) MarkThreadRead(ctx context.Context, agent, threadID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_mentions SET unread = 0 WHERE agent = ? AND thread_id = ? AND unread = 1`,
		strings.TrimSpace(agent), strings.TrimSpace(threadID))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
