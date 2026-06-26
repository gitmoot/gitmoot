package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// withFakeSkillOptABJudge swaps the off-by-default cross-family judge seam for a
// fake returning a fixed raw output, AND stubs the authed-runtimes probe so
// PickCrossFamilyReviewer yields a DIFFERENT family (claude) than the codex agent
// under test (a cross-family judge, SelfFamily=false). It records whether the
// judge seam was actually invoked and restores both seams on cleanup.
func withFakeSkillOptABJudge(t *testing.T, judgeRaw string, authed map[string]bool) *bool {
	t.Helper()
	called := false
	prevDeliver := skillOptABJudgeDeliver
	skillOptABJudgeDeliver = func(_ context.Context, agent runtime.Agent, _ string) (string, error) {
		called = true
		// The judge MUST run on a different family than the codex agent under test.
		if agent.Runtime == runtime.CodexRuntime {
			t.Errorf("judge ran on the SAME family (codex) as the agent under test; cross-family only")
		}
		return judgeRaw, nil
	}
	prevAuthed := skillOptABJudgeAuthedRuntimes
	skillOptABJudgeAuthedRuntimes = func(string) reviewAuthedRuntimes {
		return func(context.Context) map[string]bool { return authed }
	}
	t.Cleanup(func() {
		skillOptABJudgeDeliver = prevDeliver
		skillOptABJudgeAuthedRuntimes = prevAuthed
	})
	return &called
}

// TestRunSkillOptABJudgeRecordsSeparateRow proves the core #483 contract: with
// --judge ON and a cross-family family (claude) authed, the judge picks A/B from
// the SAME shuffled options and a SINGLE skillopt-ab-judge RankedFeedbackEvent is
// written that COEXISTS with the human source=skillopt-ab row on the same run/item
// (ListRankedFeedbackEvents len==2, distinct reviewer+source). The judge winner
// maps back through the SAME shuffle as the human, and the judge adds NO bandit pull
// (only the human pick moves the arm).
func TestRunSkillOptABJudgeRecordsSeparateRow(t *testing.T) {
	home, store, championID, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	// Judge picks Option A. seed 1 -> swap -> Option A is the challenger, so the
	// judge winner must map to the challenger role (same mapping the human pick uses).
	judgeCalled := withFakeSkillOptABJudge(t, `{"pick":"a"}`, map[string]bool{runtime.ClaudeRuntime: true})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	// --pick b (the human picks Option B = champion under seed-1 swap) so the human
	// and judge winners DIFFER, proving the two rows are independently sourced.
	code := runSkillOptAB([]string{"planner-bot", "Plan the migration.", "--home", home, "--pick", "b", "--seed", "1", "--judge"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if !*judgeCalled {
		t.Fatal("judge seam was not called with --judge on and a cross-family runtime authed")
	}

	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ranked feedback events = %d, want exactly 2 (human + judge coexisting)", len(events))
	}

	var humanWinner, judgeWinner string
	humanSeen, judgeSeen := false, false
	for _, ev := range events {
		switch {
		case ev.Reviewer == "human" && ev.Source == skillOptABSource:
			humanWinner, humanSeen = ev.Winner, true
		case ev.Reviewer == skillOptABJudgeReviewer && ev.Source == skillOptABJudgeSource:
			judgeWinner, judgeSeen = ev.Winner, true
		}
	}
	if !humanSeen {
		t.Fatalf("missing human row (reviewer=human source=%s) among %d events", skillOptABSource, len(events))
	}
	if !judgeSeen {
		t.Fatalf("missing judge row (reviewer=%s source=%s) among %d events", skillOptABJudgeReviewer, skillOptABJudgeSource, len(events))
	}
	// Seed 1 swaps: Option A=challenger, Option B=champion.
	// Human picked b -> champion; judge picked a -> challenger.
	if humanWinner != skillOptABChampionLabel {
		t.Fatalf("human winner = %q, want %q (pick b under seed-1 swap is champion)", humanWinner, skillOptABChampionLabel)
	}
	if judgeWinner != skillOptABChallengerLabel {
		t.Fatalf("judge winner = %q, want %q (pick a under seed-1 swap is challenger)", judgeWinner, skillOptABChallengerLabel)
	}

	// The judge added NO bandit pull: only the human pick moved the arms (1 pull each).
	champArm, found, err := store.GetBanditArm(ctx, "planner", championID)
	if err != nil || !found {
		t.Fatalf("champion arm: found=%v err=%v", found, err)
	}
	if champArm.Pulls != 1 {
		t.Fatalf("champion arm pulls = %d, want 1 (the judge must NOT increment the bandit)", champArm.Pulls)
	}
	chalArm, found, err := store.GetBanditArm(ctx, "planner", challengerID)
	if err != nil || !found {
		t.Fatalf("challenger arm: found=%v err=%v", found, err)
	}
	if chalArm.Pulls != 1 {
		t.Fatalf("challenger arm pulls = %d, want 1 (the judge must NOT increment the bandit)", chalArm.Pulls)
	}
	// Human picked champion under seed-1 swap: champion won (Beta(2,1)), challenger lost (Beta(1,2)).
	if champArm.Alpha != 2 || champArm.Beta != 1 {
		t.Fatalf("champion arm = Beta(%.0f,%.0f), want Beta(2,1) (human picked champion, judge did not move it)", champArm.Alpha, champArm.Beta)
	}
	if chalArm.Alpha != 1 || chalArm.Beta != 2 {
		t.Fatalf("challenger arm = Beta(%.0f,%.0f), want Beta(1,2)", chalArm.Alpha, chalArm.Beta)
	}
}

// TestRunSkillOptABJudgeLabelShuffleMapsPickCorrectly proves the off-by-one guard
// for the JUDGE: under both shuffles, a judge picking the option that is actually
// the challenger records challenger-as-judge-winner, and picking the champion
// option records champion-as-judge-winner — the judge maps back through the SAME
// optionA/optionB mapping as the human, asserting the recorded ROLE, not the letter.
func TestRunSkillOptABJudgeLabelShuffleMapsPickCorrectly(t *testing.T) {
	cases := []struct {
		name       string
		seed       string
		judgePick  string
		wantWinner string
	}{
		// seed 0: no swap. A=champion, B=challenger.
		{"no-swap judge picks A is champion", "0", "a", skillOptABChampionLabel},
		{"no-swap judge picks B is challenger", "0", "b", skillOptABChallengerLabel},
		// seed 1: swap. A=challenger, B=champion.
		{"swap judge picks A is challenger", "1", "a", skillOptABChallengerLabel},
		{"swap judge picks B is champion", "1", "b", skillOptABChampionLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, store, _, challengerID := skillOptABFixture(t)
			withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
			withFakeSkillOptABJudge(t, `{"pick":"`+tc.judgePick+`"}`, map[string]bool{runtime.ClaudeRuntime: true})
			ctx := context.Background()

			var stdout, stderr bytes.Buffer
			// --judge-only: record ONLY the judge row so we read exactly one event.
			code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--seed", tc.seed, "--judge-only"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
			}
			runID := skillOptABRunIDPrefix + challengerID
			events, err := store.ListRankedFeedbackEvents(ctx, runID)
			if err != nil || len(events) != 1 {
				t.Fatalf("events = %d err=%v (want exactly 1 judge row under --judge-only)", len(events), err)
			}
			if events[0].Reviewer != skillOptABJudgeReviewer || events[0].Source != skillOptABJudgeSource {
				t.Fatalf("row reviewer/source = %q/%q, want %q/%q", events[0].Reviewer, events[0].Source, skillOptABJudgeReviewer, skillOptABJudgeSource)
			}
			if events[0].Winner != tc.wantWinner {
				t.Fatalf("judge winner = %q, want %q", events[0].Winner, tc.wantWinner)
			}
		})
	}
}

