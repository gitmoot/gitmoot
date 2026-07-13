package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
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
	if !strings.Contains(stdout, "nothing to do") {
		t.Fatalf("stdout = %q, want nothing to do", stdout)
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

func TestGroomSplitDryRunApplyAndRerun(t *testing.T) {
	home, store := memoryTestHome(t)
	owner := db.MemoryOwner{Kind: "agent", Ref: "lead"}
	fixture := func(id string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("..", "memory", "testdata", "groom", id+".md"))
		if err != nil {
			t.Fatalf("read groom fixture %s: %v", id, err)
		}
		return strings.TrimSuffix(string(body), "\n")
	}
	content := fixture("80")
	parentID := seedConfirmed(t, store, owner, "acme/widget", "repo", "session-brick", content)
	structuredID := seedConfirmed(t, store, owner, "acme/widget", "repo", "structured-round", fixture("152"))
	ctx := context.Background()

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("split dry-run exit %d: %s", code, stderr)
	}
	var dry groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &dry); err != nil {
		t.Fatalf("parse dry-run JSON: %v (%s)", err, stdout)
	}
	if !dry.DryRun || dry.Detected != 1 || dry.Applied != 0 || len(dry.Splits) != 1 || len(dry.Splits[0].Children) != 2 {
		t.Fatalf("dry-run output = %+v", dry)
	}
	rows, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("read active rows after dry-run: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("dry-run changed active rows: %+v", rows)
	}

	code, stdout, stderr = runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("split apply exit %d: %s", code, stderr)
	}
	var applied groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &applied); err != nil {
		t.Fatalf("parse apply JSON: %v (%s)", err, stdout)
	}
	if applied.DryRun || applied.Detected != 1 || applied.Applied != 1 || len(applied.Splits[0].Children) != 2 {
		t.Fatalf("apply output = %+v", applied)
	}
	for _, child := range applied.Splits[0].Children {
		if child.ID == 0 || child.Key == "" {
			t.Fatalf("applied child missing id/key: %+v", child)
		}
	}
	firstKeys := []string{applied.Splits[0].Children[0].Key, applied.Splits[0].Children[1].Key}
	firstIDs := []int64{applied.Splits[0].Children[0].ID, applied.Splits[0].Children[1].ID}

	code, stdout, stderr = runGroom(t, "--home", home, "--split-revert", "--parent", strconv.FormatInt(parentID, 10), "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("split-revert dry-run exit %d: %s", code, stderr)
	}
	var revertDry groomSplitRevertOutput
	if err := json.Unmarshal([]byte(stdout), &revertDry); err != nil {
		t.Fatalf("parse revert dry-run JSON: %v (%s)", err, stdout)
	}
	if !revertDry.DryRun || revertDry.Matched != 1 || len(revertDry.Reverted) != 1 || revertDry.Reverted[0].ParentID != parentID {
		t.Fatalf("revert dry-run output = %+v", revertDry)
	}

	code, stdout, stderr = runGroom(t, "--home", home, "--split-revert", "--parent", strconv.FormatInt(parentID, 10), "--json")
	if code != 0 {
		t.Fatalf("split-revert exit %d: %s", code, stderr)
	}
	var reverted groomSplitRevertOutput
	if err := json.Unmarshal([]byte(stdout), &reverted); err != nil {
		t.Fatalf("parse revert JSON: %v (%s)", err, stdout)
	}
	if reverted.DryRun || len(reverted.Reverted) != 1 || len(reverted.Skipped) != 0 {
		t.Fatalf("revert output = %+v", reverted)
	}
	matches, err := store.QueryConfirmedMemories(ctx, owner, "acme/widget", `"waveform"`, 10)
	if err != nil {
		t.Fatalf("query restored parent: %v", err)
	}
	if len(matches) == 0 || matches[0].ID != parentID {
		t.Fatalf("restored parent is not FTS-searchable: %+v", matches)
	}
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	defer raw.Close()
	for _, childID := range firstIDs {
		var retiredAt, reason string
		if err := raw.QueryRowContext(ctx, `SELECT retired_at, retired_reason FROM confirmed_memories WHERE id = ?`, childID).Scan(&retiredAt, &reason); err != nil {
			t.Fatalf("read reverted child %d: %v", childID, err)
		}
		if retiredAt == "" || reason != "groom-split-revert:"+strconv.FormatInt(parentID, 10) {
			t.Fatalf("child %d not retired: retired=%q reason=%q", childID, retiredAt, reason)
		}
	}

	code, stdout, stderr = runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("split rerun exit %d: %s", code, stderr)
	}
	var resplit groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &resplit); err != nil {
		t.Fatalf("parse re-split JSON: %v (%s)", err, stdout)
	}
	if resplit.Detected != 1 || resplit.Applied != 1 || len(resplit.Splits) != 1 {
		t.Fatalf("re-split output = %+v", resplit)
	}
	for i, child := range resplit.Splits[0].Children {
		if child.Key != firstKeys[i] || child.ID == firstIDs[i] {
			t.Fatalf("re-split child %d = %+v, want key %q and new id", i, child, firstKeys[i])
		}
	}
	active, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range active {
		if row.ID == structuredID {
			return
		}
	}
	t.Fatalf("structured fact %d was incorrectly split or retired", structuredID)
}

