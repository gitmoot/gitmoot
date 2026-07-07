package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	case "lessons":
		return runSkillOptBinaryLessons(args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "  gitmoot skillopt binary lessons --template <id> [--set <file>] [--run <run-id> ...] [--no-passes] [--apply] [--home path] [--json]")
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

// binaryLessonsRunID is the deterministic id of the synthetic eval run that
// --apply writes the derived lessons into. It is namespaced per template so
// re-applying replaces the same run's events wholesale (idempotent full replace,
// so a shrinking lesson set removes stale events).
func binaryLessonsRunID(templateID string) string {
	return "binary-lessons:" + strings.TrimSpace(templateID)
}

// binaryLessonsReviewer / binaryLessonsSource stamp the written RankedFeedbackEvents
// so they are attributable and filterable as the #527 disagreement channel.
const (
	binaryLessonsReviewer = "binary-disagreement"
	binaryLessonsSource   = "binary-disagreement"
)

// repeatableStringFlag collects a flag passed multiple times.
type repeatableStringFlag []string

func (r *repeatableStringFlag) String() string { return strings.Join(*r, ",") }
func (r *repeatableStringFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v != "" {
		*r = append(*r, v)
	}
	return nil
}

// runSkillOptBinaryLessons derives optimizer-consumable lessons from the binary
// verdicts already recorded for a template (#527): questions whose yes/no
// verdict flips across runs (candidate-vs-champion or repeated runs of one
// version) are targeted improvement signals, stable NOs are concrete lessons,
// and stable YESes are traits to preserve. It PREVIEWS by default and writes
// NOTHING; only --apply projects the lessons onto RankedFeedbackEvent rows via
// the existing store API so the existing optimizer + rubric-induce consume them
// with zero contract change. There is no daemon/automatic path — CLI-explicit
// only, mirroring the synth approval gate.
func runSkillOptBinaryLessons(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt binary lessons", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	template := fs.String("template", "", "template id whose binary verdicts to derive lessons from")
	setPath := fs.String("set", "", "binary question set file (YAML or JSON) used to recover question wording (optional)")
	noPasses := fs.Bool("no-passes", false, "omit stable-pass (yes-in-every-run) traits; keep only flips and stable failures")
	apply := fs.Bool("apply", false, "write the derived lessons as ranked feedback events (default previews only, writes nothing)")
	asJSON := fs.Bool("json", false, "emit JSON")
	var runFilter repeatableStringFlag
	fs.Var(&runFilter, "run", "restrict to these run ids (repeatable); default uses every run of the template")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt binary lessons does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*template) == "" {
		fmt.Fprintln(stderr, "skillopt binary lessons requires --template")
		return 2
	}

	var questionText map[string]string
	if strings.TrimSpace(*setPath) != "" {
		set, err := skillopt.LoadBinaryQuestionSet(*setPath)
		if err != nil {
			fmt.Fprintf(stderr, "skillopt binary lessons: load question set: %v\n", err)
			return 1
		}
		questionText = skillopt.QuestionTextIndex(set)
	}

	runAllow := map[string]struct{}{}
	for _, id := range runFilter {
		runAllow[id] = struct{}{}
	}

	ctx := context.Background()
	var lessons []skillopt.BinaryLesson
	var appliedRunID string
	appliedEvents := 0

	err := withStore(*home, func(store *db.Store) error {
		rows, err := store.ListBinaryVerdictsForTemplate(ctx, strings.TrimSpace(*template))
		if err != nil {
			return err
		}
		flat := make([]struct {
			QuestionID  string
			Dimension   string
			RunID       string
			VersionID   string
			Verdict     string
			Explanation string
		}, 0, len(rows))
		for _, r := range rows {
			if len(runAllow) > 0 {
				if _, ok := runAllow[r.RunID]; !ok {
					continue
				}
			}
			flat = append(flat, struct {
				QuestionID  string
				Dimension   string
				RunID       string
				VersionID   string
				Verdict     string
				Explanation string
			}{
				QuestionID:  r.QuestionID,
				Dimension:   r.Dimension,
				RunID:       r.RunID,
				VersionID:   r.TemplateVersionID,
				Verdict:     r.Verdict,
				Explanation: r.Explanation,
			})
		}
		groups := skillopt.GroupBinaryObservations(flat)
		opts := skillopt.BinaryLessonsOptions{}
		if *noPasses {
			opts = opts.WithIncludeStableYes(false)
		}
		lessons = skillopt.DeriveBinaryLessons(groups, questionText, opts)

		if !*apply {
			return nil
		}
		// --apply is a FULL REPLACE, not an accumulating upsert: writeBinaryLessonEvents
		// clears the synthetic run's prior events before rewriting the current set, so a
		// shrinking lesson set (e.g. re-running with --no-passes or a narrower --run
		// filter) removes stale events. We therefore run it even when the current set is
		// empty — an early return there would strand previously-written events.
		appliedRunID = binaryLessonsRunID(*template)
		if err := writeBinaryLessonEvents(ctx, store, strings.TrimSpace(*template), appliedRunID, lessons); err != nil {
			return err
		}
		appliedEvents = len(lessons)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "skillopt binary lessons: %v\n", err)
		return 1
	}

	if *asJSON {
		payload := map[string]any{
			"template": strings.TrimSpace(*template),
			"applied":  *apply,
			"lessons":  lessons,
		}
		if *apply {
			payload["run_id"] = appliedRunID
			payload["events_written"] = appliedEvents
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "skillopt binary lessons: %v\n", err)
			return 1
		}
		encoded = append(encoded, '\n')
		stdout.Write(encoded)
		return 0
	}

	writeBinaryLessonsHuman(stdout, strings.TrimSpace(*template), lessons, *apply, appliedRunID, appliedEvents)
	return 0
}

