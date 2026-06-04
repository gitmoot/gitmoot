package feedback

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"gopkg.in/yaml.v3"
)

type GitHubCollector struct {
	BlobStore artifact.Store
	GitHub    github.Client
	Now       func() time.Time
}

type GitHubPublishTarget struct {
	Repo        github.Repository
	PullRequest int64
}

type GitHubPublishResult struct {
	Repo        github.Repository
	IssueNumber int64
	URL         string
	Mode        string
}

const githubFeedbackPacketMarker = "<!-- gitmoot:skillopt-feedback-packet -->"

func (c GitHubCollector) Publish(ctx context.Context, store *db.Store, runID string, target GitHubPublishTarget) (GitHubPublishResult, error) {
	if c.GitHub == nil {
		return GitHubPublishResult{}, errors.New("github client is required")
	}
	if target.Repo.FullName() == "" {
		return GitHubPublishResult{}, errors.New("github feedback repo is required")
	}
	body, err := c.Body(ctx, store, runID)
	if err != nil {
		return GitHubPublishResult{}, err
	}
	if target.PullRequest > 0 {
		comment, err := c.GitHub.PostIssueComment(ctx, target.Repo, target.PullRequest, body)
		if err != nil {
			return GitHubPublishResult{}, err
		}
		return GitHubPublishResult{Repo: target.Repo, IssueNumber: target.PullRequest, URL: comment.URL, Mode: "pr-comment"}, nil
	}
	issue, err := c.GitHub.CreateIssue(ctx, github.CreateIssueInput{
		Repo:  target.Repo,
		Title: fmt.Sprintf("Gitmoot SkillOpt feedback: %s", strings.TrimSpace(runID)),
		Body:  body,
	})
	if err != nil {
		return GitHubPublishResult{}, err
	}
	return GitHubPublishResult{Repo: target.Repo, IssueNumber: issue.Number, URL: issue.URL, Mode: "issue"}, nil
}

func (c GitHubCollector) Body(ctx context.Context, store *db.Store, runID string) (string, error) {
	if store == nil {
		return "", errors.New("store is required")
	}
	run, err := store.GetEvalRun(ctx, strings.TrimSpace(runID))
	if err != nil {
		return "", err
	}
	items, assignments, err := c.reviewItemsAndAssignments(ctx, store, run.ID)
	if err != nil {
		return "", err
	}
	recommendation, err := phaseRecommendationForRun(ctx, store, run, items)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString(githubFeedbackPacketMarker + "\n")
	fmt.Fprintf(&builder, "# Gitmoot SkillOpt Feedback: %s\n\n", run.ID)
	if reviewUsesRankedOptions(run) {
		return c.rankedBody(ctx, store, run, items, assignments, recommendation)
	}
	builder.WriteString("Review each item without trying to infer which option is baseline or candidate.\n\n")
	writePhaseRecommendation(&builder, recommendation)
	builder.WriteString("Reply by copying the fenced `yaml` block below into a new comment and filling every item. Allowed choices: `a`, `b`, `tie`, `neither`, `skip`.\n\n")
	builder.WriteString("## Copy-Paste YAML Reply\n\n")
	builder.WriteString("```yaml\n")
	yamlTemplate, err := githubFeedbackYAMLTemplate(run.ID, items)
	if err != nil {
		return "", err
	}
	builder.WriteString(yamlTemplate)
	builder.WriteString("```\n\n")
	builder.WriteString("## Short-Form Reply\n\n")
	builder.WriteString("```text\n")
	builder.WriteString("run_id: " + run.ID + "\n")
	for _, item := range items {
		builder.WriteString(item.ItemID + ": <choice> - optional reason\n")
	}
	builder.WriteString("```\n\n")
	builder.WriteString("## Items\n")
	for _, item := range items {
		assignment := assignments[item.ItemID]
		fmt.Fprintf(&builder, "\n### %s\n\n", itemTitle(item))
		builder.WriteString("#### Option A\n\n")
		writeMarkdownFence(&builder, assignment.OptionA)
		builder.WriteString("\n#### Option B\n\n")
		writeMarkdownFence(&builder, assignment.OptionB)
	}
	return builder.String(), nil
}