func TestGroomRejectsAmbiguousFlags(t *testing.T) {
	home, _ := memoryTestHome(t)
	if code, _, _ := runGroom(t, "--home", home); code != 2 {
		t.Fatalf("no mode should exit 2, got %d", code)
	}
	if code, _, _ := runGroom(t, "--home", home, "--propose", "--yes"); code != 2 {
		t.Fatalf("both modes should exit 2, got %d", code)
	}
	if code, _, _ := runGroom(t, "--home", home, "--propose", "--split"); code != 2 {
		t.Fatalf("two modes should exit 2, got %d", code)
	}
	if code, _, _ := runGroom(t, "--home", home, "--propose", "--dry-run"); code != 2 {
		t.Fatalf("--dry-run without --split should exit 2, got %d", code)
	}
	if code, _, _ := runGroom(t, "--home", home, "--split", "--parent", "1"); code != 2 {
		t.Fatalf("--parent without --split-revert should exit 2, got %d", code)
	}
	if code, _, _ := runGroom(t, "--home", home, "--split-revert", "--since", "yesterday"); code != 2 {
		t.Fatalf("invalid --since should exit 2, got %d", code)
	}
	if code, _, stderr := runGroom(t, "--home", home, "--yes"); code != 2 || !strings.Contains(stderr, "--plan") {
		t.Fatalf("--yes without --plan should exit 2 asking for --plan, got %d %q", code, stderr)
	}
}

