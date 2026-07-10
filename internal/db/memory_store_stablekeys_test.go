package db

// Tests for the #804 store changes: supersede-preserving keyed updates, the
// active-row partial unique indexes, the groom plan apply (rekey + cross-pool),
// and the cross-pool secondary-signal read.

import (
	"context"
	"testing"
)

func activeConfirmedByID(t *testing.T, store *Store, id int64) (content, key, retiredAt, retiredReason string) {
	t.Helper()
	err := store.db.QueryRowContext(context.Background(), `
SELECT content, key, retired_at, retired_reason FROM confirmed_memories WHERE id = ?`, id).
		Scan(&content, &key, &retiredAt, &retiredReason)
	if err != nil {
		t.Fatalf("read confirmed %d: %v", id, err)
	}
	return content, key, retiredAt, retiredReason
}

func supersededRowsFor(t *testing.T, store *Store, liveID int64) []ConfirmedMemory {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(), `
SELECT id, key, content FROM confirmed_memories WHERE superseded_by = ? ORDER BY id`, liveID)
	if err != nil {
		t.Fatalf("query superseded rows: %v", err)
	}
	defer rows.Close()
	var out []ConfirmedMemory
	for rows.Next() {
		var c ConfirmedMemory
		if err := rows.Scan(&c.ID, &c.Key, &c.Content); err != nil {
			t.Fatalf("scan superseded row: %v", err)
		}
		out = append(out, c)
	}
	return out
}

func ftsRowCount(t *testing.T, store *Store, id int64) int {
	t.Helper()
	var n int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM confirmed_memories_fts WHERE rowid = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count fts rows for %d: %v", id, err)
	}
	return n
}

// TestUpsertPreserveSupersededEditionArchivesPrior is the #804 poisoned-edit
// hardening contract: an auto-confirmed key-matched update keeps the live row id
// (memory_links intact), overwrites its content, and preserves the prior edition
// as a superseded_by row that is out of FTS and out of the vault.
func TestUpsertPreserveSupersededEditionArchivesPrior(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")

	liveID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "runbook-deploy", Content: "edition one of the deploy runbook fact",
	})
	if err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	otherID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "other-fact", Content: "an unrelated fact used as a link target",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT OR IGNORE INTO memory_links (src_id, dst_id, score, origin, created_at)
VALUES (?, ?, 1.0, 'auto', '2026-01-01T00:00:00Z')`, liveID, otherID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	updatedID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "runbook-deploy", Content: "edition two after the note was edited",
	}, PreserveSupersededEdition())
	if err != nil {
		t.Fatalf("preserve-edition update: %v", err)
	}
	if updatedID != liveID {
		t.Fatalf("key-matched update must keep the live row id: got %d want %d", updatedID, liveID)
	}

	content, key, _, _ := activeConfirmedByID(t, store, liveID)
	if content != "edition two after the note was edited" || key != "runbook-deploy" {
		t.Fatalf("live row not updated in place: content=%q key=%q", content, key)
	}
	archived := supersededRowsFor(t, store, liveID)
	if len(archived) != 1 {
		t.Fatalf("expected exactly one archived edition, got %d", len(archived))
	}
	if archived[0].Content != "edition one of the deploy runbook fact" || archived[0].Key != "runbook-deploy" {
		t.Fatalf("archived edition wrong: %+v", archived[0])
	}
	if n := ftsRowCount(t, store, archived[0].ID); n != 0 {
		t.Fatalf("archived edition must not be in FTS, found %d rows", n)
	}

	// memory_links key on the live row id, which is unchanged.
	links, err := store.ListMemoryLinks(ctx, liveID)
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(links) != 1 || links[0].DstID != otherID {
		t.Fatalf("links must survive the in-place update: %+v", links)
	}

	// The vault (injectable set) contains the live row, never the archive.
	vault, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("vault list: %v", err)
	}
	for _, m := range vault {
		if m.ID == archived[0].ID {
			t.Fatalf("archived edition leaked into the vault: %+v", m)
		}
	}

	// A byte-identical update archives nothing new.
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "runbook-deploy", Content: "edition two after the note was edited",
	}, PreserveSupersededEdition()); err != nil {
		t.Fatalf("idempotent update: %v", err)
	}
	if got := supersededRowsFor(t, store, liveID); len(got) != 1 {
		t.Fatalf("byte-identical update must not archive, got %d editions", len(got))
	}
}

// TestUpsertWithoutPreserveKeepsOverwriteSemantics pins the default (and manual
// confirm) path: a keyed update still overwrites in place with NO archived row.
func TestUpsertWithoutPreserveKeepsOverwriteSemantics(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")
	id, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k", Content: "one",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k", Content: "two",
	}); err != nil {
		t.Fatalf("plain update: %v", err)
	}
	if got := supersededRowsFor(t, store, id); len(got) != 0 {
		t.Fatalf("plain upsert must not archive editions, got %d", len(got))
	}
}

// TestVaultImportCASDoesNotArchive pins the manual human path (#804 scope
// boundary): a vault-import CAS edit updates the exact row with no superseded
// archive — human curation semantics are unchanged.
func TestVaultImportCASDoesNotArchive(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")
	id, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "k", Content: "original",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM confirmed_memories WHERE id = ?`, id).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if err := store.UpdateConfirmedMemoryByID(ctx, id, updatedAt, "human edited", "vault:edit"); err != nil {
		t.Fatalf("CAS edit: %v", err)
	}
	content, _, _, _ := activeConfirmedByID(t, store, id)
	if content != "human edited" {
		t.Fatalf("CAS edit did not land: %q", content)
	}
	if got := supersededRowsFor(t, store, id); len(got) != 0 {
		t.Fatalf("vault import CAS must not archive editions, got %d", len(got))
	}
}

