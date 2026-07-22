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
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

// runSkillOptJudgeReport renders an offline calibration report comparing the
// LLM judge's accept/reject signal against the human promote/reject decision
// captured at decision time (#345). It is read-only: it lists the stored
// judge-outcome rows and computes a confusion matrix, overall agreement,
// Cohen's kappa, soft-score calibration bands, and per-dimension disagreement.
func runSkillOptJudgeReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt judge-report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "template id to filter")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt judge-report does not accept positional arguments")
		return 2
	}
	var outcomes []db.SkillOptJudgeOutcome
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		outcomes, err = store.ListSkillOptJudgeOutcomes(context.Background(), *templateID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt judge-report: %v\n", err)
		return 1
	}
	renderSkillOptJudgeReport(stdout, outcomes)
	return 0
}

func renderSkillOptJudgeReport(stdout io.Writer, outcomes []db.SkillOptJudgeOutcome) {
	if len(outcomes) == 0 {
		writeLine(stdout, "no judge outcomes captured")
		return
	}
	// Confusion matrix: count each of the four direction buckets.
	counts := map[string]int{
		db.SkillOptJudgeDirectionAgreeAccept:            0,
		db.SkillOptJudgeDirectionAgreeReject:            0,
		db.SkillOptJudgeDirectionJudgeAcceptHumanReject: 0,
		db.SkillOptJudgeDirectionJudgeRejectHumanAccept: 0,
	}
	for _, outcome := range outcomes {
		counts[outcome.Direction]++
	}
	total := len(outcomes)
	agreeAccept := counts[db.SkillOptJudgeDirectionAgreeAccept]
	agreeReject := counts[db.SkillOptJudgeDirectionAgreeReject]
	judgeAcceptHumanReject := counts[db.SkillOptJudgeDirectionJudgeAcceptHumanReject]
	judgeRejectHumanAccept := counts[db.SkillOptJudgeDirectionJudgeRejectHumanAccept]

	fmt.Fprintf(stdout, "judge outcomes: %d\n", total)
	writeLine(stdout, "")
	writeLine(stdout, "confusion matrix (judge vs human)")
	fmt.Fprintf(stdout, "  %-22s %-14s %-14s\n", "", "human promote", "human reject")
	fmt.Fprintf(stdout, "  %-22s %-14d %-14d\n", "judge accept", agreeAccept, judgeAcceptHumanReject)
	fmt.Fprintf(stdout, "  %-22s %-14d %-14d\n", "judge reject", judgeRejectHumanAccept, agreeReject)
	writeLine(stdout, "")

	// Overall agreement = (agree_accept + agree_reject) / total.
	agreements := agreeAccept + agreeReject
	fmt.Fprintf(stdout, "agreement rate: %.3f (%d/%d)\n", float64(agreements)/float64(total), agreements, total)

	// Cohen's kappa for the 2x2 judge-vs-human table:
	//   po = observed agreement; pe = chance agreement from the marginals.
	//   kappa = (po - pe) / (1 - pe). When pe == 1 (a rater used a single label
	//   for every row) kappa is undefined: report 1.000 only when observed
	//   agreement is also perfect, otherwise "n/a" rather than a misleading 1.0.
	po := float64(agreements) / float64(total)
	judgeAcceptTotal := agreeAccept + judgeAcceptHumanReject
	humanAcceptTotal := agreeAccept + judgeRejectHumanAccept
	judgeRejectTotal := total - judgeAcceptTotal
	humanRejectTotal := total - humanAcceptTotal
	pe := (float64(judgeAcceptTotal)*float64(humanAcceptTotal) + float64(judgeRejectTotal)*float64(humanRejectTotal)) / (float64(total) * float64(total))
	switch {
	case pe < 1:
		fmt.Fprintf(stdout, "cohen's kappa: %.3f\n", (po-pe)/(1-pe))
	case po >= 1:
		writeLine(stdout, "cohen's kappa: 1.000")
	default:
		writeLine(stdout, "cohen's kappa: n/a (degenerate: a rater used a single label)")
	}
	writeLine(stdout, "")

	renderSkillOptJudgeCalibration(stdout, outcomes)
	renderSkillOptJudgeDimensionDisagreement(stdout, outcomes)
}