func TestGroomLLMContractValidation(t *testing.T) {
	menu := []memory.GroomLLMBoundary{
		{ID: "c001", Offset: 100, Text: "Second story"},
		{ID: "c002", Offset: 200, Text: "Third story"},
	}
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "valid with surrounding prose", raw: `answer: {"split":true,"cuts":[{"id":"c002","text":"Third story"},{"id":"c001","text":"Second story"}]} done`},
		{name: "unknown id", raw: `{"split":true,"cuts":[{"id":"c999","text":"Second story"}]}`, wantErr: "unknown cut id"},
		{name: "wrong echo", raw: `{"split":true,"cuts":[{"id":"c001","text":"wrong"}]}`, wantErr: "echoed"},
		{name: "duplicate id", raw: `{"split":true,"cuts":[{"id":"c001","text":"Second story"},{"id":"c001","text":"Second story"}]}`, wantErr: "duplicate cut id"},
		{name: "split without cuts", raw: `{"split":true,"cuts":[]}`, wantErr: "at least one cut"},
		{name: "keep with cuts", raw: `{"split":false,"cuts":[{"id":"c001","text":"Second story"}]}`, wantErr: "requires empty cuts"},
		{name: "unknown field", raw: `{"split":false,"cuts":[],"reason":"no"}`, wantErr: "unknown field"},
		{name: "garbage JSON", raw: `not JSON`, wantErr: "no complete JSON object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := parseGroomLLMReply(tc.raw)
			if err == nil {
				var ids []string
				ids, _, err = validateGroomLLMReply(reply, menu)
				if tc.wantErr == "" && (len(ids) != 2 || ids[0] != "c001" || ids[1] != "c002") {
					t.Fatalf("sorted cut ids = %v", ids)
				}
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestGroomStaleContractValidation(t *testing.T) {
	content := "The stale baton also says only one submission may be in flight per team."
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "expired", raw: `{"verdict":"expired","residue":""}`, want: "expired"},
		{name: "surrounding prose", raw: `result: {"verdict":"still_relevant","residue":""} done`, want: "still_relevant"},
		{name: "durable residue", raw: `{"verdict":"contains_durable_residue","residue":"only one submission may be in flight per team"}`, want: "contains_durable_residue"},
		{name: "unknown verdict", raw: `{"verdict":"maybe","residue":""}`, wantErr: "unknown verdict"},
		{name: "missing residue", raw: `{"verdict":"expired"}`, wantErr: "must include"},
		{name: "expired with residue", raw: `{"verdict":"expired","residue":"keeper"}`, wantErr: "empty residue"},
		{name: "residue without quote", raw: `{"verdict":"contains_durable_residue","residue":"invented"}`, wantErr: "exact content quote"},
		{name: "unknown field", raw: `{"verdict":"expired","residue":"","extra":1}`, wantErr: "unknown field"},
		{name: "garbage", raw: `not json`, wantErr: "no complete JSON object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := parseGroomStaleReply(tc.raw)
			if err == nil {
				err = validateGroomStaleReply(reply, content)
			}
			if tc.wantErr == "" {
				if err != nil || reply.Verdict != tc.want {
					t.Fatalf("reply=%+v err=%v, want %s", reply, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestGroomQualityContractValidation(t *testing.T) {
	content := "The original misparse error now explains itself."
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "useless", raw: `{"verdict":"useless","confidence":0.91,"residue":""}`, want: "useless"},
		{name: "surrounding prose", raw: `result: {"verdict":"useful","confidence":0.75,"residue":""} done`, want: "useful"},
		{name: "durable residue", raw: `{"verdict":"contains_durable_residue","confidence":0.8,"residue":"misparse error now explains itself"}`, want: "contains_durable_residue"},
		{name: "unknown verdict", raw: `{"verdict":"maybe","confidence":0.5,"residue":""}`, wantErr: "unknown verdict"},
		{name: "missing confidence", raw: `{"verdict":"useless","residue":""}`, wantErr: "must include"},
		{name: "bad confidence", raw: `{"verdict":"useless","confidence":1.1,"residue":""}`, wantErr: "between 0 and 1"},
		{name: "useless with residue", raw: `{"verdict":"useless","confidence":0.5,"residue":"keeper"}`, wantErr: "empty residue"},
		{name: "invented residue", raw: `{"verdict":"contains_durable_residue","confidence":0.5,"residue":"invented"}`, wantErr: "exact content quote"},
		{name: "unknown field", raw: `{"verdict":"useless","confidence":0.5,"residue":"","extra":1}`, wantErr: "unknown field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := parseGroomQualityReply(tc.raw)
			if err == nil {
				err = validateGroomQualityReply(reply, content)
			}
			if tc.wantErr == "" {
				if err != nil || reply.Verdict != tc.want {
					t.Fatalf("reply=%+v err=%v, want %s", reply, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestGroomQualityShadowProposeAndCachedSplitDoNotRetire(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomQualityConfig(t, home, false)
	content := groomQualityFixture(t, "392")
	id := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "shipping-status", content)
	ageGroomFact(t, home, id)
	logPath := installGroomCodexStub(t, `{"verdict":"useless","confidence":0.96,"residue":""}`)
	planPath := filepath.Join(t.TempDir(), "quality-plan.json")

	code, stdout, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath, "--json")
	if code != 0 {
		t.Fatalf("quality propose exit %d: %s", code, stderr)
	}
	var summary struct {
		groomPlan
		Out string `json:"out"`
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("parse quality proposal: %v (%s)", err, stdout)
	}
	if !summary.Quality.Shadow || len(summary.Quality.Candidates) != 1 || summary.Quality.Candidates[0].Candidate.ID != id || summary.Quality.Candidates[0].Action != "would_retire" || summary.Quality.Candidates[0].Verdict != "useless" {
		t.Fatalf("shadow quality section = %+v", summary.Quality)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("shadow quality calls = %d, want 1", calls)
	}
	active, _ := store.ListConfirmedMemoriesForVault(context.Background(), "")
	if len(active) != 1 || active[0].ID != id {
		t.Fatalf("shadow proposal mutated fact: %+v", active)
	}

	code, stdout, stderr = runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("cached shadow split exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil || len(output.Quality.Candidates) != 1 || !output.Quality.Candidates[0].Cached || len(output.Quality.Retired) != 0 {
		t.Fatalf("cached shadow quality output=%+v err=%v", output.Quality, err)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("cached shadow verdict was billed again: %d", calls)
	}
}

func TestGroomQualityEnabledUselessAutoRetiresThroughHousePath(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomQualityConfig(t, home, true)
	id := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "vacuous", groomQualityFixture(t, "171"))
	ageGroomFact(t, home, id)
	installGroomCodexStub(t, `{"verdict":"useless","confidence":0.99,"residue":""}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("quality split exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse quality split: %v (%s)", err, stdout)
	}
	if output.Quality.Shadow || len(output.Quality.Candidates) != 1 || output.Quality.Candidates[0].Action != "auto_retire" || len(output.Quality.Retired) != 1 || output.Quality.Retired[0] != id {
		t.Fatalf("quality retire output = %+v", output.Quality)
	}
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	var reason string
	if err := raw.QueryRow(`SELECT retired_reason FROM confirmed_memories WHERE id = ?`, id).Scan(&reason); err != nil || !strings.HasPrefix(reason, "groom-quality:") {
		t.Fatalf("quality retired reason=%q err=%v", reason, err)
	}
}

func TestGroomQualityResidueStaysOwnerGated(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomQualityConfig(t, home, true)
	content := groomQualityFixture(t, "396")
	id := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "mixed-status", content)
	ageGroomFact(t, home, id)
	const residue = "the original misparse error now explains itself"
	installGroomCodexStub(t, `{"verdict":"contains_durable_residue","confidence":0.88,"residue":"`+residue+`"}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--propose", "--out", filepath.Join(t.TempDir(), "plan.json"), "--json")
	if code != 0 {
		t.Fatalf("quality residue propose exit %d: %s", code, stderr)
	}
	var summary struct{ groomPlan }
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil || len(summary.Quality.Candidates) != 1 {
		t.Fatalf("parse quality residue: section=%+v err=%v", summary.Quality, err)
	}
	entry := summary.Quality.Candidates[0]
	if entry.Action != "propose_extract" || entry.Residue != residue {
		t.Fatalf("quality residue entry = %+v", entry)
	}
	active, _ := store.ListConfirmedMemoriesForVault(context.Background(), "")
	if len(active) != 1 || active[0].ID != id {
		t.Fatalf("residue proposal mutated fact: %+v", active)
	}
}

func TestGroomLLMTotalBudgetSharedAcrossQualityStaleAndSplit(t *testing.T) {
	home, store := memoryTestHome(t)
	appendConfig(t, config.PathsForHome(home), "\n[memory]\n"+
		"groom_split_llm = true\n"+
		"groom_split_llm_runtime = \"codex\"\n"+
		"groom_split_llm_model = \"\"\n"+
		"groom_split_llm_max_per_run = 5\n"+
		"groom_llm_total_max_per_run = 1\n"+
		"groom_quality_max_per_run = 8\n")
	qualityID := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "quality-first", groomQualityFixture(t, "392"))
	ageGroomFact(t, home, qualityID)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "stale-second", groomStaleFixture(t, "154"))
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "split-third", groomLLMTestBrick())
	logPath := installGroomCodexStub(t, `{"verdict":"useless","confidence":0.95,"residue":""}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("shared-budget split exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse shared-budget output: %v (%s)", err, stdout)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("shared budget made %d calls, want 1", calls)
	}
	if len(output.Stale) != 1 || !strings.Contains(output.Stale[0].FallbackReason, "shared") {
		t.Fatalf("stale pass did not observe shared cap: %+v", output.Stale)
	}
	if len(output.LLM) != 1 || !strings.Contains(output.LLM[0].FallbackReason, "shared") {
		t.Fatalf("split pass did not observe shared cap: %+v", output.LLM)
	}
}

func TestGroomStaleExpiredStubAutoRetiresIdempotently(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, true)
	owner := db.MemoryOwner{Kind: "agent", Ref: "lead"}
	id := seedConfirmed(t, store, owner, "acme/widget", "repo", "stale-expired", groomStaleFixture(t, "154"))
	logPath := installGroomCodexStub(t, `{"verdict":"expired","residue":""}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("stale split exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse stale split output: %v (%s)", err, stdout)
	}
	if output.Detected != 0 || output.Applied != 0 || len(output.Stale) != 1 || output.Stale[0].Verdict != "expired" || output.Stale[0].Action != "auto_retire" || output.Stale[0].Cached || len(output.StaleRetired) != 1 || output.StaleRetired[0] != id {
		t.Fatalf("stale expired output = %+v", output)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("expired stub calls = %d, want 1", calls)
	}
	active, err := store.ListConfirmedMemoriesForVault(context.Background(), "")
	if err != nil || len(active) != 0 {
		t.Fatalf("active after stale retire = %+v err=%v", active, err)
	}
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	var reason string
	if err := raw.QueryRow(`SELECT retired_reason FROM confirmed_memories WHERE id = ?`, id).Scan(&reason); err != nil || !strings.HasPrefix(reason, "groom-stale:") {
		t.Fatalf("retired reason=%q err=%v", reason, err)
	}
	var ftsCount int
	if err := raw.QueryRow(`SELECT count(*) FROM confirmed_memories_fts WHERE rowid = ?`, id).Scan(&ftsCount); err != nil || ftsCount != 0 {
		t.Fatalf("fts count=%d err=%v", ftsCount, err)
	}
	if code, stdout, stderr = runGroom(t, "--home", home, "--split", "--json"); code != 0 {
		t.Fatalf("stale rerun exit %d: %s", code, stderr)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("retired candidate was billed again: %d calls", calls)
	}
}

func TestGroomStaleResidueStubProposesExtraction(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, true)
	content := groomStaleFixture(t, "158")
	id := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "stale-residue", content)
	const residue = "only one submission may be in flight per team"
	logPath := installGroomCodexStub(t, `{"verdict":"contains_durable_residue","residue":"`+residue+`"}`)
	planPath := filepath.Join(t.TempDir(), "stale-plan.json")

	code, stdout, stderr := runGroom(t, "--home", home, "--propose", "--out", planPath, "--json")
	if code != 0 {
		t.Fatalf("stale propose exit %d: %s", code, stderr)
	}
	var summary struct {
		groomPlan
		Out string `json:"out"`
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("parse stale proposal: %v (%s)", err, stdout)
	}
	if len(summary.Stale) != 1 || summary.Stale[0].Candidate.ID != id || summary.Stale[0].Verdict != "contains_durable_residue" || summary.Stale[0].Action != "propose_extract" || summary.Stale[0].Residue != residue {
		t.Fatalf("stale residue plan = %+v", summary.Stale)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("residue stub calls = %d, want 1", calls)
	}
	active, _ := store.ListConfirmedMemoriesForVault(context.Background(), "")
	if len(active) != 1 || active[0].ID != id {
		t.Fatalf("residue candidate was mutated: %+v", active)
	}

	code, stdout, stderr = runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("cached residue split exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil || len(output.Stale) != 1 || !output.Stale[0].Cached || len(output.StaleRetired) != 0 {
		t.Fatalf("cached residue output=%+v err=%v", output, err)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("cached residue was billed again: %d calls", calls)
	}
}

func TestGroomStaleGarbageAndLLMOffProposeOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		answer  string
		calls   int
		want    string
	}{
		{name: "garbage", enabled: true, answer: "garbage response", calls: 1, want: "no complete JSON object"},
		{name: "LLM off", enabled: false, answer: `{"verdict":"expired","residue":""}`, calls: 0, want: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, store := memoryTestHome(t)
			writeGroomLLMConfig(t, home, tc.enabled)
			id := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "stale-fallback", groomStaleFixture(t, "164"))
			logPath := installGroomCodexStub(t, tc.answer)
			code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
			if code != 0 {
				t.Fatalf("fallback split exit %d: %s", code, stderr)
			}
			var output groomSplitOutput
			if err := json.Unmarshal([]byte(stdout), &output); err != nil {
				t.Fatalf("parse fallback output: %v (%s)", err, stdout)
			}
			if len(output.Stale) != 1 || output.Stale[0].Candidate.ID != id || output.Stale[0].Verdict != "unknown" || output.Stale[0].Action != "propose_review" || !strings.Contains(output.Stale[0].FallbackReason, tc.want) || len(output.StaleRetired) != 0 {
				t.Fatalf("fallback output = %+v", output)
			}
			if calls := groomStubCalls(t, logPath); calls != tc.calls {
				t.Fatalf("stub calls = %d, want %d", calls, tc.calls)
			}
			active, _ := store.ListConfirmedMemoriesForVault(context.Background(), "")
			if len(active) != 1 || active[0].ID != id {
				t.Fatalf("fallback candidate was mutated: %+v", active)
			}
		})
	}
}

