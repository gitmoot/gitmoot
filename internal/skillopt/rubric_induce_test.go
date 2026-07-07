package skillopt

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// mustTraitJSON encodes a per-option trait map the way the store persists
// useful_traits_json / rejected_traits_json.
func mustTraitJSON(t *testing.T, traits map[string][]string) string {
	t.Helper()
	encoded, err := json.Marshal(traits)
	if err != nil {
		t.Fatalf("marshal traits: %v", err)
	}
	return string(encoded)
}

func mustListJSON(t *testing.T, list []string) string {
	t.Helper()
	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	return string(encoded)
}

// TestTokenizeDropsNoiseAndDedups pins the normalization the whole pipeline
// leans on: lowercase, split on non-alphanumerics, drop stopwords and <3-char
// tokens, dedup, and sort.
func TestTokenizeDropsNoiseAndDedups(t *testing.T) {
	got := tokenize("The clear, Clear value-prop is A win!!")
	want := []string{"clear", "prop", "value", "win"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokens = %v, want %v", got, want)
		}
	}
	if len(tokenize("a to of an")) != 0 {
		t.Fatalf("all-short/stopword text must tokenize to nothing, got %v", tokenize("a to of an"))
	}
}

// TestJaccardExact pins the similarity math with hand-computed fixtures.
func TestJaccardExact(t *testing.T) {
	cases := []struct {
		a, b []string
		want float64
	}{
		{[]string{"clarity", "layout"}, []string{"clarity", "layout"}, 1.0},
		{[]string{"clarity", "layout"}, []string{"clarity", "color"}, 1.0 / 3.0},
		{[]string{"clarity"}, []string{"color"}, 0.0},
		{[]string{}, []string{"color"}, 0.0},
		{[]string{"a"}, []string{}, 0.0},
	}
	for _, tc := range cases {
		if got := jaccard(tc.a, tc.b); math.Abs(got-tc.want) > 1e-12 {
			t.Fatalf("jaccard(%v,%v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestGroundAspectsOrderAndSigns pins grounding: useful traits are positive,
// rejected traits and required improvements are negative, option labels are
// visited in sorted order, and empty / untokenizable traits are dropped.
func TestGroundAspectsOrderAndSigns(t *testing.T) {
	events := []db.RankedFeedbackEventWithTemplate{
		{
			RankedFeedbackEvent: db.RankedFeedbackEvent{
				ID:                       "evt-1",
				UsefulTraitsJSON:         mustTraitJSON(t, map[string][]string{"b": {"clear headline"}, "a": {"strong value prop"}}),
				RejectedTraitsJSON:       mustTraitJSON(t, map[string][]string{"a": {"cluttered layout"}}),
				RequiredImprovementsJSON: mustListJSON(t, []string{"tighten the spacing", "  ", "of an"}),
			},
		},
	}
	aspects := groundAspects(events)
	// Order: useful (labels sorted a,b) then rejected then improvements;
	// "  " (empty) and "of an" (only stopwords) are dropped.
	wantTexts := []string{"strong value prop", "clear headline", "cluttered layout", "tighten the spacing"}
	wantSigns := []string{AspectSignPositive, AspectSignPositive, AspectSignNegative, AspectSignNegative}
	if len(aspects) != len(wantTexts) {
		t.Fatalf("got %d aspects %+v, want %d", len(aspects), aspects, len(wantTexts))
	}
	for i := range wantTexts {
		if aspects[i].Text != wantTexts[i] || aspects[i].Sign != wantSigns[i] {
			t.Fatalf("aspect %d = {%q,%q}, want {%q,%q}", i, aspects[i].Text, aspects[i].Sign, wantTexts[i], wantSigns[i])
		}
		if aspects[i].SourceEventID != "evt-1" {
			t.Fatalf("aspect %d source = %q, want evt-1", i, aspects[i].SourceEventID)
		}
	}
	if usableEventCount(aspects) != 1 {
		t.Fatalf("usable events = %d, want 1", usableEventCount(aspects))
	}
}

// TestSplitAspectsEvenStride pins the deterministic holdout stride: for f=0.2
// every 5th aspect is held out, distributed (not a contiguous tail).
func TestSplitAspectsEvenStride(t *testing.T) {
	aspects := make([]Aspect, 10)
	for i := range aspects {
		aspects[i] = Aspect{Text: string(rune('a' + i))}
	}
	train, holdout := splitAspects(aspects, 0.2)
	if len(holdout) != 2 || len(train) != 8 {
		t.Fatalf("train=%d holdout=%d, want 8/2", len(train), len(holdout))
	}
	// f=0.2: floor((i+1)*.2)>floor(i*.2) is true at i=4 and i=9.
	if holdout[0].Text != string(rune('a'+4)) || holdout[1].Text != string(rune('a'+9)) {
		t.Fatalf("holdout picks = %q,%q, want e,j", holdout[0].Text, holdout[1].Text)
	}
}

// TestClusterAspectsSeparatesThemes seeds three clearly-separated token themes
// and asserts greedy single-linkage clustering recovers exactly three metrics,
// each grouping its own theme, with a deterministic dominant-token name.
func TestClusterAspectsSeparatesThemes(t *testing.T) {
	texts := []struct {
		text string
		sign string
	}{
		{"clarity of the headline copy", AspectSignPositive},
		{"headline clarity is strong", AspectSignPositive},
		{"layout spacing feels cluttered", AspectSignNegative},
		{"cluttered layout spacing", AspectSignNegative},
		{"color contrast is accessible", AspectSignPositive},
		{"poor color contrast accessible", AspectSignNegative},
	}
	var aspects []Aspect
	for i, tc := range texts {
		aspects = append(aspects, Aspect{Text: tc.text, Sign: tc.sign, SourceEventID: "e", tokens: tokenize(tc.text)})
		_ = i
	}
	clusters := clusterAspects(aspects, 0.3, 6)
	if len(clusters) != 3 {
		t.Fatalf("clusters = %d, want 3: %+v", len(clusters), clusters)
	}
	for _, cl := range clusters {
		if len(cl.members) != 2 {
			t.Fatalf("each theme should hold 2 aspects, got %d", len(cl.members))
		}
	}
	// Redundancy between well-separated themes must be below the threshold.
	metrics := make([]RubricMetric, 0, len(clusters))
	for _, cl := range clusters {
		metrics = append(metrics, cl.toMetric())
	}
	if red := maxInterMetricSimilarity(metrics); red >= 0.3 {
		t.Fatalf("redundancy = %v, want < 0.3 for separated themes", red)
	}
	// Names derive from dominant tokens.
	if metrics[0].Name == "" || metrics[0].Definition == "" {
		t.Fatalf("metric 0 missing name/definition: %+v", metrics[0])
	}
}

// TestMergeDownRespectsCap seeds seven singleton themes and asserts the
// merge-down pass caps the metric count at maxMetrics.
func TestMergeDownRespectsCap(t *testing.T) {
	var aspects []Aspect
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"}
	for _, w := range words {
		aspects = append(aspects, Aspect{Text: w + " signal", Sign: AspectSignPositive, tokens: tokenize(w + " signal")})
	}
	clusters := clusterAspects(aspects, 0.9, 6)
	if len(clusters) > 6 {
		t.Fatalf("clusters = %d, want <= 6 after merge-down", len(clusters))
	}
}

// TestCoverageFractionExact pins the coverage math: two held-out aspects, one
// matching an induced metric above threshold and one not, is coverage 0.5.
func TestCoverageFractionExact(t *testing.T) {
	metric := RubricMetric{members: []Aspect{{tokens: tokenize("clarity headline copy")}}}
	holdout := []Aspect{
		{tokens: tokenize("clarity of the headline")}, // shares clarity+headline -> matches
		{tokens: tokenize("unrelated pricing tier")},  // no overlap -> misses
	}
	if got := coverageFraction(holdout, []RubricMetric{metric}, 0.3); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("coverage = %v, want 0.5", got)
	}
	if got := coverageFraction(nil, []RubricMetric{metric}, 0.3); got != 1.0 {
		t.Fatalf("empty coverage = %v, want 1.0 (vacuous)", got)
	}
}

// TestInduceRubricEndToEndDeterministic runs the full pipeline over a seeded
// three-theme fixture and pins the frozen rubric + report numbers.
func TestInduceRubricEndToEndDeterministic(t *testing.T) {
	events := rubricThemeFixture(t)
	rubric, report, err := InduceRubric("planner", events, RubricInduceOptions{HoldoutFraction: 0})
	if err != nil {
		t.Fatalf("InduceRubric returned error: %v", err)
	}
	if rubric.Version != RubricInductionVersion || rubric.Template != "planner" {
		t.Fatalf("rubric header = {v%d,%q}, want {v%d,planner}", rubric.Version, rubric.Template, RubricInductionVersion)
	}
	if len(rubric.Metrics) != 3 {
		t.Fatalf("metrics = %d, want 3: %+v", len(rubric.Metrics), rubric.Metrics)
	}
	if report.MetricCount != 3 || report.UsableEvents != 6 {
		t.Fatalf("report metrics=%d usableEvents=%d, want 3/6", report.MetricCount, report.UsableEvents)
	}
	// holdout=0 -> in-sample coverage, which must be perfect for the
	// self-consistent themes.
	if !report.InSampleCoverage || report.Coverage != 1.0 {
		t.Fatalf("coverage = %v (inSample=%v), want 1.0 in-sample", report.Coverage, report.InSampleCoverage)
	}
	if report.Redundancy >= report.MatchThreshold {
		t.Fatalf("redundancy = %v, want < threshold %v", report.Redundancy, report.MatchThreshold)
	}
	// Every emitted metric carries provenance.
	for i, metric := range rubric.Metrics {
		if len(metric.SourceEventIDs) == 0 {
			t.Fatalf("metric %d has no source event ids", i)
		}
		if len(metric.PositiveExamples)+len(metric.NegativeExamples) == 0 {
			t.Fatalf("metric %d has no examples", i)
		}
	}
	// Determinism: a second run yields byte-identical frozen JSON.
	rubric2, _, err := InduceRubric("planner", events, RubricInduceOptions{HoldoutFraction: 0})
	if err != nil {
		t.Fatalf("second InduceRubric returned error: %v", err)
	}
	a, _ := json.Marshal(rubric)
	b, _ := json.Marshal(rubric2)
	if string(a) != string(b) {
		t.Fatalf("frozen rubric not deterministic:\n%s\n%s", a, b)
	}
}

// TestInduceRubricErrors covers the two actionable error paths.
func TestInduceRubricErrors(t *testing.T) {
	// Too few usable events (< 3).
	few := []db.RankedFeedbackEventWithTemplate{
		{RankedFeedbackEvent: db.RankedFeedbackEvent{ID: "e1", UsefulTraitsJSON: mustTraitJSON(t, map[string][]string{"a": {"clear headline"}})}},
		{RankedFeedbackEvent: db.RankedFeedbackEvent{ID: "e2", UsefulTraitsJSON: mustTraitJSON(t, map[string][]string{"a": {"nice layout"}})}},
	}
	if _, _, err := InduceRubric("planner", few, RubricInduceOptions{}); err == nil {
		t.Fatal("expected error for < 3 usable events")
	}
	// Enough events but homogeneous -> single cluster -> < 2 metrics error.
	var homo []db.RankedFeedbackEventWithTemplate
	for i := 0; i < 4; i++ {
		homo = append(homo, db.RankedFeedbackEventWithTemplate{RankedFeedbackEvent: db.RankedFeedbackEvent{
			ID:               "h" + string(rune('0'+i)),
			UsefulTraitsJSON: mustTraitJSON(t, map[string][]string{"a": {"clarity headline clarity headline"}}),
		}})
	}
	if _, _, err := InduceRubric("planner", homo, RubricInduceOptions{HoldoutFraction: 0}); err == nil {
		t.Fatal("expected error for < 2 metric clusters")
	}
}

// rubricThemeFixture builds six events across three separable themes (clarity,
// layout, color), one theme-pair per event so usableEventCount is 6.
func rubricThemeFixture(t *testing.T) []db.RankedFeedbackEventWithTemplate {
	t.Helper()
	seeds := []struct {
		id      string
		useful  []string
		rejects []string
	}{
		{"evt-1", []string{"clarity of the headline copy"}, nil},
		{"evt-2", []string{"headline clarity reads strong"}, nil},
		{"evt-3", nil, []string{"layout spacing feels cluttered"}},
		{"evt-4", nil, []string{"cluttered layout spacing"}},
		{"evt-5", []string{"color contrast is accessible"}, nil},
		{"evt-6", nil, []string{"poor color contrast accessible"}},
	}
	var events []db.RankedFeedbackEventWithTemplate
	for _, seed := range seeds {
		event := db.RankedFeedbackEvent{ID: seed.id}
		if len(seed.useful) > 0 {
			event.UsefulTraitsJSON = mustTraitJSON(t, map[string][]string{"a": seed.useful})
		}
		if len(seed.rejects) > 0 {
			event.RejectedTraitsJSON = mustTraitJSON(t, map[string][]string{"a": seed.rejects})
		}
		events = append(events, db.RankedFeedbackEventWithTemplate{RankedFeedbackEvent: event, TemplateID: "planner"})
	}
	return events
}
