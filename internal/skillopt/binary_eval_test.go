package skillopt

import (
	"context"
	"math"
	"testing"
)

func TestBinaryQuestionSetNormalizeDefaults(t *testing.T) {
	set := BinaryQuestionSet{
		Dimensions: []BinaryDimension{
			{Name: "clarity", Questions: []BinaryQuestion{{ID: "q1", Text: "clear?"}}},
		},
	}
	set.Normalize()
	if set.Version != BinaryQuestionSetVersion {
		t.Fatalf("version default = %d, want %d", set.Version, BinaryQuestionSetVersion)
	}
	if set.Dimensions[0].Weight != 1 {
		t.Fatalf("dimension weight default = %v, want 1", set.Dimensions[0].Weight)
	}
	if set.Dimensions[0].Questions[0].Weight != 1 {
		t.Fatalf("question weight default = %v, want 1", set.Dimensions[0].Questions[0].Weight)
	}
	if err := set.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestBinaryQuestionSetValidateDuplicateID(t *testing.T) {
	set := BinaryQuestionSet{
		Version: 1,
		Dimensions: []BinaryDimension{
			{Name: "a", Questions: []BinaryQuestion{{ID: "dup", Text: "x"}}},
			{Name: "b", Questions: []BinaryQuestion{{ID: "dup", Text: "y"}}},
		},
	}
	set.Normalize()
	if err := set.Validate(); err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestBinaryQuestionSetValidateErrors(t *testing.T) {
	cases := map[string]BinaryQuestionSet{
		"bad version":        {Version: 2, Dimensions: []BinaryDimension{{Name: "a", Questions: []BinaryQuestion{{ID: "q", Text: "t"}}}}},
		"no dimensions":      {Version: 1},
		"empty dim name":     {Version: 1, Dimensions: []BinaryDimension{{Questions: []BinaryQuestion{{ID: "q", Text: "t"}}}}},
		"dup dim":            {Version: 1, Dimensions: []BinaryDimension{{Name: "a", Questions: []BinaryQuestion{{ID: "q1", Text: "t"}}}, {Name: "a", Questions: []BinaryQuestion{{ID: "q2", Text: "t"}}}}},
		"empty question id":  {Version: 1, Dimensions: []BinaryDimension{{Name: "a", Questions: []BinaryQuestion{{Text: "t"}}}}},
		"empty text":         {Version: 1, Dimensions: []BinaryDimension{{Name: "a", Questions: []BinaryQuestion{{ID: "q"}}}}},
		"no questions in dim": {Version: 1, Dimensions: []BinaryDimension{{Name: "a"}}},
		"bad regex":          {Version: 1, Dimensions: []BinaryDimension{{Name: "a", Questions: []BinaryQuestion{{ID: "q", Text: "t", Regex: "("}}}}},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			if err := set.Validate(); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}

func TestParseBinaryQuestionSetYAML(t *testing.T) {
	yaml := `
version: 1
template_or_task_kind: bugfix
dimensions:
  - name: correctness
    weight: 2
    questions:
      - id: has_test
        text: Does the change add a test?
        contains: "func Test"
      - id: no_todo
        text: Is the change free of TODOs?
        not_contains: "TODO"
`
	set, err := ParseBinaryQuestionSet([]byte(yaml), ".yaml")
	if err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	if set.TemplateOrTaskKind != "bugfix" {
		t.Fatalf("template_or_task_kind = %q", set.TemplateOrTaskKind)
	}
	if len(set.Dimensions) != 1 || len(set.Dimensions[0].Questions) != 2 {
		t.Fatalf("unexpected shape: %+v", set)
	}
	if set.Dimensions[0].Weight != 2 {
		t.Fatalf("dimension weight = %v", set.Dimensions[0].Weight)
	}
}

func TestParseBinaryQuestionSetJSON(t *testing.T) {
	js := `{"version":1,"dimensions":[{"name":"d","questions":[{"id":"q","text":"t","contains":"x"}]}]}`
	set, err := ParseBinaryQuestionSet([]byte(js), ".json")
	if err != nil {
		t.Fatalf("parse json: %v", err)
	}
	if len(set.Dimensions) != 1 {
		t.Fatalf("unexpected shape: %+v", set)
	}
}

func TestRuleBasedEvaluatorVerdicts(t *testing.T) {
	ev := RuleBasedBinaryEvaluator{}
	ctx := context.Background()

	yes, err := ev.Answer(ctx, "d", BinaryQuestion{ID: "q", Text: "t", Contains: "hello"}, "hello world")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if yes.Verdict != BinaryVerdictYes {
		t.Fatalf("contains hit verdict = %q, want yes", yes.Verdict)
	}

	no, _ := ev.Answer(ctx, "d", BinaryQuestion{ID: "q", Text: "t", Contains: "missing"}, "hello world")
	if no.Verdict != BinaryVerdictNo {
		t.Fatalf("contains miss verdict = %q, want no", no.Verdict)
	}

	reNo, _ := ev.Answer(ctx, "d", BinaryQuestion{ID: "q", Text: "t", NotContains: "world"}, "hello world")
	if reNo.Verdict != BinaryVerdictNo {
		t.Fatalf("not_contains hit verdict = %q, want no", reNo.Verdict)
	}

	reYes, _ := ev.Answer(ctx, "d", BinaryQuestion{ID: "q", Text: "t", Regex: `^h.*d$`}, "hello world")
	if reYes.Verdict != BinaryVerdictYes {
		t.Fatalf("regex verdict = %q, want yes", reYes.Verdict)
	}

	// A question with no assertions cannot be judged deterministically -> no.
	none, _ := ev.Answer(ctx, "d", BinaryQuestion{ID: "q", Text: "t"}, "anything")
	if none.Verdict != BinaryVerdictNo {
		t.Fatalf("no-assertion verdict = %q, want no", none.Verdict)
	}
}

func TestRunBinaryEvaluationAggregation(t *testing.T) {
	set := BinaryQuestionSet{
		Version: 1,
		Dimensions: []BinaryDimension{
			{
				Name:   "correctness",
				Weight: 2,
				Questions: []BinaryQuestion{
					{ID: "c1", Text: "t", Contains: "alpha"},   // yes
					{ID: "c2", Text: "t", Contains: "missing"}, // no
				},
			},
			{
				Name:   "style",
				Weight: 1,
				Questions: []BinaryQuestion{
					{ID: "s1", Text: "t", Contains: "beta"}, // yes
				},
			},
		},
	}
	res, err := RunBinaryEvaluation(context.Background(), set, "alpha beta", RuleBasedBinaryEvaluator{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Verdicts) != 3 {
		t.Fatalf("verdicts = %d, want 3", len(res.Verdicts))
	}
	if got := res.DimensionScores["correctness"]; got != 0.5 {
		t.Fatalf("correctness score = %v, want 0.5", got)
	}
	if got := res.DimensionScores["style"]; got != 1.0 {
		t.Fatalf("style score = %v, want 1.0", got)
	}
	// overall = (2*0.5 + 1*1.0) / 3 = 2/3
	want := 2.0 / 3.0
	if math.Abs(res.Overall-want) > 1e-9 {
		t.Fatalf("overall = %v, want %v", res.Overall, want)
	}
}

func TestRunBinaryEvaluationWeightedQuestions(t *testing.T) {
	set := BinaryQuestionSet{
		Version: 1,
		Dimensions: []BinaryDimension{
			{
				Name: "d",
				Questions: []BinaryQuestion{
					{ID: "q1", Text: "t", Weight: 3, Contains: "yes"}, // yes (weight 3)
					{ID: "q2", Text: "t", Weight: 1, Contains: "no"},  // no  (weight 1)
				},
			},
		},
	}
	res, err := RunBinaryEvaluation(context.Background(), set, "yes only", RuleBasedBinaryEvaluator{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// weighted yes-fraction = 3 / (3+1) = 0.75
	if got := res.DimensionScores["d"]; math.Abs(got-0.75) > 1e-9 {
		t.Fatalf("weighted dim score = %v, want 0.75", got)
	}
}

// TestAggregateBinaryVerdictsMatchesRun proves the re-read aggregation (used by
// `skillopt binary show`) reproduces the SAME weighted scores RunBinaryEvaluation
// emits — for BOTH non-uniform dimension weights and non-uniform question weights
// — so the two commands never disagree on the same run (#525 review finding).
func TestAggregateBinaryVerdictsMatchesRun(t *testing.T) {
	set := BinaryQuestionSet{
		Version: 1,
		Dimensions: []BinaryDimension{
			{
				Name:   "correctness",
				Weight: 2,
				Questions: []BinaryQuestion{
					{ID: "q1", Text: "t", Weight: 3, Contains: "yes"}, // yes (w3)
					{ID: "q2", Text: "t", Weight: 1, Contains: "no"},  // no  (w1)
				},
			},
			{
				Name:   "style",
				Weight: 1,
				Questions: []BinaryQuestion{
					{ID: "q3", Text: "t", Contains: "yes"}, // yes (default w1)
				},
			},
		},
	}
	res, err := RunBinaryEvaluation(context.Background(), set, "yes only", RuleBasedBinaryEvaluator{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The verdicts carry the weights RunBinaryEvaluation used; re-aggregating them
	// must yield identical per-dimension and overall scores.
	dims, overall := AggregateBinaryVerdicts(res.Verdicts)
	for name, want := range res.DimensionScores {
		if math.Abs(dims[name]-want) > 1e-9 {
			t.Fatalf("re-read dim %s = %v, want %v", name, dims[name], want)
		}
	}
	if math.Abs(overall-res.Overall) > 1e-9 {
		t.Fatalf("re-read overall = %v, want %v", overall, res.Overall)
	}
	// Sanity: correctness = 3/4 (weighted), style = 1, overall = (2*0.75 + 1*1)/3.
	if math.Abs(dims["correctness"]-0.75) > 1e-9 {
		t.Fatalf("correctness = %v, want 0.75", dims["correctness"])
	}
	if math.Abs(overall-(2.0*0.75+1.0*1.0)/3.0) > 1e-9 {
		t.Fatalf("overall = %v, want %v", overall, (2.0*0.75+1.0*1.0)/3.0)
	}
}

// TestAggregateBinaryVerdictsDefaultsZeroWeight proves rows written before
// weights were persisted (zero weight) aggregate as equal-weight rather than
// collapsing a dimension to a divide-by-zero.
func TestAggregateBinaryVerdictsDefaultsZeroWeight(t *testing.T) {
	verdicts := []BinaryVerdict{
		{QuestionID: "q1", Dimension: "d", Verdict: "yes"}, // zero weights
		{QuestionID: "q2", Dimension: "d", Verdict: "no"},
	}
	dims, overall := AggregateBinaryVerdicts(verdicts)
	if math.Abs(dims["d"]-0.5) > 1e-9 {
		t.Fatalf("dim d = %v, want 0.5 (equal-weight fallback)", dims["d"])
	}
	if math.Abs(overall-0.5) > 1e-9 {
		t.Fatalf("overall = %v, want 0.5", overall)
	}
}

func TestToEvaluatorScoreShape(t *testing.T) {
	res := BinaryEvaluationResult{
		DimensionScores: map[string]float64{"a": 0.5, "b": 1.0},
		Overall:         0.75,
	}
	score := res.ToEvaluatorScore("bugfix")
	if score.TaskKind != "bugfix" {
		t.Fatalf("task kind = %q", score.TaskKind)
	}
	if len(score.DimensionScores) != 2 || score.DimensionScores["a"] != 0.5 {
		t.Fatalf("dimension scores = %+v", score.DimensionScores)
	}
	if score.Soft != nil || score.Hard != nil {
		t.Fatal("expected Soft/Hard unset (contract additive, DimensionScores only)")
	}
}

// stubEvaluator lets RunBinaryEvaluation be tested independently of the rule runner.
type stubEvaluator struct{ answers map[string]BinaryAnswer }

func (s stubEvaluator) Answer(_ context.Context, _ string, q BinaryQuestion, _ string) (BinaryAnswer, error) {
	return s.answers[q.ID], nil
}

func TestRunBinaryEvaluationCoercesUnknownVerdict(t *testing.T) {
	set := BinaryQuestionSet{
		Version:    1,
		Dimensions: []BinaryDimension{{Name: "d", Questions: []BinaryQuestion{{ID: "q1", Text: "t"}, {ID: "q2", Text: "t"}}}},
	}
	ev := stubEvaluator{answers: map[string]BinaryAnswer{
		"q1": {Verdict: "YES"},
		"q2": {Verdict: "garbage"},
	}}
	res, err := RunBinaryEvaluation(context.Background(), set, "", ev)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdicts[0].Verdict != BinaryVerdictYes {
		t.Fatalf("q1 verdict = %q, want yes (case-insensitive)", res.Verdicts[0].Verdict)
	}
	if res.Verdicts[1].Verdict != BinaryVerdictNo {
		t.Fatalf("q2 verdict = %q, want no (fail-safe coercion)", res.Verdicts[1].Verdict)
	}
}