func TestGroomStaleMasterSwitchDisablesPass(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, true)
	appendConfig(t, config.PathsForHome(home), "\n[memory]\ngroom_stale = false\n")
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "stale-disabled", groomStaleFixture(t, "154"))
	logPath := installGroomCodexStub(t, `{"verdict":"expired","residue":""}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("disabled stale pass exit %d: %s", code, stderr)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse disabled stale output: %v", err)
	}
	if _, present := output["stale"]; present {
		t.Fatalf("disabled stale pass changed output: %s", stdout)
	}
	if calls := groomStubCalls(t, logPath); calls != 0 {
		t.Fatalf("disabled stale pass made %d runtime calls", calls)
	}
}

func groomStaleFixture(t *testing.T, id string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "memory", "testdata", "groom", id+".md"))
	if err != nil {
		t.Fatalf("read stale fixture %s: %v", id, err)
	}
	return strings.TrimSuffix(string(body), "\n")
}

func TestGroomLLMPathStubSplitsBrick(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, true)
	content := groomLLMTestBrick()
	parentID := seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "llm-brick", content)
	logPath := installGroomCodexStub(t, `{"split":true,"cuts":[{"id":"c001","text":"Second independent story"}]}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("LLM split exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse LLM split output: %v (%s)", err, stdout)
	}
	if output.Applied != 1 || output.Detected != 1 || len(output.LLM) != 1 || output.LLM[0].ParentID != parentID || output.LLM[0].Decision != "split" || output.LLM[0].Cached || len(output.LLM[0].CutIDs) != 1 || output.LLM[0].CutIDs[0] != "c001" {
		t.Fatalf("LLM split output = %+v", output)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("stub calls = %d, want 1", calls)
	}
	if len(output.Splits) != 1 || len(output.Splits[0].Children) != 2 {
		t.Fatalf("split children = %+v", output.Splits)
	}
}

