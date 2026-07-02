package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// TestSkillOptAgreementStatsExactKappa pins the agreement math to
// hand-computed fixtures — the exact numbers the E2E later asserts through the
// real CLI. Cohen's kappa: po = observed agreement, pe = chance agreement from
// the two raters' label marginals, kappa = (po - pe) / (1 - pe).
func TestSkillOptAgreementStatsExactKappa(t *testing.T) {
	cases := []struct {
		name          string
		pairs         [][2]string
		wantN         int
		wantAgree     int
		wantAgreement float64
		wantKappa     float64
		wantDefined   bool
	}{
		{
			// The canonical 3-agree/1-disagree fixture: judge marginals
			// champion=3/challenger=1, human marginals champion=2/challenger=2.
			// po=0.75, pe=(3*2+1*2)/16=0.5, kappa=(0.75-0.5)/0.5=0.5.
			name: "three agree one disagree",
			pairs: [][2]string{
				{"champion", "champion"},
				{"champion", "champion"},
				{"challenger", "challenger"},
				{"champion", "challenger"},
			},
			wantN: 4, wantAgree: 3, wantAgreement: 0.75, wantKappa: 0.5, wantDefined: true,
		},
		{
			// Agreement at chance level: judge always champion, human split.
			// po=0.5, pe=(2*1+0*1)/4=0.5, kappa=0.
			name:  "chance-level agreement is kappa zero",
			pairs: [][2]string{{"champion", "champion"}, {"champion", "challenger"}},
			wantN: 2, wantAgree: 1, wantAgreement: 0.5, wantKappa: 0, wantDefined: true,
		},
		{
			// Perfect agreement with both labels used: po=1, pe=0.5, kappa=1.
			name:  "perfect agreement",
			pairs: [][2]string{{"champion", "champion"}, {"challenger", "challenger"}},
			wantN: 2, wantAgree: 2, wantAgreement: 1, wantKappa: 1, wantDefined: true,
		},
		{
			// Degenerate: both raters used ONE identical label everywhere, so
			// pe=1; kappa is reported as 1 only because po is also perfect.
			name:  "single shared label degenerates to kappa one",
			pairs: [][2]string{{"champion", "champion"}, {"champion", "champion"}},
			wantN: 2, wantAgree: 2, wantAgreement: 1, wantKappa: 1, wantDefined: true,
		},
		{
			// Total systematic disagreement: po=0, pe=0, kappa=0 — chance
			// correction does not reward a judge that inverts every human pick.
			name:  "total disagreement",
			pairs: [][2]string{{"champion", "challenger"}, {"champion", "challenger"}},
			wantN: 2, wantAgree: 0, wantAgreement: 0, wantKappa: 0, wantDefined: true,
		},
		{
			// Multi-class: labels a/b/c. po=0.5; both marginals a=2,b=1,c=1 so
			// pe=(2*2+1*1+1*1)/16=0.375; kappa=(0.5-0.375)/0.625=0.2.
			name: "multi-class kappa",
			pairs: [][2]string{
				{"a", "a"},
				{"b", "b"},
				{"c", "a"},
				{"a", "c"},
			},
			wantN: 4, wantAgree: 2, wantAgreement: 0.5, wantKappa: 0.2, wantDefined: true,
		},
		{
			name:  "empty",
			pairs: nil,
			wantN: 0, wantAgree: 0, wantAgreement: 0, wantKappa: 0, wantDefined: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats := skillOptAgreementStats(tc.pairs)
			if stats.N != tc.wantN || stats.Agreements != tc.wantAgree {
				t.Fatalf("N=%d agreements=%d, want N=%d agreements=%d", stats.N, stats.Agreements, tc.wantN, tc.wantAgree)
			}
			if math.Abs(stats.Agreement-tc.wantAgreement) > 1e-12 {
				t.Fatalf("agreement = %v, want %v", stats.Agreement, tc.wantAgreement)
			}
			if stats.KappaDefined != tc.wantDefined {
				t.Fatalf("kappa defined = %v (note %q), want %v", stats.KappaDefined, stats.KappaNote, tc.wantDefined)
			}
			if tc.wantDefined && math.Abs(stats.Kappa-tc.wantKappa) > 1e-12 {
				t.Fatalf("kappa = %v, want %v", stats.Kappa, tc.wantKappa)
			}
		})
	}
}

