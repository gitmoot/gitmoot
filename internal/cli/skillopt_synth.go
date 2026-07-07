package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// Autodata-style synthetic SkillOpt review item generation (#535). This is a
// STRICTLY opt-in prototype: the `gitmoot skillopt synth` command is the only
// entry point, there is NO daemon/auto integration, and accepted items are
// created status='pending_human_approval' — a load-bearing governance boundary
// that NOTHING in the promotion/training path reads. Cost is hard-bounded by
// --max-items and --max-rounds-per-item.

// Score thresholds for the accept decision. Scores are judge-assigned in [0,1].
// A useful item is one the weak agent STRUGGLES on and the strong agent SOLVES,
// with a meaningful gap between them — exactly the Autodata "keep only the items
// that produce a learning signal" heuristic.
const (
	// synthStrongPassScore is the minimum judged score at which the strong agent is
	// considered to have SOLVED the item. Below it, the strong attempt failed.
	synthStrongPassScore = 0.6
	// synthWeakStruggleMax is the score at/above which the weak agent is considered
	// to have already SOLVED the item (so the item is too easy to be discriminating).
	synthWeakStruggleMax = 0.6
	// synthDefaultGapThreshold is the default minimum strong−weak score gap for an
	// item to be accepted as discriminating (overridable via --gap).
	synthDefaultGapThreshold = 0.2
)

// Diagnostic labels recorded for every skipped/rejected candidate (#535 spec).
const (
	synthDiagTooEasy     = "too_easy"
	synthDiagTooHard     = "too_hard"
	synthDiagStrongFail  = "strong_failed"
	synthDiagBadRubric   = "bad_rubric"
	synthDiagContextLeak = "context_leak"
)

// skillOptSynthDeliver is the runtime-adapter delivery seam for the synth loop.
// It reuses realSkillOptABDeliver — the SAME forked/throwaway one-shot runtime
// invocation the manual A/B path uses (Start on an empty RuntimeRef, so the
// agent's live session is never touched). Tests override it to script the
// weak/strong/judge/challenger answers deterministically without any LLM.
var skillOptSynthDeliver skillOptABDeliverFunc = realSkillOptABDeliver

// synthGeneratedItem is the Challenger output: a {context, question, rubric}
// triple. Parsed from the challenger agent's JSON answer.
type synthGeneratedItem struct {
	Context  string `json:"context"`
	Question string `json:"question"`
	Rubric   string `json:"rubric"`
}

// synthJudgeVerdict is the judge output: per-attempt scores, a well-formedness
// flag, and an optional diagnostic hint. Only the "context_leak" diagnostic hint
// is honored from the judge (a leak is invisible to the score gap); all other
// diagnostics are DERIVED deterministically from the scores by classifySynthItem.
type synthJudgeVerdict struct {
	WeakScore   float64 `json:"weak_score"`
	StrongScore float64 `json:"strong_score"`
	WellFormed  bool    `json:"well_formed"`
	Diagnostic  string  `json:"diagnostic"`
}

// classifySynthItem is the PURE accept/reject decision. accepted is true iff the
// item is well-formed, the strong agent solved it, the weak agent struggled, and
// the strong−weak gap is at least gapThreshold. Otherwise it returns a stable
// diagnostic naming why the item was rejected. Kept pure (no I/O) so it is
// exhaustively unit-tested.
func classifySynthItem(v synthJudgeVerdict, gapThreshold float64) (accepted bool, diagnostic string) {
	if strings.EqualFold(strings.TrimSpace(v.Diagnostic), synthDiagContextLeak) {
		return false, synthDiagContextLeak
	}
	if !v.WellFormed {
		return false, synthDiagBadRubric
	}
	if v.StrongScore < synthStrongPassScore {
		if v.WeakScore < synthStrongPassScore {
			// Nobody could solve it — the item is simply too hard (rubric too demanding).
			return false, synthDiagTooHard
		}
		// The weak agent solved it but the strong agent did not — anomalous; the
		// strong attempt failed and the item can't demonstrate a strong>weak signal.
		return false, synthDiagStrongFail
	}
	if v.WeakScore >= synthWeakStruggleMax {
		// The weak agent already solves it — no learning signal.
		return false, synthDiagTooEasy
	}
	if v.StrongScore-v.WeakScore < gapThreshold {
		// Strong solved it but weak came close — not discriminating enough.
		return false, synthDiagTooEasy
	}
	return true, ""
}

