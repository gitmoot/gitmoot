package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// skillOptABFixture installs a "planner" template (its current version is the
// champion), adds one pending challenger version with distinct content, and
// registers a codex agent bound to the template. It returns the home, the
// store, and the champion/challenger version ids.
func skillOptABFixture(t *testing.T) (string, *db.Store, string, string) {
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
	champ, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	// Add a pending challenger with different content so the two answers differ.
	challengerTmpl := cliSkillOptTemplate("planner", "Challenger guidance, stronger and more actionable.")
	challengerVersion, err := store.AddPendingAgentTemplateVersion(ctx, challengerTmpl)
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion: %v", err)
	}

	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "planner-bot",
		Role:           "ask",
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     "last",
		RepoScope:      "",
		TemplateID:     "planner",
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	return home, store, champ.VersionID, challengerVersion.ID
}

// withFakeSkillOptABDeliver swaps the delivery seam for a fake that returns a
// fixed answer per variant, keyed by which template instructions the prompt
// carries (champion vs challenger), so the test never touches a live runtime and
// stays deterministic. It restores the seam on cleanup.
func withFakeSkillOptABDeliver(t *testing.T, championAnswer, challengerAnswer string) {
	t.Helper()
	prev := skillOptABDeliver
	skillOptABDeliver = func(_ context.Context, _ runtime.Agent, prompt string) (string, error) {
		if strings.Contains(prompt, "Challenger guidance") {
			return challengerAnswer, nil
		}
		return championAnswer, nil
	}
	t.Cleanup(func() { skillOptABDeliver = prev })
}

// TestRunSkillOptABRecordsValidPairwiseEvent is the core contract test: a pick
// writes exactly one RankedFeedbackEvent that store.UpsertRankedFeedbackEvent
// ACCEPTED (the 2-option/ranking==OptionsCount/all-labels-covered gate), tagged
// source=skillopt-ab with a skillopt-ab: run id, two eval_review_options
// (champion/challenger), and BOTH bandit arms incremented. Here Option B is the
// challenger (seed 1 swaps) and the human picks B, so the challenger wins.
func TestRunSkillOptABRecordsValidPairwiseEvent(t *testing.T) {
	home, store, championID, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	// seed 1 -> rng.Intn(2)==1 -> swap, so Option A=challenger, Option B=champion.
	// We will assert the actual mapping by reading the recorded winner.
	code := runSkillOptAB([]string{"planner-bot", "Plan the migration.", "--home", home, "--pick", "a", "--seed", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}

	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ranked feedback events = %d, want exactly 1", len(events))
	}
	ev := events[0]
	if ev.Source != skillOptABSource {
		t.Fatalf("event source = %q, want %q", ev.Source, skillOptABSource)
	}
	if ev.Reviewer != "human" {
		t.Fatalf("event reviewer = %q, want human", ev.Reviewer)
	}
	// With seed 1 (swap) and pick a, Option A is the challenger, so challenger wins.
	if ev.Winner != skillOptABChallengerLabel {
		t.Fatalf("winner = %q, want %q (seed-1 swap maps pick a -> challenger)", ev.Winner, skillOptABChallengerLabel)
	}

	// Two registered options.
	options, err := store.ListEvalReviewOptions(ctx, runID, skillOptABItemID)
	if err != nil {
		t.Fatalf("ListEvalReviewOptions: %v", err)
	}
	if len(options) != 2 {
		t.Fatalf("eval review options = %d, want 2", len(options))
	}

	// Both bandit arms incremented: challenger won (Beta(2,1)), champion lost (Beta(1,2)).
	champArm, found, err := store.GetBanditArm(ctx, "planner", championID)
	if err != nil || !found {
		t.Fatalf("champion arm: found=%v err=%v", found, err)
	}
	if champArm.Alpha != 1 || champArm.Beta != 2 || champArm.Pulls != 1 {
		t.Fatalf("champion arm = Beta(%.0f,%.0f) pulls=%d, want Beta(1,2) pulls=1", champArm.Alpha, champArm.Beta, champArm.Pulls)
	}
	chalArm, found, err := store.GetBanditArm(ctx, "planner", challengerID)
	if err != nil || !found {
		t.Fatalf("challenger arm: found=%v err=%v", found, err)
	}
	if chalArm.Alpha != 2 || chalArm.Beta != 1 || chalArm.Pulls != 1 {
		t.Fatalf("challenger arm = Beta(%.0f,%.0f) pulls=%d, want Beta(2,1) pulls=1", chalArm.Alpha, chalArm.Beta, chalArm.Pulls)
	}

	if !strings.Contains(stdout.String(), "P(challenger>champion):") {
		t.Fatalf("stdout missing P(>) line:\n%s", stdout.String())
	}
}

