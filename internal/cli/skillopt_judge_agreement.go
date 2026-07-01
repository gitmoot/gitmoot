package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// `gitmoot skillopt judge agreement` (#344, MEASURE the judge) — the read-only
// judge<->human agreement measurement harness. It joins the stored LLM-judge
// verdicts against the stored HUMAN verdicts on the SAME items and reports
// chance-corrected agreement, with Cohen's kappa as the HEADLINE metric (raw
// agreement overstates judge quality because it does not correct for chance —
// the epic's "Reliability without Validity" note).
//
// Two slices are measured:
//
//  1. PAIRWISE slice — Mode B A/B judge rows (source=skillopt-ab-judge, single
//     judge or jury aggregate) joined per (run_id, item_id) against the human
//     ranked rows (source=skillopt-ab, live-pairwise, markdown, github, ...)
//     on the same item. This is the slice skillopt_ab.go explicitly defers as
//     "NOT calibrated against human gold ... until a judge<->human agreement
//     capture lands (#344)". Multiple rows per rater side collapse to a
//     per-item MAJORITY verdict (ties are skipped and counted) so repeated
//     picks never inflate N. It also runs the position-bias audit over judge
//     rows carrying the recorded raw a|b pick: |P(pick=a) - 0.5|.
//  2. CANDIDATE slice — the #345 skillopt_judge_outcomes capture (judge
//     accept/reject vs human promote/reject), summarized here with the SAME
//     kappa-headline framing; `skillopt judge-report` remains the full
//     calibration view (confusion matrix, soft-score bands, dimensions).
//
// The command is off-by-default (it only runs when invoked), read-only over
// the store, and changes no existing flow.

// skillOptAgreementSmallNThreshold is the loud small-N caveat boundary: below
// this many joined observations the numbers are reported but flagged as not
// yet trustworthy. Sample size is the epic's own stated limiter (#344).
const skillOptAgreementSmallNThreshold = 30

// skillOptJudgeAgreementStats is one measured slice: N joined observations,
// how many agreed, the raw agreement rate, and Cohen's kappa (the headline).
// KappaDefined mirrors judge-report's degenerate-marginals stance: when chance
// agreement pe == 1 (both raters used one identical label for every row),
// kappa is 1.000 only if observed agreement is also perfect, otherwise
// undefined — never a misleading 1.0.
type skillOptJudgeAgreementStats struct {
	N            int     `json:"n"`
	Agreements   int     `json:"agreements"`
	Agreement    float64 `json:"agreement"`
	Kappa        float64 `json:"kappa"`
	KappaDefined bool    `json:"kappa_defined"`
	KappaNote    string  `json:"kappa_note,omitempty"`
}

// skillOptJudgeAgreementBreakdown is one keyed sub-slice (per human source or
// per juror family) with its own stats.
type skillOptJudgeAgreementBreakdown struct {
	Key string `json:"key"`
	skillOptJudgeAgreementStats
}

// skillOptJudgeAgreementPosition is the position-bias audit over judge/juror
// rows that carry the recorded raw presented-position pick: P(pick=a) should
// hover near 0.5 under the randomized label shuffle; a large |P(a) - 0.5| is
// position bias ("Reliability without Validity": a stable-but-biased judge is
// a failure mode, not a strength).
type skillOptJudgeAgreementPosition struct {
	N      int     `json:"n"`
	PickA  int     `json:"pick_a"`
	PPickA float64 `json:"p_pick_a"`
	Bias   float64 `json:"bias"`
}

// skillOptJudgeAgreementPairwise is the pairwise-slice report.
type skillOptJudgeAgreementPairwise struct {
	skillOptJudgeAgreementStats
	JudgeRows      int                               `json:"judge_rows"`
	HumanRows      int                               `json:"human_rows"`
	JurorRows      int                               `json:"juror_rows"`
	TiesSkipped    int                               `json:"ties_skipped"`
	PerSource      []skillOptJudgeAgreementBreakdown `json:"per_source,omitempty"`
	PerJurorFamily []skillOptJudgeAgreementBreakdown `json:"per_juror_family,omitempty"`
	Position       *skillOptJudgeAgreementPosition   `json:"position,omitempty"`
}

