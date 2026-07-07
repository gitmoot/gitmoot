package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// seedRubricFeedback creates one eval run for templateID and, for each seed,
// registers a two-option review item and upserts a ranked feedback event
// carrying the given useful (positive) / rejected (negative) traits on option
// a. This is the real store-API seeding path the E2E exercises.
func seedRubricFeedback(t *testing.T, home, templateID string, seeds []struct {
	item    string
	useful  []string
	rejects []string
}) {
	t.Helper()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	const runID = "rubric-run-1"
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:           runID,
		TemplateID:   templateID,
		TargetRepo:   "owner/repo",
		State:        "review",
		Mode:         db.EvalRunModeValidate,
		OptionsCount: 2,
	}); err != nil {
		t.Fatalf("UpsertEvalRun: %v", err)
	}
	for _, seed := range seeds {
		if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{RunID: runID, ItemID: seed.item, Title: seed.item}); err != nil {
			t.Fatalf("UpsertEvalReviewItem %s: %v", seed.item, err)
		}
		for _, label := range []string{"a", "b"} {
			if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{
				RunID: runID, ItemID: seed.item, Label: label, Role: "option",
				ArtifactID: "art-" + seed.item + "-" + label,
			}); err != nil {
				t.Fatalf("UpsertEvalReviewOption %s/%s: %v", seed.item, label, err)
			}
		}
		event := db.RankedFeedbackEvent{
			ID:          "evt-" + seed.item,
			RunID:       runID,
			ItemID:      seed.item,
			RankingJSON: `["a","b"]`,
			Winner:      "a",
			Reviewer:    "jerry",
			Source:      "github",
			SourceURL:   "https://example.test/" + seed.item,
			CreatedAt:   "2026-07-01T10:00:00Z",
		}
		if len(seed.useful) > 0 {
			encoded, _ := json.Marshal(map[string][]string{"a": seed.useful})
			event.UsefulTraitsJSON = string(encoded)
		}
		if len(seed.rejects) > 0 {
			encoded, _ := json.Marshal(map[string][]string{"a": seed.rejects})
			event.RejectedTraitsJSON = string(encoded)
		}
		if err := store.UpsertRankedFeedbackEvent(ctx, event); err != nil {
			t.Fatalf("UpsertRankedFeedbackEvent %s: %v", seed.item, err)
		}
	}
}

// TestSkillOptRubricInduceE2E drives the real CLI over a temp home seeded with
// three separable feedback themes and asserts the emitted frozen rubric JSON
// structure + the report numbers, end to end, with no LLM.
func TestSkillOptRubricInduceE2E(t *testing.T) {
	home := t.TempDir()
	seedRubricFeedback(t, home, "planner", []struct {
		item    string
		useful  []string
		rejects []string
	}{
		{"i1", []string{"clarity of the headline copy"}, nil},
		{"i2", []string{"headline clarity reads strong"}, nil},
		{"i3", nil, []string{"layout spacing feels cluttered"}},
		{"i4", nil, []string{"cluttered layout spacing"}},
		{"i5", []string{"color contrast is accessible"}, nil},
		{"i6", nil, []string{"poor color contrast accessible"}},
	})

	outDir := home + "/rubric-out"
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "rubric", "induce", "--home", home, "--template", "planner",
		"--out", outDir, "--holdout", "0", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("rubric induce exit = %d, stderr: %s", code, stderr.String())
	}

	var result skillOptRubricInduceResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v\n%s", err, stdout.String())
	}
	if result.Report.Template != "planner" || result.Report.UsableEvents != 6 {
		t.Fatalf("report template=%q usableEvents=%d, want planner/6", result.Report.Template, result.Report.UsableEvents)
	}
	if result.Report.MetricCount != 3 {
		t.Fatalf("metric count = %d, want 3\n%s", result.Report.MetricCount, stdout.String())
	}
	if !result.Report.InSampleCoverage || result.Report.Coverage != 1.0 {
		t.Fatalf("coverage = %v (inSample=%v), want in-sample 1.0", result.Report.Coverage, result.Report.InSampleCoverage)
	}
	if result.Report.Redundancy >= result.Report.MatchThreshold {
		t.Fatalf("redundancy %v must be below threshold %v", result.Report.Redundancy, result.Report.MatchThreshold)
	}

	// The frozen rubric JSON on disk must parse into the induced-rubric shape
	// with provenance and version stamp.
	raw, err := os.ReadFile(result.RubricPath)
	if err != nil {
		t.Fatalf("read rubric.json: %v", err)
	}
	var rubric skillopt.InducedRubric
	if err := json.Unmarshal(raw, &rubric); err != nil {
		t.Fatalf("decode rubric.json: %v\n%s", err, raw)
	}
	if rubric.Version != skillopt.RubricInductionVersion || rubric.Template != "planner" || len(rubric.Metrics) != 3 {
		t.Fatalf("rubric = v%d template=%q metrics=%d, want v%d/planner/3", rubric.Version, rubric.Template, len(rubric.Metrics), skillopt.RubricInductionVersion)
	}
	sawSource := false
	for _, metric := range rubric.Metrics {
		if metric.Name == "" || metric.Definition == "" {
			t.Fatalf("metric missing name/definition: %+v", metric)
		}
		for _, id := range metric.SourceEventIDs {
			if strings.HasPrefix(id, "evt-i") {
				sawSource = true
			}
		}
	}
	if !sawSource {
		t.Fatalf("expected provenance source_event_ids like evt-i1, got %+v", rubric.Metrics)
	}

	// The human-readable report.txt exists and carries the headline numbers.
	text, err := os.ReadFile(result.TextPath)
	if err != nil {
		t.Fatalf("read report.txt: %v", err)
	}
	for _, want := range []string{"rubric induction report", "metrics induced:   3", "usable events:     6"} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("report.txt missing %q\n%s", want, text)
		}
	}
}

