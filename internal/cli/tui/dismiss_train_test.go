package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestDismissConfirmYes(t *testing.T) {
	var dismissed string
	deps := Deps{
		Load:    func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Dismiss: func(id string) error { dismissed = id; return nil },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("d"))
	m = next.(Model)
	if m.mode != modeConfirmDismiss {
		t.Fatalf("d should open the dismiss confirm, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "Delete this prompt?") {
		t.Fatalf("confirm prompt missing:\n%s", m.View())
	}
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("y should submit a dismiss command")
	}
	_ = cmd()
	if dismissed != "choice.p" {
		t.Fatalf("Dismiss called with %q, want choice.p", dismissed)
	}
}

func TestDismissConfirmCancel(t *testing.T) {
	called := false
	deps := Deps{
		Load:    func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Dismiss: func(id string) error { called = true; return nil },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("d"))
	m = next.(Model)
	// 'n' cancels.
	next, _ = m.Update(key("n"))
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("n should cancel the dismiss, mode=%v", m.mode)
	}
	if called {
		t.Fatal("cancel must not call Dismiss")
	}
}

func TestDismissNotFoundTreatedAsSuccess(t *testing.T) {
	deps := Deps{
		Load: func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Dismiss: func(id string) error {
			return fmt.Errorf("interactive prompt %q: %w", id, db.ErrInteractivePromptNotFound)
		},
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("a not-found prompt should still close the overlay, mode=%v", m.mode)
	}
	if m.actionErr != "" {
		t.Fatalf("not-found should not surface an error, got %q", m.actionErr)
	}
}

func TestDismissRealErrorKeepsOverlay(t *testing.T) {
	deps := Deps{
		Load:    func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Dismiss: func(id string) error { return errors.New("database is locked") },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmDismiss {
		t.Fatalf("a real error should keep the confirm open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "database is locked") {
		t.Fatalf("expected the error in view:\n%s", m.View())
	}
}

func trainSnapshot() Snapshot {
	return Snapshot{
		Trains: []TrainSession{
			{ID: "train-aaa", Phase: "generating_options", Candidate: "c@v1", Repo: "owner/a"},
			{ID: "train-bbb", Phase: "run_abandoned", Repo: "owner/b"},
		},
		ResourceLocks: []ResourceLock{
			{Key: "generation:train-aaa", Owner: "pid:1", Stale: false},
			{Key: "optimizer:train-bbb", Owner: "pid:2", Stale: true},
		},
	}
}

func trainsModel(t *testing.T) Model {
	t.Helper()
	deps := Deps{Load: func() (Snapshot, error) { return trainSnapshot(), nil }}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: trainSnapshot(), at: time.Unix(1, 0)})
	m = next.(Model)
	// Tab from Attention to Trains.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	return next.(Model)
}

func TestTrainDetailShowsSessionLocksOnly(t *testing.T) {
	m := trainsModel(t)
	if pages[m.selected].page != pageTrains {
		t.Fatalf("expected to be on the Trains page")
	}
	// Open detail for the first train.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeTrainDetail {
		t.Fatalf("enter should open train detail, mode=%v", m.mode)
	}
	view := m.View()
	if !strings.Contains(view, "generating_options") || !strings.Contains(view, "generation:train-aaa") {
		t.Fatalf("detail should show the session phase and its lock:\n%s", view)
	}
	if strings.Contains(view, "optimizer:train-bbb") {
		t.Fatalf("detail must not show another session's lock:\n%s", view)
	}
	// Esc returns to the list.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("esc should leave detail, mode=%v", m.mode)
	}
}

func TestTrainLocksMatchWholeSegmentNotPrefix(t *testing.T) {
	locks := []ResourceLock{
		{Key: "generation:train-a"},
		{Key: "generation:train-aaa"},
		{Key: "skillopt-train:train-a:iter-1"},
	}
	got := trainLocks(locks, "train-a")
	if len(got) != 2 {
		t.Fatalf("expected 2 locks for train-a (exact segment), got %d: %+v", len(got), got)
	}
	for _, l := range got {
		if l.Key == "generation:train-aaa" {
			t.Fatalf("train-a must not match train-aaa's lock: %+v", got)
		}
	}
}

func TestTrainCursorClampedOnRefresh(t *testing.T) {
	m := trainsModel(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.trainCursor != 1 {
		t.Fatalf("train cursor should be 1, got %d", m.trainCursor)
	}
	next, _ = m.Update(snapshotMsg{snap: Snapshot{}, at: time.Unix(2, 0)})
	m = next.(Model)
	if m.trainCursor != 0 {
		t.Fatalf("train cursor should clamp to 0, got %d", m.trainCursor)
	}
}
