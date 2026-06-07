package feedback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"gopkg.in/yaml.v3"
)

const assignmentsName = ".assignments.json"

type MarkdownCollector struct {
	BlobStore artifact.Store
	Now       func() time.Time
}

type assignmentFile struct {
	RunID string            `json:"run_id"`
	Items []blindAssignment `json:"items"`
}

type blindAssignment struct {
	ItemID    string                  `json:"item_id"`
	A         string                  `json:"a"`
	B         string                  `json:"b"`
	Baseline  string                  `json:"baseline"`
	Candidate string                  `json:"candidate"`
	Options   []blindOptionAssignment `json:"options,omitempty"`
}

type blindOptionAssignment struct {
	Label        string `json:"label"`
	ArtifactID   string `json:"artifact_id"`
	Role         string `json:"role,omitempty"`
	MetadataJSON string `json:"metadata_json,omitempty"`
}

type feedbackFile struct {
	RunID     string              `yaml:"run_id"`
	Reviewer  string              `yaml:"reviewer"`
	Items     []feedbackFileEntry `yaml:"items"`
	ShortForm bool                `yaml:"-"`
}

type feedbackFileEntry struct {
	ItemID               string              `yaml:"item_id"`
	Choice               string              `yaml:"choice"`
	Ranking              []string            `yaml:"ranking,omitempty"`
	TieGroups            [][]string          `yaml:"-"`
	Winner               string              `yaml:"winner,omitempty"`
	UsefulTraits         map[string][]string `yaml:"useful_traits,omitempty"`
	RejectedTraits       map[string][]string `yaml:"rejected_traits,omitempty"`
	RequiredImprovements []string            `yaml:"required_improvements,omitempty"`
	Quality              string              `yaml:"quality"`
	ContinueMode         string              `yaml:"continue_mode"`
	Promote              string              `yaml:"promote"`
	Reasoning            string              `yaml:"reasoning,omitempty"`
}

type ImportResult struct {
	FeedbackEvents       []db.FeedbackEvent
	RankedFeedbackEvents []db.RankedFeedbackEvent
	Diagnostics          []string
}

func (r ImportResult) Count() int {
	return len(r.FeedbackEvents) + len(r.RankedFeedbackEvents)
}

func (c MarkdownCollector) WritePacket(ctx context.Context, store *db.Store, runID string, dir string) error {
	if store == nil {
		return errors.New("store is required")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("packet output directory is required")
	}
	run, err := store.GetEvalRun(ctx, runID)
	if err != nil {
		return err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("eval run %s has no review items", run.ID)
	}
	if err := os.MkdirAll(filepath.Join(dir, "items"), 0o755); err != nil {
		return fmt.Errorf("create packet directory: %w", err)
	}
	assignments := assignmentFile{RunID: run.ID}
	for _, item := range items {
		assignment, err := validatedAssignmentForReview(ctx, store, run, item)
		if err != nil {
			return err
		}
		assignments.Items = append(assignments.Items, assignment)
		if err := c.writeItem(ctx, store, dir, item, assignment); err != nil {
			return err
		}
	}
	sort.Slice(assignments.Items, func(i, j int) bool {
		return assignments.Items[i].ItemID < assignments.Items[j].ItemID
	})
	if err := writeJSONFile(filepath.Join(dir, assignmentsName), assignments, 0o600); err != nil {
		return err
	}
	recommendation, err := phaseRecommendationForRun(ctx, store, run, items)
	if err != nil {
		return err
	}
	if err := writeTextFile(filepath.Join(dir, "index.md"), indexMarkdown(run, items, assignments, recommendation), 0o644); err != nil {
		return err
	}
	return writeFeedbackYAML(filepath.Join(dir, "feedback.yml"), run.ID, assignments)
}

