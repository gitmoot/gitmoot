package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// TestClassifySynthItem exhaustively covers the pure accept/reject decision and
// every diagnostic branch.
func TestClassifySynthItem(t *testing.T) {
	cases := []struct {
		name       string
		verdict    synthJudgeVerdict
		gap        float64
		wantAccept bool
		wantDiag   string
	}{
		{
			name:       "accept strong beats weak",
			verdict:    synthJudgeVerdict{WeakScore: 0.3, StrongScore: 0.9, WellFormed: true},
			gap:        0.2,
			wantAccept: true,
		},
		{
			name:     "context leak honored from judge",
			verdict:  synthJudgeVerdict{WeakScore: 0.2, StrongScore: 0.95, WellFormed: true, Diagnostic: "context_leak"},
			gap:      0.2,
			wantDiag: synthDiagContextLeak,
		},
		{
			name:     "not well formed is bad rubric",
			verdict:  synthJudgeVerdict{WeakScore: 0.2, StrongScore: 0.9, WellFormed: false},
			gap:      0.2,
			wantDiag: synthDiagBadRubric,
		},
		{
			name:     "both fail is too hard",
			verdict:  synthJudgeVerdict{WeakScore: 0.1, StrongScore: 0.3, WellFormed: true},
			gap:      0.2,
			wantDiag: synthDiagTooHard,
		},
		{
			name:     "weak solves strong fails is strong_failed",
			verdict:  synthJudgeVerdict{WeakScore: 0.8, StrongScore: 0.4, WellFormed: true},
			gap:      0.2,
			wantDiag: synthDiagStrongFail,
		},
		{
			name:     "weak already solves is too easy",
			verdict:  synthJudgeVerdict{WeakScore: 0.7, StrongScore: 0.95, WellFormed: true},
			gap:      0.2,
			wantDiag: synthDiagTooEasy,
		},
		{
			name:     "gap too small is too easy",
			verdict:  synthJudgeVerdict{WeakScore: 0.5, StrongScore: 0.65, WellFormed: true},
			gap:      0.2,
			wantDiag: synthDiagTooEasy,
		},
		{
			name:       "custom gap threshold accepts smaller gap",
			verdict:    synthJudgeVerdict{WeakScore: 0.5, StrongScore: 0.65, WellFormed: true},
			gap:        0.1,
			wantAccept: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accept, diag := classifySynthItem(tc.verdict, tc.gap)
			if accept != tc.wantAccept {
				t.Fatalf("accept = %v, want %v", accept, tc.wantAccept)
			}
			if !accept && diag != tc.wantDiag {
				t.Fatalf("diagnostic = %q, want %q", diag, tc.wantDiag)
			}
			if accept && diag != "" {
				t.Fatalf("accepted item returned diagnostic %q", diag)
			}
		})
	}
}

func TestExtractSynthJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `{"a":1}`, `{"a":1}`},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"prose", "Sure, here it is: {\"a\":1} done.", `{"a":1}`},
		{"nested", `{"a":{"b":2},"c":3}`, `{"a":{"b":2},"c":3}`},
		{"brace in string", `{"a":"}{"}`, `{"a":"}{"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractSynthJSONObject(tc.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := extractSynthJSONObject("no json here"); err == nil {
		t.Fatal("expected error for text with no object")
	}
}

// synthTestHome creates an isolated temp home with a store, an installed template,
// and the named agents registered so `skillopt synth` can resolve them.
func synthTestHome(t *testing.T, agents ...string) (string, *db.Store) {
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
	if err := store.UpsertAgentTemplate(ctx, cliSkillOptTemplate("planner", "Plan software migrations well.")); err != nil {
		t.Fatalf("UpsertAgentTemplate: %v", err)
	}
	for _, name := range agents {
		if err := store.UpsertAgent(ctx, db.Agent{
			Name:           name,
			Role:           "ask",
			Runtime:        runtime.CodexRuntime,
			RuntimeRef:     "last",
			TemplateID:     "planner",
			AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
		}); err != nil {
			t.Fatalf("UpsertAgent %s: %v", name, err)
		}
	}
	return home, store
}

// withScriptedSynthDeliver installs a deterministic delivery seam that keys off
// the prompt content to return challenger/weak/strong/judge answers, so the whole
// loop runs with NO LLM. It restores the seam on cleanup.
func withScriptedSynthDeliver(t *testing.T, challenger, weak, strong, judge string) {
	t.Helper()
	prev := skillOptSynthDeliver
	t.Cleanup(func() { skillOptSynthDeliver = prev })
	// The weak/strong attempts share an identical prompt, so key those off the
	// runtime.Agent name; the challenger/judge are keyed off their prompt text.
	skillOptSynthDeliver = func(_ context.Context, agent runtime.Agent, prompt string) (string, error) {
		switch {
		case strings.Contains(prompt, "generating a synthetic review item"):
			return challenger, nil
		case strings.Contains(prompt, "Score two answers against a rubric"):
			return judge, nil
		case strings.Contains(prompt, "Answer the following question"):
			if agent.Name == "weak-bot" {
				return weak, nil
			}
			return strong, nil
		default:
			return "", nil
		}
	}
}

// TestRunSkillOptSynthAcceptAndApprove is the deterministic no-LLM E2E: a
// challenger produces a valid item, the strong agent beats the weak agent, the
// judge confirms it, the item is persisted pending_human_approval with a file,
// and the human gate flips it to approved.
func TestRunSkillOptSynthAcceptAndApprove(t *testing.T) {
	home, store := synthTestHome(t, "weak-bot", "strong-bot", "judge-bot")
	withScriptedSynthDeliver(t,
		`{"context":"A legacy monolith with no tests.","question":"Outline a safe migration.","rubric":"Rewards incremental strangler-fig steps."}`,
		"Just rewrite it all at once.",
		"Wrap the monolith, migrate endpoints incrementally behind a facade, add tests first.",
		`{"weak_score":0.2,"strong_score":0.9,"well_formed":true,"diagnostic":""}`,
	)
	outDir := filepath.Join(t.TempDir(), "synth-out")

	var stdout, stderr bytes.Buffer
	code := runSkillOptSynth([]string{
		"--template", "planner", "--repo", "acme/widgets",
		"--weak", "weak-bot", "--strong", "strong-bot", "--judge", "judge-bot",
		"--max-items", "1", "--out", outDir, "--home", home,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("synth exit = %d, stderr: %s", code, stderr.String())
	}

	pending, err := store.ListSynthReviewItems(context.Background(), db.SynthItemStatusPending)
	if err != nil {
		t.Fatalf("ListSynthReviewItems: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending items = %d, want 1", len(pending))
	}
	item := pending[0]
	if item.Status != db.SynthItemStatusPending {
		t.Fatalf("status = %q, want pending_human_approval", item.Status)
	}
	if item.StrongScore <= item.WeakScore {
		t.Fatalf("strong score %.2f not greater than weak %.2f", item.StrongScore, item.WeakScore)
	}
	if !strings.Contains(item.Rubric, "strangler-fig") {
		t.Fatalf("rubric not persisted: %q", item.Rubric)
	}
	// The accepted item file exists on disk.
	if _, err := os.Stat(filepath.Join(outDir, item.ID+".json")); err != nil {
		t.Fatalf("item file missing: %v", err)
	}

	// Human gate: approve flips the status.
	var aout, aerr bytes.Buffer
	if code := runSkillOptSynth([]string{"approve", item.ID, "--home", home}, &aout, &aerr); code != 0 {
		t.Fatalf("approve exit = %d, stderr: %s", code, aerr.String())
	}
	got, ok, err := store.GetSynthReviewItem(context.Background(), item.ID)
	if err != nil || !ok {
		t.Fatalf("GetSynthReviewItem: ok=%v err=%v", ok, err)
	}
	if got.Status != db.SynthItemStatusApproved {
		t.Fatalf("status after approve = %q, want approved", got.Status)
	}
	// Double-approve is rejected (already approved, not pending).
	var bout, berr bytes.Buffer
	if code := runSkillOptSynth([]string{"approve", item.ID, "--home", home}, &bout, &berr); code == 0 {
		t.Fatalf("second approve should fail (item no longer pending)")
	}
}

// TestRunSkillOptSynthRejectsTooEasyAndExhaustsRounds proves a non-discriminating
// item is skipped with a too_easy diagnostic, no DB row is written, and the round
// cap is honored.
func TestRunSkillOptSynthRejectsTooEasyAndExhaustsRounds(t *testing.T) {
	home, store := synthTestHome(t, "weak-bot", "strong-bot")
	withScriptedSynthDeliver(t,
		`{"context":"2+2","question":"What is 2+2?","rubric":"Rewards the answer 4."}`,
		"4",
		"4",
		`{"weak_score":0.9,"strong_score":0.95,"well_formed":true,"diagnostic":""}`,
	)
	outDir := filepath.Join(t.TempDir(), "synth-out")

	var stdout, stderr bytes.Buffer
	code := runSkillOptSynth([]string{
		"--template", "planner", "--repo", "acme/widgets",
		"--weak", "weak-bot", "--strong", "strong-bot",
		"--max-items", "1", "--max-rounds-per-item", "2", "--out", outDir, "--json", "--home", home,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("synth exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), synthDiagTooEasy) {
		t.Fatalf("expected too_easy diagnostic in output: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"rounds": 2`) {
		t.Fatalf("expected the round cap (2) to be exhausted: %s", stdout.String())
	}
	// No item persisted — a too-easy candidate never becomes a pending row.
	all, err := store.ListSynthReviewItems(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSynthReviewItems: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("persisted items = %d, want 0 (rejected candidates are not stored)", len(all))
	}
}

