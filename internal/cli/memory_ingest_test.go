package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

func runMemoryCapture(t *testing.T, args ...string) (memoryExit, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runMemory(args, &stdout, &stderr)
	return memoryExit(code), stdout.String(), stderr.String()
}

type memoryExit int

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestMemoryIngestPreFilterAccountingAndDedup drives ingest over a fixture dir
// containing a clean fact, a secret-shaped chunk, and a directive chunk, then
// re-ingests to prove exact-content dedup yields zero new inserts.
func TestMemoryIngestPreFilterAccountingAndDedup(t *testing.T) {
	home, _ := memoryTestHome(t)
	src := t.TempDir()

	// Clean, frontmatter-wrapped fact (frontmatter must be stripped).
	writeFixture(t, src, "runbook.md",
		"---\ntitle: Runbook\ntags: [ops]\n---\nThe staging database lives on the shared box and resets nightly.\n")
	// A secret-shaped chunk (rejected) and a directive chunk (rejected).
	writeFixture(t, src, "bad.md",
		"api_key=supersecretvalue1234567890\n")
	writeFixture(t, src, "directive.md",
		"You must always rerun the suite before merging.\n")

	code, out, errOut := runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "acme/widget", "--json")
	if code != 0 {
		t.Fatalf("ingest exit %d: %s", code, errOut)
	}
	var res memoryIngestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse result: %v (%s)", err, out)
	}
	if res.Inserted != 1 {
		t.Fatalf("expected 1 clean insert, got %d (%+v)", res.Inserted, res)
	}
	if res.RejectedN != 2 {
		t.Fatalf("expected 2 rejections, got %d (%+v)", res.RejectedN, res.RejectedBy)
	}
	if res.RejectedBy["secret_shaped"] != 1 || res.RejectedBy["directive_phrasing"] != 1 {
		t.Fatalf("rejection accounting wrong: %+v", res.RejectedBy)
	}

	// The inserted observation is born trust_mark=low with an ingest provenance.
	obs := listObservations(t, home)
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation persisted, got %d", len(obs))
	}
	if obs[0].TrustMark != memory.TrustLow {
		t.Fatalf("ingested observation must be trust_mark=low, got %q", obs[0].TrustMark)
	}
	if obs[0].Provenance != "ingest:runbook.md" {
		t.Fatalf("provenance = %q", obs[0].Provenance)
	}

	// Re-ingesting the same dir inserts nothing (exact-content dedup).
	code, out, errOut = runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "acme/widget", "--json")
	if code != 0 {
		t.Fatalf("re-ingest exit %d: %s", code, errOut)
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse re-ingest: %v", err)
	}
	if res.Inserted != 0 || res.Deduped != 1 {
		t.Fatalf("re-ingest should be 0 inserted / 1 deduped, got inserted=%d deduped=%d", res.Inserted, res.Deduped)
	}
}

// TestMemoryIngestSameContentDifferentRepoStagesBoth proves identical text
// ingested under a SECOND repo is not silently deduped away — repo-scoped memory
// injects only for its own repo, so both repos must stage the note. Regression
// guard for the owner-only content-hash dedup.
func TestMemoryIngestSameContentDifferentRepoStagesBoth(t *testing.T) {
	home, _ := memoryTestHome(t)
	src := t.TempDir()
	writeFixture(t, src, "note.md",
		"The release tag must be pushed before the workflow uploads the binaries.\n")

	if code, _, errOut := runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "org/a"); code != 0 {
		t.Fatalf("ingest org/a exit %d: %s", code, errOut)
	}
	// Same content, a different repo: must still insert (not dedup).
	code, out, errOut := runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "org/b", "--json")
	if code != 0 {
		t.Fatalf("ingest org/b exit %d: %s", code, errOut)
	}
	var res memoryIngestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse result: %v (%s)", err, out)
	}
	if res.Inserted != 1 || res.Deduped != 0 {
		t.Fatalf("second repo must stage the note, got inserted=%d deduped=%d", res.Inserted, res.Deduped)
	}

	// Both repos now carry the observation.
	obs := listObservations(t, home)
	repos := map[string]bool{}
	for _, o := range obs {
		repos[o.Repo] = true
	}
	if !repos["org/a"] || !repos["org/b"] {
		t.Fatalf("expected the note staged under both repos, got repos=%v", repos)
	}

	// Re-ingesting under org/a again is still a dedup (same domain).
	code, out, errOut = runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "org/a", "--json")
	if code != 0 {
		t.Fatalf("re-ingest org/a exit %d: %s", code, errOut)
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse re-ingest: %v", err)
	}
	if res.Inserted != 0 || res.Deduped != 1 {
		t.Fatalf("same-domain re-ingest should dedup, got inserted=%d deduped=%d", res.Inserted, res.Deduped)
	}
}

