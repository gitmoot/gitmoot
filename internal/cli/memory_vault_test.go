package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

// readVaultTree returns every regular file under dir keyed by its relative path,
// so two exports can be compared byte-for-byte.
func readVaultTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk vault: %v", err)
	}
	return out
}

func seedConfirmed(t *testing.T, store *db.Store, owner db.MemoryOwner, repo, scope, key, content string) int64 {
	t.Helper()
	id, err := store.UpsertConfirmedMemory(context.Background(), db.ConfirmedMemory{
		Owner: owner, Repo: repo, Scope: scope, Key: key, Content: content, Provenance: "test", SourceJob: "job-" + key,
	})
	if err != nil {
		t.Fatalf("seed confirmed %q: %v", key, err)
	}
	return id
}

func exportVault(t *testing.T, home, outDir, agent string) vaultExportResult {
	t.Helper()
	args := []string{"vault", "export", "--home", home, "--out", outDir, "--json"}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	var stdout, stderr bytes.Buffer
	if code := runMemory(args, &stdout, &stderr); code != 0 {
		t.Fatalf("vault export exit %d: %s", code, stderr.String())
	}
	var res vaultExportResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("parse export result: %v (%s)", err, stdout.String())
	}
	return res
}

func TestVaultExportByteIdentical(t *testing.T) {
	home, store := memoryTestHome(t)
	builder := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	reviewer := db.MemoryOwner{Kind: "agent", Ref: "reviewer"}
	seedConfirmed(t, store, builder, "acme/widget", "repo", "ci-flake", "arm64 CI is flaky under load")
	seedConfirmed(t, store, builder, "", "general", "toolchain-trap", "the default go toolchain is too old for this module")
	seedConfirmed(t, store, builder, "acme/widget", "repo", "flaky-tests", "the CI arm64 runner drops flaky failures intermittently")
	seedConfirmed(t, store, reviewer, "acme/widget", "repo", "review-style", "reviewers prefer small focused diffs over sweeping rewrites")

	out1 := filepath.Join(t.TempDir(), "v1")
	out2 := filepath.Join(t.TempDir(), "v2")
	r1 := exportVault(t, home, out1, "")
	r2 := exportVault(t, home, out2, "")

	if r1.SnapshotHash != r2.SnapshotHash {
		t.Fatalf("snapshot hashes differ: %s vs %s", r1.SnapshotHash, r2.SnapshotHash)
	}
	if r1.Memories != 4 {
		t.Fatalf("memories = %d, want 4", r1.Memories)
	}
	if r1.Indexes != 2 {
		t.Fatalf("indexes = %d, want 2 (builder + reviewer)", r1.Indexes)
	}
	tree1 := readVaultTree(t, out1)
	tree2 := readVaultTree(t, out2)
	if len(tree1) != len(tree2) {
		t.Fatalf("file counts differ: %d vs %d", len(tree1), len(tree2))
	}
	for name, b1 := range tree1 {
		b2, ok := tree2[name]
		if !ok {
			t.Fatalf("file %q missing from second export", name)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("file %q differs between exports:\n--- 1 ---\n%s\n--- 2 ---\n%s", name, b1, b2)
		}
	}
	// Manifest is present, well-formed, and carries the snapshot hash.
	mf, ok := tree1["manifest.json"]
	if !ok {
		t.Fatal("manifest.json missing")
	}
	var manifest vaultManifest
	if err := json.Unmarshal(mf, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.SchemaVersion != vaultSchemaVersion || manifest.SnapshotHash != r1.SnapshotHash {
		t.Fatalf("manifest mismatch: %+v vs result %s", manifest, r1.SnapshotHash)
	}
}

func TestVaultExportSupersededExcluded(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	oldID := seedConfirmed(t, store, owner, "acme/widget", "repo", "old-fact", "the old flaky note about arm64 CI")
	newID := seedConfirmed(t, store, owner, "acme/widget", "repo", "new-fact", "the current note about arm64 CI stability")

	// No production writer sets superseded_by; link the chain directly.
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE confirmed_memories SET superseded_by = ? WHERE id = ?`, newID, oldID); err != nil {
		t.Fatalf("set superseded_by: %v", err)
	}
	raw.Close()

	out := filepath.Join(t.TempDir(), "v")
	res := exportVault(t, home, out, "")
	if res.Memories != 1 {
		t.Fatalf("memories = %d, want 1 (superseded excluded)", res.Memories)
	}
	tree := readVaultTree(t, out)
	for name, body := range tree {
		if strings.Contains(name, "old-fact") {
			t.Fatalf("superseded memory %q must not be exported", name)
		}
		// The surviving note must not link back to the retired row.
		if strings.Contains(string(body), "old-fact") {
			t.Fatalf("note %q links to a superseded memory:\n%s", name, body)
		}
	}
}

func TestVaultExportDeterministicLinksUnderBM25Ties(t *testing.T) {
	home, store := memoryTestHome(t)
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	// Identical content across several rows forces bm25 ties, so only the id
	// tie-break makes link order (and thus bytes) reproducible.
	seedConfirmed(t, store, owner, "acme/widget", "repo", "anchor", "shared arm64 flaky runner token soup")
	for _, k := range []string{"twin-a", "twin-b", "twin-c", "twin-d", "twin-e", "twin-f"} {
		seedConfirmed(t, store, owner, "acme/widget", "repo", k, "shared arm64 flaky runner token soup")
	}

	out1 := filepath.Join(t.TempDir(), "v1")
	out2 := filepath.Join(t.TempDir(), "v2")
	r1 := exportVault(t, home, out1, "")
	r2 := exportVault(t, home, out2, "")
	if r1.SnapshotHash != r2.SnapshotHash {
		t.Fatalf("link order under bm25 ties is non-deterministic: %s vs %s", r1.SnapshotHash, r2.SnapshotHash)
	}

	// The anchor note must carry exactly K=5 links and never link to itself.
	tree := readVaultTree(t, out1)
	var anchor string
	for name, body := range tree {
		if strings.HasSuffix(name, "-anchor.md") {
			anchor = string(body)
		}
	}
	if anchor == "" {
		t.Fatal("anchor note not found")
	}
	linkCount := strings.Count(anchor, "- [[")
	if linkCount != vaultLinkK {
		t.Fatalf("anchor link count = %d, want %d", linkCount, vaultLinkK)
	}
	if strings.Contains(anchor, "-anchor|") {
		t.Fatalf("anchor links to itself:\n%s", anchor)
	}
	// Links are sorted by target id (ascending numeric prefix).
	var stems []string
	for _, line := range strings.Split(anchor, "\n") {
		if strings.HasPrefix(line, "- [[") {
			stems = append(stems, line[:20])
		}
	}
	if !sort.StringsAreSorted(stems) {
		t.Fatalf("link stems not id-sorted: %v", stems)
	}
}

func TestVaultExportIncludesPersistedMemoryLinks(t *testing.T) {
	home, store := memoryTestHome(t)
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	srcID := seedConfirmed(t, store, owner, "acme/widget", "repo", "source", "alpha-only operational note")
	dstID := seedConfirmed(t, store, owner, "acme/widget", "repo", "target", "zulu-only release note")
	clearMemoryLinks(t, home)

	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), `
INSERT INTO memory_links (src_id, dst_id, score, origin, created_at)
VALUES (?, ?, 0.42, 'auto', '2026-07-09T00:00:00Z')`, srcID, dstID); err != nil {
		t.Fatalf("insert persisted link: %v", err)
	}
	raw.Close()

	out := filepath.Join(t.TempDir(), "v")
	exportVault(t, home, out, "")
	tree := readVaultTree(t, out)
	var source string
	for name, body := range tree {
		if strings.HasSuffix(name, "-source.md") {
			source = string(body)
		}
	}
	if source == "" {
		t.Fatal("source note not found")
	}
	want := "[[" + memory.VaultStem(dstID, "target") + "|target]]"
	if !strings.Contains(source, want) {
		t.Fatalf("vault note missing persisted link %s:\n%s", want, source)
	}
}

func TestVaultExportEmptyDB(t *testing.T) {
	home, _ := memoryTestHome(t)
	out := filepath.Join(t.TempDir(), "v")
	res := exportVault(t, home, out, "")
	if res.Memories != 0 || res.Indexes != 0 {
		t.Fatalf("empty export should have 0 memories/indexes, got %+v", res)
	}
	// A valid, empty vault still writes a manifest with the empty-sequence hash.
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if res.SnapshotHash != emptySHA {
		t.Fatalf("empty snapshot hash = %q, want %q", res.SnapshotHash, emptySHA)
	}
	tree := readVaultTree(t, out)
	if len(tree) != 1 {
		t.Fatalf("empty vault should contain only manifest.json, got %v", vaultKeysOf(tree))
	}
	if _, ok := tree["manifest.json"]; !ok {
		t.Fatalf("empty vault missing manifest.json, got %v", vaultKeysOf(tree))
	}
}

func TestVaultExportAgentFilter(t *testing.T) {
	home, store := memoryTestHome(t)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "builder"}, "acme/widget", "repo", "b-fact", "builder fact about arm64")
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "reviewer"}, "acme/widget", "repo", "r-fact", "reviewer fact about diffs")

	out := filepath.Join(t.TempDir(), "v")
	res := exportVault(t, home, out, "builder")
	if res.Memories != 1 {
		t.Fatalf("agent-filtered export memories = %d, want 1", res.Memories)
	}
	tree := readVaultTree(t, out)
	for name := range tree {
		if strings.Contains(name, "r-fact") {
			t.Fatalf("reviewer memory leaked into builder-only export: %q", name)
		}
	}
}

func TestVaultExportDefaultOutUnderEvals(t *testing.T) {
	home, store := memoryTestHome(t)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "builder"}, "", "general", "g", "a general fact about toolchains")
	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"vault", "export", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("vault export exit %d: %s", code, stderr.String())
	}
	want := filepath.Join(config.PathsForHome(home).Evals, "vault")
	if _, err := os.Stat(filepath.Join(want, "manifest.json")); err != nil {
		t.Fatalf("default vault not written under evals: %v (stdout=%s)", err, stdout.String())
	}
}

func TestVaultExportReExportOverwritesCleanly(t *testing.T) {
	home, store := memoryTestHome(t)
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	seedConfirmed(t, store, owner, "acme/widget", "repo", "keep-me", "content one about arm64")
	staleID := seedConfirmed(t, store, owner, "acme/widget", "repo", "drop-me", "content two about arm64")

	out := filepath.Join(t.TempDir(), "v")
	exportVault(t, home, out, "")
	if _, err := os.Stat(filepath.Join(out, memory.VaultFilename(staleID, "drop-me"))); err != nil {
		t.Fatalf("expected stale note present after first export: %v", err)
	}

	// Retire drop-me, re-export; the stale note must be gone (atomic replace).
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), `UPDATE confirmed_memories SET superseded_by = 999999 WHERE id = ?`, staleID); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	raw.Close()

	exportVault(t, home, out, "")
	if _, err := os.Stat(filepath.Join(out, memory.VaultFilename(staleID, "drop-me"))); !os.IsNotExist(err) {
		t.Fatalf("stale note survived re-export: err=%v", err)
	}
}

func TestVaultExportRefusesToClobberNonVaultDir(t *testing.T) {
	home, store := memoryTestHome(t)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "builder"}, "acme/widget", "repo", "keep", "content about arm64")

	// A directory that looks like the user's own Obsidian vault: notes + config,
	// no gitmoot manifest.
	out := filepath.Join(t.TempDir(), "my-obsidian-vault")
	if err := os.MkdirAll(filepath.Join(out, ".obsidian"), 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	precious := filepath.Join(out, "important-note.md")
	if err := os.WriteFile(precious, []byte("do not delete me"), 0o644); err != nil {
		t.Fatalf("seed note: %v", err)
	}

	// Without --force the export must refuse and leave the user's files intact.
	var stdout, stderr bytes.Buffer
	code := runMemory([]string{"vault", "export", "--home", home, "--out", out, "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("export into populated non-vault dir should fail, got exit 0 (stdout=%s)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "refusing to overwrite") {
		t.Fatalf("expected refusal message, got: %s", stderr.String())
	}
	if b, err := os.ReadFile(precious); err != nil || string(b) != "do not delete me" {
		t.Fatalf("user's note was clobbered: err=%v content=%q", err, string(b))
	}

	// With --force the export proceeds and replaces the directory.
	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"vault", "export", "--home", home, "--out", out, "--force", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("forced export exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
		t.Fatalf("forced export did not write a vault: %v", err)
	}
	if _, err := os.Stat(precious); !os.IsNotExist(err) {
		t.Fatalf("forced export should have replaced the dir, note still present: err=%v", err)
	}
}

func TestVaultExportIntoEmptyDirAllowed(t *testing.T) {
	home, store := memoryTestHome(t)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "builder"}, "acme/widget", "repo", "keep", "content about arm64")

	// A pre-existing but EMPTY directory is a safe target (no --force needed).
	out := filepath.Join(t.TempDir(), "empty-target")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"vault", "export", "--home", home, "--out", out, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("export into empty dir should succeed, exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
		t.Fatalf("vault not written into empty dir: %v", err)
	}
}

func vaultKeysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
