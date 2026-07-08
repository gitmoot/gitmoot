package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openMemTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func agentOwner(ref string) MemoryOwner {
	return MemoryOwner{Kind: "agent", Ref: ref, Version: ""}
}

// TestMemoryMigrationCreatesTables asserts the additive #626 migration created
// both tables and the FTS5 virtual table under the shipped modernc driver.
func TestMemoryMigrationCreatesTables(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	for _, name := range []string{"memory_observations", "confirmed_memories", "confirmed_memories_fts"} {
		exists, err := store.tableExists(ctx, name)
		if err != nil {
			t.Fatalf("tableExists(%q): %v", name, err)
		}
		if !exists {
			t.Fatalf("expected table %q to exist after migration", name)
		}
	}
}

func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count)
	return count > 0, err
}

// TestConfirmedMemoryFTSRoundTrip proves an upsert lands, the FTS index is
// synced, and a sanitized BM25 MATCH query retrieves it under the tiered owner
// filter.
func TestConfirmedMemoryFTSRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")

	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "ci-flake", Content: "arm64 CI is flaky and often needs a rerun",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.QueryConfirmedMemories(ctx, owner, "acme/widget", `"arm64" OR "flaky"`, 15)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 confirmed memory, got %d", len(got))
	}
	if got[0].Key != "ci-flake" {
		t.Fatalf("want key ci-flake, got %q", got[0].Key)
	}
}

// TestConfirmedMemoryUpsertKeyed proves the keyed row deduplicates: two upserts
// on the same (owner, repo, key) leave one row with the latest content.
func TestConfirmedMemoryUpsertKeyed(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	for _, content := range []string{"first", "second latest content"} {
		if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
			Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k", Content: content,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	rows, err := store.ListConfirmedMemories(ctx, "builder", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 keyed row, got %d", len(rows))
	}
	if rows[0].Content != "second latest content" {
		t.Fatalf("want latest content, got %q", rows[0].Content)
	}
}

// TestConfirmedMemoryRepoNullPartialIndex proves a general-scope fact (repo
// NULL) and a repo-scoped fact with the SAME key coexist under the partial
// unique indexes, and that the general one repeats-upserts to a single row.
func TestConfirmedMemoryRepoNullPartialIndex(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")

	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "", Scope: "general", Key: "k", Content: "general fact",
	}); err != nil {
		t.Fatalf("upsert general: %v", err)
	}
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k", Content: "repo fact",
	}); err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	// Repeat the general upsert: must not create a second general row.
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "", Scope: "general", Key: "k", Content: "general fact v2",
	}); err != nil {
		t.Fatalf("re-upsert general: %v", err)
	}
	rows, err := store.ListConfirmedMemories(ctx, "builder", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (one general, one repo), got %d", len(rows))
	}
}

// TestQueryConfirmedTierFilterExcludesOtherRepo proves the retrieval default:
// a repo-scoped fact for repo A must NOT surface when querying repo B, but a
// general fact must. This is the tier/scope filter whose mutation the E2E breaks.
func TestQueryConfirmedTierFilterExcludesOtherRepo(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")

	mustUpsert(t, store, ConfirmedMemory{Owner: owner, Repo: "acme/a", Scope: "repo", Key: "ka", Content: "alpha fact about widgets"})
	mustUpsert(t, store, ConfirmedMemory{Owner: owner, Repo: "acme/b", Scope: "repo", Key: "kb", Content: "beta fact about widgets"})
	mustUpsert(t, store, ConfirmedMemory{Owner: owner, Repo: "", Scope: "general", Key: "kg", Content: "general fact about widgets"})

	got, err := store.QueryConfirmedMemories(ctx, owner, "acme/a", `"widgets"`, 15)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	keys := map[string]bool{}
	for _, c := range got {
		keys[c.Key] = true
	}
	if !keys["ka"] {
		t.Fatalf("want repo-A fact ka in results, got %v", keys)
	}
	if keys["kb"] {
		t.Fatalf("repo-B fact kb must NOT leak into repo-A retrieval, got %v", keys)
	}
	if !keys["kg"] {
		t.Fatalf("want general fact kg to travel into repo-A retrieval, got %v", keys)
	}
}

