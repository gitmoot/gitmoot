package skillopt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

const PromoteRecommendation = "promote"

type PhaseRecommendation struct {
	CurrentMode             string
	RecommendedMode         string
	ExplorationLevel        string
	ReviewItemCount         int
	FeedbackItemCount       int
	MissingFeedbackCount    int
	FeedbackCount           int
	RankedFeedbackCount     int
	PairwisePreferenceCount int
	RankingStability        string
	Reason                  string
}

func (r PhaseRecommendation) Summary() string {
	mode := strings.TrimSpace(r.RecommendedMode)
	if mode == "" {
		mode = normalizeRecommendationMode(r.CurrentMode)
	}
	if mode == normalizeRecommendationMode(r.CurrentMode) {
		return fmt.Sprintf("recommend continue %s - %s", mode, strings.TrimSpace(r.Reason))
	}
	return fmt.Sprintf("recommend %s - %s", mode, strings.TrimSpace(r.Reason))
}

func RecommendPhaseForItems(run db.EvalRun, items []db.EvalReviewItem, feedback []db.FeedbackEvent, ranked []db.RankedFeedbackEvent, pairwise []db.PairwisePreference) PhaseRecommendation {
	currentMode := normalizeRecommendationMode(run.Mode)
	explorationLevel := strings.TrimSpace(run.ExplorationLevel)
	stability := rankingStability(ranked)
	itemCount, feedbackItemCount, missingFeedbackCount := feedbackCoverage(items, feedback, ranked)
	recommendation := PhaseRecommendation{
		CurrentMode:             currentMode,
		RecommendedMode:         currentMode,
		ExplorationLevel:        explorationLevel,
		ReviewItemCount:         itemCount,
		FeedbackItemCount:       feedbackItemCount,
		MissingFeedbackCount:    missingFeedbackCount,
		FeedbackCount:           len(feedback) + len(ranked),
		RankedFeedbackCount:     len(ranked),
		PairwisePreferenceCount: len(pairwise),
		RankingStability:        stability,
	}
	if missingFeedbackCount > 0 {
		recommendation.Reason = fmt.Sprintf("%d of %d review items have imported feedback; collect feedback for every item before changing phases.", feedbackItemCount, itemCount)
		return recommendation
	}
	if shouldContinueExploration(ranked) {
		recommendation.RecommendedMode = db.EvalRunModeExplore
		recommendation.Reason = explorationSignalReason(ranked)
		return recommendation
	}

	switch currentMode {
	case db.EvalRunModeExplore:
		recommendation.RecommendedMode, recommendation.Reason = recommendExplore(ranked, stability)
	case db.EvalRunModeRefine:
		recommendation.RecommendedMode, recommendation.Reason = recommendRefine(ranked, pairwise, stability)
	case db.EvalRunModeDistill:
		recommendation.RecommendedMode, recommendation.Reason = recommendDistill(ranked, pairwise)
	case db.EvalRunModeValidate:
		recommendation.RecommendedMode, recommendation.Reason = recommendValidate(feedback, ranked)
	default:
		recommendation.RecommendedMode = currentMode
		recommendation.Reason = "current mode is not recognized, so keep collecting feedback before changing phases."
	}
	return recommendation
}

func shouldContinueExploration(ranked []db.RankedFeedbackEvent) bool {
	if len(ranked) == 0 {
		return false
	}
	if hasContinueMode(ranked, db.EvalRunModeExplore) {
		return true
	}
	return allRankedQuality(ranked, "poor")
}

func explorationSignalReason(ranked []db.RankedFeedbackEvent) string {
	if hasContinueMode(ranked, db.EvalRunModeExplore) {
		return "ranked feedback explicitly requested continue_mode: explore, so keep exploring broader alternatives."
	}
	return "all ranked feedback is marked quality: poor, so ranking stability is only relative and broad exploration should continue."
}