func (c GitHubCollector) rankedBody(ctx context.Context, store *db.Store, run db.EvalRun, items []db.EvalReviewItem, assignments map[string]githubAssignment, recommendation skillopt.PhaseRecommendation) (string, error) {
	var builder strings.Builder
	builder.WriteString(githubFeedbackPacketMarker + "\n")
	fmt.Fprintf(&builder, "# Gitmoot SkillOpt Feedback: %s\n\n", run.ID)
	if iteration, err := store.GetSkillOptTrainIterationByEvalRun(ctx, run.ID); err == nil {
		fmt.Fprintf(&builder, "- Train session: `%s`\n", iteration.SessionID)
		fmt.Fprintf(&builder, "- Train iteration: `%s`\n", iteration.ID)
	}
	builder.WriteString("- Compare each item across the option links below.\n")
	builder.WriteString("- Rank every option from best to worst using the exact labels shown.\n")
	builder.WriteString("- Reply by copying the fenced `yaml` block below into a new comment and filling every item.\n")
	builder.WriteString("- Valid `quality` values: `poor`, `acceptable`, `strong`.\n")
	builder.WriteString("- Valid `continue_mode` values: `explore`, `refine`, `distill`, `validate`.\n")
	builder.WriteString("- Valid `promote` values: `yes`, `no`.\n\n")
	writePhaseRecommendation(&builder, recommendation)
	builder.WriteString("## Review Table\n\n")
	writeGitHubRankedReviewTable(&builder, items, assignments)
	if err := c.writeUnlinkedRankedOptionContent(ctx, store, &builder, items, assignments); err != nil {
		return "", err
	}
	builder.WriteString("## Copy-Paste YAML Reply\n\n")
	builder.WriteString("```yaml\n")
	replyYAML, err := rankedReplyYAML(run.ID, items, assignments)
	if err != nil {
		return "", err
	}
	builder.WriteString(replyYAML)
	builder.WriteString("```\n\n")
	return builder.String(), nil
}

func (c GitHubCollector) writeUnlinkedRankedOptionContent(ctx context.Context, store *db.Store, builder *strings.Builder, items []db.EvalReviewItem, assignments map[string]githubAssignment) error {
	wroteHeader := false
	for _, item := range items {
		assignment := assignments[item.ItemID]
		wroteItem := false
		for _, option := range assignment.Options {
			if optionHasPreviewBundleWithoutReference(option) {
				return missingPreviewURLForBundleError(item.ItemID, option.Label)
			}
			if optionReference(option, false) != "" {
				continue
			}
			content, err := c.optionText(ctx, store, option.ArtifactID)
			if err != nil {
				return fmt.Errorf("item %s option %s: %w", item.ItemID, option.Label, err)
			}
			if !wroteHeader {
				builder.WriteString("## Inline Options Without Public Links\n\n")
				builder.WriteString("These options do not have preview URLs yet, so their content is included here to keep the review actionable.\n\n")
				wroteHeader = true
			}
			if !wroteItem {
				fmt.Fprintf(builder, "### %s\n\n", itemTitle(item))
				wroteItem = true
			}
			fmt.Fprintf(builder, "#### Option %s\n\n", displayOptionLabel(option.Label))
			writeMarkdownFence(builder, content)
			builder.WriteString("\n")
		}
	}
	return nil
}

func writeGitHubRankedReviewTable(builder *strings.Builder, items []db.EvalReviewItem, assignments map[string]githubAssignment) {
	builder.WriteString("| Item | What to compare | Options |\n")
	builder.WriteString("| --- | --- | --- |\n")
	for _, item := range items {
		assignment := assignments[item.ItemID]
		optionRefs := make([]string, 0, len(assignment.Options))
		for _, option := range assignment.Options {
			ref := optionReference(option, false)
			if ref == "" {
				ref = "`" + strings.ReplaceAll(option.ArtifactID, "`", "") + "`"
			}
			optionRefs = append(optionRefs, fmt.Sprintf("Option %s: %s", displayOptionLabel(option.Label), ref))
		}
		fmt.Fprintf(builder, "| `%s` | %s | %s |\n", item.ItemID, markdownTableCell(itemTitle(item)), markdownTableCell(strings.Join(optionRefs, "<br>")))
	}
	builder.WriteString("\n")
}

func markdownTableCell(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	if value == "" {
		return "-"
	}
	return value
}