// skillOptJudgeAgreementReport is the full machine-readable report (--json).
type skillOptJudgeAgreementReport struct {
	Template        string                         `json:"template,omitempty"`
	Pairwise        skillOptJudgeAgreementPairwise `json:"pairwise"`
	Candidate       skillOptJudgeAgreementStats    `json:"candidate"`
	SmallNThreshold int                            `json:"small_n_threshold"`
	SmallNWarning   bool                           `json:"small_n_warning"`
}

func runSkillOptJudgeAgreement(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt judge agreement", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "template id to filter")
	jsonOutput := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt judge agreement does not accept positional arguments")
		return 2
	}
	var events []db.RankedFeedbackEventWithTemplate
	var outcomes []db.SkillOptJudgeOutcome
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		events, err = store.ListRankedFeedbackEventsAcrossRuns(context.Background(), *templateID)
		if err != nil {
			return err
		}
		outcomes, err = store.ListSkillOptJudgeOutcomes(context.Background(), *templateID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt judge agreement: %v\n", err)
		return 1
	}
	report := buildSkillOptJudgeAgreementReport(strings.TrimSpace(*templateID), events, outcomes)
	if *jsonOutput {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "skillopt judge agreement: encode report: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}
	renderSkillOptJudgeAgreementReport(stdout, report)
	return 0
}

// buildSkillOptJudgeAgreementReport computes the full report from the stored
// rows. Pure over its inputs so the math is unit-testable with exact fixtures.
func buildSkillOptJudgeAgreementReport(templateID string, events []db.RankedFeedbackEventWithTemplate, outcomes []db.SkillOptJudgeOutcome) skillOptJudgeAgreementReport {
	report := skillOptJudgeAgreementReport{
		Template:        templateID,
		Pairwise:        buildSkillOptJudgeAgreementPairwise(events),
		Candidate:       skillOptCandidateAgreementStats(outcomes),
		SmallNThreshold: skillOptAgreementSmallNThreshold,
	}
	report.SmallNWarning = report.Pairwise.N < skillOptAgreementSmallNThreshold || report.Candidate.N < skillOptAgreementSmallNThreshold
	return report
}

// skillOptAgreementRowKind classifies one ranked feedback row for the join.
const (
	skillOptAgreementRowJudge = "judge"
	skillOptAgreementRowJuror = "juror"
	skillOptAgreementRowHuman = "human"
	skillOptAgreementRowOther = "other"
)

// classifySkillOptAgreementRow decides which rater a ranked feedback row
// belongs to. Judge and juror rows are positively identified by their
// dedicated source tags; every machine writer of ranked feedback today uses
// one of the three machine sources (skillopt-ab-judge, skillopt-ab-juror,
// auto-trace — the auto-trace source also covers the gitmoot-review
// cross-family rubric rows, which ride the auto-trace run), so anything else
// is a human verdict (skillopt-ab, live-pairwise, markdown, github, ...). The
// auto-trace reviewer sentinel is ALSO excluded defensively in case a future
// writer reuses the reviewer without the source.
func classifySkillOptAgreementRow(event db.RankedFeedbackEvent) string {
	switch event.Source {
	case skillOptABJudgeSource:
		return skillOptAgreementRowJudge
	case skillOptABJurorSource:
		return skillOptAgreementRowJuror
	case skillopt.AutoTraceSource:
		return skillOptAgreementRowOther
	}
	if event.Reviewer == skillopt.AutoTraceReviewer {
		return skillOptAgreementRowOther
	}
	return skillOptAgreementRowHuman
}

// skillOptAgreementItem is the per-(run_id, item_id) join bucket: all judge,
// juror, and human winner votes on one item.
type skillOptAgreementItem struct {
	judgeWinners []string
	humanWinners []string
	humanSources map[string][]string // human source -> winners from that source
	jurorWinners map[string][]string // juror family -> winners from that family
}

