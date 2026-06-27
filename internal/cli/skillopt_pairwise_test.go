package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// skillOptPairwiseHome installs a "planner" template (its current version is the
// promoted champion) so the import has a real template/version to reference, and
// returns the home, the store, and the champion version id.
func skillOptPairwiseHome(t *testing.T) (string, *db.Store, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.UpsertAgentTemplate(ctx, cliSkillOptTemplate("planner", "Champion guidance.")); err != nil {
		t.Fatalf("UpsertAgentTemplate: %v", err)
	}
	tmpl, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	return home, store, tmpl.VersionID
}

// writePairwisePacketDir writes a one-item blinded packet, the matching secret
// map, and a picks file into a temp dir. championIsA controls the A/B placement
// of the champion. It returns the packet dir.
func writePairwisePacketDir(t *testing.T, baseVersionID string, championIsA bool, pick string) string {
	t.Helper()
	championLabel, challengerLabel := "A", "B"
	if !championIsA {
		championLabel, challengerLabel = "B", "A"
	}
	var sideA, sideB string
	if championIsA {
		sideA, sideB = "CHAMPION-RESPONSE", "CHALLENGER-RESPONSE"
	} else {
		sideA, sideB = "CHALLENGER-RESPONSE", "CHAMPION-RESPONSE"
	}
	packet := skillopt.PairwiseReviewPacket{
		Kind:            skillopt.PairwiseReviewPacketKind,
		ContractVersion: skillopt.ContractVersion,
		Mode:            skillopt.PairwiseMode,
		TemplateID:      "planner",
		BaseVersionID:   baseVersionID,
		RunID:           "run-1",
		Items: []skillopt.PairwisePacketItem{
			{
				ItemID:  "item-1",
				Title:   "Item One",
				Prompt:  "Plan the migration.",
				Outputs: []skillopt.PairwisePacketSide{{Label: "A", Response: sideA}, {Label: "B", Response: sideB}},
			},
		},
	}
	secret := skillopt.PairwiseSecretMap{
		Kind:                 skillopt.PairwiseSecretMapKind,
		ContractVersion:      skillopt.ContractVersion,
		RunID:                "run-1",
		TemplateID:           "planner",
		ChampionRole:         "promoted",
		ChallengerRole:       "candidate",
		CandidateContentHash: "sha256:candidatehash",
		Items: []skillopt.PairwiseSecretItem{
			{
				ItemID:          "item-1",
				ChampionLabel:   championLabel,
				ChallengerLabel: challengerLabel,
				Mapping:         map[string]string{championLabel: "promoted", challengerLabel: "candidate"},
			},
		},
	}
	picks := skillopt.PairwisePicks{
		Kind:  skillopt.PairwisePicksKind,
		RunID: "run-1",
		Picks: []skillopt.PairwisePick{{ItemID: "item-1", Pick: pick}},
	}
	dir := t.TempDir()
	writePairwiseJSON(t, filepath.Join(dir, pairwisePacketFileName), packet)
	writePairwiseJSON(t, filepath.Join(dir, pairwiseSecretMapFileName), secret)
	writePairwiseJSON(t, filepath.Join(dir, pairwisePicksFileName), picks)
	return dir
}

func writePairwiseJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRunSkillOptPairwiseImportUnblindOrientations is the consumer-side analogue
// of the unblind test: it runs the FULL CLI import for both A->champion and
// A->challenger orientations and asserts the persisted RankedFeedbackEvent names
// the correct winner. It would FAIL if the unblind were inverted.
func TestRunSkillOptPairwiseImportUnblindOrientations(t *testing.T) {
	cases := []struct {
		name        string
		championIsA bool
		pick        string
		wantWinner  string
	}{
		{name: "A champion, pick A -> champion wins", championIsA: true, pick: "A", wantWinner: skillOptABChampionLabel},
		{name: "A champion, pick B -> challenger wins", championIsA: true, pick: "B", wantWinner: skillOptABChallengerLabel},
		{name: "A challenger, pick A -> challenger wins", championIsA: false, pick: "A", wantWinner: skillOptABChallengerLabel},
		{name: "A challenger, pick B -> champion wins", championIsA: false, pick: "B", wantWinner: skillOptABChampionLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, store, championID := skillOptPairwiseHome(t)
			dir := writePairwisePacketDir(t, championID, tc.championIsA, tc.pick)
			var stdout, stderr bytes.Buffer
			code := runSkillOptPairwise([]string{"import", dir, "--home", home}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("import exit = %d, stderr: %s", code, stderr.String())
			}
			runID := skillOptPairwiseRunIDPrefix + "run-1"
			events, err := store.ListRankedFeedbackEvents(context.Background(), runID)
			if err != nil {
				t.Fatalf("ListRankedFeedbackEvents: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			ev := events[0]
			if ev.Source != skillOptPairwiseSource {
				t.Fatalf("source = %q, want %q", ev.Source, skillOptPairwiseSource)
			}
			if ev.Winner != tc.wantWinner {
				t.Fatalf("winner = %q, want %q", ev.Winner, tc.wantWinner)
			}
			if ev.ItemID != "item-1" {
				t.Fatalf("item id = %q, want item-1", ev.ItemID)
			}
		})
	}
}

// TestRunSkillOptPairwiseImportForeignPicksRunID asserts a picks file whose
// run_id names a DIFFERENT pairwise run is rejected before any unblind, even when
// the packet and secret map agree on the run and the items share generic ids.
// Picks are the artifact that decides each winner, so a silent item_id-only join
// against foreign preferences would unblind the wrong winners; the guard makes the
// import fail (exit 1) and persist nothing.
func TestRunSkillOptPairwiseImportForeignPicksRunID(t *testing.T) {
	home, store, championID := skillOptPairwiseHome(t)
	dir := writePairwisePacketDir(t, championID, true, "A")
	// Overwrite the picks file so its run_id points at a foreign run while keeping
	// the same generic item_id the packet/secret map use.
	foreignPicks := skillopt.PairwisePicks{
		Kind:  skillopt.PairwisePicksKind,
		RunID: "run-OTHER",
		Picks: []skillopt.PairwisePick{{ItemID: "item-1", Pick: "A"}},
	}
	writePairwiseJSON(t, filepath.Join(dir, pairwisePicksFileName), foreignPicks)

	var stdout, stderr bytes.Buffer
	code := runSkillOptPairwise([]string{"import", dir, "--home", home}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("foreign-picks import exit = %d, want 1 (rejected); stderr: %s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("picks run_id")) {
		t.Fatalf("stderr = %q, want a picks run_id mismatch error", stderr.String())
	}
	events, err := store.ListRankedFeedbackEvents(context.Background(), skillOptPairwiseRunIDPrefix+"run-1")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0: a foreign-picks import must persist nothing", len(events))
	}
}

// TestRunSkillOptPairwiseImportIdempotent asserts re-importing the same reviewed
// packet does NOT double-count: the stable per-item source_url keeps the conflict
// key identical so the row is upserted in place.
func TestRunSkillOptPairwiseImportIdempotent(t *testing.T) {
	home, store, championID := skillOptPairwiseHome(t)
	dir := writePairwisePacketDir(t, championID, true, "A")
	runID := skillOptPairwiseRunIDPrefix + "run-1"
	for i := 0; i < 3; i++ {
		var stdout, stderr bytes.Buffer
		if code := runSkillOptPairwise([]string{"import", dir, "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("import %d exit non-zero: %s", i, stderr.String())
		}
	}
	events, err := store.ListRankedFeedbackEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("after 3 imports events = %d, want exactly 1 (no double count)", len(events))
	}
}

// TestRunSkillOptPairwiseImportNoPromotion asserts ingestion writes feedback only
// and NEVER promotes: the template's current promoted version is unchanged and no
// pending candidate version was created or promoted.
func TestRunSkillOptPairwiseImportNoPromotion(t *testing.T) {
	home, store, championID := skillOptPairwiseHome(t)
	ctx := context.Background()
	dir := writePairwisePacketDir(t, championID, true, "B") // challenger preferred
	var stdout, stderr bytes.Buffer
	if code := runSkillOptPairwise([]string{"import", dir, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("import exit non-zero: %s", stderr.String())
	}
	tmpl, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	if tmpl.VersionID != championID {
		t.Fatalf("current version changed to %q (want unchanged %q): import must not promote", tmpl.VersionID, championID)
	}
	pending, err := store.ListPendingAgentTemplateVersions(ctx, "planner")
	if err != nil {
		t.Fatalf("ListPendingAgentTemplateVersions: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending versions = %d, want 0: import must not create/promote versions", len(pending))
	}
}

// TestRunSkillOptPairwiseImportPartialPacket asserts a packet with a good item
// plus an item lacking a pick and an item lacking a secret entry imports the good
// item and reports the others per item without aborting. The exit code is 1
// (partial) but the good item's event is persisted.
func TestRunSkillOptPairwiseImportPartialPacket(t *testing.T) {
	home, store, championID := skillOptPairwiseHome(t)
	ctx := context.Background()
	packet := skillopt.PairwiseReviewPacket{
		Kind:            skillopt.PairwiseReviewPacketKind,
		ContractVersion: skillopt.ContractVersion,
		Mode:            skillopt.PairwiseMode,
		TemplateID:      "planner",
		BaseVersionID:   championID,
		RunID:           "run-1",
		Items: []skillopt.PairwisePacketItem{
			{ItemID: "good", Outputs: []skillopt.PairwisePacketSide{{Label: "A", Response: "champ"}, {Label: "B", Response: "chal"}}},
			{ItemID: "no-pick", Outputs: []skillopt.PairwisePacketSide{{Label: "A", Response: "x"}, {Label: "B", Response: "y"}}},
			{ItemID: "no-secret", Outputs: []skillopt.PairwisePacketSide{{Label: "A", Response: "x"}, {Label: "B", Response: "y"}}},
		},
	}
	secret := skillopt.PairwiseSecretMap{
		Kind:            skillopt.PairwiseSecretMapKind,
		ContractVersion: skillopt.ContractVersion,
		RunID:           "run-1",
		Items: []skillopt.PairwiseSecretItem{
			{ItemID: "good", ChampionLabel: "A", ChallengerLabel: "B", Mapping: map[string]string{"A": "promoted", "B": "candidate"}},
			{ItemID: "no-pick", ChampionLabel: "A", ChallengerLabel: "B", Mapping: map[string]string{"A": "promoted", "B": "candidate"}},
		},
	}
	picks := skillopt.PairwisePicks{Picks: []skillopt.PairwisePick{{ItemID: "good", Pick: "A"}, {ItemID: "no-secret", Pick: "A"}}}
	dir := t.TempDir()
	writePairwiseJSON(t, filepath.Join(dir, pairwisePacketFileName), packet)
	writePairwiseJSON(t, filepath.Join(dir, pairwiseSecretMapFileName), secret)
	writePairwiseJSON(t, filepath.Join(dir, pairwisePicksFileName), picks)

	var stdout, stderr bytes.Buffer
	code := runSkillOptPairwise([]string{"import", dir, "--home", home, "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("partial import exit = %d, want 1", code)
	}
	var summary pairwiseImportSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v\n%s", err, stdout.String())
	}
	if summary.Imported != 1 || summary.Skipped != 2 {
		t.Fatalf("summary imported=%d skipped=%d, want 1/2", summary.Imported, summary.Skipped)
	}
	runID := skillOptPairwiseRunIDPrefix + "run-1"
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want exactly 1 (only the good item)", len(events))
	}
	if events[0].ItemID != "good" {
		t.Fatalf("persisted item = %q, want good", events[0].ItemID)
	}
}

// TestRunSkillOptPairwiseImportDistinctSource asserts the recorded source/
// feedback_source are the distinct live-pairwise tags, not the single-prompt Mode
// B tags, so val-set feedback is separable from single-prompt A/B.
func TestRunSkillOptPairwiseImportDistinctSource(t *testing.T) {
	home, store, championID := skillOptPairwiseHome(t)
	ctx := context.Background()
	dir := writePairwisePacketDir(t, championID, true, "A")
	var stdout, stderr bytes.Buffer
	if code := runSkillOptPairwise([]string{"import", dir, "--home", home, "--reviewer", "alice"}, &stdout, &stderr); code != 0 {
		t.Fatalf("import exit non-zero: %s", stderr.String())
	}
	runID := skillOptPairwiseRunIDPrefix + "run-1"
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Source != skillOptPairwiseSource {
		t.Fatalf("source = %q, want %q (distinct from %q)", events[0].Source, skillOptPairwiseSource, skillOptABSource)
	}
	if events[0].Reviewer != "alice" {
		t.Fatalf("reviewer = %q, want alice", events[0].Reviewer)
	}
	run, err := store.GetEvalRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetEvalRun: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(run.MetadataJSON), &meta); err != nil {
		t.Fatalf("decode run metadata: %v", err)
	}
	if meta["feedback_source"] != skillOptPairwiseFeedbackSource {
		t.Fatalf("run feedback_source = %v, want %q", meta["feedback_source"], skillOptPairwiseFeedbackSource)
	}
}