func TestGroomLLMPathStubGarbageFallsBack(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, true)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "llm-garbage", groomLLMTestBrick())
	logPath := installGroomCodexStub(t, `garbage response`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("garbage fallback exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse garbage output: %v (%s)", err, stdout)
	}
	if output.Applied != 0 || len(output.LLM) != 1 || output.LLM[0].Decision != "fallback" || !strings.Contains(output.LLM[0].FallbackReason, "no complete JSON object") {
		t.Fatalf("garbage fallback output = %+v", output)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("stub calls = %d, want 1", calls)
	}
}

func TestGroomLLMNoSplitCacheAvoidsSecondCall(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, true)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "llm-keep", groomLLMTestBrick())
	logPath := installGroomCodexStub(t, `{"split":false,"cuts":[]}`)

	for run := 1; run <= 2; run++ {
		code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
		if code != 0 {
			t.Fatalf("no-split run %d exit %d: %s", run, code, stderr)
		}
		var output groomSplitOutput
		if err := json.Unmarshal([]byte(stdout), &output); err != nil {
			t.Fatalf("parse no-split run %d: %v (%s)", run, err, stdout)
		}
		if output.Applied != 0 || len(output.LLM) != 1 || output.LLM[0].Decision != "no_split" || output.LLM[0].Cached != (run == 2) {
			t.Fatalf("no-split run %d output = %+v", run, output)
		}
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("stub calls after cached rerun = %d, want 1", calls)
	}
}