// skillOptJudgeBands are the soft-score calibration bands, low edge inclusive.
var skillOptJudgeBands = []struct {
	label string
	low   float64
	high  float64
}{
	{"[0.00,0.25)", 0.00, 0.25},
	{"[0.25,0.50)", 0.25, 0.50},
	{"[0.50,0.75)", 0.50, 0.75},
	{"[0.75,1.00]", 0.75, 1.01},
}

// renderSkillOptJudgeCalibration buckets outcomes by the judge's soft score and
// reports the human promote rate within each band — a calibration curve showing
// whether higher judge confidence tracks more human promotions.
func renderSkillOptJudgeCalibration(stdout io.Writer, outcomes []db.SkillOptJudgeOutcome) {
	type bandStat struct {
		count    int
		promoted int
	}
	stats := make([]bandStat, len(skillOptJudgeBands))
	unscored := 0
	for _, outcome := range outcomes {
		soft, ok := skillOptJudgeSoftScore(outcome.JudgeScoreJSON)
		if !ok {
			unscored++
			continue
		}
		humanPromoted := outcome.HumanDecision == "promoted"
		// Clamp out-of-range soft scores into [0,1] so a malformed value lands in
		// an edge band rather than being silently dropped from the calibration.
		if soft < 0 {
			soft = 0
		}
		if soft > 1 {
			soft = 1
		}
		for index, band := range skillOptJudgeBands {
			if soft >= band.low && soft < band.high {
				stats[index].count++
				if humanPromoted {
					stats[index].promoted++
				}
				break
			}
		}
	}
	writeLine(stdout, "calibration (judge soft-score band vs human promote rate)")
	fmt.Fprintf(stdout, "  %-14s %-8s %s\n", "BAND", "N", "HUMAN PROMOTE RATE")
	for index, band := range skillOptJudgeBands {
		stat := stats[index]
		rate := "-"
		if stat.count > 0 {
			rate = fmt.Sprintf("%.3f (%d/%d)", float64(stat.promoted)/float64(stat.count), stat.promoted, stat.count)
		}
		fmt.Fprintf(stdout, "  %-14s %-8d %s\n", band.label, stat.count, rate)
	}
	if unscored > 0 {
		fmt.Fprintf(stdout, "  %-14s %-8d %s\n", "no soft score", unscored, "-")
	}
	writeLine(stdout, "")
}

// renderSkillOptJudgeDimensionDisagreement, when dimension_scores are present in
// the captured eval reports, reports each dimension's mean score split by human
// promote vs reject, plus the gap between them — surfacing dimensions where the
// judge's per-dimension score diverges from the human decision.
func renderSkillOptJudgeDimensionDisagreement(stdout io.Writer, outcomes []db.SkillOptJudgeOutcome) {
	type dimStat struct {
		promoteSum   float64
		promoteCount int
		rejectSum    float64
		rejectCount  int
	}
	stats := map[string]*dimStat{}
	for _, outcome := range outcomes {
		dimensions := skillOptJudgeDimensionScores(outcome.JudgeScoreJSON)
		humanPromoted := outcome.HumanDecision == "promoted"
		for name, score := range dimensions {
			stat := stats[name]
			if stat == nil {
				stat = &dimStat{}
				stats[name] = stat
			}
			if humanPromoted {
				stat.promoteSum += score
				stat.promoteCount++
			} else {
				stat.rejectSum += score
				stat.rejectCount++
			}
		}
	}
	if len(stats) == 0 {
		return
	}
	names := make([]string, 0, len(stats))
	for name := range stats {
		names = append(names, name)
	}
	sort.Strings(names)
	writeLine(stdout, "per-dimension disagreement (mean judge dimension score by human decision)")
	fmt.Fprintf(stdout, "  %-22s %-16s %-16s %s\n", "DIMENSION", "PROMOTE MEAN", "REJECT MEAN", "GAP")
	for _, name := range names {
		stat := stats[name]
		promoteMean, promoteOK := skillOptJudgeMean(stat.promoteSum, stat.promoteCount)
		rejectMean, rejectOK := skillOptJudgeMean(stat.rejectSum, stat.rejectCount)
		gap := "-"
		if promoteOK && rejectOK {
			gap = fmt.Sprintf("%+.3f", promoteMean-rejectMean)
		}
		fmt.Fprintf(stdout, "  %-22s %-16s %-16s %s\n", name, skillOptJudgeMeanText(promoteMean, promoteOK, stat.promoteCount), skillOptJudgeMeanText(rejectMean, rejectOK, stat.rejectCount), gap)
	}
	writeLine(stdout, "")
}

