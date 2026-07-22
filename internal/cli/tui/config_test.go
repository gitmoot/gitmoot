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
	t.Parallel()
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

// TestConfigEditableSettingsGroupedBySection verifies the editable settings
// render grouped under their section sub-headers (in section order) while the
// cursor still indexes the flat field order across sections.
func TestConfigEditableSettingsGroupedBySection(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Config: ConfigView{
			Path: "/c.toml",
			Sections: []ConfigSection{
				{Title: "daemon", Editable: []ConfigField{{Label: "workers", Value: "1"}}},
				{Title: "delegation", Editable: []ConfigField{
					{Label: "max depth", Value: "8"},
					{Label: "budget", Value: "40"},
				}},
			},
		},
	}
	m := configModel(t, Deps{}, snap)
	view := m.View()

	// Within the editable block, each section sub-header precedes its fields, in
	// section order (slice the view from "editable settings" so the read-only
	// overview above it can't satisfy the assertion).
	eb := strings.Index(view, "editable settings")
	if eb < 0 {
		t.Fatalf("missing editable settings block:\n%s", view)
	}
	sub := view[eb:]
	last := -1
	for _, s := range []string{"daemon", "workers", "delegation", "max depth", "budget"} {
		i := strings.Index(sub, s)
		if i < 0 {
			t.Fatalf("editable block missing %q:\n%s", s, sub)
		}
		if i < last {
			t.Fatalf("editable block out of order at %q:\n%s", s, sub)
		}
		last = i
	}

	// The cursor indexes the flat field order across sections.
	if f := m.configEditableFields(); len(f) != 3 || f[0].Label != "workers" || f[2].Label != "budget" {
		t.Fatalf("flat field order wrong: %+v", f)
	}
	for i := 0; i < 2; i++ {
		next, _ := m.Update(key("j"))
		m = next.(Model)
	}
	if m.configCursor != 2 {
		t.Fatalf("cursor should be 2 (budget) after two downs, got %d", m.configCursor)
	}
}

// TestConfigEditableSettingsSubgroupedByEntity verifies that editable fields with
// a 3-part KeyPath (e.g. per agent type) sub-group under the entity name, with the
// redundant "<entity> · " prefix dropped from the field label.
func TestConfigEditableSettingsSubgroupedByEntity(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Config: ConfigView{
			Path: "/c.toml",
			Sections: []ConfigSection{{
				Title: "agent types",
				Editable: []ConfigField{
					{Label: "planner · max_background", KeyPath: []string{"agents", "planner", "max_background"}, Value: "4"},
					{Label: "planner · idle_timeout", KeyPath: []string{"agents", "planner", "idle_timeout"}, Value: "10m"},
					{Label: "reviewer · max_background", KeyPath: []string{"agents", "reviewer", "max_background"}, Value: "2"},
				},
			}},
		},
	}
	m := configModel(t, Deps{}, snap)
	view := m.View()
	eb := strings.Index(view, "editable settings")
	if eb < 0 {
		t.Fatalf("missing editable settings block:\n%s", view)
	}
	sub := view[eb:]
	// The flat "<entity> · <setting>" label is gone; entity is a sub-heading.
	if strings.Contains(sub, "planner · max_background") {
		t.Fatalf("entity prefix should be dropped from the field label:\n%s", sub)
	}
	last := -1
	for _, s := range []string{"agent types", "planner", "max_background", "idle_timeout", "reviewer"} {
		i := strings.Index(sub, s)
		if i < 0 {
			t.Fatalf("editable block missing %q:\n%s", s, sub)
		}
		if i < last {
			t.Fatalf("editable block out of order at %q:\n%s", s, sub)
		}
		last = i
	}
}

func TestConfigEditDispatchesEditorCmd(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	m := configModel(t, Deps{EditConfig: func() tea.Cmd { return nil }}, configSnapshot())
	next, _ := m.Update(ConfigEditedMsg{Err: errors.New("editor: command not found")})
	m = next.(Model)
	if !strings.Contains(m.View(), "command not found") {
		t.Fatalf("editor launch error should render:\n%s", m.View())
	}
}

func TestConfigEditNoOpWithoutDep(t *testing.T) {
	t.Parallel()
	m := configModel(t, Deps{}, configSnapshot())
	next, cmd := m.Update(key("e"))
	m = next.(Model)
	if cmd != nil {
		t.Fatal("e without an EditConfig dep must be a no-op")
	}
}

