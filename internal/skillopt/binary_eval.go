package skillopt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// BINEVAL-style binary evaluation (#525). A rubric dimension is decomposed into
// small, independent yes/no questions; each question is answered on its own and
// the per-question verdicts aggregate back into a per-dimension score and an
// overall score. This is an ADDITIVE, opt-in evaluation mode — it never runs
// unless `gitmoot skillopt binary run` is invoked, so every existing SkillOpt
// review/optimize flow is byte-identical when it is unused.
//
// Reference: "Ask, Don't Judge: Binary Questions for Interpretable LLM
// Evaluation and Self-Improvement" (arXiv:2606.27226), §3.1/§3.2.

// BinaryQuestionSetVersion is the only supported question-set schema version.
const BinaryQuestionSetVersion = 1

// Binary verdict values.
const (
	BinaryVerdictYes = "yes"
	BinaryVerdictNo  = "no"
)

// BinaryQuestion is one atomic yes/no check. Weight defaults to 1. The optional
// ViolationExample documents what a "no" looks like (carried through to the LLM
// prompt). The Contains/NotContains/Regex/NotRegex fields are consulted ONLY by
// the deterministic rule-based runner (RuleBasedBinaryEvaluator) — they are the
// test/deterministic mode and are ignored by the LLM-backed runner.
type BinaryQuestion struct {
	ID               string  `json:"id" yaml:"id"`
	Text             string  `json:"text" yaml:"text"`
	ViolationExample string  `json:"violation_example,omitempty" yaml:"violation_example,omitempty"`
	Weight           float64 `json:"weight,omitempty" yaml:"weight,omitempty"`

	// Deterministic/test-mode assertions (rule-based runner only). A question is
	// answered "yes" only when EVERY specified assertion holds against the source.
	Contains    string `json:"contains,omitempty" yaml:"contains,omitempty"`
	NotContains string `json:"not_contains,omitempty" yaml:"not_contains,omitempty"`
	Regex       string `json:"regex,omitempty" yaml:"regex,omitempty"`
	NotRegex    string `json:"not_regex,omitempty" yaml:"not_regex,omitempty"`
}

// BinaryDimension groups related questions. Weight defaults to 1 and scales the
// dimension's contribution to the overall score.
type BinaryDimension struct {
	Name      string           `json:"name" yaml:"name"`
	Weight    float64          `json:"weight,omitempty" yaml:"weight,omitempty"`
	Questions []BinaryQuestion `json:"questions" yaml:"questions"`
}

// BinaryQuestionSet is a loadable (YAML or JSON) BINEVAL question set.
type BinaryQuestionSet struct {
	Version            int               `json:"version" yaml:"version"`
	TemplateOrTaskKind string            `json:"template_or_task_kind,omitempty" yaml:"template_or_task_kind,omitempty"`
	Dimensions         []BinaryDimension `json:"dimensions" yaml:"dimensions"`
}

// BinaryAnswer is a single question's verdict + explanation returned by a runner.
type BinaryAnswer struct {
	Verdict     string `json:"verdict"`
	Explanation string `json:"explanation,omitempty"`
}

