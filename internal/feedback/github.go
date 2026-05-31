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
	var builder strings.Builder
	builder.WriteString(githubFeedbackPacketMarker + "\n")
	fmt.Fprintf(&builder, "# Gitmoot SkillOpt Feedback: %s\n\n", run.ID)
	builder.WriteString("Review each item without trying to infer which option is baseline or candidate.\n\n")
	builder.WriteString("Reply in a comment with one of these formats. Allowed choices: `a`, `b`, `tie`, `neither`, `skip`.\n\n")
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

func (c GitHubCollector) Sync(ctx context.Context, store *db.Store, runID string, repo github.Repository, issueNumber int64) ([]db.FeedbackEvent, error) {
	if c.GitHub == nil {
		return nil, errors.New("github client is required")
	}
	if repo.FullName() == "" {
		return nil, errors.New("github feedback repo is required")
	}
	if issueNumber <= 0 {
		return nil, errors.New("github feedback issue or pull request number is required")
	}
	comments, err := c.GitHub.ListIssueComments(ctx, repo, issueNumber)
	if err != nil {
		return nil, err
	}
	return c.ImportComments(ctx, store, runID, comments)
}

func (c GitHubCollector) ImportComments(ctx context.Context, store *db.Store, runID string, comments []github.IssueComment) ([]db.FeedbackEvent, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, errors.New("run id is required")
	}
	items, assignments, err := c.reviewItemsAndAssignments(ctx, store, runID)
	if err != nil {
		return nil, err
	}
	knownItems := make(map[string]struct{}, len(items))
	for _, item := range items {
		knownItems[item.ItemID] = struct{}{}
	}
	var imported []db.FeedbackEvent
	for _, comment := range comments {
		parsed, ok, err := ParseGitHubFeedbackComment(comment.Body)
		if err != nil {
			return nil, fmt.Errorf("comment %d: %w", comment.ID, err)
		}
		if !ok {
			continue
		}
		if parsed.RunID == "" {
			continue
		}
		if parsed.RunID != runID {
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
		for _, entry := range parsed.Items {
			itemID := strings.TrimSpace(entry.ItemID)
			if _, ok := knownItems[itemID]; !ok {
				if parsed.ShortForm {
					continue
				}
				return nil, fmt.Errorf("comment %d: unknown feedback item_id %q", comment.ID, itemID)
			}
			choice, err := NormalizeChoice(entry.Choice)
			if err != nil {
				return nil, fmt.Errorf("comment %d item %s: %w", comment.ID, itemID, err)
			}
			choice, err = deblindChoice(choice, assignments[itemID].blindAssignment)
			if err != nil {
				return nil, fmt.Errorf("comment %d item %s: %w", comment.ID, itemID, err)
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
		imported = append(imported, commentEvents...)
	}
	for _, event := range imported {
		if err := store.UpsertFeedbackEvent(ctx, event); err != nil {
			return nil, err
		}
	}
	return imported, nil
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
		assignment, err := validatedAssignmentFor(run.ID, item)
		if err != nil {
			return nil, nil, err
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
		return feedbackFile{}, false, nil
	}
	for index := range parsed.Items {
		parsed.Items[index].ItemID = strings.TrimSpace(parsed.Items[index].ItemID)
		parsed.Items[index].Choice = strings.TrimSpace(parsed.Items[index].Choice)
		parsed.Items[index].Reasoning = strings.TrimSpace(parsed.Items[index].Reasoning)
	}
	hasFeedbackItem := false
	for _, item := range parsed.Items {
		if item.ItemID != "" {
			hasFeedbackItem = true
			break
		}
	}
	if !hasFeedbackItem {
		return feedbackFile{}, false, nil
	}
	return parsed, true, nil
}

var shortFeedbackLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*)\s*:\s*([A-Za-z]+)(?:\s*-\s*(.*))?$`)
var shortMetadataLine = regexp.MustCompile(`^(run_id|reviewer)\s*:\s*(.+)$`)

func parseShortFormFeedback(content string) (feedbackFile, bool, error) {
	parsed := feedbackFile{ShortForm: true}
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
		match := shortFeedbackLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
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