func (c MarkdownCollector) ImportPacket(ctx context.Context, store *db.Store, dir string, reviewerOverride string) (ImportResult, error) {
	if store == nil {
		return ImportResult{}, errors.New("store is required")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ImportResult{}, errors.New("packet directory is required")
	}
	dir, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return ImportResult{}, fmt.Errorf("resolve packet directory: %w", err)
	}
	assignments, err := readAssignments(filepath.Join(dir, assignmentsName))
	if err != nil {
		return ImportResult{}, err
	}
	feedback, err := readFeedbackYAML(filepath.Join(dir, "feedback.yml"))
	if err != nil {
		return ImportResult{}, err
	}
	if feedback.RunID != assignments.RunID {
		return ImportResult{}, fmt.Errorf("feedback run_id %q does not match assignments run_id %q", feedback.RunID, assignments.RunID)
	}
	if err := validateAssignmentsAgainstStore(ctx, store, assignments); err != nil {
		return ImportResult{}, err
	}
	reviewer := strings.TrimSpace(reviewerOverride)
	if reviewer == "" {
		reviewer = strings.TrimSpace(feedback.Reviewer)
	}
	if reviewer == "" {
		return ImportResult{}, errors.New("feedback reviewer is required")
	}
	assignmentsByItem := map[string]blindAssignment{}
	for _, assignment := range assignments.Items {
		itemID := strings.TrimSpace(assignment.ItemID)
		if itemID == "" {
			return ImportResult{}, errors.New("assignments contain an item with empty item_id")
		}
		if _, ok := assignmentsByItem[itemID]; ok {
			return ImportResult{}, fmt.Errorf("assignments contain duplicate item_id %q", itemID)
		}
		assignmentsByItem[itemID] = assignment
	}
	now := c.now().UTC().Format(time.RFC3339)
	events := make([]db.FeedbackEvent, 0, len(feedback.Items))
	rankedEvents := make([]db.RankedFeedbackEvent, 0, len(feedback.Items))
	seenItems := map[string]struct{}{}
	for _, entry := range feedback.Items {
		itemID := strings.TrimSpace(entry.ItemID)
		assignment, ok := assignmentsByItem[itemID]
		if !ok {
			return ImportResult{}, fmt.Errorf("unknown feedback item_id %q", itemID)
		}
		if _, ok := seenItems[itemID]; ok {
			return ImportResult{}, fmt.Errorf("duplicate feedback item_id %q", itemID)
		}
		seenItems[itemID] = struct{}{}
		if len(assignment.Options) > 0 {
			event, err := rankedFeedbackEventFromEntry(feedback.RunID, itemID, entry, assignment, reviewer, SourceMarkdown, dir, now)
			if err != nil {
				return ImportResult{}, fmt.Errorf("item %s: %w", itemID, feedbackImportErrorHint(err))
			}
			rankedEvents = append(rankedEvents, event)
			continue
		}
		choice, err := NormalizeChoice(entry.Choice)
		if err != nil {
			return ImportResult{}, fmt.Errorf("item %s: %w", itemID, err)
		}
		choice, err = deblindChoice(choice, assignment)
		if err != nil {
			return ImportResult{}, fmt.Errorf("item %s: %w", itemID, err)
		}
		event := db.FeedbackEvent{
			RunID:     feedback.RunID,
			ItemID:    itemID,
			Choice:    choice,
			Reasoning: strings.TrimSpace(entry.Reasoning),
			Reviewer:  reviewer,
			Source:    SourceMarkdown,
			SourceURL: dir,
			CreatedAt: now,
		}
		events = append(events, event)
	}
	if len(seenItems) != len(assignmentsByItem) {
		missing := make([]string, 0, len(assignmentsByItem)-len(seenItems))
		for itemID := range assignmentsByItem {
			if _, ok := seenItems[itemID]; !ok {
				missing = append(missing, itemID)
			}
		}
		sort.Strings(missing)
		return ImportResult{}, fmt.Errorf("feedback.yml missing feedback for item_id %q", missing[0])
	}
	for _, event := range events {
		if err := store.UpsertFeedbackEvent(ctx, event); err != nil {
			return ImportResult{}, err
		}
	}
	for _, event := range rankedEvents {
		if err := store.UpsertRankedFeedbackEvent(ctx, event); err != nil {
			return ImportResult{}, err
		}
	}
	return ImportResult{FeedbackEvents: events, RankedFeedbackEvents: rankedEvents}, nil
}

func (c MarkdownCollector) writeItem(ctx context.Context, store *db.Store, dir string, item db.EvalReviewItem, assignment blindAssignment) error {
	if len(assignment.Options) > 0 {
		return c.writeRankedItem(ctx, store, dir, item, assignment)
	}
	optionA, err := c.optionText(ctx, store, assignment.A)
	if err != nil {
		return fmt.Errorf("item %s option a: %w", item.ItemID, err)
	}
	optionB, err := c.optionText(ctx, store, assignment.B)
	if err != nil {
		return fmt.Errorf("item %s option b: %w", item.ItemID, err)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "# %s\n\n", itemTitle(item))
	builder.WriteString("## Option A\n\n")
	writeMarkdownFence(&builder, optionA)
	builder.WriteString("\n## Option B\n\n")
	writeMarkdownFence(&builder, optionB)
	builder.WriteString("\n## Feedback\n\n")
	builder.WriteString("Record this item in `../feedback.yml` with one of: `a`, `b`, `tie`, `neither`, `skip`.\n")
	return writeTextFile(filepath.Join(dir, "items", itemFilename(item.ItemID)), builder.String(), 0o644)
}

func (c MarkdownCollector) writeRankedItem(ctx context.Context, store *db.Store, dir string, item db.EvalReviewItem, assignment blindAssignment) error {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# %s\n\n", itemTitle(item))
	builder.WriteString("Rank every option in `../feedback.yml` from best to worst. Use the option labels exactly as shown here.\n\n")
	writeOptionReferenceTable(&builder, assignment.Options)
	for _, option := range assignment.Options {
		if optionHasPreviewBundleWithoutReference(option) {
			return missingPreviewURLForBundleError(item.ItemID, option.Label)
		}
		if optionHasPreviewBundle(option) {
			fmt.Fprintf(&builder, "\n## Option %s\n\nReference: %s\n", displayOptionLabel(option.Label), optionReference(option, true))
			continue
		}
		content, err := c.optionText(ctx, store, option.ArtifactID)
		if err != nil {
			return fmt.Errorf("item %s option %s: %w", item.ItemID, option.Label, err)
		}
		fmt.Fprintf(&builder, "\n## Option %s\n\n", displayOptionLabel(option.Label))
		writeMarkdownFence(&builder, content)
	}
	builder.WriteString("\n## Feedback\n\n")
	builder.WriteString("Record `ranking`, optional trait notes, and concise reasoning in `../feedback.yml`.\n")
	return writeTextFile(filepath.Join(dir, "items", itemFilename(item.ItemID)), builder.String(), 0o644)
}

