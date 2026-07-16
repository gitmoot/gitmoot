package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/memory"
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

func assertIngestedEventSource(t *testing.T, store *db.Store, want string) {
	t.Helper()
	events, err := store.ListMemoryEvents(context.Background(), db.MemoryEventFilter{
		Kinds: []string{db.MemoryEventIngested}, Limit: 10,
	})
	if err != nil || len(events) != 1 {
		t.Fatalf("ingested events=%+v err=%v", events, err)
	}
	var detail struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(events[0].Detail), &detail); err != nil || detail.Source != want {
		t.Fatalf("ingested detail=%s parsed=%+v err=%v", events[0].Detail, detail, err)
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

func TestMemoryIngestSkipsMemoryIndexLinkLists(t *testing.T) {
	home, _ := memoryTestHome(t)
	src := t.TempDir()
	indexBody, err := os.ReadFile(filepath.Join("..", "memory", "testdata", "quality", "memory-index-165.md"))
	if err != nil {
		t.Fatalf("read live index fixture: %v", err)
	}
	writeFixture(t, src, "MEMORY.md", string(indexBody))
	writeFixture(t, src, "keeper.md", "The arm64 runner uses a dedicated cache directory during release builds.\n")

	code, out, errOut := runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "acme/widget", "--json")
	if code != 0 {
		t.Fatalf("ingest exit %d: %s", code, errOut)
	}
	var result memoryIngestResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse ingest result: %v (%s)", err, out)
	}
	if result.Files != 2 || result.Chunks != 1 || result.Inserted != 1 || result.RejectedN != 1 || result.RejectedBy["index_file"] != 1 {
		t.Fatalf("index skip accounting = %+v", result)
	}
	observations := listObservations(t, home)
	if len(observations) != 1 || observations[0].Provenance != "ingest:keeper.md" {
		t.Fatalf("index file was ingested: %+v", observations)
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

func TestMemoryIngestAutoConfirmFlagPrivateOnly(t *testing.T) {
	home, store := memoryTestHome(t)
	src := t.TempDir()
	writeFixture(t, src, "note.md",
		"The release calendar uses the blue marker for the July cutoff.\n")

	code, out, errOut := runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "owner/repo", "--json")
	if code != 0 {
		t.Fatalf("default ingest exit %d: %s", code, errOut)
	}
	var off memoryIngestResult
	if err := json.Unmarshal([]byte(out), &off); err != nil {
		t.Fatalf("parse default ingest: %v (%s)", err, out)
	}
	if off.AutoConfirm || off.Confirmed != 0 || confirmedCount(t, store) != 0 {
		t.Fatalf("auto-confirm off should leave pending only, result=%+v confirmed=%d", off, confirmedCount(t, store))
	}

	homeOn, storeOn := memoryTestHome(t)
	paths := config.PathsForHome(homeOn)
	writeMemoryPipelineConfig(t, paths, `
[memory]
ingest_auto_confirm = true
`)
	code, out, errOut = runMemoryCapture(t, "ingest", src, "--home", homeOn,
		"--agent", "lead", "--repo", "owner/repo", "--json")
	if code != 0 {
		t.Fatalf("auto-confirm ingest exit %d: %s", code, errOut)
	}
	var on memoryIngestResult
	if err := json.Unmarshal([]byte(out), &on); err != nil {
		t.Fatalf("parse auto-confirm ingest: %v (%s)", err, out)
	}
	if !on.AutoConfirm || on.Inserted != 1 || on.Confirmed != 1 {
		t.Fatalf("auto-confirm result wrong: %+v", on)
	}
	privateRows, err := storeOn.QueryConfirmedMemories(context.Background(),
		db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: "lead"}, "owner/repo", `"calendar" OR "cutoff"`, 10)
	if err != nil || len(privateRows) != 1 || privateRows[0].Owner.Kind != memory.OwnerKindAgent || privateRows[0].Owner.Ref != "lead" {
		t.Fatalf("auto-confirm should write lead private memory, rows=%+v err=%v", privateRows, err)
	}
	sharedRows, err := storeOn.QueryConfirmedMemoriesForShared(context.Background(), "owner/repo", `"calendar" OR "cutoff"`, 10)
	if err != nil {
		t.Fatalf("query shared: %v", err)
	}
	if len(sharedRows) != 0 {
		t.Fatalf("auto-confirm must not write shared memory, got %+v", sharedRows)
	}
	assertIngestedEventSource(t, storeOn, "note.md")
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