func skillOptJudgeMean(sum float64, count int) (float64, bool) {
	if count == 0 {
		return 0, false
	}
	return sum / float64(count), true
}

func skillOptJudgeMeanText(mean float64, ok bool, count int) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.3f (n=%d)", mean, count)
}

// skillOptJudgeReportRoot decodes a captured judge eval report and returns the
// candidate sources to inspect: the report root plus any nested evaluator_score
// object (mirroring how the report is read at capture time).
func skillOptJudgeReportRoot(judgeScoreJSON string) []map[string]any {
	judgeScoreJSON = strings.TrimSpace(judgeScoreJSON)
	if judgeScoreJSON == "" {
		return nil
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(judgeScoreJSON), &report); err != nil {
		return nil
	}
	sources := []map[string]any{report}
	if nested := decodedSkillOptMetadataValue(report["evaluator_score"]); len(nested) > 0 {
		sources = append(sources, nested)
	}
	return sources
}

// skillOptJudgeSoftScore extracts the continuous judge score used for the
// calibration curve. It mirrors the soft-score fallback chain of the verdict
// heuristic (skillOptJudgeAcceptFromReport in internal/db/store.go): "soft",
// then the landing-page profile's "best_selection_soft"/"best_selection_hard".
// Keeping the two in lockstep means every report that produced an accept/reject
// verdict is also banded in calibration — otherwise landing-page reports (which
// carry best_selection_soft, not a top-level "soft") would be silently dropped
// into "no soft score", blinding calibration to the dominant real report shape.
func skillOptJudgeSoftScore(judgeScoreJSON string) (float64, bool) {
	for _, source := range skillOptJudgeReportRoot(judgeScoreJSON) {
		for _, key := range []string{"soft", "best_selection_soft", "best_selection_hard"} {
			if value, ok := skillOptJudgeFloatValue(source[key]); ok {
				return value, true
			}
		}
	}
	return 0, false
}

func skillOptJudgeDimensionScores(judgeScoreJSON string) map[string]float64 {
	for _, source := range skillOptJudgeReportRoot(judgeScoreJSON) {
		raw := decodedSkillOptMetadataValue(source["dimension_scores"])
		if len(raw) == 0 {
			continue
		}
		scores := make(map[string]float64, len(raw))
		for name, value := range raw {
			if score, ok := skillOptJudgeFloatValue(value); ok {
				scores[name] = score
			}
		}
		if len(scores) > 0 {
			return scores
		}
	}
	return nil
}

func skillOptJudgeFloatValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func runSkillOptJudge(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptJudgeUsage(stdout)
		return 0
	}
	switch args[0] {
	case "promote":
		return runSkillOptJudgePromote(args[1:], stdout, stderr)
	case "agreement":
		return runSkillOptJudgeAgreement(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt judge command %q\n\n", args[0])
		printSkillOptJudgeUsage(stderr)
		return 2
	}
}

func printSkillOptJudgeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt judge promote --template <id> --task-kind <kind> --file <pkg.json> [--home <h>] [--yes] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt judge agreement [--template <id>] [--home <h>] [--json]")
}

// skillOptJudgePromoteResult is the machine-readable preview/apply summary for
// `skillopt judge promote`. Applied is false in preview mode (no --yes).
type skillOptJudgePromoteResult struct {
	TemplateID            string  `json:"template_id"`
	TaskKind              string  `json:"task_kind"`
	Applied               bool    `json:"applied"`
	Accepted              bool    `json:"accepted"`
	BaselineAgreement     float64 `json:"baseline_agreement"`
	BestAgreement         float64 `json:"best_agreement"`
	AgreementDelta        float64 `json:"agreement_delta"`
	BestOrigin            string  `json:"best_origin,omitempty"`
	PreviousPromptVersion string  `json:"previous_judge_prompt_version,omitempty"`
	NewPromptVersion      string  `json:"new_judge_prompt_version,omitempty"`
	PromptBytes           int     `json:"prompt_bytes"`
	PromptPreview         string  `json:"prompt_preview,omitempty"`
}