// TestRunSkillOptABLabelShuffleMapsPickCorrectly proves the off-by-one-sensitive
// invariant: under BOTH shuffles, picking the option that is actually the
// challenger records challenger-as-winner, and picking the champion option
// records champion-as-winner. We drive the two shuffle outcomes via the seed
// (seed 0 -> no swap: A=champion, B=challenger; seed 1 -> swap: A=challenger,
// B=champion) and assert the recorded winner matches the true role behind the
// pick, never the presented letter.
func TestRunSkillOptABLabelShuffleMapsPickCorrectly(t *testing.T) {
	cases := []struct {
		name       string
		seed       string
		pick       string
		wantWinner string
	}{
		// seed 0: no swap. A=champion, B=challenger.
		{"no-swap pick A is champion", "0", "a", skillOptABChampionLabel},
		{"no-swap pick B is challenger", "0", "b", skillOptABChallengerLabel},
		// seed 1: swap. A=challenger, B=champion.
		{"swap pick A is challenger", "1", "a", skillOptABChallengerLabel},
		{"swap pick B is champion", "1", "b", skillOptABChampionLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, store, _, challengerID := skillOptABFixture(t)
			withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
			ctx := context.Background()

			var stdout, stderr bytes.Buffer
			code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", tc.pick, "--seed", tc.seed}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
			}
			runID := skillOptABRunIDPrefix + challengerID
			events, err := store.ListRankedFeedbackEvents(ctx, runID)
			if err != nil || len(events) != 1 {
				t.Fatalf("events = %d err=%v", len(events), err)
			}
			if events[0].Winner != tc.wantWinner {
				t.Fatalf("winner = %q, want %q", events[0].Winner, tc.wantWinner)
			}
		})
	}
}

// TestRunSkillOptABUnseededShuffleIsNotPinned proves the debiasing fix: on the
// DEFAULT interactive path (no --seed), the label shuffle must NOT be pinned to
// swap=false. Before the fix, options.seed defaulted to 0 and
// rand.NewSource(0).Intn(2) is a constant 0, so Option A was ALWAYS the champion
// and Option B ALWAYS the challenger on every run — a human doing repeated A/Bs
// would learn "Option B is always the new variant", reintroducing exactly the
// position/identity bias the shuffle exists to remove. With a nondeterministic
// (time-based) fallback seed, both shuffle orderings occur, so picking the SAME
// presented letter (a) records the champion on some runs and the challenger on
// others. We assert BOTH winner roles appear across many unseeded runs.
func TestRunSkillOptABUnseededShuffleIsNotPinned(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	ctx := context.Background()

	sawChampion, sawChallenger := false, false
	const runs = 40
	for i := 0; i < runs; i++ {
		var stdout, stderr bytes.Buffer
		// NO --seed: the shuffle seed must be nondeterministic. Always pick a.
		code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "a"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
		}
	}

	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	for _, ev := range events {
		switch ev.Winner {
		case skillOptABChampionLabel:
			sawChampion = true
		case skillOptABChallengerLabel:
			sawChallenger = true
		}
	}
	if !sawChampion || !sawChallenger {
		t.Fatalf("unseeded shuffle is pinned: over %d runs with pick a, sawChampion=%v sawChallenger=%v (want both); the default-0 seed would pin swap=false", runs, sawChampion, sawChallenger)
	}
}