func (c MarkdownCollector) optionText(ctx context.Context, store *db.Store, artifactID string) (string, error) {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return "", nil
	}
	record, err := store.GetEvalArtifact(ctx, artifactID)
	if err != nil {
		return "", err
	}
	content, err := c.BlobStore.Read(record.Hash)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func (c MarkdownCollector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func assignmentFor(runID string, item db.EvalReviewItem) blindAssignment {
	assignment := blindAssignment{
		ItemID:    item.ItemID,
		A:         item.BaselineArtifactID,
		B:         item.CandidateArtifactID,
		Baseline:  item.BaselineArtifactID,
		Candidate: item.CandidateArtifactID,
	}
	sum := sha256.Sum256([]byte(runID + "\x00" + item.ItemID))
	if sum[0]%2 == 1 {
		assignment.A = item.CandidateArtifactID
		assignment.B = item.BaselineArtifactID
	}
	return assignment
}

func validatedAssignmentForReview(ctx context.Context, store *db.Store, run db.EvalRun, item db.EvalReviewItem) (blindAssignment, error) {
	if reviewUsesRankedOptions(run) {
		return validatedRankedAssignmentFor(ctx, store, run, item)
	}
	return validatedAssignmentFor(run.ID, item)
}

func reviewUsesRankedOptions(run db.EvalRun) bool {
	return run.Mode != "" && run.Mode != db.EvalRunModeValidate || run.OptionsCount > 2
}

func validatedRankedAssignmentFor(ctx context.Context, store *db.Store, run db.EvalRun, item db.EvalReviewItem) (blindAssignment, error) {
	options, err := store.ListEvalReviewOptions(ctx, run.ID, item.ItemID)
	if err != nil {
		return blindAssignment{}, err
	}
	if len(options) == 0 {
		return blindAssignment{}, fmt.Errorf("eval review item %s requires registered options", item.ItemID)
	}
	if run.OptionsCount > 0 && len(options) != run.OptionsCount {
		return blindAssignment{}, fmt.Errorf("eval review item %s has %d options, want %d", item.ItemID, len(options), run.OptionsCount)
	}
	assignment := blindAssignment{ItemID: item.ItemID}
	for _, option := range options {
		if strings.TrimSpace(option.ArtifactID) == "" {
			return blindAssignment{}, fmt.Errorf("eval review item %s option %s missing artifact", item.ItemID, option.Label)
		}
		assignment.Options = append(assignment.Options, blindOptionAssignment{
			Label:        strings.TrimSpace(option.Label),
			ArtifactID:   strings.TrimSpace(option.ArtifactID),
			Role:         strings.TrimSpace(option.Role),
			MetadataJSON: strings.TrimSpace(option.MetadataJSON),
		})
	}
	sort.Slice(assignment.Options, func(i, j int) bool {
		return assignment.Options[i].Label < assignment.Options[j].Label
	})
	return assignment, nil
}

func validatedAssignmentFor(runID string, item db.EvalReviewItem) (blindAssignment, error) {
	baselineArtifactID := strings.TrimSpace(item.BaselineArtifactID)
	candidateArtifactID := strings.TrimSpace(item.CandidateArtifactID)
	if baselineArtifactID == "" || candidateArtifactID == "" {
		return blindAssignment{}, fmt.Errorf("eval review item %s requires both baseline and candidate artifacts", item.ItemID)
	}
	if baselineArtifactID == candidateArtifactID {
		return blindAssignment{}, fmt.Errorf("eval review item %s requires distinct baseline and candidate artifacts", item.ItemID)
	}
	return assignmentFor(runID, item), nil
}

func validateAssignmentsAgainstStore(ctx context.Context, store *db.Store, assignments assignmentFile) error {
	run, err := store.GetEvalRun(ctx, assignments.RunID)
	if err != nil {
		return fmt.Errorf("load eval run %s: %w", assignments.RunID, err)
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return err
	}
	itemsByID := make(map[string]db.EvalReviewItem, len(items))
	for _, item := range items {
		itemsByID[item.ItemID] = item
	}
	if len(itemsByID) != len(assignments.Items) {
		return fmt.Errorf("packet assignments contain %d items, store has %d review items for run %s", len(assignments.Items), len(itemsByID), run.ID)
	}
	for _, assignment := range assignments.Items {
		itemID := strings.TrimSpace(assignment.ItemID)
		item, ok := itemsByID[itemID]
		if !ok {
			return fmt.Errorf("packet assignment item_id %q is not in eval run %s", itemID, run.ID)
		}
		if len(assignment.Options) > 0 {
			current, err := validatedRankedAssignmentFor(ctx, store, run, item)
			if err != nil {
				return err
			}
			if !rankedAssignmentsMatch(assignment, current) {
				return fmt.Errorf("packet assignment item_id %q does not match stored review options", itemID)
			}
			continue
		}
		if strings.TrimSpace(assignment.Baseline) != strings.TrimSpace(item.BaselineArtifactID) ||
			strings.TrimSpace(assignment.Candidate) != strings.TrimSpace(item.CandidateArtifactID) {
			return fmt.Errorf("packet assignment item_id %q does not match stored baseline/candidate artifacts", itemID)
		}
		if strings.TrimSpace(item.BaselineArtifactID) == strings.TrimSpace(item.CandidateArtifactID) {
			return fmt.Errorf("packet assignment item_id %q stored baseline and candidate artifacts must be distinct", itemID)
		}
		optionA, err := canonicalChoiceForArtifact(assignment.A, assignment)
		if err != nil {
			return fmt.Errorf("packet assignment item_id %q option a: %w", itemID, err)
		}
		optionB, err := canonicalChoiceForArtifact(assignment.B, assignment)
		if err != nil {
			return fmt.Errorf("packet assignment item_id %q option b: %w", itemID, err)
		}
		if optionA == optionB {
			return fmt.Errorf("packet assignment item_id %q options a and b must map to different baseline/candidate artifacts", itemID)
		}
	}
	return nil
}

func rankedAssignmentsMatch(left blindAssignment, right blindAssignment) bool {
	if len(left.Options) != len(right.Options) {
		return false
	}
	for index := range left.Options {
		if strings.TrimSpace(left.Options[index].Label) != strings.TrimSpace(right.Options[index].Label) ||
			strings.TrimSpace(left.Options[index].ArtifactID) != strings.TrimSpace(right.Options[index].ArtifactID) {
			return false
		}
	}
	return true
}

func rankedFeedbackEventFromEntry(runID string, itemID string, entry feedbackFileEntry, assignment blindAssignment, reviewer string, source string, sourceURL string, createdAt string) (db.RankedFeedbackEvent, error) {
	ranking, tieGroups, err := normalizedRankingWithTieGroups(entry)
	if err != nil {
		return db.RankedFeedbackEvent{}, err
	}
	if len(ranking) == 0 {
		return db.RankedFeedbackEvent{}, errors.New("ranking is required")
	}
	known := map[string]struct{}{}
	for _, option := range assignment.Options {
		label := normalizeReviewOptionLabel(option.Label)
		if label != "" {
			known[label] = struct{}{}
		}
	}
	for _, label := range ranking {
		if _, ok := known[label]; !ok {
			return db.RankedFeedbackEvent{}, fmt.Errorf("ranking references unknown option %q", label)
		}
	}
	if len(ranking) != len(known) {
		return db.RankedFeedbackEvent{}, fmt.Errorf("ranking includes %d options, want %d options", len(ranking), len(known))
	}
	for label := range known {
		found := false
		for _, ranked := range ranking {
			if ranked == label {
				found = true
				break
			}
		}
		if !found {
			return db.RankedFeedbackEvent{}, fmt.Errorf("ranking missing option %q", label)
		}
	}
	if err := validateRankedTraitLabels(entry.UsefulTraits, known); err != nil {
		return db.RankedFeedbackEvent{}, fmt.Errorf("useful_traits: %w", err)
	}
	if err := validateRankedTraitLabels(entry.RejectedTraits, known); err != nil {
		return db.RankedFeedbackEvent{}, fmt.Errorf("rejected_traits: %w", err)
	}
	rankingJSON, err := json.Marshal(ranking)
	if err != nil {
		return db.RankedFeedbackEvent{}, err
	}
	tieGroupsJSON := ""
	if rankingHasTies(tieGroups) {
		encoded, err := json.Marshal(tieGroups)
		if err != nil {
			return db.RankedFeedbackEvent{}, err
		}
		tieGroupsJSON = string(encoded)
	}
	usefulTraitsJSON, err := rankedTraitJSON(entry.UsefulTraits)
	if err != nil {
		return db.RankedFeedbackEvent{}, fmt.Errorf("useful_traits: %w", err)
	}
	rejectedTraitsJSON, err := rankedTraitJSON(entry.RejectedTraits)
	if err != nil {
		return db.RankedFeedbackEvent{}, fmt.Errorf("rejected_traits: %w", err)
	}
	requiredImprovementsJSON, err := rankedStringListJSON(entry.RequiredImprovements)
	if err != nil {
		return db.RankedFeedbackEvent{}, fmt.Errorf("required_improvements: %w", err)
	}
	winner := normalizeReviewOptionLabel(entry.Winner)
	if winner == "" {
		if len(tieGroups) > 0 && len(tieGroups[0]) == 1 {
			winner = ranking[0]
		}
	} else {
		if _, ok := known[winner]; !ok {
			return db.RankedFeedbackEvent{}, fmt.Errorf("winner references unknown option %q", winner)
		}
		if len(tieGroups) == 0 || !rankingGroupContains(tieGroups[0], winner) {
			return db.RankedFeedbackEvent{}, fmt.Errorf("winner %q is not in first ranked group", winner)
		}
	}
	quality, continueMode, promote, err := normalizeRankedFeedbackSignals(entry)
	if err != nil {
		return db.RankedFeedbackEvent{}, err
	}
	return db.RankedFeedbackEvent{
		RunID:                    strings.TrimSpace(runID),
		ItemID:                   strings.TrimSpace(itemID),
		RankingJSON:              string(rankingJSON),
		TieGroupsJSON:            tieGroupsJSON,
		Winner:                   winner,
		UsefulTraitsJSON:         usefulTraitsJSON,
		RejectedTraitsJSON:       rejectedTraitsJSON,
		RequiredImprovementsJSON: requiredImprovementsJSON,
		Quality:                  quality,
		ContinueMode:             continueMode,
		Promote:                  promote,
		Reasoning:                rankedFeedbackReasoning(entry),
		Reviewer:                 strings.TrimSpace(reviewer),
		Source:                   strings.TrimSpace(source),
		SourceURL:                strings.TrimSpace(sourceURL),
		CreatedAt:                strings.TrimSpace(createdAt),
	}, nil
}

func normalizeRankedFeedbackSignals(entry feedbackFileEntry) (string, string, string, error) {
	quality := strings.TrimSpace(strings.ToLower(entry.Quality))
	switch quality {
	case "", "poor", "acceptable", "strong":
	default:
		return "", "", "", errors.New("quality must be one of poor, acceptable, or strong")
	}
	continueMode := strings.TrimSpace(strings.ToLower(entry.ContinueMode))
	switch continueMode {
	case "", db.EvalRunModeExplore, db.EvalRunModeRefine, db.EvalRunModeDistill, db.EvalRunModeValidate:
	default:
		return "", "", "", errors.New("continue_mode must be one of explore, refine, distill, or validate")
	}
	promote := strings.TrimSpace(strings.ToLower(entry.Promote))
	switch promote {
	case "", "yes", "y", "true":
		if promote != "" {
			promote = "yes"
		}
	case "no", "n", "false":
		promote = "no"
	default:
		return "", "", "", errors.New("promote must be yes or no")
	}
	return quality, continueMode, promote, nil
}

func validateRankedTraitLabels(traits map[string][]string, known map[string]struct{}) error {
	for label := range traits {
		optionLabel := normalizeReviewOptionLabel(label)
		if optionLabel == "" {
			return errors.New("trait option label is required")
		}
		if _, ok := known[optionLabel]; !ok {
			return fmt.Errorf("references unknown option %q", optionLabel)
		}
	}
	return nil
}

func normalizedRankingWithTieGroups(entry feedbackFileEntry) ([]string, [][]string, error) {
	groups := entry.TieGroups
	if len(groups) == 0 {
		_, groups = splitRankingLabelsAndTieGroups(entry.Ranking)
	}
	ranking := make([]string, 0, len(entry.Ranking))
	tieGroups := make([][]string, 0, len(groups))
	seen := map[string]struct{}{}
	for groupIndex, group := range groups {
		if len(group) == 0 {
			return nil, nil, fmt.Errorf("ranking contains an empty tie group at position %d", groupIndex+1)
		}
		normalizedGroup := make([]string, 0, len(group))
		for _, value := range group {
			label := normalizeReviewOptionLabel(value)
			if label == "" {
				return nil, nil, errors.New("ranking contains an empty option label")
			}
			if _, ok := seen[label]; ok {
				return nil, nil, fmt.Errorf("ranking contains duplicate option label %q", label)
			}
			seen[label] = struct{}{}
			normalizedGroup = append(normalizedGroup, label)
			ranking = append(ranking, label)
		}
		tieGroups = append(tieGroups, normalizedGroup)
	}
	if len(ranking) < 2 {
		return nil, nil, errors.New("ranking must include at least two options")
	}
	return ranking, tieGroups, nil
}

func rankingHasTies(groups [][]string) bool {
	for _, group := range groups {
		if len(group) > 1 {
			return true
		}
	}
	return false
}

func rankingGroupContains(group []string, label string) bool {
	for _, value := range group {
		if value == label {
			return true
		}
	}
	return false
}

func rankedTraitJSON(traits map[string][]string) (string, error) {
	if len(traits) == 0 {
		return "", nil
	}
	normalized := map[string][]string{}
	for label, values := range traits {
		optionLabel := normalizeReviewOptionLabel(label)
		if optionLabel == "" {
			return "", errors.New("trait option label is required")
		}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				normalized[optionLabel] = append(normalized[optionLabel], value)
			}
		}
		if _, ok := normalized[optionLabel]; !ok {
			normalized[optionLabel] = []string{}
		}
	}
	content, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func rankedStringListJSON(values []string) (string, error) {
	values = trimStringList(values)
	if len(values) == 0 {
		return "", nil
	}
	content, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func rankedFeedbackReasoning(entry feedbackFileEntry) string {
	reasoning := strings.TrimSpace(entry.Reasoning)
	if reasoning != "" {
		return reasoning
	}
	if len(entry.Ranking) == 0 {
		return ""
	}
	return strings.TrimSpace(entry.Choice)
}

func normalizeReviewOptionLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

func deblindChoice(choice string, assignment blindAssignment) (string, error) {
	switch choice {
	case ChoiceTie, ChoiceNeither, ChoiceSkip:
		return choice, nil
	case ChoiceA:
		return canonicalChoiceForArtifact(assignment.A, assignment)
	case ChoiceB:
		return canonicalChoiceForArtifact(assignment.B, assignment)
	default:
		return "", fmt.Errorf("invalid feedback choice %q; use a, b, tie, neither, or skip", choice)
	}
}

func canonicalChoiceForArtifact(artifactID string, assignment blindAssignment) (string, error) {
	artifactID = strings.TrimSpace(artifactID)
	baseline := strings.TrimSpace(assignment.Baseline)
	candidate := strings.TrimSpace(assignment.Candidate)
	if baseline == "" || candidate == "" {
		return "", errors.New("assignment missing baseline or candidate mapping")
	}
	switch artifactID {
	case baseline:
		return ChoiceA, nil
	case candidate:
		return ChoiceB, nil
	default:
		return "", fmt.Errorf("assignment artifact %q is not baseline or candidate", artifactID)
	}
}

func indexMarkdown(run db.EvalRun, items []db.EvalReviewItem, assignments assignmentFile, recommendation skillopt.PhaseRecommendation) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Gitmoot Feedback Packet: %s\n\n", run.ID)
	if reviewUsesRankedOptions(run) {
		builder.WriteString("Review each item by ranking every option from best to worst.\n\n")
	} else {
		builder.WriteString("Review each item without trying to infer which option is baseline or candidate.\n\n")
	}
	builder.WriteString("## How To Review\n\n")
	builder.WriteString("1. Open this `index.md` file.\n")
	builder.WriteString("2. Open each linked item in `items/*.md`.\n")
	if reviewUsesRankedOptions(run) {
		builder.WriteString("3. Rank every option for each item in `feedback.yml`.\n")
		builder.WriteString("4. Add useful traits and rejected traits when they explain what should be kept or avoided.\n")
		builder.WriteString("5. Import the completed packet. If `reviewer` is not set in `feedback.yml`, pass `--reviewer <name>`:\n\n")
	} else {
		builder.WriteString("3. Compare Option A and Option B blind for each item.\n")
		builder.WriteString("4. Edit `feedback.yml` in this packet directory and set `reviewer`.\n")
		builder.WriteString("5. For every item, choose exactly one of: `a`, `b`, `tie`, `neither`, `skip`.\n")
		builder.WriteString("6. Add concise `reasoning` when it helps explain the choice.\n")
		builder.WriteString("7. Import the completed packet. If `reviewer` is not set in `feedback.yml`, pass `--reviewer <name>`:\n\n")
	}
	builder.WriteString("   ```sh\n")
	builder.WriteString("   gitmoot skillopt feedback markdown import --packet <packet-dir> --reviewer <name>\n")
	builder.WriteString("   ```\n\n")
	builder.WriteString("Keep `.assignments.json` untouched. It is hidden Gitmoot metadata used to preserve and validate option mappings on import.\n\n")
	writePhaseRecommendation(&builder, recommendation)
	builder.WriteString("## Items\n\n")
	for _, item := range items {
		fmt.Fprintf(&builder, "- [%s](items/%s)\n", itemTitle(item), itemFilename(item.ItemID))
	}
	if reviewUsesRankedOptions(run) {
		builder.WriteString("\n## Ranking Labels\n\n")
		assignmentsByItem := map[string]blindAssignment{}
		for _, assignment := range assignments.Items {
			assignmentsByItem[assignment.ItemID] = assignment
		}
		for _, item := range items {
			assignment := assignmentsByItem[item.ItemID]
			labels := displayOptionLabels(assignment.Options)
			fmt.Fprintf(&builder, "- %s: `%s`\n", itemTitle(item), strings.Join(labels, " > "))
		}
	}
	builder.WriteString("\n## Submit Feedback\n\n")
	builder.WriteString("After all items are filled in `feedback.yml`, run the import command above.\n")
	return builder.String()
}

