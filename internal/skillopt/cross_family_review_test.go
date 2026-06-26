package skillopt

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// reviewOutcome builds a cross-family OutcomeReviewed for the test PR with the
// given rubric + reviewer + self-family flag.
func reviewOutcome(rubric map[string]float64, reviewer string, selfFamily bool) workflow.Outcome {
	return workflow.Outcome{
		Kind:        workflow.OutcomeReviewed,
		Repo:        "owner/repo",
		PullRequest: 7,
		HeadSHA:     "deadbeef",
		Reviewer:    reviewer,
		SelfFamily:  selfFamily,
		Rubric:      rubric,
		Findings:    "scope drift on PR #7",
	}
}

func reviewItemFor(t *testing.T, store *db.Store, versionID, itemID string) (db.EvalReviewItem, bool) {
	t.Helper()
	items, err := store.ListEvalReviewItems(context.Background(), autoTraceRunIDPrefix+versionID)
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	for _, item := range items {
		if item.ItemID == itemID {
			return item, true
		}
	}
	return db.EvalReviewItem{}, false
}

// TestProjectReviewMeanOfDimensionScores: the rubric projects to the arithmetic
// MEAN of its DimensionScores (the #462 mean path), HasScore=true.
func TestProjectReviewMeanOfDimensionScores(t *testing.T) {
	signal := projectReview(reviewOutcome(map[string]float64{
		"coverage": 0.6, "fidelity": 0.8, "architecture": 1.0,
	}, "claude", false))
	if !signal.HasScore {
		t.Fatal("a non-empty rubric must yield HasScore=true")
	}
	want := (0.6 + 0.8 + 1.0) / 3.0
	if diff := signal.Score - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("review score = %v, want the mean %v", signal.Score, want)
	}
}

// TestProjectReviewEmptyRubricNoScore: an EMPTY rubric yields HasScore=false (no
// fabricated neutral 0.5).
func TestProjectReviewEmptyRubricNoScore(t *testing.T) {
	signal := projectReview(reviewOutcome(map[string]float64{}, "claude", false))
	if signal.HasScore {
		t.Fatalf("empty rubric must yield HasScore=false, got score=%v", signal.Score)
	}
}

// TestReviewRowCoexistsWithFloor: harvesting a merge floor THEN a cross-family
// review writes TWO distinct rows in the SAME auto-trace run under distinct item
// ids + reviewers — the review row NEVER overwrites the verifiable floor.
func TestReviewRowCoexistsWithFloor(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, &stubStatusReader{status: realCIStatus()})

	// Verifiable floor (merge with real CI).
	if err := h.Harvest(ctx, implementJob(), payload, workflow.Outcome{
		Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef",
	}); err != nil {
		t.Fatalf("Harvest merge returned error: %v", err)
	}
	// Soft cross-family review.
	if err := h.Harvest(ctx, implementJob(), payload, reviewOutcome(map[string]float64{"coverage": 0.5}, "claude", false)); err != nil {
		t.Fatalf("Harvest review returned error: %v", err)
	}

	events := feedbackForVersion(t, store, version.ID)
	if len(events) != 2 {
		t.Fatalf("expected the floor + review row (2), got %d: %+v", len(events), events)
	}
	var floor, review *db.FeedbackEvent
	for i := range events {
		switch events[i].Reviewer {
		case autoTraceReviewer:
			floor = &events[i]
		case reviewReviewerID:
			review = &events[i]
		}
	}
	if floor == nil {
		t.Fatal("the verifiable-floor row (gitmoot-auto) must still exist")
	}
	if review == nil {
		t.Fatalf("the review row (gitmoot-review) must exist; got %+v", events)
	}
	if floor.ItemID == review.ItemID {
		t.Fatalf("floor and review must use DISTINCT item ids; both = %q", floor.ItemID)
	}
	if review.ItemID != reviewItemIDPrefix+"owner/repo#7" {
		t.Fatalf("review item id = %q, want review#owner/repo#7", review.ItemID)
	}
}

