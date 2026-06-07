package skillopt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestRecommendPhaseExploreToRefine(t *testing.T) {
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "c", "c", "d", "a", "b"),
	}
	recommendation := RecommendPhase(
		db.EvalRun{ID: "run-1", Mode: db.EvalRunModeExplore, ExplorationLevel: db.ExplorationLevelHigh},
		nil,
		ranked,
		pairwisePreferences(12),
	)

	if recommendation.CurrentMode != db.EvalRunModeExplore || recommendation.RecommendedMode != db.EvalRunModeRefine {
		t.Fatalf("recommendation = %+v", recommendation)
	}
	if recommendation.RankingStability != "c 2/2" {
		t.Fatalf("ranking stability = %q", recommendation.RankingStability)
	}
}

func TestRecommendPhaseExploreStableWinnerWithPoorQualityStaysExplore(t *testing.T) {
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "c", "c", "d", "a", "b"),
	}
	ranked[0].Quality = "poor"
	ranked[1].Quality = "poor"

	recommendation := RecommendPhase(
		db.EvalRun{ID: "run-1", Mode: db.EvalRunModeExplore, ExplorationLevel: db.ExplorationLevelHigh},
		nil,
		ranked,
		pairwisePreferences(12),
	)

	if recommendation.RecommendedMode != db.EvalRunModeExplore || !strings.Contains(recommendation.Reason, "quality: poor") {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseExploreExplicitContinueModeStaysExplore(t *testing.T) {
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "c", "c", "d", "a", "b"),
	}
	ranked[0].ContinueMode = db.EvalRunModeExplore

	recommendation := RecommendPhase(
		db.EvalRun{ID: "run-1", Mode: db.EvalRunModeExplore, ExplorationLevel: db.ExplorationLevelHigh},
		nil,
		ranked,
		pairwisePreferences(12),
	)

	if recommendation.RecommendedMode != db.EvalRunModeExplore || !strings.Contains(recommendation.Reason, "continue_mode: explore") {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseExploreTieStaysExplore(t *testing.T) {
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "d", "d", "c", "a", "b"),
	}
	recommendation := RecommendPhase(
		db.EvalRun{ID: "run-1", Mode: db.EvalRunModeExplore, ExplorationLevel: db.ExplorationLevelHigh},
		nil,
		ranked,
		pairwisePreferences(12),
	)

	if recommendation.RecommendedMode != db.EvalRunModeExplore || recommendation.RankingStability != "tie 1/2" {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseCountsTopTieAsFeedback(t *testing.T) {
	tiedTop := rankedEvent(t, "", "a", "b", "c", "d")
	tiedTop.TieGroupsJSON = `[["a","b","c","d"]]`
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "c", "c", "d", "a", "b"),
		tiedTop,
	}
	recommendation := RecommendPhase(
		db.EvalRun{ID: "run-1", Mode: db.EvalRunModeExplore, ExplorationLevel: db.ExplorationLevelHigh},
		nil,
		ranked,
		pairwisePreferences(12),
	)

	if recommendation.RankingStability != "c 2/3" {
		t.Fatalf("ranking stability = %q, want c 2/3", recommendation.RankingStability)
	}
}

