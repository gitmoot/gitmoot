package skillopt

import (
	"fmt"
	"sort"
	"strings"
)

// Binary-disagreement lesson derivation (#527). BINEVAL (arXiv:2606.27226,
// §3.3/§3.4/§5.4) observes that a per-question yes/no verdict is a far more
// actionable improvement signal than a scalar score gap: a question whose
// verdict FLIPS across runs (candidate-vs-champion on the same question set, or
// repeated runs of one version) is a targeted, unstable check the template
// prompt can be made to reliably satisfy; a question that is a STABLE NO is a
// concrete lesson; a STABLE YES is a trait worth preserving.
//
// This module is a PURE, deterministic transform: it takes the per-question
// verdicts already persisted by `skillopt binary run` (#525) and turns them
// into lessons. It performs NO writes and NO LLM calls. The CLI layer
// (`skillopt binary lessons`) previews these lessons by default and, only with
// an explicit --apply, projects them onto RankedFeedbackEvent rows so the
// EXISTING optimizer + rubric-induce (#347) consume them with zero contract
// change. Nothing on any daemon/hot path calls into here.

// Binary-disagreement lesson kinds.
const (
	// BinaryLessonFlip marks a question whose verdict disagrees across the
	// compared runs (at least one yes AND at least one no): an unstable check
	// and a targeted improvement signal.
	BinaryLessonFlip = "flip"
	// BinaryLessonStableNo marks a question answered "no" in every compared run:
	// a concrete, repeatable failure lesson.
	BinaryLessonStableNo = "stable_no"
	// BinaryLessonStableYes marks a question answered "yes" in every compared
	// run: a trait worth preserving (positive signal).
	BinaryLessonStableYes = "stable_yes"
)

// BinaryVerdictObservation is one verdict for one question, tagged with the run
// and template version that produced it.
type BinaryVerdictObservation struct {
	RunID       string
	VersionID   string
	Verdict     string // canonical yes|no (normalizeVerdict is applied on ingest)
	Explanation string
}

// BinaryQuestionObservations groups every observed verdict for a single
// question across the compared runs.
type BinaryQuestionObservations struct {
	QuestionID   string
	Dimension    string
	Observations []BinaryVerdictObservation
}

// BinaryLesson is one derived, optimizer-consumable signal for a question.
// Sign is AspectSignPositive for a stable-yes trait and AspectSignNegative for
// a flip/stable-no lesson, matching the rubric-induce grounding convention.
type BinaryLesson struct {
	QuestionID   string   `json:"question_id"`
	Dimension    string   `json:"dimension,omitempty"`
	QuestionText string   `json:"question_text,omitempty"`
	Kind         string   `json:"kind"`
	Sign         string   `json:"sign"`
	Runs         int      `json:"runs"`
	YesCount     int      `json:"yes_count"`
	NoCount      int      `json:"no_count"`
	Versions     []string `json:"versions,omitempty"`
	Explanations []string `json:"explanations,omitempty"`
	// Text is the human-readable, framed lesson shown in the CLI preview
	// (e.g. "Unstable check ... Update the template ...").
	Text string `json:"text"`
	// Trait is the CONTENT-focused string projected into a RankedFeedbackEvent's
	// required_improvements (negative) or useful_traits (positive) on --apply. It
	// is deliberately the question's own wording + the failing explanations
	// (text+explanations) with minimal boilerplate, so the rubric inducer's
	// token-overlap clustering separates lessons by theme rather than collapsing
	// them on shared framing words.
	Trait string `json:"trait"`
}

// BinaryLessonsOptions tunes derivation. Zero values resolve to documented
// defaults via normalize().
type BinaryLessonsOptions struct {
	// MinRuns is the minimum number of observations a question must have to be
	// eligible at all (default/floor 1). A flip inherently needs >=2 and is
	// filtered independently of this floor.
	MinRuns int
	// IncludeStableYes controls whether stable-yes traits are emitted. Default
	// true — a preserved-behavior trait is a useful positive signal for the
	// rubric inducer.
	IncludeStableYes bool
	// includeStableYesSet records whether the caller explicitly set
	// IncludeStableYes so the default can be true without a tri-state field.
	includeStableYesSet bool
}

// WithIncludeStableYes returns a copy with IncludeStableYes set explicitly,
// distinguishing an intentional false from the zero-value default of true.
func (o BinaryLessonsOptions) WithIncludeStableYes(v bool) BinaryLessonsOptions {
	o.IncludeStableYes = v
	o.includeStableYesSet = true
	return o
}

func (o BinaryLessonsOptions) normalize() BinaryLessonsOptions {
	out := o
	if out.MinRuns < 1 {
		out.MinRuns = 1
	}
	if !out.includeStableYesSet {
		out.IncludeStableYes = true
	}
	return out
}

