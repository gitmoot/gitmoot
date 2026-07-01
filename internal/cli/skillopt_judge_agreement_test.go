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
// 3 of 4 items, then runs the REAL `skillopt judge agreement` CLI and asserts
// the hand-computed numbers EXACTLY:
//
//	agreement = 3/4 = 0.750
//	kappa     = (0.75 - 0.5) / (1 - 0.5) = 0.500
//	          (judge marginals champion=3/challenger=1; human 2/2 -> pe=0.5)
//	position  = picks a,a,b,a -> P(a)=0.750, bias 0.250
//
// Any break in the judge-row/human-row join (wrong field, wrong source tag,
// wrong unblinding) shifts these exact numbers and turns the test red.
func TestSkillOptJudgeAgreementE2EMeasuresKnownAgreement(t *testing.T) {
	home, _, challengers := multiChallengerABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	judgeRaw := settableSkillOptABJudgeFake(t)

	// All rounds use --seed 0 (no swap): Option A=champion, Option B=challenger.
	rounds := []struct {
		judgePick string // raw position the judge returns
		humanPick string // raw position the human picks
	}{
		{"a", "a"}, // both champion -> agree
		{"a", "a"}, // both champion -> agree
		{"b", "b"}, // both challenger -> agree
		{"a", "b"}, // judge champion vs human challenger -> disagree
	}
	for index, round := range rounds {
		*judgeRaw = fmt.Sprintf(`{"pick":%q}`, round.judgePick)
		var stdout, stderr bytes.Buffer
		code := runSkillOptAB([]string{"planner-bot", "Plan the migration.", "--home", home,
			"--challenger", challengers[index], "--pick", round.humanPick, "--seed", "0", "--judge"}, &stdout, &stderr)
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
	if len(report.Pairwise.PerSource) != 1 || report.Pairwise.PerSource[0].Key != skillOptABSource || report.Pairwise.PerSource[0].N != 4 {
		t.Fatalf("per-source breakdown = %+v, want a single skillopt-ab entry with N=4", report.Pairwise.PerSource)
	}
	if report.Pairwise.Position == nil {
		t.Fatalf("position audit missing: the judge rows must carry recorded picks\n%s", stdout.String())
	}
	if report.Pairwise.Position.N != 4 || report.Pairwise.Position.PickA != 3 || report.Pairwise.Position.PPickA != 0.75 || report.Pairwise.Position.Bias != 0.25 {
		t.Fatalf("position audit = %+v, want N=4 pick_a=3 p=0.75 bias=0.25", report.Pairwise.Position)
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
		"items joined: 4 (judge rows: 4, human rows: 4, juror rows: 0, tied items skipped: 0)",
		"cohen's kappa (headline): 0.500",
		"raw agreement: 0.750 (3/4)",
		"per human source:",
		"skillopt-ab",
		"position audit",
		"N=4  P(pick=a)=0.750  position bias |P(a)-0.5|=0.250",
		"candidate slice",
		"no judge outcomes captured",
		"WARNING: small sample (pairwise N=4, candidate N=0; threshold 30)",
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