// TestApplyGroomPlanRekey seeds legacy hash-suffixed editions, applies a rekey
// group, and proves the keeper's key is rewritten with its FTS row re-synced in
// the same transaction while the older sibling is retired with the verbatim
// rekey reason.
func TestApplyGroomPlanRekey(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")

	oldID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "notes-deploy-a1b2c3d4", Content: "old edition of the deploy note",
	})
	if err != nil {
		t.Fatalf("seed old sibling: %v", err)
	}
	keepID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "notes-deploy-ffee0011", Content: "newest edition of the deploy note",
	})
	if err != nil {
		t.Fatalf("seed keeper: %v", err)
	}

	res, err := store.ApplyGroomPlan(ctx, nil, []GroomRekeyItem{{
		KeepID: keepID, NewKey: "notes-deploy", RetireIDs: []int64{oldID},
		Reason: "rekey: superseded edition",
	}}, nil)
	if err != nil {
		t.Fatalf("apply rekey: %v", err)
	}
	if len(res.Rekeyed) != 1 || res.Rekeyed[0] != keepID || len(res.RekeySkipped) != 0 {
		t.Fatalf("rekey result wrong: %+v", res)
	}

	_, key, retiredAt, _ := activeConfirmedByID(t, store, keepID)
	if key != "notes-deploy" || retiredAt != "" {
		t.Fatalf("keeper should be active under the stable key: key=%q retired=%q", key, retiredAt)
	}
	_, _, retiredAt, reason := activeConfirmedByID(t, store, oldID)
	if retiredAt == "" || reason != "rekey: superseded edition" {
		t.Fatalf("sibling should be retired with the rekey reason: retired=%q reason=%q", retiredAt, reason)
	}
	if n := ftsRowCount(t, store, oldID); n != 0 {
		t.Fatalf("retired sibling must leave FTS, found %d rows", n)
	}

	// FTS key column re-synced in the same tx: the raw stored key is the new one.
	var ftsKey string
	if err := store.db.QueryRowContext(ctx,
		`SELECT key FROM confirmed_memories_fts WHERE rowid = ?`, keepID).Scan(&ftsKey); err != nil {
		t.Fatalf("read fts key: %v", err)
	}
	if ftsKey != "notes-deploy" {
		t.Fatalf("FTS key column not re-synced: %q", ftsKey)
	}

	// A subsequent auto-confirm write on the stable key updates the keeper in
	// place even though a retired sibling still shares the domain.
	newID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "notes-deploy", Content: "post-rekey edited edition",
	}, PreserveSupersededEdition())
	if err != nil {
		t.Fatalf("post-rekey upsert: %v", err)
	}
	if newID != keepID {
		t.Fatalf("post-rekey upsert must hit the active keeper: got %d want %d", newID, keepID)
	}
}

// TestApplyGroomPlanRekeySkipsWhenKeeperRetired proves a rekey group whose
// keeper lost a race to an earlier retirement is skipped whole: no sibling is
// retired and nothing is rewritten.
func TestApplyGroomPlanRekeySkipsWhenKeeperRetired(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")
	oldID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "n-a1b2c3d4", Content: "sibling", Provenance: "x",
	})
	if err != nil {
		t.Fatalf("seed sibling: %v", err)
	}
	keepID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "n-ffee0011", Content: "keeper", Provenance: "x",
	})
	if err != nil {
		t.Fatalf("seed keeper: %v", err)
	}
	if err := store.RetireConfirmedMemory(ctx, keepID, "test"); err != nil {
		t.Fatalf("retire keeper: %v", err)
	}
	res, err := store.ApplyGroomPlan(ctx, nil, []GroomRekeyItem{{
		KeepID: keepID, NewKey: "n", RetireIDs: []int64{oldID}, Reason: "rekey: superseded edition",
	}}, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.RekeySkipped) != 1 || len(res.Rekeyed) != 0 {
		t.Fatalf("group should be skipped whole: %+v", res)
	}
	_, _, retiredAt, _ := activeConfirmedByID(t, store, oldID)
	if retiredAt != "" {
		t.Fatal("sibling must NOT be retired when its group is skipped")
	}
}

