package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// jurorErrorSentinel is the per-family raw value that makes the fake judge seam
// return an ERROR for that juror (to exercise the per-juror fail-safe drop).
const jurorErrorSentinel = "__error__"

// withFakeSkillOptABJury swaps the judge seam for a fake that returns a per-family
// pick (keyed by the juror's runtime family) and stubs the authed-runtimes probe.
// A family mapped to jurorErrorSentinel returns an error (a dropped juror). It
// records the families the seam was actually invoked on. The codex agent under test
// must never be judged (cross-family only).
func withFakeSkillOptABJury(t *testing.T, picks map[string]string, authed map[string]bool) *[]string {
	t.Helper()
	calls := []string{}
	prevDeliver := skillOptABJudgeDeliver
	skillOptABJudgeDeliver = func(_ context.Context, agent runtime.Agent, _ string) (string, error) {
		calls = append(calls, agent.Runtime)
		if agent.Runtime == runtime.CodexRuntime {
			t.Errorf("a juror ran on the SAME family (codex) as the agent under test; cross-family only")
		}
		raw, ok := picks[agent.Runtime]
		if !ok || raw == jurorErrorSentinel {
			return "", fmt.Errorf("simulated juror error on family %s", agent.Runtime)
		}
		return raw, nil
	}
	prevAuthed := skillOptABJudgeAuthedRuntimes
	skillOptABJudgeAuthedRuntimes = func(string) reviewAuthedRuntimes {
		return func(context.Context) map[string]bool { return authed }
	}
	t.Cleanup(func() {
		skillOptABJudgeDeliver = prevDeliver
		skillOptABJudgeAuthedRuntimes = prevAuthed
	})
	return &calls
}

// TestRunSkillOptABJuryRecordsAggregateAndJurorRows proves the #349 contract: with
// jury-size 2 and two distinct cross-families authed, BOTH jurors judge the SAME
// blind A/B, the aggregated verdict is recorded under the canonical skillopt-ab-judge
// tag, EACH juror's pick is recorded under the distinct skillopt-ab-juror source,
// and the human row coexists. The jury adds NO bandit pull.
func TestRunSkillOptABJuryRecordsAggregateAndJurorRows(t *testing.T) {
	home, store, championID, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	// Both jurors pick Option A. seed 1 -> swap -> Option A is the challenger, so the
	// aggregate (2:0) winner is the challenger.
	calls := withFakeSkillOptABJury(t, map[string]string{
		runtime.ClaudeRuntime: `{"pick":"a"}`,
		runtime.KimiRuntime:   `{"pick":"a"}`,
	}, map[string]bool{runtime.ClaudeRuntime: true, runtime.KimiRuntime: true})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	// Human picks Option B (champion under seed-1 swap) so human != jury.
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "b", "--seed", "1", "--jury-size", "2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("judge seam invoked %d times, want 2 (one per distinct-family juror): %v", len(*calls), *calls)
	}

	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	// human + aggregate judge + 2 juror rows = 4.
	var human, aggregate int
	jurors := map[string]string{}
	for _, ev := range events {
		switch {
		case ev.Reviewer == "human" && ev.Source == skillOptABSource:
			human++
		case ev.Reviewer == skillOptABJudgeReviewer && ev.Source == skillOptABJudgeSource:
			aggregate++
			if ev.Winner != skillOptABChallengerLabel {
				t.Fatalf("aggregate winner = %q, want %q (2:0 jury for the challenger)", ev.Winner, skillOptABChallengerLabel)
			}
		case ev.Source == skillOptABJurorSource:
			jurors[ev.Reviewer] = ev.Winner
		default:
			t.Fatalf("unexpected row reviewer=%q source=%q", ev.Reviewer, ev.Source)
		}
	}
	if human != 1 {
		t.Fatalf("human rows = %d, want 1", human)
	}
	if aggregate != 1 {
		t.Fatalf("aggregate judge rows = %d, want exactly 1 (canonical skillopt-ab-judge tag)", aggregate)
	}
	if len(jurors) != 2 {
		t.Fatalf("juror rows = %d, want 2 distinct-family jurors: %v", len(jurors), jurors)
	}
	for _, fam := range []string{runtime.ClaudeRuntime, runtime.KimiRuntime} {
		reviewer := skillOptABJurorReviewerPrefix + fam
		if jurors[reviewer] != skillOptABChallengerLabel {
			t.Fatalf("juror %s winner = %q, want %q", reviewer, jurors[reviewer], skillOptABChallengerLabel)
		}
	}

	// Jury metadata rides the eval_review_item (no contract bump).
	meta := juryMetadataFor(t, store, runID)
	if meta["jury"] != true {
		t.Fatalf("eval_review_item metadata missing jury flag: %v", meta)
	}
	if meta["jury_disagreement"] != false {
		t.Fatalf("expected no disagreement on a 2:0 jury, metadata: %v", meta)
	}

	// MANUAL PROMOTION preserved: only the human pick moved the bandit (1 pull each).
	champArm, _, _ := store.GetBanditArm(ctx, "planner", championID)
	chalArm, _, _ := store.GetBanditArm(ctx, "planner", challengerID)
	if champArm.Pulls != 1 || chalArm.Pulls != 1 {
		t.Fatalf("bandit pulls champ=%d chal=%d, want 1/1 (the jury must NOT move the bandit)", champArm.Pulls, chalArm.Pulls)
	}
}

