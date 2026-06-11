package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// sessionsPageModel loads a snapshot and tabs to the Sessions page (the 4th
// page: Attention → Trains → Agents → Sessions).
func sessionsPageModel(t *testing.T, snap Snapshot) Model {
	t.Helper()
	m := sizedModel(Deps{Load: func() (Snapshot, error) { return snap, nil }})
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1_700_000_000, 0)})
	m = next.(Model)
	for i := 0; i < 3; i++ {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
	}
	if pages[m.selected].page != pageSessions {
		t.Fatalf("expected Sessions page, got %v", pages[m.selected].page)
	}
	return m
}

func sessionDetailSnapshot() Snapshot {
	return Snapshot{
		DatabaseExists: true,
		Sessions: []Session{
			{Name: "planner", Runtime: "claude", Repo: "owner/repo", State: "idle",
				Type: "planner", Role: "plan", Template: "planner-tpl",
				LastUsed: "2026-06-10T12:00:00Z", Expires: "2026-06-10T12:30:00Z"},
			{Name: "skillopt-generator-bg-aa11", Runtime: "claude", State: "idle", Type: "skillopt-generator"},
			{Name: "skillopt-generator-bg-bb22", Runtime: "claude", State: "idle", Type: "skillopt-generator"},
		},
	}
}

func TestSessionDetailRendersInstanceFields(t *testing.T) {
	m := sessionsPageModel(t, sessionDetailSnapshot())
	// The grouped bg rows sort first, then the single "planner". Move to it.
	// rows: [generator group, planner]. Cursor down to planner (index 1).
	next, _ := m.Update(key("j"))
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeSessionDetail {
		t.Fatalf("enter should open the session detail, mode=%v", m.mode)
	}
	view := m.View()
	for _, want := range []string{"runtime session planner", "plan", "owner/repo", "planner-tpl", "2026-06-10T12:30:00Z"} {
		if !strings.Contains(view, want) {
			t.Fatalf("session detail missing %q:\n%s", want, view)
		}
	}
	// esc returns to the list.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("esc should return to the list, got %v", m.mode)
	}
}

func TestSessionDetailGroupShowsCount(t *testing.T) {
	m := sessionsPageModel(t, sessionDetailSnapshot())
	// Cursor starts on the collapsed generator group (row 0).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	view := m.View()
	if !strings.Contains(view, "×2") {
		t.Fatalf("group detail should note the member count:\n%s", view)
	}
	if !strings.Contains(view, "background workers") {
		t.Fatalf("group detail should explain the collapse:\n%s", view)
	}
}

func TestLocksPageShowsGuidance(t *testing.T) {
	snap := Snapshot{
		DatabaseExists: true,
		BranchLocks:    []BranchLock{{Repo: "owner/repo", Branch: "feature", Owner: "agent"}},
		ResourceLocks:  []ResourceLock{{Key: "generation:s1", Owner: "pid:1"}},
	}
	m := sizedModel(Deps{Load: func() (Snapshot, error) { return snap, nil }})
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1_700_000_000, 0)})
	m = next.(Model)
	for i := 0; i < 5; i++ { // Attention → … → Locks (6th page)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
	}
	if pages[m.selected].page != pageLocks {
		t.Fatalf("expected Locks page, got %v", pages[m.selected].page)
	}
	view := m.View()
	for _, want := range []string{"what to do: usually nothing", "gitmoot lock release"} {
		if !strings.Contains(view, want) {
			t.Fatalf("locks page missing guidance %q:\n%s", want, view)
		}
	}
}
