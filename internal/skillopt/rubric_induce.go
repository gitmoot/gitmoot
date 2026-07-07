package skillopt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

// Offline rubric induction (#347, AutoLibra-style — 2505.02820).
//
// The judge scores against a rubric; if that rubric omits a dimension real
// reviewers care about, the judge structurally cannot catch it. This module
// INDUCES a criterion-separated rubric from the human feedback already captured
// in the store (RankedFeedbackEvent.UsefulTraits / RejectedTraits /
// RequiredImprovements) and MEASURES it for coverage/redundancy on a held-out
// split, then emits it as frozen, human-reviewable static JSON.
//
// It is deliberately OFFLINE and DETERMINISTIC (no LLM calls) so it stays
// testable and reproducible: grounding is string extraction, clustering is
// greedy single-linkage token-overlap (Jaccard). The LLM thematic-clustering
// upgrade AutoLibra describes is a clearly-marked extension point
// (see clusterAspects) — it would replace step (2) only, leaving grounding,
// meta-eval, and the emitted contract unchanged.
//
// Nothing here auto-injects anywhere. The tool only produces files; a human
// reviews the frozen rubric and (per the docs recipe) maps its metrics onto
// gitmoot-skillopt's evaluator_config['rubric'] with zero judge-code change.

// RubricInductionVersion is the schema version stamped into every emitted
// frozen rubric so a downstream reader can reject a shape it does not know.
const RubricInductionVersion = 1

// Aspect is one grounded behavior signal extracted from a single feedback
// trait string: the text, its sign (positive/negative), and the id of the
// ranked feedback event it came from (for provenance / audit).
type Aspect struct {
	Text          string `json:"text"`
	Sign          string `json:"sign"` // AspectSignPositive or AspectSignNegative
	SourceEventID string `json:"source_event_id"`

	// tokens is the normalized token set used for clustering; unexported so it
	// never leaks into the emitted JSON.
	tokens []string
}

const (
	// AspectSignPositive marks a trait the reviewer valued (useful_traits).
	AspectSignPositive = "+"
	// AspectSignNegative marks a trait the reviewer rejected or asked to fix
	// (rejected_traits, required_improvements).
	AspectSignNegative = "-"
)

// RubricMetric is one induced, criterion-separated metric: a named cluster of
// aspects with a definition, positive/negative examples, and the source event
// ids that grounded it. This is the shape that maps onto the judge's
// evaluator_config['rubric'] dimensions.
type RubricMetric struct {
	Name             string   `json:"name"`
	Definition       string   `json:"definition"`
	PositiveExamples []string `json:"positive_examples"`
	NegativeExamples []string `json:"negative_examples"`
	SourceEventIDs   []string `json:"source_event_ids"`

	// members are the aspects assigned to this metric; unexported, kept for the
	// meta-eval (coverage/redundancy) and never emitted.
	members []Aspect
}

// InducedRubric is the frozen artifact written to rubric.json. It carries the
// schema version, the template it was induced for, and the metrics.
type InducedRubric struct {
	Version  int            `json:"version"`
	Template string         `json:"template"`
	Metrics  []RubricMetric `json:"metrics"`
}

// RubricReport is the coverage/redundancy meta-evaluation written to
// report.json (and rendered to report.txt). All counts are exact so the
// numbers are reproducible and testable.
type RubricReport struct {
	Template        string  `json:"template"`
	MetricCount     int     `json:"metric_count"`
	UsableEvents    int     `json:"usable_events"`
	TotalAspects    int     `json:"total_aspects"`
	TrainAspects    int     `json:"train_aspects"`
	HoldoutAspects  int     `json:"holdout_aspects"`
	HoldoutFraction float64 `json:"holdout_fraction"`
	MatchThreshold  float64 `json:"match_threshold"`
	// Coverage is the fraction of held-out aspects that match some induced
	// metric at or above MatchThreshold. When there is no held-out split
	// (HoldoutFraction resolves to zero aspects) it is measured in-sample and
	// InSampleCoverage is set true so the number is not read as generalization.
	Coverage         float64 `json:"coverage"`
	InSampleCoverage bool    `json:"in_sample_coverage"`
	// Redundancy is the maximum inter-metric similarity (single-linkage
	// Jaccard between any two metrics' member aspects). Lower is better; by
	// construction it is below the clustering threshold unless a merge-down
	// pass (too many raw clusters) combined near-duplicates.
	Redundancy float64 `json:"redundancy"`
}