// TestObservationsAppendNotUpsert proves repeated observations of the same key
// accumulate distinct rows (witness counting), unlike the confirmed table.
func TestObservationsAppendNotUpsert(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	for i := 0; i < 3; i++ {
		if _, err := store.InsertMemoryObservation(ctx, MemoryObservation{
			Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k",
			Content: "same fact seen again", TrustMark: "normal", SourceJob: "job-1",
		}); err != nil {
			t.Fatalf("insert observation: %v", err)
		}
	}
	n, err := store.CountMemoryObservationsForKey(ctx, owner, "acme/widget", "k")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 appended observations, got %d", n)
	}
}

// TestQueryConfirmedIgnoresSuperseded proves a superseded row never surfaces in
// retrieval (supersession, not deletion — history survives).
func TestQueryConfirmedIgnoresSuperseded(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	id := mustUpsert(t, store, ConfirmedMemory{Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k", Content: "old flaky note about widgets"})
	if _, err := store.db.ExecContext(ctx, `UPDATE confirmed_memories SET superseded_by = 999 WHERE id = ?`, id); err != nil {
		t.Fatalf("mark superseded: %v", err)
	}
	got, err := store.QueryConfirmedMemories(ctx, owner, "acme/widget", `"widgets"`, 15)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("superseded row must not surface, got %d", len(got))
	}
}

func mustUpsert(t *testing.T, store *Store, cm ConfirmedMemory) int64 {
	t.Helper()
	id, err := store.UpsertConfirmedMemory(context.Background(), cm)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return id
}

// TestUpdateConfirmedMemoryByIDCAS proves the optimistic CAS: a correct
// expected updated_at applies the content edit and resyncs FTS, while a stale
// expected updated_at is refused with an id-naming error and writes nothing.
func TestUpdateConfirmedMemoryByIDCAS(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	id, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "ci-flake",
		Content: "arm64 CI is flaky",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := store.ListConfirmedMemories(ctx, "builder", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v (rows=%d)", err, len(rows))
	}
	current := rows[0].UpdatedAt

	// Stale CAS: wrong expected updated_at → refused, names the id, no write.
	err = store.UpdateConfirmedMemoryByID(ctx, id, "1999-01-01T00:00:00Z", "clobbered", "vault-import")
	if err == nil {
		t.Fatal("expected CAS conflict error for a stale expected updated_at")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", id)) {
		t.Fatalf("CAS error must name the id %d, got: %v", id, err)
	}
	rows, _ = store.ListConfirmedMemories(ctx, "builder", "")
	if rows[0].Content != "arm64 CI is flaky" {
		t.Fatalf("stale CAS must not write; content = %q", rows[0].Content)
	}

	// Correct CAS: applies the edit and the new content is FTS-searchable while the
	// old token is gone.
	if err := store.UpdateConfirmedMemoryByID(ctx, id, current, "the runner now uses graviton", "vault-import"); err != nil {
		t.Fatalf("CAS update: %v", err)
	}
	got, err := store.QueryConfirmedMemories(ctx, owner, "acme/widget", `"graviton"`, 15)
	if err != nil {
		t.Fatalf("query new token: %v", err)
	}
	if len(got) != 1 || got[0].Content != "the runner now uses graviton" {
		t.Fatalf("edited content not FTS-searchable: %+v", got)
	}
	stale, err := store.QueryConfirmedMemories(ctx, owner, "acme/widget", `"flaky"`, 15)
	if err != nil {
		t.Fatalf("query old token: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("old content token must be gone from FTS, got %d rows", len(stale))
	}
}

// TestUpdateConfirmedMemoryByIDNotFound reports a missing id distinctly from a CAS
// conflict.
func TestUpdateConfirmedMemoryByIDNotFound(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	err := store.UpdateConfirmedMemoryByID(ctx, 999, "whenever", "x", "vault-import")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got: %v", err)
	}
}

// TestRetireConfirmedMemory proves retirement removes a fact from BOTH the FTS
// injection query and the vault-export lister, without deleting the audit row.
func TestRetireConfirmedMemory(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	id, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "ci-flake",
		Content: "arm64 CI is flaky",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := store.RetireConfirmedMemory(ctx, id, "vault-import: deleted by owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// Injection query no longer surfaces it (FTS rowid deleted + retired filter).
	got, err := store.QueryConfirmedMemories(ctx, owner, "acme/widget", `"arm64" OR "flaky"`, 15)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("retired memory must not be injected, got %d rows", len(got))
	}

	// Vault-export lister excludes it too.
	vaultRows, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("vault list: %v", err)
	}
	if len(vaultRows) != 0 {
		t.Fatalf("retired memory must not appear in the vault export, got %d rows", len(vaultRows))
	}

	// A second retire is a distinct, id-naming error (already retired).
	if err := store.RetireConfirmedMemory(ctx, id, "again"); err == nil || !strings.Contains(err.Error(), "already retired") {
		t.Fatalf("want already-retired error, got: %v", err)
	}
}

