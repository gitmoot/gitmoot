package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// stubChild is a minimal pushed child view. It stands in for whatever full-screen
// model a page pushes (the Trains page's train-run view went with the SkillOpt loop
// in #1752; the agent-create form is the surviving pusher). It pops on "q" and
// records the last broadcast size, which is exactly the surface these Root tests
// exercise — the router's stack and broadcast behavior, not the child's content.
type stubChild struct {
	width  int
	height int
}

func (s stubChild) Init() tea.Cmd { return nil }

func (s stubChild) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = m.Width, m.Height
		return s, nil
	case tea.KeyMsg:
		if m.String() == "q" {
			return s, Pop()
		}
	}
	return s, nil
}

func (s stubChild) View() string { return "stub child view" }

// rootWithDashboard returns a Root whose dashboard can push a child from the Agents
// page (n = new agent), which is how a child view is opened now.
func rootWithDashboard(t *testing.T) Root {
	t.Helper()
	deps := Deps{
		Load:            func() (Snapshot, error) { return sampleSnapshot(), nil },
		OpenAgentCreate: func() (tea.Model, error) { return stubChild{}, nil },
	}
	dash := New(deps)
	root := NewRoot(dash)
	next, _ := root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	root = next.(Root)
	next, _ = root.Update(snapshotMsg{snap: sampleSnapshot(), at: time.Unix(1, 0)})
	return next.(Root)
}

// pushChild tabs to the Agents page and presses n, leaving the stub child on top.
func pushChild(t *testing.T, root Root) Root {
	t.Helper()
	for range pages {
		dash, ok := root.top().(Model)
		if ok && pages[dash.selected].page == pageAgents {
			break
		}
		root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyTab})
	}
	return driveRoot(t, root, key("n"))
}

// driveRoot applies a msg and any push/pop msgs its commands produce (the test
// stand-in for the bubbletea runtime's command loop).
func driveRoot(t *testing.T, root Root, msg tea.Msg) Root {
	t.Helper()
	next, cmd := root.Update(msg)
	root = next.(Root)
	for cmd != nil {
		out := cmd()
		switch out.(type) {
		case PushModelMsg, PopModelMsg:
			next, cmd2 := root.Update(out)
			root = next.(Root)
			cmd = cmd2
		default:
			cmd = nil
		}
	}
	return root
}

func TestRootPopsBackToDashboard(t *testing.T) {
	root := pushChild(t, rootWithDashboard(t))
	if len(root.stack) != 2 {
		t.Fatalf("setup: stack=%d", len(root.stack))
	}
	// q in the pushed child pops, not quits.
	root = driveRoot(t, root, key("q"))
	if len(root.stack) != 1 {
		t.Fatalf("q should pop back to the dashboard, stack=%d", len(root.stack))
	}
	if !strings.Contains(root.View(), "planner") {
		t.Fatalf("dashboard should be visible again:\n%s", root.View())
	}
}

func TestRootCtrlCQuitsFromChild(t *testing.T) {
	root := pushChild(t, rootWithDashboard(t))
	_, cmd := root.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should produce a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c should quit, got %T", cmd())
	}
}

func TestRootBroadcastsWindowSize(t *testing.T) {
	root := pushChild(t, rootWithDashboard(t))
	next, _ := root.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	root = next.(Root)
	// Pop back; the dashboard below must have received the resize too.
	root = driveRoot(t, root, key("q"))
	dash := root.top().(Model)
	if dash.width != 60 || dash.height != 20 {
		t.Fatalf("dashboard missed the broadcast resize: %dx%d", dash.width, dash.height)
	}
}

func TestRootPopOnBaseQuits(t *testing.T) {
	root := rootWithDashboard(t)
	_, cmd := root.Update(PopModelMsg{})
	if cmd == nil {
		t.Fatal("pop on the base should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected quit, got %T", cmd())
	}
}

func TestDashboardHelpOverlay(t *testing.T) {
	m := loadedModel(t)
	next, _ := m.Update(key("?"))
	m = next.(Model)
	if !strings.Contains(m.View(), "Help — Attention") {
		t.Fatalf("expected the help overlay:\n%s", m.View())
	}
	next, _ = m.Update(key("?"))
	m = next.(Model)
	if strings.Contains(m.View(), "Help — Attention") {
		t.Fatalf("? again should close help:\n%s", m.View())
	}
}