// RubricInduceOptions are the tunable knobs for one induction run. Zero values
// resolve to the documented defaults via normalize().
type RubricInduceOptions struct {
	// HoldoutFraction is the fraction of aspects reserved to measure coverage.
	// A NEGATIVE value means "unset" and resolves to the default 0.2; an
	// explicit 0 keeps in-sample coverage (no split); values above 0.9 clamp.
	HoldoutFraction float64
	// MinEvents is the minimum number of usable (aspect-producing) events
	// required; below it the run errors (hard floor 3, AutoLibra needs data).
	MinEvents int
	// MinMetrics is the minimum number of induced metrics required; below it
	// the run errors (default/hard floor 2 — a single cluster is not a rubric).
	MinMetrics int
	// MaxMetrics caps the metric count; excess raw clusters are merged down by
	// similarity (default 6, AutoLibra's 3–6 target upper bound).
	MaxMetrics int
	// Threshold is the Jaccard token-overlap threshold for both clustering
	// (single-linkage assignment) and coverage matching (default 0.3).
	Threshold float64
}

const (
	rubricDefaultHoldout   = 0.2
	rubricMinEventsFloor   = 3
	rubricMinMetricsFloor  = 2
	rubricDefaultMaxMetric = 6
	rubricDefaultThreshold = 0.3
)

func (o RubricInduceOptions) normalize() RubricInduceOptions {
	out := o
	if out.HoldoutFraction < 0 {
		out.HoldoutFraction = rubricDefaultHoldout
	}
	if out.HoldoutFraction > 0.9 {
		out.HoldoutFraction = 0.9
	}
	if out.MinEvents < rubricMinEventsFloor {
		out.MinEvents = rubricMinEventsFloor
	}
	if out.MinMetrics < rubricMinMetricsFloor {
		out.MinMetrics = rubricMinMetricsFloor
	}
	if out.MaxMetrics <= 0 {
		out.MaxMetrics = rubricDefaultMaxMetric
	}
	if out.MaxMetrics < out.MinMetrics {
		out.MaxMetrics = out.MinMetrics
	}
	if out.Threshold <= 0 || out.Threshold > 1 {
		out.Threshold = rubricDefaultThreshold
	}
	return out
}

// InduceRubric runs the full deterministic pipeline over the ranked feedback
// events of one template: ground -> split -> cluster (train) -> meta-eval
// (coverage on holdout, redundancy) -> freeze. The returned InducedRubric is
// the train-induced metric set (the holdout is spent validating it, the
// standard train/validation contract); the RubricReport carries the exact
// coverage/redundancy numbers. It errors on too little data (< MinEvents usable
// events) or a degenerate rubric (< MinMetrics clusters) with an actionable
// message.
func InduceRubric(template string, events []db.RankedFeedbackEventWithTemplate, opts RubricInduceOptions) (InducedRubric, RubricReport, error) {
	template = strings.TrimSpace(template)
	opts = opts.normalize()

	aspects := groundAspects(events)
	usable := usableEventCount(aspects)
	if usable < opts.MinEvents {
		return InducedRubric{}, RubricReport{}, fmt.Errorf("rubric induction needs at least %d usable feedback events (events with useful/rejected traits or required improvements), found %d for template %q: capture more human feedback before inducing", opts.MinEvents, usable, template)
	}

	train, holdout := splitAspects(aspects, opts.HoldoutFraction)
	// The training split must still carry enough signal to cluster; if the
	// holdout swallowed everything (pathologically small N) fall back to all
	// aspects for training so the run is still meaningful.
	if len(train) == 0 {
		train = aspects
		holdout = nil
	}

	clusters := clusterAspects(train, opts.Threshold, opts.MaxMetrics)
	if len(clusters) < opts.MinMetrics {
		return InducedRubric{}, RubricReport{}, fmt.Errorf("rubric induction produced only %d metric cluster(s) for template %q, need at least %d: feedback is too homogeneous or too sparse to separate criteria", len(clusters), template, opts.MinMetrics)
	}

	metrics := make([]RubricMetric, 0, len(clusters))
	for _, cl := range clusters {
		metrics = append(metrics, cl.toMetric())
	}

	report := RubricReport{
		Template:        template,
		MetricCount:     len(metrics),
		UsableEvents:    usable,
		TotalAspects:    len(aspects),
		TrainAspects:    len(train),
		HoldoutAspects:  len(holdout),
		HoldoutFraction: opts.HoldoutFraction,
		MatchThreshold:  opts.Threshold,
		Redundancy:      maxInterMetricSimilarity(metrics),
	}
	if len(holdout) > 0 {
		report.Coverage = coverageFraction(holdout, metrics, opts.Threshold)
	} else {
		report.Coverage = coverageFraction(train, metrics, opts.Threshold)
		report.InSampleCoverage = true
	}

	rubric := InducedRubric{
		Version:  RubricInductionVersion,
		Template: template,
		Metrics:  metrics,
	}
	return rubric, report, nil
}

