package feedback

import (
	"context"
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
	events, err := collector.ImportPacket(ctx, store, packetDir+string(os.PathSeparator)+".", "")
	if err != nil {
		t.Fatalf("ImportPacket returned error: %v", err)
	}
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
