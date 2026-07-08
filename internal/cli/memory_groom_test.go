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

// groomSeed seeds a home with a realistic mix: a ToC/changelog note, a bare to-do
// list, two exact duplicates, an over-long brick, and a genuine keeper. Returns the
// seeded ids by label.
func groomSeed(t *testing.T, store *db.Store) map[string]int64 {
	t.Helper()
	owner := db.MemoryOwner{Kind: "agent", Ref: "lead"}
	ids := map[string]int64{}
	ids["toc"] = seedConfirmed(t, store, owner, "acme/widget", "repo", "index",
		"- [ci fast lanes](ci.md) — #754 shipped 2026-07-08\n- [dashboard](dash.md) — LIVE 2026-07-08\n- [feedback](fb.md) — full loop")
	ids["todo"] = seedConfirmed(t, store, owner, "acme/widget", "repo", "wave-todo",
		"- [ ] wire the advancer\n- [x] add config keys\n* [ ] write tests")
	ids["dup-a"] = seedConfirmed(t, store, owner, "acme/widget", "repo", "dup-a", "identical duplicate content body about arm64")
	ids["dup-b"] = seedConfirmed(t, store, owner, "acme/widget", "repo", "dup-b", "identical duplicate content body about arm64")
	ids["brick"] = seedConfirmed(t, store, owner, "acme/widget", "repo", "brick", strings.Repeat("a substantive but very long multi-fact brick. ", 40))
	ids["keep"] = seedConfirmed(t, store, owner, "acme/widget", "repo", "keeper", "killing a foreground agent ask strands a runtime-session lock")
	return ids
}

func runGroom(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runMemory(append([]string{"groom"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestGroomProposeWritesPlanTouchesNothing(t *testing.T) {
	home, store := memoryTestHome(t)
	ids := groomSeed(t, store)
	ctx := context.Background()

	before, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	planPath := filepath.Join(t.TempDir(), "plan.json")
	code, stdout, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath, "--json")
	if code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}

	// Store is untouched: same active set.
	after, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("propose mutated the store: active %d -> %d", len(before), len(after))
	}

	// Plan on disk equals the --json summary's plan and lists the expected actions.
	var summary struct {
		groomPlan
		Out string `json:"out"`
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("parse summary: %v (%s)", err, stdout)
	}
	if summary.Out != planPath {
		t.Fatalf("summary out = %q, want %q", summary.Out, planPath)
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if plan.SnapshotHash == "" || plan.SnapshotHash != summary.SnapshotHash {
		t.Fatalf("plan/summary snapshot mismatch: %q vs %q", plan.SnapshotHash, summary.SnapshotHash)
	}

	retired := map[int64]string{}
	for _, r := range plan.ProposedRetirements {
		retired[r.ID] = r.Reason
	}
	// toc, todo, and one duplicate proposed; brick + keeper + lowest-dup kept.
	if retired[ids["toc"]] != memory.GroomReasonStatusChangelog {
		t.Fatalf("toc not proposed status-changelog: %v", retired)
	}
	if retired[ids["todo"]] != memory.GroomReasonTaskList {
		t.Fatalf("todo not proposed task-list: %v", retired)
	}
	lowDup, highDup := ids["dup-a"], ids["dup-b"]
	if lowDup > highDup {
		lowDup, highDup = highDup, lowDup
	}
	if retired[highDup] != memory.GroomReasonDuplicate {
		t.Fatalf("higher dup not proposed duplicate: %v", retired)
	}
	if _, ok := retired[lowDup]; ok {
		t.Fatalf("lower-id duplicate must be kept")
	}
	if _, ok := retired[ids["keep"]]; ok {
		t.Fatalf("keeper must not be proposed")
	}
	if _, ok := retired[ids["brick"]]; ok {
		t.Fatalf("brick must be flagged, not retired")
	}
	if len(plan.RewriteFlags) != 1 || plan.RewriteFlags[0].ID != ids["brick"] {
		t.Fatalf("rewrite flags = %+v, want brick %d", plan.RewriteFlags, ids["brick"])
	}
}

func TestGroomApplyRetiresPlannedIds(t *testing.T) {
	home, store := memoryTestHome(t)
	ids := groomSeed(t, store)
	ctx := context.Background()

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath); code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	wantRetired := map[int64]bool{}
	for _, r := range plan.ProposedRetirements {
		wantRetired[r.ID] = true
	}
	if len(wantRetired) != 3 {
		t.Fatalf("expected 3 proposed retirements, got %d", len(wantRetired))
	}

	code, stdout, stderr := runGroom(t, "--home", home, "--yes", "--plan", planPath, "--json")
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, stderr)
	}
	var res groomApplyResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("parse apply result: %v (%s)", err, stdout)
	}
	if !res.Applied || len(res.Retired) != 3 || len(res.Skipped) != 0 {
		t.Fatalf("apply result unexpected: %+v", res)
	}

	// The active vault set no longer contains the retired ids; keeper survives.
	active, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	activeIDs := map[int64]bool{}
	for _, m := range active {
		activeIDs[m.ID] = true
	}
	for id := range wantRetired {
		if activeIDs[id] {
			t.Fatalf("retired id %d still active after apply", id)
		}
	}
	if !activeIDs[ids["keep"]] || !activeIDs[ids["brick"]] {
		t.Fatalf("keeper/brick wrongly retired; active=%v", activeIDs)
	}
}