// groundAspects extracts one Aspect per feedback trait string, in a
// deterministic order: events arrive store-ordered; within each event we emit
// useful traits (positive) then rejected traits (negative) then required
// improvements (negative). Empty/whitespace traits and aspects that tokenize to
// nothing are dropped (they carry no clustering signal).
func groundAspects(events []db.RankedFeedbackEventWithTemplate) []Aspect {
	var aspects []Aspect
	add := func(text, sign, eventID string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		tokens := tokenize(text)
		if len(tokens) == 0 {
			return
		}
		aspects = append(aspects, Aspect{Text: text, Sign: sign, SourceEventID: eventID, tokens: tokens})
	}
	for _, event := range events {
		for _, text := range flattenTraitMap(event.UsefulTraitsJSON) {
			add(text, AspectSignPositive, event.ID)
		}
		for _, text := range flattenTraitMap(event.RejectedTraitsJSON) {
			add(text, AspectSignNegative, event.ID)
		}
		for _, text := range flattenStringList(event.RequiredImprovementsJSON) {
			add(text, AspectSignNegative, event.ID)
		}
	}
	return aspects
}

// flattenTraitMap decodes a useful/rejected traits JSON blob
// (map[optionLabel][]trait) into a flat, deterministically ordered slice of
// trait strings: option labels sorted, traits in their stored order.
func flattenTraitMap(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var traits map[string][]string
	if err := json.Unmarshal([]byte(value), &traits); err != nil {
		return nil
	}
	labels := make([]string, 0, len(traits))
	for label := range traits {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	var out []string
	for _, label := range labels {
		out = append(out, traits[label]...)
	}
	return out
}

// flattenStringList decodes a required_improvements JSON blob (a flat []string)
// preserving its stored order.
func flattenStringList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(value), &list); err != nil {
		return nil
	}
	return list
}

// usableEventCount counts the distinct source events that produced at least one
// aspect (an event with no gradeable traits is not "usable" for induction).
func usableEventCount(aspects []Aspect) int {
	seen := map[string]struct{}{}
	for _, aspect := range aspects {
		seen[aspect.SourceEventID] = struct{}{}
	}
	return len(seen)
}

// splitAspects partitions aspects into a training set and a held-out set using
// an even deterministic stride (never a contiguous tail, which would bias the
// holdout toward the latest feedback). Aspect i is held out iff
// floor((i+1)*f) > floor(i*f).
func splitAspects(aspects []Aspect, fraction float64) (train, holdout []Aspect) {
	if fraction <= 0 {
		return append([]Aspect(nil), aspects...), nil
	}
	prev := 0
	for i, aspect := range aspects {
		cur := int(float64(i+1) * fraction)
		if cur > prev {
			holdout = append(holdout, aspect)
		} else {
			train = append(train, aspect)
		}
		prev = cur
	}
	return train, holdout
}

// cluster is the internal accumulator for one metric before it is frozen.
type cluster struct {
	members []Aspect
}

// clusterAspects greedily groups aspects by single-linkage Jaccard token
// overlap with stable ordering: each aspect joins the best-matching existing
// cluster (highest single-linkage member similarity, ties broken to the
// earliest cluster) when that similarity meets the threshold, else it seeds a
// new cluster. If more than maxMetrics clusters result, the most-similar pairs
// are merged down until the cap holds. Order-stable and fully deterministic.
//
// EXTENSION POINT (#347): this single-pass token-overlap clusterer is the
// deterministic v1. AutoLibra's thematic clustering would swap ONLY this
// function for an LLM pass that groups aspects by meaning; grounding, the
// held-out meta-eval, and the emitted contract stay identical, so the LLM
// variant remains offline-reviewable and drop-in.
func clusterAspects(aspects []Aspect, threshold float64, maxMetrics int) []cluster {
	var clusters []cluster
	for _, aspect := range aspects {
		bestIdx := -1
		bestSim := threshold
		for i := range clusters {
			sim := clusterSingleLinkage(clusters[i].members, aspect)
			if sim >= bestSim && (bestIdx == -1 || sim > bestSim) {
				bestSim = sim
				bestIdx = i
			}
		}
		if bestIdx == -1 {
			clusters = append(clusters, cluster{members: []Aspect{aspect}})
		} else {
			clusters[bestIdx].members = append(clusters[bestIdx].members, aspect)
		}
	}
	return mergeDownClusters(clusters, maxMetrics)
}

