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
	ItemID    string `json:"item_id"`
	A         string `json:"a"`
	B         string `json:"b"`
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
}

type feedbackFile struct {
	RunID     string              `yaml:"run_id"`
	Reviewer  string              `yaml:"reviewer"`
	Items     []feedbackFileEntry `yaml:"items"`
	ShortForm bool                `yaml:"-"`
}

type feedbackFileEntry struct {
	ItemID    string `yaml:"item_id"`
	Choice    string `yaml:"choice"`
	Reasoning string `yaml:"reasoning,omitempty"`
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
		assignment, err := validatedAssignmentFor(run.ID, item)
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
	if err := writeTextFile(filepath.Join(dir, "index.md"), indexMarkdown(run, items), 0o644); err != nil {
		return err
	}
	return writeFeedbackYAML(filepath.Join(dir, "feedback.yml"), run.ID, items)
}

func (c MarkdownCollector) ImportPacket(ctx context.Context, store *db.Store, dir string, reviewerOverride string) ([]db.FeedbackEvent, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("packet directory is required")
	}
	dir, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return nil, fmt.Errorf("resolve packet directory: %w", err)
	}
	assignments, err := readAssignments(filepath.Join(dir, assignmentsName))
	if err != nil {
		return nil, err
	}
	feedback, err := readFeedbackYAML(filepath.Join(dir, "feedback.yml"))
	if err != nil {
		return nil, err
	}
	if feedback.RunID != assignments.RunID {
		return nil, fmt.Errorf("feedback run_id %q does not match assignments run_id %q", feedback.RunID, assignments.RunID)
	}
	if err := validateAssignmentsAgainstStore(ctx, store, assignments); err != nil {
		return nil, err
	}
	reviewer := strings.TrimSpace(reviewerOverride)
	if reviewer == "" {
		reviewer = strings.TrimSpace(feedback.Reviewer)
	}
	if reviewer == "" {
		return nil, errors.New("feedback reviewer is required")
	}
	assignmentsByItem := map[string]blindAssignment{}
	for _, assignment := range assignments.Items {
		itemID := strings.TrimSpace(assignment.ItemID)
		if itemID == "" {
			return nil, errors.New("assignments contain an item with empty item_id")
		}
		if _, ok := assignmentsByItem[itemID]; ok {
			return nil, fmt.Errorf("assignments contain duplicate item_id %q", itemID)
		}
		assignmentsByItem[itemID] = assignment
	}
	now := c.now().UTC().Format(time.RFC3339)
	events := make([]db.FeedbackEvent, 0, len(feedback.Items))
	seenItems := map[string]struct{}{}
	for _, entry := range feedback.Items {
		itemID := strings.TrimSpace(entry.ItemID)
		assignment, ok := assignmentsByItem[itemID]
		if !ok {
			return nil, fmt.Errorf("unknown feedback item_id %q", itemID)
		}
		if _, ok := seenItems[itemID]; ok {
			return nil, fmt.Errorf("duplicate feedback item_id %q", itemID)
		}
		seenItems[itemID] = struct{}{}
		choice, err := NormalizeChoice(entry.Choice)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", itemID, err)
		}
		choice, err = deblindChoice(choice, assignment)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", itemID, err)
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
		return nil, fmt.Errorf("feedback.yml missing feedback for item_id %q", missing[0])
	}
	for _, event := range events {
		if err := store.UpsertFeedbackEvent(ctx, event); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (c MarkdownCollector) writeItem(ctx context.Context, store *db.Store, dir string, item db.EvalReviewItem, assignment blindAssignment) error {
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

func indexMarkdown(run db.EvalRun, items []db.EvalReviewItem) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Gitmoot Feedback Packet: %s\n\n", run.ID)
	builder.WriteString("Review each item without trying to infer which option is baseline or candidate.\n\n")
	builder.WriteString("## How To Review\n\n")
	builder.WriteString("1. Open this `index.md` file.\n")
	builder.WriteString("2. Open each linked item in `items/*.md`.\n")
	builder.WriteString("3. Compare Option A and Option B blind for each item.\n")
	builder.WriteString("4. Edit `feedback.yml` in this packet directory and set `reviewer`.\n")
	builder.WriteString("5. For every item, choose exactly one of: `a`, `b`, `tie`, `neither`, `skip`.\n")
	builder.WriteString("6. Add concise `reasoning` when it helps explain the choice.\n")
	builder.WriteString("7. Import the completed packet. If `reviewer` is not set in `feedback.yml`, pass `--reviewer <name>`:\n\n")
	builder.WriteString("   ```sh\n")
	builder.WriteString("   gitmoot skillopt feedback markdown import --packet <packet-dir> --reviewer <name>\n")
	builder.WriteString("   ```\n\n")
	builder.WriteString("Keep `.assignments.json` untouched. It is hidden Gitmoot metadata used to preserve and validate the blind A/B mapping on import.\n\n")
	builder.WriteString("## Items\n\n")
	for _, item := range items {
		fmt.Fprintf(&builder, "- [%s](items/%s)\n", itemTitle(item), itemFilename(item.ItemID))
	}
	builder.WriteString("\n## Submit Feedback\n\n")
	builder.WriteString("After all items are filled in `feedback.yml`, run the import command above.\n")
	return builder.String()
}

func writeFeedbackYAML(path string, runID string, items []db.EvalReviewItem) error {
	packet := feedbackFile{RunID: runID, Reviewer: ""}
	for _, item := range items {
		packet.Items = append(packet.Items, feedbackFileEntry{
			ItemID:    item.ItemID,
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
	return feedback, nil
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
