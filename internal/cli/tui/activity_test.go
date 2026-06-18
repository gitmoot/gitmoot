package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func activityPageModel(t *testing.T, snap Snapshot) Model {
	t.Helper()
	m := sizedModel(Deps{Load: func() (Snapshot, error) { return snap, nil }})
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	return tabToPage(t, m, pageActivity)
}

func TestActivityPageRendersActiveTrees(t *testing.T) {
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Activity: []ActivityRoot{{
			JobID: "root-1", Agent: "planner", Action: "implement", State: "running", Repo: "o/r",
			Children: []JobChild{
				{ID: "c1", DelegationID: "d1", Agent: "impl-a", Action: "build the API", State: "running"},
				{ID: "c2", DelegationID: "d2", Agent: "impl-b", Action: "write tests", State: "blocked"},
			},
			ContinuationID: "cont-1", ContinuationState: "queued",
			Total: 2, Running: 1, Blocked: 1, Done: 0,
		}},
	}
	m := activityPageModel(t, snap)
	view := m.View()
	for _, want := range []string{"root-1", "o/r", "impl-a", "build the API", "impl-b", "2 delegations", "continuation"} {
		if !strings.Contains(view, want) {
			t.Fatalf("activity view missing %q:\n%s", want, view)
		}
	}
	// Cursor 0 = the root; enter opens its detail.
	if r, ok := m.activityUnderCursor(); !ok || r.ID != "root-1" {
		t.Fatalf("cursor 0 should select root-1, got %+v ok=%v", r, ok)
	}
	// Cursor 1 = the first delegate; the cursor walks into the tree, and enter
	// opens that delegate's detail (its request/result), not the root's.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if r, ok := m.activityUnderCursor(); !ok || r.ID != "c1" {
		t.Fatalf("cursor 1 should select delegate c1, got %+v ok=%v", r, ok)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeJobDetail || m.activeJob.ID != "c1" {
		t.Fatalf("enter on a delegate should open ITS detail, mode=%v job=%q", m.mode, m.activeJob.ID)
	}
	_ = cmd
}

func TestActivityPageEmpty(t *testing.T) {
	m := activityPageModel(t, Snapshot{Daemon: Daemon{Running: true}})
	if !strings.Contains(m.View(), "No active jobs") {
		t.Fatalf("empty activity page should say so:\n%s", m.View())
	}
}

// TestActivityPageShowsCorrectiveContinuation guards that a live continuation is
// rendered even when the root has no fresh delegation children (Total == 0) — the
// engine's corrective-continuation path, where the continuation is the only live
// work and must not be hidden.
func TestActivityPageShowsCorrectiveContinuation(t *testing.T) {
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Activity: []ActivityRoot{{
			JobID: "root-1", Agent: "planner", Action: "implement", State: "succeeded",
			ContinuationID: "root-1/continuation", ContinuationState: "running",
		}},
	}
	m := activityPageModel(t, snap)
	if view := m.View(); !strings.Contains(view, "continuation") {
		t.Fatalf("a live continuation with no fresh delegations must still render:\n%s", view)
	}
}

// TestActivityPageWindowsWideFanout guards that a single root with many
// delegation children windows its rows around the cursor: a short terminal must
// keep the selected delegate visible (not rendered below the viewport) and show
// the scrolled-off markers.
func TestActivityPageWindowsWideFanout(t *testing.T) {
	children := make([]JobChild, 0, 30)
	for i := 0; i < 30; i++ {
		children = append(children, JobChild{
			ID:    "c" + strconv.Itoa(i),
			Agent: "impl-" + strconv.Itoa(i),
			// A distinctive action token so we can assert the selected row renders.
			Action: "task-" + strconv.Itoa(i),
			State:  "running",
		})
	}
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Activity: []ActivityRoot{{
			JobID: "root-1", Agent: "planner", Action: "implement", State: "running",
			Children: children, Total: 30, Running: 30,
		}},
	}
	m := sizedModel(Deps{Load: func() (Snapshot, error) { return snap, nil }})
	// A short terminal: far fewer rows than the 30+ the tree wants to draw.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 18})
	m = next.(Model)
	next, _ = m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	m = tabToPage(t, m, pageActivity)

	// Walk the cursor down to a deep delegate (root + child index 25 = 26 downs).
	for i := 0; i < 26; i++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(Model)
	}
	if r, ok := m.activityUnderCursor(); !ok || r.ID != "c25" {
		t.Fatalf("cursor should be on c25, got %+v ok=%v", r, ok)
	}
	view := m.View()
	// The selected delegate's row must be in the rendered window...
	if !strings.Contains(view, "task-25") {
		t.Fatalf("selected delegate task-25 should be visible:\n%s", view)
	}
	// ...and the window must have scrolled (rows above are hidden behind a marker).
	if !strings.Contains(view, "more rows above") {
		t.Fatalf("a deep cursor should scroll the window and show an above marker:\n%s", view)
	}
	// The very first delegate is far above the window and must be hidden.
	if strings.Contains(view, "task-0  ") {
		t.Fatalf("task-0 should be scrolled out of the window:\n%s", view)
	}
}