func phaseRecommendationForRun(ctx context.Context, store *db.Store, run db.EvalRun, items []db.EvalReviewItem) (skillopt.PhaseRecommendation, error) {
	feedbackEvents, err := store.ListFeedbackEvents(ctx, run.ID)
	if err != nil {
		return skillopt.PhaseRecommendation{}, err
	}
	rankedEvents, err := store.ListRankedFeedbackEvents(ctx, run.ID)
	if err != nil {
		return skillopt.PhaseRecommendation{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, run.ID)
	if err != nil {
		return skillopt.PhaseRecommendation{}, err
	}
	return skillopt.RecommendPhaseForItems(run, items, feedbackEvents, rankedEvents, pairwisePreferences), nil
}

func writePhaseRecommendation(builder *strings.Builder, recommendation skillopt.PhaseRecommendation) {
	builder.WriteString("## Phase Recommendation\n\n")
	fmt.Fprintf(builder, "- Current mode: `%s`\n", recommendation.CurrentMode)
	if recommendation.FeedbackCount > 0 {
		builder.WriteString("- Outcome recommendation is hidden in blind review packets after feedback exists to avoid biasing reviewers.\n")
		builder.WriteString("- Run `gitmoot skillopt review status --run <run-id>` after importing feedback to inspect the full recommendation.\n\n")
		return
	}
	fmt.Fprintf(builder, "- Ranking stability: `%s`\n", recommendation.RankingStability)
	fmt.Fprintf(builder, "- Recommended next mode: `%s`\n", recommendation.RecommendedMode)
	fmt.Fprintf(builder, "- %s\n\n", recommendation.Summary())
}

func displayOptionLabels(options []blindOptionAssignment) []string {
	labels := make([]string, 0, len(options))
	for _, option := range options {
		labels = append(labels, displayOptionLabel(option.Label))
	}
	return labels
}

func displayOptionLabel(label string) string {
	label = strings.TrimSpace(label)
	if len(label) == 1 {
		return strings.ToUpper(label)
	}
	return label
}

func writeOptionReferenceTable(builder *strings.Builder, options []blindOptionAssignment) {
	writeOptionReferenceTableWithLocalPaths(builder, options, true)
}

func writeGitHubOptionReferenceTable(builder *strings.Builder, options []blindOptionAssignment) {
	writeOptionReferenceTableWithLocalPaths(builder, options, false)
}

func writeOptionReferenceTableWithLocalPaths(builder *strings.Builder, options []blindOptionAssignment, includeLocalPaths bool) {
	builder.WriteString("| Option | Artifact | Reference |\n")
	builder.WriteString("| --- | --- | --- |\n")
	for _, option := range options {
		fmt.Fprintf(builder, "| %s | `%s` | %s |\n", displayOptionLabel(option.Label), option.ArtifactID, optionReference(option, includeLocalPaths))
	}
	builder.WriteString("\n")
}

func optionReference(option blindOptionAssignment, includeLocalPaths bool) string {
	metadata := optionMetadata(option)
	for _, key := range []string{"preview_url", "url"} {
		if value := metadataString(metadata, key); value != "" {
			label := "open"
			switch strings.ToLower(strings.TrimSpace(metadataString(metadata, "preview_status"))) {
			case "pending":
				label = "pending deployment"
			case "failed":
				label = "failed deployment"
			case "stale":
				label = "stale deployment"
			}
			if reason := metadataString(metadata, "preview_status_reason"); reason != "" && label != "open" {
				return fmt.Sprintf("[%s](%s) (%s)", label, value, strings.ReplaceAll(reason, "\n", " "))
			}
			return fmt.Sprintf("[%s](%s)", label, value)
		}
	}
	if path := metadataString(metadata, "path"); includeLocalPaths && path != "" {
		return "`" + strings.ReplaceAll(path, "`", "") + "`"
	}
	return ""
}

func optionHasPreviewBundleWithoutReference(option blindOptionAssignment) bool {
	if !optionHasPreviewBundle(option) {
		return false
	}
	metadata := optionMetadata(option)
	for _, key := range []string{"preview_url", "url"} {
		if metadataString(metadata, key) != "" {
			return false
		}
	}
	return true
}

func optionHasPreviewBundle(option blindOptionAssignment) bool {
	metadata := optionMetadata(option)
	if metadata["preview_bundle"] == nil {
		return false
	}
	return true
}

func missingPreviewURLForBundleError(itemID string, label string) error {
	return fmt.Errorf("item %s option %s has a preview bundle but no preview_url; publish previews before publishing a feedback packet", itemID, displayOptionLabel(label))
}

func optionMetadata(option blindOptionAssignment) map[string]any {
	metadata := map[string]any{}
	if strings.TrimSpace(option.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(option.MetadataJSON), &metadata)
	}
	return metadata
}