func configEditSnapshot() Snapshot {
	return Snapshot{
		Daemon: Daemon{Running: true},
		Config: ConfigView{
			Path: "/home/.gitmoot/config.toml",
			Sections: []ConfigSection{
				{Title: "agent types", Rows: [][]string{{"NAME"}, {"planner"}}, Editable: []ConfigField{
					{Label: "planner · max_background", KeyPath: []string{"agents", "planner", "max_background"}, Kind: ConfigInt, Value: "4"},
					{Label: "planner · idle_timeout", KeyPath: []string{"agents", "planner", "idle_timeout"}, Kind: ConfigDuration, Value: "10m"},
				}},
				{Title: "feedback", Rows: [][]string{{"repo", "owner/feedback"}}, Editable: []ConfigField{
					{Label: "feedback · repo", KeyPath: []string{"feedback", "repo"}, Kind: ConfigText, Value: "owner/feedback"},
				}},
			},
		},
	}
}

func configEditModel(t *testing.T, deps Deps) Model {
	t.Helper()
	snap := configEditSnapshot()
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
			cmd()
		}
	}
	return m
}

func TestConfigInlineEditWritesScalar(t *testing.T) {
	t.Parallel()
	var gotPath []string
	var gotValue string
	deps := Deps{SetConfigScalar: func(keyPath []string, value string, kind ConfigKind) error {
		gotPath, gotValue = keyPath, value
		return nil
	}}
	m := configEditModel(t, deps)
	// Cursor on the first editable field (planner · max_background).
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeConfigEdit || cmd == nil {
		t.Fatalf("enter should open the inline editor, mode=%v", m.mode)
	}
	// Clear "4", type "8".
	for range "4" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = next.(Model)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("8")})
	m = next.(Model)
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if len(gotPath) != 3 || gotPath[2] != "max_background" || gotValue != "8" {
		t.Fatalf("SetConfigScalar called with (%v, %q)", gotPath, gotValue)
	}
	if m.mode != modeNormal {
		t.Fatalf("a successful write should close the editor, mode=%v", m.mode)
	}
}

func TestConfigInlineEditValidatesBeforeWrite(t *testing.T) {
	t.Parallel()
	deps := Deps{SetConfigScalar: func(keyPath []string, value string, kind ConfigKind) error {
		t.Fatal("must not write an invalid value")
		return nil
	}}
	m := configEditModel(t, deps)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // open editor on max_background (int)
	m = next.(Model)
	for range "4" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = next.(Model)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")})
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("an invalid int must not dispatch a write")
	}
	if m.mode != modeConfigEdit || !strings.Contains(m.View(), "integer ≥ 1") {
		t.Fatalf("invalid value should re-ask in the overlay:\n%s", m.View())
	}
}

func TestConfigInlineEditWriteErrorKeepsOverlay(t *testing.T) {
	t.Parallel()
	deps := Deps{SetConfigScalar: func(keyPath []string, value string, kind ConfigKind) error {
		return errors.New("config invalid after edit (reverted)")
	}}
	m := configEditModel(t, deps)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // submit unchanged value
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfigEdit || !strings.Contains(m.View(), "reverted") {
		t.Fatalf("a write error should keep the overlay with the message:\n%s", m.View())
	}
}

func TestConfigDurationFieldValidation(t *testing.T) {
	t.Parallel()
	if validateConfigValue(ConfigField{Kind: ConfigDuration}, "10m") != "" {
		t.Fatal("10m should be a valid duration")
	}
	if validateConfigValue(ConfigField{Kind: ConfigDuration}, "nope") == "" {
		t.Fatal("nope should be rejected as a duration")
	}
	if validateConfigValue(ConfigField{Kind: ConfigInt}, "0") == "" {
		t.Fatal("0 should be rejected (must be >= 1)")
	}
}

func TestConfigEnumAndListValidation(t *testing.T) {
	t.Parallel()
	enum := ConfigField{Kind: ConfigText, Allowed: []string{"queue", "fork_temp_session"}}
	if validateConfigValue(enum, "queue") != "" {
		t.Fatal("queue should be allowed")
	}
	if validateConfigValue(enum, "nonsense") == "" {
		t.Fatal("a value outside the allowed set should be rejected")
	}
	list := ConfigField{Kind: ConfigStringList, Allowed: []string{"ask", "review", "implement"}}
	if validateConfigValue(list, `["ask", "review"]`) != "" {
		t.Fatal("a list of allowed actions should pass")
	}
	if validateConfigValue(list, `["ask", "deploy"]`) == "" {
		t.Fatal("a list with a disallowed action should be rejected")
	}
	if validateConfigValue(list, "ask, review") == "" {
		t.Fatal("a non-bracketed value should be rejected for a list field")
	}
}