// TestReviewRowJudgeTaggedAndDownWeighted: the review row carries the judge-derived
// reviewer + item tag and the run keeps feedback_source=automatic_trace, and no
// contract field/version changed.
func TestReviewRowJudgeTaggedAndDownWeighted(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, nil)

	if err := h.Harvest(ctx, implementJob(), payload, reviewOutcome(map[string]float64{"coverage": 0.7}, "claude", false)); err != nil {
		t.Fatalf("Harvest review returned error: %v", err)
	}

	events := feedbackForVersion(t, store, version.ID)
	if len(events) != 1 || events[0].Reviewer != reviewReviewerID {
		t.Fatalf("expected one gitmoot-review row, got %+v", events)
	}
	if events[0].Source != autoTraceSource {
		t.Fatalf("review row source = %q, want auto-trace (rides the automatic_trace run)", events[0].Source)
	}

	// The review item carries judge_derived=true and is NOT self-family.
	item, ok := reviewItemFor(t, store, version.ID, reviewItemIDPrefix+"owner/repo#7")
	if !ok {
		t.Fatal("review eval_review_item missing")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.MetadataJSON), &meta); err != nil {
		t.Fatalf("review item metadata did not unmarshal: %v", err)
	}
	if meta[reviewItemMetadataJudgeKey] != true {
		t.Fatalf("review item must be judge_derived=true, got %v", meta[reviewItemMetadataJudgeKey])
	}
	if _, present := meta[reviewItemMetadataSelfFamilyKey]; present {
		t.Fatal("a cross-family review must NOT carry the self_family tag")
	}

	// The run still stamps feedback_source=automatic_trace (no contract bump).
	run, err := store.GetEvalRun(ctx, autoTraceRunIDPrefix+version.ID)
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	pkg, err := ExportTrainingPackage(ctx, store, run.ID)
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	if pkg.ContractVersion != ContractVersion || ContractVersion != 1 {
		t.Fatalf("ContractVersion = %d (const %d), want 1 — additive, no bump", pkg.ContractVersion, ContractVersion)
	}
	var fc map[string]any
	if err := json.Unmarshal(pkg.FeedbackContext, &fc); err != nil {
		t.Fatalf("feedback context did not unmarshal: %v", err)
	}
	if fc["feedback_source"] != FeedbackSourceAutomaticTrace {
		t.Fatalf("feedback_source = %v, want %q", fc["feedback_source"], FeedbackSourceAutomaticTrace)
	}
}

// TestSelfFamilyReviewWeightsLower: a same-family fallback row carries
// self_family=true (and its reviewer_runtime) in the item metadata so it weights
// below a cross-family review (REFINEMENT #1), while the feedback row keeps the
// FIXED gitmoot-review reviewer sentinel (the family lives in metadata, not the
// conflict key, so a family change re-reviews in place — see TestReviewRowReUpserts*).
func TestSelfFamilyReviewWeightsLower(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, nil)

	// A same-family (self) review.
	if err := h.Harvest(ctx, implementJob(), payload, reviewOutcome(map[string]float64{"coverage": 0.4}, "codex", true)); err != nil {
		t.Fatalf("Harvest self-family review returned error: %v", err)
	}

	events := feedbackForVersion(t, store, version.ID)
	if len(events) != 1 {
		t.Fatalf("expected one self-family review row, got %+v", events)
	}
	if events[0].Reviewer != reviewReviewerID {
		t.Fatalf("review reviewer = %q, want the fixed gitmoot-review sentinel", events[0].Reviewer)
	}

	item, ok := reviewItemFor(t, store, version.ID, reviewItemIDPrefix+"owner/repo#7")
	if !ok {
		t.Fatal("self-family review item missing")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	if meta[reviewItemMetadataSelfFamilyKey] != true {
		t.Fatalf("self-family review must carry self_family=true, got %v", meta[reviewItemMetadataSelfFamilyKey])
	}
	if meta[reviewItemMetadataReviewerKey] != "codex" {
		t.Fatalf("self-family review must carry reviewer_runtime=codex, got %v", meta[reviewItemMetadataReviewerKey])
	}
	if meta[reviewItemMetadataJudgeKey] != true {
		t.Fatal("self-family review must still be judge_derived=true")
	}
}

// TestReviewRowReUpsertsAcrossFamilyChange (#469 fix): a re-review of the SAME PR
// by a DIFFERENT reviewer family (cross-family claude, then same-family codex
// fallback) re-upserts the SAME single review row in place instead of accumulating
// a stale duplicate — the reviewer id is family-independent, the family lives in
// the item metadata, which the later harvest overwrites.
func TestReviewRowReUpsertsAcrossFamilyChange(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, nil)

	// First: a cross-family claude review.
	if err := h.Harvest(ctx, implementJob(), payload, reviewOutcome(map[string]float64{"coverage": 0.7}, "claude", false)); err != nil {
		t.Fatalf("Harvest cross-family review returned error: %v", err)
	}
	// Later: a same-family codex fallback (different family) for the SAME PR.
	if err := h.Harvest(ctx, implementJob(), payload, reviewOutcome(map[string]float64{"coverage": 0.3}, "codex", true)); err != nil {
		t.Fatalf("Harvest self-family review returned error: %v", err)
	}

	events := feedbackForVersion(t, store, version.ID)
	if len(events) != 1 {
		t.Fatalf("a family change must overwrite, not accumulate; want 1 review row, got %d: %+v", len(events), events)
	}
	// The surviving row reflects the LATEST (codex, self-family, low-mean) harvest.
	if events[0].Choice != "b" {
		t.Fatalf("the latest low-mean review must overwrite to Choice=b, got %q", events[0].Choice)
	}
	item, ok := reviewItemFor(t, store, version.ID, reviewItemIDPrefix+"owner/repo#7")
	if !ok {
		t.Fatal("review item missing after family change")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	if meta[reviewItemMetadataReviewerKey] != "codex" || meta[reviewItemMetadataSelfFamilyKey] != true {
		t.Fatalf("the overwritten item must reflect the latest codex self-family review, got %v", meta)
	}
}

