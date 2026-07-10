package cli

// End-to-end CLI tests for #804: the stable ingest-key round-trip under
// auto-confirm, and the groom rekey / cross-pool propose-and-apply flows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

// rawMemoryDB opens a second connection to a test home's database for direct
// row inspection and timestamp backdating (UpsertConfirmedMemory always stamps
// now, and RFC3339 has one-second resolution, so tests that need a strict
// updated_at ordering set it explicitly).
func rawMemoryDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	return raw
}

func backdateConfirmed(t *testing.T, raw *sql.DB, id int64, ts string) {
	t.Helper()
	if _, err := raw.Exec(`UPDATE confirmed_memories SET updated_at = ? WHERE id = ?`, ts, id); err != nil {
		t.Fatalf("backdate confirmed %d: %v", id, err)
	}
}

// TestMemoryIngestStableKeyRoundTrip is the #804 headline contract: editing a
// note and re-sweeping lands on the SAME key, so auto-confirm updates the
// existing confirmed fact in place, and the prior edition survives as a
// superseded row.
func TestMemoryIngestStableKeyRoundTrip(t *testing.T) {
	home, store := memoryTestHome(t)
	paths := config.PathsForHome(home)
	writeMemoryPipelineConfig(t, paths, `
[memory]
ingest_auto_confirm = true
`)
	src := t.TempDir()
	writeFixture(t, src, "runbook.md",
		"The deploy runs from the shared box after the nightly build finishes.\n")

	code, out, errOut := runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "acme/widget", "--json")
	if code != 0 {
		t.Fatalf("first ingest exit %d: %s", code, errOut)
	}
	var first memoryIngestResult
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatalf("parse first ingest: %v (%s)", err, out)
	}
	if first.Inserted != 1 || first.Confirmed != 1 {
		t.Fatalf("first ingest should insert+confirm one chunk: %+v", first)
	}
	if len(first.InsertedKeys) != 1 || first.InsertedKeys[0] != "runbook-untitled" {
		t.Fatalf("stable key shape wrong (no content-hash suffix): %v", first.InsertedKeys)
	}

	ctx := context.Background()
	rows, err := store.ListConfirmedMemories(ctx, "lead", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("confirmed rows after first ingest: %v (%d)", err, len(rows))
	}
	liveID := rows[0].ID

	// Edit the note and re-sweep: same key, in-place update on the same row.
	writeFixture(t, src, "runbook.md",
		"The deploy runs from the shared box after the nightly build finishes, and it retries once on a flaky upload.\n")
	code, out, errOut = runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "acme/widget", "--json")
	if code != 0 {
		t.Fatalf("re-ingest exit %d: %s", code, errOut)
	}
	var second memoryIngestResult
	if err := json.Unmarshal([]byte(out), &second); err != nil {
		t.Fatalf("parse re-ingest: %v (%s)", err, out)
	}
	if second.Inserted != 1 || second.Confirmed != 1 || second.Deduped != 0 {
		t.Fatalf("edited chunk should insert+confirm again: %+v", second)
	}
	if len(second.InsertedKeys) != 1 || second.InsertedKeys[0] != first.InsertedKeys[0] {
		t.Fatalf("edited chunk must keep its key: %v vs %v", second.InsertedKeys, first.InsertedKeys)
	}

	raw := rawMemoryDB(t, home)
	var liveContent string
	if err := raw.QueryRow(`SELECT content FROM confirmed_memories WHERE id = ?`, liveID).Scan(&liveContent); err != nil {
		t.Fatalf("read live row: %v", err)
	}
	if liveContent != "The deploy runs from the shared box after the nightly build finishes, and it retries once on a flaky upload." {
		t.Fatalf("live row not updated in place: %q", liveContent)
	}
	var archivedCount int
	var archivedContent string
	if err := raw.QueryRow(`
SELECT COUNT(*), COALESCE(MAX(content), '') FROM confirmed_memories WHERE superseded_by = ?`, liveID).
		Scan(&archivedCount, &archivedContent); err != nil {
		t.Fatalf("read archived editions: %v", err)
	}
	if archivedCount != 1 || archivedContent != "The deploy runs from the shared box after the nightly build finishes." {
		t.Fatalf("prior edition must survive as a superseded row: count=%d content=%q", archivedCount, archivedContent)
	}
}