// TestRetiredExcludedFromCountAndGraph proves retirement stays consistent across
// every read path: retiring one of two confirmed rows drops the injectable count to
// 1 (CountConfirmedMemoriesForOwner) AND removes the row from the brain-graph lister
// (ListConfirmedMemoriesByOwnerKind), so the documented "count == injectable set"
// invariant holds and the graph never draws a retired fact as a live node.
func TestRetiredExcludedFromCountAndGraph(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	keepID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "keep", Content: "keep me",
	})
	if err != nil {
		t.Fatalf("upsert keep: %v", err)
	}
	dropID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "drop", Content: "drop me",
	})
	if err != nil {
		t.Fatalf("upsert drop: %v", err)
	}

	if n, err := store.CountConfirmedMemoriesForOwner(ctx, "agent", "builder"); err != nil || n != 2 {
		t.Fatalf("pre-retire count = %d (err %v), want 2", n, err)
	}

	if err := store.RetireConfirmedMemory(ctx, dropID, "vault-import: deleted by owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	if n, err := store.CountConfirmedMemoriesForOwner(ctx, "agent", "builder"); err != nil || n != 1 {
		t.Fatalf("post-retire count = %d (err %v), want 1 (must equal injectable set)", n, err)
	}
	graph, err := store.ListConfirmedMemoriesByOwnerKind(ctx, "agent")
	if err != nil {
		t.Fatalf("list by owner kind: %v", err)
	}
	if len(graph) != 1 || graph[0].ID != keepID {
		t.Fatalf("brain-graph lister must exclude the retired row, got %+v", graph)
	}
	_ = dropID
}

// TestReconfirmRetiredKeyReactivates proves a fresh confirmation of a key that was
// previously retired clears retired_at, so the re-admitted fact is injectable and
// exportable again instead of staying permanently hidden behind the stale retirement.
func TestReconfirmRetiredKeyReactivates(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	id, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "ci-flake", Content: "arm64 CI is flaky",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.RetireConfirmedMemory(ctx, id, "vault-import: deleted by owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// Re-confirm the same key (the confirmation gate re-admits the fact).
	reID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "ci-flake", Content: "arm64 CI flaky, rerun helps",
	})
	if err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	if reID != id {
		t.Fatalf("re-confirm must reuse the keyed row (%d), got %d", id, reID)
	}

	// It is injectable again (retired_at cleared) and re-exports.
	got, err := store.QueryConfirmedMemories(ctx, owner, "acme/widget", `"arm64" OR "flaky"`, 15)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].Content != "arm64 CI flaky, rerun helps" {
		t.Fatalf("re-confirmed fact must be injectable with new content, got %+v", got)
	}
	vaultRows, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("vault list: %v", err)
	}
	if len(vaultRows) != 1 {
		t.Fatalf("re-confirmed fact must re-export, got %d rows", len(vaultRows))
	}
}