func TestRecommendPhaseForItemsMissingFeedbackStaysCurrent(t *testing.T) {
	items := []db.EvalReviewItem{
		{ItemID: "item-001"},
		{ItemID: "item-002"},
	}
	ranked := []db.RankedFeedbackEvent{
		rankedEventForItem(t, "item-001", "c", "c", "a", "d", "b"),
		rankedEventForItem(t, "item-001", "c", "c", "d", "a", "b"),
	}
	recommendation := RecommendPhaseForItems(
		db.EvalRun{ID: "run-1", Mode: db.EvalRunModeExplore, ExplorationLevel: db.ExplorationLevelHigh},
		items,
		nil,
		ranked,
		pairwisePreferences(12),
	)

	if recommendation.RecommendedMode != db.EvalRunModeExplore || recommendation.MissingFeedbackCount != 1 {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseRefineToDistill(t *testing.T) {
	useful, err := json.Marshal(map[string][]string{"c": {"clear product explanation"}, "a": {"best visual tone"}})
	if err != nil {
		t.Fatalf("marshal useful traits: %v", err)
	}
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "c", "c", "d", "a", "b"),
	}
	ranked[0].UsefulTraitsJSON = string(useful)

	recommendation := RecommendPhase(db.EvalRun{ID: "run-1", Mode: db.EvalRunModeRefine}, nil, ranked, pairwisePreferences(12))

	if recommendation.RecommendedMode != db.EvalRunModeDistill {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseRefineTieStaysRefine(t *testing.T) {
	useful, err := json.Marshal(map[string][]string{"c": {"clear product explanation"}, "d": {"best motion"}})
	if err != nil {
		t.Fatalf("marshal useful traits: %v", err)
	}
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "d", "d", "c", "a", "b"),
	}
	ranked[0].UsefulTraitsJSON = string(useful)

	recommendation := RecommendPhase(db.EvalRun{ID: "run-1", Mode: db.EvalRunModeRefine}, nil, ranked, pairwisePreferences(12))

	if recommendation.RecommendedMode != db.EvalRunModeRefine || recommendation.RankingStability != "tie 1/2" {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseDistillToValidate(t *testing.T) {
	recommendation := RecommendPhase(
		db.EvalRun{ID: "run-1", Mode: db.EvalRunModeDistill},
		nil,
		[]db.RankedFeedbackEvent{rankedEvent(t, "b", "b", "a")},
		pairwisePreferences(1),
	)

	if recommendation.RecommendedMode != db.EvalRunModeValidate {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseValidateToPromote(t *testing.T) {
	feedback := []db.FeedbackEvent{
		{Choice: "b"},
		{Choice: "b"},
		{Choice: "a"},
	}
	recommendation := RecommendPhase(db.EvalRun{ID: "run-1", Mode: db.EvalRunModeValidate}, feedback, nil, nil)

	if recommendation.RecommendedMode != PromoteRecommendation {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseRankedValidateToPromote(t *testing.T) {
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "c", "c", "d", "a", "b"),
	}
	recommendation := RecommendPhase(db.EvalRun{ID: "run-1", Mode: db.EvalRunModeValidate, OptionsCount: 4}, nil, ranked, pairwisePreferences(12))

	if recommendation.RecommendedMode != PromoteRecommendation {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseRankedValidateTieStaysValidate(t *testing.T) {
	ranked := []db.RankedFeedbackEvent{
		rankedEvent(t, "c", "c", "a", "d", "b"),
		rankedEvent(t, "d", "d", "c", "a", "b"),
	}
	recommendation := RecommendPhase(db.EvalRun{ID: "run-1", Mode: db.EvalRunModeValidate, OptionsCount: 4}, nil, ranked, pairwisePreferences(12))

	if recommendation.RecommendedMode != db.EvalRunModeValidate || recommendation.RankingStability != "tie 1/2" {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func TestRecommendPhaseNoFeedbackContinuesCurrentMode(t *testing.T) {
	recommendation := RecommendPhase(db.EvalRun{ID: "run-1", Mode: db.EvalRunModeExplore}, nil, nil, nil)

	if recommendation.RecommendedMode != db.EvalRunModeExplore || recommendation.RankingStability != "none" {
		t.Fatalf("recommendation = %+v", recommendation)
	}
}

func rankedEvent(t *testing.T, winner string, ranking ...string) db.RankedFeedbackEvent {
	t.Helper()
	return rankedEventForItem(t, "", winner, ranking...)
}

func rankedEventForItem(t *testing.T, itemID string, winner string, ranking ...string) db.RankedFeedbackEvent {
	t.Helper()
	encoded, err := json.Marshal(ranking)
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	return db.RankedFeedbackEvent{
		ItemID:      itemID,
		Winner:      winner,
		RankingJSON: string(encoded),
	}
}

func pairwisePreferences(count int) []db.PairwisePreference {
	preferences := make([]db.PairwisePreference, count)
	for i := range preferences {
		preferences[i] = db.PairwisePreference{Preferred: "c", Rejected: "a"}
	}
	return preferences
}