// skillOptAgreementMajority collapses one side's votes to a single verdict:
// the strictly-most-frequent winner label, or ok=false on a tie (the item is
// skipped rather than resolved arbitrarily) or no votes.
func skillOptAgreementMajority(winners []string) (string, bool) {
	if len(winners) == 0 {
		return "", false
	}
	counts := map[string]int{}
	for _, winner := range winners {
		counts[winner]++
	}
	best, bestCount, tied := "", 0, false
	// Deterministic iteration so a tie is detected identically on every run.
	labels := make([]string, 0, len(counts))
	for label := range counts {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		switch {
		case counts[label] > bestCount:
			best, bestCount, tied = label, counts[label], false
		case counts[label] == bestCount:
			tied = true
		}
	}
	if tied {
		return "", false
	}
	return best, true
}

// skillOptAgreementStats computes raw agreement + multi-class Cohen's kappa
// over paired (judge label, human label) observations. po is the observed
// agreement; pe is the chance agreement from the two raters' label marginals;
// kappa = (po - pe) / (1 - pe). Degenerate pe >= 1 (both raters used one
// identical label everywhere) reports kappa=1 only when po is also perfect,
// otherwise undefined — mirroring judge-report's stance.
func skillOptAgreementStats(pairs [][2]string) skillOptJudgeAgreementStats {
	stats := skillOptJudgeAgreementStats{N: len(pairs)}
	if stats.N == 0 {
		return stats
	}
	judgeMarginals := map[string]int{}
	humanMarginals := map[string]int{}
	for _, pair := range pairs {
		if pair[0] == pair[1] {
			stats.Agreements++
		}
		judgeMarginals[pair[0]]++
		humanMarginals[pair[1]]++
	}
	total := float64(stats.N)
	po := float64(stats.Agreements) / total
	stats.Agreement = po
	pe := 0.0
	for label, judgeCount := range judgeMarginals {
		pe += (float64(judgeCount) / total) * (float64(humanMarginals[label]) / total)
	}
	switch {
	case pe < 1:
		stats.Kappa = (po - pe) / (1 - pe)
		stats.KappaDefined = true
	case po >= 1:
		stats.Kappa = 1
		stats.KappaDefined = true
	default:
		stats.KappaNote = "degenerate: a rater used a single label"
	}
	return stats
}

