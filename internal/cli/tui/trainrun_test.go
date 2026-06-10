package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func trainRunModel(t *testing.T, snap TrainRunSnapshot) TrainRunModel {
	t.Helper()
	m := NewTrainRun(TrainRunDeps{Load: func() (TrainRunSnapshot, error) { return snap, nil }})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(TrainRunModel)
	next, _ = m.Update(trainSnapshotMsg{snap: snap, at: time.Unix(1, 0)})
	return next.(TrainRunModel)
}

func TestTrainPhaseSegment(t *testing.T) {
	cases := map[string]int{
		"items_ready":                0,
		"generating_options":         0,
		"options_generated":          0,
		"review_published":           1,
		"feedback_synced":            1,
		"optimizer_running":          2,
		"candidate_created":          2,
		"candidate_review_published": 3,
		"candidate_promoted":         3,
		"something_unknown":          0,
	}
	for phase, want := range cases {
		if got := trainPhaseSegment(phase); got != want {
			t.Fatalf("trainPhaseSegment(%q) = %d, want %d", phase, got, want)
		}
	}
}

func TestTrainRunRendersHeaderAndPhaseBar(t *testing.T) {
	m := trainRunModel(t, TrainRunSnapshot{
		SessionID: "train-abc", Template: "smithyx@v9", ReviewRepo: "o/r",
		Phase: "items_ready", ReviewItems: 2, NextAction: "generate review options",
	})
	view := m.View()
	for _, want := range []string{"train-abc", "smithyx@v9", "o/r", "generate", "review", "optimize", "promote", "2 review items", "next: generate review options"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestTrainRunReviewPhaseShowsIssueLink(t *testing.T) {
	m := trainRunModel(t, TrainRunSnapshot{
		SessionID: "s", Phase: "review_published",
		IssueURL: "https://github.com/o/r/issues/7", FeedbackCount: 3,
	})
	view := m.View()
	if !strings.Contains(view, "review issue: https://github.com/o/r/issues/7") {
		t.Fatalf("expected the issue link:\n%s", view)
	}
	if !strings.Contains(view, "the review watcher picks it up") {
		t.Fatalf("expected the continue-from-github hint:\n%s", view)
	}
	if !strings.Contains(view, "feedback so far: 3") {
		t.Fatalf("expected feedback count:\n%s", view)
	}
}

func TestTrainRunGeneratingShowsJobCounts(t *testing.T) {
	m := trainRunModel(t, TrainRunSnapshot{
		SessionID: "s", Phase: "generating_options",
		JobsRunning: 1, JobsSucceeded: 2, JobsFailed: 0, ETA: "41s",
	})
	view := m.View()
	if !strings.Contains(view, "1 running") || !strings.Contains(view, "2 done") {
		t.Fatalf("expected job counts:\n%s", view)
	}
}

func TestTrainRunLoadErrorKeepsStaleData(t *testing.T) {
	m := trainRunModel(t, TrainRunSnapshot{SessionID: "s", Template: "t@v1", Phase: "items_ready"})
	next, _ := m.Update(trainSnapshotMsg{err: errors.New("db locked"), at: time.Unix(2, 0)})
	m = next.(TrainRunModel)
	view := m.View()
	if !strings.Contains(view, "refresh error: db locked") {
		t.Fatalf("expected error banner:\n%s", view)
	}
	if !strings.Contains(view, "t@v1") {
		t.Fatalf("stale data should remain:\n%s", view)
	}
}

func TestTrainRunRefreshSuppression(t *testing.T) {
	m := trainRunModel(t, TrainRunSnapshot{SessionID: "s", Phase: "items_ready"})
	if m.inFlight {
		t.Fatal("should be idle after a snapshot")
	}
	if cmd := m.queueLoad(); cmd == nil {
		t.Fatal("first queueLoad should return a command")
	}
	if cmd := m.queueLoad(); cmd != nil {
		t.Fatal("overlapping queueLoad should be suppressed")
	}
}

func TestTrainRunTickRearms(t *testing.T) {
	m := trainRunModel(t, TrainRunSnapshot{SessionID: "s", Phase: "items_ready"})
	_, cmd := m.Update(trainTickMsg{})
	if cmd == nil {
		t.Fatal("tick should re-arm and refresh")
	}
}