func rankedReplyYAML(runID string, items []db.EvalReviewItem, assignments map[string]githubAssignment) (string, error) {
	type rankedReplyFile struct {
		RunID string              `yaml:"run_id"`
		Items []feedbackFileEntry `yaml:"items"`
	}
	packet := rankedReplyFile{RunID: runID}
	for _, item := range items {
		labels := displayOptionLabels(assignments[item.ItemID].Options)
		packet.Items = append(packet.Items, feedbackFileEntry{
			ItemID:       item.ItemID,
			Ranking:      []string{fmt.Sprintf("<replace with ranked option labels, e.g. %s>", strings.Join(labels, " > "))},
			Quality:      "",
			ContinueMode: "",
			Promote:      "",
			Reasoning:    "",
		})
	}
	content, err := yaml.Marshal(packet)
	if err != nil {
		return "", fmt.Errorf("encode ranked feedback reply: %w", err)
	}
	return string(content), nil
}

func (c GitHubCollector) Sync(ctx context.Context, store *db.Store, runID string, repo github.Repository, issueNumber int64) (ImportResult, error) {
	if c.GitHub == nil {
		return ImportResult{}, errors.New("github client is required")
	}
	if repo.FullName() == "" {
		return ImportResult{}, errors.New("github feedback repo is required")
	}
	if issueNumber <= 0 {
		return ImportResult{}, errors.New("github feedback issue or pull request number is required")
	}
	comments, err := c.GitHub.ListIssueComments(ctx, repo, issueNumber)
	if err != nil {
		return ImportResult{}, err
	}
	return c.ImportComments(ctx, store, runID, comments)
}

func (c GitHubCollector) ImportComments(ctx context.Context, store *db.Store, runID string, comments []github.IssueComment) (ImportResult, error) {
	if store == nil {
		return ImportResult{}, errors.New("store is required")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return ImportResult{}, errors.New("run id is required")
	}
	items, assignments, err := c.reviewItemsAndAssignments(ctx, store, runID)
	if err != nil {
		return ImportResult{}, err
	}
	knownItems := make(map[string]struct{}, len(items))
	for _, item := range items {
		knownItems[item.ItemID] = struct{}{}
	}
	var imported []db.FeedbackEvent
	var importedRanked []db.RankedFeedbackEvent
	var diagnostics []string
	if len(comments) == 0 {
		return ImportResult{}, errors.New("no comments found on GitHub issue or pull request")
	}
	for _, comment := range comments {
		parsed, ok, err := ParseGitHubFeedbackComment(comment.Body)
		if err != nil {
			return ImportResult{}, fmt.Errorf("comment %d: %w", comment.ID, feedbackImportErrorHint(err))
		}
		if !ok {
			diagnostics = append(diagnostics, fmt.Sprintf("comment %d: no parseable feedback YAML or short-form feedback", comment.ID))
			continue
		}
		if parsed.RunID == "" {
			diagnostics = append(diagnostics, fmt.Sprintf("comment %d: missing run_id", comment.ID))
			continue
		}
		if parsed.RunID != runID {
			diagnostics = append(diagnostics, fmt.Sprintf("comment %d: wrong run_id %q, want %q", comment.ID, parsed.RunID, runID))
			continue
		}
		reviewer := strings.TrimSpace(comment.Author)
		if reviewer == "" {
			reviewer = strings.TrimSpace(parsed.Reviewer)
		}
		if reviewer == "" {
			reviewer = "github"
		}
		sourceURL := strings.TrimSpace(comment.URL)
		if sourceURL == "" {
			sourceURL = fmt.Sprintf("github-comment:%d", comment.ID)
		}
		createdAt := strings.TrimSpace(comment.CreatedAt)
		if createdAt == "" {
			createdAt = c.now().UTC().Format(time.RFC3339)
		}
		commentEvents := make([]db.FeedbackEvent, 0, len(parsed.Items))
		commentRankedEvents := make([]db.RankedFeedbackEvent, 0, len(parsed.Items))
		seenKnownItem := false
		reportedMissingItemFeedback := false
		for _, entry := range parsed.Items {
			itemID := strings.TrimSpace(entry.ItemID)
			if itemID == "" {
				diagnostics = append(diagnostics, fmt.Sprintf("comment %d: missing item feedback for run %q", comment.ID, runID))
				reportedMissingItemFeedback = true
				continue
			}
			if _, ok := knownItems[itemID]; !ok {
				if parsed.ShortForm {
					diagnostics = append(diagnostics, fmt.Sprintf("comment %d: unknown feedback item_id %q", comment.ID, itemID))
					continue
				}
				return ImportResult{}, fmt.Errorf("comment %d: unknown feedback item_id %q", comment.ID, itemID)
			}
			seenKnownItem = true
			assignment := assignments[itemID].blindAssignment
			if len(assignment.Options) > 0 {
				event, err := rankedFeedbackEventFromEntry(runID, itemID, entry, assignment, reviewer, SourceGitHub, sourceURL, createdAt)
				if err != nil {
					return ImportResult{}, fmt.Errorf("comment %d item %s: %w", comment.ID, itemID, feedbackImportErrorHint(err))
				}
				commentRankedEvents = append(commentRankedEvents, event)
				continue
			}
			choice, err := NormalizeChoice(entry.Choice)
			if err != nil {
				return ImportResult{}, fmt.Errorf("comment %d item %s: %w", comment.ID, itemID, err)
			}
			choice, err = deblindChoice(choice, assignment)
			if err != nil {
				return ImportResult{}, fmt.Errorf("comment %d item %s: %w", comment.ID, itemID, err)
			}
			event := db.FeedbackEvent{
				RunID:     runID,
				ItemID:    itemID,
				Choice:    choice,
				Reasoning: strings.TrimSpace(entry.Reasoning),
				Reviewer:  reviewer,
				Source:    SourceGitHub,
				SourceURL: sourceURL,
				CreatedAt: createdAt,
			}
			commentEvents = append(commentEvents, event)
		}
		if !seenKnownItem && !reportedMissingItemFeedback {
			diagnostics = append(diagnostics, fmt.Sprintf("comment %d: missing item feedback for run %q", comment.ID, runID))
		}
		imported = append(imported, commentEvents...)
		importedRanked = append(importedRanked, commentRankedEvents...)
	}
	result := ImportResult{FeedbackEvents: imported, RankedFeedbackEvents: importedRanked, Diagnostics: diagnostics}
	if result.Count() == 0 {
		if len(diagnostics) > 0 {
			return ImportResult{}, fmt.Errorf("no github feedback imported: %s", strings.Join(diagnostics, "; "))
		}
		return ImportResult{}, errors.New("no parseable feedback YAML or short-form comments found")
	}
	for _, event := range imported {
		if err := store.UpsertFeedbackEvent(ctx, event); err != nil {
			return ImportResult{}, err
		}
	}
	for _, event := range importedRanked {
		if err := store.UpsertRankedFeedbackEvent(ctx, event); err != nil {
			return ImportResult{}, err
		}
	}
	return result, nil
}

