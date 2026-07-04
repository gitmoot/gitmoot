package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// This file holds the store layer for agent persistent memory (RFC #626,
// Phase 0/1). It owns the two tables from the evidence/upsert split plus the
// standalone FTS5 index over confirmed content. The pure ranking/sanitizing/
// filtering logic lives in internal/memory; this layer is only SQL.

// MemoryOwner is the structured, version-aware identity that owns a memory pool.
// It mirrors the internal/memory.Owner shape but lives here so the db package
// carries no dependency on the memory package.
type MemoryOwner struct {
	Kind    string // "agent" | "role"
	Ref     string // registered agent name, or template identity for a role
	Version string // template version awareness for roles; "" for agents
}

// MemoryObservation is one append-only sighting report for a pending claim.
// Multiple observations of the same key accumulate (append, never upsert) so
// witness-counting for the Phase-2 confirmation protocol has evidence to count.
type MemoryObservation struct {
	ID         int64
	Owner      MemoryOwner
	Repo       string // "" == general (scope carries the authoritative meaning)
	Scope      string
	Key        string
	Content    string
	Provenance string
	TrustMark  string
	SourceJob  string
	CreatedAt  string
}

// ConfirmedMemory is one keyed, injectable fact. Only a confirmation
// transaction (Phase 2) or a gitmoot-authored mechanical producer (Phase 1)
// writes here. Pending observations can never overwrite a confirmed row.
type ConfirmedMemory struct {
	ID               int64
	Owner            MemoryOwner
	Repo             string // "" == general scope (stored as SQL NULL)
	Scope            string
	Key              string
	Content          string
	Provenance       string
	SourceJob        string
	FirstConfirmedAt string
	UpdatedAt        string
	SupersededBy     int64 // 0 == not superseded
}

