package feedback

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestMarkdownCollectorWriteAndImportPacket(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	blobStore := artifact.NewStore(filepath.Join(t.TempDir(), "blobs"))
	baseline, err := blobStore.Put([]byte("baseline answer\n```go\nfmt.Println(\"baseline\")\n```"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidate, err := blobStore.Put([]byte("candidate answer"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "baseline", Hash: baseline.Hash, MediaType: "text/markdown", SizeBytes: baseline.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "candidate", Hash: candidate.Hash, MediaType: "text/markdown", SizeBytes: candidate.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{ID: "run-1", State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:               "run-1",
		ItemID:              "item-001",
		Title:               "Item One",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:               "run-1",
		ItemID:              "x",
		Title:               "Swapped Item",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem swapped returned error: %v", err)
	}
	packetDir := filepath.Join(t.TempDir(), "packet")
	collector := MarkdownCollector{
		BlobStore: blobStore,
		Now: func() time.Time {
			return time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
		},
	}
	if err := collector.WritePacket(ctx, store, "run-1", packetDir); err != nil {
		t.Fatalf("WritePacket returned error: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(packetDir, "index.md"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if !strings.Contains(string(index), "Open each linked item") ||
		!strings.Contains(string(index), "set `reviewer`") ||
		!strings.Contains(string(index), "## Phase Recommendation") ||
		!strings.Contains(string(index), "recommend continue validate") ||
		!strings.Contains(string(index), "gitmoot skillopt feedback markdown import --packet <packet-dir> --reviewer <name>") ||
		!strings.Contains(string(index), ".assignments.json") {
		t.Fatalf("index content misses instructions:\n%s", string(index))
	}
	itemContent, err := os.ReadFile(filepath.Join(packetDir, "items", itemFilename("item-001")))
	if err != nil {
		t.Fatalf("read item: %v", err)
	}
	if !strings.Contains(string(itemContent), "Option A") || !strings.Contains(string(itemContent), "Option B") {
		t.Fatalf("item content missing options:\n%s", string(itemContent))
	}
	if !strings.Contains(string(itemContent), "../feedback.yml") {
		t.Fatalf("item content missing feedback.yml guidance:\n%s", string(itemContent))
	}
	if !strings.Contains(string(itemContent), "````text") || !strings.Contains(string(itemContent), "```go") {
		t.Fatalf("item content does not preserve nested Markdown fences:\n%s", string(itemContent))
	}
	assignments, err := os.ReadFile(filepath.Join(packetDir, assignmentsName))
	if err != nil {
		t.Fatalf("read assignments: %v", err)
	}
	if !strings.Contains(string(assignments), `"baseline"`) || !strings.Contains(string(assignments), `"candidate"`) {
		t.Fatalf("assignments do not preserve mapping:\n%s", string(assignments))
	}
	feedbackContent := `run_id: run-1
reviewer: jerry
items:
    - item_id: item-001
      choice: b
      reasoning: More concrete.
    - item_id: x
      choice: a
`
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackContent), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}
	result, err := collector.ImportPacket(ctx, store, packetDir+string(os.PathSeparator)+".", "")
	if err != nil {
		t.Fatalf("ImportPacket returned error: %v", err)
	}
	events := result.FeedbackEvents
	if len(events) != 2 || events[0].Choice != "b" || events[1].Choice != "b" || events[0].CreatedAt != "2026-05-31T10:00:00Z" {
		t.Fatalf("events = %+v", events)
	}
	stored, err := store.ListFeedbackEvents(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 2 || stored[0].Reasoning != "More concrete." || stored[1].Reasoning != "" || stored[0].Source != SourceMarkdown {
		t.Fatalf("stored events = %+v", stored)
	}
	if _, err := collector.ImportPacket(ctx, store, packetDir, ""); err != nil {
		t.Fatalf("second ImportPacket returned error: %v", err)
	}
	stored, err = store.ListFeedbackEvents(ctx, "run-1")
	if err != nil {
		t.Fatalf("second ListFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("second import duplicated feedback events: %+v", stored)
	}
}

func TestMarkdownCollectorImportsRankedPacket(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupMarkdownRankedFeedbackRun(t, "ranked-1")
	packetDir := filepath.Join(t.TempDir(), "packet")
	collector := MarkdownCollector{
		BlobStore: blobs,
		Now: func() time.Time {
			return time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
		},
	}
	if err := collector.WritePacket(ctx, store, "ranked-1", packetDir); err != nil {
		t.Fatalf("WritePacket returned error: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(packetDir, "index.md"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if !strings.Contains(string(index), "## Phase Recommendation") || !strings.Contains(string(index), "recommend continue explore") {
		t.Fatalf("ranked index missing phase recommendation:\n%s", string(index))
	}
	if _, err := collector.ImportPacket(ctx, store, packetDir, "jerry"); err == nil || !strings.Contains(err.Error(), "ranking") {
		t.Fatalf("unchanged ranked packet import error = %v", err)
	}
	feedbackContent := `run_id: ranked-1
reviewer: jerry
items:
  - item_id: item-001
    ranking:
      - C > A > D > B
    useful_traits:
      A:
        - visual style
      D:
        - motion
    rejected_traits:
      B:
        - too generic
    required_improvements:
      - stronger branding
      - richer animation
    quality: poor
    continue_mode: explore
    promote: no
    choice: C explains the product best and still needs more visual polish.
`
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackContent), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}
	result, err := collector.ImportPacket(ctx, store, packetDir, "")
	if err != nil {
		t.Fatalf("ImportPacket returned error: %v", err)
	}
	if result.Count() != 1 || len(result.RankedFeedbackEvents) != 1 {
		t.Fatalf("result = %+v", result)
	}
	stored, err := store.ListRankedFeedbackEvents(ctx, "ranked-1")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 1 || stored[0].Winner != "c" || stored[0].Reasoning != "C explains the product best and still needs more visual polish." || stored[0].Source != SourceMarkdown {
		t.Fatalf("stored ranked events = %+v", stored)
	}
	if stored[0].Quality != "poor" || stored[0].ContinueMode != "explore" || stored[0].Promote != "no" {
		t.Fatalf("stored ranked event signals = %+v", stored[0])
	}
	if !strings.Contains(stored[0].UsefulTraitsJSON, `"a":["visual style"]`) || !strings.Contains(stored[0].RejectedTraitsJSON, `"b":["too generic"]`) {
		t.Fatalf("stored traits useful=%s rejected=%s", stored[0].UsefulTraitsJSON, stored[0].RejectedTraitsJSON)
	}
	if !strings.Contains(stored[0].RequiredImprovementsJSON, "stronger branding") || !strings.Contains(stored[0].RequiredImprovementsJSON, "richer animation") {
		t.Fatalf("stored required improvements = %s", stored[0].RequiredImprovementsJSON)
	}
	pairs, err := store.ListPairwisePreferences(ctx, "ranked-1")
	if err != nil {
		t.Fatalf("ListPairwisePreferences returned error: %v", err)
	}
	if len(pairs) != 6 || pairs[0].Preferred != "c" || pairs[0].Rejected != "a" || pairs[5].Preferred != "d" || pairs[5].Rejected != "b" {
		t.Fatalf("pairwise preferences = %+v", pairs)
	}
	ranking, err := json.Marshal([]string{"c", "a", "d", "b"})
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(ctx, db.RankedFeedbackEvent{
		RunID:       "ranked-1",
		ItemID:      "item-001",
		RankingJSON: string(ranking),
		Winner:      "c",
		Reviewer:    "second-reviewer",
		Source:      SourceMarkdown,
		SourceURL:   packetDir + "#second",
		CreatedAt:   "2026-06-02T11:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent second returned error: %v", err)
	}
	secondPacketDir := filepath.Join(t.TempDir(), "packet-with-feedback")
	if err := collector.WritePacket(ctx, store, "ranked-1", secondPacketDir); err != nil {
		t.Fatalf("WritePacket with feedback returned error: %v", err)
	}
	secondIndex, err := os.ReadFile(filepath.Join(secondPacketDir, "index.md"))
	if err != nil {
		t.Fatalf("read second index: %v", err)
	}
	if !strings.Contains(string(secondIndex), "Outcome recommendation is hidden") ||
		strings.Contains(string(secondIndex), "recommend refine") ||
		strings.Contains(string(secondIndex), "top option") ||
		strings.Contains(string(secondIndex), "Ranking stability") ||
		strings.Contains(string(secondIndex), "Recommended next mode") {
		t.Fatalf("ranked index leaks outcome recommendation:\n%s", string(secondIndex))
	}
}

func TestMarkdownCollectorRejectsInvalidRankedWinnerWithoutPartialImport(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupMarkdownRankedFeedbackRun(t, "ranked-1")
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:  "ranked-1",
		ItemID: "item-002",
		Title:  "Second Ranked Item",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem item-002 returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{RunID: "ranked-1", ItemID: "item-002", Label: label, ArtifactID: "option-" + label, Role: "option"}); err != nil {
			t.Fatalf("UpsertEvalReviewOption item-002 %s returned error: %v", label, err)
		}
	}
	packetDir := filepath.Join(t.TempDir(), "packet")
	collector := MarkdownCollector{BlobStore: blobs}
	if err := collector.WritePacket(ctx, store, "ranked-1", packetDir); err != nil {
		t.Fatalf("WritePacket returned error: %v", err)
	}
	feedbackContent := `run_id: ranked-1
reviewer: jerry
items:
  - item_id: item-001
    ranking:
      - C > A > D > B
  - item_id: item-002
    ranking:
      - C > A > D > B
    winner: B
`
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackContent), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}
	_, err := collector.ImportPacket(ctx, store, packetDir, "")
	if err == nil || !strings.Contains(err.Error(), "winner") {
		t.Fatalf("ImportPacket error = %v", err)
	}
	stored, err := store.ListRankedFeedbackEvents(ctx, "ranked-1")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("ImportPacket persisted partial ranked events: %+v", stored)
	}
}