// TestSkillOptAgreementMajority pins the per-item vote collapse: strict
// majority wins, ties and empty sides are not resolvable.
func TestSkillOptAgreementMajority(t *testing.T) {
	if winner, ok := skillOptAgreementMajority([]string{"champion", "challenger", "champion"}); !ok || winner != "champion" {
		t.Fatalf("majority = %q ok=%v, want champion true", winner, ok)
	}
	if _, ok := skillOptAgreementMajority([]string{"champion", "challenger"}); ok {
		t.Fatal("tie must not resolve to a winner")
	}
	if _, ok := skillOptAgreementMajority(nil); ok {
		t.Fatal("empty votes must not resolve to a winner")
	}
}

// TestSkillOptABJudgePickPositionRoundTrip proves the additive position
// persistence channel: the writer's blob parses back to the same pick, and
// legacy/free-text/invalid reasoning is skipped (never fabricated).
func TestSkillOptABJudgePickPositionRoundTrip(t *testing.T) {
	for _, pick := range []string{"a", "b"} {
		blob := skillOptABJudgePickPositionJSON(pick)
		got, ok := skillOptABJudgePickPosition(blob)
		if !ok || got != pick {
			t.Fatalf("round trip %q -> %q ok=%v (blob %q)", pick, got, ok, blob)
		}
	}
	for _, reasoning := range []string{"", "the answer was clearer", `{"judge_pick_position":"c"}`, `{"other":"a"}`, "{"} {
		if got, ok := skillOptABJudgePickPosition(reasoning); ok {
			t.Fatalf("reasoning %q parsed to position %q, want skipped", reasoning, got)
		}
	}
}

// TestBuildSkillOptJudgeAgreementPairwiseMajorityAndTies covers the join
// edge-paths without the CLI: repeated rows on one item collapse to a majority
// (never inflating N), an internally-tied side skips the item and counts it,
// and auto-trace rows are excluded from both sides.
func TestBuildSkillOptJudgeAgreementPairwiseMajorityAndTies(t *testing.T) {
	event := func(runID, itemID, winner, reviewer, source, sourceURL string) db.RankedFeedbackEventWithTemplate {
		return db.RankedFeedbackEventWithTemplate{
			RankedFeedbackEvent: db.RankedFeedbackEvent{RunID: runID, ItemID: itemID, Winner: winner, Reviewer: reviewer, Source: source, SourceURL: sourceURL},
			TemplateID:          "planner",
		}
	}
	events := []db.RankedFeedbackEventWithTemplate{
		// Item 1: three human picks collapse to champion (2:1); judge says
		// champion -> ONE agreeing observation, not three.
		event("run-1", "ab", "champion", "human", "skillopt-ab", "u1"),
		event("run-1", "ab", "champion", "human", "skillopt-ab", "u2"),
		event("run-1", "ab", "challenger", "human", "skillopt-ab", "u3"),
		event("run-1", "ab", "champion", "skillopt-ab-judge", "skillopt-ab-judge", "j1"),
		// Item 2: human side is tied 1:1 -> skipped and counted.
		event("run-2", "ab", "champion", "human", "skillopt-ab", "u4"),
		event("run-2", "ab", "challenger", "human", "skillopt-ab", "u5"),
		event("run-2", "ab", "challenger", "skillopt-ab-judge", "skillopt-ab-judge", "j2"),
		// Auto-trace rows never join either side.
		event("run-3", "ab", "champion", "gitmoot-auto", "auto-trace", "u6"),
		event("run-3", "ab", "champion", "skillopt-ab-judge", "skillopt-ab-judge", "j3"),
	}
	pairwise := buildSkillOptJudgeAgreementPairwise(events)
	if pairwise.N != 1 || pairwise.Agreements != 1 {
		t.Fatalf("N=%d agreements=%d, want 1/1 (majority collapse, tie skipped, auto-trace excluded)", pairwise.N, pairwise.Agreements)
	}
	if pairwise.TiesSkipped != 1 {
		t.Fatalf("ties skipped = %d, want 1", pairwise.TiesSkipped)
	}
	if pairwise.HumanRows != 5 || pairwise.JudgeRows != 3 {
		t.Fatalf("human rows = %d judge rows = %d, want 5 and 3", pairwise.HumanRows, pairwise.JudgeRows)
	}
	if pairwise.LegacyRowsExcluded != 0 {
		t.Fatalf("legacy rows excluded = %d, want 0 (non-skillopt-ab runs need no comparison token)", pairwise.LegacyRowsExcluded)
	}
}

