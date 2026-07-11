package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
	AuthorRef  string // "" == author is Owner.Ref; set when a shared row preserves the writer
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
	AuthorRef        string // "" == author is Owner.Ref; set when owner changes to the shared pool
	Repo             string // "" == general scope (stored as SQL NULL)
	Scope            string
	Key              string
	Context          string // optional subject inherited from a groom-split parent
	Content          string
	Provenance       string
	SourceJob        string
	FirstConfirmedAt string
	UpdatedAt        string
	SupersededBy     int64 // 0 == not superseded
}

// MemoryLink is one persisted, derived edge from a source confirmed memory to a
// related target confirmed memory. It never mutates fact content; vault export and
// inspection commands opt into this side table explicitly.
type MemoryLink struct {
	SrcID     int64
	DstID     int64
	DstKey    string
	Score     float64
	Origin    string
	CreatedAt string
}

// LinkedConfirmedMemory is one active memory_links edge plus the visible target
// confirmed memory row. It backs retrieval expansion: direct FTS hits stay first,
// then callers append these linked target facts in link-rank order.
type LinkedConfirmedMemory struct {
	SrcID  int64
	Score  float64
	Memory ConfirmedMemory
}

// MemoryLinkEnrichment reports what the auto-link pass created or skipped for one
// source memory. Created contains rows that were written, or rows that WOULD be
// written when the caller requested a dry run.
type MemoryLinkEnrichment struct {
	SrcID           int64
	Created         []MemoryLink
	SkippedExisting int
	SkippedWeak     int
}

const (
	memoryOwnerKindAgent  = "agent"
	memoryOwnerKindShared = "shared"
	memorySharedOwnerRef  = "shared"

	memoryAutoLinkK = 3
	// memoryAutoLinkMinScore is a tiny absolute floor guarding degenerate
	// near-zero bm25 matches. The REAL weak-link guard is relative:
	// memoryAutoLinkMinRelative drops any candidate scoring below that
	// fraction of the best candidate for the same source fact, which stays
	// meaningful as the corpus grows (absolute bm25 magnitudes are
	// corpus-dependent; a live probe on 95 facts showed scores 1.25-45.6,
	// so any fixed absolute cutoff is either dead code or brittle).
	memoryAutoLinkMinScore    = 0.000002
	memoryAutoLinkMinRelative = 0.30
)

// ErrConfirmedMemoryRetired is returned when a keyed confirmed-memory upsert
// matches a retired row and the caller did not explicitly opt into resurrection.
// Collector and automatic ingestion paths count this as skipped-retired rather
// than reviving content an operator already pulled from circulation.
var ErrConfirmedMemoryRetired = errors.New("confirmed memory is retired")

type upsertConfirmedMemoryOptions struct {
	allowResurrect   bool
	preserveEditions bool
}

// UpsertConfirmedMemoryOption tunes confirmed-memory upsert behavior.
type UpsertConfirmedMemoryOption func(*upsertConfirmedMemoryOptions)

// AllowResurrectConfirmedMemory lets a deliberate human-controlled path revive a
// retired keyed row. Automated collectors must not pass this option.
func AllowResurrectConfirmedMemory() UpsertConfirmedMemoryOption {
	return func(o *upsertConfirmedMemoryOptions) {
		o.allowResurrect = true
	}
}