// TestRunSkillOptABRepeatedPicksEachPersist proves the contract-row fix: repeated
// A/Bs of the SAME challenger each write a DISTINCT RankedFeedbackEvent instead of
// overwriting a single row. The ranked_feedback_events conflict key is
// (run_id, item_id, reviewer, source, source_url); run_id (skillopt-ab:<versionId>),
// item_id (ab), reviewer (human), and source (skillopt-ab) are all constant across
// picks, so without a unique per-pick source_url the ON CONFLICT DO UPDATE would
// overwrite every prior preference and only the last pick would survive as evidence
// — silently losing all earlier human preferences the optimizer/audit consumes.
func TestRunSkillOptABRepeatedPicksEachPersist(t *testing.T) {
	home, store, _, challengerID := skillOptABFixture(t)
	withFakeSkillOptABDeliver(t, "Champion answer.", "Challenger answer.")
	ctx := context.Background()

	const picks = 5
	for i := 0; i < picks; i++ {
		var stdout, stderr bytes.Buffer
		// Fixed seed so each pick records the challenger as winner (seed 1 swaps:
		// A=challenger, pick a -> challenger wins). The point is row COUNT, not role.
		code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "a", "--seed", "1"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("pick %d: runSkillOptAB exit = %d, stderr: %s", i, code, stderr.String())
		}
	}

	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != picks {
		t.Fatalf("ranked feedback events = %d after %d picks, want %d (each pick must persist a distinct contract row)", len(events), picks, picks)
	}
	// Every row must carry a distinct source_url (the per-pick uniqueness key).
	seen := make(map[string]struct{}, len(events))
	for _, ev := range events {
		if strings.TrimSpace(ev.SourceURL) == "" {
			t.Fatalf("pick row has empty source_url; the conflict key would collapse repeated picks")
		}
		if _, dup := seen[ev.SourceURL]; dup {
			t.Fatalf("duplicate source_url %q across picks; rows would overwrite", ev.SourceURL)
		}
		seen[ev.SourceURL] = struct{}{}
	}

	// The bandit arm still accrues one pull per pick (it is keyed per (template,
	// version), not per pick, and is unchanged by the source_url fix).
	chalArm, found, err := store.GetBanditArm(ctx, "planner", challengerID)
	if err != nil || !found {
		t.Fatalf("challenger arm: found=%v err=%v", found, err)
	}
	if chalArm.Pulls != picks {
		t.Fatalf("challenger arm pulls = %d, want %d", chalArm.Pulls, picks)
	}
}

// TestRunSkillOptABNoChallengerIsCleanNoOp proves the honest no-op: with NO
// pending challenger, the command exits 0, writes no eval run and no bandit arm,
// and prints the "nothing to A/B" message.
func TestRunSkillOptABNoChallengerIsCleanNoOp(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := store.UpsertAgentTemplate(ctx, cliSkillOptTemplate("planner", "Only champion.")); err != nil {
		t.Fatalf("UpsertAgentTemplate: %v", err)
	}
	champ, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "planner-bot", Role: "ask", Runtime: runtime.CodexRuntime, RuntimeRef: "last", TemplateID: "planner", AutonomyPolicy: runtime.AutonomyPolicyReadOnly}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	store.Close()

	// The seam must NEVER be called when there is nothing to A/B.
	called := false
	prev := skillOptABDeliver
	skillOptABDeliver = func(_ context.Context, _ runtime.Agent, _ string) (string, error) {
		called = true
		return "x", nil
	}
	t.Cleanup(func() { skillOptABDeliver = prev })

	var stdout, stderr bytes.Buffer
	code := runSkillOptAB([]string{"planner-bot", "Plan it.", "--home", home, "--pick", "a"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSkillOptAB exit = %d, stderr: %s", code, stderr.String())
	}
	if called {
		t.Fatal("delivery seam was called despite no challenger")
	}
	if !strings.Contains(stdout.String(), "nothing to A/B") {
		t.Fatalf("stdout missing no-op message:\n%s", stdout.String())
	}

	store2, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	if _, found, err := store2.GetBanditArm(ctx, "planner", champ.VersionID); err != nil || found {
		t.Fatalf("expected no bandit arm written, found=%v err=%v", found, err)
	}
	runID := skillOptABRunIDPrefix + "anything"
	if events, err := store2.ListRankedFeedbackEvents(ctx, runID); err != nil || len(events) != 0 {
		t.Fatalf("expected no ranked events, got %d err=%v", len(events), err)
	}
}