// TestSkillOptABComparisonTokenRoundTrip pins the per-comparison join channel:
// all three writers (human pick, judge, juror) embed the SAME invocation token
// and the extractor recovers it; legacy pre-token SourceURLs (the old per-row
// "#<nano>-<seq>" suffixes) must NOT parse — they are the unmeasurable rows the
// agreement harness excludes instead of pooling.
func TestSkillOptABComparisonTokenRoundTrip(t *testing.T) {
	token := skillOptABComparisonToken()
	if strings.TrimSpace(token) == "" {
		t.Fatal("comparison token is empty")
	}
	if second := skillOptABComparisonToken(); second == token {
		t.Fatalf("two invocations minted the same comparison token %q", token)
	}
	for name, url := range map[string]string{
		"human pick": skillOptABPickSourceURL("planner@v2", token),
		"judge":      skillOptABJudgeSourceURL("planner@v2", token),
		"juror":      skillOptABJurorSourceURL("planner@v2", "claude", token),
	} {
		got, ok := skillOptABComparisonTokenFromSourceURL(url)
		if !ok || got != token {
			t.Fatalf("%s url %q -> token %q ok=%v, want %q", name, url, got, ok, token)
		}
	}
	for _, legacy := range []string{
		"skillopt-ab:planner@v2#1750000000000-1",
		"skillopt-ab:planner@v2:judge#1750000000000-2",
		"skillopt-ab:planner@v2:juror:claude#1750000000000-3",
		"",
		"skillopt-ab:planner@v2#cmp:",
	} {
		if got, ok := skillOptABComparisonTokenFromSourceURL(legacy); ok {
			t.Fatalf("legacy url %q parsed to token %q, want unmeasurable", legacy, got)
		}
	}
}

// TestBuildSkillOptJudgeAgreementPairwisePerComparisonAndLegacy pins the
// cross-item-contamination fix at the unit level: on a skillopt-ab run the
// bucket is (run_id, item_id, comparison_token) — two comparisons of the SAME
// challenger stay two observations (the old (run_id, item_id) pooling would
// collapse all six rows below to N=1 agreements=0 by comparing side
// majorities across different comparisons) — and legacy tokenless rows are
// excluded and counted, never pooled.
func TestBuildSkillOptJudgeAgreementPairwisePerComparisonAndLegacy(t *testing.T) {
	event := func(winner, reviewer, source, sourceURL string) db.RankedFeedbackEventWithTemplate {
		return db.RankedFeedbackEventWithTemplate{
			RankedFeedbackEvent: db.RankedFeedbackEvent{RunID: "skillopt-ab:planner@v2", ItemID: "ab", Winner: winner, Reviewer: reviewer, Source: source, SourceURL: sourceURL},
			TemplateID:          "planner",
		}
	}
	events := []db.RankedFeedbackEventWithTemplate{
		// Comparison c1: judge and human agree (champion).
		event("champion", "human", "skillopt-ab", "skillopt-ab:planner@v2#cmp:c1"),
		event("champion", "skillopt-ab-judge", "skillopt-ab-judge", "skillopt-ab:planner@v2:judge#cmp:c1"),
		// Comparison c2 (same run, same item id): judge and human disagree —
		// its OWN observation, never merged into c1's bucket.
		event("challenger", "human", "skillopt-ab", "skillopt-ab:planner@v2#cmp:c2"),
		event("champion", "skillopt-ab-judge", "skillopt-ab-judge", "skillopt-ab:planner@v2:judge#cmp:c2"),
		// Legacy rows (pre-token per-row suffixes): unmeasurable, excluded, counted.
		event("challenger", "human", "skillopt-ab", "skillopt-ab:planner@v2#1750000000000-1"),
		event("challenger", "skillopt-ab-judge", "skillopt-ab-judge", "skillopt-ab:planner@v2:judge#1750000000000-2"),
	}
	pairwise := buildSkillOptJudgeAgreementPairwise(events)
	if pairwise.N != 2 || pairwise.Agreements != 1 {
		t.Fatalf("N=%d agreements=%d, want 2/1 (per-comparison buckets; pooled join would give 1/0)", pairwise.N, pairwise.Agreements)
	}
	if pairwise.LegacyRowsExcluded != 2 {
		t.Fatalf("legacy rows excluded = %d, want 2", pairwise.LegacyRowsExcluded)
	}
	if pairwise.JudgeRows != 2 || pairwise.HumanRows != 2 {
		t.Fatalf("judge rows = %d human rows = %d, want 2 and 2 (legacy rows never enter the join)", pairwise.JudgeRows, pairwise.HumanRows)
	}
}

