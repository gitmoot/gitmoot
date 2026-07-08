package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
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

// envelopeClaudeRunner is a subprocess.Runner that returns a fixed claude
// --output-format json envelope for every invocation, mirroring the real claude
// CLI's Start output so a real ClaudeAdapter runs end-to-end with no LLM.
type envelopeClaudeRunner struct{ stdout string }

func (r envelopeClaudeRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{Stdout: r.stdout}, nil
}

func (r envelopeClaudeRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }

// TestRealSkillOptABDeliverUnwrapsClaudeEnvelopeForSynth is the #721 consumer
// regression. It drives the REAL delivery seam (realSkillOptABDeliver) with a
// forked-session claude agent (empty RuntimeRef → adapter.Start, the synth
// challenger path) wired to a real ClaudeAdapter over a fake runner that returns
// a claude CLI JSON envelope. The delivered answer must be the envelope's inner
// "result" text — NOT the whole envelope — so parseSynthGeneratedItem finds the
// context/question/rubric. Before the fix Start leaked the envelope as Raw and
// this parse returned "synth item missing context, question, or rubric"
// (bad_rubric, the live failure).
func TestRealSkillOptABDeliverUnwrapsClaudeEnvelopeForSynth(t *testing.T) {
	inner := `{"context":"A legacy monolith with no tests.","question":"Outline a safe migration.","rubric":"Rewards incremental strangler-fig steps."}`
	envelope := `{"type":"result","subtype":"success","is_error":false,` +
		`"session_id":"550e8400-e29b-41d4-a716-446655440010",` +
		`"result":` + strconv.Quote(inner) + `,` +
		`"usage":{"input_tokens":10,"output_tokens":20}}`

	restore := replaceRuntimeFactory(runtime.Factory{Runner: envelopeClaudeRunner{stdout: envelope}})
	t.Cleanup(restore)

	// Empty RuntimeRef routes realSkillOptABDeliver through adapter.Start (a fresh
	// throwaway session) — the exact skillopt synth challenger delivery path.
	answer, err := realSkillOptABDeliver(context.Background(), runtime.Agent{
		Name:       "challenger-bot",
		Role:       "reviewer",
		Runtime:    runtime.ClaudeRuntime,
		RepoScope:  "acme/widgets",
		RuntimeRef: "",
	}, synthChallengerPrompt("", ""))
	if err != nil {
		t.Fatalf("realSkillOptABDeliver: %v", err)
	}
	if answer != inner {
		t.Fatalf("delivered answer = %q, want the unwrapped inner result %q", answer, inner)
	}
	item, err := parseSynthGeneratedItem(answer)
	if err != nil {
		t.Fatalf("parseSynthGeneratedItem on the delivered answer failed (the #721 bad_rubric bug): %v (answer=%q)", err, answer)
	}
	if item.Context == "" || item.Question == "" || item.Rubric == "" {
		t.Fatalf("parsed synth item missing fields: %+v", item)
	}
}

// transcriptCodexRunner is a subprocess.Runner that returns a fixed `codex exec
// --json` JSONL transcript for every invocation, mirroring the real codex CLI's
// Start output (banner + thread.started + turn events + agent_message) so a real
// CodexAdapter runs end-to-end with no LLM.
type transcriptCodexRunner struct{ stdout string }

func (r transcriptCodexRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{Stdout: r.stdout}, nil
}

func (r transcriptCodexRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }

// TestRealSkillOptABDeliverUnwrapsCodexTranscriptForSynth is the #724 consumer
// regression, the codex flavor of TestRealSkillOptABDeliverUnwrapsClaudeEnvelope-
// ForSynth. It drives the REAL delivery seam (realSkillOptABDeliver) with a
// forked-session codex agent (empty RuntimeRef → adapter.Start, the synth
// challenger path) wired to a real CodexAdapter over a fake runner that returns a
// codex exec --json transcript. The delivered answer must be the agent_message
// text — NOT the whole transcript (banner/thread.started/reasoning/turn events) —
// so parseSynthGeneratedItem finds the context/question/rubric. Before the fix
// Start leaked the whole transcript as Raw and this parse returned the wrong
// object (the thread.started event, not the challenger item).
func TestRealSkillOptABDeliverUnwrapsCodexTranscriptForSynth(t *testing.T) {
	inner := `{"context":"A legacy monolith with no tests.","question":"Outline a safe migration.","rubric":"Rewards incremental strangler-fig steps."}`
	transcript := `{"type":"thread.started","thread_id":"019f3041-cfed-7e82-8766-b5ca75cf92da"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"designing a discriminating item"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":` + strconv.Quote(inner) + `}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":16504,"output_tokens":20}}`

	restore := replaceRuntimeFactory(runtime.Factory{Runner: transcriptCodexRunner{stdout: transcript}})
	t.Cleanup(restore)

	// Empty RuntimeRef routes realSkillOptABDeliver through adapter.Start (a fresh
	// throwaway session) — the exact skillopt synth challenger delivery path.
	answer, err := realSkillOptABDeliver(context.Background(), runtime.Agent{
		Name:       "challenger-bot",
		Role:       "reviewer",
		Runtime:    runtime.CodexRuntime,
		RepoScope:  "acme/widgets",
		RuntimeRef: "",
	}, synthChallengerPrompt("", ""))
	if err != nil {
		t.Fatalf("realSkillOptABDeliver: %v", err)
	}
	if answer != inner {
		t.Fatalf("delivered answer = %q, want the unwrapped agent_message %q", answer, inner)
	}
	item, err := parseSynthGeneratedItem(answer)
	if err != nil {
		t.Fatalf("parseSynthGeneratedItem on the delivered answer failed (the #724 codex-bloat bug): %v (answer=%q)", err, answer)
	}
	if item.Context == "" || item.Question == "" || item.Rubric == "" {
		t.Fatalf("parsed synth item missing fields: %+v", item)
	}
}