// TestReviewRowReUpsertsInPlace: re-reviewing the SAME PR re-upserts the SAME
// review row (stable row count) — a corrective overwrite, not a duplicate.
func TestReviewRowReUpsertsInPlace(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, nil)

	for i := 0; i < 3; i++ {
		if err := h.Harvest(ctx, implementJob(), payload, reviewOutcome(map[string]float64{"coverage": 0.5}, "claude", false)); err != nil {
			t.Fatalf("Harvest review iteration %d returned error: %v", i, err)
		}
	}
	events := feedbackForVersion(t, store, version.ID)
	if len(events) != 1 {
		t.Fatalf("re-review must re-upsert in place (1 row), got %d: %+v", len(events), events)
	}
}

// TestReviewChoiceTracksQualityLevel: the review's a/b choice is DERIVED from the
// projected rubric mean, not hardcoded — a low-mean (poor) review registers as a
// "b" (non-baseline) vote and a high-mean (good) review as "a" — so a negative
// quality review is no longer silently inverted into a baseline win (#469 fix).
func TestReviewChoiceTracksQualityLevel(t *testing.T) {
	ctx := context.Background()

	// Low mean (0.2/0.3 => 0.25) must register as a non-baseline "b" vote.
	storeLow := newTraceStore(t)
	versionLow, payloadLow := installTraceTemplate(t, storeLow, "planner")
	hLow := NewOutcomeHarvester(storeLow, nil)
	if err := hLow.Harvest(ctx, implementJob(), payloadLow, reviewOutcome(map[string]float64{"coverage": 0.2, "fidelity": 0.3}, "claude", false)); err != nil {
		t.Fatalf("Harvest low review returned error: %v", err)
	}
	low := feedbackForVersion(t, storeLow, versionLow.ID)
	if len(low) != 1 || low[0].Choice != "b" {
		t.Fatalf("a low-mean review must write Choice=b, got %+v", low)
	}

	// High mean (0.9/0.8 => 0.85) stays a positive-leaning "a".
	storeHigh := newTraceStore(t)
	versionHigh, payloadHigh := installTraceTemplate(t, storeHigh, "planner")
	hHigh := NewOutcomeHarvester(storeHigh, nil)
	if err := hHigh.Harvest(ctx, implementJob(), payloadHigh, reviewOutcome(map[string]float64{"coverage": 0.9, "fidelity": 0.8}, "claude", false)); err != nil {
		t.Fatalf("Harvest high review returned error: %v", err)
	}
	high := feedbackForVersion(t, storeHigh, versionHigh.ID)
	if len(high) != 1 || high[0].Choice != "a" {
		t.Fatalf("a high-mean review must write Choice=a, got %+v", high)
	}
}

// TestReviewEmptyRubricWritesNoRow: an EMPTY rubric (HasScore=false) is
// non-informative, so the harvester writes NO review row at all rather than
// fabricating a baseline vote (#469 fix — the projected magnitude is load-bearing).
func TestReviewEmptyRubricWritesNoRow(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, nil)

	if err := h.Harvest(ctx, implementJob(), payload, reviewOutcome(map[string]float64{}, "claude", false)); err != nil {
		t.Fatalf("Harvest empty-rubric review returned error: %v", err)
	}
	if events := feedbackForVersion(t, store, version.ID); len(events) != 0 {
		t.Fatalf("an empty (HasScore=false) rubric must write no review row, got %+v", events)
	}
}

// TestReviewItemCarriesProjectedScore: the projected rubric MEAN is persisted on
// the review item (rubric_score) so the quality MAGNITUDE the judge measured
// reaches the row — projectReview's score path is wired, not dead.
func TestReviewItemCarriesProjectedScore(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, nil)

	if err := h.Harvest(ctx, implementJob(), payload, reviewOutcome(map[string]float64{"coverage": 0.2, "fidelity": 0.4}, "claude", false)); err != nil {
		t.Fatalf("Harvest review returned error: %v", err)
	}
	item, ok := reviewItemFor(t, store, version.ID, reviewItemIDPrefix+"owner/repo#7")
	if !ok {
		t.Fatal("review eval_review_item missing")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.MetadataJSON), &meta); err != nil {
		t.Fatalf("review item metadata did not unmarshal: %v", err)
	}
	score, ok := meta[reviewItemMetadataScoreKey].(float64)
	if !ok {
		t.Fatalf("review item must carry the projected rubric_score, got %v", meta[reviewItemMetadataScoreKey])
	}
	if want := (0.2 + 0.4) / 2.0; score-want > 1e-9 || score-want < -1e-9 {
		t.Fatalf("rubric_score = %v, want the mean %v", score, want)
	}
}

// TestHarvestReviewOutOfScopeJobSkips: an OutcomeReviewed against a non-implement
// (out-of-scope) job writes no review row (the harvester's inScope gate applies).
func TestHarvestReviewOutOfScopeJobSkips(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, nil)

	reviewJob := db.Job{ID: "job-review-1", Type: "review"}
	if err := h.Harvest(ctx, reviewJob, payload, reviewOutcome(map[string]float64{"coverage": 0.5}, "claude", false)); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	if events := feedbackForVersion(t, store, version.ID); len(events) != 0 {
		t.Fatalf("out-of-scope review must write nothing, got %+v", events)
	}
}