// TestBuildSkillOptJudgeAgreementPositionAssignmentCorrected pins the
// position-bias estimator's two key properties under an IMBALANCED (3:1) but
// non-degenerate position assignment, where the raw |P(pick=a) - 0.5| would
// fabricate a bias for a content-driven judge:
//
//   - a purely content-driven judge (always picks the champion, wherever it is
//     presented) measures bias 0 even though P(pick=a)=0.75;
//   - a purely position-driven judge (always picks Option A) measures the full
//     0.5 bias.
//
// The champion's presented position is recovered per row from the unblinded
// Winner and the recorded raw pick: champion at A iff (Winner==champion) ==
// (pick=="a").
func TestBuildSkillOptJudgeAgreementPositionAssignmentCorrected(t *testing.T) {
	judgeRow := func(winner, pick, sourceURL string) db.RankedFeedbackEventWithTemplate {
		return db.RankedFeedbackEventWithTemplate{
			RankedFeedbackEvent: db.RankedFeedbackEvent{
				RunID:     "skillopt-ab:planner@v2",
				ItemID:    "ab",
				Winner:    winner,
				Reviewer:  "skillopt-ab-judge",
				Source:    "skillopt-ab-judge",
				SourceURL: sourceURL,
				Reasoning: skillOptABJudgePickPositionJSON(pick),
			},
			TemplateID: "planner",
		}
	}
	t.Run("content-driven judge measures zero bias", func(t *testing.T) {
		events := []db.RankedFeedbackEventWithTemplate{
			// Champion at A on three comparisons (pick a = champion)...
			judgeRow("champion", "a", "skillopt-ab:planner@v2:judge#cmp:c1"),
			judgeRow("champion", "a", "skillopt-ab:planner@v2:judge#cmp:c2"),
			judgeRow("champion", "a", "skillopt-ab:planner@v2:judge#cmp:c3"),
			// ...champion at B on one (pick b = champion): same content preference.
			judgeRow("champion", "b", "skillopt-ab:planner@v2:judge#cmp:c4"),
		}
		position := buildSkillOptJudgeAgreementPairwise(events).Position
		if position == nil {
			t.Fatal("position audit missing")
		}
		if position.N != 4 || position.PickA != 3 || position.PPickA != 0.75 {
			t.Fatalf("position = %+v, want N=4 pick_a=3 p_pick_a=0.75", position)
		}
		if position.ChampionA != 3 || position.PChampionA != 0.75 {
			t.Fatalf("position = %+v, want champion_a=3 p_champion_a=0.75", position)
		}
		if !position.BiasDefined || position.Bias != 0 {
			t.Fatalf("position = %+v, want DEFINED bias 0 (raw |P(a)-0.5| would fabricate 0.25)", position)
		}
	})
	t.Run("position-driven judge measures full bias", func(t *testing.T) {
		events := []db.RankedFeedbackEventWithTemplate{
			// Same 3:1 assignment; the judge always picks Option A regardless of
			// which role is presented there.
			judgeRow("champion", "a", "skillopt-ab:planner@v2:judge#cmp:c1"),
			judgeRow("champion", "a", "skillopt-ab:planner@v2:judge#cmp:c2"),
			judgeRow("champion", "a", "skillopt-ab:planner@v2:judge#cmp:c3"),
			judgeRow("challenger", "a", "skillopt-ab:planner@v2:judge#cmp:c4"),
		}
		position := buildSkillOptJudgeAgreementPairwise(events).Position
		if position == nil {
			t.Fatal("position audit missing")
		}
		if !position.BiasDefined || position.Bias != 0.5 {
			t.Fatalf("position = %+v, want DEFINED bias 0.5 (always-pick-A)", position)
		}
	})
	t.Run("single-position assignment is undefined", func(t *testing.T) {
		events := []db.RankedFeedbackEventWithTemplate{
			judgeRow("champion", "a", "skillopt-ab:planner@v2:judge#cmp:c1"),
			judgeRow("challenger", "b", "skillopt-ab:planner@v2:judge#cmp:c2"),
		}
		position := buildSkillOptJudgeAgreementPairwise(events).Position
		if position == nil {
			t.Fatal("position audit missing")
		}
		if position.ChampionA != 2 || position.PChampionA != 1.0 {
			t.Fatalf("position = %+v, want champion_a=2 (both rows presented the champion at A)", position)
		}
		if position.BiasDefined || position.BiasNote == "" {
			t.Fatalf("position = %+v, want UNDEFINED bias with a degenerate-assignment note", position)
		}
	})
}