// buildSkillOptJudgeAgreementPairwise runs the pairwise-slice join: bucket
// rows by (run_id, item_id), collapse each side to its per-item majority,
// pair judge-vs-human per item, then compute overall/per-source/per-juror
// stats and the position audit.
func buildSkillOptJudgeAgreementPairwise(events []db.RankedFeedbackEventWithTemplate) skillOptJudgeAgreementPairwise {
	pairwise := skillOptJudgeAgreementPairwise{}
	items := map[string]*skillOptAgreementItem{}
	itemKeys := []string{}
	position := skillOptJudgeAgreementPosition{}
	positionSeen := false
	for _, event := range events {
		kind := classifySkillOptAgreementRow(event.RankedFeedbackEvent)
		if kind == skillOptAgreementRowOther || strings.TrimSpace(event.Winner) == "" {
			continue
		}
		key := event.RunID + "\x00" + event.ItemID
		item := items[key]
		if item == nil {
			item = &skillOptAgreementItem{humanSources: map[string][]string{}, jurorWinners: map[string][]string{}}
			items[key] = item
			itemKeys = append(itemKeys, key)
		}
		switch kind {
		case skillOptAgreementRowJudge:
			pairwise.JudgeRows++
			item.judgeWinners = append(item.judgeWinners, event.Winner)
		case skillOptAgreementRowJuror:
			pairwise.JurorRows++
			family := strings.TrimPrefix(event.Reviewer, skillOptABJurorReviewerPrefix)
			item.jurorWinners[family] = append(item.jurorWinners[family], event.Winner)
		case skillOptAgreementRowHuman:
			pairwise.HumanRows++
			item.humanWinners = append(item.humanWinners, event.Winner)
			item.humanSources[event.Source] = append(item.humanSources[event.Source], event.Winner)
		}
		// Position audit: any judge/juror row carrying a recorded raw pick.
		if kind == skillOptAgreementRowJudge || kind == skillOptAgreementRowJuror {
			if pick, ok := skillOptABJudgePickPosition(event.Reasoning); ok {
				positionSeen = true
				position.N++
				if pick == "a" {
					position.PickA++
				}
			}
		}
	}
	sort.Strings(itemKeys)

	var overallPairs [][2]string
	sourcePairs := map[string][][2]string{}
	jurorPairs := map[string][][2]string{}
	for _, key := range itemKeys {
		item := items[key]
		humanWinner, humanOK := skillOptAgreementMajority(item.humanWinners)
		judgeWinner, judgeOK := skillOptAgreementMajority(item.judgeWinners)
		if len(item.humanWinners) > 0 && len(item.judgeWinners) > 0 && (!humanOK || !judgeOK) {
			// The item HAS both verdicts but one side is internally tied: skip it
			// loudly (counted) instead of resolving the tie arbitrarily.
			pairwise.TiesSkipped++
		}
		if humanOK && judgeOK {
			overallPairs = append(overallPairs, [2]string{judgeWinner, humanWinner})
			// Per-source: compare the judge verdict against each contributing human
			// source's OWN majority, so a per-source line reflects that source's
			// reviewers only.
			for source, winners := range item.humanSources {
				if sourceWinner, ok := skillOptAgreementMajority(winners); ok {
					sourcePairs[source] = append(sourcePairs[source], [2]string{judgeWinner, sourceWinner})
				}
			}
		}
		// Per-juror-family: each family's own majority against the human majority
		// (independent of the aggregate judge verdict).
		if humanOK {
			for family, winners := range item.jurorWinners {
				if jurorWinner, ok := skillOptAgreementMajority(winners); ok {
					jurorPairs[family] = append(jurorPairs[family], [2]string{jurorWinner, humanWinner})
				}
			}
		}
	}

	pairwise.skillOptJudgeAgreementStats = skillOptAgreementStats(overallPairs)
	pairwise.PerSource = skillOptAgreementBreakdowns(sourcePairs)
	pairwise.PerJurorFamily = skillOptAgreementBreakdowns(jurorPairs)
	if positionSeen {
		position.PPickA = float64(position.PickA) / float64(position.N)
		position.Bias = math.Abs(position.PPickA - 0.5)
		pairwise.Position = &position
	}
	return pairwise
}

// skillOptAgreementBreakdowns turns keyed pair sets into sorted breakdown rows.
func skillOptAgreementBreakdowns(pairsByKey map[string][][2]string) []skillOptJudgeAgreementBreakdown {
	if len(pairsByKey) == 0 {
		return nil
	}
	keys := make([]string, 0, len(pairsByKey))
	for key := range pairsByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	breakdowns := make([]skillOptJudgeAgreementBreakdown, 0, len(keys))
	for _, key := range keys {
		breakdowns = append(breakdowns, skillOptJudgeAgreementBreakdown{Key: key, skillOptJudgeAgreementStats: skillOptAgreementStats(pairsByKey[key])})
	}
	return breakdowns
}

// skillOptCandidateAgreementStats maps the #345 candidate-level judge outcomes
// (accept/reject vs promote/reject directions) onto the same paired-label
// stats, so the candidate slice's kappa is computed by the identical formula
// as the pairwise slice (and matches judge-report's 2x2 kappa exactly).
func skillOptCandidateAgreementStats(outcomes []db.SkillOptJudgeOutcome) skillOptJudgeAgreementStats {
	var pairs [][2]string
	for _, outcome := range outcomes {
		switch outcome.Direction {
		case db.SkillOptJudgeDirectionAgreeAccept:
			pairs = append(pairs, [2]string{"accept", "accept"})
		case db.SkillOptJudgeDirectionAgreeReject:
			pairs = append(pairs, [2]string{"reject", "reject"})
		case db.SkillOptJudgeDirectionJudgeAcceptHumanReject:
			pairs = append(pairs, [2]string{"accept", "reject"})
		case db.SkillOptJudgeDirectionJudgeRejectHumanAccept:
			pairs = append(pairs, [2]string{"reject", "accept"})
		}
	}
	return skillOptAgreementStats(pairs)
}

