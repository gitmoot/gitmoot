package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func rootWithDashboard(t *testing.T) Root {
	t.Helper()
	deps := Deps{
		Load: func() (Snapshot, error) { return trainSnapshot(), nil },
		OpenTrain: func(sessionID string) tea.Model {
			td := TrainRunDeps{
				Embedded: true,
				Load: func() (TrainRunSnapshot, error) {
					return TrainRunSnapshot{SessionID: sessionID, Phase: "items_ready"}, nil
				},
			}
			return NewTrainRun(td)
		},
	}
	dash := New(deps)
	root := NewRoot(dash)
	next, _ := root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	root = next.(Root)
	next, _ = root.Update(snapshotMsg{snap: trainSnapshot(), at: time.Unix(1, 0)})
	return next.(Root)
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

func TestRootPushesTrainViewOnTrainsEnter(t *testing.T) {
	root := rootWithDashboard(t)
	// Tab to Trains, then enter.
	root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyTab})
	root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyEnter})
	if len(root.stack) != 2 {
		t.Fatalf("enter on Trains should push the train view, stack=%d", len(root.stack))
	}
	if _, ok := root.top().(TrainRunModel); !ok {
		t.Fatalf("top of stack should be the train-run model, got %T", root.top())
	}
}

func TestRootPopsBackToDashboard(t *testing.T) {
	root := rootWithDashboard(t)
	root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyTab})
	root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyEnter})
	if len(root.stack) != 2 {
		t.Fatalf("setup: stack=%d", len(root.stack))
	}
	// q in the embedded train view pops, not quits.
	root = driveRoot(t, root, key("q"))
	if len(root.stack) != 1 {
		t.Fatalf("q should pop back to the dashboard, stack=%d", len(root.stack))
	}
	if !strings.Contains(root.View(), "train-aaa") {
		t.Fatalf("dashboard should be visible again:\n%s", root.View())
	}
}

func TestRootCtrlCQuitsFromChild(t *testing.T) {
	root := rootWithDashboard(t)
	root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyTab})
	root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyEnter})
	_, cmd := root.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should produce a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c should quit, got %T", cmd())
	}
}

func TestRootBroadcastsWindowSize(t *testing.T) {
	root := rootWithDashboard(t)
	root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyTab})
	root = driveRoot(t, root, tea.KeyMsg{Type: tea.KeyEnter})
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

func TestDashboardTrainsEnterWithoutOpenTrainKeepsDetail(t *testing.T) {
	// Without OpenTrain the old inline detail still works (standalone use).
	m := trainsModel(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeTrainDetail {
		t.Fatalf("without OpenTrain, enter should open the inline detail, mode=%v", m.mode)
	}
}