const skillOptJudgePromptPreviewLimit = 800

func runSkillOptJudgePromote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt judge promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "agent template id to promote the judge prompt into")
	taskKind := fs.String("task-kind", "", "task kind variant to promote (use _global for the all-items pass)")
	file := fs.String("file", "", "judge candidate package JSON file")
	yes := fs.Bool("yes", false, "apply the promotion; without it the command previews and writes nothing")
	jsonOutput := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt judge promote does not accept positional arguments")
		return 2
	}
	templateRef := strings.TrimSpace(*templateID)
	if templateRef == "" {
		fmt.Fprintln(stderr, "skillopt judge promote requires --template")
		return 2
	}
	kind := strings.TrimSpace(*taskKind)
	if kind == "" {
		fmt.Fprintln(stderr, "skillopt judge promote requires --task-kind")
		return 2
	}
	packagePath := strings.TrimSpace(*file)
	if packagePath == "" {
		fmt.Fprintln(stderr, "skillopt judge promote requires --file")
		return 2
	}
	data, err := os.ReadFile(packagePath)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt judge promote: read package: %v\n", err)
		return 2
	}
	pkg, err := skillopt.ParseJudgeCandidatePackage(data)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt judge promote: %v\n", err)
		return 2
	}
	variant, ok := pkg.Variants[kind]
	if !ok {
		fmt.Fprintf(stderr, "skillopt judge promote: task-kind %q not found in package variants\n", kind)
		return 2
	}
	if !variant.Accepted {
		fmt.Fprintf(stderr, "skillopt judge promote: variant %q was not accepted by the judge optimizer (best_origin=%q); refusing to promote\n", kind, variant.BestOrigin)
		return 1
	}
	if strings.TrimSpace(variant.BestPrompt) == "" {
		fmt.Fprintf(stderr, "skillopt judge promote: variant %q has an empty best_prompt; nothing to promote\n", kind)
		return 1
	}

	result := skillOptJudgePromoteResult{
		TemplateID:        templateRef,
		TaskKind:          kind,
		Accepted:          variant.Accepted,
		BaselineAgreement: variant.BaselineAgreement,
		BestAgreement:     variant.BestAgreement,
		AgreementDelta:    variant.BestAgreement - variant.BaselineAgreement,
		BestOrigin:        variant.BestOrigin,
		NewPromptVersion:  strings.TrimSpace(variant.JudgePromptVersion),
		PromptBytes:       len(variant.BestPrompt),
		PromptPreview:     truncateSkillOptJudgePrompt(variant.BestPrompt),
	}

	// Preview by default; only --yes writes.
	openStore := withReadOnlyStore
	if *yes {
		openStore = withStore
	}
	if err := openStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		template, err := loadInstalledTemplate(ctx, store, templateRef)
		if err != nil {
			return err
		}
		metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON)
		if err != nil {
			return fmt.Errorf("decode template metadata: %w", err)
		}
		result.PreviousPromptVersion = strings.TrimSpace(metadata.Evaluation["judge_prompt_version"])
		updatedMetadata, err := applyJudgePromptToMetadata(metadata, kind, variant)
		if err != nil {
			return err
		}
		if !*yes {
			return nil
		}
		metadataJSON, err := agenttemplate.MarshalMetadata(updatedMetadata)
		if err != nil {
			return fmt.Errorf("encode template metadata: %w", err)
		}
		if _, err := store.UpdateAgentTemplateMetadata(ctx, template.ID, metadataJSON); err != nil {
			return err
		}
		result.Applied = true
		reason := fmt.Sprintf("judge prompt promoted for task_kind=%s: agreement %.3f→%.3f (delta %+.3f), origin=%s, version %s→%s",
			kind, variant.BaselineAgreement, variant.BestAgreement, result.AgreementDelta, variant.BestOrigin,
			emptyToDash(result.PreviousPromptVersion), emptyToDash(result.NewPromptVersion))
		outcome := db.SkillOptJudgeOutcome{
			CandidateVersionID: judgeOutcomeCandidateVersionID(template),
			TemplateID:         template.ID,
			JudgePromptVersion: result.NewPromptVersion,
			HumanDecision:      "promoted",
			// The human promoted and the judge optimizer already accepted this
			// variant (the accepted-gate above), so the decision agrees with the
			// judge's signal.
			Direction: db.SkillOptJudgeDirectionAgreeAccept,
			Reason:    reason,
		}
		if err := store.InsertSkillOptJudgeOutcome(ctx, outcome); err != nil {
			return fmt.Errorf("record judge outcome: %w", err)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt judge promote: %v\n", err)
		return 1
	}

	if *jsonOutput {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "skillopt judge promote: %v\n", err)
			return 1
		}
		return 0
	}
	printSkillOptJudgePromoteResult(stdout, result)
	return 0
}