// TestSkillOptRubricInduceTooFewEventsErrors proves the actionable error path
// (and non-zero exit) when there is not enough feedback to induce a rubric.
func TestSkillOptRubricInduceTooFewEventsErrors(t *testing.T) {
	home := t.TempDir()
	seedRubricFeedback(t, home, "planner", []struct {
		item    string
		useful  []string
		rejects []string
	}{
		{"i1", []string{"clear headline"}, nil},
		{"i2", []string{"nice layout"}, nil},
	})
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "rubric", "induce", "--home", home, "--template", "planner"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for too few events, stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "usable feedback events") {
		t.Fatalf("stderr missing actionable message: %s", stderr.String())
	}
}

// TestSkillOptRubricInduceRequiresTemplate proves --template is mandatory.
func TestSkillOptRubricInduceRequiresTemplate(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "rubric", "induce", "--home", home}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 for missing --template", code)
	}
	if !strings.Contains(stderr.String(), "--template is required") {
		t.Fatalf("stderr missing required-flag message: %s", stderr.String())
	}
}

// TestSkillOptRubricInduceHoldoutSplit exercises the held-out coverage path
// (default 0.2 holdout) and asserts the split is reported and coverage is not
// flagged in-sample.
func TestSkillOptRubricInduceHoldoutSplit(t *testing.T) {
	home := t.TempDir()
	var seeds []struct {
		item    string
		useful  []string
		rejects []string
	}
	// 12 events across the same three themes so a 0.2 holdout is non-empty.
	themes := [][]string{
		{"clarity of the headline copy", "headline clarity reads strong"},
		{"layout spacing feels cluttered", "cluttered layout spacing here"},
		{"color contrast is accessible", "strong color contrast accessible"},
	}
	for i := 0; i < 12; i++ {
		theme := themes[i%3]
		text := theme[i%2]
		seeds = append(seeds, struct {
			item    string
			useful  []string
			rejects []string
		}{item: fmt.Sprintf("i%02d", i), useful: []string{text}})
	}
	seedRubricFeedback(t, home, "planner", seeds)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "rubric", "induce", "--home", home, "--template", "planner", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("rubric induce exit = %d, stderr: %s", code, stderr.String())
	}
	var result skillOptRubricInduceResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v\n%s", err, stdout.String())
	}
	if result.Report.InSampleCoverage {
		t.Fatalf("expected a held-out split, got in-sample coverage: %+v", result.Report)
	}
	if result.Report.HoldoutAspects == 0 || result.Report.TrainAspects == 0 {
		t.Fatalf("expected non-empty train/holdout split, got %+v", result.Report)
	}
	if result.Report.HoldoutFraction != 0.2 {
		t.Fatalf("holdout fraction = %v, want default 0.2", result.Report.HoldoutFraction)
	}
}