// multiChallengerABFixture builds on skillOptABFixture with three MORE pending
// challenger versions, so the E2E can drive four independent A/B items (each
// challenger keys its own skillopt-ab:<version> run).
func multiChallengerABFixture(t *testing.T) (string, *db.Store, []string) {
	t.Helper()
	home, store, _, firstChallenger := skillOptABFixture(t)
	ctx := context.Background()
	challengers := []string{firstChallenger}
	for i := 2; i <= 4; i++ {
		version, err := store.AddPendingAgentTemplateVersion(ctx, cliSkillOptTemplate("planner", fmt.Sprintf("Challenger guidance variant %d.", i)))
		if err != nil {
			t.Fatalf("AddPendingAgentTemplateVersion %d: %v", i, err)
		}
		challengers = append(challengers, version.ID)
	}
	return home, store, challengers
}

// settableSkillOptABJudgeFake swaps the judge seam for a fake whose raw output
// is read from the returned pointer AT CALL TIME (so each E2E round can steer
// the judge's pick), and stubs the authed probe so a cross-family judge
// (claude vs the codex agent) is always available.
func settableSkillOptABJudgeFake(t *testing.T) *string {
	t.Helper()
	raw := `{"pick":"a"}`
	prevDeliver := skillOptABJudgeDeliver
	skillOptABJudgeDeliver = func(_ context.Context, _ runtime.Agent, _ string) (string, error) {
		return raw, nil
	}
	prevAuthed := skillOptABJudgeAuthedRuntimes
	skillOptABJudgeAuthedRuntimes = func(string) reviewAuthedRuntimes {
		return func(context.Context) map[string]bool { return map[string]bool{runtime.ClaudeRuntime: true} }
	}
	t.Cleanup(func() {
		skillOptABJudgeDeliver = prevDeliver
		skillOptABJudgeAuthedRuntimes = prevAuthed
	})
	return &raw
}