// applyJudgePromptToMetadata folds an accepted judge variant into a template's
// flat Evaluation map: judge_prompt_templates is stored as a JSON-encoded
// map[task_kind]string (merging so sibling task kinds are preserved), and
// judge_prompt_version records the variant's version. The encoded map is read by
// judgePromptConfigFromConfig.
func applyJudgePromptToMetadata(metadata agenttemplate.Metadata, taskKind string, variant skillopt.JudgeCandidateVariant) (agenttemplate.Metadata, error) {
	templates := map[string]string{}
	if existing := strings.TrimSpace(metadata.Evaluation["judge_prompt_templates"]); existing != "" {
		if err := json.Unmarshal([]byte(existing), &templates); err != nil {
			// Existing value is not a JSON object map; start fresh rather than
			// silently dropping the new prompt.
			templates = map[string]string{}
		}
	}
	templates[taskKind] = variant.BestPrompt
	encoded, err := json.Marshal(templates)
	if err != nil {
		return agenttemplate.Metadata{}, fmt.Errorf("encode judge prompt templates: %w", err)
	}
	if metadata.Evaluation == nil {
		metadata.Evaluation = map[string]string{}
	}
	metadata.Evaluation["judge_prompt_templates"] = string(encoded)
	if version := strings.TrimSpace(variant.JudgePromptVersion); version != "" {
		metadata.Evaluation["judge_prompt_version"] = version
	}
	return metadata, nil
}

// judgeOutcomeCandidateVersionID picks a non-empty candidate_version_id for the
// audit row (the column is NOT NULL): the template's current version when known,
// otherwise the template id itself, since judge-prompt promotion targets the
// template rather than a specific candidate version.
func judgeOutcomeCandidateVersionID(template db.AgentTemplate) string {
	if id := strings.TrimSpace(template.VersionID); id != "" {
		return id
	}
	return template.ID
}

func truncateSkillOptJudgePrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if utf8.RuneCountInString(prompt) <= skillOptJudgePromptPreviewLimit {
		return prompt
	}
	runes := []rune(prompt)
	return string(runes[:skillOptJudgePromptPreviewLimit]) + "…"
}

func emptyToDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func printSkillOptJudgePromoteResult(w io.Writer, result skillOptJudgePromoteResult) {
	writeLine(w, "template: %s", result.TemplateID)
	writeLine(w, "task_kind: %s", result.TaskKind)
	writeLine(w, "accepted: %t", result.Accepted)
	writeLine(w, "agreement: %.3f → %.3f (delta %+.3f)", result.BaselineAgreement, result.BestAgreement, result.AgreementDelta)
	if result.BestOrigin != "" {
		writeLine(w, "best_origin: %s", result.BestOrigin)
	}
	writeLine(w, "judge_prompt_version: %s → %s", emptyToDash(result.PreviousPromptVersion), emptyToDash(result.NewPromptVersion))
	writeLine(w, "prompt_bytes: %d", result.PromptBytes)
	if result.PromptPreview != "" {
		writeLine(w, "prompt_preview:")
		writeLine(w, "%s", result.PromptPreview)
	}
	if result.Applied {
		writeLine(w, "applied: wrote judge prompt into template %s", result.TemplateID)
	} else {
		writeLine(w, "preview only: nothing was written. Re-run with --yes to apply.")
	}
}