// TestRetireCASGuardsConcurrentWrite proves a retirement carrying an ExpectedUpdatedAt
// aborts (rolling back the whole batch) when the row was rewritten since the fresh
// export observed it, so a plan→apply-window write can never be buried by a retirement.
func TestRetireCASGuardsConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	id, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "drop", Content: "original",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// A retirement whose expected updated_at does not match the current row must fail
	// wholesale (stale CAS), and the fact must remain live.
	plan := VaultImportPlan{
		Retirements: []VaultImportRetire{{ID: id, ExpectedUpdatedAt: "1999-01-01T00:00:00Z", Reason: "vault-import: deleted by owner"}},
	}
	if err := store.ApplyVaultImport(ctx, plan); err == nil || !strings.Contains(err.Error(), "changed since export") {
		t.Fatalf("want changed-since-export CAS failure, got: %v", err)
	}
	vaultRows, _ := store.ListConfirmedMemoriesForVault(ctx, "")
	if len(vaultRows) != 1 {
		t.Fatalf("stale retirement must not land: want 1 live row, got %d", len(vaultRows))
	}
}

// TestApplyVaultImportAtomic proves a whole import plan commits together and that a
// failing element (a stale CAS) rolls the ENTIRE batch back — no partial curation.
func TestApplyVaultImportAtomic(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")
	keepID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "keep", Content: "original keep",
	})
	if err != nil {
		t.Fatalf("upsert keep: %v", err)
	}
	dropID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "drop", Content: "original drop",
	})
	if err != nil {
		t.Fatalf("upsert drop: %v", err)
	}

	// A plan whose update carries a stale expected updated_at must fail wholesale:
	// the retirement of dropID must NOT land.
	badPlan := VaultImportPlan{
		Updates:     []VaultImportUpdate{{ID: keepID, ExpectedUpdatedAt: "1999-01-01T00:00:00Z", Content: "edited", Provenance: "vault-import"}},
		Retirements: []VaultImportRetire{{ID: dropID, Reason: "vault-import: deleted by owner"}},
	}
	if err := store.ApplyVaultImport(ctx, badPlan); err == nil {
		t.Fatal("expected the stale-CAS plan to fail")
	}
	vaultRows, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("vault list: %v", err)
	}
	if len(vaultRows) != 2 {
		t.Fatalf("rollback failed: want 2 live rows, got %d", len(vaultRows))
	}

	// A well-formed plan applies edit + retire + observation together.
	rows, _ := store.ListConfirmedMemories(ctx, "builder", "")
	var keepUpdatedAt string
	for _, r := range rows {
		if r.ID == keepID {
			keepUpdatedAt = r.UpdatedAt
		}
	}
	goodPlan := VaultImportPlan{
		Updates:      []VaultImportUpdate{{ID: keepID, ExpectedUpdatedAt: keepUpdatedAt, Content: "edited keep", Provenance: "vault-import"}},
		Retirements:  []VaultImportRetire{{ID: dropID, Reason: "vault-import: deleted by owner"}},
		Observations: []MemoryObservation{{Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "new", Content: "a new note", Provenance: "vault-import:new.md", TrustMark: "normal"}},
	}
	if err := store.ApplyVaultImport(ctx, goodPlan); err != nil {
		t.Fatalf("apply good plan: %v", err)
	}
	vaultRows, _ = store.ListConfirmedMemoriesForVault(ctx, "")
	if len(vaultRows) != 1 || vaultRows[0].ID != keepID || vaultRows[0].Content != "edited keep" {
		t.Fatalf("post-apply vault state wrong: %+v", vaultRows)
	}
	obsCount, err := store.CountMemoryObservationsForOwner(ctx, "agent", "builder")
	if err != nil {
		t.Fatalf("count obs: %v", err)
	}
	if obsCount != 1 {
		t.Fatalf("want 1 staged observation, got %d", obsCount)
	}
}

