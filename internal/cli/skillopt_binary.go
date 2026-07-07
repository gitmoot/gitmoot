package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// runSkillOptBinary dispatches the BINEVAL binary-evaluation subcommands (#525).
// This whole surface is additive and opt-in: nothing here runs — and no
// skillopt_binary_verdicts row is ever written — unless a `skillopt binary`
// command is invoked, so existing review/optimize flows are byte-identical.
func runSkillOptBinary(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptBinaryUsage(stdout)
		return 0
	}
	switch args[0] {
	case "run":
		return runSkillOptBinaryRun(args[1:], stdout, stderr)
	case "show":
		return runSkillOptBinaryShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt binary command %q\n\n", args[0])
		printSkillOptBinaryUsage(stderr)
		return 2
	}
}

func printSkillOptBinaryUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt binary run --set <file> --run <run-id> --source <file> [--deterministic] [--reviewer runtime] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt binary show --run <run-id> [--home path] [--json]")
}

// skillOptBinaryDeliver is the delivery seam for the opt-in LLM-backed binary
// runner. It defaults to the SAME real cross-family adapter path the A/B judge
// uses (realSkillOptABDeliver) and is a var precisely so tests can inject a fake
// adapter without any live runtime.
var skillOptBinaryDeliver skillOptABDeliverFunc = realSkillOptABDeliver

// binaryLLMEvaluator is the opt-in LLM-backed BinaryEvaluator. It answers one
// question at a time by delivering a small yes/no prompt through the EXISTING
// cross-family judge plumbing (skillOptABJudgeAgent + the shared deliver seam),
// so the judgment runs on a DIFFERENT family than any template under test and
// never becomes a self-preference. Each Answer is a fresh, serialized one-shot
// ask (read-only) — the same discipline the A/B judge documents.
type binaryLLMEvaluator struct {
	agent   runtime.Agent
	deliver skillOptABDeliverFunc
}

// Answer implements skillopt.BinaryEvaluator via the LLM plumbing.
func (e binaryLLMEvaluator) Answer(ctx context.Context, dimension string, question skillopt.BinaryQuestion, source string) (skillopt.BinaryAnswer, error) {
	prompt := buildBinaryQuestionPrompt(dimension, question, source)
	raw, err := e.deliver(ctx, e.agent, prompt)
	if err != nil {
		return skillopt.BinaryAnswer{}, err
	}
	verdict, explanation, ok := parseBinaryVerdict(raw)
	if !ok {
		// Fail-safe: an unparseable answer is a "no" so a garbled judge never
		// fabricates a pass. The raw text is surfaced as the explanation.
		return skillopt.BinaryAnswer{Verdict: skillopt.BinaryVerdictNo, Explanation: strings.TrimSpace(raw)}, nil
	}
	return skillopt.BinaryAnswer{Verdict: verdict, Explanation: explanation}, nil
}

// buildBinaryQuestionPrompt assembles the single-question prompt. It asks for a
// strict {"verdict":"yes|no","explanation":"..."} JSON object over ONE atomic
// check applied to the source output.
func buildBinaryQuestionPrompt(dimension string, question skillopt.BinaryQuestion, source string) string {
	var b strings.Builder
	b.WriteString("You are an impartial evaluator answering ONE yes/no question about the output below. ")
	b.WriteString("Answer ONLY this question; do not judge anything else. This is a SOFT, advisory signal.\n\n")
	if strings.TrimSpace(dimension) != "" {
		b.WriteString("## Dimension\n" + strings.TrimSpace(dimension) + "\n\n")
	}
	b.WriteString("## Question\n" + strings.TrimSpace(question.Text) + "\n\n")
	if strings.TrimSpace(question.ViolationExample) != "" {
		b.WriteString("## Example of a violation (would be \"no\")\n" + strings.TrimSpace(question.ViolationExample) + "\n\n")
	}
	b.WriteString("## Output under evaluation\n" + strings.TrimSpace(source) + "\n\n")
	b.WriteString(`Return ONLY a JSON object of the form {"verdict":"yes","explanation":"..."} or `)
	b.WriteString(`{"verdict":"no","explanation":"..."} (no other prose). Do NOT modify any files (read-only).`)
	b.WriteString("\n")
	return b.String()
}

// parseBinaryVerdict extracts a yes/no verdict + explanation from the runner's
// raw output using the shared brace-balanced jsonCandidates scan, tolerating
// surrounding prose. ok=false on empty/unparseable/ambiguous output (the
// fail-safe drop), so a garbled answer never fabricates a verdict.
func parseBinaryVerdict(raw string) (verdict, explanation string, ok bool) {
	for _, candidate := range jsonCandidates(raw) {
		var envelope struct {
			Verdict       string `json:"verdict"`
			Explanation   string `json:"explanation"`
			GitmootResult struct {
				Verdict     string `json:"verdict"`
				Explanation string `json:"explanation"`
			} `json:"gitmoot_result"`
		}
		if err := json.Unmarshal([]byte(candidate), &envelope); err != nil {
			continue
		}
		v := firstNonEmpty(
			strings.ToLower(strings.TrimSpace(envelope.Verdict)),
			strings.ToLower(strings.TrimSpace(envelope.GitmootResult.Verdict)),
		)
		explanation = firstNonEmpty(
			strings.TrimSpace(envelope.Explanation),
			strings.TrimSpace(envelope.GitmootResult.Explanation),
		)
		if v == skillopt.BinaryVerdictYes || v == skillopt.BinaryVerdictNo {
			return v, explanation, true
		}
	}
	return "", "", false
}

func runSkillOptBinaryRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt binary run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	setPath := fs.String("set", "", "binary question set file (YAML or JSON)")
	runID := fs.String("run", "", "eval run id to attach the verdicts to")
	sourcePath := fs.String("source", "", "file containing the output text to evaluate")
	deterministic := fs.Bool("deterministic", false, "use the deterministic rule-based runner (no LLM) — required for reproducible/test runs")
	reviewer := fs.String("reviewer", "", "runtime family for the opt-in LLM-backed runner (ignored with --deterministic)")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt binary run does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*setPath) == "" {
		fmt.Fprintln(stderr, "skillopt binary run requires --set")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt binary run requires --run")
		return 2
	}
	if strings.TrimSpace(*sourcePath) == "" {
		fmt.Fprintln(stderr, "skillopt binary run requires --source")
		return 2
	}
	set, err := skillopt.LoadBinaryQuestionSet(*setPath)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt binary run: load question set: %v\n", err)
		return 1
	}
	sourceBytes, err := os.ReadFile(*sourcePath)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt binary run: read source: %v\n", err)
		return 1
	}
	source := string(sourceBytes)

	var evaluator skillopt.BinaryEvaluator
	if *deterministic {
		evaluator = skillopt.RuleBasedBinaryEvaluator{}
	} else {
		if strings.TrimSpace(*reviewer) == "" {
			fmt.Fprintln(stderr, "skillopt binary run: the LLM runner requires --reviewer <runtime> (or pass --deterministic)")
			return 2
		}
		evaluator = binaryLLMEvaluator{
			agent:   skillOptABJudgeAgent(workflow.CrossFamilyReviewer{Runtime: strings.TrimSpace(*reviewer)}),
			deliver: skillOptBinaryDeliver,
		}
	}

	result, err := skillopt.RunBinaryEvaluation(context.Background(), set, source, evaluator)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt binary run: %v\n", err)
		return 1
	}

	if err := withStore(*home, func(store *db.Store) error {
		for _, v := range result.Verdicts {
			if err := store.UpsertBinaryVerdict(context.Background(), db.BinaryVerdict{
				RunID:           strings.TrimSpace(*runID),
				QuestionID:      v.QuestionID,
				Dimension:       v.Dimension,
				Verdict:         v.Verdict,
				Explanation:     v.Explanation,
				QuestionWeight:  v.QuestionWeight,
				DimensionWeight: v.DimensionWeight,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt binary run: persist verdicts: %v\n", err)
		return 1
	}

	if *asJSON {
		payload := map[string]any{
			"run_id":           strings.TrimSpace(*runID),
			"template_or_kind": set.TemplateOrTaskKind,
			"overall":          result.Overall,
			"dimension_scores": result.DimensionScores,
			"verdicts":         result.Verdicts,
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "skillopt binary run: %v\n", err)
			return 1
		}
		encoded = append(encoded, '\n')
		stdout.Write(encoded)
		return 0
	}

	writeBinaryHuman(stdout, strings.TrimSpace(*runID), result)
	return 0
}

func runSkillOptBinaryShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt binary show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id to show verdicts for")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt binary show does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt binary show requires --run")
		return 2
	}
	var stored []db.BinaryVerdict
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		stored, err = store.ListBinaryVerdicts(context.Background(), strings.TrimSpace(*runID))
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt binary show: %v\n", err)
		return 1
	}

	verdicts := make([]skillopt.BinaryVerdict, 0, len(stored))
	for _, v := range stored {
		verdicts = append(verdicts, skillopt.BinaryVerdict{
			QuestionID:      v.QuestionID,
			Dimension:       v.Dimension,
			Verdict:         v.Verdict,
			Explanation:     v.Explanation,
			QuestionWeight:  v.QuestionWeight,
			DimensionWeight: v.DimensionWeight,
			CreatedAt:       v.CreatedAt,
		})
	}
	// Aggregate with the SAME weighted logic (and the persisted weights) that
	// RunBinaryEvaluation used, so `show` reproduces the score the `run` emitted
	// for this exact run rather than an unweighted approximation.
	dimensionScores, overall := skillopt.AggregateBinaryVerdicts(verdicts)

	if *asJSON {
		payload := map[string]any{
			"run_id":           strings.TrimSpace(*runID),
			"overall":          overall,
			"dimension_scores": dimensionScores,
			"verdicts":         verdicts,
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "skillopt binary show: %v\n", err)
			return 1
		}
		encoded = append(encoded, '\n')
		stdout.Write(encoded)
		return 0
	}

	writeBinaryHuman(stdout, strings.TrimSpace(*runID), skillopt.BinaryEvaluationResult{
		Verdicts:        verdicts,
		DimensionScores: dimensionScores,
		Overall:         overall,
	})
	return 0
}

func writeBinaryHuman(w io.Writer, runID string, result skillopt.BinaryEvaluationResult) {
	writeLine(w, "binary evaluation for run %s", runID)
	writeLine(w, "overall: %.3f", result.Overall)
	skillopt.SortBinaryVerdicts(result.Verdicts)
	dims := make([]string, 0, len(result.DimensionScores))
	for d := range result.DimensionScores {
		dims = append(dims, d)
	}
	sort.Strings(dims)
	for _, d := range dims {
		writeLine(w, "dimension %s: %.3f", d, result.DimensionScores[d])
	}
	for _, v := range result.Verdicts {
		if strings.TrimSpace(v.Explanation) != "" {
			writeLine(w, "  [%s] %s (%s): %s", v.Dimension, v.QuestionID, v.Verdict, v.Explanation)
		} else {
			writeLine(w, "  [%s] %s (%s)", v.Dimension, v.QuestionID, v.Verdict)
		}
	}
}