// TestSynthJudgePromptCapsAnswers pins the second half of #724: synthJudgePrompt
// caps each embedded weak/strong answer so a verbose runtime answer cannot bloat
// (or, combined, blow ARG_MAX on) the judge exec, while under-limit answers are
// embedded byte-identical with no marker.
func TestSynthJudgePromptCapsAnswers(t *testing.T) {
	item := synthGeneratedItem{Context: "ctx", Question: "q?", Rubric: "rewards X"}

	t.Run("short answers embedded verbatim, no marker", func(t *testing.T) {
		weak, strong := "a short weak answer", "a short strong answer"
		got := synthJudgePrompt(item, weak, strong)
		if !strings.Contains(got, weak) || !strings.Contains(got, strong) {
			t.Fatalf("short answers must be embedded verbatim; prompt=%q", got)
		}
		if strings.Contains(got, "[truncated") {
			t.Fatalf("short answers must not carry a truncation marker; prompt=%q", got)
		}
	})

	t.Run("oversized answers capped with byte-accurate marker", func(t *testing.T) {
		oversize := 5000
		weak := strings.Repeat("W", synthMaxAnswerBytes+oversize)
		strong := strings.Repeat("S", synthMaxAnswerBytes+oversize)
		got := synthJudgePrompt(item, weak, strong)

		if strings.Contains(got, weak) {
			t.Fatal("oversized weak answer must not be embedded verbatim")
		}
		wantMarker := fmt.Sprintf("[truncated %d bytes]", oversize)
		if strings.Count(got, wantMarker) != 2 {
			t.Fatalf("want the byte-accurate marker %q for both answers; prompt tail=%q", wantMarker, got[len(got)-200:])
		}
		// The retained prefix is exactly the cap; nothing beyond it survives.
		if !strings.Contains(got, strings.Repeat("W", synthMaxAnswerBytes)) {
			t.Fatal("capped weak answer must keep the first synthMaxAnswerBytes bytes")
		}
		if strings.Contains(got, strings.Repeat("W", synthMaxAnswerBytes+1)) {
			t.Fatal("capped weak answer kept more than synthMaxAnswerBytes bytes")
		}
	})

	t.Run("answer exactly at the limit is not truncated", func(t *testing.T) {
		exact := strings.Repeat("E", synthMaxAnswerBytes)
		got := synthJudgePrompt(item, exact, "short")
		if strings.Contains(got, "[truncated") {
			t.Fatalf("answer exactly at the limit must not be truncated; prompt=%q", got[len(got)-120:])
		}
		if !strings.Contains(got, exact) {
			t.Fatal("at-limit answer must be embedded verbatim")
		}
	})
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

// TestRunSkillOptSynthDeliversInScratchNotCheckout is the #725 deterministic no-LLM
// E2E: even with the target repo registered to a real filesystem checkout, EVERY
// delivery (challenger/weak/strong/judge) must be handed a fresh per-item temp
// scratch dir as its adapter working dir — never the live checkout or the
// "owner/repo" RepoScope — and those scratch dirs must be cleaned up after the run.
func TestRunSkillOptSynthDeliversInScratchNotCheckout(t *testing.T) {
	home, store := synthTestHome(t, "weak-bot", "strong-bot", "judge-bot")
	ctx := context.Background()
	// Register the repo with a real checkout — the pre-#725 resolveSynthAgent would
	// have handed THIS directory to every delivery.
	checkout := t.TempDir()
	if err := store.UpsertRepo(ctx, db.Repo{
		Owner: "acme", Name: "widgets", DefaultBranch: "main",
		CheckoutPath: checkout, Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}

	// Capturing seam: records the working dir of every delivered agent, then returns
	// scripted answers so the accept path runs to completion with no LLM.
	prev := skillOptSynthDeliver
	t.Cleanup(func() { skillOptSynthDeliver = prev })
	var deliveredDirs []string
	skillOptSynthDeliver = func(_ context.Context, agent runtime.Agent, prompt string) (string, error) {
		deliveredDirs = append(deliveredDirs, agent.WorkingDir)
		switch {
		case strings.Contains(prompt, "generating a synthetic review item"):
			return `{"context":"A legacy monolith.","question":"Outline a safe migration.","rubric":"Rewards incremental steps."}`, nil
		case strings.Contains(prompt, "Score two answers against a rubric"):
			return `{"weak_score":0.2,"strong_score":0.9,"well_formed":true,"diagnostic":""}`, nil
		case strings.Contains(prompt, "Answer the following question"):
			if agent.Name == "weak-bot" {
				return "rewrite it all at once", nil
			}
			return "wrap and migrate incrementally behind a facade", nil
		default:
			return "", nil
		}
	}

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
	if len(deliveredDirs) < 4 {
		t.Fatalf("expected >=4 deliveries (challenger/weak/strong/judge), got %d", len(deliveredDirs))
	}
	var scratchDir string
	for i, dir := range deliveredDirs {
		if strings.TrimSpace(dir) == "" {
			t.Fatalf("delivery %d had an empty working dir — the sandbox did not force a scratch dir", i)
		}
		if dir == checkout {
			t.Fatalf("delivery %d ran in the live checkout %q (the #725 bug)", i, checkout)
		}
		if dir == "acme/widgets" {
			t.Fatalf("delivery %d ran in the RepoScope %q, not a scratch dir", i, dir)
		}
		if !strings.HasPrefix(filepath.Base(dir), "gitmoot-synth-item-") {
			t.Fatalf("delivery %d dir %q is not a per-item synth scratch dir", i, dir)
		}
		// All deliveries for one item share the same per-item scratch dir.
		if scratchDir == "" {
			scratchDir = dir
		} else if dir != scratchDir {
			t.Fatalf("delivery %d used a different scratch dir %q, want the shared per-item %q", i, dir, scratchDir)
		}
	}
	// Cleanup: the per-item scratch dir must be gone after the run.
	if _, err := os.Stat(scratchDir); !os.IsNotExist(err) {
		t.Fatalf("scratch dir %q was not cleaned up after the item (stat err=%v)", scratchDir, err)
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
// TestResolveSynthAgentNeverCarriesCheckout proves the #725 hard guarantee at the
// resolve layer: even when the repo IS registered with a real filesystem checkout,
// a resolved synth agent must NOT carry that checkout in WorkingDir. Every synth
// delivery runs in a fresh per-item temp scratch dir instead (see
// sandboxSynthAgent), so nothing in the synth path may hand an agentic CLI a live
// checkout to write into. RepoScope still stays in "owner/repo" form for adapter
// family resolution.
func TestResolveSynthAgentNeverCarriesCheckout(t *testing.T) {
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
	if agent.WorkingDir != "" {
		t.Fatalf("WorkingDir = %q, want empty — synth agents must never carry the live checkout %q (#725)", agent.WorkingDir, checkout)
	}
}

// TestSandboxSynthAgentForcesScratchDir proves sandboxSynthAgent overrides the
// adapter working dir to the scratch dir regardless of what the agent otherwise
// carries, and leaves RepoScope intact for family resolution.
func TestSandboxSynthAgentForcesScratchDir(t *testing.T) {
	scratch := t.TempDir()
	agent := runtime.Agent{
		Name:           "weak-bot",
		Runtime:        runtime.CodexRuntime,
		RepoScope:      "acme/widgets",
		WorkingDir:     "/root/gitmoot",                        // a live checkout — must be overridden
		AutonomyPolicy: runtime.AutonomyPolicyDangerFullAccess, // a write grant — must be downgraded
	}
	got := sandboxSynthAgent(agent, scratch)
	if got.WorkingDir != scratch {
		t.Fatalf("WorkingDir = %q, want scratch %q", got.WorkingDir, scratch)
	}
	if got.RepoScope != "acme/widgets" {
		t.Fatalf("RepoScope = %q, want acme/widgets preserved", got.RepoScope)
	}
	// #725: the scratch cwd is not a hard guarantee unless the permission grant is
	// also clamped — a write-permissioned agent could otherwise escape by absolute
	// path. Every synth delivery must be forced to read-only.
	if got.AutonomyPolicy != runtime.AutonomyPolicyReadOnly {
		t.Fatalf("AutonomyPolicy = %q, want read-only downgrade", got.AutonomyPolicy)
	}
	// The original agent is not mutated (value copy).
	if agent.WorkingDir != "/root/gitmoot" {
		t.Fatalf("original agent mutated: WorkingDir = %q", agent.WorkingDir)
	}
	if agent.AutonomyPolicy != runtime.AutonomyPolicyDangerFullAccess {
		t.Fatalf("original agent mutated: AutonomyPolicy = %q", agent.AutonomyPolicy)
	}
}

// TestSynthPromptsCarryEvalOnlyPreamble proves the #725 soft complement: the
// challenger, attempt, and judge prompts all lead with the answer-only preamble
// that tells an agentic CLI not to create files or run commands.
func TestSynthPromptsCarryEvalOnlyPreamble(t *testing.T) {
	item := synthGeneratedItem{Context: "ctx", Question: "q", Rubric: "r"}
	prompts := map[string]string{
		"challenger": synthChallengerPrompt("guidance", ""),
		"attempt":    synthAttemptPrompt(item),
		"judge":      synthJudgePrompt(item, "weak", "strong"),
	}
	for name, p := range prompts {
		if !strings.HasPrefix(p, synthEvalOnlyPreamble) {
			t.Fatalf("%s prompt does not lead with the eval-only preamble:\n%s", name, p)
		}
		if !strings.Contains(p, "Do NOT create files, run commands, start servers") {
			t.Fatalf("%s prompt missing the do-not-execute instruction", name)
		}
	}
}

// TestResolveSynthWeakAgentDefaultsToChampion proves the #741 default: with --weak
// omitted, the weak attempt resolves to the target template's CURRENT CHAMPION
// version — an ephemeral agent pinned to exactly that version, delivered with the
// champion's own template instructions as its role frame, and NOT a later
// (pending, non-champion) version's content.
func TestResolveSynthWeakAgentDefaultsToChampion(t *testing.T) {
	home, store := synthTestHome(t, "weak-bot")
	_ = home
	ctx := context.Background()
	// Add a pending v2 (becomes latest, but NOT champion). The champion stays v1.
	if _, err := store.AddPendingAgentTemplateVersion(ctx, cliSkillOptTemplate("planner", "REVISED v2 guidance that must not leak.")); err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion: %v", err)
	}
	template, err := loadInstalledTemplate(ctx, store, "planner")
	if err != nil {
		t.Fatalf("loadInstalledTemplate: %v", err)
	}
	if template.VersionID != "planner@v1" {
		t.Fatalf("champion version = %q, want planner@v1 (v2 is only pending)", template.VersionID)
	}

	agent, frame, label, err := resolveSynthWeakAgent(ctx, store, synthOptions{template: "planner", repo: "acme/widgets"})
	if err != nil {
		t.Fatalf("resolveSynthWeakAgent: %v", err)
	}
	if agent.TemplateID != "planner@v1" {
		t.Fatalf("weak agent TemplateID = %q, want it pinned to the champion version planner@v1", agent.TemplateID)
	}
	if agent.Runtime != runtime.CodexRuntime {
		t.Fatalf("weak agent Runtime = %q, want codex (template runtime_compatibility[0])", agent.Runtime)
	}
	if agent.AutonomyPolicy != runtime.AutonomyPolicyReadOnly {
		t.Fatalf("weak agent AutonomyPolicy = %q, want read-only", agent.AutonomyPolicy)
	}
	if agent.RepoScope != "acme/widgets" {
		t.Fatalf("weak agent RepoScope = %q, want acme/widgets", agent.RepoScope)
	}
	// Ephemeral: no live session, and no live checkout (the #725 sandbox forces the
	// scratch dir later).
	if agent.RuntimeRef != "" || agent.WorkingDir != "" {
		t.Fatalf("weak agent must be ephemeral: RuntimeRef=%q WorkingDir=%q", agent.RuntimeRef, agent.WorkingDir)
	}
	if !strings.Contains(frame, "Plan software migrations well.") {
		t.Fatalf("champion frame missing the champion (v1) instructions: %q", frame)
	}
	if strings.Contains(frame, "REVISED v2") {
		t.Fatalf("champion frame leaked the non-champion (v2) content: %q", frame)
	}
	if label != "planner@v1 (champion)" {
		t.Fatalf("weak label = %q, want \"planner@v1 (champion)\"", label)
	}
}

// TestResolveSynthWeakAgentExplicitUnchanged is the regression guard: an explicit
// --weak still resolves the named agent from the store, returns NO champion frame,
// and records the agent name as the label — byte-identical to pre-#741.
func TestResolveSynthWeakAgentExplicitUnchanged(t *testing.T) {
	home, store := synthTestHome(t, "weak-bot")
	_ = home
	ctx := context.Background()
	agent, frame, label, err := resolveSynthWeakAgent(ctx, store, synthOptions{template: "planner", weak: "weak-bot", repo: "acme/widgets"})
	if err != nil {
		t.Fatalf("resolveSynthWeakAgent: %v", err)
	}
	if agent.Name != "weak-bot" {
		t.Fatalf("explicit weak agent Name = %q, want weak-bot (resolved from store)", agent.Name)
	}
	if frame != "" {
		t.Fatalf("explicit --weak must carry NO champion frame, got %q", frame)
	}
	if label != "weak-bot" {
		t.Fatalf("explicit weak label = %q, want the agent name weak-bot", label)
	}
	// resolveSynthAgent clears the live session/checkout, same as the strong path.
	if agent.RuntimeRef != "" || agent.WorkingDir != "" {
		t.Fatalf("explicit weak agent should carry no live session/checkout: RuntimeRef=%q WorkingDir=%q", agent.RuntimeRef, agent.WorkingDir)
	}
}

// TestResolveSynthWeakAgentIgnoresVersionRefLatest proves the champion default
// resolves the CURRENT champion off the logical id even when --template carries an
// @latest ref that points at a newer, non-champion (pending) version. Before the
// #741 review fix the version-pinned ref would have made the pending v2 get labeled
// and run as the "champion".
func TestResolveSynthWeakAgentIgnoresVersionRefLatest(t *testing.T) {
	_, store := synthTestHome(t)
	ctx := context.Background()
	// v2 is pending → it is `@latest`, but NOT the champion (which stays v1).
	if _, err := store.AddPendingAgentTemplateVersion(ctx, cliSkillOptTemplate("planner", "REVISED v2 guidance that must not leak.")); err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion: %v", err)
	}
	// Sanity: @latest really does resolve to the pending v2.
	latest, err := loadInstalledTemplate(ctx, store, "planner@latest")
	if err != nil {
		t.Fatalf("loadInstalledTemplate @latest: %v", err)
	}
	if latest.VersionID != "planner@v2" {
		t.Fatalf("@latest VersionID = %q, want planner@v2 (the pending version)", latest.VersionID)
	}

	agent, frame, label, err := resolveSynthWeakAgent(ctx, store, synthOptions{template: "planner@latest", repo: "acme/widgets"})
	if err != nil {
		t.Fatalf("resolveSynthWeakAgent: %v", err)
	}
	if agent.TemplateID != "planner@v1" {
		t.Fatalf("weak agent TemplateID = %q, want the CHAMPION planner@v1 despite @latest ref", agent.TemplateID)
	}
	if label != "planner@v1 (champion)" {
		t.Fatalf("weak label = %q, want \"planner@v1 (champion)\"", label)
	}
	if !strings.Contains(frame, "Plan software migrations well.") {
		t.Fatalf("champion frame missing the champion (v1) instructions: %q", frame)
	}
	if strings.Contains(frame, "REVISED v2") {
		t.Fatalf("champion frame leaked the non-champion (@latest/v2) content: %q", frame)
	}
}

// TestResolveSynthWeakAgentIgnoresActiveCanary proves an active canary version
// referenced explicitly via --template planner@v2 does NOT become the weak
// "champion": the incumbent champion (v1) is resolved off the logical id.
func TestResolveSynthWeakAgentIgnoresActiveCanary(t *testing.T) {
	_, store := synthTestHome(t)
	ctx := context.Background()
	v2, err := store.AddPendingAgentTemplateVersion(ctx, cliSkillOptTemplate("planner", "REVISED v2 guidance that must not leak."))
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion: %v", err)
	}
	// Promote v2 to an active canary sitting BEHIND the v1 champion.
	if _, err := store.CanaryPromoteAgentTemplateVersion(ctx, v2.ID, 0.5); err != nil {
		t.Fatalf("CanaryPromoteAgentTemplateVersion: %v", err)
	}
	canary, ok, err := store.GetActiveCanaryVersion(ctx, "planner")
	if err != nil || !ok || canary.ID != "planner@v2" {
		t.Fatalf("active canary = (%+v, ok=%v, err=%v), want planner@v2", canary, ok, err)
	}

	// --template pins the canary version explicitly.
	agent, frame, label, err := resolveSynthWeakAgent(ctx, store, synthOptions{template: "planner@v2", repo: "acme/widgets"})
	if err != nil {
		t.Fatalf("resolveSynthWeakAgent: %v", err)
	}
	if agent.TemplateID != "planner@v1" {
		t.Fatalf("weak agent TemplateID = %q, want the incumbent CHAMPION planner@v1, not the canary v2", agent.TemplateID)
	}
	if label != "planner@v1 (champion)" {
		t.Fatalf("weak label = %q, want \"planner@v1 (champion)\"", label)
	}
	if strings.Contains(frame, "REVISED v2") {
		t.Fatalf("champion frame leaked the canary (v2) content: %q", frame)
	}
}

// TestSynthChampionRuntimeSkipsShell proves synthChampionRuntime skips a
// non-START-capable `shell` first entry and selects the first agentic CLI runtime,
// so the ephemeral weak agent can actually launch a forked session. A `shell`-only
// template falls back to codex.
func TestSynthChampionRuntimeSkipsShell(t *testing.T) {
	shellFirst := synthTemplateWithRuntimes(t, "shell", "codex")
	if got := synthChampionRuntime(shellFirst); got != runtime.CodexRuntime {
		t.Fatalf("shell-first template runtime = %q, want codex (shell is not START-capable)", got)
	}
	claudeAfterShell := synthTemplateWithRuntimes(t, "shell", "claude")
	if got := synthChampionRuntime(claudeAfterShell); got != runtime.ClaudeRuntime {
		t.Fatalf("shell,claude template runtime = %q, want claude", got)
	}
	shellOnly := synthTemplateWithRuntimes(t, "shell")
	if got := synthChampionRuntime(shellOnly); got != runtime.CodexRuntime {
		t.Fatalf("shell-only template runtime = %q, want codex fallback", got)
	}
	none := synthTemplateWithRuntimes(t)
	if got := synthChampionRuntime(none); got != runtime.CodexRuntime {
		t.Fatalf("no-runtime template = %q, want codex fallback", got)
	}
}

// synthTemplateWithRuntimes builds a db.AgentTemplate whose metadata declares the
// given runtime_compatibility list (in order), for synthChampionRuntime tests.
func synthTemplateWithRuntimes(t *testing.T, runtimes ...string) db.AgentTemplate {
	t.Helper()
	if len(runtimes) == 0 {
		// MarshalMetadata requires a non-empty runtime_compatibility, so represent a
		// template that declares none as empty metadata (UnmarshalMetadata yields no
		// entries → codex fallback).
		return db.AgentTemplate{ID: "planner"}
	}
	md := agenttemplate.Metadata{
		ID:                   "planner",
		Name:                 "Planner",
		Description:          "Plans implementation work.",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask"},
		RuntimeCompatibility: runtimes,
		Tags:                 []string{"planning"},
		Inputs:               []string{"task"},
		Outputs:              []string{"plan"},
	}
	metadataJSON, err := agenttemplate.MarshalMetadata(md)
	if err != nil {
		t.Fatalf("MarshalMetadata: %v", err)
	}
	return db.AgentTemplate{ID: "planner", MetadataJSON: metadataJSON}
}

// TestSynthWeakAttemptPromptFrame proves synthWeakAttemptPrompt is byte-identical
// to synthAttemptPrompt with an empty frame (explicit --weak) and injects the
// champion instructions between the eval-only preamble and the question with a
// non-empty frame (#741 champion default).
func TestSynthWeakAttemptPromptFrame(t *testing.T) {
	item := synthGeneratedItem{Context: "ctx", Question: "q?", Rubric: "r"}
	if got, base := synthWeakAttemptPrompt(item, ""), synthAttemptPrompt(item); got != base {
		t.Fatalf("empty frame must be byte-identical to synthAttemptPrompt:\n got=%q\nwant=%q", got, base)
	}
	framed := synthWeakAttemptPrompt(item, "# Champion\n\nDo the champion thing.")
	if !strings.HasPrefix(framed, synthEvalOnlyPreamble) {
		t.Fatalf("framed weak prompt must still lead with the eval-only preamble: %q", framed)
	}
	if !strings.Contains(framed, "Do the champion thing.") {
		t.Fatalf("framed weak prompt must inject the champion instructions: %q", framed)
	}
	if !strings.Contains(framed, "Answer the following question") {
		t.Fatalf("framed weak prompt must still carry the attempt body: %q", framed)
	}
	// The champion frame precedes the attempt body.
	if strings.Index(framed, "Do the champion thing.") > strings.Index(framed, "Answer the following question") {
		t.Fatalf("champion frame must precede the attempt body: %q", framed)
	}
}

// TestRunSkillOptSynthChampionDefaultE2E is the deterministic no-LLM E2E for the
// full accept path with the DEFAULTED weak: --weak is omitted, so the weak attempt
// runs as the champion (planner@v1) with the champion's instructions injected, the
// strong agent beats it, the judge confirms, and the item is persisted
// pending_human_approval with weak_agent recorded as the champion version.
func TestRunSkillOptSynthChampionDefaultE2E(t *testing.T) {
	home, store := synthTestHome(t, "strong-bot", "judge-bot")
	ctx := context.Background()
	// A pending v2 exists but must NOT reach the weak delivery (champion is v1).
	if _, err := store.AddPendingAgentTemplateVersion(ctx, cliSkillOptTemplate("planner", "REVISED v2 guidance that must not leak.")); err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion: %v", err)
	}

	prev := skillOptSynthDeliver
	t.Cleanup(func() { skillOptSynthDeliver = prev })
	var weakName, weakTemplateID, weakPrompt string
	var weakPolicy string
	skillOptSynthDeliver = func(_ context.Context, agent runtime.Agent, prompt string) (string, error) {
		switch {
		case strings.Contains(prompt, "generating a synthetic review item"):
			return `{"context":"A legacy monolith.","question":"Outline a safe migration.","rubric":"Rewards incremental steps."}`, nil
		case strings.Contains(prompt, "Score two answers against a rubric"):
			return `{"weak_score":0.2,"strong_score":0.9,"well_formed":true,"diagnostic":""}`, nil
		case strings.Contains(prompt, "Answer the following question"):
			if agent.Name == "synth-weak-champion" {
				weakName = agent.Name
				weakTemplateID = agent.TemplateID
				weakPolicy = agent.AutonomyPolicy
				weakPrompt = prompt
				return "rewrite it all at once", nil
			}
			return "wrap and migrate incrementally behind a facade", nil
		default:
			return "", nil
		}
	}

	outDir := filepath.Join(t.TempDir(), "synth-out")
	var stdout, stderr bytes.Buffer
	// NOTE: no --weak flag — the champion default is exercised.
	code := runSkillOptSynth([]string{
		"--template", "planner", "--repo", "acme/widgets",
		"--strong", "strong-bot", "--judge", "judge-bot",
		"--max-items", "1", "--out", outDir, "--home", home,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("synth exit = %d, stderr: %s", code, stderr.String())
	}
	if weakName != "synth-weak-champion" {
		t.Fatalf("weak delivery agent name = %q, want the ephemeral synth-weak-champion", weakName)
	}
	if weakTemplateID != "planner@v1" {
		t.Fatalf("weak agent not pinned to the champion version: TemplateID = %q, want planner@v1", weakTemplateID)
	}
	if weakPolicy != runtime.AutonomyPolicyReadOnly {
		t.Fatalf("weak delivery policy = %q, want read-only (sandboxed)", weakPolicy)
	}
	if !strings.Contains(weakPrompt, "Plan software migrations well.") {
		t.Fatalf("champion (v1) instructions did not reach the weak delivery: %q", weakPrompt)
	}
	if strings.Contains(weakPrompt, "REVISED v2") {
		t.Fatalf("non-champion (v2) content leaked into the weak delivery: %q", weakPrompt)
	}

	pending, err := store.ListSynthReviewItems(ctx, db.SynthItemStatusPending)
	if err != nil {
		t.Fatalf("ListSynthReviewItems: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending items = %d, want 1", len(pending))
	}
	if pending[0].WeakAgent != "planner@v1 (champion)" {
		t.Fatalf("persisted weak_agent = %q, want \"planner@v1 (champion)\"", pending[0].WeakAgent)
	}
}

// TestRunSkillOptSynthChampionDefaultMissingTemplate proves the champion-default
// path surfaces an actionable error when the target template is not installed
// (the weak resolution can only default off a resolvable champion).
func TestRunSkillOptSynthChampionDefaultMissingTemplate(t *testing.T) {
	home, _ := synthTestHome(t, "strong-bot")
	var stdout, stderr bytes.Buffer
	code := runSkillOptSynth([]string{
		"--template", "ghost", "--repo", "acme/widgets",
		"--strong", "strong-bot", "--max-items", "1", "--home", home,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a missing template; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not installed") {
		t.Fatalf("want an actionable not-installed error, got: %s", stderr.String())
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