// PreserveSupersededEdition makes a key-matched in-place UPDATE archive the prior
// edition first (#804): the old content is copied to a new row whose
// superseded_by points at the live row, then the live row is overwritten.
// AUTO-CONFIRMED writers (ingest auto-confirm, chat remember) pass this so a bad
// or poisoned edit can never silently destroy the last human-reviewed edition —
// the archived row stays inspectable by groom, the vault ghosts, and the brain
// graph. Manual human-controlled paths (vault import CAS edits, `memory confirm
// --yes`) keep their existing overwrite semantics and do NOT pass this option.
func PreserveSupersededEdition() UpsertConfirmedMemoryOption {
	return func(o *upsertConfirmedMemoryOptions) {
		o.preserveEditions = true
	}
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
	(owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, provenance, trust_mark, source_job, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obs.Owner.Kind, obs.Owner.Ref, obs.Owner.Version, strings.TrimSpace(obs.AuthorRef), nullableRepo(obs.Repo), scope,
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

// CountConfirmedMemoriesForOwner returns how many active confirmed rows an
// owner owns or, for agent owners, authored after a row moved to the shared pool.
// It scans across ALL owner versions and every repo/scope (owner_version is
// deliberately not filtered — an agent owner always writes owner_version=”), so
// it answers "how many injectable facts belong to this agent". It backs the
// dashboard's per-agent memory count and is a plain read (no FTS). Superseded AND
// retired rows are excluded (superseded_by IS NULL AND retired_at = ”) so the
// count stays exactly equal to the injectable set surfaced by
// QueryConfirmedMemories (which applies the same two filters).
func (s *Store) CountConfirmedMemoriesForOwner(ctx context.Context, ownerKind, ownerRef string) (int, error) {
	var n int
	query := `
SELECT COUNT(*) FROM confirmed_memories
WHERE owner_kind = ? AND owner_ref = ? AND superseded_by IS NULL AND retired_at = ''`
	args := []any{ownerKind, ownerRef}
	if ownerKind == memoryOwnerKindAgent {
		query = `
SELECT COUNT(*) FROM confirmed_memories
WHERE ((owner_kind = ? AND owner_ref = ?) OR (owner_kind = ? AND owner_ref = ? AND author_ref = ?))
	AND superseded_by IS NULL AND retired_at = ''`
		args = []any{memoryOwnerKindAgent, ownerRef, memoryOwnerKindShared, memorySharedOwnerRef, ownerRef}
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count confirmed memories for owner: %w", err)
	}
	return n, nil
}

// CountMemoryObservationsForOwner returns how many memory_observations rows an
// owner owns or, for agent owners, authored after a row staged directly in the
// shared pool, across ALL owner versions and every repo/scope/key. Observations
// are append-only, so this is the raw sighting-report volume for the owner. It
// backs the dashboard's per-agent observation count.
func (s *Store) CountMemoryObservationsForOwner(ctx context.Context, ownerKind, ownerRef string) (int, error) {
	var n int
	query := `
SELECT COUNT(*) FROM memory_observations
WHERE owner_kind = ? AND owner_ref = ?`
	args := []any{ownerKind, ownerRef}
	if ownerKind == memoryOwnerKindAgent {
		query = `
SELECT COUNT(*) FROM memory_observations
WHERE (owner_kind = ? AND owner_ref = ?) OR (owner_kind = ? AND owner_ref = ? AND author_ref = ?)`
		args = []any{memoryOwnerKindAgent, ownerRef, memoryOwnerKindShared, memorySharedOwnerRef, ownerRef}
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
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
// Retired rows (retired_at != ”) ARE excluded, though: retirement carries no
// supersede pointer and no edge, so a retired fact drawn as a live node would be a
// phantom — and excluding it keeps the graph's fact set aligned with the injectable
// set (QueryConfirmedMemories / CountConfirmedMemoriesForOwner both filter retired).
// Rows come back ordered by id for a stable, deterministic traversal. Plain read, no FTS.
func (s *Store) ListConfirmedMemoriesByOwnerKind(ctx context.Context, ownerKind string) ([]ConfirmedMemory, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
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
		if err := rows.Scan(&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &c.AuthorRef, &repoNull,
			&c.Scope, &c.Key, &c.Content, &c.Context, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt, &c.UpdatedAt, &c.SupersededBy); err != nil {
			return nil, err
		}
		c.Repo = repoNull.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListConfirmedMemoriesForKnowledge returns every non-retired fact the dashboard
// Knowledge graph should render: private agent facts plus the reserved shared
// pool. Superseded rows stay included as graph ghosts, matching
// ListConfirmedMemoriesByOwnerKind.
func (s *Store) ListConfirmedMemoriesForKnowledge(ctx context.Context) ([]ConfirmedMemory, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
	provenance, source_job, first_confirmed_at, updated_at, COALESCE(superseded_by, 0)
FROM confirmed_memories
WHERE (owner_kind = 'agent' OR (owner_kind = 'shared' AND owner_ref = 'shared'))
	AND retired_at = ''
ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list confirmed memories for knowledge: %w", err)
	}
	defer rows.Close()
	var out []ConfirmedMemory
	for rows.Next() {
		var c ConfirmedMemory
		var repoNull sql.NullString
		if err := rows.Scan(&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &c.AuthorRef, &repoNull,
			&c.Scope, &c.Key, &c.Content, &c.Context, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt, &c.UpdatedAt, &c.SupersededBy); err != nil {
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
	where := "owner_kind = ?"
	args := []any{ownerKind}
	if ownerKind == memoryOwnerKindAgent {
		where = "(owner_kind = ? OR owner_kind = ?)"
		args = []any{memoryOwnerKindAgent, memoryOwnerKindShared}
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT CASE WHEN author_ref <> '' THEN author_ref ELSE owner_ref END AS witness_owner_ref,
	repo, key, COUNT(*)
FROM memory_observations
WHERE `+where+`
GROUP BY witness_owner_ref, repo, key`, args...)
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
func (s *Store) UpsertConfirmedMemory(ctx context.Context, cm ConfirmedMemory, opts ...UpsertConfirmedMemoryOption) (int64, error) {
	if strings.TrimSpace(cm.Owner.Kind) == "" || strings.TrimSpace(cm.Owner.Ref) == "" {
		return 0, fmt.Errorf("confirmed memory requires an owner kind and ref")
	}
	if strings.TrimSpace(cm.Key) == "" || strings.TrimSpace(cm.Content) == "" {
		return 0, fmt.Errorf("confirmed memory requires a key and content")
	}
	var options upsertConfirmedMemoryOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
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

	// Superseded archival editions never key-match. Since the #804 partial unique
	// indexes cover only active rows, several retired rows may share a key: prefer
	// the active row, then the newest retired one, so the match (and an explicit
	// resurrection) stays deterministic.
	var id int64
	var retiredAt string
	err = tx.QueryRowContext(ctx, `
SELECT id, retired_at FROM confirmed_memories
WHERE owner_kind = ? AND owner_ref = ? AND owner_version = ?
	AND ((? IS NULL AND repo IS NULL) OR repo = ?)
	AND key = ?
	AND superseded_by IS NULL
ORDER BY CASE WHEN retired_at = '' THEN 0 ELSE 1 END, id DESC
LIMIT 1`,
		cm.Owner.Kind, cm.Owner.Ref, cm.Owner.Version, nullableRepo(cm.Repo), nullableRepo(cm.Repo), cm.Key).Scan(&id, &retiredAt)
	switch {
	case err == sql.ErrNoRows:
		res, insErr := tx.ExecContext(ctx, `
INSERT INTO confirmed_memories
	(owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context, provenance, source_job, first_confirmed_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cm.Owner.Kind, cm.Owner.Ref, cm.Owner.Version, strings.TrimSpace(cm.AuthorRef), nullableRepo(cm.Repo), scope,
			cm.Key, cm.Content, strings.TrimSpace(cm.Context), cm.Provenance, cm.SourceJob, now, now)
		if insErr != nil {
			return 0, fmt.Errorf("insert confirmed memory: %w", insErr)
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, fmt.Errorf("lookup confirmed memory: %w", err)
	default:
		if retiredAt != "" && !options.allowResurrect {
			return 0, fmt.Errorf("%w: confirmed memory %d key %q", ErrConfirmedMemoryRetired, id, cm.Key)
		}
		if options.preserveEditions {
			if err := archiveSupersededEditionTx(ctx, tx, id, cm.Content); err != nil {
				return 0, err
			}
		}
		// Only explicit human-controlled paths pass AllowResurrectConfirmedMemory.
		// When they do, a fresh confirmation of an existing key re-activates it by
		// clearing retired_at/retired_reason. Automated collectors use the default
		// refusal above so re-ingest cannot undo a bulk-retire cleanup.
		if _, upErr := tx.ExecContext(ctx, `
UPDATE confirmed_memories
SET author_ref = ?, content = ?, context = ?, provenance = ?, source_job = ?, updated_at = ?, retired_at = '', retired_reason = ''
WHERE id = ?`,
			strings.TrimSpace(cm.AuthorRef), cm.Content, strings.TrimSpace(cm.Context), cm.Provenance, cm.SourceJob, now, id); upErr != nil {
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
	if _, err := enrichConfirmedMemoryLinksTx(ctx, tx, id, false); err != nil {
		return 0, fmt.Errorf("auto-link confirmed memory: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// archiveSupersededEditionTx copies confirmed row id's CURRENT edition to a new
// row carrying superseded_by = id, so the history of an auto-confirmed in-place
// update survives for groom, the brain graph, and audit (#804). The archival row
// escapes the active-row partial unique indexes (superseded_by is set), is never
// added to FTS, and carries no memory_links — links stay keyed on the live row
// id, which does not change. A byte-identical update archives nothing: there is
// no history to preserve.
func archiveSupersededEditionTx(ctx context.Context, tx *sql.Tx, id int64, newContent string) error {
	var prev ConfirmedMemory
	var repoNull sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
	provenance, source_job, first_confirmed_at, updated_at
FROM confirmed_memories WHERE id = ?`, id).Scan(
		&prev.Owner.Kind, &prev.Owner.Ref, &prev.Owner.Version, &prev.AuthorRef, &repoNull,
		&prev.Scope, &prev.Key, &prev.Content, &prev.Context, &prev.Provenance, &prev.SourceJob,
		&prev.FirstConfirmedAt, &prev.UpdatedAt)
	if err != nil {
		return fmt.Errorf("read confirmed memory %d for supersede archive: %w", id, err)
	}
	if prev.Content == newContent {
		return nil
	}
	prev.Repo = repoNull.String
	if _, err := tx.ExecContext(ctx, `
INSERT INTO confirmed_memories
	(owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context, provenance, source_job, first_confirmed_at, updated_at, superseded_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		prev.Owner.Kind, prev.Owner.Ref, prev.Owner.Version, prev.AuthorRef, nullableRepo(prev.Repo), prev.Scope,
		prev.Key, prev.Content, prev.Context, prev.Provenance, prev.SourceJob, prev.FirstConfirmedAt, prev.UpdatedAt, id); err != nil {
		return fmt.Errorf("archive superseded edition of confirmed memory %d: %w", id, err)
	}
	return nil
}

// PromoteConfirmedMemoriesToShared moves active confirmed rows into the reserved
// shared pool without changing their primary keys, content, keys, or FTS rows.
// Existing memory_links survive because they key on confirmed_memories.id. When
// a row has no author_ref, the previous owner_ref is recorded as its author
// before owner_kind/owner_ref change to shared/shared. Retired or superseded rows
// are refused so stale facts cannot be widened to every agent.
func (s *Store) PromoteConfirmedMemoriesToShared(ctx context.Context, ids []int64) ([]ConfirmedMemory, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("at least one confirmed memory id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := nowRFC3339()
	out := make([]ConfirmedMemory, 0, len(ids))
	seen := map[int64]struct{}{}
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("invalid confirmed memory id %d", id)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		c, err := promoteConfirmedMemoryToSharedTx(ctx, tx, id, now)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// promoteConfirmedMemoryToSharedTx is the per-row transaction body shared by
// PromoteConfirmedMemoriesToShared and the groom cross-pool apply. It moves ONE
// active confirmed row into the reserved shared pool (a no-op returning the row
// unchanged when it is already shared), preserving the author. Retired or
// superseded rows are refused so stale facts cannot be widened to every agent.
func promoteConfirmedMemoryToSharedTx(ctx context.Context, tx *sql.Tx, id int64, now string) (ConfirmedMemory, error) {
	var c ConfirmedMemory
	var repoNull sql.NullString
	var retiredAt string
	err := tx.QueryRowContext(ctx, `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
	provenance, source_job, first_confirmed_at, updated_at, COALESCE(superseded_by, 0), retired_at
FROM confirmed_memories
WHERE id = ?`, id).Scan(
		&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &c.AuthorRef, &repoNull,
		&c.Scope, &c.Key, &c.Content, &c.Context, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt,
		&c.UpdatedAt, &c.SupersededBy, &retiredAt)
	if err == sql.ErrNoRows {
		return ConfirmedMemory{}, fmt.Errorf("confirmed memory %d not found", id)
	}
	if err != nil {
		return ConfirmedMemory{}, fmt.Errorf("read confirmed memory %d: %w", id, err)
	}
	c.Repo = repoNull.String
	if c.SupersededBy != 0 {
		return ConfirmedMemory{}, fmt.Errorf("confirmed memory %d is superseded", id)
	}
	if retiredAt != "" {
		return ConfirmedMemory{}, fmt.Errorf("confirmed memory %d is retired", id)
	}

	if c.Owner.Kind == memoryOwnerKindShared && c.Owner.Ref == memorySharedOwnerRef {
		return c, nil
	}
	author := strings.TrimSpace(c.AuthorRef)
	if author == "" {
		author = c.Owner.Ref
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE confirmed_memories
SET owner_kind = ?, owner_ref = ?, owner_version = '', author_ref = ?, updated_at = ?
WHERE id = ?`,
		memoryOwnerKindShared, memorySharedOwnerRef, author, now, id); err != nil {
		return ConfirmedMemory{}, fmt.Errorf("promote confirmed memory %d to shared: %w", id, err)
	}
	c.Owner = MemoryOwner{Kind: memoryOwnerKindShared, Ref: memorySharedOwnerRef}
	c.AuthorRef = author
	c.UpdatedAt = now
	return c, nil
}

// EnrichConfirmedMemoryLinks computes the deterministic top-K similarity links
// for one active confirmed memory and inserts any missing edges. When dryRun is
// true it returns the rows that would be inserted without writing anything.
func (s *Store) EnrichConfirmedMemoryLinks(ctx context.Context, srcID int64, dryRun bool) (MemoryLinkEnrichment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryLinkEnrichment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := enrichConfirmedMemoryLinksTx(ctx, tx, srcID, dryRun)
	if err != nil {
		return MemoryLinkEnrichment{}, err
	}
	if dryRun {
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return MemoryLinkEnrichment{}, err
	}
	return result, nil
}

// ListMemoryLinks returns active target links for one source memory, ordered by
// target id for deterministic CLI and vault rendering. Retired/superseded targets
// are hidden so stale side-table rows never reappear in derived views.
func (s *Store) ListMemoryLinks(ctx context.Context, srcID int64) ([]MemoryLink, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ml.src_id, ml.dst_id, d.key, ml.score, ml.origin, ml.created_at
FROM memory_links ml
JOIN confirmed_memories d ON d.id = ml.dst_id
WHERE ml.src_id = ?
	AND d.superseded_by IS NULL
	AND d.retired_at = ''
ORDER BY ml.dst_id`, srcID)
	if err != nil {
		return nil, fmt.Errorf("list memory links: %w", err)
	}
	defer rows.Close()
	var out []MemoryLink
	for rows.Next() {
		var l MemoryLink
		if err := rows.Scan(&l.SrcID, &l.DstID, &l.DstKey, &l.Score, &l.Origin, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListMemoryLinksAmong returns every persisted link whose source and target are
// both in ids. It deliberately applies no memory visibility or active-row
// policy: callers supply an already-filtered fact set, and this query only
// intersects the side-table edges against that set.
func (s *Store) ListMemoryLinksAmong(ctx context.Context, ids []int64) ([]MemoryLink, error) {
	ids = uniquePositiveInt64s(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	values := strings.TrimSuffix(strings.Repeat("(?),", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
WITH selected(id) AS (VALUES `+values+`)
SELECT ml.src_id, ml.dst_id, ml.score, ml.origin, ml.created_at
FROM memory_links ml
JOIN selected src ON src.id = ml.src_id
JOIN selected dst ON dst.id = ml.dst_id
ORDER BY ml.src_id, ml.dst_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list memory links among facts: %w", err)
	}
	defer rows.Close()
	var out []MemoryLink
	for rows.Next() {
		var l MemoryLink
		if err := rows.Scan(&l.SrcID, &l.DstID, &l.Score, &l.Origin, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListMemoryLinksForSources returns active linked target facts for a batch of
// source ids. It applies only the universal active-row filter; callers that need
// owner/repo visibility should use one of the visibility-specific variants
// below. Rows are ordered by link score descending, target id ascending, then
// source id ascending for deterministic expansion and dedupe.
func (s *Store) ListMemoryLinksForSources(ctx context.Context, srcIDs []int64) ([]LinkedConfirmedMemory, error) {
	return s.listMemoryLinksForSources(ctx, srcIDs, "", nil)
}

// ListMemoryLinksForSourcesVisibleToOwner applies the prompt-injection
// visibility policy for one agent owner: private owner pool plus the reserved
// shared pool, limited to the current repo plus general-scope facts.
func (s *Store) ListMemoryLinksForSourcesVisibleToOwner(ctx context.Context, owner MemoryOwner, repo string, srcIDs []int64) ([]LinkedConfirmedMemory, error) {
	return s.listMemoryLinksForSources(ctx, srcIDs,
		`((d.owner_kind = ? AND d.owner_ref = ? AND d.owner_version = ?) OR (d.owner_kind = ? AND d.owner_ref = ?))
	AND (d.scope = 'general' OR d.repo = ?)`,
		[]any{owner.Kind, owner.Ref, owner.Version, memoryOwnerKindShared, memorySharedOwnerRef, strings.TrimSpace(repo)})
}

// ListMemoryLinksForSourcesVisibleToOwnerAllRepos is the single-agent recall
// expansion policy when recall omits --repo: private owner pool plus shared,
// without a repo/general restriction.
func (s *Store) ListMemoryLinksForSourcesVisibleToOwnerAllRepos(ctx context.Context, owner MemoryOwner, srcIDs []int64) ([]LinkedConfirmedMemory, error) {
	return s.listMemoryLinksForSources(ctx, srcIDs,
		`((d.owner_kind = ? AND d.owner_ref = ? AND d.owner_version = ?) OR (d.owner_kind = ? AND d.owner_ref = ?))`,
		[]any{owner.Kind, owner.Ref, owner.Version, memoryOwnerKindShared, memorySharedOwnerRef})
}

// ListMemoryLinksForSourcesVisibleToAllAgents is the default recall expansion
// policy: every private agent pool plus the reserved shared pool. A non-empty
// repo applies the same repo/general restriction as the direct recall query.
func (s *Store) ListMemoryLinksForSourcesVisibleToAllAgents(ctx context.Context, repo string, srcIDs []int64) ([]LinkedConfirmedMemory, error) {
	where := `(d.owner_kind = 'agent' OR (d.owner_kind = ? AND d.owner_ref = ?))`
	args := []any{memoryOwnerKindShared, memorySharedOwnerRef}
	if strings.TrimSpace(repo) != "" {
		where += "\n\tAND (d.scope = 'general' OR d.repo = ?)"
		args = append(args, strings.TrimSpace(repo))
	}
	return s.listMemoryLinksForSources(ctx, srcIDs, where, args)
}

// ListMemoryLinksForSourcesVisibleToShared is the shared-only recall expansion
// policy. A non-empty repo applies the same repo/general restriction as the
// direct shared recall query.
func (s *Store) ListMemoryLinksForSourcesVisibleToShared(ctx context.Context, repo string, srcIDs []int64) ([]LinkedConfirmedMemory, error) {
	where := `d.owner_kind = ? AND d.owner_ref = ?`
	args := []any{memoryOwnerKindShared, memorySharedOwnerRef}
	if strings.TrimSpace(repo) != "" {
		where += "\n\tAND (d.scope = 'general' OR d.repo = ?)"
		args = append(args, strings.TrimSpace(repo))
	}
	return s.listMemoryLinksForSources(ctx, srcIDs, where, args)
}

func (s *Store) listMemoryLinksForSources(ctx context.Context, srcIDs []int64, visibilityWhere string, visibilityArgs []any) ([]LinkedConfirmedMemory, error) {
	ids := uniquePositiveInt64s(srcIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+len(visibilityArgs))
	for _, id := range ids {
		args = append(args, id)
	}
	query := `
SELECT ml.src_id, ml.score,
	d.id, d.owner_kind, d.owner_ref, d.owner_version, d.author_ref, d.repo, d.scope, d.key, d.content,
	d.provenance, d.source_job, d.first_confirmed_at, d.updated_at
FROM memory_links ml
JOIN confirmed_memories d ON d.id = ml.dst_id
WHERE ml.src_id IN (` + placeholders + `)
	AND d.superseded_by IS NULL
	AND d.retired_at = ''`
	if strings.TrimSpace(visibilityWhere) != "" {
		query += "\n\tAND " + visibilityWhere
		args = append(args, visibilityArgs...)
	}
	query += `
ORDER BY ml.score DESC, d.id ASC, ml.src_id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list memory links for sources: %w", err)
	}
	defer rows.Close()
	var out []LinkedConfirmedMemory
	for rows.Next() {
		var l LinkedConfirmedMemory
		var repoNull sql.NullString
		if err := rows.Scan(&l.SrcID, &l.Score,
			&l.Memory.ID, &l.Memory.Owner.Kind, &l.Memory.Owner.Ref, &l.Memory.Owner.Version, &l.Memory.AuthorRef,
			&repoNull, &l.Memory.Scope, &l.Memory.Key, &l.Memory.Content, &l.Memory.Provenance,
			&l.Memory.SourceJob, &l.Memory.FirstConfirmedAt, &l.Memory.UpdatedAt); err != nil {
			return nil, err
		}
		l.Memory.Repo = repoNull.String
		out = append(out, l)
	}
	return out, rows.Err()
}

func uniquePositiveInt64s(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func enrichConfirmedMemoryLinksTx(ctx context.Context, tx *sql.Tx, srcID int64, dryRun bool) (MemoryLinkEnrichment, error) {
	result := MemoryLinkEnrichment{SrcID: srcID}
	src, ok, err := confirmedMemoryForLinkingTx(ctx, tx, srcID)
	if err != nil || !ok {
		return result, err
	}
	matchQuery := memoryLinkMatchQuery(src.Content)
	if strings.TrimSpace(matchQuery) == "" {
		return result, nil
	}
	candidates, err := queryMemoryLinkCandidatesTx(ctx, tx, src, matchQuery, memoryAutoLinkK)
	if err != nil {
		return MemoryLinkEnrichment{}, err
	}
	now := nowRFC3339()
	// Candidates arrive best-first; the top score anchors the relative
	// weak-link cutoff for this source fact.
	var topScore float64
	if len(candidates) > 0 {
		topScore = candidates[0].Score
	}
	for _, c := range candidates {
		c.SrcID = srcID
		c.CreatedAt = now
		if c.Score < memoryAutoLinkMinScore || c.Score < topScore*memoryAutoLinkMinRelative {
			result.SkippedWeak++
			continue
		}
		if c.Origin == "existing" {
			result.SkippedExisting++
			continue
		}
		c.Origin = "auto"
		if dryRun {
			result.Created = append(result.Created, c)
			continue
		}
		res, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO memory_links (src_id, dst_id, score, origin, created_at)
VALUES (?, ?, ?, 'auto', ?)`, srcID, c.DstID, c.Score, now)
		if err != nil {
			return MemoryLinkEnrichment{}, fmt.Errorf("insert memory link %d -> %d: %w", srcID, c.DstID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return MemoryLinkEnrichment{}, err
		}
		if affected == 0 {
			result.SkippedExisting++
			continue
		}
		result.Created = append(result.Created, c)
	}
	return result, nil
}

func confirmedMemoryForLinkingTx(ctx context.Context, tx *sql.Tx, id int64) (ConfirmedMemory, bool, error) {
	var c ConfirmedMemory
	var repoNull sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
	provenance, source_job, first_confirmed_at, updated_at
FROM confirmed_memories
WHERE id = ? AND superseded_by IS NULL AND retired_at = ''`, id).Scan(
		&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &c.AuthorRef, &repoNull,
		&c.Scope, &c.Key, &c.Content, &c.Context, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return ConfirmedMemory{}, false, nil
	}
	if err != nil {
		return ConfirmedMemory{}, false, fmt.Errorf("read confirmed memory %d for linking: %w", id, err)
	}
	c.Repo = repoNull.String
	return c, true, nil
}

func queryMemoryLinkCandidatesTx(ctx context.Context, tx *sql.Tx, src ConfirmedMemory, matchQuery string, limit int) ([]MemoryLink, error) {
	if limit <= 0 {
		limit = memoryAutoLinkK
	}
	own := src.Owner
	if src.Owner.Kind == memoryOwnerKindShared && src.Owner.Ref == memorySharedOwnerRef {
		if author := strings.TrimSpace(src.AuthorRef); author != "" {
			own = MemoryOwner{Kind: memoryOwnerKindAgent, Ref: author}
		}
	}
	rows, err := tx.QueryContext(ctx, `
SELECT c.id, c.key, -bm25(f.confirmed_memories_fts) AS score,
	EXISTS(SELECT 1 FROM memory_links ml WHERE ml.src_id = ? AND ml.dst_id = c.id) AS already_linked
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND ((c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ?) OR (c.owner_kind = ? AND c.owner_ref = ?))
	AND (c.scope = 'general' OR c.repo = ?)
	AND c.superseded_by IS NULL
	AND c.retired_at = ''
	AND c.id <> ?
ORDER BY bm25(f.confirmed_memories_fts),
	CASE WHEN c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ? THEN 0 ELSE 1 END,
	c.id
LIMIT ?`,
		src.ID, matchQuery, own.Kind, own.Ref, own.Version, memoryOwnerKindShared, memorySharedOwnerRef,
		src.Repo, src.ID, own.Kind, own.Ref, own.Version, limit)
	if err != nil {
		return nil, fmt.Errorf("query memory link candidates: %w", err)
	}
	defer rows.Close()
	var out []MemoryLink
	for rows.Next() {
		var l MemoryLink
		var already int
		if err := rows.Scan(&l.DstID, &l.DstKey, &l.Score, &already); err != nil {
			return nil, err
		}
		if already != 0 {
			l.Origin = "existing"
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

var memoryLinkWordRun = regexp.MustCompile(`[A-Za-z0-9]+`)

var memoryLinkFTSKeywords = map[string]struct{}{
	"and": {}, "or": {}, "not": {}, "near": {},
}

var memoryLinkStopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "that": {}, "this": {},
	"you": {}, "your": {}, "are": {}, "was": {}, "will": {}, "have": {},
	"from": {}, "into": {}, "should": {}, "would": {}, "could": {},
}

func memoryLinkMatchQuery(content string) string {
	const maxTokens = 24
	seen := make(map[string]struct{}, maxTokens)
	tokens := make([]string, 0, maxTokens)
	for _, raw := range memoryLinkWordRun.FindAllString(content, -1) {
		tok := strings.ToLower(raw)
		if len(tok) < 3 {
			continue
		}
		if _, ok := memoryLinkFTSKeywords[tok]; ok {
			continue
		}
		if _, ok := memoryLinkStopwords[tok]; ok {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		tokens = append(tokens, `"`+tok+`"`)
		if len(tokens) >= maxTokens {
			break
		}
	}
	return strings.Join(tokens, " OR ")
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

// ListActiveConfirmedMemoriesForRetire returns active confirmed rows whose
// provenance starts with prefix. A non-empty agentRef narrows to rows owned by
// or authored by that agent. It is the dry-run half of
// `memory retire --provenance-prefix`.
func (s *Store) ListActiveConfirmedMemoriesForRetire(ctx context.Context, prefix, agentRef string) ([]ConfirmedMemory, error) {
	rows, err := listActiveConfirmedMemoriesByProvenancePrefix(ctx, s.db, prefix, agentRef)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// RetireConfirmedMemoriesByProvenancePrefix retires every active confirmed row
// whose provenance starts with prefix, removing each row from FTS in the same
// transaction. It returns the rows selected before retirement so callers can
// report the blast radius.
func (s *Store) RetireConfirmedMemoriesByProvenancePrefix(ctx context.Context, prefix, agentRef, reason string) ([]ConfirmedMemory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := listActiveConfirmedMemoriesByProvenancePrefix(ctx, tx, prefix, agentRef)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := retireConfirmedMemoryTx(ctx, tx, row.ID, "", reason); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rows, nil
}

func listActiveConfirmedMemoriesByProvenancePrefix(ctx context.Context, q rowsQuerier, prefix, agentRef string) ([]ConfirmedMemory, error) {
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("provenance prefix is required")
	}
	where := []string{"provenance LIKE ? ESCAPE '\\'", "superseded_by IS NULL", "retired_at = ''"}
	args := []any{likePrefix(prefix)}
	if strings.TrimSpace(agentRef) != "" {
		where = append(where, "(owner_ref = ? OR author_ref = ?)")
		args = append(args, strings.TrimSpace(agentRef), strings.TrimSpace(agentRef))
	}
	query := `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
	provenance, source_job, first_confirmed_at, updated_at
FROM confirmed_memories
WHERE ` + strings.Join(where, " AND ") + `
ORDER BY updated_at DESC, id DESC`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list confirmed memories by provenance prefix: %w", err)
	}
	defer rows.Close()
	return scanConfirmedMemories(rows)
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

// GroomSplitChild is one exact-substring child to insert for a brick parent.
type GroomSplitChild struct {
	Key     string
	Content string
}

// GroomSplitItem is one lossless brick split. ExpectedUpdatedAt is the
// detector's active-row revision and guards the parent mutation with a CAS.
type GroomSplitItem struct {
	ParentID          int64
	ExpectedUpdatedAt string
	Children          []GroomSplitChild
}

// GroomSplitApplied records the child ids created for one superseded parent.
type GroomSplitApplied struct {
	ParentID int64
	ChildIDs []int64
}

// GroomSplitResult reports applied parent splits and parents skipped because
// they were missing, changed, retired, or already superseded.
type GroomSplitResult struct {
	Applied []GroomSplitApplied
	Skipped []int64
}

// ApplyGroomSplits applies every deterministic split in one transaction. Each
// parent is CAS-guarded against the detector's active revision. Children inherit
// ownership, author, repo, scope, and source lineage; every child FTS row, the
// parent's FTS removal, superseded_by pointer, and cluster membership replacement
// commit atomically. memory_links are intentionally left for normal enrichment.
func (s *Store) ApplyGroomSplits(ctx context.Context, items []GroomSplitItem) (GroomSplitResult, error) {
	var result GroomSplitResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, item := range items {
		applied, ok, err := applyGroomSplitTx(ctx, tx, item)
		if err != nil {
			return GroomSplitResult{}, err
		}
		if !ok {
			result.Skipped = append(result.Skipped, item.ParentID)
			continue
		}
		result.Applied = append(result.Applied, applied)
	}
	if err := tx.Commit(); err != nil {
		return GroomSplitResult{}, err
	}
	return result, nil
}

func applyGroomSplitTx(ctx context.Context, tx *sql.Tx, item GroomSplitItem) (GroomSplitApplied, bool, error) {
	if item.ParentID <= 0 || len(item.Children) < 2 {
		return GroomSplitApplied{}, false, fmt.Errorf("groom split parent %d requires at least two children", item.ParentID)
	}
	var parent ConfirmedMemory
	var repoNull sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
	provenance, source_job, first_confirmed_at, updated_at
FROM confirmed_memories
WHERE id = ? AND updated_at = ? AND superseded_by IS NULL AND retired_at = ''`,
		item.ParentID, item.ExpectedUpdatedAt).Scan(
		&parent.ID, &parent.Owner.Kind, &parent.Owner.Ref, &parent.Owner.Version, &parent.AuthorRef, &repoNull,
		&parent.Scope, &parent.Key, &parent.Content, &parent.Context, &parent.Provenance, &parent.SourceJob,
		&parent.FirstConfirmedAt, &parent.UpdatedAt)
	if err == sql.ErrNoRows {
		return GroomSplitApplied{}, false, nil
	}
	if err != nil {
		return GroomSplitApplied{}, false, fmt.Errorf("read groom split parent %d: %w", item.ParentID, err)
	}
	parent.Repo = repoNull.String

	var covered strings.Builder
	seenKeys := make(map[string]struct{}, len(item.Children))
	for _, child := range item.Children {
		if strings.TrimSpace(child.Key) == "" || strings.TrimSpace(child.Content) == "" {
			return GroomSplitApplied{}, false, fmt.Errorf("groom split parent %d has an empty child key or content", item.ParentID)
		}
		if !strings.HasPrefix(child.Key, parent.Key+"-") {
			return GroomSplitApplied{}, false, fmt.Errorf("groom split parent %d child key %q is outside parent key %q", item.ParentID, child.Key, parent.Key)
		}
		if _, duplicate := seenKeys[child.Key]; duplicate {
			return GroomSplitApplied{}, false, fmt.Errorf("groom split parent %d repeats child key %q", item.ParentID, child.Key)
		}
		seenKeys[child.Key] = struct{}{}
		covered.WriteString(child.Content)
	}
	if covered.String() != strings.TrimSpace(parent.Content) {
		return GroomSplitApplied{}, false, fmt.Errorf("groom split parent %d violates lossless coverage invariant", item.ParentID)
	}

	var clusterID int64
	hasCluster := true
	if err := tx.QueryRowContext(ctx, `SELECT cluster_id FROM memory_cluster_members WHERE memory_id = ?`, parent.ID).Scan(&clusterID); err == sql.ErrNoRows {
		hasCluster = false
	} else if err != nil {
		return GroomSplitApplied{}, false, fmt.Errorf("read groom split parent cluster %d: %w", parent.ID, err)
	}

	now := nowRFC3339()
	applied := GroomSplitApplied{ParentID: parent.ID, ChildIDs: make([]int64, 0, len(item.Children))}
	for _, child := range item.Children {
		res, err := tx.ExecContext(ctx, `
INSERT INTO confirmed_memories
	(owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context, provenance, source_job, first_confirmed_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			parent.Owner.Kind, parent.Owner.Ref, parent.Owner.Version, parent.AuthorRef, nullableRepo(parent.Repo), parent.Scope,
			child.Key, child.Content, parent.Key, fmt.Sprintf("groom-split:%d", parent.ID), parent.SourceJob, parent.FirstConfirmedAt, now)
		if err != nil {
			return GroomSplitApplied{}, false, fmt.Errorf("insert groom split child for parent %d: %w", parent.ID, err)
		}
		childID, err := res.LastInsertId()
		if err != nil {
			return GroomSplitApplied{}, false, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO confirmed_memories_fts(rowid, content, key) VALUES (?, ?, ?)`, childID, child.Content, child.Key); err != nil {
			return GroomSplitApplied{}, false, fmt.Errorf("sync groom split child fts %d: %w", childID, err)
		}
		if hasCluster {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_cluster_members (memory_id, cluster_id) VALUES (?, ?)
ON CONFLICT(memory_id) DO UPDATE SET cluster_id = excluded.cluster_id`, childID, clusterID); err != nil {
				return GroomSplitApplied{}, false, fmt.Errorf("attach groom split child %d to cluster %d: %w", childID, clusterID, err)
			}
		}
		applied.ChildIDs = append(applied.ChildIDs, childID)
	}

	res, err := tx.ExecContext(ctx, `
UPDATE confirmed_memories SET superseded_by = ?
WHERE id = ? AND updated_at = ? AND superseded_by IS NULL AND retired_at = ''`,
		applied.ChildIDs[0], parent.ID, item.ExpectedUpdatedAt)
	if err != nil {
		return GroomSplitApplied{}, false, fmt.Errorf("supersede groom split parent %d: %w", parent.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return GroomSplitApplied{}, false, err
	}
	if affected != 1 {
		return GroomSplitApplied{}, false, fmt.Errorf("groom split parent %d changed during apply", parent.ID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM confirmed_memories_fts WHERE rowid = ?`, parent.ID); err != nil {
		return GroomSplitApplied{}, false, fmt.Errorf("sync groom split parent fts %d: %w", parent.ID, err)
	}
	if hasCluster {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_cluster_members WHERE memory_id = ?`, parent.ID); err != nil {
			return GroomSplitApplied{}, false, fmt.Errorf("detach groom split parent %d from cluster: %w", parent.ID, err)
		}
	}
	return applied, true, nil
}

// GroomSplitRevertOptions selects active groom splits to restore. Empty
// ParentIDs and Since select every currently active split.
type GroomSplitRevertOptions struct {
	ParentIDs []int64
	Since     string
	DryRun    bool
}

// GroomSplitReverted records one parent and the direct children that were, or
// in dry-run mode would be, retired to restore it.
type GroomSplitReverted struct {
	ParentID int64   `json:"parent_id"`
	ChildIDs []int64 `json:"child_ids"`
}

// GroomSplitRevertSkipped is one selected parent that failed a reverse coverage
// or active-split invariant and was left untouched.
type GroomSplitRevertSkipped struct {
	ParentID int64  `json:"parent_id"`
	Reason   string `json:"reason"`
}

// GroomSplitRevertResult reports restored and fail-closed parent groups.
type GroomSplitRevertResult struct {
	Reverted []GroomSplitReverted      `json:"reverted"`
	Skipped  []GroomSplitRevertSkipped `json:"skipped"`
}

// RevertGroomSplits restores active parents superseded by direct groom-split
// children. Validation is per-parent and happens before any mutation for that
// group; an edited, retired, or recursively split child therefore skips only its
// parent. All successful groups commit atomically.
func (s *Store) RevertGroomSplits(ctx context.Context, options GroomSplitRevertOptions) (GroomSplitRevertResult, error) {
	var result GroomSplitRevertResult
	if strings.TrimSpace(options.Since) != "" {
		if _, err := time.Parse(time.RFC3339, options.Since); err != nil {
			return result, fmt.Errorf("invalid groom split revert since timestamp %q: %w", options.Since, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	parentIDs, err := selectActiveGroomSplitParents(ctx, tx, options)
	if err != nil {
		return result, err
	}
	for _, parentID := range parentIDs {
		reverted, ok, reason, err := revertGroomSplitTx(ctx, tx, parentID, options.DryRun)
		if err != nil {
			return GroomSplitRevertResult{}, err
		}
		if !ok {
			result.Skipped = append(result.Skipped, GroomSplitRevertSkipped{ParentID: parentID, Reason: reason})
			continue
		}
		result.Reverted = append(result.Reverted, reverted)
	}
	if err := tx.Commit(); err != nil {
		return GroomSplitRevertResult{}, err
	}
	return result, nil
}

func selectActiveGroomSplitParents(ctx context.Context, tx *sql.Tx, options GroomSplitRevertOptions) ([]int64, error) {
	query := `
SELECT DISTINCT p.id
FROM confirmed_memories p
JOIN confirmed_memories c ON c.provenance = 'groom-split:' || p.id
WHERE p.superseded_by IS NOT NULL AND p.retired_at = ''
	AND c.superseded_by IS NULL AND c.retired_at = ''`
	var args []any
	if len(options.ParentIDs) > 0 {
		seen := make(map[int64]struct{}, len(options.ParentIDs))
		ids := make([]int64, 0, len(options.ParentIDs))
		for _, id := range options.ParentIDs {
			if id <= 0 {
				return nil, fmt.Errorf("invalid groom split parent id %d", id)
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += "\n\tAND p.id IN (" + strings.Join(placeholders, ",") + ")"
	}
	if since := strings.TrimSpace(options.Since); since != "" {
		query += "\n\tAND c.updated_at >= ?"
		args = append(args, since)
	}
	query += "\nORDER BY p.id"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select active groom split parents: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func revertGroomSplitTx(ctx context.Context, tx *sql.Tx, parentID int64, dryRun bool) (GroomSplitReverted, bool, string, error) {
	var parentKey, parentContent string
	var expectedChildID int64
	err := tx.QueryRowContext(ctx, `
SELECT key, content, COALESCE(superseded_by, 0)
FROM confirmed_memories
WHERE id = ? AND retired_at = ''`, parentID).Scan(&parentKey, &parentContent, &expectedChildID)
	if err == sql.ErrNoRows {
		return GroomSplitReverted{}, false, "parent is missing or retired", nil
	}
	if err != nil {
		return GroomSplitReverted{}, false, "", fmt.Errorf("read groom split parent %d for revert: %w", parentID, err)
	}

	type childRow struct {
		id        int64
		content   string
		clusterID sql.NullInt64
	}
	rows, err := tx.QueryContext(ctx, `
SELECT c.id, c.content, m.cluster_id
FROM confirmed_memories c
LEFT JOIN memory_cluster_members m ON m.memory_id = c.id
WHERE c.provenance = ? AND c.superseded_by IS NULL AND c.retired_at = ''
ORDER BY c.id`, fmt.Sprintf("groom-split:%d", parentID))
	if err != nil {
		return GroomSplitReverted{}, false, "", fmt.Errorf("list groom split children for parent %d: %w", parentID, err)
	}
	var children []childRow
	for rows.Next() {
		var child childRow
		if err := rows.Scan(&child.id, &child.content, &child.clusterID); err != nil {
			rows.Close()
			return GroomSplitReverted{}, false, "", err
		}
		children = append(children, child)
	}
	if err := rows.Close(); err != nil {
		return GroomSplitReverted{}, false, "", err
	}
	if len(children) < 2 {
		return GroomSplitReverted{}, false, "fewer than two active split children", nil
	}
	if expectedChildID != children[0].id {
		return GroomSplitReverted{}, false, "parent superseded pointer does not match the lowest active child", nil
	}
	var coverage strings.Builder
	childIDs := make([]int64, 0, len(children))
	for _, child := range children {
		coverage.WriteString(child.content)
		childIDs = append(childIDs, child.id)
	}
	if coverage.String() != strings.TrimSpace(parentContent) {
		return GroomSplitReverted{}, false, "active children no longer reconstruct the parent", nil
	}

	reverted := GroomSplitReverted{ParentID: parentID, ChildIDs: childIDs}
	if dryRun {
		return reverted, true, "", nil
	}
	for _, child := range children {
		if err := retireConfirmedMemoryTx(ctx, tx, child.id, "", fmt.Sprintf("groom-split-revert:%d", parentID)); err != nil {
			return GroomSplitReverted{}, false, "", fmt.Errorf("retire groom split child %d: %w", child.id, err)
		}
	}
	placeholders := make([]string, len(childIDs))
	args := make([]any, 0, len(childIDs))
	for i, id := range childIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_cluster_members WHERE memory_id IN (`+strings.Join(placeholders, ",")+`)`, args...); err != nil {
		return GroomSplitReverted{}, false, "", fmt.Errorf("detach groom split children for parent %d: %w", parentID, err)
	}
	now := nowRFC3339()
	res, err := tx.ExecContext(ctx, `
UPDATE confirmed_memories SET superseded_by = NULL, updated_at = ?
WHERE id = ? AND superseded_by = ? AND retired_at = ''`, now, parentID, expectedChildID)
	if err != nil {
		return GroomSplitReverted{}, false, "", fmt.Errorf("restore groom split parent %d: %w", parentID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return GroomSplitReverted{}, false, "", err
	}
	if affected != 1 {
		return GroomSplitReverted{}, false, "", fmt.Errorf("groom split parent %d changed during revert", parentID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM confirmed_memories_fts WHERE rowid = ?`, parentID); err != nil {
		return GroomSplitReverted{}, false, "", fmt.Errorf("clear restored parent FTS %d: %w", parentID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO confirmed_memories_fts(rowid, content, key) VALUES (?, ?, ?)`, parentID, parentContent, parentKey); err != nil {
		return GroomSplitReverted{}, false, "", fmt.Errorf("restore parent FTS %d: %w", parentID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_cluster_members WHERE memory_id = ?`, parentID); err != nil {
		return GroomSplitReverted{}, false, "", fmt.Errorf("clear restored parent cluster %d: %w", parentID, err)
	}
	if children[0].clusterID.Valid {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_cluster_members(memory_id, cluster_id) VALUES (?, ?)`, parentID, children[0].clusterID.Int64); err != nil {
			return GroomSplitReverted{}, false, "", fmt.Errorf("restore parent %d cluster: %w", parentID, err)
		}
	}
	return reverted, true, "", nil
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

// GroomRetire is one row a groom plan proposes retiring: the memory id and the
// full store reason ("groom:<detector>").
type GroomRetire struct {
	ID     int64
	Reason string
}

// GroomRetireResult reports which ids were actually retired and which were skipped
// (already retired or no longer present) by ApplyGroomRetirements.
type GroomRetireResult struct {
	Retired []int64
	Skipped []int64
}

// ApplyGroomRetirements retires every planned row in ONE transaction (all-or-
// nothing on error), removing each from the FTS index in the same transaction so
// the retired facts stop being injected and stop appearing in future exports.
// Unlike RetireConfirmedMemory, a planned id that is already retired or missing is
// SKIPPED gracefully rather than aborting the batch: the groom plan may carry
// duplicate ids (one memory flagged by two detectors), and re-applying is
// idempotent. The caller's snapshot-hash staleness guard is what protects against
// a concurrent vault edit landing between propose and apply; this method just
// executes the vetted plan. It is additive and NON-destructive (the row is kept
// for audit with retired_at/retired_reason set) and never writes superseded_by.
func (s *Store) ApplyGroomRetirements(ctx context.Context, items []GroomRetire) (GroomRetireResult, error) {
	var result GroomRetireResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	now := nowRFC3339()
	for _, it := range items {
		retired, err := groomRetireTx(ctx, tx, it.ID, it.Reason, now)
		if err != nil {
			return GroomRetireResult{}, err
		}
		if retired {
			result.Retired = append(result.Retired, it.ID)
		} else {
			result.Skipped = append(result.Skipped, it.ID)
		}
	}
	if err := tx.Commit(); err != nil {
		return GroomRetireResult{}, err
	}
	return result, nil
}

// groomRetireTx retires one row for a groom-style plan apply, removing it from
// FTS in the same transaction. Unlike retireConfirmedMemoryTx it SKIPS gracefully
// (returns false, nil) when the row is already retired or missing: a plan may
// carry duplicate ids and re-applying must stay idempotent.
func groomRetireTx(ctx context.Context, tx *sql.Tx, id int64, reason, now string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
UPDATE confirmed_memories
SET retired_at = ?, retired_reason = ?
WHERE id = ? AND retired_at = ''`, now, reason, id)
	if err != nil {
		return false, fmt.Errorf("groom retire %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		// Already retired within this batch (a duplicate id), already retired by a
		// prior apply, or the row is gone — skip gracefully.
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM confirmed_memories_fts WHERE rowid = ?`, id); err != nil {
		return false, fmt.Errorf("groom retire fts %d: %w", id, err)
	}
	return true, nil
}

// GroomRekeyItem is one planned #804 rekey group: rewrite the keeper's key to
// the stable form and retire its older sibling editions, stamping Reason as
// their retired_reason.
type GroomRekeyItem struct {
	KeepID    int64
	NewKey    string
	RetireIDs []int64
	Reason    string
}

// GroomCrossPoolItem is one planned #804 promote-and-retire pair: retire the
// stale shared row (Reason as retired_reason) and promote the newer private row
// into the shared pool, preserving its author.
type GroomCrossPoolItem struct {
	PrivateID int64
	SharedID  int64
	Reason    string
}

// GroomPlanApplyResult reports what one ApplyGroomPlan transaction did. Skipped
// ids were already retired, missing, no longer active, or would have collided
// with an active same-key row — every skip is graceful so re-applying stays
// idempotent.
type GroomPlanApplyResult struct {
	Retired          []int64
	RetireSkipped    []int64
	Rekeyed          []int64 // keeper ids whose group applied
	RekeySkipped     []int64 // keeper ids whose group was skipped whole
	Promoted         []int64 // private ids promoted to the shared pool
	CrossPoolSkipped []int64 // private ids whose pair was skipped
}

// ApplyGroomPlan applies a whole groom plan in ONE transaction, in a fixed
// order: plain retirements first, then rekey groups, then cross-pool
// promote-and-retire pairs. All-or-nothing on error; within the transaction each
// action skips gracefully when its target rows are no longer in the expected
// state (the caller's snapshot-hash staleness guard protects against a store
// that moved between propose and apply — this method just executes the vetted
// plan defensively).
func (s *Store) ApplyGroomPlan(ctx context.Context, retirements []GroomRetire, rekeys []GroomRekeyItem, crossPool []GroomCrossPoolItem) (GroomPlanApplyResult, error) {
	var result GroomPlanApplyResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	now := nowRFC3339()

	for _, it := range retirements {
		retired, err := groomRetireTx(ctx, tx, it.ID, it.Reason, now)
		if err != nil {
			return GroomPlanApplyResult{}, err
		}
		if retired {
			result.Retired = append(result.Retired, it.ID)
		} else {
			result.RetireSkipped = append(result.RetireSkipped, it.ID)
		}
	}

	for _, it := range rekeys {
		applied, err := groomRekeyTx(ctx, tx, it, now)
		if err != nil {
			return GroomPlanApplyResult{}, err
		}
		if applied {
			result.Rekeyed = append(result.Rekeyed, it.KeepID)
		} else {
			result.RekeySkipped = append(result.RekeySkipped, it.KeepID)
		}
	}

	for _, it := range crossPool {
		applied, err := groomCrossPoolTx(ctx, tx, it, now)
		if err != nil {
			return GroomPlanApplyResult{}, err
		}
		if applied {
			result.Promoted = append(result.Promoted, it.PrivateID)
		} else {
			result.CrossPoolSkipped = append(result.CrossPoolSkipped, it.PrivateID)
		}
	}

	if err := tx.Commit(); err != nil {
		return GroomPlanApplyResult{}, err
	}
	return result, nil
}

// groomRekeyTx applies one rekey group: verify the keeper is still active,
// verify the stable key is not held by another active row in the same domain,
// retire the sibling editions, rewrite the keeper's key, and re-sync its FTS row
// (content unchanged, key column updated) — all inside the caller's transaction.
// Returns (false, nil) to skip the WHOLE group when the keeper is gone/inactive
// or the target key is occupied, so a plan raced by an earlier retirement never
// half-applies a group.
func groomRekeyTx(ctx context.Context, tx *sql.Tx, item GroomRekeyItem, now string) (bool, error) {
	var keep ConfirmedMemory
	var repoNull sql.NullString
	var retiredAt string
	err := tx.QueryRowContext(ctx, `
SELECT owner_kind, owner_ref, owner_version, repo, key, content, retired_at
FROM confirmed_memories
WHERE id = ? AND superseded_by IS NULL`, item.KeepID).Scan(
		&keep.Owner.Kind, &keep.Owner.Ref, &keep.Owner.Version, &repoNull, &keep.Key, &keep.Content, &retiredAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("groom rekey read keeper %d: %w", item.KeepID, err)
	}
	if retiredAt != "" {
		return false, nil
	}
	keep.Repo = repoNull.String

	if item.NewKey != keep.Key {
		var occupied int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM confirmed_memories
WHERE owner_kind = ? AND owner_ref = ? AND owner_version = ?
	AND ((? IS NULL AND repo IS NULL) OR repo = ?)
	AND key = ? AND superseded_by IS NULL AND retired_at = '' AND id <> ?`,
			keep.Owner.Kind, keep.Owner.Ref, keep.Owner.Version,
			nullableRepo(keep.Repo), nullableRepo(keep.Repo), item.NewKey, item.KeepID).Scan(&occupied); err != nil {
			return false, fmt.Errorf("groom rekey occupancy check for %q: %w", item.NewKey, err)
		}
		if occupied > 0 {
			return false, nil
		}
	}

	for _, rid := range item.RetireIDs {
		if _, err := groomRetireTx(ctx, tx, rid, item.Reason, now); err != nil {
			return false, err
		}
	}

	if item.NewKey != keep.Key {
		if _, err := tx.ExecContext(ctx, `
UPDATE confirmed_memories SET key = ?, updated_at = ? WHERE id = ?`,
			item.NewKey, now, item.KeepID); err != nil {
			return false, fmt.Errorf("groom rekey %d to %q: %w", item.KeepID, item.NewKey, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM confirmed_memories_fts WHERE rowid = ?`, item.KeepID); err != nil {
			return false, fmt.Errorf("groom rekey fts delete %d: %w", item.KeepID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO confirmed_memories_fts(rowid, content, key) VALUES (?, ?, ?)`,
			item.KeepID, keep.Content, item.NewKey); err != nil {
			return false, fmt.Errorf("groom rekey fts insert %d: %w", item.KeepID, err)
		}
	}
	return true, nil
}

// groomCrossPoolTx applies one promote-and-retire pair: retire the stale shared
// row FIRST (freeing its slot in the active-row unique index for the equal-key
// case), then promote the private row into the shared pool with its author
// preserved. Skips gracefully when the private row is no longer active or when
// promoting would collide with a different active shared row on the same key.
func groomCrossPoolTx(ctx context.Context, tx *sql.Tx, item GroomCrossPoolItem, now string) (bool, error) {
	var privateKey string
	var repoNull sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT key, repo FROM confirmed_memories
WHERE id = ? AND superseded_by IS NULL AND retired_at = ''`, item.PrivateID).Scan(&privateKey, &repoNull)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("groom cross-pool read private %d: %w", item.PrivateID, err)
	}

	// The stale shared edition may already be retired by an earlier plan section
	// or a prior apply — that is fine, retiring is graceful.
	if _, err := groomRetireTx(ctx, tx, item.SharedID, item.Reason, now); err != nil {
		return false, err
	}

	var occupied int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM confirmed_memories
WHERE owner_kind = ? AND owner_ref = ? AND owner_version = ''
	AND ((? IS NULL AND repo IS NULL) OR repo = ?)
	AND key = ? AND superseded_by IS NULL AND retired_at = '' AND id <> ?`,
		memoryOwnerKindShared, memorySharedOwnerRef,
		nullableRepo(repoNull.String), nullableRepo(repoNull.String), privateKey, item.PrivateID).Scan(&occupied); err != nil {
		return false, fmt.Errorf("groom cross-pool occupancy check for %q: %w", privateKey, err)
	}
	if occupied > 0 {
		return false, nil
	}

	if _, err := promoteConfirmedMemoryToSharedTx(ctx, tx, item.PrivateID, now); err != nil {
		return false, err
	}
	return true, nil
}

// CrossPoolSharedMatch is the store-computed secondary-evidence tuple for the
// groom cross-pool detector (#804): one active private agent fact, its TOP bm25
// match among active shared-pool facts in the same repo/scope domain, and
// whether a memory_links edge connects the two rows in either direction. Pure
// read; zero writes.
type CrossPoolSharedMatch struct {
	PrivateID int64
	SharedID  int64
	Score     float64 // -bm25, higher is better
	Linked    bool
}

// ListCrossPoolSharedMatches computes the cross-pool secondary signals: for
// every active private agent fact, the single best bm25 shared-pool match in the
// same repo/scope domain (built from the fact's own content, like auto-linking)
// plus the linked flag. Private facts are collected FIRST and queried after —
// the store runs on one connection, so nesting a query inside an open rows
// iteration would deadlock.
func (s *Store) ListCrossPoolSharedMatches(ctx context.Context) ([]CrossPoolSharedMatch, error) {
	type privateFact struct {
		id      int64
		repo    string
		scope   string
		content string
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, repo, scope, content FROM confirmed_memories
WHERE owner_kind = ? AND superseded_by IS NULL AND retired_at = ''
ORDER BY id`, memoryOwnerKindAgent)
	if err != nil {
		return nil, fmt.Errorf("list private facts for cross-pool signals: %w", err)
	}
	var facts []privateFact
	for rows.Next() {
		var f privateFact
		var repoNull sql.NullString
		if err := rows.Scan(&f.id, &repoNull, &f.scope, &f.content); err != nil {
			rows.Close()
			return nil, err
		}
		f.repo = repoNull.String
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var out []CrossPoolSharedMatch
	for _, f := range facts {
		matchQuery := memoryLinkMatchQuery(f.content)
		if strings.TrimSpace(matchQuery) == "" {
			continue
		}
		var sharedID int64
		var score float64
		err := s.db.QueryRowContext(ctx, `
SELECT c.id, -bm25(f.confirmed_memories_fts) AS score
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND c.owner_kind = ? AND c.owner_ref = ?
	AND ((? IS NULL AND c.repo IS NULL) OR c.repo = ?)
	AND c.scope = ?
	AND c.superseded_by IS NULL AND c.retired_at = ''
ORDER BY bm25(f.confirmed_memories_fts), c.id
LIMIT 1`,
			matchQuery, memoryOwnerKindShared, memorySharedOwnerRef,
			nullableRepo(f.repo), nullableRepo(f.repo), f.scope).Scan(&sharedID, &score)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cross-pool shared match for %d: %w", f.id, err)
		}
		var linked int
		if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM memory_links
WHERE (src_id = ? AND dst_id = ?) OR (src_id = ? AND dst_id = ?))`,
			f.id, sharedID, sharedID, f.id).Scan(&linked); err != nil {
			return nil, fmt.Errorf("cross-pool link check %d<->%d: %w", f.id, sharedID, err)
		}
		out = append(out, CrossPoolSharedMatch{PrivateID: f.id, SharedID: sharedID, Score: score, Linked: linked != 0})
	}
	return out, nil
}

// QueryConfirmedMemories is the READ path for job-prompt assembly. It runs one
// FTS5/BM25 query over confirmed content, filtered by the retrieval default:
// the running agent's private pool UNION the reserved shared pool, then
// (repo = current OR scope = general), confirmed tier only, and active rows
// only. matchQuery MUST be a sanitized MATCH string (see
// internal/memory.SanitizeFTSQuery) — never raw job text. Results are ranked by
// BM25 (ascending; lower is more relevant), then private facts before shared on
// an equal BM25 score, then recency, capped by limit. After SQL ranking, a
// deterministic floor protects private memory: if private matches exist but the
// limit slice contains only shared rows, the weakest shared row is swapped for
// the strongest private row.
func (s *Store) QueryConfirmedMemories(ctx context.Context, owner MemoryOwner, repo, matchQuery string, limit int) ([]ConfirmedMemory, error) {
	if strings.TrimSpace(matchQuery) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 15
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, c.owner_kind, c.owner_ref, c.owner_version, c.author_ref, c.repo, c.scope, c.key, c.content, c.context,
	c.provenance, c.source_job, c.first_confirmed_at, c.updated_at
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND ((c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ?) OR (c.owner_kind = ? AND c.owner_ref = ?))
	AND (c.scope = 'general' OR c.repo = ?)
	AND c.superseded_by IS NULL
	AND c.retired_at = ''
ORDER BY bm25(f.confirmed_memories_fts),
	CASE WHEN c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ? THEN 0 ELSE 1 END,
	c.updated_at DESC, c.id DESC
LIMIT ?`,
		matchQuery, owner.Kind, owner.Ref, owner.Version, memoryOwnerKindShared, memorySharedOwnerRef, repo,
		owner.Kind, owner.Ref, owner.Version, limit)
	if err != nil {
		return nil, fmt.Errorf("query confirmed memories: %w", err)
	}
	defer rows.Close()
	out, err := scanConfirmedMemories(rows)
	if err != nil {
		return nil, err
	}
	return s.ensureOwnMemoryFloor(ctx, owner, repo, matchQuery, limit, true, out)
}

// QueryConfirmedMemoriesForOwnerAllRepos is the recall variant for a single
// agent pool when the caller does not provide a repo filter. It keeps the same
// confirmed-only, active-row, FTS5/BM25 ranking as QueryConfirmedMemories, but
// intentionally does not apply the repo/general visibility clause.
func (s *Store) QueryConfirmedMemoriesForOwnerAllRepos(ctx context.Context, owner MemoryOwner, matchQuery string, limit int) ([]ConfirmedMemory, error) {
	if strings.TrimSpace(matchQuery) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 15
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, c.owner_kind, c.owner_ref, c.owner_version, c.author_ref, c.repo, c.scope, c.key, c.content, c.context,
	c.provenance, c.source_job, c.first_confirmed_at, c.updated_at
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND ((c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ?) OR (c.owner_kind = ? AND c.owner_ref = ?))
	AND c.superseded_by IS NULL
	AND c.retired_at = ''
ORDER BY bm25(f.confirmed_memories_fts),
	CASE WHEN c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ? THEN 0 ELSE 1 END,
	c.updated_at DESC, c.id DESC
LIMIT ?`,
		matchQuery, owner.Kind, owner.Ref, owner.Version, memoryOwnerKindShared, memorySharedOwnerRef,
		owner.Kind, owner.Ref, owner.Version, limit)
	if err != nil {
		return nil, fmt.Errorf("query confirmed memories for owner all repos: %w", err)
	}
	defer rows.Close()
	out, err := scanConfirmedMemories(rows)
	if err != nil {
		return nil, err
	}
	return s.ensureOwnMemoryFloor(ctx, owner, "", matchQuery, limit, false, out)
}

func (s *Store) ensureOwnMemoryFloor(ctx context.Context, owner MemoryOwner, repo, matchQuery string, limit int, filterRepo bool, rows []ConfirmedMemory) ([]ConfirmedMemory, error) {
	if len(rows) == 0 || limit <= 0 {
		return rows, nil
	}
	hasOwn := false
	weakestShared := -1
	for i, r := range rows {
		if r.Owner.Kind == owner.Kind && r.Owner.Ref == owner.Ref && r.Owner.Version == owner.Version {
			hasOwn = true
			break
		}
		if r.Owner.Kind == memoryOwnerKindShared {
			weakestShared = i
		}
	}
	if hasOwn || weakestShared < 0 {
		return rows, nil
	}
	own, ok, err := s.queryStrongestOwnMemory(ctx, owner, repo, matchQuery, filterRepo)
	if err != nil || !ok {
		return rows, err
	}
	if len(rows) < limit {
		return append(rows, own), nil
	}
	rows[weakestShared] = own
	return rows, nil
}

func (s *Store) queryStrongestOwnMemory(ctx context.Context, owner MemoryOwner, repo, matchQuery string, filterRepo bool) (ConfirmedMemory, bool, error) {
	query := `
SELECT c.id, c.owner_kind, c.owner_ref, c.owner_version, c.author_ref, c.repo, c.scope, c.key, c.content, c.context,
	c.provenance, c.source_job, c.first_confirmed_at, c.updated_at
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ?
	AND c.superseded_by IS NULL
	AND c.retired_at = ''`
	args := []any{matchQuery, owner.Kind, owner.Ref, owner.Version}
	if filterRepo {
		query += "\n\tAND (c.scope = 'general' OR c.repo = ?)"
		args = append(args, repo)
	}
	query += `
ORDER BY bm25(f.confirmed_memories_fts), c.updated_at DESC, c.id DESC
LIMIT 1`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ConfirmedMemory{}, false, fmt.Errorf("query strongest own memory: %w", err)
	}
	defer rows.Close()
	matches, err := scanConfirmedMemories(rows)
	if err != nil {
		return ConfirmedMemory{}, false, err
	}
	if len(matches) == 0 {
		return ConfirmedMemory{}, false, nil
	}
	return matches[0], true, nil
}

// QueryConfirmedMemoriesForAllAgents is the read-only recall path for sessions
// that are not running as a specific Gitmoot agent. It uses the same FTS5/BM25
// ranking as QueryConfirmedMemories, but searches every agent owner pool plus
// the shared pool. A non-empty repo applies the same repo/general visibility
// filter as injection; an empty repo searches every repo and general facts. Role
// pools remain excluded.
func (s *Store) QueryConfirmedMemoriesForAllAgents(ctx context.Context, repo, matchQuery string, limit int) ([]ConfirmedMemory, error) {
	if strings.TrimSpace(matchQuery) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 15
	}
	query := `
SELECT c.id, c.owner_kind, c.owner_ref, c.owner_version, c.author_ref, c.repo, c.scope, c.key, c.content, c.context,
	c.provenance, c.source_job, c.first_confirmed_at, c.updated_at
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND (c.owner_kind = 'agent' OR (c.owner_kind = 'shared' AND c.owner_ref = 'shared'))
	AND c.superseded_by IS NULL
	AND c.retired_at = ''
ORDER BY bm25(f.confirmed_memories_fts), c.updated_at DESC, c.id DESC
LIMIT ?`
	args := []any{matchQuery}
	if strings.TrimSpace(repo) != "" {
		query = strings.Replace(query, "\n\tAND c.superseded_by IS NULL", "\n\tAND (c.scope = 'general' OR c.repo = ?)\n\tAND c.superseded_by IS NULL", 1)
		args = append(args, strings.TrimSpace(repo))
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query confirmed memories for all agents: %w", err)
	}
	defer rows.Close()
	return scanConfirmedMemories(rows)
}

// QueryConfirmedMemoriesForShared returns active confirmed rows from only the
// reserved shared pool. It backs `memory recall --shared`.
func (s *Store) QueryConfirmedMemoriesForShared(ctx context.Context, repo, matchQuery string, limit int) ([]ConfirmedMemory, error) {
	if strings.TrimSpace(matchQuery) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 15
	}
	query := `
SELECT c.id, c.owner_kind, c.owner_ref, c.owner_version, c.author_ref, c.repo, c.scope, c.key, c.content, c.context,
	c.provenance, c.source_job, c.first_confirmed_at, c.updated_at
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND c.owner_kind = 'shared' AND c.owner_ref = 'shared'
	AND c.superseded_by IS NULL
	AND c.retired_at = ''
ORDER BY bm25(f.confirmed_memories_fts), c.updated_at DESC, c.id DESC
LIMIT ?`
	args := []any{matchQuery}
	if strings.TrimSpace(repo) != "" {
		query = strings.Replace(query, "\n\tAND c.superseded_by IS NULL", "\n\tAND (c.scope = 'general' OR c.repo = ?)\n\tAND c.superseded_by IS NULL", 1)
		args = append(args, strings.TrimSpace(repo))
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query confirmed memories for shared: %w", err)
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
	return listConfirmedMemoriesForVault(ctx, s.db, agentRef)
}

// rowsQuerier is the QueryContext shape shared by *sql.DB and *sql.Tx, so the
// vault read can run on a plain connection or INSIDE a transaction (the atomic
// cluster recompute re-reads the anchor rows within its own tx).
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// listConfirmedMemoriesForVault is the shared body of ListConfirmedMemoriesForVault
// so the exact active-fact projection/ordering can be reused inside a transaction
// (via a *sql.Tx) without duplicating the query.
func listConfirmedMemoriesForVault(ctx context.Context, q rowsQuerier, agentRef string) ([]ConfirmedMemory, error) {
	query := `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
	provenance, source_job, first_confirmed_at, updated_at, COALESCE(superseded_by, 0)
FROM confirmed_memories
WHERE superseded_by IS NULL AND retired_at = ''`
	var args []any
	if strings.TrimSpace(agentRef) != "" {
		query += "\n\tAND ((owner_kind = 'agent' AND owner_ref = ?) OR (owner_kind = 'shared' AND owner_ref = 'shared' AND author_ref = ?))"
		args = append(args, agentRef, agentRef)
	}
	query += "\nORDER BY id"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list confirmed memories for vault: %w", err)
	}
	defer rows.Close()
	var out []ConfirmedMemory
	for rows.Next() {
		var c ConfirmedMemory
		var repoNull sql.NullString
		if err := rows.Scan(&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &c.AuthorRef, &repoNull,
			&c.Scope, &c.Key, &c.Content, &c.Context, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt, &c.UpdatedAt, &c.SupersededBy); err != nil {
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
SELECT c.id, c.owner_kind, c.owner_ref, c.owner_version, c.author_ref, c.repo, c.scope, c.key, c.content, c.context,
	c.provenance, c.source_job, c.first_confirmed_at, c.updated_at
FROM confirmed_memories_fts f
JOIN confirmed_memories c ON c.id = f.rowid
WHERE f.confirmed_memories_fts MATCH ?
	AND ((c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ?) OR (c.owner_kind = ? AND c.owner_ref = ?))
	AND (c.scope = 'general' OR c.repo = ?)
	AND c.superseded_by IS NULL
	AND c.retired_at = ''
ORDER BY bm25(f.confirmed_memories_fts),
	CASE WHEN c.owner_kind = ? AND c.owner_ref = ? AND c.owner_version = ? THEN 0 ELSE 1 END,
	c.id
LIMIT ?`,
		matchQuery, owner.Kind, owner.Ref, owner.Version, memoryOwnerKindShared, memorySharedOwnerRef, repo,
		owner.Kind, owner.Ref, owner.Version, limit)
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
		where = append(where, "(owner_ref = ? OR author_ref = ?)")
		args = append(args, ownerRef, ownerRef)
	}
	if strings.TrimSpace(repo) != "" {
		where = append(where, "(repo = ? OR scope = 'general')")
		args = append(args, repo)
	}
	query := `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
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

// ListActiveConfirmedMemoriesByProvenancePrefix returns active confirmed rows for
// one owner, repo, and provenance prefix. It is a targeted non-FTS read for
// deterministic producers that need to reason over confirmed facts without
// surfacing retired or superseded rows.
func (s *Store) ListActiveConfirmedMemoriesByProvenancePrefix(ctx context.Context, owner MemoryOwner, repo, provenancePrefix string, limit int) ([]ConfirmedMemory, error) {
	if strings.TrimSpace(owner.Kind) == "" || strings.TrimSpace(owner.Ref) == "" || strings.TrimSpace(provenancePrefix) == "" {
		return nil, nil
	}
	query := `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content, context,
	provenance, source_job, first_confirmed_at, updated_at
FROM confirmed_memories
WHERE owner_kind = ? AND owner_ref = ? AND owner_version = ?
	AND ((? IS NULL AND repo IS NULL) OR repo = ?)
	AND provenance LIKE ? ESCAPE '\'
	AND superseded_by IS NULL
	AND retired_at = ''
ORDER BY updated_at DESC, id DESC`
	args := []any{owner.Kind, owner.Ref, owner.Version, nullableRepo(repo), nullableRepo(repo), likePrefix(provenancePrefix)}
	if limit > 0 {
		query += "\nLIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list active confirmed memories by provenance: %w", err)
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
		where = append(where, "(owner_ref = ? OR author_ref = ?)")
		args = append(args, ownerRef, ownerRef)
	}
	if strings.TrimSpace(repo) != "" {
		where = append(where, "(repo = ? OR scope = 'general')")
		args = append(args, repo)
	}
	query := `
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content,
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
		if err := rows.Scan(&o.ID, &o.Owner.Kind, &o.Owner.Ref, &o.Owner.Version, &o.AuthorRef, &repoNull,
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
		if err := rows.Scan(&c.ID, &c.Owner.Kind, &c.Owner.Ref, &c.Owner.Version, &c.AuthorRef, &repoNull,
			&c.Scope, &c.Key, &c.Content, &c.Context, &c.Provenance, &c.SourceJob, &c.FirstConfirmedAt, &c.UpdatedAt); err != nil {
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
		where = append(where, "(o.owner_ref = ? OR o.author_ref = ?)")
		args = append(args, ownerRef, ownerRef)
	}
	if strings.TrimSpace(provenancePrefix) != "" {
		where = append(where, "o.provenance LIKE ? ESCAPE '\\'")
		args = append(args, likePrefix(provenancePrefix))
	}
	query := `
SELECT o.id, o.owner_kind, o.owner_ref, o.owner_version, o.author_ref, o.repo, o.scope, o.key,
	o.content, o.provenance, o.trust_mark, o.source_job, o.created_at,
	EXISTS(
		SELECT 1 FROM confirmed_memories c
		WHERE ((
				c.owner_kind = o.owner_kind AND c.owner_ref = o.owner_ref
					AND c.owner_version = o.owner_version
			) OR (
				c.owner_kind = 'shared'
					AND (CASE WHEN c.author_ref <> '' THEN c.author_ref ELSE c.owner_ref END) =
						(CASE WHEN o.author_ref <> '' THEN o.author_ref ELSE o.owner_ref END)
			))
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
		if err := rows.Scan(&o.ID, &o.Owner.Kind, &o.Owner.Ref, &o.Owner.Version, &o.AuthorRef, &repoNull,
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
SELECT id, owner_kind, owner_ref, owner_version, author_ref, repo, scope, key, content,
	provenance, trust_mark, source_job, created_at
FROM memory_observations WHERE id = ? AND provenance NOT LIKE ? ESCAPE '\'`,
		id, likePrefix(MemoryDistillWitnessProvenancePrefix)).Scan(
		&o.ID, &o.Owner.Kind, &o.Owner.Ref, &o.Owner.Version, &o.AuthorRef, &repoNull,
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