func skillOptAgreementKappaText(stats skillOptJudgeAgreementStats) string {
	if stats.KappaDefined {
		return fmt.Sprintf("%.3f", stats.Kappa)
	}
	if stats.KappaNote != "" {
		return "n/a (" + stats.KappaNote + ")"
	}
	return "n/a"
}

func renderSkillOptJudgeAgreementReport(stdout io.Writer, report skillOptJudgeAgreementReport) {
	writeLine(stdout, "judge <-> human agreement (#344 measure-the-judge)")
	if report.Template != "" {
		writeLine(stdout, "template: %s", report.Template)
	}
	writeLine(stdout, "")

	pairwise := report.Pairwise
	writeLine(stdout, "pairwise slice (A/B judge vs human ranked feedback, per-item majority join)")
	writeLine(stdout, "  items joined: %d (judge rows: %d, human rows: %d, juror rows: %d, tied items skipped: %d)",
		pairwise.N, pairwise.JudgeRows, pairwise.HumanRows, pairwise.JurorRows, pairwise.TiesSkipped)
	if pairwise.N == 0 {
		writeLine(stdout, "  no overlap yet: no item carries BOTH a judge verdict and a human verdict")
	} else {
		writeLine(stdout, "  cohen's kappa (headline): %s", skillOptAgreementKappaText(pairwise.skillOptJudgeAgreementStats))
		writeLine(stdout, "  raw agreement: %.3f (%d/%d)", pairwise.Agreement, pairwise.Agreements, pairwise.N)
	}
	if len(pairwise.PerSource) > 0 {
		writeLine(stdout, "  per human source:")
		for _, breakdown := range pairwise.PerSource {
			writeLine(stdout, "    %-16s N=%-4d kappa %-8s agreement %.3f (%d/%d)", breakdown.Key, breakdown.N, skillOptAgreementKappaText(breakdown.skillOptJudgeAgreementStats), breakdown.Agreement, breakdown.Agreements, breakdown.N)
		}
	}
	if len(pairwise.PerJurorFamily) > 0 {
		writeLine(stdout, "  per juror family (juror majority vs human majority):")
		for _, breakdown := range pairwise.PerJurorFamily {
			writeLine(stdout, "    %-16s N=%-4d kappa %-8s agreement %.3f (%d/%d)", breakdown.Key, breakdown.N, skillOptAgreementKappaText(breakdown.skillOptJudgeAgreementStats), breakdown.Agreement, breakdown.Agreements, breakdown.N)
		}
	}
	if pairwise.Position != nil {
		writeLine(stdout, "  position audit (judge/juror rows with a recorded raw pick):")
		writeLine(stdout, "    N=%d  P(pick=a)=%.3f  position bias |P(a)-0.5|=%.3f", pairwise.Position.N, pairwise.Position.PPickA, pairwise.Position.Bias)
	} else {
		writeLine(stdout, "  position audit: no judge rows carry a recorded pick position yet (rows written before this capture are not measurable)")
	}
	writeLine(stdout, "")

	candidate := report.Candidate
	writeLine(stdout, "candidate slice (judge accept/reject vs human promote/reject — full calibration: `skillopt judge-report`)")
	if candidate.N == 0 {
		writeLine(stdout, "  no judge outcomes captured")
	} else {
		writeLine(stdout, "  N=%d  cohen's kappa (headline): %s  raw agreement: %.3f (%d/%d)", candidate.N, skillOptAgreementKappaText(candidate), candidate.Agreement, candidate.Agreements, candidate.N)
	}
	writeLine(stdout, "")

	if report.SmallNWarning {
		writeLine(stdout, "WARNING: small sample (pairwise N=%d, candidate N=%d; threshold %d): these numbers are NOT yet trustworthy — sample size is the limiter (#344). Accumulate more human picks/decisions before acting on them.",
			pairwise.N, candidate.N, report.SmallNThreshold)
	}
}