func TestGroomStalePlanAborts(t *testing.T) {
	home, store := memoryTestHome(t)
	groomSeed(t, store)
	ctx := context.Background()

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath); code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}

	// Mutate the store between propose and apply — a new confirmed memory changes the
	// snapshot_hash.
	owner := db.MemoryOwner{Kind: "agent", Ref: "lead"}
	seedConfirmed(t, store, owner, "acme/widget", "repo", "added-after", "a fresh fact added after the proposal")

	before, _ := store.ListConfirmedMemoriesForVault(ctx, "")

	code, _, stderr := runGroom(t, "--home", home, "--yes", "--plan", planPath, "--json")
	if code == 0 {
		t.Fatalf("apply should have aborted stale")
	}
	if !strings.Contains(stderr, "stale") {
		t.Fatalf("stderr = %q, want stale abort", stderr)
	}
	// Nothing was retired.
	after, _ := store.ListConfirmedMemoriesForVault(ctx, "")
	if len(after) != len(before) {
		t.Fatalf("stale abort still mutated store: %d -> %d", len(before), len(after))
	}
}

func TestGroomAlreadyRetiredIdsSkipped(t *testing.T) {
	home, store := memoryTestHome(t)
	ids := groomSeed(t, store)
	ctx := context.Background()

	// Manually retire the to-do note BEFORE proposing, so it drops out of the active
	// set the snapshot covers.
	if err := store.RetireConfirmedMemory(ctx, ids["todo"], "manual"); err != nil {
		t.Fatalf("manual retire: %v", err)
	}

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath); code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	// Inject the already-retired id into the plan without changing snapshot_hash
	// (the retired row is not part of the active snapshot, so the hash still matches).
	plan.ProposedRetirements = append(plan.ProposedRetirements, groomPlanRetirement{
		ID: ids["todo"], Key: "wave-todo", Reason: memory.GroomReasonTaskList, FirstLine: "- [ ] wire the advancer",
	})
	if err := writeJSONFile(planPath, plan); err != nil {
		t.Fatalf("rewrite plan: %v", err)
	}

	code, stdout, stderr := runGroom(t, "--home", home, "--yes", "--plan", planPath, "--json")
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, stderr)
	}
	var res groomApplyResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("parse apply result: %v (%s)", err, stdout)
	}
	skipped := map[int64]bool{}
	for _, id := range res.Skipped {
		skipped[id] = true
	}
	if !skipped[ids["todo"]] {
		t.Fatalf("already-retired todo id %d not skipped; result=%+v", ids["todo"], res)
	}
	// The genuinely active proposed ids (toc + a duplicate) still retired.
	if len(res.Retired) == 0 {
		t.Fatalf("expected some ids retired alongside the skip; result=%+v", res)
	}
}

func TestGroomProposeDoesNotRetireCrossScopeDuplicates(t *testing.T) {
	home, store := memoryTestHome(t)
	const body = "identical duplicate content body about arm64 retries"
	alice := db.MemoryOwner{Kind: "agent", Ref: "alice"}
	bob := db.MemoryOwner{Kind: "agent", Ref: "bob"}
	// Same content, different owners AND different repos: each copy is the only one
	// visible in its own retrieval scope, so neither may be proposed for retirement.
	seedConfirmed(t, store, alice, "acme/widget", "repo", "a", body)
	seedConfirmed(t, store, bob, "acme/gadget", "repo", "b", body)

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath); code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	for _, r := range plan.ProposedRetirements {
		if r.Reason == memory.GroomReasonDuplicate {
			t.Fatalf("cross-scope duplicate wrongly proposed for retirement: %+v", r)
		}
	}
	if plan.Stats.ByReason[memory.GroomReasonDuplicate] != 0 {
		t.Fatalf("duplicate proposals across scopes: %+v", plan.Stats.ByReason)
	}
}

func TestGroomProposeEmptyStore(t *testing.T) {
	home, _ := memoryTestHome(t)
	planPath := filepath.Join(t.TempDir(), "plan.json")
	code, stdout, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath)
	if code != 0 {
		t.Fatalf("propose exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to retire") {
		t.Fatalf("stdout = %q, want nothing to retire", stdout)
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(plan.ProposedRetirements) != 0 || plan.Stats.TotalMemories != 0 {
		t.Fatalf("empty-store plan not empty: %+v", plan)
	}

	// Applying an empty plan is a graceful no-op.
	code, _, stderr = runGroom(t, "--home", home, "--yes", "--plan", planPath)
	if code != 0 {
		t.Fatalf("apply empty exit %d: %s", code, stderr)
	}
}

func TestGroomRejectsAmbiguousFlags(t *testing.T) {
	home, _ := memoryTestHome(t)
	if code, _, _ := runGroom(t, "--home", home); code != 2 {
		t.Fatalf("no mode should exit 2, got %d", code)
	}
	if code, _, _ := runGroom(t, "--home", home, "--propose", "--yes"); code != 2 {
		t.Fatalf("both modes should exit 2, got %d", code)
	}
	if code, _, stderr := runGroom(t, "--home", home, "--yes"); code != 2 || !strings.Contains(stderr, "--plan") {
		t.Fatalf("--yes without --plan should exit 2 asking for --plan, got %d %q", code, stderr)
	}
}

func TestGroomApplyBadPlanFile(t *testing.T) {
	home, _ := memoryTestHome(t)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schema_version": 999}`), 0o644); err != nil {
		t.Fatalf("write bad plan: %v", err)
	}
	if code, _, stderr := runGroom(t, "--home", home, "--yes", "--plan", bad); code != 1 || !strings.Contains(stderr, "schema") {
		t.Fatalf("bad schema should exit 1 with schema error, got %d %q", code, stderr)
	}
}