func TestGroomLLMSplitCacheReplaysForIdenticalContent(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, true)
	content := groomLLMTestBrick()
	owner := db.MemoryOwner{Kind: "agent", Ref: "lead"}
	seedConfirmed(t, store, owner, "acme/widget", "repo", "llm-split-a", content)
	seedConfirmed(t, store, owner, "acme/widget", "repo", "llm-split-b", content)
	logPath := installGroomCodexStub(t, `{"split":true,"cuts":[{"id":"c001","text":"Second independent story"}]}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("cached split replay exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse cached split replay: %v (%s)", err, stdout)
	}
	if output.Applied != 2 || len(output.LLM) != 2 || output.LLM[0].Cached || !output.LLM[1].Cached || output.LLM[0].Decision != "split" || output.LLM[1].Decision != "split" {
		t.Fatalf("cached split replay output = %+v", output)
	}
	if calls := groomStubCalls(t, logPath); calls != 1 {
		t.Fatalf("identical split content stub calls = %d, want 1", calls)
	}
}

func TestGroomLLMOversizeSkipsWithoutCall(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, true)
	content := strings.Repeat("oversize continuous narrative content. ", 260)
	if len(strings.TrimSpace(content)) <= memory.GroomLLMMaxContentBytes {
		t.Fatal("oversize fixture is not over the hard limit")
	}
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "llm-oversize", content)
	logPath := installGroomCodexStub(t, `{"split":false,"cuts":[]}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("oversize split exit %d: %s", code, stderr)
	}
	var output groomSplitOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse oversize output: %v (%s)", err, stdout)
	}
	if len(output.LLM) != 1 || output.LLM[0].Decision != "skipped" || !strings.Contains(output.LLM[0].FallbackReason, "8192-byte") {
		t.Fatalf("oversize output = %+v", output)
	}
	if calls := groomStubCalls(t, logPath); calls != 0 {
		t.Fatalf("oversize stub calls = %d, want 0", calls)
	}
}