// GroupBinaryObservations collapses a flat list of (question, verdict)
// observations into per-question groups, preserving a deterministic
// (dimension, question_id) order. The dimension of a group is the first
// non-empty dimension seen for that question. Verdicts are normalized to the
// canonical yes/no here so a raw/garbled stored verdict is fail-safe ("no").
func GroupBinaryObservations(rows []struct {
	QuestionID  string
	Dimension   string
	RunID       string
	VersionID   string
	Verdict     string
	Explanation string
}) []BinaryQuestionObservations {
	byQuestion := map[string]*BinaryQuestionObservations{}
	var order []string
	for _, r := range rows {
		qid := strings.TrimSpace(r.QuestionID)
		if qid == "" {
			continue
		}
		g, ok := byQuestion[qid]
		if !ok {
			g = &BinaryQuestionObservations{QuestionID: qid, Dimension: strings.TrimSpace(r.Dimension)}
			byQuestion[qid] = g
			order = append(order, qid)
		}
		if g.Dimension == "" {
			g.Dimension = strings.TrimSpace(r.Dimension)
		}
		g.Observations = append(g.Observations, BinaryVerdictObservation{
			RunID:       strings.TrimSpace(r.RunID),
			VersionID:   strings.TrimSpace(r.VersionID),
			Verdict:     normalizeVerdict(r.Verdict),
			Explanation: strings.TrimSpace(r.Explanation),
		})
	}
	groups := make([]BinaryQuestionObservations, 0, len(order))
	for _, qid := range order {
		groups = append(groups, *byQuestion[qid])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Dimension != groups[j].Dimension {
			return groups[i].Dimension < groups[j].Dimension
		}
		return groups[i].QuestionID < groups[j].QuestionID
	})
	return groups
}

// DeriveBinaryLessons turns per-question observations into lessons. questionText
// (optional; keyed by question id) enriches the lesson label with the question's
// own text when available — the verdicts table stores only ids, so a caller may
// pass the loaded question set to recover the wording. The result is sorted by
// (dimension, question_id) for a stable, reproducible ordering.
func DeriveBinaryLessons(groups []BinaryQuestionObservations, questionText map[string]string, opts BinaryLessonsOptions) []BinaryLesson {
	opts = opts.normalize()
	var lessons []BinaryLesson
	for _, g := range groups {
		if len(g.Observations) < opts.MinRuns {
			continue
		}
		yes, no := 0, 0
		versions := newOrderedStringSet()
		posExpl := newOrderedStringSet()
		negExpl := newOrderedStringSet()
		for _, obs := range g.Observations {
			if obs.Verdict == BinaryVerdictYes {
				yes++
				if obs.Explanation != "" {
					posExpl.add(obs.Explanation)
				}
			} else {
				no++
				if obs.Explanation != "" {
					negExpl.add(obs.Explanation)
				}
			}
			if obs.VersionID != "" {
				versions.add(obs.VersionID)
			}
		}
		runs := len(g.Observations)
		label := strings.TrimSpace(questionText[g.QuestionID])
		if label == "" {
			label = g.QuestionID
		}

		var kind, sign, text string
		var explanations []string
		switch {
		case yes > 0 && no > 0:
			kind = BinaryLessonFlip
			sign = AspectSignNegative
			text = fmt.Sprintf("Unstable check %q (dimension %q): verdict flips across %d runs (%d yes / %d no). Update the template so this check is reliably satisfied.", label, g.Dimension, runs, yes, no)
			explanations = negExpl.values()
		case no > 0:
			kind = BinaryLessonStableNo
			sign = AspectSignNegative
			text = fmt.Sprintf("Consistently fails check %q (dimension %q) across %d run(s). Update the template to satisfy it.", label, g.Dimension, runs)
			explanations = negExpl.values()
		default:
			if !opts.IncludeStableYes {
				continue
			}
			kind = BinaryLessonStableYes
			sign = AspectSignPositive
			text = fmt.Sprintf("Reliably satisfies check %q (dimension %q) across %d run(s); preserve this behavior.", label, g.Dimension, runs)
			explanations = posExpl.values()
		}
		if len(explanations) > 0 {
			text += " Evidence: " + strings.Join(explanations, " | ")
		}

		// Trait is the content-focused projection: the question's own wording (or
		// id) plus the failing/passing explanations, without the framing
		// boilerplate that would otherwise dominate token clustering.
		trait := label
		if len(explanations) > 0 {
			trait += ": " + strings.Join(explanations, "; ")
		}

		lessons = append(lessons, BinaryLesson{
			QuestionID:   g.QuestionID,
			Dimension:    g.Dimension,
			QuestionText: strings.TrimSpace(questionText[g.QuestionID]),
			Kind:         kind,
			Sign:         sign,
			Runs:         runs,
			YesCount:     yes,
			NoCount:      no,
			Versions:     versions.values(),
			Explanations: explanations,
			Text:         text,
			Trait:        trait,
		})
	}
	return lessons
}

// QuestionTextIndex flattens a question set into a question_id -> text map so
// lesson derivation can recover the human wording the verdicts table does not
// persist.
func QuestionTextIndex(set BinaryQuestionSet) map[string]string {
	out := map[string]string{}
	for _, d := range set.Dimensions {
		for _, q := range d.Questions {
			id := strings.TrimSpace(q.ID)
			if id == "" {
				continue
			}
			out[id] = strings.TrimSpace(q.Text)
		}
	}
	return out
}