// TestRunSkillOptSynthRejectGate proves the reject side of the human gate.
func TestRunSkillOptSynthRejectGate(t *testing.T) {
	home, store := synthTestHome(t, "weak-bot", "strong-bot", "judge-bot")
	withScriptedSynthDeliver(t,
		`{"context":"ctx","question":"q","rubric":"r"}`,
		"weak answer",
		"strong answer",
		`{"weak_score":0.1,"strong_score":0.8,"well_formed":true,"diagnostic":""}`,
	)
	outDir := filepath.Join(t.TempDir(), "synth-out")
	var stdout, stderr bytes.Buffer
	if code := runSkillOptSynth([]string{
		"--template", "planner", "--repo", "acme/widgets",
		"--weak", "weak-bot", "--strong", "strong-bot", "--judge", "judge-bot",
		"--max-items", "1", "--out", outDir, "--home", home,
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("synth exit = %d, stderr: %s", code, stderr.String())
	}
	pending, _ := store.ListSynthReviewItems(context.Background(), db.SynthItemStatusPending)
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	id := pending[0].ID
	var rout, rerr bytes.Buffer
	if code := runSkillOptSynth([]string{"reject", id, "--home", home}, &rout, &rerr); code != 0 {
		t.Fatalf("reject exit = %d, stderr: %s", code, rerr.String())
	}
	got, ok, _ := store.GetSynthReviewItem(context.Background(), id)
	if !ok || got.Status != db.SynthItemStatusRejected {
		t.Fatalf("status after reject = %q (ok=%v), want rejected", got.Status, ok)
	}
}

// TestRunSkillOptSynthMissingFlags proves required-flag validation and that an
// unknown agent errors cleanly.
// TestResolveSynthAgentResolvesCheckoutPath proves the review fix for #535: a
// resolved agent carries the registered repo's filesystem CheckoutPath in
// WorkingDir (so a real, non-stubbed delivery chdirs into an existing directory)
// while RepoScope stays in "owner/repo" form. When the repo is not registered,
// WorkingDir stays empty so the delivery seam falls back to RepoScope.
func TestResolveSynthAgentResolvesCheckoutPath(t *testing.T) {
	home, store := synthTestHome(t, "weak-bot")
	_ = home
	ctx := context.Background()
	checkout := t.TempDir()
	if err := store.UpsertRepo(ctx, db.Repo{
		Owner:         "acme",
		Name:          "widgets",
		DefaultBranch: "main",
		CheckoutPath:  checkout,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}

	agent, err := resolveSynthAgent(ctx, store, "weak-bot", "acme/widgets")
	if err != nil {
		t.Fatalf("resolveSynthAgent: %v", err)
	}
	if agent.RepoScope != "acme/widgets" {
		t.Fatalf("RepoScope = %q, want acme/widgets (must stay owner/repo form)", agent.RepoScope)
	}
	if agent.WorkingDir != checkout {
		t.Fatalf("WorkingDir = %q, want resolved checkout %q", agent.WorkingDir, checkout)
	}

	// Unregistered repo: WorkingDir stays empty so the seam falls back to RepoScope.
	other, err := resolveSynthAgent(ctx, store, "weak-bot", "acme/unregistered")
	if err != nil {
		t.Fatalf("resolveSynthAgent (unregistered): %v", err)
	}
	if other.WorkingDir != "" {
		t.Fatalf("WorkingDir = %q, want empty for unregistered repo", other.WorkingDir)
	}
}

func TestRunSkillOptSynthMissingFlags(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runSkillOptSynth([]string{"--template", "planner", "--home", home}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2 for missing flags", code)
	}
	if !strings.Contains(stderr.String(), "missing required flags") {
		t.Fatalf("expected missing-flags error, got: %s", stderr.String())
	}
}