type githubAssignment struct {
	blindAssignment
	OptionA string
	OptionB string
}

func (c GitHubCollector) reviewItemsAndAssignments(ctx context.Context, store *db.Store, runID string) ([]db.EvalReviewItem, map[string]githubAssignment, error) {
	run, err := store.GetEvalRun(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("eval run %s has no review items", run.ID)
	}
	assignments := make(map[string]githubAssignment, len(items))
	for _, item := range items {
		assignment, err := validatedAssignmentForReview(ctx, store, run, item)
		if err != nil {
			return nil, nil, err
		}
		if len(assignment.Options) > 0 {
			assignments[item.ItemID] = githubAssignment{blindAssignment: assignment}
			continue
		}
		optionA, err := c.optionText(ctx, store, assignment.A)
		if err != nil {
			return nil, nil, fmt.Errorf("item %s option a: %w", item.ItemID, err)
		}
		optionB, err := c.optionText(ctx, store, assignment.B)
		if err != nil {
			return nil, nil, fmt.Errorf("item %s option b: %w", item.ItemID, err)
		}
		assignments[item.ItemID] = githubAssignment{blindAssignment: assignment, OptionA: optionA, OptionB: optionB}
	}
	return items, assignments, nil
}

func (c GitHubCollector) optionText(ctx context.Context, store *db.Store, artifactID string) (string, error) {
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

func (c GitHubCollector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func ParseGitHubFeedbackComment(body string) (feedbackFile, bool, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return feedbackFile{}, false, nil
	}
	if isGitHubFeedbackPacket(body) {
		return feedbackFile{}, false, nil
	}
	blocks := fencedBlocks(body)
	for _, block := range blocks {
		parsed, ok, err := parseFullYAMLFeedback(block)
		if ok || err != nil {
			return parsed, ok, err
		}
	}
	if len(blocks) == 0 && !hasQuotedLines(body) {
		parsed, ok, err := parseFullYAMLFeedback(body)
		if ok || err != nil {
			return parsed, ok, err
		}
	}
	return parseShortFormFeedback(body)
}

func parseFullYAMLFeedback(content string) (feedbackFile, bool, error) {
	if !strings.Contains(content, "items:") && !strings.Contains(content, "run_id:") {
		return feedbackFile{}, false, nil
	}
	var parsed feedbackFile
	if err := yaml.Unmarshal([]byte(content), &parsed); err != nil {
		return feedbackFile{}, true, fmt.Errorf("parse feedback yaml: %w", err)
	}
	parsed.RunID = strings.TrimSpace(parsed.RunID)
	parsed.Reviewer = strings.TrimSpace(parsed.Reviewer)
	if len(parsed.Items) == 0 {
		if parsed.RunID != "" && strings.Contains(content, "items:") {
			return parsed, true, nil
		}
		return feedbackFile{}, false, nil
	}
	normalizeFeedbackFileEntries(&parsed)
	hasFeedbackItem := false
	for _, item := range parsed.Items {
		if item.ItemID != "" {
			hasFeedbackItem = true
			break
		}
	}
	if !hasFeedbackItem {
		return parsed, true, nil
	}
	return parsed, true, nil
}

var shortFeedbackLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*)\s*:\s*([A-Za-z]+)(?:\s*-\s*(.*))?$`)
var shortRankingLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*)\s+ranking\s*:\s*(.+)$`)
var shortDirectRankingLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*)\s*:\s*([A-Za-z][A-Za-z0-9._/-]*(?:\s*(?:>|,|\|)\s*[A-Za-z][A-Za-z0-9._/-]*)+)(?:\s*-\s*(.*))?$`)
var shortMetadataLine = regexp.MustCompile(`^(run_id|reviewer)\s*:\s*(.+)$`)
var shortItemSignalLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*)\s+(quality|continue_mode|promote)\s*:\s*(.+)$`)
var shortBareSignalLine = regexp.MustCompile(`^(quality|continue_mode|promote)\s*:\s*(.+)$`)
var shortTraitHeaderLine = regexp.MustCompile(`^(best traits|useful traits|reject|rejected traits)\s*:\s*$`)
var shortTraitLine = regexp.MustCompile(`^-\s*([^:]+):\s*(.+)$`)