// TestObservationDedupKeysSpansBothTiers proves ingest dedup sees content in
// BOTH memory_observations and confirmed_memories for an owner, keyed by the
// (scope, repo, content-hash) visibility domain.
func TestObservationDedupKeysSpansBothTiers(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")

	if _, err := store.InsertMemoryObservation(ctx, MemoryObservation{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "obs-a",
		Content: "the deploy host is the CI box", TrustMark: "low",
	}); err != nil {
		t.Fatalf("insert obs: %v", err)
	}
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "conf-b",
		Content: "arm64 runners are slow", Provenance: "test",
	}); err != nil {
		t.Fatalf("upsert confirmed: %v", err)
	}

	keys, err := store.ObservationDedupKeys(ctx, "lead")
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if _, ok := keys[MemoryDedupKey("repo", "acme/widget", sha256HexOf("the deploy host is the CI box"))]; !ok {
		t.Fatal("observation dedup key missing")
	}
	if _, ok := keys[MemoryDedupKey("repo", "acme/widget", sha256HexOf("arm64 runners are slow"))]; !ok {
		t.Fatal("confirmed dedup key missing")
	}
	// A different owner's content is not in this owner's set.
	if _, ok := keys[MemoryDedupKey("repo", "acme/widget", sha256HexOf("unrelated content"))]; ok {
		t.Fatal("unexpected key present")
	}
}

// TestObservationDedupKeysDomainScoped proves identical content under a DIFFERENT
// repo is NOT treated as a duplicate: repo-scoped memory injects only for its own
// repo, so the second repo must be able to stage the same note. Regression guard
// for the owner-only dedup that silently dropped cross-repo re-ingests.
func TestObservationDedupKeysDomainScoped(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")
	const content = "the deploy host is the CI box"

	if _, err := store.InsertMemoryObservation(ctx, MemoryObservation{
		Owner: owner, Repo: "org/a", Scope: "repo", Key: "obs-a",
		Content: content, TrustMark: "low",
	}); err != nil {
		t.Fatalf("insert obs: %v", err)
	}

	keys, err := store.ObservationDedupKeys(ctx, "lead")
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if _, ok := keys[MemoryDedupKey("repo", "org/a", sha256HexOf(content))]; !ok {
		t.Fatal("org/a dedup key missing")
	}
	// Same content, second repo: must NOT collide, so ingest still stages it.
	if _, ok := keys[MemoryDedupKey("repo", "org/b", sha256HexOf(content))]; ok {
		t.Fatal("org/b must not be deduped against org/a for identical content")
	}
}

// TestListMemoryObservationsWithConfirmationFlagsConfirmedKeys proves the join
// flags exactly the observations whose owner+repo+key already exists confirmed,
// and that the provenance-prefix filter is a literal (wildcard-safe) prefix.
func TestListMemoryObservationsWithConfirmationFlagsConfirmedKeys(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")

	if _, err := store.InsertMemoryObservation(ctx, MemoryObservation{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k-confirmed",
		Content: "already promoted", Provenance: "ingest:notes/a.md", TrustMark: "low",
	}); err != nil {
		t.Fatalf("insert obs a: %v", err)
	}
	if _, err := store.InsertMemoryObservation(ctx, MemoryObservation{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k-pending",
		Content: "still pending", Provenance: "ingest:notes/b.md", TrustMark: "low",
	}); err != nil {
		t.Fatalf("insert obs b: %v", err)
	}
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k-confirmed",
		Content: "already promoted", Provenance: "ingest:notes/a.md",
	}); err != nil {
		t.Fatalf("confirm a: %v", err)
	}

	rows, err := store.ListMemoryObservationsWithConfirmation(ctx, "lead", "ingest:")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 ingest observations, got %d", len(rows))
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Key] = r.Confirmed
	}
	if !got["k-confirmed"] {
		t.Fatal("k-confirmed should be flagged confirmed")
	}
	if got["k-pending"] {
		t.Fatal("k-pending should NOT be flagged confirmed")
	}

	// Prefix with a LIKE metacharacter matches nothing (escaped literal).
	none, err := store.ListMemoryObservationsWithConfirmation(ctx, "lead", "ingest:%")
	if err != nil {
		t.Fatalf("list escaped: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("literal %%-prefix should match no rows, got %d", len(none))
	}
}