// TestRunSkillOptABJudgeSelfFamilySkips proves CROSS-FAMILY ONLY: when the only
// authed runtime is the SAME family as the agent under test (codex), the selector
// returns SelfFamily=true and the judge is SKIPPED — no judge row is written, a
// clear "skipping judge" message is printed, and the human path still records its
// single row normally.
func TestRunSkillOptABJudgeSelfFamilySkips(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	// Only codex authed = same family as the planner-bot agent -> SelfFamily skip.
	judgeCalled := withFakeSkillOptABJudge(t, `{"pick":"a"}`, map[string]bool{runtime.CodexRuntime: true})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "a", "--seed", "1", "--judge"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if *judgeCalled {
		t.Fatal("judge seam was called despite no cross-family runtime (same-family must SKIP, never self-judge)")
	}
	if !strings.Contains(stdout.String(), "skipping judge") {
		t.Fatalf("stdout missing 'skipping judge' message:\n%s", stdout.String())
	}

	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ranked feedback events = %d, want exactly 1 (human only; judge skipped)", len(events))
	}
	if events[0].Reviewer != "human" || events[0].Source != skillOptABSource {
		t.Fatalf("the surviving row must be the human row, got reviewer=%q source=%q", events[0].Reviewer, events[0].Source)
	}
}