// nullableRepo maps an empty repo string to SQL NULL (a general-scope fact) and
// a non-empty repo to itself, matching the NULLABLE repo column semantics the
// partial unique indexes depend on.
func nullableRepo(repo string) any {
	if strings.TrimSpace(repo) == "" {
		return nil
	}
	return repo
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// InsertMemoryObservation appends a sighting report to memory_observations. It
// is append-only by construction: repeated observations of the same key produce
// distinct rows so witnesses accumulate.
func (s *Store) InsertMemoryObservation(ctx context.Context, obs MemoryObservation) (int64, error) {
	if strings.TrimSpace(obs.Owner.Kind) == "" || strings.TrimSpace(obs.Owner.Ref) == "" {
		return 0, fmt.Errorf("memory observation requires an owner kind and ref")
	}
	if strings.TrimSpace(obs.Key) == "" || strings.TrimSpace(obs.Content) == "" {
		return 0, fmt.Errorf("memory observation requires a key and content")
	}
	scope := obs.Scope
	if strings.TrimSpace(scope) == "" {
		scope = "repo"
	}
	created := obs.CreatedAt
	if strings.TrimSpace(created) == "" {
		created = nowRFC3339()
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO memory_observations
	(owner_kind, owner_ref, owner_version, repo, scope, key, content, provenance, trust_mark, source_job, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obs.Owner.Kind, obs.Owner.Ref, obs.Owner.Version, nullableRepo(obs.Repo), scope,
		obs.Key, obs.Content, obs.Provenance, obs.TrustMark, obs.SourceJob, created)
	if err != nil {
		return 0, fmt.Errorf("insert memory observation: %w", err)
	}
	return res.LastInsertId()
}

// CountMemoryObservationsForKey returns how many observation rows exist for the
// given owner+repo+key. It backs the append-not-upsert invariant test and the
// Phase-2 witness-counting confirmation protocol.
func (s *Store) CountMemoryObservationsForKey(ctx context.Context, owner MemoryOwner, repo, key string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM memory_observations
WHERE owner_kind = ? AND owner_ref = ? AND owner_version = ?
	AND ((? IS NULL AND repo IS NULL) OR repo = ?)
	AND key = ?`,
		owner.Kind, owner.Ref, owner.Version, nullableRepo(repo), nullableRepo(repo), key).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count memory observations: %w", err)
	}
	return n, nil
}

// UpsertConfirmedMemory writes or updates the single keyed confirmed row for a
// fact and keeps the FTS index in sync, all in one transaction. Writes are
// serialized by the store (MaxOpenConns=1) so a manual select-then-insert/update
// is race-free; it avoids the fragility of ON CONFLICT inference against the
// partial unique indexes. Returns the confirmed row id.
func (s *Store) UpsertConfirmedMemory(ctx context.Context, cm ConfirmedMemory) (int64, error) {
	if strings.TrimSpace(cm.Owner.Kind) == "" || strings.TrimSpace(cm.Owner.Ref) == "" {
		return 0, fmt.Errorf("confirmed memory requires an owner kind and ref")
	}
	if strings.TrimSpace(cm.Key) == "" || strings.TrimSpace(cm.Content) == "" {
		return 0, fmt.Errorf("confirmed memory requires a key and content")
	}
	scope := cm.Scope
	if strings.TrimSpace(scope) == "" {
		if strings.TrimSpace(cm.Repo) == "" {
			scope = "general"
		} else {
			scope = "repo"
		}
	}
	now := nowRFC3339()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx, `
SELECT id FROM confirmed_memories
WHERE owner_kind = ? AND owner_ref = ? AND owner_version = ?
	AND ((? IS NULL AND repo IS NULL) OR repo = ?)
	AND key = ?`,
		cm.Owner.Kind, cm.Owner.Ref, cm.Owner.Version, nullableRepo(cm.Repo), nullableRepo(cm.Repo), cm.Key).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		res, insErr := tx.ExecContext(ctx, `
INSERT INTO confirmed_memories
	(owner_kind, owner_ref, owner_version, repo, scope, key, content, provenance, source_job, first_confirmed_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cm.Owner.Kind, cm.Owner.Ref, cm.Owner.Version, nullableRepo(cm.Repo), scope,
			cm.Key, cm.Content, cm.Provenance, cm.SourceJob, now, now)
		if insErr != nil {
			return 0, fmt.Errorf("insert confirmed memory: %w", insErr)
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, fmt.Errorf("lookup confirmed memory: %w", err)
	default:
		if _, upErr := tx.ExecContext(ctx, `
UPDATE confirmed_memories
SET content = ?, provenance = ?, source_job = ?, updated_at = ?
WHERE id = ?`,
			cm.Content, cm.Provenance, cm.SourceJob, now, id); upErr != nil {
			return 0, fmt.Errorf("update confirmed memory: %w", upErr)
		}
	}

	// Keep the plain FTS5 index in sync inside the same transaction.
	if _, err := tx.ExecContext(ctx, `DELETE FROM confirmed_memories_fts WHERE rowid = ?`, id); err != nil {
		return 0, fmt.Errorf("sync confirmed memory fts (delete): %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO confirmed_memories_fts(rowid, content, key) VALUES (?, ?, ?)`, id, cm.Content, cm.Key); err != nil {
		return 0, fmt.Errorf("sync confirmed memory fts (insert): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// QueryConfirmedMemories is the READ path for job-prompt assembly. It runs one
// FTS5/BM25 query over confirmed content, filtered by the retrieval default
// (owner match AND (repo = current OR scope = general)), confirmed tier only,
// and excludes superseded rows. matchQuery MUST be a sanitized MATCH string
// (see internal/memory.SanitizeFTSQuery) — never raw job text. Results are
// ranked by BM25 (ascending; lower is more relevant) then recency, capped by
// limit. An empty matchQuery returns no rows (no block).
func (s *Store) QueryConfirmedMemories(ctx context.Context, owner MemoryOwner, repo, matchQuery string, limit int) ([]ConfirmedMemory, error) {
	if strings.TrimSpace(matchQuery) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 15
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, c.owner_kind, c.owner_ref, c.owner_version, c.repo, c.scope, c.key, c.content,
	c.provenance, c.source_job, c.first_confirmed_at, c.updated_at
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ?
	AND (c.scope = 'general' OR c.repo = ?)
	AND c.superseded_by IS NULL
ORDER BY bm25(f.confirmed_memories_fts), c.updated_at DESC
LIMIT ?`,
		matchQuery, owner.Kind, owner.Ref, owner.Version, repo, limit)
	if err != nil {
		return nil, fmt.Errorf("query confirmed memories: %w", err)
	}
	defer rows.Close()
	return scanConfirmedMemories(rows)
}

// ListConfirmedMemories returns confirmed rows for the audit CLI, filtered
// optionally by owner ref and repo. A blank ownerRef matches all owners; a blank
// repo matches all repos (both repo-scoped and general).
func (s *Store) ListConfirmedMemories(ctx context.Context, ownerRef, repo string) ([]ConfirmedMemory, error) {
	var where []string
	var args []any
	if strings.TrimSpace(ownerRef) != "" {
		where = append(where, "owner_ref = ?")
		args = append(args, ownerRef)
	}
	if strings.TrimSpace(repo) != "" {
		where = append(where, "(repo = ? OR scope = 'general')")
		args = append(args, repo)
	}
	query := `
SELECT id, owner_kind, owner_ref, owner_version, repo, scope, key, content,
	provenance, source_job, first_confirmed_at, updated_at
FROM confirmed_memories`
	if len(where) > 0 {
		query += "\nWHERE " + strings.Join(where, " AND ")
	}
	query += "\nORDER BY updated_at DESC, id DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list confirmed memories: %w", err)
	}
	defer rows.Close()
	return scanConfirmedMemories(rows)
}

// ListMemoryObservations returns pending observation rows for the audit CLI,
// filtered optionally by owner ref and repo.
func (s *Store) ListMemoryObservations(ctx context.Context, ownerRef, repo string) ([]MemoryObservation, error) {
	var where []string
	var args []any
	if strings.TrimSpace(ownerRef) != "" {
		where = append(where, "owner_ref = ?")
		args = append(args, ownerRef)
	}
	if strings.TrimSpace(repo) != "" {
		where = append(where, "(repo = ? OR scope = 'general')")
		args = append(args, repo)
	}
	query := `
SELECT id, owner_kind, owner_ref, owner_version, repo, scope, key, content,
	provenance, trust_mark, source_job, created_at
FROM memory_observations`
	if len(where) > 0 {
		query += "\nWHERE " + strings.Join(where, " AND ")
	}
	query += "\nORDER BY created_at DESC, id DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list memory observations: %w", err)
	}
	defer rows.Close()
	var out []MemoryObservation
	for rows.Next() {
		var o MemoryObservation
		var repoNull sql.NullString
		if err := rows.Scan(&o.ID, &o.Owner.Kind, &o.Owner.Ref, &o.Owner.Version, &repoNull,
			&o.Scope, &o.Key, &o.Content, &o.Provenance, &o.TrustMark, &o.SourceJob, &o.CreatedAt); err != nil {
			return nil, err
		}
		o.Repo = repoNull.String
		out = append(out, o)
	}
	return out, rows.Err()
}

func scanConfirmedMemories(rows *sql.Rows) ([]ConfirmedMemory, error) {
	var out []ConfirmedMemory
	for rows.Next() {
		var c ConfirmedMemory
		var repoNull sql.NullString
		if err := rows.Scan(&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &repoNull,
			&c.Scope, &c.Key, &c.Content, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Repo = repoNull.String
		out = append(out, c)
	}
	return out, rows.Err()
}