// TestRunSkillOptABJuryDisagreementFlag proves a split jury (1:1) records the
// disagreement flag in the item metadata, prints the route-to-human notice, and
// the aggregate resolves to the fail-safe champion (baseline).
func TestRunSkillOptABJuryDisagreementFlag(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	// claude picks A (challenger), kimi picks B (champion) -> 1:1 split.
	withFakeSkillOptABJury(t, map[string]string{
		runtime.ClaudeRuntime: `{"pick":"a"}`,
		runtime.KimiRuntime:   `{"pick":"b"}`,
	}, map[string]bool{runtime.ClaudeRuntime: true, runtime.KimiRuntime: true})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--seed", "1", "--jury-size", "2", "--judge-only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "DISAGREEMENT") {
		t.Fatalf("stdout missing the disagreement route-to-human notice:\n%s", stdout.String())
	}

	runID := skillOptABRunIDPrefix + challengerID
	meta := juryMetadataFor(t, store, runID)
	if meta["jury_disagreement"] != true {
		t.Fatalf("expected jury_disagreement=true on a 1:1 split, metadata: %v", meta)
	}

	// The aggregate resolves to the fail-safe champion on a tie.
	events, _ := store.ListRankedFeedbackEvents(ctx, runID)
	for _, ev := range events {
		if ev.Reviewer == skillOptABJudgeReviewer && ev.Source == skillOptABJudgeSource {
			if ev.Winner != skillOptABChampionLabel {
				t.Fatalf("aggregate winner = %q, want %q (1:1 tie is fail-safe to the champion)", ev.Winner, skillOptABChampionLabel)
			}
		}
	}
}

// TestRunSkillOptABJuryJurorFailureFailSafe proves the per-juror fail-safe: a juror
// that errors is DROPPED and the jury proceeds with the survivor — no error, the
// surviving juror's row + the aggregate are still recorded.
func TestRunSkillOptABJuryJurorFailureFailSafe(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	// kimi errors (dropped); claude survives and picks A (challenger).
	withFakeSkillOptABJury(t, map[string]string{
		runtime.ClaudeRuntime: `{"pick":"a"}`,
		runtime.KimiRuntime:   jurorErrorSentinel,
	}, map[string]bool{runtime.ClaudeRuntime: true, runtime.KimiRuntime: true})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--seed", "1", "--jury-size", "2", "--judge-only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	var aggregate, jurorRows int
	for _, ev := range events {
		switch {
		case ev.Reviewer == skillOptABJudgeReviewer && ev.Source == skillOptABJudgeSource:
			aggregate++
		case ev.Source == skillOptABJurorSource:
			jurorRows++
		}
	}
	if aggregate != 1 {
		t.Fatalf("aggregate rows = %d, want 1 (survivor still aggregated)", aggregate)
	}
	if jurorRows != 1 {
		t.Fatalf("juror rows = %d, want 1 (the failed kimi juror is dropped)", jurorRows)
	}
}