// TestSkillOptJudgeAgreementE2EMeasuresKnownAgreement is the mutation-proven
// E2E for the #344 measurement harness: it drives FOUR real `skillopt ab
// --judge` rounds through the REAL command path (real store writes, real
// shuffle/unblinding, real judge + human row recording — only the LLM Deliver
// seams are fakes), engineered so the judge agrees with the human on exactly
// 3 of 4 comparisons, then runs the REAL `skillopt judge agreement` CLI and
// asserts the hand-computed numbers EXACTLY:
//
//	agreement = 3/4 = 0.750
//	kappa     = (0.75 - 0.5) / (1 - 0.5) = 0.500
//	          (judge marginals champion=3/challenger=1; human 2/2 -> pe=0.5)
//	position  = picks a,a,b,a with the champion presented at A on rounds 1-2
//	          (seed 0) and at B on rounds 3-4 (seed 1):
//	          P(pick=a)=0.750, P(option A=champion)=0.500,
//	          P(pick=a|champion at A)=1.0, P(pick=a|champion at B)=0.5
//	          -> assignment-corrected bias |(1.0+0.5-1)/2| = 0.250 (defined)
//
// Any break in the judge-row/human-row join (wrong field, wrong source tag,
// wrong unblinding, wrong comparison token) shifts these exact numbers and
// turns the test red.
func TestSkillOptJudgeAgreementE2EMeasuresKnownAgreement(t *testing.T) {
	home, _, challengers := multiChallengerABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	judgeRaw := settableSkillOptABJudgeFake(t)

	// The seed pins the label shuffle: seed 0 -> no swap (Option A=champion),
	// seed 1 -> swap (Option A=challenger). Mixing both makes the position
	// assignment balanced, so the assignment-corrected bias is DEFINED.
	rounds := []struct {
		seed      string // label-shuffle seed: 0=champion at A, 1=champion at B
		judgePick string // raw position the judge returns
		humanPick string // raw position the human picks
	}{
		{"0", "a", "a"}, // champion at A: both pick champion -> agree
		{"0", "a", "b"}, // champion at A: judge champion vs human challenger -> disagree
		{"1", "b", "b"}, // champion at B: both pick champion -> agree
		{"1", "a", "a"}, // champion at B: both pick challenger -> agree
	}
	for index, round := range rounds {
		*judgeRaw = fmt.Sprintf(`{"pick":%q}`, round.judgePick)
		var stdout, stderr bytes.Buffer
		code := runSkillOptAB([]string{"planner-bot", "Plan the migration.", "--home", home,
			"--challenger", challengers[index], "--pick", round.humanPick, "--seed", round.seed, "--judge"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("round %d: runSkillOptAB exit = %d, stderr: %s", index+1, code, stderr.String())
		}
	}

	// JSON path: exact machine-readable numbers through the real CLI.
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "judge", "agreement", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt judge agreement exit = %d, stderr: %s", code, stderr.String())
	}
	var report skillOptJudgeAgreementReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.Pairwise.N != 4 || report.Pairwise.Agreements != 3 {
		t.Fatalf("pairwise N=%d agreements=%d, want 4 and 3\n%s", report.Pairwise.N, report.Pairwise.Agreements, stdout.String())
	}
	if report.Pairwise.Agreement != 0.75 {
		t.Fatalf("pairwise agreement = %v, want exactly 0.75", report.Pairwise.Agreement)
	}
	if !report.Pairwise.KappaDefined || report.Pairwise.Kappa != 0.5 {
		t.Fatalf("pairwise kappa = %v (defined=%v), want exactly 0.5", report.Pairwise.Kappa, report.Pairwise.KappaDefined)
	}
	if report.Pairwise.JudgeRows != 4 || report.Pairwise.HumanRows != 4 || report.Pairwise.TiesSkipped != 0 {
		t.Fatalf("rows judge=%d human=%d ties=%d, want 4/4/0", report.Pairwise.JudgeRows, report.Pairwise.HumanRows, report.Pairwise.TiesSkipped)
	}
	if report.Pairwise.LegacyRowsExcluded != 0 {
		t.Fatalf("legacy rows excluded = %d, want 0 (every row was written with a comparison token)", report.Pairwise.LegacyRowsExcluded)
	}
	if len(report.Pairwise.PerSource) != 1 || report.Pairwise.PerSource[0].Key != skillOptABSource || report.Pairwise.PerSource[0].N != 4 {
		t.Fatalf("per-source breakdown = %+v, want a single skillopt-ab entry with N=4", report.Pairwise.PerSource)
	}
	if report.Pairwise.Position == nil {
		t.Fatalf("position audit missing: the judge rows must carry recorded picks\n%s", stdout.String())
	}
	if report.Pairwise.Position.N != 4 || report.Pairwise.Position.PickA != 3 || report.Pairwise.Position.PPickA != 0.75 {
		t.Fatalf("position audit = %+v, want N=4 pick_a=3 p=0.75", report.Pairwise.Position)
	}
	if report.Pairwise.Position.ChampionA != 2 || report.Pairwise.Position.PChampionA != 0.5 {
		t.Fatalf("position assignment = %+v, want champion_a=2 p_champion_a=0.5 (seeds 0,0,1,1)", report.Pairwise.Position)
	}
	if !report.Pairwise.Position.BiasDefined || report.Pairwise.Position.Bias != 0.25 {
		t.Fatalf("position bias = %+v, want defined assignment-corrected bias 0.25", report.Pairwise.Position)
	}
	if report.Candidate.N != 0 {
		t.Fatalf("candidate N = %d, want 0 (no judge outcomes seeded)", report.Candidate.N)
	}
	if !report.SmallNWarning || report.SmallNThreshold != skillOptAgreementSmallNThreshold {
		t.Fatalf("small-N caveat missing: warning=%v threshold=%d", report.SmallNWarning, report.SmallNThreshold)
	}

	// Text path: the human-readable shape carries the same exact numbers, with
	// kappa as the HEADLINE metric and the loud small-N warning.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "judge", "agreement", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt judge agreement (text) exit = %d, stderr: %s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"judge <-> human agreement (#344 measure-the-judge)",
		"pairwise slice",
		"comparisons joined: 4 (judge rows: 4, human rows: 4, juror rows: 0, tied comparisons skipped: 0)",
		"cohen's kappa (headline): 0.500",
		"raw agreement: 0.750 (3/4)",
		"per human source:",
		"skillopt-ab",
		"position audit",
		"N=4  P(pick=a)=0.750  P(option A=champion)=0.500  position bias=0.250 (assignment-corrected)",
		"candidate slice",
		"no judge outcomes captured",
		"WARNING: small sample (pairwise N=4, candidate N=0; threshold 30)",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("text output missing %q\n%s", want, output)
		}
	}
	if strings.Contains(output, "legacy rows excluded") {
		t.Fatalf("text output reports excluded legacy rows on an all-token store\n%s", output)
	}
}