func metadataString(metadata map[string]any, key string) string {
	switch value := metadata[key].(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func writeFeedbackYAML(path string, runID string, assignments assignmentFile) error {
	packet := feedbackFile{RunID: runID, Reviewer: ""}
	for _, assignment := range assignments.Items {
		if len(assignment.Options) > 0 {
			packet.Items = append(packet.Items, feedbackFileEntry{
				ItemID:         assignment.ItemID,
				Ranking:        []string{"<replace with ranked option labels, best to worst>"},
				Winner:         "",
				UsefulTraits:   map[string][]string{},
				RejectedTraits: map[string][]string{},
				Quality:        "",
				ContinueMode:   "",
				Promote:        "",
				Reasoning:      "",
			})
			continue
		}
		packet.Items = append(packet.Items, feedbackFileEntry{
			ItemID:    assignment.ItemID,
			Choice:    "",
			Reasoning: "",
		})
	}
	content, err := yaml.Marshal(packet)
	if err != nil {
		return fmt.Errorf("encode feedback.yml: %w", err)
	}
	return writeTextFile(path, string(content), 0o644)
}

func readFeedbackYAML(path string) (feedbackFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return feedbackFile{}, fmt.Errorf("read feedback.yml: %w", err)
	}
	var feedback feedbackFile
	if err := yaml.Unmarshal(content, &feedback); err != nil {
		return feedbackFile{}, fmt.Errorf("parse feedback.yml: %w", err)
	}
	feedback.RunID = strings.TrimSpace(feedback.RunID)
	if feedback.RunID == "" {
		return feedbackFile{}, errors.New("feedback.yml missing run_id")
	}
	feedback.Reviewer = strings.TrimSpace(feedback.Reviewer)
	normalizeFeedbackFileEntries(&feedback)
	return feedback, nil
}

func normalizeFeedbackFileEntries(feedback *feedbackFile) {
	for index := range feedback.Items {
		feedback.Items[index].ItemID = strings.TrimSpace(feedback.Items[index].ItemID)
		feedback.Items[index].Choice = strings.TrimSpace(feedback.Items[index].Choice)
		feedback.Items[index].Ranking, feedback.Items[index].TieGroups = splitRankingLabelsAndTieGroups(feedback.Items[index].Ranking)
		feedback.Items[index].Winner = strings.TrimSpace(feedback.Items[index].Winner)
		feedback.Items[index].UsefulTraits = trimTraitMap(feedback.Items[index].UsefulTraits)
		feedback.Items[index].RejectedTraits = trimTraitMap(feedback.Items[index].RejectedTraits)
		feedback.Items[index].RequiredImprovements = trimStringList(feedback.Items[index].RequiredImprovements)
		feedback.Items[index].Quality = strings.TrimSpace(feedback.Items[index].Quality)
		feedback.Items[index].ContinueMode = strings.TrimSpace(feedback.Items[index].ContinueMode)
		feedback.Items[index].Promote = strings.TrimSpace(feedback.Items[index].Promote)
		feedback.Items[index].Reasoning = strings.TrimSpace(feedback.Items[index].Reasoning)
	}
}

func feedbackImportErrorHint(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if strings.Contains(message, "reasoning: |") || strings.Contains(message, "block-scalar") {
		return err
	}
	if strings.Contains(message, "yaml") || strings.Contains(message, "mapping values are not allowed") || strings.Contains(message, "did not find expected") || strings.Contains(message, "ranking") {
		return fmt.Errorf("%w; for long reasoning or text containing colons, use block-scalar YAML like `reasoning: |`", err)
	}
	return err
}

func readAssignments(path string) (assignmentFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return assignmentFile{}, fmt.Errorf("read assignments: %w", err)
	}
	var assignments assignmentFile
	if err := json.Unmarshal(content, &assignments); err != nil {
		return assignmentFile{}, fmt.Errorf("parse assignments: %w", err)
	}
	assignments.RunID = strings.TrimSpace(assignments.RunID)
	if assignments.RunID == "" {
		return assignmentFile{}, errors.New("assignments missing run_id")
	}
	return assignments, nil
}