func hasContinueMode(ranked []db.RankedFeedbackEvent, mode string) bool {
	mode = strings.TrimSpace(mode)
	for _, event := range ranked {
		if strings.TrimSpace(event.ContinueMode) == mode {
			return true
		}
	}
	return false
}

func allRankedQuality(ranked []db.RankedFeedbackEvent, quality string) bool {
	if len(ranked) == 0 {
		return false
	}
	quality = strings.TrimSpace(quality)
	for _, event := range ranked {
		if strings.TrimSpace(event.Quality) != quality {
			return false
		}
	}
	return true
}

func feedbackCoverage(items []db.EvalReviewItem, feedback []db.FeedbackEvent, ranked []db.RankedFeedbackEvent) (int, int, int) {
	if len(items) == 0 {
		return 0, 0, 0
	}
	itemIDs := make(map[string]struct{}, len(items))
	for _, item := range items {
		itemID := reviewItemID(item)
		if itemID != "" {
			itemIDs[itemID] = struct{}{}
		}
	}
	covered := map[string]struct{}{}
	for _, event := range feedback {
		itemID := strings.TrimSpace(event.ItemID)
		if _, ok := itemIDs[itemID]; ok {
			covered[itemID] = struct{}{}
		}
	}
	for _, event := range ranked {
		itemID := strings.TrimSpace(event.ItemID)
		if _, ok := itemIDs[itemID]; ok {
			covered[itemID] = struct{}{}
		}
	}
	return len(itemIDs), len(covered), len(itemIDs) - len(covered)
}

func reviewItemID(item db.EvalReviewItem) string {
	itemID := strings.TrimSpace(item.ItemID)
	if itemID != "" {
		return itemID
	}
	return strings.TrimSpace(item.ID)
}

func recommendExplore(ranked []db.RankedFeedbackEvent, stability string) (string, string) {
	winner, count, total := topWinner(ranked)
	if winner != "tie" && total >= 2 && count >= 2 {
		return db.EvalRunModeRefine, fmt.Sprintf("top option %s leads %d of %d ranked feedback events, so broad exploration has a stable direction.", winner, count, total)
	}
	if total == 0 {
		return db.EvalRunModeExplore, "no ranked feedback has been imported yet, so keep exploring broad alternatives."
	}
	return db.EvalRunModeExplore, fmt.Sprintf("ranking stability is %s; collect more ranked feedback before narrowing.", stability)
}

func recommendRefine(ranked []db.RankedFeedbackEvent, pairwise []db.PairwisePreference, stability string) (string, string) {
	winner, count, total := topWinner(ranked)
	stableWinner := winner != "tie" && total >= 2 && count >= 2
	traitCount := rankedTraitCount(ranked)
	if stableWinner && traitCount >= 2 {
		return db.EvalRunModeDistill, fmt.Sprintf("ranked feedback includes %d trait notes, so refine learnings are ready to distill into template guidance.", traitCount)
	}
	if stableWinner && len(pairwise) >= 12 {
		return db.EvalRunModeDistill, fmt.Sprintf("ranked feedback produced %d pairwise preferences with %s stability, so the direction is specific enough to distill.", len(pairwise), stability)
	}
	if len(ranked) == 0 {
		return db.EvalRunModeRefine, "no ranked feedback has been imported yet, so keep refining with focused alternatives."
	}
	return db.EvalRunModeRefine, "feedback is still broad; collect more specific useful and rejected traits before distilling."
}

func recommendDistill(ranked []db.RankedFeedbackEvent, pairwise []db.PairwisePreference) (string, string) {
	if len(ranked) > 0 && len(pairwise) > 0 {
		return db.EvalRunModeValidate, fmt.Sprintf("distillation has %d ranked feedback events and %d pairwise preferences, so prepare a fresh validation run.", len(ranked), len(pairwise))
	}
	return db.EvalRunModeDistill, "distill candidate guidance first, then validate against fresh items."
}

