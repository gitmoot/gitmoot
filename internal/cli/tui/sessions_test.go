package tui

import (
	"errors"
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

func TestSessionsIntroExplainsRuntimeProcesses(t *testing.T) {
	m := sessionsPageModel(t, sessionDetailSnapshot())
	view := m.View()
	// The intro must plainly frame sessions as live runtime processes that run
	// jobs for agents, and that stopping one frees the process (not a job).
	for _, want := range []string{"runtime processes", "execute jobs for your agents", "does NOT cancel a running job"} {
		if !strings.Contains(view, want) {
			t.Fatalf("sessions intro missing %q:\n%s", want, view)
		}
	}
}

func TestSessionRowShowsOwningAgentType(t *testing.T) {
	m := sessionsPageModel(t, sessionDetailSnapshot())
	view := m.View()
	// The single "planner" session row must surface its owning agent type so a
	// name like "planner" reads as belonging to the "planner" type.
	if !strings.Contains(view, "planner (planner)") {
		t.Fatalf("session row should show the owning agent type:\n%s", view)
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

func sessionsPageModelWithDeps(t *testing.T, snap Snapshot, deps Deps) Model {
	t.Helper()
	if deps.Load == nil {
		deps.Load = func() (Snapshot, error) { return snap, nil }
	}
	m := sizedModel(deps)
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

func TestSessionStopSingleConfirms(t *testing.T) {
	var stopped string
	deps := Deps{StopSession: func(name string) error { stopped = name; return nil }}
	m := sessionsPageModelWithDeps(t, sessionDetailSnapshot(), deps)
	// Cursor down to the single "planner" session (row 1; row 0 is the bg group).
	next, _ := m.Update(key("j"))
	m = next.(Model)
	next, _ = m.Update(key("s"))
	m = next.(Model)
	if m.mode != modeConfirmSessionStop {
		t.Fatalf("s on a single session should open the stop confirm, mode=%v", m.mode)
	}
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("y should dispatch the stop")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if stopped != "planner" {
		t.Fatalf("StopSession called with %q, want planner", stopped)
	}
	if m.mode != modeNormal {
		t.Fatalf("a successful stop should close the confirm, mode=%v", m.mode)
	}
}

func TestSessionStopGroupShowsHint(t *testing.T) {
	deps := Deps{StopSession: func(string) error {
		t.Fatal("a group row must not call StopSession")
		return nil
	}}
	m := sessionsPageModelWithDeps(t, sessionDetailSnapshot(), deps)
	// Cursor starts on the collapsed generator group (row 0).
	next, _ := m.Update(key("s"))
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("s on a group must not open a confirm, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "agent gc") {
		t.Fatalf("group stop should hint at gc:\n%s", m.View())
	}
}

func TestSessionStopRefusalRendersInline(t *testing.T) {
	deps := Deps{StopSession: func(string) error {
		return errors.New("session \"planner\" is running a job; cancel it from Jobs first")
	}}
	m := sessionsPageModelWithDeps(t, sessionDetailSnapshot(), deps)
	next, _ := m.Update(key("j"))
	m = next.(Model)
	next, _ = m.Update(key("s"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmSessionStop {
		t.Fatalf("a refused stop should keep the confirm open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "running a job") {
		t.Fatalf("refusal should render inline:\n%s", m.View())
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