func writeJSONFile(path string, value any, perm os.FileMode) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	content = append(content, '\n')
	return os.WriteFile(path, content, perm)
}

func writeTextFile(path string, content string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return os.WriteFile(path, []byte(content), perm)
}

func writeMarkdownFence(builder *strings.Builder, content string) {
	fence := markdownFenceFor(content)
	builder.WriteString(fence)
	builder.WriteString("text\n")
	builder.WriteString(strings.TrimRight(content, "\n"))
	builder.WriteString("\n")
	builder.WriteString(fence)
	builder.WriteString("\n")
}

func markdownFenceFor(content string) string {
	maxRun := 0
	current := 0
	for _, r := range content {
		if r == '`' {
			current++
			if current > maxRun {
				maxRun = current
			}
			continue
		}
		current = 0
	}
	if maxRun < 3 {
		maxRun = 3
	} else {
		maxRun++
	}
	return strings.Repeat("`", maxRun)
}

func itemTitle(item db.EvalReviewItem) string {
	if strings.TrimSpace(item.Title) != "" {
		return strings.TrimSpace(item.Title)
	}
	return item.ItemID
}

func safeItemFilename(itemID string) string {
	itemID = strings.TrimSpace(itemID)
	var builder strings.Builder
	for _, r := range itemID {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	value := strings.Trim(builder.String(), "._-")
	if value == "" {
		return "item"
	}
	return value
}

func itemFilename(itemID string) string {
	sum := sha256.Sum256([]byte(itemID))
	return fmt.Sprintf("%s-%s.md", safeItemFilename(itemID), hex.EncodeToString(sum[:])[:12])
}