func TestMarkdownCollectorRejectsInvalidRankedSignalWithoutPartialImport(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupMarkdownRankedFeedbackRun(t, "ranked-1")
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:  "ranked-1",
		ItemID: "item-002",
		Title:  "Second Ranked Item",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem item-002 returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{RunID: "ranked-1", ItemID: "item-002", Label: label, ArtifactID: "option-" + label, Role: "option"}); err != nil {
			t.Fatalf("UpsertEvalReviewOption item-002 %s returned error: %v", label, err)
		}
	}
	packetDir := filepath.Join(t.TempDir(), "packet")
	collector := MarkdownCollector{BlobStore: blobs}
	if err := collector.WritePacket(ctx, store, "ranked-1", packetDir); err != nil {
		t.Fatalf("WritePacket returned error: %v", err)
	}
	feedbackContent := `run_id: ranked-1
reviewer: jerry
items:
  - item_id: item-001
    ranking:
      - C > A > D > B
    quality: poor
  - item_id: item-002
    ranking:
      - C > A > D > B
    quality: ok
`
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackContent), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}
	_, err := collector.ImportPacket(ctx, store, packetDir, "")
	if err == nil || !strings.Contains(err.Error(), "quality") {
		t.Fatalf("ImportPacket error = %v", err)
	}
	stored, err := store.ListRankedFeedbackEvents(ctx, "ranked-1")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("ImportPacket persisted partial ranked events: %+v", stored)
	}
}