// synthFeedbackForDiagnostic maps a rejection diagnostic to targeted feedback the
// next Challenger round uses to regenerate a better item.
func synthFeedbackForDiagnostic(diagnostic string) string {
	switch diagnostic {
	case synthDiagTooEasy:
		return "The previous item was TOO EASY: the weak agent already solved it. Generate a HARDER, more subtle item that a default/weaker agent would likely get wrong but a strong agent can still solve."
	case synthDiagTooHard:
		return "The previous item was TOO HARD: even the strong agent failed. Generate a more TRACTABLE item with a clear, achievable rubric that a strong agent can solve."
	case synthDiagStrongFail:
		return "The previous item's rubric rewarded the wrong thing (the strong agent scored below the weak agent). Rewrite the rubric so a genuinely stronger answer scores higher."
	case synthDiagBadRubric:
		return "The previous item was not well-formed. Produce a clear context, a single precise question, and a concrete, checkable rubric. Respond with STRICT JSON only."
	case synthDiagContextLeak:
		return "The previous item LEAKED its answer in the context. Remove any giveaways so the question is genuinely challenging."
	default:
		return ""
	}
}

// synthOptions holds the parsed `skillopt synth` flags.
type synthOptions struct {
	home         string
	template     string
	repo         string
	out          string
	maxItems     int
	maxRounds    int
	weak         string
	strong       string
	judge        string
	challenger   string
	gapThreshold float64
	json         bool
}

// synthItemSummary is the per-candidate result surfaced in text and JSON output.
type synthItemSummary struct {
	ID          string  `json:"id,omitempty"`
	Accepted    bool    `json:"accepted"`
	Status      string  `json:"status,omitempty"`
	Rounds      int     `json:"rounds"`
	WeakScore   float64 `json:"weak_score"`
	StrongScore float64 `json:"strong_score"`
	Gap         float64 `json:"gap"`
	Diagnostic  string  `json:"diagnostic,omitempty"`
	OutPath     string  `json:"out_path,omitempty"`
}

// synthRunSummary is the whole run result (JSON with --json).
type synthRunSummary struct {
	Template     string             `json:"template"`
	Repo         string             `json:"repo"`
	Requested    int                `json:"requested_items"`
	MaxRounds    int                `json:"max_rounds_per_item"`
	GapThreshold float64            `json:"gap_threshold"`
	Accepted     int                `json:"accepted"`
	Skipped      int                `json:"skipped"`
	Items        []synthItemSummary `json:"items"`
}

func runSkillOptSynth(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "approve":
			return runSkillOptSynthApprove(args[1:], stdout, stderr)
		case "reject":
			return runSkillOptSynthReject(args[1:], stdout, stderr)
		case "list":
			return runSkillOptSynthList(args[1:], stdout, stderr)
		case "-h", "--help":
			printSkillOptSynthUsage(stdout)
			return 0
		}
	}
	return runSkillOptSynthGenerate(args, stdout, stderr)
}

func printSkillOptSynthUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt synth --template <id> --repo owner/repo --weak <agent> --strong <agent> [--judge <agent>] [--challenger <agent>] [--max-items N] [--max-rounds-per-item M] [--gap F] [--out dir] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt synth list [--status pending_human_approval|approved|rejected] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt synth approve <item-id> [--home path]")
	fmt.Fprintln(w, "  gitmoot skillopt synth reject <item-id> [--home path]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Generates Autodata-style synthetic review items: a Challenger writes a")
	fmt.Fprintln(w, "{context, question, rubric}; a weak and a strong agent attempt it; a judge scores")
	fmt.Fprintln(w, "both. An item is ACCEPTED only when the strong agent meaningfully beats the weak")
	fmt.Fprintln(w, "agent and the judge confirms it is well-formed. Accepted items are stored")
	fmt.Fprintln(w, "pending_human_approval and NEVER enter any training/review pool until approved.")
}