// TestDistillWitnessExcludedFromListAndConfirm proves the #737 P4.1 recurrence
// witness is INTERNAL bookkeeping: a distill-seen: row is invisible on every
// human-reviewable surface (ListMemoryObservations, ListMemoryObservationsWithConfirmation)
// and is un-fetchable by GetMemoryObservationByID, so it can never be shown by
// `memory list` nor promoted to confirmed memory by `memory confirm`. A genuine
// staged distill: row alongside it stays fully visible/confirmable.
func TestDistillWitnessExcludedFromListAndConfirm(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("audit")

	witnessID, err := store.InsertMemoryObservation(ctx, MemoryObservation{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "distill-test:testx",
		Content: "A failure signal was observed once in this repository and is held pending recurrence before it is recorded.",
		Provenance: MemoryDistillWitnessProvenancePrefix + "job-1", TrustMark: "low",
	})
	if err != nil {
		t.Fatalf("insert witness: %v", err)
	}
	stagedID, err := store.InsertMemoryObservation(ctx, MemoryObservation{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "distill-test:testy",
		Content: "Test testy FAILED in a implement job in this repository.",
		Provenance: "distill:job-2", TrustMark: "low",
	})
	if err != nil {
		t.Fatalf("insert staged: %v", err)
	}

	// ListMemoryObservations: only the staged row is visible.
	pending, err := store.ListMemoryObservations(ctx, "audit", "acme/widget")
	if err != nil {
		t.Fatalf("list observations: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != stagedID {
		t.Fatalf("pending list should show only the staged row, got %+v", pending)
	}

	// ListMemoryObservationsWithConfirmation: witness hidden even with no prefix.
	rows, err := store.ListMemoryObservationsWithConfirmation(ctx, "audit", "")
	if err != nil {
		t.Fatalf("list w/ confirmation: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != stagedID {
		t.Fatalf("confirm surface should show only the staged row, got %+v", rows)
	}

	// A caller cannot target witnesses by their own provenance prefix.
	byPrefix, err := store.ListMemoryObservationsWithConfirmation(ctx, "audit", MemoryDistillWitnessProvenancePrefix)
	if err != nil {
		t.Fatalf("list by witness prefix: %v", err)
	}
	if len(byPrefix) != 0 {
		t.Fatalf("--prefix distill-seen: must select nothing, got %+v", byPrefix)
	}

	// GetMemoryObservationByID: a witness id resolves to (not found); a staged id resolves.
	if _, ok, err := store.GetMemoryObservationByID(ctx, witnessID); err != nil || ok {
		t.Fatalf("witness id must be un-confirmable (not found), ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.GetMemoryObservationByID(ctx, stagedID); err != nil || !ok {
		t.Fatalf("staged id must remain confirmable, ok=%v err=%v", ok, err)
	}

	// The witness DID persist (recurrence counting still sees it).
	if n, err := store.CountMemoryObservationsForKey(ctx, owner, "acme/widget", "distill-test:testx"); err != nil || n != 1 {
		t.Fatalf("witness must persist for recurrence counting, n=%d err=%v", n, err)
	}
}