func TestMarkdownCollectorRejectsInvalidFeedback(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertEvalRun(ctx, db.EvalRun{ID: "run-1", State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	for _, itemID := range []string{"item-001", "item-002"} {
		if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
			RunID:               "run-1",
			ItemID:              itemID,
			BaselineArtifactID:  "baseline",
			CandidateArtifactID: "candidate",
		}); err != nil {
			t.Fatalf("UpsertEvalReviewItem %s returned error: %v", itemID, err)
		}
	}
	packetDir := t.TempDir()
	assignments := `{"run_id":"run-1","items":[{"item_id":"item-001","a":"baseline","b":"candidate","baseline":"baseline","candidate":"candidate"},{"item_id":"item-002","a":"baseline","b":"candidate","baseline":"baseline","candidate":"candidate"}]}`
	if err := os.WriteFile(filepath.Join(packetDir, assignmentsName), []byte(assignments), 0o600); err != nil {
		t.Fatalf("write assignments: %v", err)
	}
	tests := map[string]string{
		"unknown item":   "run_id: run-1\nreviewer: jerry\nitems:\n  - item_id: item-001\n    choice: a\n  - item_id: item-404\n    choice: a\n",
		"bad choice":     "run_id: run-1\nreviewer: jerry\nitems:\n  - item_id: item-001\n    choice: yes\n",
		"missing item":   "run_id: run-1\nreviewer: jerry\nitems:\n  - item_id: item-001\n    choice: a\n",
		"duplicate item": "run_id: run-1\nreviewer: jerry\nitems:\n  - item_id: item-001\n    choice: a\n  - item_id: item-001\n    choice: b\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(content), 0o644); err != nil {
				t.Fatalf("write feedback.yml: %v", err)
			}
			_, err := (MarkdownCollector{}).ImportPacket(ctx, store, packetDir, "")
			if err == nil {
				t.Fatal("ImportPacket returned nil error")
			}
			events, err := store.ListFeedbackEvents(ctx, "run-1")
			if err != nil {
				t.Fatalf("ListFeedbackEvents returned error: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("ImportPacket persisted partial events after validation error: %+v", events)
			}
		})
	}

	t.Run("corrupt assignment permutation", func(t *testing.T) {
		corruptAssignments := `{"run_id":"run-1","items":[{"item_id":"item-001","a":"baseline","b":"baseline","baseline":"baseline","candidate":"candidate"},{"item_id":"item-002","a":"baseline","b":"candidate","baseline":"baseline","candidate":"candidate"}]}`
		if err := os.WriteFile(filepath.Join(packetDir, assignmentsName), []byte(corruptAssignments), 0o600); err != nil {
			t.Fatalf("write assignments: %v", err)
		}
		content := "run_id: run-1\nreviewer: jerry\nitems:\n  - item_id: item-001\n    choice: a\n  - item_id: item-002\n    choice: b\n"
		if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(content), 0o644); err != nil {
			t.Fatalf("write feedback.yml: %v", err)
		}
		_, err := (MarkdownCollector{}).ImportPacket(ctx, store, packetDir, "")
		if err == nil || !strings.Contains(err.Error(), "options a and b must map to different") {
			t.Fatalf("ImportPacket error = %v", err)
		}
		events, err := store.ListFeedbackEvents(ctx, "run-1")
		if err != nil {
			t.Fatalf("ListFeedbackEvents returned error: %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("ImportPacket persisted partial events after corrupt assignment: %+v", events)
		}
	})
}