func parseShortFormFeedback(content string) (feedbackFile, bool, error) {
	parsed := feedbackFile{ShortForm: true}
	var traitTarget string
	traitItemIndex := -1
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "```") {
			continue
		}
		if strings.HasPrefix(line, ">") {
			continue
		}
		if match := shortMetadataLine.FindStringSubmatch(line); match != nil {
			switch strings.ToLower(strings.TrimSpace(match[1])) {
			case "run_id":
				parsed.RunID = strings.TrimSpace(match[2])
			case "reviewer":
				parsed.Reviewer = strings.TrimSpace(match[2])
			}
			continue
		}
		if match := shortItemSignalLine.FindStringSubmatch(line); match != nil {
			if index := findFeedbackItemIndex(parsed.Items, strings.TrimSpace(match[1])); index >= 0 {
				setFeedbackItemSignal(&parsed.Items[index], match[2], match[3])
			}
			continue
		}
		if match := shortBareSignalLine.FindStringSubmatch(line); match != nil && traitItemIndex >= 0 {
			setFeedbackItemSignal(&parsed.Items[traitItemIndex], match[1], match[2])
			continue
		}
		if match := shortTraitHeaderLine.FindStringSubmatch(strings.ToLower(line)); match != nil {
			if traitItemIndex < 0 {
				continue
			}
			switch match[1] {
			case "best traits", "useful traits":
				traitTarget = "useful"
			case "reject", "rejected traits":
				traitTarget = "rejected"
			}
			continue
		}
		if traitTarget != "" {
			if match := shortTraitLine.FindStringSubmatch(line); match != nil && traitItemIndex >= 0 {
				label := strings.TrimSpace(match[1])
				trait := strings.TrimSpace(match[2])
				if trait != "" {
					entry := &parsed.Items[traitItemIndex]
					if traitTarget == "useful" {
						if entry.UsefulTraits == nil {
							entry.UsefulTraits = map[string][]string{}
						}
						entry.UsefulTraits[label] = append(entry.UsefulTraits[label], trait)
					} else {
						if entry.RejectedTraits == nil {
							entry.RejectedTraits = map[string][]string{}
						}
						entry.RejectedTraits[label] = append(entry.RejectedTraits[label], trait)
					}
				}
				continue
			}
			traitTarget = ""
			traitItemIndex = -1
		}
		if match := shortRankingLine.FindStringSubmatch(line); match != nil {
			parsed.Items = append(parsed.Items, feedbackFileEntry{
				ItemID:  strings.TrimSpace(match[1]),
				Ranking: splitRankingString(match[2]),
			})
			traitItemIndex = len(parsed.Items) - 1
			traitTarget = ""
			continue
		}
		if match := shortDirectRankingLine.FindStringSubmatch(line); match != nil {
			parsed.Items = append(parsed.Items, feedbackFileEntry{
				ItemID:    strings.TrimSpace(match[1]),
				Ranking:   splitRankingString(match[2]),
				Reasoning: strings.TrimSpace(match[3]),
			})
			traitItemIndex = len(parsed.Items) - 1
			traitTarget = ""
			continue
		}
		match := shortFeedbackLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		traitTarget = ""
		traitItemIndex = -1
		parsed.Items = append(parsed.Items, feedbackFileEntry{
			ItemID:    strings.TrimSpace(match[1]),
			Choice:    strings.TrimSpace(match[2]),
			Reasoning: strings.TrimSpace(match[3]),
		})
	}
	if len(parsed.Items) == 0 {
		return feedbackFile{}, false, nil
	}
	return parsed, true, nil
}