func runSkillOptSynthGenerate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt synth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	template := fs.String("template", "", "agent template id the synthetic items exercise")
	repo := fs.String("repo", "", "repository in owner/repo form")
	out := fs.String("out", "", "output directory for accepted item files (default <home>/evals/synth)")
	maxItems := fs.Int("max-items", 3, "maximum number of items to accept")
	maxRounds := fs.Int("max-rounds-per-item", 3, "maximum Challenger regeneration rounds per item")
	weak := fs.String("weak", "", "weak/default agent name")
	strong := fs.String("strong", "", "strong/deeper agent name")
	judge := fs.String("judge", "", "judge agent name (defaults to the strong agent)")
	challenger := fs.String("challenger", "", "challenger agent that generates items (defaults to the strong agent)")
	gap := fs.Float64("gap", synthDefaultGapThreshold, "minimum strong−weak score gap to accept an item")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt synth does not accept positional arguments")
		printSkillOptSynthUsage(stderr)
		return 2
	}
	opts := synthOptions{
		home:         strings.TrimSpace(*home),
		template:     strings.TrimSpace(*template),
		repo:         strings.TrimSpace(*repo),
		out:          strings.TrimSpace(*out),
		maxItems:     *maxItems,
		maxRounds:    *maxRounds,
		weak:         strings.TrimSpace(*weak),
		strong:       strings.TrimSpace(*strong),
		judge:        strings.TrimSpace(*judge),
		challenger:   strings.TrimSpace(*challenger),
		gapThreshold: *gap,
		json:         *jsonOut,
	}
	if missing := missingSynthFlags(opts); len(missing) > 0 {
		fmt.Fprintf(stderr, "skillopt synth missing required flags: %s\n", strings.Join(missing, ", "))
		printSkillOptSynthUsage(stderr)
		return 2
	}
	if opts.maxItems < 1 {
		fmt.Fprintln(stderr, "skillopt synth: --max-items must be >= 1")
		return 2
	}
	if opts.maxRounds < 1 {
		fmt.Fprintln(stderr, "skillopt synth: --max-rounds-per-item must be >= 1")
		return 2
	}
	if opts.judge == "" {
		opts.judge = opts.strong
	}
	if opts.challenger == "" {
		opts.challenger = opts.strong
	}

	exit := 0
	err := withStoreAndPaths(opts.home, func(paths config.Paths, store *db.Store) error {
		if strings.TrimSpace(opts.out) == "" {
			opts.out = filepath.Join(paths.Evals, "synth")
		}
		exit = runSkillOptSynthWithStore(context.Background(), store, opts, stdout, stderr)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth: %v\n", err)
		return 1
	}
	return exit
}

func missingSynthFlags(opts synthOptions) []string {
	var missing []string
	if opts.template == "" {
		missing = append(missing, "--template")
	}
	if opts.repo == "" {
		missing = append(missing, "--repo")
	}
	if opts.weak == "" {
		missing = append(missing, "--weak")
	}
	if opts.strong == "" {
		missing = append(missing, "--strong")
	}
	return missing
}