func TestMemoryIngestSharedAndConfirmToSharedAuthorStamping(t *testing.T) {
	home, store := memoryTestHome(t)
	src := t.TempDir()
	writeFixture(t, src, "shared.md",
		"The release checklist lives in the shared memory pool after human review.\n")

	code, out, errOut := runMemoryCapture(t, "ingest", src, "--home", home,
		"--agent", "lead", "--repo", "acme/widget", "--shared", "--json")
	if code != 0 {
		t.Fatalf("shared ingest exit %d: %s", code, errOut)
	}
	var ingest memoryIngestResult
	if err := json.Unmarshal([]byte(out), &ingest); err != nil {
		t.Fatalf("parse ingest: %v (%s)", err, out)
	}
	if !ingest.Shared || ingest.Agent != "lead" || ingest.Inserted != 1 {
		t.Fatalf("shared ingest result wrong: %+v", ingest)
	}

	obs := listObservations(t, home)
	if len(obs) != 1 {
		t.Fatalf("expected one observation, got %+v", obs)
	}
	if obs[0].Owner.Kind != memory.OwnerKindShared || obs[0].Owner.Ref != memory.SharedOwnerRef || obs[0].AuthorRef != "lead" {
		t.Fatalf("shared observation should preserve author lead, got %+v", obs[0])
	}

	code, out, errOut = runMemoryCapture(t, "list", "--home", home, "--pending", "--agent", "lead", "--json")
	if code != 0 {
		t.Fatalf("list shared pending by author exit %d: %s", code, errOut)
	}
	var pending []memoryListEntry
	if err := json.Unmarshal([]byte(out), &pending); err != nil {
		t.Fatalf("parse pending list: %v (%s)", err, out)
	}
	if len(pending) != 1 || pending[0].OwnerKind != memory.OwnerKindShared || pending[0].AuthorRef != "lead" {
		t.Fatalf("--agent lead should include shared pending rows authored by lead, got %+v", pending)
	}

	code, out, errOut = runMemoryCapture(t, "confirm", "--home", home,
		"--provenance-prefix", "ingest:", "--agent", "lead", "--to-shared", "--yes", "--json")
	if code != 0 {
		t.Fatalf("confirm --to-shared exit %d: %s", code, errOut)
	}
	var confirm memoryConfirmResult
	if err := json.Unmarshal([]byte(out), &confirm); err != nil {
		t.Fatalf("parse confirm: %v (%s)", err, out)
	}
	if !confirm.ToShared || confirm.Confirmed != 1 {
		t.Fatalf("confirm to shared result wrong: %+v", confirm)
	}

	rows, err := store.QueryConfirmedMemoriesForShared(context.Background(), "acme/widget", `"shared" OR "memory"`, 5)
	if err != nil {
		t.Fatalf("query shared: %v", err)
	}
	if len(rows) != 1 || rows[0].Owner.Kind != memory.OwnerKindShared || rows[0].AuthorRef != "lead" {
		t.Fatalf("confirmed shared row should preserve author lead, got %+v", rows)
	}
}

func TestMemoryIngestSweepPartialFailureStillExitsZero(t *testing.T) {
	home, _ := memoryTestHome(t)
	paths := config.PathsForHome(home)
	srcA := t.TempDir()
	srcB := t.TempDir()
	writeFixture(t, srcA, "alpha.md", "The alpha memory sweep source records release notes for the owner repo.\n")
	writeFixture(t, srcB, "beta.md", "The beta memory sweep source records verification notes for the owner repo.\n")
	missing := filepath.Join(t.TempDir(), "missing")
	writeMemoryPipelineConfig(t, paths, `
[[memory.ingest]]
path = "`+filepath.ToSlash(srcA)+`"
agent = "lead"
repo = "owner/repo"
tier = "repo"

[[memory.ingest]]
path = "`+filepath.ToSlash(missing)+`"
agent = "lead"
repo = "owner/repo"
tier = "repo"

[[memory.ingest]]
path = "`+filepath.ToSlash(srcB)+`"
agent = "lead"
repo = "owner/repo"
tier = "repo"
`)

	code, out, errOut := runMemoryCapture(t, "ingest", "sweep", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("partial sweep exit %d, want 0: %s", code, errOut)
	}
	var result memoryIngestSweepResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse sweep: %v (%s)", err, out)
	}
	if result.Totals.Sources != 3 || result.Totals.Succeeded != 2 || result.Totals.Failed != 1 {
		t.Fatalf("totals = %+v, want 3 sources / 2 succeeded / 1 failed", result.Totals)
	}
	if result.Totals.Inserted != 2 || result.Totals.Deduped != 0 || result.Totals.Rejected != 0 {
		t.Fatalf("counts = %+v, want inserted=2 deduped=0 rejected=0", result.Totals)
	}
	if len(result.Sources) != 3 || result.Sources[1].Path != missing || result.Sources[1].Error == "" {
		t.Fatalf("source errors not reported per source: %+v", result.Sources)
	}
	if obs := listObservations(t, home); len(obs) != 2 {
		t.Fatalf("observations = %d, want two successful-source inserts: %+v", len(obs), obs)
	}
}

