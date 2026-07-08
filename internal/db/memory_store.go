package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := insertMemoryObservationTx(ctx, tx, obs)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// insertMemoryObservationTx is the transaction body shared by the single-op public
// method and the atomic ApplyVaultImport batch.
func insertMemoryObservationTx(ctx context.Context, tx *sql.Tx, obs MemoryObservation) (int64, error) {
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
	res, err := tx.ExecContext(ctx, `
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

// CountConfirmedMemoriesForOwner returns how many confirmed_memories rows an
// owner owns, across ALL owner versions and every repo/scope (owner_version is
// deliberately not filtered — an agent owner always writes owner_version=”), so
// it answers "how many injectable facts does this agent own". It backs the
// dashboard's per-agent memory count and is a plain read (no FTS). Superseded AND
// retired rows are excluded (superseded_by IS NULL AND retired_at = '') so the
// count stays exactly equal to the injectable set surfaced by
// QueryConfirmedMemories (which applies the same two filters).
func (s *Store) CountConfirmedMemoriesForOwner(ctx context.Context, ownerKind, ownerRef string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM confirmed_memories
WHERE owner_kind = ? AND owner_ref = ? AND superseded_by IS NULL AND retired_at = ''`, ownerKind, ownerRef).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count confirmed memories for owner: %w", err)
	}
	return n, nil
}