// TestRunSkillOptABJudgeUnparseableOutputDrops proves the fail-safe: a garbled /
// empty / tie judge output drops the judge result (no judge row, no error) and the
// human path still records normally.
func TestRunSkillOptABJudgeUnparseableOutputDrops(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"prose only", "I think they are both good, hard to say."},
		{"no pick field", `{"verdict":"a"}`},
		{"invalid pick value", `{"pick":"tie"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, store, _, challengerID := skillOptABFixture(t)
			withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
			withFakeSkillOptABJudge(t, tc.raw, map[string]bool{runtime.ClaudeRuntime: true})
			ctx := context.Background()

			var stdout, stderr bytes.Buffer
			code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "a", "--seed", "1", "--judge"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
			}
			runID := skillOptABRunIDPrefix + challengerID
			events, err := store.ListRankedFeedbackEvents(ctx, runID)
			if err != nil {
				t.Fatalf("ListRankedFeedbackEvents: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("ranked feedback events = %d, want exactly 1 (human only; garbled judge dropped)", len(events))
			}
			if events[0].Reviewer != "human" {
				t.Fatalf("the surviving row must be the human row, got reviewer=%q", events[0].Reviewer)
			}
		})
	}
}

// TestRunSkillOptABJudgeOffByDefaultIsByteIdentical proves OFF-BY-DEFAULT: with
// neither --judge nor the config knob set, the judge seam is NEVER called, no judge
// row is written, and the stored result (the single human row + the bandit state)
// is identical to the #473 human-only path. A judge seam that errors if invoked is
// installed to assert it is never touched on the off path.
func TestRunSkillOptABJudgeOffByDefaultIsByteIdentical(t *testing.T) {
	home, store, championID, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")

	// The judge seam (and the authed probe) must NEVER be touched on the off path.
	prevDeliver := skillOptABJudgeDeliver
	skillOptABJudgeDeliver = func(context.Context, runtime.Agent, string) (string, error) {
		t.Fatal("judge seam was called on the OFF-BY-DEFAULT path")
		return "", nil
	}
	prevProbe := skillOptABJudgeAuthedRuntimes
	skillOptABJudgeAuthedRuntimes = func(string) reviewAuthedRuntimes {
		t.Fatal("authed-runtimes probe was called on the OFF-BY-DEFAULT path")
		return nil
	}
	t.Cleanup(func() {
		skillOptABJudgeDeliver = prevDeliver
		skillOptABJudgeAuthedRuntimes = prevProbe
	})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	// NO --judge, NO mode_b_judge_enabled config: byte-identical to #473.
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "a", "--seed", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}

	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ranked feedback events = %d, want exactly 1 (human only; no judge row on the off path)", len(events))
	}
	if events[0].Reviewer != "human" || events[0].Source != skillOptABSource {
		t.Fatalf("off path must record only the human row, got reviewer=%q source=%q", events[0].Reviewer, events[0].Source)
	}
	// Bandit state matches the #473 human-only path (seed-1 swap, pick a -> challenger wins).
	chalArm, found, err := store.GetBanditArm(ctx, "planner", challengerID)
	if err != nil || !found {
		t.Fatalf("challenger arm: found=%v err=%v", found, err)
	}
	if chalArm.Alpha != 2 || chalArm.Beta != 1 || chalArm.Pulls != 1 {
		t.Fatalf("challenger arm = Beta(%.0f,%.0f) pulls=%d, want Beta(2,1) pulls=1", chalArm.Alpha, chalArm.Beta, chalArm.Pulls)
	}
	champArm, _, _ := store.GetBanditArm(ctx, "planner", championID)
	if champArm.Pulls != 1 {
		t.Fatalf("champion arm pulls = %d, want 1", champArm.Pulls)
	}
}

// TestRunSkillOptABJudgeConfigKnobEnables proves the config admission gate: with
// [skillopt].mode_b_judge_enabled=true in the home config and NO --judge flag, the
// judge path runs and records its separate row (the persistent knob is equivalent
// to the per-invocation flag).
func TestRunSkillOptABJudgeConfigKnobEnables(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	judgeCalled := withFakeSkillOptABJudge(t, `{"pick":"a"}`, map[string]bool{runtime.ClaudeRuntime: true})

	// Append the config knob to the home's config file.
	paths := config.PathsForHome(home)
	cfg, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, append(cfg, []byte("\n[skillopt]\nmode_b_judge_enabled = true\n")...), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Confirm the policy parses on (guards the config wiring directly).
	policy, err := config.LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy: %v", err)
	}
	if !policy.ModeBJudgeEnabled {
		t.Fatalf("ModeBJudgeEnabled = false after setting the knob; config wiring is broken")
	}

	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	// NO --judge flag: the config knob alone must admit the judge.
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "b", "--seed", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if !*judgeCalled {
		t.Fatal("judge seam was not called with the config knob on (no --judge flag)")
	}
	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ranked feedback events = %d, want 2 (human + judge via config knob)", len(events))
	}
}

// TestParseSkillOptABJudgePick unit-tests the lenient brace-balanced pick parser:
// it accepts a top-level pick, a gitmoot_result-nested pick, and a pick wrapped in
// surrounding prose; it rejects empty/garbled/ambiguous output (fail-safe drop).
func TestParseSkillOptABJudgePick(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"top level a", `{"pick":"a"}`, "a", true},
		{"top level b uppercase", `{"pick":"B"}`, "b", true},
		{"nested gitmoot_result", `{"gitmoot_result":{"pick":"a"}}`, "a", true},
		{"nested metadata", `{"gitmoot_result":{"metadata":{"pick":"b"}}}`, "b", true},
		{"wrapped in prose", "Here is my verdict:\n```json\n{\"pick\":\"a\"}\n```\nThanks!", "a", true},
		{"empty", "", "", false},
		{"prose only", "Option A seems better but it's close.", "", false},
		{"no pick field", `{"winner":"a"}`, "", false},
		{"invalid value", `{"pick":"both"}`, "", false},
		{"tie", `{"pick":"tie"}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSkillOptABJudgePick(tc.raw)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseSkillOptABJudgePick(%q) = (%q,%v), want (%q,%v)", tc.raw, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