// TestApplyGroomPlanCrossPoolPromoteAndRetire proves the promote-and-retire pair
// on the equal-raw-key case: the stale shared row is retired first (freeing the
// active-row unique index slot), the newer private row is promoted to the shared
// pool, and the author is preserved.
func TestApplyGroomPlanCrossPoolPromoteAndRetire(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)

	sharedID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: MemoryOwner{Kind: "shared", Ref: "shared"}, AuthorRef: "scout",
		Repo: "acme/widget", Scope: "repo",
		Key: "notes-deploy", Content: "stale shared edition",
	})
	if err != nil {
		t.Fatalf("seed shared: %v", err)
	}
	privateID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: agentOwner("lead"), Repo: "acme/widget", Scope: "repo",
		Key: "notes-deploy", Content: "newer private edition",
	})
	if err != nil {
		t.Fatalf("seed private: %v", err)
	}

	res, err := store.ApplyGroomPlan(ctx, nil, nil, []GroomCrossPoolItem{{
		PrivateID: privateID, SharedID: sharedID,
		Reason: "cross-pool: superseded by promoted edition",
	}})
	if err != nil {
		t.Fatalf("apply cross-pool: %v", err)
	}
	if len(res.Promoted) != 1 || res.Promoted[0] != privateID {
		t.Fatalf("promotion result wrong: %+v", res)
	}

	_, _, retiredAt, reason := activeConfirmedByID(t, store, sharedID)
	if retiredAt == "" || reason != "cross-pool: superseded by promoted edition" {
		t.Fatalf("stale shared edition should be retired with the cross-pool reason: retired=%q reason=%q", retiredAt, reason)
	}
	var ownerKind, ownerRef, authorRef string
	if err := store.db.QueryRowContext(ctx, `
SELECT owner_kind, owner_ref, author_ref FROM confirmed_memories WHERE id = ?`, privateID).
		Scan(&ownerKind, &ownerRef, &authorRef); err != nil {
		t.Fatalf("read promoted row: %v", err)
	}
	if ownerKind != "shared" || ownerRef != "shared" || authorRef != "lead" {
		t.Fatalf("promotion must move the row to the shared pool with the author preserved: %s/%s author=%q",
			ownerKind, ownerRef, authorRef)
	}

	// The promoted edition is now the one the shared recall surfaces.
	rows, err := store.QueryConfirmedMemoriesForShared(ctx, "acme/widget", `"private" OR "edition" OR "deploy"`, 10)
	if err != nil {
		t.Fatalf("shared recall: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != privateID {
		t.Fatalf("shared recall should surface only the promoted edition: %+v", rows)
	}
}

// TestListCrossPoolSharedMatchesSignals proves the secondary-evidence read:
// same-domain private/shared pairs surface with a linked flag, and cross-repo
// pairs never match.
func TestListCrossPoolSharedMatchesSignals(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)

	sharedID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: MemoryOwner{Kind: "shared", Ref: "shared"}, Repo: "acme/widget", Scope: "repo",
		Key: "arm-ci", Content: "the arm64 runners flake on the nightly suite",
	})
	if err != nil {
		t.Fatalf("seed shared: %v", err)
	}
	privateID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: agentOwner("lead"), Repo: "acme/widget", Scope: "repo",
		Key: "arm-ci-note", Content: "arm64 runners flake on the nightly suite and need one retry",
	})
	if err != nil {
		t.Fatalf("seed private: %v", err)
	}
	// A private fact in a DIFFERENT repo: must not match the shared row.
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: agentOwner("lead"), Repo: "other/repo", Scope: "repo",
		Key: "arm-ci-note", Content: "arm64 runners flake on the nightly suite elsewhere",
	}); err != nil {
		t.Fatalf("seed other-repo private: %v", err)
	}

	matches, err := store.ListCrossPoolSharedMatches(ctx)
	if err != nil {
		t.Fatalf("list matches: %v", err)
	}
	var got *CrossPoolSharedMatch
	for i := range matches {
		if matches[i].PrivateID == privateID {
			got = &matches[i]
		}
		if matches[i].SharedID == sharedID && matches[i].PrivateID != privateID {
			t.Fatalf("cross-repo private fact matched a shared row: %+v", matches[i])
		}
	}
	if got == nil || got.SharedID != sharedID || got.Score <= 0 {
		t.Fatalf("expected a same-domain top match for the private fact: %+v", matches)
	}
	// The auto-link pass created an edge between the two overlapping facts, so
	// the linked flag is set; assert it matches the memory_links table.
	var linked int
	if err := store.db.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM memory_links WHERE (src_id = ? AND dst_id = ?) OR (src_id = ? AND dst_id = ?))`,
		privateID, sharedID, sharedID, privateID).Scan(&linked); err != nil {
		t.Fatalf("link check: %v", err)
	}
	if got.Linked != (linked != 0) {
		t.Fatalf("linked flag %v does not match memory_links %d", got.Linked, linked)
	}
}