// TestRunSkillOptABJuryGracefulDegradationToSingleJudge proves that when fewer than
// 2 distinct cross-families are available, the jury FALLS BACK to the single-judge
// path: ONE skillopt-ab-judge row, NO skillopt-ab-juror rows.
func TestRunSkillOptABJuryGracefulDegradationToSingleJudge(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	// Only claude authed -> PickCrossFamilyJury returns 1 -> fall back to single judge.
	withFakeSkillOptABJury(t, map[string]string{
		runtime.ClaudeRuntime: `{"pick":"a"}`,
	}, map[string]bool{runtime.ClaudeRuntime: true})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--seed", "1", "--jury-size", "3", "--judge-only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "falling back to a single judge") {
		t.Fatalf("stdout missing the single-judge fallback notice:\n%s", stdout.String())
	}
	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("rows = %d, want exactly 1 (single judge fallback, no juror rows)", len(events))
	}
	if events[0].Reviewer != skillOptABJudgeReviewer || events[0].Source != skillOptABJudgeSource {
		t.Fatalf("the single row must be the canonical judge row, got reviewer=%q source=%q", events[0].Reviewer, events[0].Source)
	}
}

// TestRunSkillOptABJuryOffByDefaultNoJuryRuns proves OFF-BY-DEFAULT: with --judge on
// but NO jury size configured (size defaults to 1), the SINGLE-judge path runs and
// NO skillopt-ab-juror row is ever written. This test FAILS if a jury runs by
// default (any juror row, or more than one judge invocation).
func TestRunSkillOptABJuryOffByDefaultNoJuryRuns(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	calls := withFakeSkillOptABJury(t, map[string]string{
		runtime.ClaudeRuntime: `{"pick":"a"}`,
		runtime.KimiRuntime:   `{"pick":"a"}`,
	}, map[string]bool{runtime.ClaudeRuntime: true, runtime.KimiRuntime: true})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	// --judge (single), NO --jury-size, NO config: the single-judge path only.
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--seed", "1", "--judge-only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("judge seam invoked %d times, want exactly 1 (single judge; a jury would fan out): %v", len(*calls), *calls)
	}
	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("rows = %d, want exactly 1 single judge row (no jury by default)", len(events))
	}
	for _, ev := range events {
		if ev.Source == skillOptABJurorSource {
			t.Fatalf("a juror row was written on the off-by-default path: %+v", ev)
		}
	}
}

// TestRunSkillOptABJuryConfigKnobEnables proves the persistent config knob
// (mode_b_jury_size) admits the jury with NO --jury-size flag (and even with no
// --judge flag — a configured jury implies the judge seam).
func TestRunSkillOptABJuryConfigKnobEnables(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	calls := withFakeSkillOptABJury(t, map[string]string{
		runtime.ClaudeRuntime: `{"pick":"a"}`,
		runtime.KimiRuntime:   `{"pick":"a"}`,
	}, map[string]bool{runtime.ClaudeRuntime: true, runtime.KimiRuntime: true})

	paths := config.PathsForHome(home)
	cfg, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, append(cfg, []byte("\n[skillopt]\nmode_b_jury_size = 2\n")...), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	policy, err := config.LoadSkillOptPolicy(paths)
	if err != nil || policy.ModeBJurySize != 2 {
		t.Fatalf("ModeBJurySize = %d err=%v, want 2 (config wiring broken)", policy.ModeBJurySize, err)
	}

	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	// NO --judge, NO --jury-size: the config knob alone admits the jury.
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "a", "--seed", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("judge seam invoked %d times, want 2 (jury via config knob): %v", len(*calls), *calls)
	}
	runID := skillOptABRunIDPrefix + challengerID
	var jurorRows int
	events, _ := store.ListRankedFeedbackEvents(ctx, runID)
	for _, ev := range events {
		if ev.Source == skillOptABJurorSource {
			jurorRows++
		}
	}
	if jurorRows != 2 {
		t.Fatalf("juror rows = %d, want 2 (jury enabled by config knob)", jurorRows)
	}
}

// juryMetadataFor reads the run's single eval_review_item MetadataJSON (where the
// jury aggregation rides) as a generic map.
func juryMetadataFor(t *testing.T, store *db.Store, runID string) map[string]any {
	t.Helper()
	items, err := store.ListEvalReviewItems(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListEvalReviewItems: %v", err)
	}
	meta := map[string]any{}
	for _, item := range items {
		if item.ItemID != skillOptABJuryItemID {
			continue
		}
		if raw := strings.TrimSpace(item.MetadataJSON); raw != "" {
			if err := json.Unmarshal([]byte(raw), &meta); err != nil {
				t.Fatalf("unmarshal item metadata %q: %v", raw, err)
			}
		}
		return meta
	}
	t.Fatalf("no dedicated jury item (%s) among %d items", skillOptABJuryItemID, len(items))
	return meta
}