// BinaryVerdict is a persisted/exported per-question result. It is the row shape
// of skillopt_binary_verdicts and the element type of the optional export packet
// section, so its JSON tags are stable wire.
type BinaryVerdict struct {
	QuestionID  string `json:"question_id"`
	Dimension   string `json:"dimension"`
	Verdict     string `json:"verdict"`
	Explanation string `json:"explanation,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// BinaryEvaluationResult is the aggregated outcome of a run: every verdict, the
// per-dimension weighted yes-fractions, and the weighted-mean overall score.
type BinaryEvaluationResult struct {
	Verdicts        []BinaryVerdict    `json:"verdicts"`
	DimensionScores map[string]float64 `json:"dimension_scores"`
	Overall         float64            `json:"overall"`
}

// BinaryEvaluator answers ONE question at a time against a source output. The
// two provided implementations are RuleBasedBinaryEvaluator (deterministic, for
// tests) and the LLM-backed runner wired in the CLI layer (opt-in).
type BinaryEvaluator interface {
	Answer(ctx context.Context, dimension string, question BinaryQuestion, source string) (BinaryAnswer, error)
}

// Normalize applies schema defaults in place: version defaults to 1, every
// missing/zero weight defaults to 1, and text fields are trimmed. It is
// idempotent and always runs before Validate.
func (s *BinaryQuestionSet) Normalize() {
	if s.Version == 0 {
		s.Version = BinaryQuestionSetVersion
	}
	s.TemplateOrTaskKind = strings.TrimSpace(s.TemplateOrTaskKind)
	for di := range s.Dimensions {
		d := &s.Dimensions[di]
		d.Name = strings.TrimSpace(d.Name)
		if d.Weight <= 0 {
			d.Weight = 1
		}
		for qi := range d.Questions {
			q := &d.Questions[qi]
			q.ID = strings.TrimSpace(q.ID)
			q.Text = strings.TrimSpace(q.Text)
			if q.Weight <= 0 {
				q.Weight = 1
			}
		}
	}
}

// Validate enforces the schema invariants: supported version, at least one
// dimension, unique non-empty dimension names, and globally-unique non-empty
// question ids with non-empty text. Call Normalize first (LoadBinaryQuestionSet
// and RunBinaryEvaluation both do).
func (s *BinaryQuestionSet) Validate() error {
	if s.Version != BinaryQuestionSetVersion {
		return fmt.Errorf("unsupported binary question set version %d (want %d)", s.Version, BinaryQuestionSetVersion)
	}
	if len(s.Dimensions) == 0 {
		return errors.New("binary question set has no dimensions")
	}
	seenDim := map[string]struct{}{}
	seenID := map[string]struct{}{}
	total := 0
	for _, d := range s.Dimensions {
		if d.Name == "" {
			return errors.New("binary question set has a dimension with an empty name")
		}
		if _, dup := seenDim[d.Name]; dup {
			return fmt.Errorf("binary question set has duplicate dimension %q", d.Name)
		}
		seenDim[d.Name] = struct{}{}
		if len(d.Questions) == 0 {
			return fmt.Errorf("dimension %q has no questions", d.Name)
		}
		for _, q := range d.Questions {
			if q.ID == "" {
				return fmt.Errorf("dimension %q has a question with an empty id", d.Name)
			}
			if _, dup := seenID[q.ID]; dup {
				return fmt.Errorf("binary question set has duplicate question id %q", q.ID)
			}
			seenID[q.ID] = struct{}{}
			if q.Text == "" {
				return fmt.Errorf("question %q has empty text", q.ID)
			}
			if q.Regex != "" {
				if _, err := regexp.Compile(q.Regex); err != nil {
					return fmt.Errorf("question %q has invalid regex: %w", q.ID, err)
				}
			}
			if q.NotRegex != "" {
				if _, err := regexp.Compile(q.NotRegex); err != nil {
					return fmt.Errorf("question %q has invalid not_regex: %w", q.ID, err)
				}
			}
			total++
		}
	}
	if total == 0 {
		return errors.New("binary question set has no questions")
	}
	return nil
}

// LoadBinaryQuestionSet reads a question set from a .yaml/.yml/.json file (YAML
// is a JSON superset, so a .json extension is parsed the same way), applies
// defaults, and validates it.
func LoadBinaryQuestionSet(path string) (BinaryQuestionSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return BinaryQuestionSet{}, err
	}
	return ParseBinaryQuestionSet(raw, filepath.Ext(path))
}

// ParseBinaryQuestionSet decodes a question set from bytes. ext selects the
// decoder ("" / ".yaml" / ".yml" / ".json"); YAML decoding accepts JSON too.
func ParseBinaryQuestionSet(raw []byte, ext string) (BinaryQuestionSet, error) {
	var set BinaryQuestionSet
	if err := yaml.Unmarshal(raw, &set); err != nil {
		return BinaryQuestionSet{}, fmt.Errorf("decode binary question set: %w", err)
	}
	set.Normalize()
	if err := set.Validate(); err != nil {
		return BinaryQuestionSet{}, err
	}
	return set, nil
}

// RunBinaryEvaluation asks every question ONE AT A TIME through the evaluator,
// records the verdicts, and aggregates them. It normalizes+validates the set
// first so callers can pass a raw set. A blank/unknown verdict from the
// evaluator is coerced to "no" (fail-safe) so a garbled answer never fabricates
// a pass.
func RunBinaryEvaluation(ctx context.Context, set BinaryQuestionSet, source string, evaluator BinaryEvaluator) (BinaryEvaluationResult, error) {
	if evaluator == nil {
		return BinaryEvaluationResult{}, errors.New("binary evaluator is required")
	}
	set.Normalize()
	if err := set.Validate(); err != nil {
		return BinaryEvaluationResult{}, err
	}
	result := BinaryEvaluationResult{DimensionScores: map[string]float64{}}
	var overallNum, overallDen float64
	for _, d := range set.Dimensions {
		var dimNum, dimDen float64
		for _, q := range d.Questions {
			answer, err := evaluator.Answer(ctx, d.Name, q, source)
			if err != nil {
				return BinaryEvaluationResult{}, fmt.Errorf("answer question %q: %w", q.ID, err)
			}
			verdict := normalizeVerdict(answer.Verdict)
			result.Verdicts = append(result.Verdicts, BinaryVerdict{
				QuestionID:  q.ID,
				Dimension:   d.Name,
				Verdict:     verdict,
				Explanation: strings.TrimSpace(answer.Explanation),
			})
			dimDen += q.Weight
			if verdict == BinaryVerdictYes {
				dimNum += q.Weight
			}
		}
		dimScore := 0.0
		if dimDen > 0 {
			dimScore = dimNum / dimDen
		}
		result.DimensionScores[d.Name] = dimScore
		overallNum += d.Weight * dimScore
		overallDen += d.Weight
	}
	if overallDen > 0 {
		result.Overall = overallNum / overallDen
	}
	return result, nil
}

// normalizeVerdict coerces any verdict to the canonical yes/no, defaulting a
// blank/unknown value to "no" (fail-safe).
func normalizeVerdict(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case BinaryVerdictYes, "true", "y", "pass":
		return BinaryVerdictYes
	default:
		return BinaryVerdictNo
	}
}

// ToEvaluatorScore maps the aggregated result onto the existing EvaluatorScore
// shape WITHOUT any contract change: the per-dimension weighted yes-fractions
// populate DimensionScores. TaskKind carries the set's template_or_task_kind so
// the score is attributable. The overall score is intentionally left off the
// contract (it is recoverable as the weighted mean of DimensionScores and is
// surfaced separately in the CLI/result).
func (r BinaryEvaluationResult) ToEvaluatorScore(taskKind string) *EvaluatorScore {
	dims := make(map[string]float64, len(r.DimensionScores))
	for k, v := range r.DimensionScores {
		dims[k] = v
	}
	return &EvaluatorScore{
		TaskKind:        strings.TrimSpace(taskKind),
		DimensionScores: dims,
	}
}

// RuleBasedBinaryEvaluator is the DETERMINISTIC, no-LLM runner used by tests and
// by `binary run --deterministic`. It answers each question purely from the
// question's Contains/NotContains/Regex/NotRegex assertions applied to the
// source output: "yes" iff every specified assertion holds. A question with NO
// assertions cannot be judged deterministically and is answered "no" with an
// explanatory note (fail-safe — it never fabricates a pass).
type RuleBasedBinaryEvaluator struct{}

// Answer implements BinaryEvaluator deterministically.
func (RuleBasedBinaryEvaluator) Answer(_ context.Context, _ string, q BinaryQuestion, source string) (BinaryAnswer, error) {
	var checks []string
	pass := true
	has := false
	if q.Contains != "" {
		has = true
		if strings.Contains(source, q.Contains) {
			checks = append(checks, fmt.Sprintf("contains %q: yes", q.Contains))
		} else {
			pass = false
			checks = append(checks, fmt.Sprintf("contains %q: no", q.Contains))
		}
	}
	if q.NotContains != "" {
		has = true
		if !strings.Contains(source, q.NotContains) {
			checks = append(checks, fmt.Sprintf("not_contains %q: yes", q.NotContains))
		} else {
			pass = false
			checks = append(checks, fmt.Sprintf("not_contains %q: no", q.NotContains))
		}
	}
	if q.Regex != "" {
		has = true
		re, err := regexp.Compile(q.Regex)
		if err != nil {
			return BinaryAnswer{}, fmt.Errorf("question %q regex: %w", q.ID, err)
		}
		if re.MatchString(source) {
			checks = append(checks, "regex: yes")
		} else {
			pass = false
			checks = append(checks, "regex: no")
		}
	}
	if q.NotRegex != "" {
		has = true
		re, err := regexp.Compile(q.NotRegex)
		if err != nil {
			return BinaryAnswer{}, fmt.Errorf("question %q not_regex: %w", q.ID, err)
		}
		if !re.MatchString(source) {
			checks = append(checks, "not_regex: yes")
		} else {
			pass = false
			checks = append(checks, "not_regex: no")
		}
	}
	if !has {
		return BinaryAnswer{Verdict: BinaryVerdictNo, Explanation: "no deterministic assertion on question"}, nil
	}
	verdict := BinaryVerdictNo
	if pass {
		verdict = BinaryVerdictYes
	}
	return BinaryAnswer{Verdict: verdict, Explanation: strings.Join(checks, "; ")}, nil
}

// SortBinaryVerdicts orders verdicts by (dimension, question_id) so persisted
// and exported ordering is deterministic.
func SortBinaryVerdicts(verdicts []BinaryVerdict) {
	sort.SliceStable(verdicts, func(i, j int) bool {
		if verdicts[i].Dimension != verdicts[j].Dimension {
			return verdicts[i].Dimension < verdicts[j].Dimension
		}
		return verdicts[i].QuestionID < verdicts[j].QuestionID
	})
}