// TestSkillOptJudgeAgreementE2EJoinsPerComparisonNotPerChallenger is the
// regression E2E for the cross-item-contamination bug: (run_id, item_id) alone
// is the CHALLENGER (run "skillopt-ab:<version>", item always "ab"), so
// repeated A/B rounds against the SAME challenger used to pool into ONE bucket
// and compare side majorities across DIFFERENT comparisons. Three real
// `skillopt ab --judge --pick` rounds against the same challenger, judge
// agreeing with the human on exactly 2 of 3 comparisons, used to report N=1,
// agreement=0.000 (the two side majorities disagreed) — a confident number
// computed over fabricated pairings. With the per-invocation comparison token
// the harness must report the true per-comparison numbers:
//
//	N = 3, agreement = 2/3
//	kappa: judge marginals champion=2/challenger=1, human 1/2
//	       -> pe = (2*1 + 1*2)/9 = 4/9, kappa = (2/3 - 4/9)/(5/9) = 0.4
//
// All rounds use --seed 0, which pins the champion at Option A on every run —
// so this E2E ALSO pins the position audit's degenerate-assignment stance: a
// fixed seed makes position bias unmeasurable (undefined), never a fabricated
// |P(pick=a) - 0.5| that actually reflects content preference.
func TestSkillOptJudgeAgreementE2EJoinsPerComparisonNotPerChallenger(t *testing.T) {
	home, _, _, _ := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	judgeRaw := settableSkillOptABJudgeFake(t)

	// Same (sole pending) challenger every round; seed 0 -> champion at A.
	rounds := []struct {
		judgePick string
		humanPick string
	}{
		{"a", "a"}, // both champion -> agree
		{"b", "b"}, // both challenger -> agree
		{"a", "b"}, // judge champion vs human challenger -> disagree
	}
	for index, round := range rounds {
		*judgeRaw = fmt.Sprintf(`{"pick":%q}`, round.judgePick)
		var stdout, stderr bytes.Buffer
		code := runSkillOptAB([]string{"planner-bot", fmt.Sprintf("Plan step %d.", index+1), "--home", home,
			"--pick", round.humanPick, "--seed", "0", "--judge"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("round %d: runSkillOptAB exit = %d, stderr: %s", index+1, code, stderr.String())
		}
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "judge", "agreement", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt judge agreement exit = %d, stderr: %s", code, stderr.String())
	}
	var report skillOptJudgeAgreementReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	// The old pooled join reported N=1 agreements=0 for this exact scenario.
	if report.Pairwise.N != 3 || report.Pairwise.Agreements != 2 {
		t.Fatalf("pairwise N=%d agreements=%d, want 3 and 2 (per-comparison join, not per-challenger pooling)\n%s",
			report.Pairwise.N, report.Pairwise.Agreements, stdout.String())
	}
	if math.Abs(report.Pairwise.Agreement-2.0/3.0) > 1e-9 {
		t.Fatalf("pairwise agreement = %v, want 2/3", report.Pairwise.Agreement)
	}
	if !report.Pairwise.KappaDefined || math.Abs(report.Pairwise.Kappa-0.4) > 1e-9 {
		t.Fatalf("pairwise kappa = %v (defined=%v), want 0.4", report.Pairwise.Kappa, report.Pairwise.KappaDefined)
	}
	if report.Pairwise.JudgeRows != 3 || report.Pairwise.HumanRows != 3 || report.Pairwise.TiesSkipped != 0 || report.Pairwise.LegacyRowsExcluded != 0 {
		t.Fatalf("rows judge=%d human=%d ties=%d legacy=%d, want 3/3/0/0",
			report.Pairwise.JudgeRows, report.Pairwise.HumanRows, report.Pairwise.TiesSkipped, report.Pairwise.LegacyRowsExcluded)
	}
	// Degenerate position assignment (--seed 0 on every round: champion always
	// at Option A): the audit must refuse to fabricate a bias number.
	position := report.Pairwise.Position
	if position == nil {
		t.Fatalf("position audit missing\n%s", stdout.String())
	}
	if position.N != 3 || position.PickA != 2 || position.ChampionA != 3 || position.PChampionA != 1.0 {
		t.Fatalf("position audit = %+v, want N=3 pick_a=2 champion_a=3 p_champion_a=1.0", position)
	}
	if position.BiasDefined || position.BiasNote == "" {
		t.Fatalf("position bias = %+v, want UNDEFINED with a degenerate-assignment note under a fixed --seed", position)
	}

	// Text path: the degenerate stance is loud and the legacy line is absent.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "judge", "agreement", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt judge agreement (text) exit = %d, stderr: %s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"comparisons joined: 3 (judge rows: 3, human rows: 3, juror rows: 0, tied comparisons skipped: 0)",
		"raw agreement: 0.667 (2/3)",
		"cohen's kappa (headline): 0.400",
		"position bias: n/a (degenerate position assignment",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("text output missing %q\n%s", want, output)
		}
	}
}

