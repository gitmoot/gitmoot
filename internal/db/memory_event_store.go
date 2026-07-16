package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MemoryEventCreated          = "created"
	MemoryEventUpdated          = "updated"
	MemoryEventRetired          = "retired"
	MemoryEventUnretired        = "unretired"
	MemoryEventSuperseded       = "superseded"
	MemoryEventConfirmed        = "confirmed"
	MemoryEventPromoted         = "promoted"
	MemoryEventIngested         = "ingested"
	MemoryEventClusterRecompute = "cluster_recompute"
	MemoryEventClusterRename    = "cluster_rename"

	memoryEventBeforeContentLimit = 2 * 1024
	memoryEventBeforePreviewRunes = 300
)

// ErrMemoryEventCursorUnknown reports a pagination cursor that references no
// existing event (stale client state or a rebuilt store).
var ErrMemoryEventCursorUnknown = fmt.Errorf("unknown memory event cursor")

// MemoryEventKinds enumerates every kind the store emits, in lifecycle order.
// The column itself stays an open string; this list exists for flag/API
// validation and docs.
var MemoryEventKinds = []string{
	MemoryEventCreated, MemoryEventUpdated, MemoryEventRetired,
	MemoryEventUnretired, MemoryEventSuperseded, MemoryEventConfirmed,
	MemoryEventPromoted, MemoryEventIngested,
	MemoryEventClusterRecompute, MemoryEventClusterRename,
}

// MemoryEvent is one immutable brain lifecycle receipt. MemoryID is zero for
// aggregate cluster events, which are stored as SQL NULL.
type MemoryEvent struct {
	ID        int64  `json:"id"`
	At        string `json:"at"`
	Kind      string `json:"kind"`
	MemoryID  int64  `json:"memory_id,omitempty"`
	Key       string `json:"key"`
	OwnerKind string `json:"owner_kind"`
	OwnerRef  string `json:"owner_ref"`
	Repo      string `json:"repo,omitempty"`
	Scope     string `json:"scope"`
	Actor     string `json:"actor"`
	Detail    string `json:"detail"`
}

type MemoryEventFilter struct {
	MemoryID    int64
	Key         string
	Agent       string
	Repo        string
	Kinds       []string
	Since       string
	Limit       int
	BeforeID    int64
	OldestFirst bool
}

type MemoryEventBackfillResult struct {
	Scanned int           `json:"scanned"`
	Created int           `json:"created"`
	Skipped int           `json:"skipped"`
	Events  []MemoryEvent `json:"events"`
}

func nullableMemoryID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func compactMemoryEventDetail(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func memoryEventBeforeDetail(content string) (string, error) {
	if len([]byte(content)) <= memoryEventBeforeContentLimit {
		return compactMemoryEventDetail(map[string]any{"before": content})
	}
	sum := sha256.Sum256([]byte(content))
	return compactMemoryEventDetail(map[string]any{
		"before_hash":    hex.EncodeToString(sum[:]),
		"before_preview": firstRunes(content, memoryEventBeforePreviewRunes),
		"truncated":      true,
	})
}

func firstRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}

func memoryEventActor(preferred, fallback string) string {
	if actor := strings.TrimSpace(preferred); actor != "" {
		return actor
	}
	return strings.TrimSpace(fallback)
}