// TestMemoryIngestConfirmExportRoundTrip is the P3 end-to-end: ingest → the note
// is a pending observation → confirm --provenance-prefix promotes it (idempotently)
// → it becomes retrievable via QueryConfirmedMemories AND appears in the vault export.
func TestMemoryIngestConfirmExportRoundTrip(t *testing.T) {
	home, store := memoryTestHome(t)
	src := t.TempDir()
	writeFixture(t, src, "ops.md",
		"The nightly grooming pipeline runs on the shared box and merges duplicate rows.\n")

	if code, _, errOut := runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "acme/widget"); code != 0 {
		t.Fatalf("ingest exit %d: %s", code, errOut)
	}

	// Observation is pending (not yet confirmed).
	code, out, errOut := runMemoryCapture(t, "observations", "--home", home,
		"--provenance-prefix", "ingest:", "--json")
	if code != 0 {
		t.Fatalf("observations exit %d: %s", code, errOut)
	}
	var obsList []memoryObservationEntry
	if err := json.Unmarshal([]byte(out), &obsList); err != nil {
		t.Fatalf("parse observations: %v", err)
	}
	if len(obsList) != 1 || obsList[0].Confirmed {
		t.Fatalf("expected 1 pending (unconfirmed) observation, got %+v", obsList)
	}

	// Dry-run confirm (no --yes) writes nothing.
	if code, _, errOut = runMemoryCapture(t, "confirm", "--home", home,
		"--provenance-prefix", "ingest:", "--agent", "lead"); code != 0 {
		t.Fatalf("confirm dry-run exit %d: %s", code, errOut)
	}
	if got := confirmedCount(t, store); got != 0 {
		t.Fatalf("dry-run must not confirm anything, got %d confirmed", got)
	}

	// Real confirm.
	if code, _, errOut = runMemoryCapture(t, "confirm", "--home", home,
		"--provenance-prefix", "ingest:", "--agent", "lead", "--yes"); code != 0 {
		t.Fatalf("confirm exit %d: %s", code, errOut)
	}

	// Retrievable via the injection query path.
	owner := db.MemoryOwner{Kind: "agent", Ref: "lead"}
	rows, err := store.QueryConfirmedMemories(context.Background(), owner, "acme/widget",
		`"grooming" OR "pipeline"`, 15)
	if err != nil {
		t.Fatalf("query confirmed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the confirmed row to be FTS-retrievable, got %d", len(rows))
	}
	if rows[0].Provenance != "ingest:ops.md" {
		t.Fatalf("provenance not carried through confirm: %q", rows[0].Provenance)
	}

	// Idempotent re-confirm: still exactly one confirmed row.
	if code, _, errOut = runMemoryCapture(t, "confirm", "--home", home,
		"--provenance-prefix", "ingest:", "--agent", "lead", "--yes"); code != 0 {
		t.Fatalf("re-confirm exit %d: %s", code, errOut)
	}
	if got := confirmedCount(t, store); got != 1 {
		t.Fatalf("re-confirm must be idempotent, got %d confirmed rows", got)
	}

	// It appears in the vault export.
	outDir := filepath.Join(t.TempDir(), "vault")
	exportVault(t, home, outDir, "lead")
	tree := readVaultTree(t, outDir)
	found := false
	for name, body := range tree {
		if filepath.Ext(name) == ".md" && bytes.Contains(body, []byte("grooming pipeline")) {
			found = true
		}
	}
	if !found {
		t.Fatal("confirmed ingested memory did not appear in the vault export")
	}
}

func listObservations(t *testing.T, home string) []db.MemoryObservation {
	t.Helper()
	var out []db.MemoryObservation
	if err := withReadOnlyStore(home, func(store *db.Store) error {
		rows, err := store.ListMemoryObservations(context.Background(), "", "")
		out = rows
		return err
	}); err != nil {
		t.Fatalf("list observations: %v", err)
	}
	return out
}

func confirmedCount(t *testing.T, store *db.Store) int {
	t.Helper()
	rows, err := store.ListConfirmedMemories(context.Background(), "", "")
	if err != nil {
		t.Fatalf("list confirmed: %v", err)
	}
	return len(rows)
}