// TestSkillOptJudgeAgreementCandidateSliceMatchesJudgeReport seeds the SAME
// candidate-outcome fixture the judge-report test uses and asserts the
// candidate slice computes the identical 2x2 kappa: directions AA/AR/AR-human
// give po=2/3, pe=4/9, kappa=(2/3-4/9)/(5/9)=0.4.
func TestSkillOptJudgeAgreementCandidateSliceMatchesJudgeReport(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	for index, outcome := range []db.SkillOptJudgeOutcome{
		{ID: "agree-1", CandidateVersionID: "planner@v2", TemplateID: "planner", HumanDecision: "promoted", Direction: db.SkillOptJudgeDirectionAgreeAccept},
		{ID: "agree-2", CandidateVersionID: "planner@v3", TemplateID: "planner", HumanDecision: "rejected", Direction: db.SkillOptJudgeDirectionAgreeReject},
		{ID: "disagree-1", CandidateVersionID: "planner@v4", TemplateID: "planner", HumanDecision: "rejected", Direction: db.SkillOptJudgeDirectionJudgeAcceptHumanReject},
	} {
		if err := store.InsertSkillOptJudgeOutcome(context.Background(), outcome); err != nil {
			t.Fatalf("InsertSkillOptJudgeOutcome %d returned error: %v", index, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "judge", "agreement", "--home", home, "--template", "planner", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt judge agreement exit = %d, stderr: %s", code, stderr.String())
	}
	var report skillOptJudgeAgreementReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.Template != "planner" {
		t.Fatalf("template = %q, want planner", report.Template)
	}
	if report.Candidate.N != 3 || report.Candidate.Agreements != 2 {
		t.Fatalf("candidate N=%d agreements=%d, want 3 and 2", report.Candidate.N, report.Candidate.Agreements)
	}
	if math.Abs(report.Candidate.Agreement-2.0/3.0) > 1e-9 {
		t.Fatalf("candidate agreement = %v, want 2/3", report.Candidate.Agreement)
	}
	if !report.Candidate.KappaDefined || math.Abs(report.Candidate.Kappa-0.4) > 1e-9 {
		t.Fatalf("candidate kappa = %v (defined=%v), want 0.4", report.Candidate.Kappa, report.Candidate.KappaDefined)
	}
	if report.Pairwise.N != 0 {
		t.Fatalf("pairwise N = %d, want 0 (no ranked feedback seeded)", report.Pairwise.N)
	}
}

// TestSkillOptJudgeAgreementEmptyStore proves the no-data path is calm and
// non-erroring (read-only harness over a fresh home).
func TestSkillOptJudgeAgreementEmptyStore(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "judge", "agreement", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt judge agreement exit = %d, stderr: %s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"no overlap yet",
		"no judge outcomes captured",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("empty-store output missing %q\n%s", want, output)
		}
	}
}
