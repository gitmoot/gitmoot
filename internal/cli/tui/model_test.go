package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/muesli/termenv"
)

func TestMain(m *testing.M) {
	// Pin a deterministic color profile so View() output is stable regardless of
	// the CI environment (TERM, CI, NO_COLOR); tests assert substrings only.
	lipgloss.SetColorProfile(termenv.Ascii)
	m.Run()
}

func sizedModel(deps Deps) Model {
	m := New(deps)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return next.(Model)
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		Home:           "/home",
		DatabaseExists: true,
		Agents:         []Agent{{Name: "planner", Runtime: "claude", Role: "plan", Health: "ok"}},
		Sessions: []Session{
			{Name: "skillopt-generator-bg-aa11", Runtime: "claude", State: "idle"},
			{Name: "skillopt-generator-bg-bb22", Runtime: "claude", State: "idle"},
			{Name: "solo", Runtime: "codex", Repo: "owner/repo", State: "running"},
		},
		Jobs:        Jobs{Total: 3, ByState: map[string]int{"failed": 1, "succeeded": 2}},
		BranchLocks: []BranchLock{{Repo: "owner/repo", Branch: "main", Owner: "agent"}},
		ResourceLocks: []ResourceLock{
			{Key: "generation:s1", Owner: "pid:1", Stale: false},
			{Key: "optimizer:s1", Owner: "pid:2", Stale: true},
		},
		Prompts: []db.InteractivePrompt{{ID: "p1", Question: "Pick", State: db.InteractivePromptStatePending}},
	}
}

func loadedModel(t *testing.T) Model {
	t.Helper()
	m := sizedModel(Deps{Load: func() (Snapshot, error) { return sampleSnapshot(), nil }})
	next, _ := m.Update(snapshotMsg{snap: sampleSnapshot(), at: time.Unix(1_700_000_000, 0)})
	return next.(Model)
}

func TestPagesRenderExpectedContent(t *testing.T) {
	m := loadedModel(t)
	// Page order: Attention, Agents, Sessions, Jobs, Locks.
	wants := []string{"pending prompts", "planner", "skillopt-generator", "failed", "branch locks"}
	for i, want := range wants {
		if i > 0 {
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
			m = next.(Model)
		}
		if view := m.View(); !strings.Contains(view, want) {
			t.Fatalf("page %d view missing %q:\n%s", i, want, view)
		}
	}
}

func TestTabCyclesAllPages(t *testing.T) {
	m := loadedModel(t)
	for range pages {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
	}
	if m.selected != 0 {
		t.Fatalf("tab should wrap to page 0, got %d", m.selected)
	}
	// shift+tab from page 0 wraps to the last page.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if next.(Model).selected != len(pages)-1 {
		t.Fatalf("shift+tab should wrap to last page, got %d", next.(Model).selected)
	}
}

func TestRefreshSuppressionWhileInFlight(t *testing.T) {
	m := loadedModel(t)
	if m.inFlight {
		t.Fatal("model should be idle after a snapshotMsg")
	}
	if cmd := m.queueLoad(); cmd == nil {
		t.Fatal("first queueLoad should return a command")
	}
	if cmd := m.queueLoad(); cmd != nil {
		t.Fatal("overlapping queueLoad should be suppressed")
	}
	next, _ := m.Update(snapshotMsg{snap: sampleSnapshot(), at: time.Unix(1, 0)})
	if next.(Model).inFlight {
		t.Fatal("snapshotMsg should clear in-flight")
	}
}

func TestLoadErrorKeepsStaleData(t *testing.T) {
	m := loadedModel(t)
	next, _ := m.Update(snapshotMsg{err: errors.New("db locked"), at: time.Unix(2, 0)})
	m = next.(Model)
	// Move to the Agents page where the stale data is visible.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(Model)
	view := m.View()
	if !strings.Contains(view, "refresh error: db locked") {
		t.Fatalf("expected error banner, got:\n%s", view)
	}
	if !strings.Contains(view, "planner") {
		t.Fatalf("stale agent data should remain visible, got:\n%s", view)
	}
}

func TestEmptyStatesRenderWithoutPanic(t *testing.T) {
	m := sizedModel(Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }})
	next, _ := m.Update(snapshotMsg{snap: Snapshot{}, at: time.Unix(3, 0)})
	m = next.(Model)
	for range pages {
		_ = m.View()
		n, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = n.(Model)
	}
	if !strings.Contains(m.View(), "No") {
		t.Fatalf("expected an empty-state message, got:\n%s", m.View())
	}
}

func TestResizeUpdatesViewport(t *testing.T) {
	m := loadedModel(t)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = next.(Model)
	if m.width != 60 || m.height != 20 {
		t.Fatalf("resize not applied: %dx%d", m.width, m.height)
	}
	if m.viewport.Height < 1 {
		t.Fatalf("viewport height should stay positive, got %d", m.viewport.Height)
	}
}

func TestTickRearmsAndRefreshes(t *testing.T) {
	m := loadedModel(t)
	_, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("tick should produce commands (re-arm + load)")
	}
}
