package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func configSnapshot() Snapshot {
	return Snapshot{
		Daemon: Daemon{Running: true},
		Config: ConfigView{
			Path: "/home/.gitmoot/config.toml",
			Sections: []ConfigSection{
				{Title: "paths", Rows: [][]string{{"database", "/home/.gitmoot/gitmoot.db"}}},
				{Title: "agent types", Rows: [][]string{
					{"NAME", "RUNTIME", "TEMPLATE"},
					{"planner", "codex", "gitmoot-plan-and-goal"},
				}},
				{Title: "feedback", Rows: [][]string{{"repo", "owner/feedback"}}},
			},
		},
	}
}

func configModel(t *testing.T, deps Deps, snap Snapshot) Model {
	t.Helper()
	if deps.Load == nil {
		deps.Load = func() (Snapshot, error) { return snap, nil }
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	for pages[m.selected].page != pageConfig {
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
		if cmd != nil {
			// Health load may fire while passing through; harmless to run.
			cmd()
		}
	}
	return m
}

func TestConfigPageRendersSections(t *testing.T) {
	m := configModel(t, Deps{}, configSnapshot())
	view := m.View()
	for _, want := range []string{
		"file: /home/.gitmoot/config.toml",
		"paths", "/home/.gitmoot/gitmoot.db",
		"agent types", "planner", "gitmoot-plan-and-goal",
		"feedback", "owner/feedback",
		"e edit in $EDITOR",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("config view missing %q:\n%s", want, view)
		}
	}
}

func TestConfigEditDispatchesEditorCmd(t *testing.T) {
	edited := false
	deps := Deps{
		EditConfig: func() tea.Cmd {
			edited = true
			return func() tea.Msg { return ConfigEditedMsg{} }
		},
		ValidateConfig: func() []string { return nil },
	}
	m := configModel(t, deps, configSnapshot())
	next, cmd := m.Update(key("e"))
	m = next.(Model)
	if cmd == nil || !edited {
		t.Fatal("e should dispatch the editor command")
	}
	// A clean edit clears problems and reloads.
	next, _ = m.Update(ConfigEditedMsg{})
	m = next.(Model)
	if len(m.configProblems) != 0 || m.configEditErr != "" {
		t.Fatalf("clean edit should leave no problems: %v / %q", m.configProblems, m.configEditErr)
	}
}

func TestConfigEditValidationProblemsRender(t *testing.T) {
	deps := Deps{
		EditConfig:     func() tea.Cmd { return func() tea.Msg { return ConfigEditedMsg{} } },
		ValidateConfig: func() []string { return []string{"[agents.*] max_background must be an integer"} },
	}
	m := configModel(t, deps, configSnapshot())
	next, _ := m.Update(key("e"))
	m = next.(Model)
	next, _ = m.Update(ConfigEditedMsg{})
	m = next.(Model)
	view := m.View()
	if !strings.Contains(view, "config problems after edit") || !strings.Contains(view, "max_background must be an integer") {
		t.Fatalf("validation problems should render:\n%s", view)
	}
}

func TestConfigEditorLaunchErrorRenders(t *testing.T) {
	m := configModel(t, Deps{EditConfig: func() tea.Cmd { return nil }}, configSnapshot())
	next, _ := m.Update(ConfigEditedMsg{Err: errors.New("editor: command not found")})
	m = next.(Model)
	if !strings.Contains(m.View(), "command not found") {
		t.Fatalf("editor launch error should render:\n%s", m.View())
	}
}

func TestConfigEditNoOpWithoutDep(t *testing.T) {
	m := configModel(t, Deps{}, configSnapshot())
	next, cmd := m.Update(key("e"))
	m = next.(Model)
	if cmd != nil {
		t.Fatal("e without an EditConfig dep must be a no-op")
	}
}
