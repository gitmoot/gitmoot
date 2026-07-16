package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryEventsMigrationFreshAndUpgradeConverge(t *testing.T) {
	ctx := context.Background()
	fresh := openMemTestStore(t)
	for _, name := range []string{"memory_events", "idx_memory_events_at", "idx_memory_events_memory_id"} {
		if ok, err := fresh.tableExists(ctx, name); err != nil || !ok {
			t.Fatalf("fresh %s exists=%v err=%v", name, ok, err)
		}
	}

	path := filepath.Join(t.TempDir(), "upgrade.db")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	version := 0
	for i, migration := range migrations {
		if strings.Contains(migration, "CREATE TABLE memory_events") {
			version = i + 1
			break
		}
	}
	if version == 0 {
		t.Fatal("memory_events migration not found")
	}
	if _, err := legacy.db.Exec(`DROP TABLE memory_events; DELETE FROM schema_migrations WHERE version = ?`, version); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer upgraded.Close()
	if ok, err := upgraded.tableExists(ctx, "memory_events"); err != nil || !ok {
		t.Fatalf("upgraded memory_events exists=%v err=%v", ok, err)
	}
}

func TestMemoryEventMutationLifecycleAndBeforeContent(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := MemoryOwner{Kind: "agent", Ref: "builder"}
	id, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "runner", Content: "before small", SourceJob: "job-create"},
		WithConfirmedMemoryEvent(MemoryEventConfirmed, "job-create"))
	if err != nil {
		t.Fatal(err)
	}
	assertMemoryEventKinds(t, store, id, MemoryEventConfirmed)

	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "runner", Content: "after small", SourceJob: "job-update"}); err != nil {
		t.Fatal(err)
	}
	events, _ := store.ListMemoryEvents(ctx, MemoryEventFilter{MemoryID: id, OldestFirst: true, Limit: 20})
	if len(events) != 2 || events[1].Kind != MemoryEventUpdated || events[1].Actor != "job-update" {
		t.Fatalf("events after update = %+v", events)
	}
	var small map[string]any
	if err := json.Unmarshal([]byte(events[1].Detail), &small); err != nil || small["before"] != "before small" {
		t.Fatalf("small before detail = %s err=%v", events[1].Detail, err)
	}

	large := strings.Repeat("é", 1100)
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "runner", Content: large, SourceJob: "job-large-seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "runner", Content: "after large", SourceJob: "job-large-update"}); err != nil {
		t.Fatal(err)
	}
	events, _ = store.ListMemoryEvents(ctx, MemoryEventFilter{MemoryID: id, Limit: 1})
	var truncated struct {
		BeforeHash    string `json:"before_hash"`
		BeforePreview string `json:"before_preview"`
		Truncated     bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(events[0].Detail), &truncated); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(large))
	if !truncated.Truncated || truncated.BeforeHash != hex.EncodeToString(sum[:]) || len([]rune(truncated.BeforePreview)) != 300 {
		t.Fatalf("large before detail = %+v", truncated)
	}

	if err := store.RetireConfirmedMemory(ctx, id, "operator cleanup", "cli:memory-retire"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "runner", Content: "restored"}, AllowResurrectConfirmedMemory(),
		WithConfirmedMemoryEvent(MemoryEventConfirmed, "cli:memory-confirm")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PromoteConfirmedMemoriesToShared(ctx, []int64{id}, "cli:memory-promote"); err != nil {
		t.Fatal(err)
	}
	assertMemoryEventKinds(t, store, id, MemoryEventConfirmed, MemoryEventUpdated, MemoryEventUpdated,
		MemoryEventUpdated, MemoryEventRetired, MemoryEventUnretired, MemoryEventPromoted)
}

func TestMemoryEventRollbackFollowsMutationTransaction(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	id, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{Owner: agentOwner("builder"), Key: "atomic", Content: "before"})
	if err != nil {
		t.Fatal(err)
	}
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM confirmed_memories WHERE id = ?`, id).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	err = store.ApplyVaultImport(ctx, VaultImportPlan{
		Updates:      []VaultImportUpdate{{ID: id, ExpectedUpdatedAt: updatedAt, Content: "must roll back", Provenance: "vault-import"}},
		Observations: []MemoryObservation{{}},
	})
	if err == nil {
		t.Fatal("ApplyVaultImport unexpectedly succeeded")
	}
	var content string
	if err := store.db.QueryRowContext(ctx, `SELECT content FROM confirmed_memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "before" {
		t.Fatalf("content committed despite rollback: %q", content)
	}
	events, err := store.ListMemoryEvents(ctx, MemoryEventFilter{MemoryID: id, Limit: 20})
	if err != nil || len(events) != 1 {
		t.Fatalf("events after rollback = %+v err=%v", events, err)
	}
}

func TestMemoryEventBackfillLiveShapeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	active := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "active", Content: "active"})
	retired := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "retired", Content: "retired"})
	superseded := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "superseded", Content: "superseded"})
	if _, err := store.db.ExecContext(ctx, `
UPDATE confirmed_memories SET retired_at='2026-07-15T00:00:00Z', retired_reason='stale fact' WHERE id=?;
UPDATE confirmed_memories SET superseded_by=? WHERE id=?;
DELETE FROM memory_events`, retired, active, superseded); err != nil {
		t.Fatal(err)
	}
	dry, err := store.BackfillMemoryEvents(ctx, true)
	if err != nil || dry.Created != 5 {
		t.Fatalf("dry backfill = %+v err=%v", dry, err)
	}
	var persisted int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_events`).Scan(&persisted); err != nil || persisted != 0 {
		t.Fatalf("dry-run persisted=%d err=%v", persisted, err)
	}
	first, err := store.BackfillMemoryEvents(ctx, false)
	if err != nil || first.Created != 5 {
		t.Fatalf("first backfill = %+v err=%v", first, err)
	}
	second, err := store.BackfillMemoryEvents(ctx, false)
	if err != nil || second.Created != 0 || second.Skipped != 3 {
		t.Fatalf("second backfill = %+v err=%v", second, err)
	}
	events, err := store.ListMemoryEvents(ctx, MemoryEventFilter{Limit: 20})
	if err != nil || len(events) != 5 {
		t.Fatalf("backfilled events=%d err=%v", len(events), err)
	}
	for _, event := range events {
		if !strings.Contains(event.Detail, `"backfilled":true`) {
			t.Fatalf("event missing backfilled marker: %+v", event)
		}
	}
}

// The realistic upgrade shape the first version of this test masked with a
// blanket DELETE: rows written AFTER the journal shipped already carry live
// events (with kinds like ingested/confirmed — never 'created') and live
// tombstone receipts. Backfill must not fabricate duplicate births or
// duplicate retired/superseded receipts for them, while still synthesizing
// the missing halves for pre-journal rows.
func TestMemoryEventBackfillMixedLiveHistory(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)

	// Pre-journal rows: born before the feature (journal wiped for them only).
	preActive := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "pre-active", Content: "a"})
	preRetiredLive := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "pre-retired-live", Content: "b"})
	if _, err := store.db.ExecContext(ctx, `DELETE FROM memory_events`); err != nil {
		t.Fatal(err)
	}
	// preRetiredLive is retired AFTER the journal shipped: live retired event.
	if err := store.RetireConfirmedMemory(ctx, preRetiredLive, "live retire", "cli-test"); err != nil {
		t.Fatal(err)
	}

	// Post-journal row: full live history from birth (ingest-style callers
	// record births as confirmed/ingested — never 'created').
	postLive, err := store.UpsertConfirmedMemory(ctx,
		ConfirmedMemory{Owner: agentOwner("builder"), Key: "post-live", Content: "c"},
		WithConfirmedMemoryEvent(MemoryEventConfirmed, "cli-test"))
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.BackfillMemoryEvents(ctx, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// Synthesized: created for preActive, created for preRetiredLive (birth
	// predates journal) — but NOT a second retired for preRetiredLive, and
	// NOTHING for postLive.
	if first.Created != 2 {
		t.Fatalf("first mixed backfill created=%d, want 2 (%+v)", first.Created, first)
	}
	assertEventCount := func(id int64, kind string, want int) {
		t.Helper()
		var got int
		if err := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memory_events WHERE memory_id = ? AND kind = ?`, id, kind).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("memory %d kind %s events=%d, want %d", id, kind, got, want)
		}
	}
	assertEventCount(preActive, MemoryEventCreated, 1)
	assertEventCount(preRetiredLive, MemoryEventCreated, 1)
	assertEventCount(preRetiredLive, MemoryEventRetired, 1) // the LIVE one only
	assertEventCount(postLive, MemoryEventCreated, 0)       // birth receipt is 'confirmed'
	assertEventCount(postLive, MemoryEventConfirmed, 1)

	second, err := store.BackfillMemoryEvents(ctx, false)
	if err != nil || second.Created != 0 {
		t.Fatalf("second mixed backfill = %+v err=%v, want fully idempotent", second, err)
	}
}