func insertMemoryEventTx(ctx context.Context, tx *sql.Tx, event MemoryEvent) (int64, error) {
	if strings.TrimSpace(event.At) == "" {
		event.At = nowRFC3339()
	}
	if strings.TrimSpace(event.Kind) == "" {
		return 0, fmt.Errorf("memory event kind is required")
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO memory_events(at, kind, memory_id, key, owner_kind, owner_ref, repo, scope, actor, detail)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.At, event.Kind, nullableMemoryID(event.MemoryID),
		event.Key, event.OwnerKind, event.OwnerRef, nullableRepo(event.Repo), event.Scope, event.Actor, event.Detail)
	if err != nil {
		return 0, fmt.Errorf("insert memory event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

func recordMemoryEventTx(ctx context.Context, tx *sql.Tx, memoryID int64, kind, actor, detail string) error {
	var event MemoryEvent
	var repo sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT id, key, owner_kind, owner_ref, repo, scope
FROM confirmed_memories WHERE id = ?`, memoryID).Scan(
		&event.MemoryID, &event.Key, &event.OwnerKind, &event.OwnerRef, &repo, &event.Scope)
	if err != nil {
		return fmt.Errorf("read confirmed memory %d for event: %w", memoryID, err)
	}
	event.Repo = repo.String
	event.At = nowRFC3339()
	event.Kind = kind
	event.Actor = strings.TrimSpace(actor)
	event.Detail = detail
	_, err = insertMemoryEventTx(ctx, tx, event)
	return err
}

func (s *Store) ListMemoryEvents(ctx context.Context, filter MemoryEventFilter) ([]MemoryEvent, error) {
	where := []string{"1=1"}
	var args []any
	if filter.MemoryID > 0 {
		where = append(where, "memory_id = ?")
		args = append(args, filter.MemoryID)
	}
	if key := strings.TrimSpace(filter.Key); key != "" {
		where = append(where, "key = ?")
		args = append(args, key)
	}
	if agent := strings.TrimSpace(filter.Agent); agent != "" {
		where = append(where, "owner_ref = ?")
		args = append(args, agent)
	}
	if repo := strings.TrimSpace(filter.Repo); repo != "" {
		where = append(where, "repo = ?")
		args = append(args, repo)
	}
	if since := strings.TrimSpace(filter.Since); since != "" {
		where = append(where, "datetime(at) >= datetime(?)")
		args = append(args, since)
	}
	if filter.BeforeID > 0 {
		// A nonexistent cursor id would make the row-value comparison NULL and
		// silently exclude everything; surface it as an error instead so the
		// API can 400 rather than serve a permanently-empty feed.
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_events WHERE id = ?)`, filter.BeforeID).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			return nil, fmt.Errorf("%w: %d", ErrMemoryEventCursorUnknown, filter.BeforeID)
		}
		where = append(where, "(at, id) < (SELECT at, id FROM memory_events WHERE id = ?)")
		args = append(args, filter.BeforeID)
	}
	if len(filter.Kinds) > 0 {
		placeholders := make([]string, 0, len(filter.Kinds))
		for _, kind := range filter.Kinds {
			kind = strings.TrimSpace(kind)
			if kind == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, kind)
		}
		if len(placeholders) > 0 {
			where = append(where, "kind IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	order := "DESC"
	if filter.OldestFirst {
		order = "ASC"
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit)
	// ORDER BY raw at (not datetime(at)): every writer stamps uniform RFC3339
	// UTC, which sorts identically as text, and the raw column keeps
	// idx_memory_events_at usable instead of forcing a full-table sort.
	rows, err := s.db.QueryContext(ctx, `
SELECT id, at, kind, memory_id, key, owner_kind, owner_ref, repo, scope, actor, detail
FROM memory_events WHERE `+strings.Join(where, " AND ")+`
ORDER BY at `+order+`, id `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list memory events: %w", err)
	}
	defer rows.Close()
	out := make([]MemoryEvent, 0)
	for rows.Next() {
		var event MemoryEvent
		var memoryID sql.NullInt64
		var repo sql.NullString
		if err := rows.Scan(&event.ID, &event.At, &event.Kind, &memoryID, &event.Key,
			&event.OwnerKind, &event.OwnerRef, &repo, &event.Scope, &event.Actor, &event.Detail); err != nil {
			return nil, err
		}
		event.MemoryID = memoryID.Int64
		event.Repo = repo.String
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) MaxMemoryEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM memory_events`).Scan(&id)
	return id, err
}

// BackfillMemoryEvents synthesizes tombstone history only for memories without
// a created receipt. That created receipt is the durable idempotency marker.
func (s *Store) BackfillMemoryEvents(ctx context.Context, dryRun bool) (MemoryEventBackfillResult, error) {
	var result MemoryEventBackfillResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
SELECT m.id, m.first_confirmed_at, m.retired_at, m.retired_reason, COALESCE(m.superseded_by, 0),
	m.key, m.owner_kind, m.owner_ref, m.repo, m.scope
FROM confirmed_memories m
ORDER BY m.id`)
	if err != nil {
		return result, err
	}
	type row struct {
		id, supersededBy                                                int64
		first, retiredAt, reason, key, ownerKind, ownerRef, repo, scope string
	}
	var memories []row
	for rows.Next() {
		var item row
		var repo sql.NullString
		if err := rows.Scan(&item.id, &item.first, &item.retiredAt, &item.reason, &item.supersededBy,
			&item.key, &item.ownerKind, &item.ownerRef, &repo, &item.scope); err != nil {
			rows.Close()
			return result, err
		}
		item.repo = repo.String
		memories = append(memories, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	for _, item := range memories {
		result.Scanned++
		// Live birth receipts are recorded under created/confirmed/ingested
		// (callers choose the kind); a row carrying any of those was born after
		// the journal shipped, so synthesizing a created event would fabricate
		// a duplicate birth. Other live kinds (retired/updated/...) are NOT
		// birth receipts — a pre-journal row retired after deploy still needs
		// its synthesized birth.
		var birthCount, liveRetired, liveSuperseded int
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN kind IN ('created','confirmed','ingested') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'retired' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'superseded' THEN 1 ELSE 0 END), 0)
			FROM memory_events WHERE memory_id = ?`, item.id).Scan(&birthCount, &liveRetired, &liveSuperseded); err != nil {
			return result, err
		}
		base := MemoryEvent{MemoryID: item.id, Key: item.key, OwnerKind: item.ownerKind, OwnerRef: item.ownerRef,
			Repo: item.repo, Scope: item.scope, Actor: "cli:memory-log-backfill"}
		synthesized := false
		if birthCount == 0 {
			createdDetail, _ := compactMemoryEventDetail(map[string]any{"backfilled": true})
			event := base
			event.At, event.Kind, event.Detail = item.first, MemoryEventCreated, createdDetail
			result.Events = append(result.Events, event)
			synthesized = true
		}
		// Tombstone receipts are synthesized independently of the birth check:
		// a pre-journal memory retired AFTER the journal shipped already has a
		// live retired event (with the real actor/reason) that must not be
		// duplicated — same for superseded.
		if item.retiredAt != "" && liveRetired == 0 {
			detail, _ := compactMemoryEventDetail(map[string]any{"backfilled": true, "reason": item.reason})
			event := base
			event.At, event.Kind, event.Detail = item.retiredAt, MemoryEventRetired, detail
			result.Events = append(result.Events, event)
			synthesized = true
		}
		if item.supersededBy > 0 && liveSuperseded == 0 {
			detail, _ := compactMemoryEventDetail(map[string]any{"backfilled": true, "superseded_by": item.supersededBy})
			event := base
			event.At, event.Kind, event.Detail = item.retiredAt, MemoryEventSuperseded, detail
			if event.At == "" {
				event.At = item.first
			}
			result.Events = append(result.Events, event)
			synthesized = true
		}
		if !synthesized {
			result.Skipped++
		}
	}
	result.Created = len(result.Events)
	if !dryRun {
		for i := range result.Events {
			id, err := insertMemoryEventTx(ctx, tx, result.Events[i])
			if err != nil {
				return MemoryEventBackfillResult{}, err
			}
			result.Events[i].ID = id
		}
	}
	if dryRun {
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return MemoryEventBackfillResult{}, err
	}
	return result, nil
}