// writeBinaryLessonEvents rewrites a synthetic per-template lessons run as a
// FULL REPLACE: it (re)registers the run, clears any events from a prior apply,
// and then for each lesson writes a two-option review item plus a
// RankedFeedbackEvent carrying the lesson text. Negative lessons (flips, stable
// failures) go into required_improvements (a flat list the rubric inducer grounds
// as negative aspects); stable-pass traits go into useful_traits keyed by a known
// option label. Clearing first (rather than upserting in place) means a shrinking
// lesson set removes stale events, so re-applying is genuinely idempotent for the
// derived set — including the empty-set case, which leaves the run with no events.
//
// The `candidate`/`champion` option labels are fixed placeholders, not real
// versions: the lesson lives entirely in useful_traits/required_improvements.
// So each event is written as a NEUTRAL single tie group with no winner, which
// derives ZERO pairwise preference (a fabricated `candidate > champion` win would
// be meaningless — neither placeholder actually won). Each option is backed by a
// real (if empty) eval_artifacts row so `skillopt export --run
// binary-lessons:<template>` resolves every artifact ref instead of erroring on a
// phantom id (the store requires a non-empty option artifact id).
func writeBinaryLessonEvents(ctx context.Context, store *db.Store, templateID, runID string, lessons []skillopt.BinaryLesson) error {
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:           runID,
		TemplateID:   templateID,
		TargetRepo:   "binary-disagreement",
		State:        "review",
		Mode:         db.EvalRunModeValidate,
		OptionsCount: 2,
	}); err != nil {
		return err
	}
	// Full replace: drop the previous apply's items/options/events before rewriting.
	if err := store.ClearEvalRunFeedback(ctx, runID); err != nil {
		return err
	}
	for _, lesson := range lessons {
		itemID := "q:" + lesson.QuestionID
		if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{RunID: runID, ItemID: itemID, Title: lesson.QuestionID}); err != nil {
			return err
		}
		for _, label := range []string{"candidate", "champion"} {
			artifactID := "binary-lessons/" + itemID + "/" + label
			// Persist a deterministic, empty synthetic artifact row so export's
			// GetEvalArtifact resolves the option's artifact id. The row is metadata
			// only (no blob is fetched during export); the hash is a stable digest of
			// the id so re-applying is idempotent.
			digest := sha256.Sum256([]byte(artifactID))
			if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{
				ID:        artifactID,
				Hash:      hex.EncodeToString(digest[:]),
				MediaType: "text/plain",
				SizeBytes: 0,
				Driver:    "text",
			}); err != nil {
				return err
			}
			if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{
				RunID:      runID,
				ItemID:     itemID,
				Label:      label,
				Role:       "option",
				ArtifactID: artifactID,
			}); err != nil {
				return err
			}
		}
		event := db.RankedFeedbackEvent{
			ID:            runID + ":" + itemID,
			RunID:         runID,
			ItemID:        itemID,
			RankingJSON:   `["candidate","champion"]`,
			TieGroupsJSON: `[["candidate","champion"]]`,
			Reviewer:      binaryLessonsReviewer,
			Source:        binaryLessonsSource,
			Reasoning:     lesson.Text,
		}
		if lesson.Sign == skillopt.AspectSignPositive {
			encoded, err := json.Marshal(map[string][]string{"candidate": {lesson.Trait}})
			if err != nil {
				return err
			}
			event.UsefulTraitsJSON = string(encoded)
		} else {
			encoded, err := json.Marshal([]string{lesson.Trait})
			if err != nil {
				return err
			}
			event.RequiredImprovementsJSON = string(encoded)
		}
		if err := store.UpsertRankedFeedbackEvent(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func writeBinaryLessonsHuman(w io.Writer, templateID string, lessons []skillopt.BinaryLesson, applied bool, runID string, events int) {
	writeLine(w, "binary-disagreement lessons for template %s", templateID)
	if len(lessons) == 0 {
		writeLine(w, "  (no lessons — no binary verdicts recorded for this template, or nothing to flag)")
		return
	}
	flips, stableNo, stableYes := 0, 0, 0
	for _, l := range lessons {
		switch l.Kind {
		case skillopt.BinaryLessonFlip:
			flips++
		case skillopt.BinaryLessonStableNo:
			stableNo++
		case skillopt.BinaryLessonStableYes:
			stableYes++
		}
	}
	writeLine(w, "  %d lesson(s): %d flip, %d stable-fail, %d stable-pass", len(lessons), flips, stableNo, stableYes)
	for _, l := range lessons {
		writeLine(w, "  [%s] %s (%s, %d yes / %d no across %d run(s)): %s", l.Kind, l.QuestionID, l.Dimension, l.YesCount, l.NoCount, l.Runs, l.Text)
	}
	if applied {
		writeLine(w, "applied: wrote %d ranked feedback event(s) to run %s", events, runID)
	} else {
		writeLine(w, "preview only — nothing written. Re-run with --apply to persist these as ranked feedback events.")
	}
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