// clusterSingleLinkage returns the maximum Jaccard between an aspect and any
// member of a cluster (single-linkage similarity).
func clusterSingleLinkage(members []Aspect, aspect Aspect) float64 {
	best := 0.0
	for _, member := range members {
		if sim := jaccard(member.tokens, aspect.tokens); sim > best {
			best = sim
		}
	}
	return best
}

// mergeDownClusters merges the most-similar cluster pair repeatedly until at
// most maxMetrics remain. The pair is chosen by highest cross single-linkage
// similarity, tie-broken by lowest (i, j) index for determinism.
func mergeDownClusters(clusters []cluster, maxMetrics int) []cluster {
	for len(clusters) > maxMetrics {
		bestI, bestJ := 0, 1
		bestSim := -1.0
		for i := 0; i < len(clusters); i++ {
			for j := i + 1; j < len(clusters); j++ {
				sim := clustersSingleLinkage(clusters[i].members, clusters[j].members)
				if sim > bestSim {
					bestSim = sim
					bestI, bestJ = i, j
				}
			}
		}
		clusters[bestI].members = append(clusters[bestI].members, clusters[bestJ].members...)
		clusters = append(clusters[:bestJ], clusters[bestJ+1:]...)
	}
	return clusters
}

// clustersSingleLinkage returns the maximum Jaccard between any member of a and
// any member of b.
func clustersSingleLinkage(a, b []Aspect) float64 {
	best := 0.0
	for _, x := range a {
		for _, y := range b {
			if sim := jaccard(x.tokens, y.tokens); sim > best {
				best = sim
			}
		}
	}
	return best
}

// toMetric freezes a cluster into the emitted RubricMetric: a name derived from
// its dominant tokens, a deterministic definition, deduplicated positive /
// negative example texts (in member order), and sorted unique source event ids.
func (c cluster) toMetric() RubricMetric {
	posCount, negCount := 0, 0
	positives := newOrderedStringSet()
	negatives := newOrderedStringSet()
	sources := map[string]struct{}{}
	for _, member := range c.members {
		if member.Sign == AspectSignNegative {
			negCount++
			negatives.add(member.Text)
		} else {
			posCount++
			positives.add(member.Text)
		}
		if member.SourceEventID != "" {
			sources[member.SourceEventID] = struct{}{}
		}
	}
	sourceIDs := make([]string, 0, len(sources))
	for id := range sources {
		sourceIDs = append(sourceIDs, id)
	}
	sort.Strings(sourceIDs)
	dominant := dominantTokens(c.members, 3)
	return RubricMetric{
		Name:             metricName(dominant),
		Definition:       metricDefinition(dominant, posCount, negCount, len(sourceIDs)),
		PositiveExamples: positives.values(),
		NegativeExamples: negatives.values(),
		SourceEventIDs:   sourceIDs,
		members:          c.members,
	}
}

// dominantTokens returns up to n tokens ranked by document frequency across the
// cluster's members (each member contributes each of its unique tokens once),
// tie-broken alphabetically for stability.
func dominantTokens(members []Aspect, n int) []string {
	freq := map[string]int{}
	for _, member := range members {
		for _, token := range member.tokens {
			freq[token]++
		}
	}
	tokens := make([]string, 0, len(freq))
	for token := range freq {
		tokens = append(tokens, token)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if freq[tokens[i]] != freq[tokens[j]] {
			return freq[tokens[i]] > freq[tokens[j]]
		}
		return tokens[i] < tokens[j]
	})
	if len(tokens) > n {
		tokens = tokens[:n]
	}
	return tokens
}

func metricName(dominant []string) string {
	if len(dominant) == 0 {
		return "uncategorized"
	}
	return strings.Join(dominant, " / ")
}

func metricDefinition(dominant []string, positives, negatives, sources int) string {
	topic := "reviewer feedback"
	if len(dominant) > 0 {
		topic = strings.Join(dominant, ", ")
	}
	return fmt.Sprintf("Covers feedback about %s (%d positive / %d negative signal(s) across %d source event(s)).", topic, positives, negatives, sources)
}

// coverageFraction is the fraction of the given aspects that match some metric
// at or above the threshold (single-linkage Jaccard to any of the metric's
// member aspects). With no aspects it is 1.0 (vacuously covered).
func coverageFraction(aspects []Aspect, metrics []RubricMetric, threshold float64) float64 {
	if len(aspects) == 0 {
		return 1.0
	}
	matched := 0
	for _, aspect := range aspects {
		if aspectMatchesAnyMetric(aspect, metrics, threshold) {
			matched++
		}
	}
	return float64(matched) / float64(len(aspects))
}

