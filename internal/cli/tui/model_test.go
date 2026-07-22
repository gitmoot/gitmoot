package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gitmoot/gitmoot/internal/db"
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
		Jobs: Jobs{Total: 3, ByState: map[string]int{"failed": 1, "succeeded": 2}},
		JobRows: []JobRow{
			{ID: "job-1", Agent: "planner", Type: "ask", State: "failed", LatestEvent: "agent returned an error"},
			{ID: "job-2", Agent: "planner", Type: "review", State: "succeeded"},
		},
		Trains:      []TrainSession{{ID: "train-s1", Phase: "items_ready", Candidate: "smithyx@v3", Repo: "owner/repo"}},
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

// tabToPage tabs forward until the given page is selected, running any cmd a
// landing dispatches (e.g. Health's lazy load). Robust to page-order changes.
func tabToPage(t *testing.T, m Model, p page) Model {
	t.Helper()
	for i := 0; i < len(pages) && pages[m.selected].page != p; i++ {
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
		if cmd != nil {
			cmd()
		}
	}
	if pages[m.selected].page != p {
		t.Fatalf("could not reach page %v", p)
	}
	return m
}

func TestPagesRenderExpectedContent(t *testing.T) {
	t.Parallel()
	m := loadedModel(t)
	// Page order: Attention, Activity, Trains, Agents, Workers, Jobs, Locks, Health, Config.
	wants := []string{"Prompts (1)", "No active jobs", "train-s1", "planner", "skillopt-generator", "failed", "branch locks", "environment", "edit in $EDITOR"}
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	m := loadedModel(t)
	next, _ := m.Update(snapshotMsg{err: errors.New("db locked"), at: time.Unix(2, 0)})
	m = next.(Model)
	// Move to the Agents page where the stale data is visible.
	m = tabToPage(t, m, pageAgents)
	view := m.View()
	if !strings.Contains(view, "refresh error: db locked") {
		t.Fatalf("expected error banner, got:\n%s", view)
	}
	if !strings.Contains(view, "planner") {
		t.Fatalf("stale agent data should remain visible, got:\n%s", view)
	}
}

func TestEmptyStatesRenderWithoutPanic(t *testing.T) {
	t.Parallel()
	// Daemon running so the attention page shows the empty state rather than
	// the (legitimate) daemon-stopped banner.
	empty := Snapshot{Daemon: Daemon{Running: true}}
	m := sizedModel(Deps{Load: func() (Snapshot, error) { return empty, nil }})
	next, _ := m.Update(snapshotMsg{snap: empty, at: time.Unix(3, 0)})
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
	t.Parallel()
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
	t.Parallel()
	m := loadedModel(t)
	_, cmd := m.Update(tickMsg{gen: m.tickGen})
	if cmd == nil {
		t.Fatal("tick should produce commands (re-arm + load)")
	}
}

func TestPopNudgeResetsInFlightLoad(t *testing.T) {
	t.Parallel()
	m := loadedModel(t)
	// A load dispatched just before a push has its snapshotMsg routed to the
	// covering view and dropped; the latch must not survive the pop.
	if cmd := m.queueLoad(); cmd == nil {
		t.Fatal("first queueLoad should start a load")
	}
	next, cmd := m.Update(refreshNudgeMsg{})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("the pop nudge should refresh despite the swallowed in-flight load")
	}
	if m.inFlight != true {
		t.Fatal("the nudge's own load should now be in flight")
	}
}

func TestStaleTickGenerationDropped(t *testing.T) {
	t.Parallel()
	m := loadedModel(t)
	// A pop-nudge starts a new tick generation…
	next, cmd := m.Update(refreshNudgeMsg{})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("nudge should refresh and re-arm")
	}
	// …so a tick from the pre-push chain must be dropped without re-arming
	// (otherwise fast push/pop cycles would accumulate parallel tick chains).
	_, cmd = m.Update(tickMsg{gen: m.tickGen - 1})
	if cmd != nil {
		t.Fatal("a stale-generation tick must not re-arm")
	}
	// The current generation still works.
	_, cmd = m.Update(tickMsg{gen: m.tickGen})
	if cmd == nil {
		t.Fatal("the current-generation tick should re-arm")
	}
}
