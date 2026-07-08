package db

import (
	"context"
	"path/filepath"
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

// TestObservationContentHashesSpansBothTiers proves ingest dedup sees content in
// BOTH memory_observations and confirmed_memories for an owner.
func TestObservationContentHashesSpansBothTiers(t *testing.T) {
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

	hashes, err := store.ObservationContentHashes(ctx, "lead")
	if err != nil {
		t.Fatalf("hashes: %v", err)
	}
	if _, ok := hashes[sha256HexOf("the deploy host is the CI box")]; !ok {
		t.Fatal("observation content hash missing")
	}
	if _, ok := hashes[sha256HexOf("arm64 runners are slow")]; !ok {
		t.Fatal("confirmed content hash missing")
	}
	// A different owner's content is not in this owner's set.
	if _, ok := hashes[sha256HexOf("unrelated content")]; ok {
		t.Fatal("unexpected hash present")
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