func aspectMatchesAnyMetric(aspect Aspect, metrics []RubricMetric, threshold float64) bool {
	for _, metric := range metrics {
		if clusterSingleLinkage(metric.members, aspect) >= threshold {
			return true
		}
	}
	return false
}

// maxInterMetricSimilarity returns the largest single-linkage Jaccard between
// any two distinct metrics' member aspects (0 for fewer than two metrics).
func maxInterMetricSimilarity(metrics []RubricMetric) float64 {
	best := 0.0
	for i := 0; i < len(metrics); i++ {
		for j := i + 1; j < len(metrics); j++ {
			if sim := clustersSingleLinkage(metrics[i].members, metrics[j].members); sim > best {
				best = sim
			}
		}
	}
	return best
}

// jaccard is |A∩B| / |A∪B| over two token sets (each already unique+sorted).
// Two empty sets have similarity 0.
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(a))
	for _, token := range a {
		set[token] = struct{}{}
	}
	intersection := 0
	for _, token := range b {
		if _, ok := set[token]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// rubricStopwords are common words dropped before clustering so overlap
// reflects content, not grammar.
var rubricStopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "that": {}, "this": {},
	"has": {}, "have": {}, "was": {}, "are": {}, "but": {}, "not": {},
	"you": {}, "your": {}, "its": {}, "too": {}, "very": {}, "more": {},
	"less": {}, "than": {}, "then": {}, "into": {}, "from": {}, "them": {},
	"they": {}, "some": {}, "such": {}, "all": {}, "any": {}, "can": {},
	"could": {}, "would": {}, "should": {}, "will": {}, "does": {},
	"did": {}, "why": {}, "how": {}, "what": {}, "which": {}, "when": {},
	"where": {}, "who": {}, "there": {}, "here": {}, "over": {}, "under": {},
	"about": {}, "also": {}, "just": {}, "only": {}, "much": {}, "many": {},
}

// tokenize lowercases, splits on any non-alphanumeric rune, drops stopwords and
// tokens shorter than three characters, and returns a unique, sorted token set.
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	seen := map[string]struct{}{}
	var tokens []string
	for _, field := range fields {
		if len(field) < 3 {
			continue
		}
		if _, stop := rubricStopwords[field]; stop {
			continue
		}
		if _, dup := seen[field]; dup {
			continue
		}
		seen[field] = struct{}{}
		tokens = append(tokens, field)
	}
	sort.Strings(tokens)
	return tokens
}

// orderedStringSet preserves first-insertion order while deduplicating.
type orderedStringSet struct {
	seen  map[string]struct{}
	order []string
}

func newOrderedStringSet() *orderedStringSet {
	return &orderedStringSet{seen: map[string]struct{}{}}
}

func (s *orderedStringSet) add(value string) {
	if _, ok := s.seen[value]; ok {
		return
	}
	s.seen[value] = struct{}{}
	s.order = append(s.order, value)
}

func (s *orderedStringSet) values() []string {
	return append([]string(nil), s.order...)
}

// RenderRubricReport renders the meta-eval report as human-readable text (the
// report.txt artifact and the default CLI summary).
func RenderRubricReport(report RubricReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rubric induction report (#347 offline, deterministic)\n")
	fmt.Fprintf(&b, "  template:          %s\n", report.Template)
	fmt.Fprintf(&b, "  metrics induced:   %d\n", report.MetricCount)
	fmt.Fprintf(&b, "  usable events:     %d\n", report.UsableEvents)
	fmt.Fprintf(&b, "  aspects (total):   %d (train %d, holdout %d)\n", report.TotalAspects, report.TrainAspects, report.HoldoutAspects)
	fmt.Fprintf(&b, "  holdout fraction:  %.2f\n", report.HoldoutFraction)
	fmt.Fprintf(&b, "  match threshold:   %.2f (Jaccard)\n", report.MatchThreshold)
	if report.InSampleCoverage {
		fmt.Fprintf(&b, "  coverage:          %.3f (in-sample — no held-out split)\n", report.Coverage)
	} else {
		fmt.Fprintf(&b, "  coverage:          %.3f (held-out aspects matched)\n", report.Coverage)
	}
	fmt.Fprintf(&b, "  redundancy:        %.3f (max inter-metric similarity; lower is better)\n", report.Redundancy)
	return b.String()
}