func recommendValidate(feedback []db.FeedbackEvent, ranked []db.RankedFeedbackEvent) (string, string) {
	if len(ranked) > 0 {
		winner, count, total := topWinner(ranked)
		if winner != "tie" && total >= 2 && count >= 2 {
			return PromoteRecommendation, fmt.Sprintf("ranked validation option %s wins %d of %d fresh feedback events.", winner, count, total)
		}
		return db.EvalRunModeValidate, fmt.Sprintf("ranked validation stability is %s; keep validating before promotion.", rankingStability(ranked))
	}
	baselineWins := 0
	candidateWins := 0
	for _, event := range feedback {
		switch strings.TrimSpace(strings.ToLower(event.Choice)) {
		case "a":
			baselineWins++
		case "b":
			candidateWins++
		}
	}
	if candidateWins >= 2 && candidateWins > baselineWins {
		return PromoteRecommendation, fmt.Sprintf("candidate wins %d validation feedback events against %d baseline wins on fresh items.", candidateWins, baselineWins)
	}
	if len(feedback) == 0 {
		return db.EvalRunModeValidate, "no validation feedback has been imported yet, so keep validating before promotion."
	}
	return db.EvalRunModeValidate, fmt.Sprintf("candidate wins %d validation feedback events against %d baseline wins; keep validating before promotion.", candidateWins, baselineWins)
}

func normalizeRecommendationMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case db.EvalRunModeExplore:
		return db.EvalRunModeExplore
	case db.EvalRunModeRefine:
		return db.EvalRunModeRefine
	case db.EvalRunModeDistill:
		return db.EvalRunModeDistill
	case db.EvalRunModeValidate:
		return db.EvalRunModeValidate
	}
	return db.EvalRunModeValidate
}

func rankingStability(ranked []db.RankedFeedbackEvent) string {
	winner, count, total := topWinner(ranked)
	if total == 0 {
		return "none"
	}
	return fmt.Sprintf("%s %d/%d", winner, count, total)
}

func topWinner(ranked []db.RankedFeedbackEvent) (string, int, int) {
	counts := map[string]int{}
	total := 0
	for _, event := range ranked {
		winner := normalizedWinner(event)
		if winner == "" {
			continue
		}
		counts[winner]++
		total++
	}
	if total == 0 {
		return "none", 0, 0
	}
	labels := make([]string, 0, len(counts))
	for label := range counts {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	best := labels[0]
	tied := false
	for _, label := range labels[1:] {
		if counts[label] > counts[best] {
			best = label
			tied = false
			continue
		}
		if counts[label] == counts[best] {
			tied = true
		}
	}
	if tied {
		return "tie", counts[best], total
	}
	return best, counts[best], total
}

func normalizedWinner(event db.RankedFeedbackEvent) string {
	winner := strings.TrimSpace(strings.ToLower(event.Winner))
	if winner != "" {
		return winner
	}
	if strings.TrimSpace(event.TieGroupsJSON) != "" {
		var groups [][]string
		if err := json.Unmarshal([]byte(event.TieGroupsJSON), &groups); err == nil && len(groups) > 0 {
			if len(groups[0]) != 1 {
				return "tie"
			}
			return strings.TrimSpace(strings.ToLower(groups[0][0]))
		}
	}
	var ranking []string
	if err := json.Unmarshal([]byte(event.RankingJSON), &ranking); err != nil || len(ranking) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.ToLower(ranking[0]))
}

func rankedTraitCount(ranked []db.RankedFeedbackEvent) int {
	total := 0
	for _, event := range ranked {
		total += traitJSONCount(event.UsefulTraitsJSON)
		total += traitJSONCount(event.RejectedTraitsJSON)
	}
	return total
}

func traitJSONCount(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var traits map[string][]string
	if err := json.Unmarshal([]byte(value), &traits); err != nil {
		return 0
	}
	total := 0
	for _, values := range traits {
		total += len(values)
	}
	return total
}
