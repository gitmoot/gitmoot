package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

// importVault runs `memory vault import --json` and returns the parsed result plus
// the exit code and stderr for the failure-path assertions.
func importVault(t *testing.T, home, dir string, apply bool) (vaultImportResult, int, string) {
	t.Helper()
	args := []string{"vault", "import", "--home", home, "--json", dir}
	if apply {
		args = append(args, "--yes")
	}
	var stdout, stderr bytes.Buffer
	code := runMemory(args, &stdout, &stderr)
	var res vaultImportResult
	if code == 0 {
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			t.Fatalf("parse import result: %v (%s)", err, stdout.String())
		}
	}
	return res, code, stderr.String()
}

func noteFileFor(id int64, key string) string { return memory.VaultFilename(id, key) }

// TestVaultImportDryRunNoWrites proves the default (dry-run) diff is correct and
// writes nothing to the store.
func TestVaultImportDryRunNoWrites(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	keepID := seedConfirmed(t, store, owner, "acme/widget", "repo", "keep", "keep content about arm64")
	editID := seedConfirmed(t, store, owner, "acme/widget", "repo", "edit", "edit content about arm64")
	delID := seedConfirmed(t, store, owner, "acme/widget", "repo", "drop", "drop content about arm64")
	_ = keepID

	dir := filepath.Join(t.TempDir(), "vault")
	exportVault(t, home, dir, "")

	// Edit one note's body, delete another, add a brand-new owner-authored note.
	editNote := filepath.Join(dir, noteFileFor(editID, "edit"))
	raw, err := os.ReadFile(editNote)
	if err != nil {
		t.Fatalf("read edit note: %v", err)
	}
	edited := strings.Replace(string(raw), "edit content about arm64", "edit content about graviton now", 1)
	if edited == string(raw) {
		t.Fatal("edit substitution did not change the note")
	}
	if err := os.WriteFile(editNote, []byte(edited), 0o644); err != nil {
		t.Fatalf("write edit note: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, noteFileFor(delID, "drop"))); err != nil {
		t.Fatalf("delete note: %v", err)
	}
	newNote := "---\nowner_kind: \"agent\"\nowner_ref: \"builder\"\nrepo: \"acme/widget\"\nscope: \"repo\"\nkey: \"brand-new\"\n---\na brand new owner-authored note about deploys\n"
	if err := os.WriteFile(filepath.Join(dir, "brand-new.md"), []byte(newNote), 0o644); err != nil {
		t.Fatalf("write new note: %v", err)
	}

	res, code, stderr := importVault(t, home, dir, false)
	if code != 0 {
		t.Fatalf("dry-run import exit %d: %s", code, stderr)
	}
	if res.Applied {
		t.Fatal("dry-run must not apply")
	}
	if len(res.Updates) != 1 || res.Updates[0].MemoryID != editID {
		t.Fatalf("want 1 update for edit id %d, got %+v", editID, res.Updates)
	}
	if len(res.Retirements) != 1 || res.Retirements[0].MemoryID != delID {
		t.Fatalf("want 1 retirement for drop id %d, got %+v", delID, res.Retirements)
	}
	if len(res.NewObservations) != 1 {
		t.Fatalf("want 1 new observation, got %+v", res.NewObservations)
	}
	if res.Unchanged != 1 {
		t.Fatalf("want 1 unchanged (keep), got %d", res.Unchanged)
	}

	// Nothing was written: still 3 confirmed rows, zero observations.
	rows, _ := store.ListConfirmedMemories(ctx, "builder", "")
	if len(rows) != 3 {
		t.Fatalf("dry-run wrote to the store: %d confirmed rows", len(rows))
	}
	obs, _ := store.CountMemoryObservationsForOwner(ctx, "agent", "builder")
	if obs != 0 {
		t.Fatalf("dry-run staged observations: %d", obs)
	}
}

// TestVaultImportApplyRoundTrip is the full P2 E2E: export → edit + delete + add →
// import --yes → a fresh re-export reflects all three curations.
func TestVaultImportApplyRoundTrip(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	keepID := seedConfirmed(t, store, owner, "acme/widget", "repo", "keep", "keep content about arm64")
	editID := seedConfirmed(t, store, owner, "acme/widget", "repo", "edit", "edit content about arm64")
	delID := seedConfirmed(t, store, owner, "acme/widget", "repo", "drop", "drop content about arm64")

	dir := filepath.Join(t.TempDir(), "vault")
	exportVault(t, home, dir, "")

	editNote := filepath.Join(dir, noteFileFor(editID, "edit"))
	raw, _ := os.ReadFile(editNote)
	edited := strings.Replace(string(raw), "edit content about arm64", "EDITED graviton content", 1)
	if err := os.WriteFile(editNote, []byte(edited), 0o644); err != nil {
		t.Fatalf("write edit note: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, noteFileFor(delID, "drop"))); err != nil {
		t.Fatalf("delete note: %v", err)
	}
	newNote := "---\nowner_kind: \"agent\"\nowner_ref: \"builder\"\nrepo: \"acme/widget\"\nscope: \"repo\"\nkey: \"brand-new\"\n---\na brand new owner-authored note about deploys\n"
	if err := os.WriteFile(filepath.Join(dir, "brand-new.md"), []byte(newNote), 0o644); err != nil {
		t.Fatalf("write new note: %v", err)
	}

	res, code, stderr := importVault(t, home, dir, true)
	if code != 0 {
		t.Fatalf("apply import exit %d: %s", code, stderr)
	}
	if !res.Applied {
		t.Fatal("--yes must apply")
	}

	// Confirmed store: edit landed, delete retired, keep intact.
	rows, _ := store.ListConfirmedMemories(ctx, "builder", "")
	byID := map[int64]db.ConfirmedMemory{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if byID[editID].Content != "EDITED graviton content\n" {
		t.Fatalf("edit not applied, content = %q", byID[editID].Content)
	}
	if byID[editID].Provenance != "vault-import" {
		t.Fatalf("edit provenance = %q, want vault-import", byID[editID].Provenance)
	}
	if byID[keepID].Content != "keep content about arm64" {
		t.Fatalf("keep must be untouched, content = %q", byID[keepID].Content)
	}

	// Re-export: the retired note is gone, the edit is reflected, keep remains, and
	// the new observation is staged (pending, not yet in the confirmed vault).
	dir2 := filepath.Join(t.TempDir(), "vault2")
	exportVault(t, home, dir2, "")
	if _, err := os.Stat(filepath.Join(dir2, noteFileFor(delID, "drop"))); !os.IsNotExist(err) {
		t.Fatalf("retired note must not re-export (stat err = %v)", err)
	}
	reRaw, err := os.ReadFile(filepath.Join(dir2, noteFileFor(editID, "edit")))
	if err != nil {
		t.Fatalf("read re-exported edit note: %v", err)
	}
	if !strings.Contains(string(reRaw), "EDITED graviton content") {
		t.Fatalf("re-export missing edit:\n%s", reRaw)
	}
	if _, err := os.Stat(filepath.Join(dir2, noteFileFor(keepID, "keep"))); err != nil {
		t.Fatalf("keep note must still export: %v", err)
	}
	obs, _ := store.CountMemoryObservationsForOwner(ctx, "agent", "builder")
	if obs != 1 {
		t.Fatalf("want 1 staged observation, got %d", obs)
	}
	pending, _ := store.ListMemoryObservations(ctx, "builder", "")
	if len(pending) != 1 || pending[0].Key != "brand-new" || pending[0].Provenance != "vault-import:brand-new.md" {
		t.Fatalf("staged observation wrong: %+v", pending)
	}
}

// TestVaultImportStaleAborts proves import refuses when the store moved since the
// vault was exported.
func TestVaultImportStaleAborts(t *testing.T) {
	home, store := memoryTestHome(t)
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	seedConfirmed(t, store, owner, "acme/widget", "repo", "keep", "keep content about arm64")

	dir := filepath.Join(t.TempDir(), "vault")
	exportVault(t, home, dir, "")

	// Mutate the store after export → the fresh snapshot no longer matches manifest.
	seedConfirmed(t, store, owner, "acme/widget", "repo", "added-after", "a fact added after export")

	_, code, stderr := importVault(t, home, dir, true)
	if code == 0 {
		t.Fatal("stale import must fail")
	}
	if !strings.Contains(stderr, "stale") {
		t.Fatalf("stale error must mention staleness, got: %s", stderr)
	}
}

// TestVaultImportUnknownFileStagesObservation isolates the new-file path: a bare
// owner-authored note (no memory_id) becomes a pending observation, nothing else.
func TestVaultImportUnknownFileStagesObservation(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	seedConfirmed(t, store, owner, "acme/widget", "repo", "keep", "keep content about arm64")

	dir := filepath.Join(t.TempDir(), "vault")
	exportVault(t, home, dir, "")

	newNote := "---\nowner_kind: \"agent\"\nowner_ref: \"builder\"\nrepo: \"acme/widget\"\nscope: \"repo\"\nkey: \"runbook\"\n---\ndeploy runbook the owner dropped in\n"
	if err := os.WriteFile(filepath.Join(dir, "runbook.md"), []byte(newNote), 0o644); err != nil {
		t.Fatalf("write new note: %v", err)
	}

	res, code, stderr := importVault(t, home, dir, true)
	if code != 0 {
		t.Fatalf("import exit %d: %s", code, stderr)
	}
	if len(res.Updates) != 0 || len(res.Retirements) != 0 || len(res.NewObservations) != 1 {
		t.Fatalf("want only a new observation, got %+v", res)
	}
	if res.Unchanged != 1 {
		t.Fatalf("want 1 unchanged (keep), got %d", res.Unchanged)
	}
	pending, _ := store.ListMemoryObservations(ctx, "builder", "")
	if len(pending) != 1 || pending[0].TrustMark != memory.TrustNormal {
		t.Fatalf("owner-authored note must be a normal-trust observation: %+v", pending)
	}
	// It stays pending, never auto-confirmed.
	confirmed, _ := store.ListConfirmedMemories(ctx, "builder", "")
	if len(confirmed) != 1 {
		t.Fatalf("new note must NOT confirm; confirmed rows = %d", len(confirmed))
	}
}

// TestVaultImportMalformedNoteNoRetire proves an owner's broken YAML frontmatter is
// NOT silently reclassified into a retirement: `import --yes` refuses to apply while
// any note fails to parse, and the underlying confirmed fact survives intact.
func TestVaultImportMalformedNoteNoRetire(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	keepID := seedConfirmed(t, store, owner, "acme/widget", "repo", "keep", "keep content about arm64")

	dir := filepath.Join(t.TempDir(), "vault")
	exportVault(t, home, dir, "")

	// Corrupt the note's frontmatter with an unclosed quote (a realistic hand-edit
	// slip) so ParseVaultNote fails and the note drops out of the disk set.
	notePath := filepath.Join(dir, noteFileFor(keepID, "keep"))
	raw, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	broken := strings.Replace(string(raw), "---\n", "---\nbroken: \"unclosed value\n", 1)
	if broken == string(raw) {
		t.Fatal("failed to inject a frontmatter break")
	}
	if err := os.WriteFile(notePath, []byte(broken), 0o644); err != nil {
		t.Fatalf("write broken note: %v", err)
	}

	// --yes must refuse rather than retire the memory whose file is still on disk.
	_, code, stderr := importVault(t, home, dir, true)
	if code == 0 {
		t.Fatal("import --yes must fail while a note failed to parse")
	}
	if !strings.Contains(stderr, "failed to parse") || !strings.Contains(stderr, "refusing to apply") {
		t.Fatalf("want a parse-refusal error, got: %s", stderr)
	}

	// The fact is untouched: still confirmed, not retired.
	rows, _ := store.ListConfirmedMemories(ctx, "builder", "")
	if len(rows) != 1 || rows[0].ID != keepID {
		t.Fatalf("malformed import must not touch the store, got %+v", rows)
	}
	if n, _ := store.CountConfirmedMemoriesForOwner(ctx, "agent", "builder"); n != 1 {
		t.Fatalf("fact must remain injectable (count 1), got %d", n)
	}
}

// TestVaultImportAgentScopedRoundTrip proves a vault produced by `export --agent NAME`
// is importable even when OTHER owners have memories: import rebuilds the fresh export
// with the manifest's recorded scope, so a filtered vault no longer aborts as stale.
func TestVaultImportAgentScopedRoundTrip(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	alice := db.MemoryOwner{Kind: "agent", Ref: "alice"}
	bob := db.MemoryOwner{Kind: "agent", Ref: "bob"}
	aliceEdit := seedConfirmed(t, store, alice, "acme/widget", "repo", "edit", "alice content about arm64")
	seedConfirmed(t, store, bob, "acme/widget", "repo", "bobkey", "bob content about arm64")

	dir := filepath.Join(t.TempDir(), "vault")
	exportVault(t, home, dir, "alice") // filtered export: only alice's note + alice-scoped hash

	// Edit alice's note; bob's memories exist but are outside the export scope.
	editNote := filepath.Join(dir, noteFileFor(aliceEdit, "edit"))
	raw, _ := os.ReadFile(editNote)
	edited := strings.Replace(string(raw), "alice content about arm64", "EDITED alice graviton content", 1)
	if edited == string(raw) {
		t.Fatal("edit substitution did not change the note")
	}
	if err := os.WriteFile(editNote, []byte(edited), 0o644); err != nil {
		t.Fatalf("write edit note: %v", err)
	}

	// Must NOT abort as stale (pre-fix bug: alice-only hash vs all-owners rebuild).
	res, code, stderr := importVault(t, home, dir, true)
	if code != 0 {
		t.Fatalf("agent-scoped import exit %d: %s", code, stderr)
	}
	if !res.Applied || len(res.Updates) != 1 || res.Updates[0].MemoryID != aliceEdit {
		t.Fatalf("want alice edit applied, got %+v", res)
	}

	rows, _ := store.ListConfirmedMemories(ctx, "alice", "")
	if len(rows) != 1 || rows[0].Content != "EDITED alice graviton content\n" {
		t.Fatalf("alice edit not applied: %+v", rows)
	}
	// Bob's memory is untouched (never in scope).
	bobRows, _ := store.ListConfirmedMemories(ctx, "bob", "")
	if len(bobRows) != 1 || bobRows[0].Content != "bob content about arm64" {
		t.Fatalf("bob must be untouched, got %+v", bobRows)
	}
}

// TestVaultImportNotAVault rejects a directory without a manifest.
func TestVaultImportNotAVault(t *testing.T) {
	home, store := memoryTestHome(t)
	_ = store
	dir := t.TempDir()
	_, code, stderr := importVault(t, home, dir, false)
	if code == 0 {
		t.Fatal("import of a non-vault dir must fail")
	}
	if !strings.Contains(stderr, "not a gitmoot vault") {
		t.Fatalf("want not-a-vault error, got: %s", stderr)
	}
}