// TestGroomRekeyProposeAndApply seeds pre-#804 legacy hash-suffixed sibling
// editions and drives the full plan flow: propose surfaces a rekey group (newest
// kept, stable target key, sibling retired), apply executes it, and the FTS key
// column is re-synced.
func TestGroomRekeyProposeAndApply(t *testing.T) {
	home, store := memoryTestHome(t)
	owner := db.MemoryOwner{Kind: "agent", Ref: "lead"}
	oldID := seedConfirmed(t, store, owner, "acme/widget", "repo", "deploy-note-a1b2c3d4",
		"the deploy note first edition body")
	keepID := seedConfirmed(t, store, owner, "acme/widget", "repo", "deploy-note-ffee0011",
		"the deploy note second edition body with more detail")
	raw := rawMemoryDB(t, home)
	backdateConfirmed(t, raw, oldID, "2026-01-01T00:00:00Z")

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath); code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(plan.Rekeys) != 1 || plan.Stats.Rekeys != 1 {
		t.Fatalf("expected one rekey group in the plan: %+v", plan.Rekeys)
	}
	rk := plan.Rekeys[0]
	if rk.KeepID != keepID || rk.NewKey != "deploy-note" {
		t.Fatalf("newest edition should be kept under the stable key: %+v", rk)
	}
	if len(rk.Retire) != 1 || rk.Retire[0].ID != oldID {
		t.Fatalf("older sibling should be retired: %+v", rk.Retire)
	}

	code, out, stderr := runGroom(t, "--home", home, "--yes", "--plan", planPath, "--json")
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, stderr)
	}
	var applied groomApplyResult
	if err := json.Unmarshal([]byte(out), &applied); err != nil {
		t.Fatalf("parse apply result: %v (%s)", err, out)
	}
	if len(applied.Rekeyed) != 1 || applied.Rekeyed[0] != keepID {
		t.Fatalf("apply should report the rekeyed keeper: %+v", applied)
	}

	var key, retiredAt, retiredReason, ftsKey string
	if err := raw.QueryRow(`SELECT key FROM confirmed_memories WHERE id = ?`, keepID).Scan(&key); err != nil {
		t.Fatalf("read keeper: %v", err)
	}
	if key != "deploy-note" {
		t.Fatalf("keeper key not rewritten: %q", key)
	}
	if err := raw.QueryRow(`SELECT retired_at, retired_reason FROM confirmed_memories WHERE id = ?`, oldID).
		Scan(&retiredAt, &retiredReason); err != nil {
		t.Fatalf("read sibling: %v", err)
	}
	if retiredAt == "" || retiredReason != memory.GroomReasonRekeySuperseded {
		t.Fatalf("sibling retirement wrong: retired=%q reason=%q", retiredAt, retiredReason)
	}
	if err := raw.QueryRow(`SELECT key FROM confirmed_memories_fts WHERE rowid = ?`, keepID).Scan(&ftsKey); err != nil {
		t.Fatalf("read fts key: %v", err)
	}
	if ftsKey != "deploy-note" {
		t.Fatalf("FTS key column not synced: %q", ftsKey)
	}
}

// TestGroomCrossPoolProposeAndApply seeds a stale shared edition and a newer
// private edition on the same stable key, then drives propose (a stable-key
// promote-and-retire pair) and apply (private promoted with author preserved,
// shared retired).
func TestGroomCrossPoolProposeAndApply(t *testing.T) {
	home, store := memoryTestHome(t)
	sharedID := seedConfirmed(t, store, db.MemoryOwner{Kind: "shared", Ref: "shared"},
		"acme/widget", "repo", "notes-deploy", "the stale shared edition of the deploy fact")
	privateID := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"},
		"acme/widget", "repo", "notes-deploy", "the newer private edition of the deploy fact")
	raw := rawMemoryDB(t, home)
	backdateConfirmed(t, raw, sharedID, "2026-01-01T00:00:00Z")

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath); code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(plan.CrossPool) != 1 || plan.Stats.CrossPool != 1 {
		t.Fatalf("expected one cross-pool pair: %+v", plan.CrossPool)
	}
	cp := plan.CrossPool[0]
	if cp.PrivateID != privateID || cp.SharedID != sharedID || cp.Basis != memory.CrossPoolBasisStableKey {
		t.Fatalf("cross-pool pair wrong: %+v", cp)
	}

	code, out, stderr := runGroom(t, "--home", home, "--yes", "--plan", planPath, "--json")
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, stderr)
	}
	var applied groomApplyResult
	if err := json.Unmarshal([]byte(out), &applied); err != nil {
		t.Fatalf("parse apply result: %v (%s)", err, out)
	}
	if len(applied.Promoted) != 1 || applied.Promoted[0] != privateID {
		t.Fatalf("apply should report the promoted private id: %+v", applied)
	}

	var ownerKind, ownerRef, authorRef string
	if err := raw.QueryRow(`SELECT owner_kind, owner_ref, author_ref FROM confirmed_memories WHERE id = ?`, privateID).
		Scan(&ownerKind, &ownerRef, &authorRef); err != nil {
		t.Fatalf("read promoted row: %v", err)
	}
	if ownerKind != "shared" || ownerRef != "shared" || authorRef != "lead" {
		t.Fatalf("promotion wrong: %s/%s author=%q", ownerKind, ownerRef, authorRef)
	}
	var retiredAt, retiredReason string
	if err := raw.QueryRow(`SELECT retired_at, retired_reason FROM confirmed_memories WHERE id = ?`, sharedID).
		Scan(&retiredAt, &retiredReason); err != nil {
		t.Fatalf("read retired shared row: %v", err)
	}
	if retiredAt == "" || retiredReason != memory.GroomReasonCrossPoolStale {
		t.Fatalf("shared retirement wrong: retired=%q reason=%q", retiredAt, retiredReason)
	}
}

// TestGroomCrossPoolDoesNotProposeWhenSharedIsNewer pins the direction guard end
// to end: an OLDER private edition never proposes replacing a newer shared one.
func TestGroomCrossPoolDoesNotProposeWhenSharedIsNewer(t *testing.T) {
	home, store := memoryTestHome(t)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "shared", Ref: "shared"},
		"acme/widget", "repo", "notes-deploy", "the fresh shared edition of the deploy fact")
	privateID := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"},
		"acme/widget", "repo", "notes-deploy", "an old private edition of the deploy fact")
	raw := rawMemoryDB(t, home)
	backdateConfirmed(t, raw, privateID, "2026-01-01T00:00:00Z")

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath); code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(plan.CrossPool) != 0 {
		t.Fatalf("no cross-pool pair should be proposed when shared is newer: %+v", plan.CrossPool)
	}
}