func TestMarkdownCollectorRejectsIncompleteABItems(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertEvalRun(ctx, db.EvalRun{ID: "run-1", State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:              "run-1",
		ItemID:             "item-001",
		BaselineArtifactID: "baseline",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	err = (MarkdownCollector{}).WritePacket(ctx, store, "run-1", filepath.Join(t.TempDir(), "packet"))
	if err == nil || !strings.Contains(err.Error(), "requires both baseline and candidate artifacts") {
		t.Fatalf("WritePacket error = %v", err)
	}
}

func TestMarkdownCollectorRejectsIdenticalABItems(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertEvalRun(ctx, db.EvalRun{ID: "run-1", State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:               "run-1",
		ItemID:              "item-001",
		BaselineArtifactID:  "same",
		CandidateArtifactID: "same",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	err = (MarkdownCollector{}).WritePacket(ctx, store, "run-1", filepath.Join(t.TempDir(), "packet"))
	if err == nil || !strings.Contains(err.Error(), "requires distinct baseline and candidate artifacts") {
		t.Fatalf("WritePacket error = %v", err)
	}

	packetDir := t.TempDir()
	assignments := `{"run_id":"run-1","items":[{"item_id":"item-001","a":"same","b":"same","baseline":"same","candidate":"same"}]}`
	if err := os.WriteFile(filepath.Join(packetDir, assignmentsName), []byte(assignments), 0o600); err != nil {
		t.Fatalf("write assignments: %v", err)
	}
	feedbackYAML := "run_id: run-1\nreviewer: jerry\nitems:\n  - item_id: item-001\n    choice: tie\n"
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackYAML), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}
	_, err = (MarkdownCollector{}).ImportPacket(ctx, store, packetDir, "")
	if err == nil || !strings.Contains(err.Error(), "stored baseline and candidate artifacts must be distinct") {
		t.Fatalf("ImportPacket error = %v", err)
	}
}

func TestMarkdownCollectorRejectsPreviewBundleWithoutURL(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupMarkdownRankedFeedbackRun(t, "ranked-1")
	bundle, err := blobs.Put([]byte(`{"renderer":"vue-vite"}`))
	if err != nil {
		t.Fatalf("Put preview bundle returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "option-a", Hash: bundle.Hash, MediaType: "application/json", SizeBytes: bundle.Size, Driver: "vue-vite"}); err != nil {
		t.Fatalf("UpsertEvalArtifact option-a returned error: %v", err)
	}
	if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{
		RunID:        "ranked-1",
		ItemID:       "item-001",
		Label:        "a",
		ArtifactID:   "option-a",
		Role:         "option",
		MetadataJSON: `{"preview_bundle":{"renderer":"vue-vite","files":["package.json"]}}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewOption option-a returned error: %v", err)
	}
	collector := MarkdownCollector{BlobStore: blobs}

	err = collector.WritePacket(ctx, store, "ranked-1", filepath.Join(t.TempDir(), "packet"))
	if err == nil || !strings.Contains(err.Error(), "preview_url") {
		t.Fatalf("WritePacket error = %v, want preview_url", err)
	}
}

func TestMarkdownCollectorLinksPreviewBundleWithURL(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupMarkdownRankedFeedbackRun(t, "ranked-1")
	bundle, err := blobs.Put([]byte(`{"renderer":"vue-vite"}`))
	if err != nil {
		t.Fatalf("Put preview bundle returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "option-a", Hash: bundle.Hash, MediaType: "application/json", SizeBytes: bundle.Size, Driver: "vue-vite"}); err != nil {
		t.Fatalf("UpsertEvalArtifact option-a returned error: %v", err)
	}
	if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{
		RunID:        "ranked-1",
		ItemID:       "item-001",
		Label:        "a",
		ArtifactID:   "option-a",
		Role:         "option",
		MetadataJSON: `{"preview_url":"https://example.com/a","preview_bundle":{"renderer":"vue-vite","files":["package.json"]}}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewOption option-a returned error: %v", err)
	}
	packetDir := filepath.Join(t.TempDir(), "packet")
	collector := MarkdownCollector{BlobStore: blobs}

	if err := collector.WritePacket(ctx, store, "ranked-1", packetDir); err != nil {
		t.Fatalf("WritePacket returned error: %v", err)
	}
	itemContent, err := os.ReadFile(filepath.Join(packetDir, "items", itemFilename("item-001")))
	if err != nil {
		t.Fatalf("read ranked item: %v", err)
	}
	if !strings.Contains(string(itemContent), "Reference: [open](https://example.com/a)") {
		t.Fatalf("ranked item missing preview reference:\n%s", string(itemContent))
	}
	if strings.Contains(string(itemContent), `"renderer":"vue-vite"`) {
		t.Fatalf("ranked item inlined preview bundle:\n%s", string(itemContent))
	}
}

func TestMarkdownCollectorRejectsPacketWithoutStoredRun(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	packetDir := t.TempDir()
	assignments := `{"run_id":"run-1","items":[{"item_id":"item-001","a":"baseline","b":"candidate","baseline":"baseline","candidate":"candidate"}]}`
	if err := os.WriteFile(filepath.Join(packetDir, assignmentsName), []byte(assignments), 0o600); err != nil {
		t.Fatalf("write assignments: %v", err)
	}
	feedbackYAML := "run_id: run-1\nreviewer: jerry\nitems:\n  - item_id: item-001\n    choice: a\n"
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackYAML), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}
	_, err = (MarkdownCollector{}).ImportPacket(ctx, store, packetDir, "")
	if err == nil || !strings.Contains(err.Error(), "load eval run run-1") {
		t.Fatalf("ImportPacket error = %v", err)
	}
}

func TestMarkdownCollectorUsesCollisionSafeItemFilenames(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	blobStore := artifact.NewStore(filepath.Join(t.TempDir(), "blobs"))
	baseline, err := blobStore.Put([]byte("baseline answer"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidate, err := blobStore.Put([]byte("candidate answer"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "baseline", Hash: baseline.Hash, MediaType: "text/markdown", SizeBytes: baseline.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "candidate", Hash: candidate.Hash, MediaType: "text/markdown", SizeBytes: candidate.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{ID: "run-1", State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	for _, itemID := range []string{"foo/bar", "foo:bar"} {
		if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
			RunID:               "run-1",
			ItemID:              itemID,
			BaselineArtifactID:  "baseline",
			CandidateArtifactID: "candidate",
		}); err != nil {
			t.Fatalf("UpsertEvalReviewItem %s returned error: %v", itemID, err)
		}
	}
	packetDir := filepath.Join(t.TempDir(), "packet")
	collector := MarkdownCollector{BlobStore: blobStore}
	if err := collector.WritePacket(ctx, store, "run-1", packetDir); err != nil {
		t.Fatalf("WritePacket returned error: %v", err)
	}
	first := itemFilename("foo/bar")
	second := itemFilename("foo:bar")
	if first == second {
		t.Fatalf("item filenames collide: %s", first)
	}
	for _, name := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(packetDir, "items", name)); err != nil {
			t.Fatalf("expected item file %s: %v", name, err)
		}
	}
	index, err := os.ReadFile(filepath.Join(packetDir, "index.md"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if !strings.Contains(string(index), "items/"+first) || !strings.Contains(string(index), "items/"+second) {
		t.Fatalf("index does not link distinct item files:\n%s", string(index))
	}
}

func setupMarkdownRankedFeedbackRun(t *testing.T, runID string) (*db.Store, artifact.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	blobStore := artifact.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:           runID,
		State:        "review",
		Mode:         db.EvalRunModeExplore,
		OptionsCount: 4,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:  runID,
		ItemID: "item-001",
		Title:  "Ranked Item",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		content := []byte("option " + label + " answer")
		blob, err := blobStore.Put(content)
		if err != nil {
			t.Fatalf("Put option %s returned error: %v", label, err)
		}
		artifactID := "option-" + label
		if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: artifactID, Hash: blob.Hash, MediaType: "text/markdown", SizeBytes: blob.Size, Driver: "text"}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", label, err)
		}
		if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{RunID: runID, ItemID: "item-001", Label: label, ArtifactID: artifactID, Role: "option"}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	return store, blobStore
}