func runSkillOptSynthWithStore(ctx context.Context, store *db.Store, opts synthOptions, stdout, stderr io.Writer) int {
	template, err := loadInstalledTemplate(ctx, store, opts.template)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth: %v\n", err)
		return 1
	}
	weakAgent, err := resolveSynthAgent(ctx, store, opts.weak, opts.repo)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth: weak %v\n", err)
		return 1
	}
	strongAgent, err := resolveSynthAgent(ctx, store, opts.strong, opts.repo)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth: strong %v\n", err)
		return 1
	}
	judgeAgent, err := resolveSynthAgent(ctx, store, opts.judge, opts.repo)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth: judge %v\n", err)
		return 1
	}
	challengerAgent, err := resolveSynthAgent(ctx, store, opts.challenger, opts.repo)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth: challenger %v\n", err)
		return 1
	}
	if err := os.MkdirAll(opts.out, 0o755); err != nil {
		fmt.Fprintf(stderr, "skillopt synth: create out dir: %v\n", err)
		return 1
	}

	summary := synthRunSummary{
		Template:     opts.template,
		Repo:         opts.repo,
		Requested:    opts.maxItems,
		MaxRounds:    opts.maxRounds,
		GapThreshold: opts.gapThreshold,
	}
	guidance := strings.TrimSpace(template.Content)

	for i := 0; i < opts.maxItems; i++ {
		result := generateSynthItem(ctx, store, opts, guidance, challengerAgent, weakAgent, strongAgent, judgeAgent, i, stderr)
		summary.Items = append(summary.Items, result)
		if result.Accepted {
			summary.Accepted++
		} else {
			summary.Skipped++
		}
	}

	if opts.json {
		if err := writeJSON(stdout, summary); err != nil {
			fmt.Fprintf(stderr, "skillopt synth: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	writeLine(stdout, "SkillOpt synth: template %s repo %s", opts.template, opts.repo)
	writeLine(stdout, "accepted %d, skipped %d (of %d requested, gap>=%.2f, <=%d rounds/item)",
		summary.Accepted, summary.Skipped, summary.Requested, summary.GapThreshold, summary.MaxRounds)
	for _, item := range summary.Items {
		if item.Accepted {
			writeLine(stdout, "  ACCEPT %s  weak=%.2f strong=%.2f gap=%.2f rounds=%d  %s",
				item.ID, item.WeakScore, item.StrongScore, item.Gap, item.Rounds, item.OutPath)
		} else {
			writeLine(stdout, "  SKIP   diagnostic=%s weak=%.2f strong=%.2f gap=%.2f rounds=%d",
				item.Diagnostic, item.WeakScore, item.StrongScore, item.Gap, item.Rounds)
		}
	}
	if summary.Accepted > 0 {
		writeLine(stdout, "%d item(s) stored pending_human_approval; run `gitmoot skillopt synth approve <id>` to release them.", summary.Accepted)
	}
	return 0
}

// generateSynthItem runs the Challenger→weak→strong→judge loop for one item slot,
// regenerating with targeted feedback until accepted or maxRounds is exhausted.
// An accepted item is persisted (file + pending_human_approval DB row). A skipped
// item logs its final diagnostic. Never returns an error: a per-call runtime
// failure is logged and treated as a skip so the bounded run continues.
//
// Sandbox (#725): every attempt/challenger/judge delivery is forced into a FRESH
// per-item temp scratch dir (never a registered repo checkout) so an agentic CLI
// that misreads the exercise as a real job can only ever write into a throwaway
// directory that is deleted when the item finishes — it can never touch a live
// checkout. This is the hard guarantee; the answer-only prompt preamble is the
// soft complement that reduces wasted agent effort.
func generateSynthItem(ctx context.Context, store *db.Store, opts synthOptions, guidance string, challengerAgent, weakAgent, strongAgent, judgeAgent runtime.Agent, index int, stderr io.Writer) synthItemSummary {
	result := synthItemSummary{}
	scratch, err := os.MkdirTemp("", "gitmoot-synth-item-")
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth: item %d: create scratch dir: %v\n", index+1, err)
		result.Diagnostic = synthDiagBadRubric
		return result
	}
	// Clean the scratch dir (and anything a misbehaving agent wrote into it) after
	// the item, regardless of accept/reject/error.
	defer os.RemoveAll(scratch)
	// deliver sandboxes every synth delivery into the per-item scratch dir before
	// handing it to the (test-overridable) delivery seam.
	deliver := func(agent runtime.Agent, prompt string) (string, error) {
		return skillOptSynthDeliver(ctx, sandboxSynthAgent(agent, scratch), prompt)
	}
	feedback := ""
	for round := 1; round <= opts.maxRounds; round++ {
		result.Rounds = round
		challengerRaw, err := deliver(challengerAgent, synthChallengerPrompt(guidance, feedback))
		if err != nil {
			fmt.Fprintf(stderr, "skillopt synth: item %d round %d: challenger: %v\n", index+1, round, err)
			result.Diagnostic = synthDiagBadRubric
			return result
		}
		item, err := parseSynthGeneratedItem(challengerRaw)
		if err != nil {
			result.Diagnostic = synthDiagBadRubric
			feedback = synthFeedbackForDiagnostic(synthDiagBadRubric)
			continue
		}
		weakAns, err := deliver(weakAgent, synthAttemptPrompt(item))
		if err != nil {
			fmt.Fprintf(stderr, "skillopt synth: item %d round %d: weak: %v\n", index+1, round, err)
			result.Diagnostic = synthDiagBadRubric
			return result
		}
		strongAns, err := deliver(strongAgent, synthAttemptPrompt(item))
		if err != nil {
			fmt.Fprintf(stderr, "skillopt synth: item %d round %d: strong: %v\n", index+1, round, err)
			result.Diagnostic = synthDiagBadRubric
			return result
		}
		judgeRaw, err := deliver(judgeAgent, synthJudgePrompt(item, weakAns, strongAns))
		if err != nil {
			fmt.Fprintf(stderr, "skillopt synth: item %d round %d: judge: %v\n", index+1, round, err)
			result.Diagnostic = synthDiagBadRubric
			return result
		}
		verdict, err := parseSynthJudgeVerdict(judgeRaw)
		if err != nil {
			result.Diagnostic = synthDiagBadRubric
			feedback = synthFeedbackForDiagnostic(synthDiagBadRubric)
			continue
		}
		result.WeakScore = verdict.WeakScore
		result.StrongScore = verdict.StrongScore
		result.Gap = verdict.StrongScore - verdict.WeakScore
		accepted, diagnostic := classifySynthItem(verdict, opts.gapThreshold)
		if !accepted {
			result.Diagnostic = diagnostic
			feedback = synthFeedbackForDiagnostic(diagnostic)
			continue
		}
		// Accepted: persist the item (file + pending_human_approval DB row).
		id := synthItemID(opts.template, index)
		record := db.SynthReviewItem{
			ID:           id,
			TemplateID:   opts.template,
			Repo:         opts.repo,
			Status:       db.SynthItemStatusPending,
			Context:      item.Context,
			Question:     item.Question,
			Rubric:       item.Rubric,
			WeakAgent:    opts.weak,
			StrongAgent:  opts.strong,
			JudgeAgent:   opts.judge,
			WeakAnswer:   weakAns,
			StrongAnswer: strongAns,
			WeakScore:    verdict.WeakScore,
			StrongScore:  verdict.StrongScore,
			Gap:          verdict.StrongScore - verdict.WeakScore,
			Rounds:       round,
		}
		outPath := filepath.Join(opts.out, id+".json")
		if err := writeSynthItemFile(outPath, record); err != nil {
			fmt.Fprintf(stderr, "skillopt synth: write item file: %v\n", err)
			result.Diagnostic = synthDiagBadRubric
			return result
		}
		record.OutPath = outPath
		if err := store.CreateSynthReviewItem(ctx, record); err != nil {
			fmt.Fprintf(stderr, "skillopt synth: persist item: %v\n", err)
			// Roll back the orphan file so a persistence failure leaves no dangling item.
			_ = os.Remove(outPath)
			result.Diagnostic = synthDiagBadRubric
			return result
		}
		result.ID = id
		result.Accepted = true
		result.Status = db.SynthItemStatusPending
		result.OutPath = outPath
		result.Diagnostic = ""
		return result
	}
	return result
}

func resolveSynthAgent(ctx context.Context, store *db.Store, name, repo string) (runtime.Agent, error) {
	dbAgent, err := store.GetAgent(ctx, name)
	if err != nil {
		return runtime.Agent{}, fmt.Errorf("agent %q: %w", name, err)
	}
	agent := runtimeAgent(dbAgent)
	// Clear RuntimeRef so the delivery seam opens a FORKED throwaway session
	// (adapter.Start on empty ref) and never touches the agent's live session.
	agent.RuntimeRef = ""
	if strings.TrimSpace(agent.RepoScope) == "" {
		agent.RepoScope = repo
	}
	// #725: DO NOT resolve the repo scope to its registered checkout directory.
	// A synth attempt/challenger/judge is delivered to an agentic CLI that runs
	// with full tool permissions; if its adapter Dir is a live checkout it will
	// happily write files, start servers, and probe ports IN that checkout (the
	// incident that motivated this fix). Every synth delivery is instead forced
	// into a fresh per-item temp scratch dir by sandboxSynthAgent (see
	// generateSynthItem), so WorkingDir is left empty here on purpose — nothing in
	// the synth path may carry a real checkout path.
	agent.WorkingDir = ""
	return agent, nil
}

// sandboxSynthAgent returns a copy of agent whose adapter working dir is forced
// to scratch — a fresh per-item temp directory (#725). Because
// realSkillOptABDeliver derives the adapter Dir from WorkingDir (falling back to
// RepoScope only when WorkingDir is empty), setting WorkingDir to the scratch dir
// guarantees the delivery chdirs into the throwaway scratch and NEVER into a
// registered repo checkout, no matter what the agent otherwise carries. RepoScope
// is left intact so the adapter still resolves the correct runtime family.
func sandboxSynthAgent(agent runtime.Agent, scratch string) runtime.Agent {
	agent.WorkingDir = scratch
	return agent
}

func synthItemID(template string, index int) string {
	base := skillOptSafeAgentName("synth-" + template)
	return fmt.Sprintf("%s-%d-%d", base, time.Now().UTC().UnixNano(), index+1)
}

func writeSynthItemFile(path string, item db.SynthReviewItem) error {
	payload := map[string]any{
		"id":            item.ID,
		"template_id":   item.TemplateID,
		"repo":          item.Repo,
		"status":        item.Status,
		"context":       item.Context,
		"question":      item.Question,
		"rubric":        item.Rubric,
		"weak_agent":    item.WeakAgent,
		"strong_agent":  item.StrongAgent,
		"judge_agent":   item.JudgeAgent,
		"weak_answer":   item.WeakAnswer,
		"strong_answer": item.StrongAnswer,
		"weak_score":    item.WeakScore,
		"strong_score":  item.StrongScore,
		"gap":           item.Gap,
		"rounds":        item.Rounds,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// --- prompt builders -------------------------------------------------------

// synthEvalOnlyPreamble frames every synth delivery as an answer-only written
// exercise (#725). The attempts run against agentic CLIs (not chat models); this
// preamble is the soft complement to the hard scratch-dir sandbox — it tells the
// agent NOT to treat the item as a real job to implement, cutting wasted effort.
const synthEvalOnlyPreamble = "This is a written evaluation exercise. Do NOT create files, run commands, start servers, or modify any repository. Respond with text only.\n\n"

func synthChallengerPrompt(guidance, feedback string) string {
	var b strings.Builder
	b.WriteString(synthEvalOnlyPreamble)
	b.WriteString("You are generating a synthetic review item to evaluate an agent skill.\n")
	if strings.TrimSpace(guidance) != "" {
		b.WriteString("The skill being exercised:\n")
		b.WriteString(guidance)
		b.WriteString("\n\n")
	}
	b.WriteString("Produce a single item that a weaker/default agent would likely get wrong ")
	b.WriteString("but a strong agent can solve. Do NOT leak the answer in the context.\n")
	if strings.TrimSpace(feedback) != "" {
		b.WriteString("\nFeedback from the previous attempt: ")
		b.WriteString(feedback)
		b.WriteString("\n")
	}
	b.WriteString("\nRespond with STRICT JSON only: ")
	b.WriteString(`{"context": "...", "question": "...", "rubric": "..."}`)
	return b.String()
}

func synthAttemptPrompt(item synthGeneratedItem) string {
	var b strings.Builder
	b.WriteString(synthEvalOnlyPreamble)
	b.WriteString("Answer the following question using the given context.\n\n")
	b.WriteString("Context:\n")
	b.WriteString(item.Context)
	b.WriteString("\n\nQuestion:\n")
	b.WriteString(item.Question)
	return b.String()
}

func synthJudgePrompt(item synthGeneratedItem, weakAnswer, strongAnswer string) string {
	var b strings.Builder
	b.WriteString(synthEvalOnlyPreamble)
	b.WriteString("Score two answers against a rubric and judge the item's quality.\n\n")
	b.WriteString("Context:\n")
	b.WriteString(item.Context)
	b.WriteString("\n\nQuestion:\n")
	b.WriteString(item.Question)
	b.WriteString("\n\nRubric:\n")
	b.WriteString(item.Rubric)
	b.WriteString("\n\nAnswer A (weak):\n")
	b.WriteString(weakAnswer)
	b.WriteString("\n\nAnswer B (strong):\n")
	b.WriteString(strongAnswer)
	b.WriteString("\n\nScore each answer 0.0-1.0 against the rubric. Set well_formed to false if the ")
	b.WriteString("item is ill-posed, and diagnostic to \"context_leak\" if the context gives the answer away.\n")
	b.WriteString("Respond with STRICT JSON only: ")
	b.WriteString(`{"weak_score": 0.0, "strong_score": 0.0, "well_formed": true, "diagnostic": ""}`)
	return b.String()
}

// --- parsing ---------------------------------------------------------------

func parseSynthGeneratedItem(raw string) (synthGeneratedItem, error) {
	obj, err := extractSynthJSONObject(raw)
	if err != nil {
		return synthGeneratedItem{}, err
	}
	var item synthGeneratedItem
	if err := json.Unmarshal([]byte(obj), &item); err != nil {
		return synthGeneratedItem{}, err
	}
	if strings.TrimSpace(item.Context) == "" || strings.TrimSpace(item.Question) == "" || strings.TrimSpace(item.Rubric) == "" {
		return synthGeneratedItem{}, fmt.Errorf("synth item missing context, question, or rubric")
	}
	return item, nil
}

func parseSynthJudgeVerdict(raw string) (synthJudgeVerdict, error) {
	obj, err := extractSynthJSONObject(raw)
	if err != nil {
		return synthJudgeVerdict{}, err
	}
	var verdict synthJudgeVerdict
	if err := json.Unmarshal([]byte(obj), &verdict); err != nil {
		return synthJudgeVerdict{}, err
	}
	return verdict, nil
}

// extractSynthJSONObject pulls the first balanced {...} object out of an LLM
// answer, tolerating surrounding prose and ```json fences.
func extractSynthJSONObject(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty response")
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", fmt.Errorf("no JSON object found")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced JSON object")
}

// --- human gate subcommands ------------------------------------------------

func runSkillOptSynthApprove(args []string, stdout, stderr io.Writer) int {
	return runSkillOptSynthSetStatus(args, stdout, stderr, "approve", db.SynthItemStatusApproved)
}

func runSkillOptSynthReject(args []string, stdout, stderr io.Writer) int {
	return runSkillOptSynthSetStatus(args, stdout, stderr, "reject", db.SynthItemStatusRejected)
}

func runSkillOptSynthSetStatus(args []string, stdout, stderr io.Writer, verb, status string) int {
	fs := flag.NewFlagSet("skillopt synth "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintf(stderr, "usage: gitmoot skillopt synth %s <item-id> [--home path]\n", verb)
		return 2
	}
	id := strings.TrimSpace(fs.Arg(0))
	// Allow flags AFTER the positional id (e.g. `approve <id> --home X`): Go's flag
	// parser stops at the first non-flag, so re-parse whatever trailed the id.
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "usage: gitmoot skillopt synth %s <item-id> [--home path]\n", verb)
		return 2
	}
	exit := 0
	err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		item, ok, err := store.GetSynthReviewItem(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(stderr, "skillopt synth %s: item %q not found\n", verb, id)
			exit = 1
			return nil
		}
		if item.Status != db.SynthItemStatusPending {
			fmt.Fprintf(stderr, "skillopt synth %s: item %q already %s\n", verb, id, item.Status)
			exit = 1
			return nil
		}
		if err := store.SetSynthReviewItemStatus(ctx, id, status); err != nil {
			return err
		}
		writeLine(stdout, "%s %s", status, id)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth %s: %v\n", verb, err)
		return 1
	}
	return exit
}

func runSkillOptSynthList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt synth list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	status := fs.String("status", "", "filter by status: pending_human_approval, approved, or rejected")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	exit := 0
	err := withStore(*home, func(store *db.Store) error {
		items, err := store.ListSynthReviewItems(context.Background(), strings.TrimSpace(*status))
		if err != nil {
			return err
		}
		if *jsonOut {
			summaries := make([]synthItemSummary, 0, len(items))
			for _, item := range items {
				summaries = append(summaries, synthItemSummary{
					ID:          item.ID,
					Accepted:    true,
					Status:      item.Status,
					Rounds:      item.Rounds,
					WeakScore:   item.WeakScore,
					StrongScore: item.StrongScore,
					Gap:         item.Gap,
					OutPath:     item.OutPath,
				})
			}
			return writeJSON(stdout, summaries)
		}
		if len(items) == 0 {
			writeLine(stdout, "no synth items")
			return nil
		}
		for _, item := range items {
			writeLine(stdout, "%s  %s  template=%s weak=%.2f strong=%.2f gap=%.2f",
				item.ID, item.Status, item.TemplateID, item.WeakScore, item.StrongScore, item.Gap)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "skillopt synth list: %v\n", err)
		return 1
	}
	return exit
}