func TestMemoryIngestSweepAutoConfirmsConfiguredSources(t *testing.T) {
	home, store := memoryTestHome(t)
	paths := config.PathsForHome(home)
	src := t.TempDir()
	writeFixture(t, src, "sweep.md", "The sweep source records the violet release-board handoff.\n")
	writeMemoryPipelineConfig(t, paths, `
[memory]
ingest_auto_confirm = true

[[memory.ingest]]
path = "`+filepath.ToSlash(src)+`"
agent = "lead"
repo = "owner/repo"
tier = "repo"
`)

	code, out, errOut := runMemoryCapture(t, "ingest", "sweep", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("auto-confirm sweep exit %d: %s", code, errOut)
	}
	var result memoryIngestSweepResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse sweep: %v (%s)", err, out)
	}
	if result.Totals.Inserted != 1 || result.Totals.Confirmed != 1 || result.Sources[0].Confirmed != 1 {
		t.Fatalf("auto-confirm sweep result wrong: %+v", result)
	}
	privateRows, err := store.QueryConfirmedMemories(context.Background(),
		db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: "lead"}, "owner/repo", `"violet" OR "handoff"`, 10)
	if err != nil || len(privateRows) != 1 {
		t.Fatalf("sweep should confirm into private memory, rows=%+v err=%v", privateRows, err)
	}
	sharedRows, err := store.QueryConfirmedMemoriesForShared(context.Background(), "owner/repo", `"violet" OR "handoff"`, 10)
	if err != nil {
		t.Fatalf("query shared: %v", err)
	}
	if len(sharedRows) != 0 {
		t.Fatalf("sweep auto-confirm must not write shared memory, got %+v", sharedRows)
	}
	assertIngestedEventSource(t, store, "sweep.md")
}

func TestMemoryIngestSweepAllSourcesFailExitsNonZero(t *testing.T) {
	home, _ := memoryTestHome(t)
	paths := config.PathsForHome(home)
	missing := filepath.Join(t.TempDir(), "missing")
	writeMemoryPipelineConfig(t, paths, `
[[memory.ingest]]
path = "`+filepath.ToSlash(missing)+`"
agent = "lead"
repo = "owner/repo"
tier = "repo"
`)

	code, out, _ := runMemoryCapture(t, "ingest", "sweep", "--home", home, "--json")
	if code != 1 {
		t.Fatalf("all-failed sweep exit %d, want 1 (out=%s)", code, out)
	}
	var result memoryIngestSweepResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse sweep: %v (%s)", err, out)
	}
	if result.Totals.Sources != 1 || result.Totals.Failed != 1 || len(result.Sources) != 1 || result.Sources[0].Error == "" {
		t.Fatalf("all-failed result = %+v", result)
	}
}

func TestMemoryIngestSweepEmptyConfigSkips(t *testing.T) {
	home, _ := memoryTestHome(t)

	code, out, errOut := runMemoryCapture(t, "ingest", "sweep", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("empty sweep exit %d, want 0: %s", code, errOut)
	}
	var result memoryIngestSweepResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse sweep: %v (%s)", err, out)
	}
	if result.Totals.Sources != 0 || result.Skipped == "" || len(result.Sources) != 0 {
		t.Fatalf("empty sweep result = %+v", result)
	}
}

func writeMemoryPipelineConfig(t *testing.T, paths config.Paths, body string) {
	t.Helper()
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
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