// CountMemoryObservationsForOwner returns how many memory_observations rows an
// owner owns, across ALL owner versions and every repo/scope/key. Observations
// are append-only, so this is the raw sighting-report volume for the owner. It
// backs the dashboard's per-agent observation count.
func (s *Store) CountMemoryObservationsForOwner(ctx context.Context, ownerKind, ownerRef string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM memory_observations
WHERE owner_kind = ? AND owner_ref = ?`, ownerKind, ownerRef).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count memory observations for owner: %w", err)
	}
	return n, nil
}

// ListConfirmedMemoriesByOwnerKind returns every confirmed_memories row owned by
// the given owner kind (e.g. memory.OwnerKindAgent), across every repo/scope/owner
// version, and INCLUDING superseded rows — surfaced with SupersededBy populated
// (COALESCEd from the nullable superseded_by column, 0 == not superseded). It is
// the dashboard brain-graph read path: unlike QueryConfirmedMemories (the
// injectable set, which drops superseded rows) and CountConfirmedMemoriesForOwner
// (which counts only injectable rows), the graph deliberately shows superseded
// "ghost" facts so it can draw supersede edges, so this MUST NOT filter them.
// Retired rows (retired_at != '') ARE excluded, though: retirement carries no
// supersede pointer and no edge, so a retired fact drawn as a live node would be a
// phantom — and excluding it keeps the graph's fact set aligned with the injectable
// set (QueryConfirmedMemories / CountConfirmedMemoriesForOwner both filter retired).
// Rows come back ordered by id for a stable, deterministic traversal. Plain read, no FTS.
func (s *Store) ListConfirmedMemoriesByOwnerKind(ctx context.Context, ownerKind string) ([]ConfirmedMemory, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner_kind, owner_ref, owner_version, repo, scope, key, content,
	provenance, source_job, first_confirmed_at, updated_at, COALESCE(superseded_by, 0)
FROM confirmed_memories
WHERE owner_kind = ? AND retired_at = ''
ORDER BY id`, ownerKind)
	if err != nil {
		return nil, fmt.Errorf("list confirmed memories by owner kind: %w", err)
	}
	defer rows.Close()
	var out []ConfirmedMemory
	for rows.Next() {
		var c ConfirmedMemory
		var repoNull sql.NullString
		if err := rows.Scan(&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &repoNull,
			&c.Scope, &c.Key, &c.Content, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt, &c.UpdatedAt, &c.SupersededBy); err != nil {
			return nil, err
		}
		c.Repo = repoNull.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// ObservationKeyWitnesses is a per-(owner_ref, repo, key) tally of how many
// append-only observation sightings back that triple — the "witness" count a
// confirmed fact on the same key accumulated. Repo is normalized ("" == general /
// NULL repo).
type ObservationKeyWitnesses struct {
	OwnerRef string
	Repo     string
	Key      string
	Count    int
}

// CountObservationWitnessesByKey groups memory_observations for the given owner
// kind by (owner_ref, repo, key) and returns each triple's sighting count in ONE
// pass, so the brain graph can attach a witness count to every fact without an N+1
// per-fact query. A NULL repo (general scope) is normalized to "".
func (s *Store) CountObservationWitnessesByKey(ctx context.Context, ownerKind string) ([]ObservationKeyWitnesses, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT owner_ref, repo, key, COUNT(*)
FROM memory_observations
WHERE owner_kind = ?
GROUP BY owner_ref, repo, key`, ownerKind)
	if err != nil {
		return nil, fmt.Errorf("count observation witnesses by key: %w", err)
	}
	defer rows.Close()
	var out []ObservationKeyWitnesses
	for rows.Next() {
		var w ObservationKeyWitnesses
		var repoNull sql.NullString
		if err := rows.Scan(&w.OwnerRef, &repoNull, &w.Key, &w.Count); err != nil {
			return nil, err
		}
		w.Repo = repoNull.String
		out = append(out, w)
	}
	return out, rows.Err()
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
		// A fresh confirmation of an existing key re-activates it: clear any
		// retirement so a previously-retired fact that the confirmation gate re-admits
		// is injectable/exportable again (otherwise the newly confirmed content would
		// stay permanently hidden behind the stale retired_at, since the lookup above
		// matches by key regardless of retirement). Re-confirming an active row leaves
		// the already-empty retirement columns untouched.
		if _, upErr := tx.ExecContext(ctx, `
UPDATE confirmed_memories
SET content = ?, provenance = ?, source_job = ?, updated_at = ?, retired_at = '', retired_reason = ''
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

// UpdateConfirmedMemoryByID applies an owner-curated content edit to ONE confirmed
// row by primary key, guarded by an optimistic-concurrency CAS on updated_at, and
// resyncs the FTS index in the same transaction. It backs `memory vault import`
// (#737 P2): the importer diffs an edited note against a fresh export and, on a
// difference, writes the human's new content back to the exact row it came from —
// never key-based (which could clobber a different row), never touching owner/
// scope/key. expectedUpdatedAt is the updated_at the fresh export observed; if the
// row changed since (or was retired), the CAS matches nothing and the update is
// refused with an id-naming error so the caller can abort and re-export. Writes are
// serialized by the store (MaxOpenConns=1), so this optimistic CAS is sufficient.
func (s *Store) UpdateConfirmedMemoryByID(ctx context.Context, id int64, expectedUpdatedAt, content, provenance string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := updateConfirmedMemoryByIDTx(ctx, tx, id, expectedUpdatedAt, content, provenance); err != nil {
		return err
	}
	return tx.Commit()
}

// updateConfirmedMemoryByIDTx is the transaction body shared by the single-op
// public method and the atomic ApplyVaultImport batch.
func updateConfirmedMemoryByIDTx(ctx context.Context, tx *sql.Tx, id int64, expectedUpdatedAt, content, provenance string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("confirmed memory %d requires content", id)
	}
	now := nowRFC3339()
	res, err := tx.ExecContext(ctx, `
UPDATE confirmed_memories
SET content = ?, provenance = ?, updated_at = ?
WHERE id = ? AND updated_at = ? AND retired_at = ''`,
		content, provenance, now, id, expectedUpdatedAt)
	if err != nil {
		return fmt.Errorf("update confirmed memory by id: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// Disambiguate a missing row from a lost CAS race so the operator gets an
		// actionable message naming the id.
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM confirmed_memories WHERE id = ?`, id).Scan(&count); err != nil {
			return fmt.Errorf("inspect confirmed memory %d: %w", id, err)
		}
		if count == 0 {
			return fmt.Errorf("confirmed memory %d not found", id)
		}
		return fmt.Errorf("confirmed memory %d changed since export (expected updated_at %q); re-export and retry", id, expectedUpdatedAt)
	}

	// Keep the FTS index in sync with the new content inside the same transaction.
	var key string
	if err := tx.QueryRowContext(ctx, `SELECT key FROM confirmed_memories WHERE id = ?`, id).Scan(&key); err != nil {
		return fmt.Errorf("read confirmed memory key %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM confirmed_memories_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("sync confirmed memory fts (delete): %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO confirmed_memories_fts(rowid, content, key) VALUES (?, ?, ?)`, id, content, key); err != nil {
		return fmt.Errorf("sync confirmed memory fts (insert): %w", err)
	}
	return nil
}

// RetireConfirmedMemory marks ONE confirmed row retired (the owner deleted its note
// from an exported vault) and removes it from the FTS index in the same transaction
// so it stops being injected into prompts and stops appearing in future exports. It
// is additive and NON-destructive: the row is preserved for audit with retired_at/
// retired_reason set; it deliberately does NOT write superseded_by (which carries
// distinct replacement semantics and has no writers today — see #737 P2). Backs
// `memory vault import` deletions ⇒ retirements. A row that does not exist (or is
// already retired) yields an id-naming error rather than a silent no-op.
func (s *Store) RetireConfirmedMemory(ctx context.Context, id int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := retireConfirmedMemoryTx(ctx, tx, id, "", reason); err != nil {
		return err
	}
	return tx.Commit()
}

// retireConfirmedMemoryTx is the transaction body shared by the single-op public
// method and the atomic ApplyVaultImport batch. expectedUpdatedAt is an optional
// optimistic-concurrency guard mirroring the edit CAS: when non-empty, the row is
// retired only if its updated_at still matches what the fresh export observed, so a
// concurrent write landing in the plan→apply window (e.g. the daemon confirming a
// newer fact on the same row) makes the retirement match nothing and roll the whole
// import batch back rather than burying the newer fact. An empty string means "no
// version guard" (the single-op public path), preserving its prior behavior.
func retireConfirmedMemoryTx(ctx context.Context, tx *sql.Tx, id int64, expectedUpdatedAt, reason string) error {
	now := nowRFC3339()
	query := `
UPDATE confirmed_memories
SET retired_at = ?, retired_reason = ?
WHERE id = ? AND retired_at = ''`
	args := []any{now, reason, id}
	if expectedUpdatedAt != "" {
		query += ` AND updated_at = ?`
		args = append(args, expectedUpdatedAt)
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("retire confirmed memory: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM confirmed_memories WHERE id = ?`, id).Scan(&count); err != nil {
			return fmt.Errorf("inspect confirmed memory %d: %w", id, err)
		}
		if count == 0 {
			return fmt.Errorf("confirmed memory %d not found", id)
		}
		// Distinguish an already-retired row from a lost CAS race (the row is still
		// active but changed since export) so the operator gets an actionable message.
		if expectedUpdatedAt != "" {
			var retiredAt string
			if err := tx.QueryRowContext(ctx, `SELECT retired_at FROM confirmed_memories WHERE id = ?`, id).Scan(&retiredAt); err != nil {
				return fmt.Errorf("inspect confirmed memory %d: %w", id, err)
			}
			if retiredAt == "" {
				return fmt.Errorf("confirmed memory %d changed since export (expected updated_at %q); re-export and retry", id, expectedUpdatedAt)
			}
		}
		return fmt.Errorf("confirmed memory %d already retired", id)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM confirmed_memories_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("sync confirmed memory fts (delete): %w", err)
	}
	return nil
}

// VaultImportUpdate is one owner-edited note applied to its source row (CAS on
// ExpectedUpdatedAt).
type VaultImportUpdate struct {
	ID                int64
	ExpectedUpdatedAt string
	Content           string
	Provenance        string
}

// VaultImportRetire is one confirmed row the owner deleted from the vault.
// ExpectedUpdatedAt is the updated_at the fresh export observed; it is an optimistic
// CAS guard (empty == none) so a retirement never lands on top of a fact that was
// rewritten in the plan→apply window, mirroring VaultImportUpdate.
type VaultImportRetire struct {
	ID                int64
	ExpectedUpdatedAt string
	Reason            string
}

// VaultImportPlan is the full set of mutations `memory vault import --yes` applies
// ATOMICALLY: content edits (CAS), retirements, and new owner-authored notes staged
// as pending observations. All-or-nothing — any failure rolls the whole batch back.
type VaultImportPlan struct {
	Updates      []VaultImportUpdate
	Retirements  []VaultImportRetire
	Observations []MemoryObservation
}

// ApplyVaultImport applies a whole import plan in ONE transaction so a partial
// curation can never land: edits, retirements, and new-note observations either all
// commit or none do. Edits and retirements resync/clear the FTS index in the same
// transaction (via the shared tx helpers); observations are append-only.
func (s *Store) ApplyVaultImport(ctx context.Context, plan VaultImportPlan) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, u := range plan.Updates {
		if err := updateConfirmedMemoryByIDTx(ctx, tx, u.ID, u.ExpectedUpdatedAt, u.Content, u.Provenance); err != nil {
			return err
		}
	}
	for _, r := range plan.Retirements {
		if err := retireConfirmedMemoryTx(ctx, tx, r.ID, r.ExpectedUpdatedAt, r.Reason); err != nil {
			return err
		}
	}
	for _, obs := range plan.Observations {
		if _, err := insertMemoryObservationTx(ctx, tx, obs); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	AND c.retired_at = ''
ORDER BY bm25(f.confirmed_memories_fts), c.updated_at DESC
LIMIT ?`,
		matchQuery, owner.Kind, owner.Ref, owner.Version, repo, limit)
	if err != nil {
		return nil, fmt.Errorf("query confirmed memories: %w", err)
	}
	defer rows.Close()
	return scanConfirmedMemories(rows)
}

// ListConfirmedMemoriesForVault returns every NON-superseded confirmed row for
// the deterministic `memory vault export` (#737 P1), across all owner kinds,
// repos, and scopes, ordered by id for a stable traversal. superseded_by is
// COALESCEd (0 == not superseded) but rows carrying a supersede pointer are
// filtered out here — the vault is a view of the injectable/current set. A
// non-empty agentRef narrows the export to a single agent owner (owner_kind
// 'agent', owner_ref = agentRef). Plain read, no FTS, zero writes.
func (s *Store) ListConfirmedMemoriesForVault(ctx context.Context, agentRef string) ([]ConfirmedMemory, error) {
	query := `
SELECT id, owner_kind, owner_ref, owner_version, repo, scope, key, content,
	provenance, source_job, first_confirmed_at, updated_at, COALESCE(superseded_by, 0)
FROM confirmed_memories
WHERE superseded_by IS NULL AND retired_at = ''`
	var args []any
	if strings.TrimSpace(agentRef) != "" {
		query += "\n\tAND owner_kind = 'agent' AND owner_ref = ?"
		args = append(args, agentRef)
	}
	query += "\nORDER BY id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list confirmed memories for vault: %w", err)
	}
	defer rows.Close()
	var out []ConfirmedMemory
	for rows.Next() {
		var c ConfirmedMemory
		var repoNull sql.NullString
		if err := rows.Scan(&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &repoNull,
			&c.Scope, &c.Key, &c.Content, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt, &c.UpdatedAt, &c.SupersededBy); err != nil {
			return nil, err
		}
		c.Repo = repoNull.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// QueryConfirmedMemoryVaultLinks is the VAULT-LOCAL co-occurrence link helper for
// `memory vault export` (#737 P1). It runs the SAME owner/repo visibility policy
// and superseded filter as QueryConfirmedMemories, but orders bm25 THEN id
// ascending — a deterministic tie-break the injection path deliberately lacks
// (its updated_at-DESC secondary sort is fine for a prompt but would make the
// vault non-reproducible under bm25 ties). It exists ONLY to serve the
// deterministic export; do NOT route job-prompt injection through it. The caller
// excludes self and caps at K. matchQuery MUST be a sanitized MATCH string
// (memory.SanitizeFTSQuery) — never raw text.
func (s *Store) QueryConfirmedMemoryVaultLinks(ctx context.Context, owner MemoryOwner, repo, matchQuery string, limit int) ([]ConfirmedMemory, error) {
	if strings.TrimSpace(matchQuery) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 6
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
ORDER BY bm25(f.confirmed_memories_fts), c.id
LIMIT ?`,
		matchQuery, owner.Kind, owner.Ref, owner.Version, repo, limit)
	if err != nil {
		return nil, fmt.Errorf("query confirmed memory vault links: %w", err)
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

// MemoryDistillWitnessProvenancePrefix marks a recurrence-WITNESS observation row
// written by distill-at-terminal (#737 P4.1). Witness rows are internal recurrence
// bookkeeping — NOT human-reviewable observations — so the pending list surface
// (ListMemoryObservations / ListMemoryObservationsWithConfirmation) and the confirm
// getter (GetMemoryObservationByID) exclude this provenance. A one-off failure's
// sentinel witness is therefore never shown in `memory list` and can never be
// promoted by `memory confirm`. The prefix is carried unchanged into
// memory_observations, so this is a read-time filter with no schema change. The
// exclusion is a no-op when distill is off (no witness rows exist).
const MemoryDistillWitnessProvenancePrefix = "distill-seen:"

// ListMemoryObservations returns pending observation rows for the audit CLI,
// filtered optionally by owner ref and repo. Recurrence witnesses (see
// MemoryDistillWitnessProvenancePrefix) are always excluded — they are internal
// bookkeeping, never a human-reviewable pending observation.
func (s *Store) ListMemoryObservations(ctx context.Context, ownerRef, repo string) ([]MemoryObservation, error) {
	where := []string{"provenance NOT LIKE ? ESCAPE '\\'"}
	args := []any{likePrefix(MemoryDistillWitnessProvenancePrefix)}
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

// ---- #737 P3 (markdown ingest + human-gated confirm) ----------------------
// The functions below back `gitmoot memory ingest`, `memory observations`, and
// `memory confirm`. They are additive reads plus the existing observation-insert
// and confirmed-upsert paths — no schema changes, no new mutation of prior rows.

// ObservationWithConfirmation is one pending observation annotated with whether
// an owner+repo+key match already exists in confirmed_memories. It backs
// `memory observations`, which flags which ingested notes have crossed the human
// confirm gate.
type ObservationWithConfirmation struct {
	MemoryObservation
	Confirmed bool
}

// ListMemoryObservationsWithConfirmation returns pending observations (optionally
// narrowed to an owner ref and/or a provenance prefix), each flagged with whether
// a confirmed row already exists for its owner+repo+key. It is a read-only join
// used by the audit surface; it mutates nothing. A blank ownerRef matches all
// owners and a blank provenancePrefix matches all provenances.
func (s *Store) ListMemoryObservationsWithConfirmation(ctx context.Context, ownerRef, provenancePrefix string) ([]ObservationWithConfirmation, error) {
	// Recurrence witnesses are never human-reviewable, so they are excluded even
	// when the caller passes a provenancePrefix — `--prefix distill-seen:` must
	// select nothing rather than expose the sentinel witnesses for confirmation.
	where := []string{"o.provenance NOT LIKE ? ESCAPE '\\'"}
	args := []any{likePrefix(MemoryDistillWitnessProvenancePrefix)}
	if strings.TrimSpace(ownerRef) != "" {
		where = append(where, "o.owner_ref = ?")
		args = append(args, ownerRef)
	}
	if strings.TrimSpace(provenancePrefix) != "" {
		where = append(where, "o.provenance LIKE ? ESCAPE '\\'")
		args = append(args, likePrefix(provenancePrefix))
	}
	query := `
SELECT o.id, o.owner_kind, o.owner_ref, o.owner_version, o.repo, o.scope, o.key,
	o.content, o.provenance, o.trust_mark, o.source_job, o.created_at,
	EXISTS(
		SELECT 1 FROM confirmed_memories c
		WHERE c.owner_kind = o.owner_kind AND c.owner_ref = o.owner_ref
			AND c.owner_version = o.owner_version
			AND ((o.repo IS NULL AND c.repo IS NULL) OR c.repo = o.repo)
			AND c.key = o.key
	) AS confirmed
FROM memory_observations o`
	if len(where) > 0 {
		query += "\nWHERE " + strings.Join(where, " AND ")
	}
	query += "\nORDER BY o.created_at DESC, o.id DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list observations with confirmation: %w", err)
	}
	defer rows.Close()
	var out []ObservationWithConfirmation
	for rows.Next() {
		var o ObservationWithConfirmation
		var repoNull sql.NullString
		var confirmed int
		if err := rows.Scan(&o.ID, &o.Owner.Kind, &o.Owner.Ref, &o.Owner.Version, &repoNull,
			&o.Scope, &o.Key, &o.Content, &o.Provenance, &o.TrustMark, &o.SourceJob, &o.CreatedAt, &confirmed); err != nil {
			return nil, err
		}
		o.Repo = repoNull.String
		o.Confirmed = confirmed != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetMemoryObservationByID returns a single pending observation by its row id,
// or (zero, false, nil) when no such row exists. It backs `memory confirm <id>`.
func (s *Store) GetMemoryObservationByID(ctx context.Context, id int64) (MemoryObservation, bool, error) {
	var o MemoryObservation
	var repoNull sql.NullString
	// A recurrence witness is not a confirmable observation: exclude it here so
	// `memory confirm <witness-id>` resolves to "no observation with id N" rather
	// than promoting the fixed sentinel string into confirmed memory.
	err := s.db.QueryRowContext(ctx, `
SELECT id, owner_kind, owner_ref, owner_version, repo, scope, key, content,
	provenance, trust_mark, source_job, created_at
FROM memory_observations WHERE id = ? AND provenance NOT LIKE ? ESCAPE '\'`,
		id, likePrefix(MemoryDistillWitnessProvenancePrefix)).Scan(
		&o.ID, &o.Owner.Kind, &o.Owner.Ref, &o.Owner.Version, &repoNull,
		&o.Scope, &o.Key, &o.Content, &o.Provenance, &o.TrustMark, &o.SourceJob, &o.CreatedAt)
	if err == sql.ErrNoRows {
		return MemoryObservation{}, false, nil
	}
	if err != nil {
		return MemoryObservation{}, false, fmt.Errorf("get memory observation %d: %w", id, err)
	}
	o.Repo = repoNull.String
	return o, true, nil
}

// MemoryDedupKey combines the visibility domain (scope + nullable repo) with a
// content hash so ingest dedup only collapses rows that would inject into the
// SAME domain. Two rows with identical text but different repos are NOT
// duplicates: repo-scoped confirmed memory injects only for its own repo (see
// QueryConfirmedMemories: scope='general' OR repo=?), so a second repo must be
// allowed to stage — and later inject — the same note. Repo is trimmed so a NULL
// repo and an empty-string repo collapse to the same general-scope domain.
func MemoryDedupKey(scope, repo, contentHash string) string {
	return strings.TrimSpace(scope) + "\x00" + strings.TrimSpace(repo) + "\x00" + contentHash
}

// ObservationDedupKeys returns the set of (scope, repo, content-hash) dedup keys
// across BOTH pending observations and confirmed rows for an owner ref. Ingest
// uses it to drop an incoming chunk only when the SAME content already exists in
// the SAME visibility domain, so identical text under a second repo still stages.
// A blank ownerRef scans every owner. Build the lookup key with MemoryDedupKey.
func (s *Store) ObservationDedupKeys(ctx context.Context, ownerRef string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	collect := func(query string) error {
		var rows *sql.Rows
		var err error
		if strings.TrimSpace(ownerRef) != "" {
			rows, err = s.db.QueryContext(ctx, query+" WHERE owner_ref = ?", ownerRef)
		} else {
			rows, err = s.db.QueryContext(ctx, query)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var content, scope string
			var repoNull sql.NullString
			if err := rows.Scan(&content, &repoNull, &scope); err != nil {
				return err
			}
			out[MemoryDedupKey(scope, repoNull.String, sha256HexOf(content))] = struct{}{}
		}
		return rows.Err()
	}
	if err := collect(`SELECT content, repo, scope FROM memory_observations`); err != nil {
		return nil, fmt.Errorf("scan observation contents: %w", err)
	}
	if err := collect(`SELECT content, repo, scope FROM confirmed_memories`); err != nil {
		return nil, fmt.Errorf("scan confirmed contents: %w", err)
	}
	return out, nil
}

// sha256HexOf mirrors memory.ContentHash without importing the memory package
// into the db layer (the store carries no dependency on internal/memory).
func sha256HexOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// likePrefix builds a LIKE pattern that matches a literal prefix followed by
// anything, escaping the LIKE metacharacters (%, _, and the \ escape char) in
// the prefix so a provenance like "ingest:%" cannot smuggle wildcards.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(prefix) + "%"
}