func findFeedbackItemIndex(items []feedbackFileEntry, itemID string) int {
	itemID = strings.TrimSpace(itemID)
	for index := range items {
		if strings.TrimSpace(items[index].ItemID) == itemID {
			return index
		}
	}
	return -1
}

func setFeedbackItemSignal(entry *feedbackFileEntry, key string, value string) {
	value = strings.TrimSpace(value)
	switch strings.TrimSpace(strings.ToLower(key)) {
	case "quality":
		entry.Quality = value
	case "continue_mode":
		entry.ContinueMode = value
	case "promote":
		entry.Promote = value
	}
}

func splitRankingString(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '>' || r == ',' || r == '|'
	})
	return splitRankingLabels(parts)
}

func splitRankingLabels(values []string) []string {
	labels := make([]string, 0, len(values))
	for _, value := range values {
		parts := strings.FieldsFunc(value, func(r rune) bool {
			return r == '>' || r == ',' || r == '|'
		})
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				labels = append(labels, part)
			}
		}
	}
	return labels
}

func trimTraitMap(values map[string][]string) map[string][]string {
	if len(values) == 0 {
		return nil
	}
	trimmed := map[string][]string{}
	for key, traits := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		for _, trait := range traits {
			trait = strings.TrimSpace(trait)
			if trait != "" {
				trimmed[key] = append(trimmed[key], trait)
			}
		}
	}
	if len(trimmed) == 0 {
		return nil
	}
	return trimmed
}

func trimStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			trimmed = append(trimmed, value)
		}
	}
	if len(trimmed) == 0 {
		return nil
	}
	return trimmed
}

func cloneTraitMap(values map[string][]string) map[string][]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string][]string, len(values))
	for key, traits := range values {
		clone[key] = append([]string(nil), traits...)
	}
	return clone
}

func fencedBlocks(content string) []string {
	var blocks []string
	var builder strings.Builder
	inFence := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			if inFence {
				blocks = append(blocks, builder.String())
				builder.Reset()
				inFence = false
				continue
			}
			inFence = true
			continue
		}
		if inFence {
			builder.WriteString(raw)
			builder.WriteByte('\n')
		}
	}
	return blocks
}

func isGitHubFeedbackPacket(body string) bool {
	body = strings.TrimSpace(body)
	return strings.Contains(body, githubFeedbackPacketMarker) ||
		strings.HasPrefix(body, "# Gitmoot SkillOpt Feedback:")
}

func hasQuotedLines(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			return true
		}
	}
	return false
}

func githubFeedbackYAMLTemplate(runID string, items []db.EvalReviewItem) (string, error) {
	packet := feedbackFile{RunID: runID}
	for _, item := range items {
		packet.Items = append(packet.Items, feedbackFileEntry{
			ItemID:    item.ItemID,
			Choice:    "",
			Reasoning: "",
		})
	}
	content, err := yaml.Marshal(packet)
	if err != nil {
		return "", fmt.Errorf("encode github feedback yaml: %w", err)
	}
	return string(content), nil
}