func TestMemoryEventsCoverSplitRevertAndClusterMutations(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	parentID := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "parent", Content: "alpha beta"})
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM confirmed_memories WHERE id=?`, parentID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	split, err := store.ApplyGroomSplits(ctx, []GroomSplitItem{{ParentID: parentID, ExpectedUpdatedAt: updatedAt,
		Children: []GroomSplitChild{{Key: "parent-alpha", Content: "alpha "}, {Key: "parent-beta", Content: "beta"}}}})
	if err != nil || len(split.Applied) != 1 || len(split.Applied[0].ChildIDs) != 2 {
		t.Fatalf("split=%+v err=%v", split, err)
	}
	assertMemoryEventKinds(t, store, parentID, MemoryEventCreated, MemoryEventSuperseded)
	for _, childID := range split.Applied[0].ChildIDs {
		assertMemoryEventKinds(t, store, childID, MemoryEventCreated)
	}
	reverted, err := store.RevertGroomSplits(ctx, GroomSplitRevertOptions{ParentIDs: []int64{parentID}})
	if err != nil || len(reverted.Reverted) != 1 {
		t.Fatalf("revert=%+v err=%v", reverted, err)
	}
	assertMemoryEventKinds(t, store, parentID, MemoryEventCreated, MemoryEventSuperseded, MemoryEventUnretired)
	for _, childID := range split.Applied[0].ChildIDs {
		assertMemoryEventKinds(t, store, childID, MemoryEventCreated, MemoryEventRetired)
	}

	assignment := MemoryClusterAssignment{
		Clusters: []MemoryCluster{{ClusterID: 7, Label: "shipping", MedoidID: parentID}},
		Members:  []MemoryClusterMember{{MemoryID: parentID, ClusterID: 7}},
	}
	if err := store.RecomputeMemoryClusters(ctx, assignment, "cli:memory-clusters-recompute"); err != nil {
		t.Fatal(err)
	}
	if err := store.RenameMemoryCluster(ctx, 7, "delivery", "cli:memory-cluster-rename"); err != nil {
		t.Fatal(err)
	}
	clusterEvents, err := store.ListMemoryEvents(ctx, MemoryEventFilter{Kinds: []string{MemoryEventClusterRecompute, MemoryEventClusterRename}, OldestFirst: true, Limit: 10})
	if err != nil || len(clusterEvents) != 2 || clusterEvents[0].Kind != MemoryEventClusterRecompute || clusterEvents[1].Kind != MemoryEventClusterRename {
		t.Fatalf("cluster events=%+v err=%v", clusterEvents, err)
	}
	if !strings.Contains(clusterEvents[0].Detail, `"member_count_delta":1`) || !strings.Contains(clusterEvents[1].Detail, `"label":"delivery"`) {
		t.Fatalf("cluster details=%s / %s", clusterEvents[0].Detail, clusterEvents[1].Detail)
	}
}

func TestMemoryEventsCoverVaultAndGroomMutationPaths(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	editID := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "edit", Content: "before"})
	retireID := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "retire", Content: "retire me"})
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM confirmed_memories WHERE id=?`, editID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyVaultImport(ctx, VaultImportPlan{
		Updates:     []VaultImportUpdate{{ID: editID, ExpectedUpdatedAt: updatedAt, Content: "after", Provenance: "vault-import"}},
		Retirements: []VaultImportRetire{{ID: retireID, Reason: "deleted by owner"}},
	}); err != nil {
		t.Fatal(err)
	}
	assertMemoryEventKinds(t, store, editID, MemoryEventCreated, MemoryEventUpdated)
	assertMemoryEventKinds(t, store, retireID, MemoryEventCreated, MemoryEventRetired)

	groomID := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Key: "stale", Content: "stale"})
	result, err := store.ApplyGroomRetirements(ctx, []GroomRetire{{ID: groomID, Reason: "groom-quality:2026-07-16"}})
	if err != nil || len(result.Retired) != 1 {
		t.Fatalf("groom retirement=%+v err=%v", result, err)
	}
	events, err := store.ListMemoryEvents(ctx, MemoryEventFilter{MemoryID: groomID, Limit: 1})
	// actor = stable producer identity ("groom-retire"); the free-text plan
	// reason lives in detail only so actor filtering stays meaningful.
	if err != nil || len(events) != 1 || events[0].Kind != MemoryEventRetired || events[0].Actor != "groom-retire" || !strings.Contains(events[0].Detail, "groom-quality") {
		t.Fatalf("groom event=%+v err=%v", events, err)
	}
}

func assertMemoryEventKinds(t *testing.T, store *Store, memoryID int64, want ...string) {
	t.Helper()
	events, err := store.ListMemoryEvents(context.Background(), MemoryEventFilter{MemoryID: memoryID, OldestFirst: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, event.Kind)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event kinds=%v want=%v", got, want)
	}
}