func TestGroomLLMFlagOffMakesZeroCalls(t *testing.T) {
	home, store := memoryTestHome(t)
	writeGroomLLMConfig(t, home, false)
	seedConfirmed(t, store, db.MemoryOwner{Kind: "agent", Ref: "lead"}, "acme/widget", "repo", "llm-off", groomLLMTestBrick())
	logPath := installGroomCodexStub(t, `{"split":false,"cuts":[]}`)

	code, stdout, stderr := runGroom(t, "--home", home, "--split", "--json")
	if code != 0 {
		t.Fatalf("flag-off split exit %d: %s", code, stderr)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("parse flag-off output: %v", err)
	}
	if _, present := output["llm"]; present {
		t.Fatalf("flag-off output changed shape: %s", stdout)
	}
	if calls := groomStubCalls(t, logPath); calls != 0 {
		t.Fatalf("flag-off stub calls = %d, want 0", calls)
	}
}

func groomLLMTestBrick() string {
	first := strings.Repeat("The first independent narrative preserves detailed operational context. ", 12)
	second := strings.Repeat("The second independent narrative records a separate durable outcome. ", 12)
	return "First subject\n" + first + "\n\nSecond independent story\n" + second
}

func writeGroomLLMConfig(t *testing.T, home string, enabled bool) {
	t.Helper()
	appendConfig(t, config.PathsForHome(home), "\n[memory]\n"+
		"groom_split_llm = "+strconv.FormatBool(enabled)+"\n"+
		"groom_split_llm_runtime = \"codex\"\n"+
		"groom_split_llm_model = \"\"\n"+
		"groom_split_llm_max_per_run = 5\n")
}

func writeGroomQualityConfig(t *testing.T, home string, enabled bool) {
	t.Helper()
	appendConfig(t, config.PathsForHome(home), "\n[memory]\n"+
		"groom_quality = "+strconv.FormatBool(enabled)+"\n"+
		"groom_quality_max_per_run = 8\n"+
		"groom_quality_min_age = \"24h\"\n"+
		"groom_llm_total_max_per_run = 10\n"+
		"groom_stale = false\n")
}

func groomQualityFixture(t *testing.T, id string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "memory", "testdata", "quality", id+".md"))
	if err != nil {
		t.Fatalf("read quality fixture %s: %v", id, err)
	}
	return strings.TrimSuffix(string(body), "\n")
}

func ageGroomFact(t *testing.T, home string, id int64) {
	t.Helper()
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE confirmed_memories SET first_confirmed_at = datetime('now', '-48 hours') WHERE id = ?`, id); err != nil {
		t.Fatalf("age groom fact %d: %v", id, err)
	}
}

func installGroomCodexStub(t *testing.T, answer string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("GROOM_LLM_STUB_LOG", logPath)
	threadID := "019f3041-cfed-7e82-8766-b5ca75cf92da"
	transcript := `{"type":"thread.started","thread_id":"` + threadID + `"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":` + strconv.Quote(answer) + `}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`
	script := "#!/bin/sh\nprintf 'call\\n' >> \"$GROOM_LLM_STUB_LOG\"\ncat <<'GROOM_EOF'\n" + transcript + "\nGROOM_EOF\n"
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func groomStubCalls(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	return len(strings.Fields(string(body)))
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
