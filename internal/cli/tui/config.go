package tui

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// configEditableFields flattens the inline-editable fields across all config
// sections, in render order — the Config page's cursor indexes into this.
func (m Model) configEditableFields() []ConfigField {
	var fields []ConfigField
	for _, section := range m.snap.Config.Sections {
		fields = append(fields, section.Editable...)
	}
	return fields
}

// openConfigEdit enters the inline edit overlay for a scalar field.
func (m *Model) openConfigEdit(field ConfigField) tea.Cmd {
	m.configField = field
	m.configActionErr = ""
	m.mode = modeConfigEdit
	ti := textinput.New()
	ti.SetValue(field.Value)
	ti.CursorEnd()
	m.input = ti
	return m.input.Focus()
}

// updateConfigOverlay handles keys while editing a scalar config field.
func (m Model) updateConfigOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.actionBusy {
			return m, nil
		}
		m.mode = modeNormal
		m.configActionErr = ""
		m.viewport.SetContent(m.content())
		return m, nil
	case "enter":
		if m.actionBusy {
			return m, nil
		}
		value := strings.TrimSpace(m.input.Value())
		if problem := validateConfigValue(m.configField, value); problem != "" {
			m.configActionErr = problem
			m.viewport.SetContent(m.content())
			return m, nil
		}
		m.actionBusy = true
		m.configActionErr = ""
		m.viewport.SetContent(m.content())
		return m, configWriteCmd(m.deps, m.configField.KeyPath, value, m.configField.Kind)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.viewport.SetContent(m.content())
	return m, cmd
}

// validateConfigValue checks a value's shape (and closed-set membership) before
// the write attempt, so a typo re-asks in the overlay instead of round-tripping
// through the writer and reverting with a generic error.
func validateConfigValue(field ConfigField, value string) string {
	switch field.Kind {
	case ConfigInt:
		if n, err := strconv.Atoi(value); err != nil || n < 1 {
			return "must be an integer ≥ 1"
		}
	case ConfigDuration:
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return "must be a positive duration like 10m or 45m"
		}
	case ConfigStringList:
		items, ok := parseConfigListLiteral(value)
		if !ok {
			return `must be a list like ["ask", "review"]`
		}
		if len(items) == 0 {
			return "value is required"
		}
		for _, it := range items {
			if it == "" {
				return "list items cannot be empty"
			}
			if len(field.Allowed) > 0 && !slices.Contains(field.Allowed, it) {
				return "allowed: " + strings.Join(field.Allowed, ", ")
			}
		}
	case ConfigText:
		if value == "" {
			return "value is required"
		}
		if len(field.Allowed) > 0 && !slices.Contains(field.Allowed, value) {
			return "allowed: " + strings.Join(field.Allowed, ", ")
		}
	}
	return ""
}

// parseConfigListLiteral parses a bracketed TOML-ish list literal into its
// trimmed, unquoted tokens. It is the validation-side counterpart of the
// writer's parser; the empty list "[]" parses to an empty slice.
func parseConfigListLiteral(value string) ([]string, bool) {
	s := strings.TrimSpace(value)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []string{}, true
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"'`)))
	}
	return out, true
}

func configWriteCmd(deps Deps, keyPath []string, value string, kind ConfigKind) tea.Cmd {
	return func() tea.Msg {
		if deps.SetConfigScalar == nil {
			return configWriteMsg{}
		}
		return configWriteMsg{err: deps.SetConfigScalar(keyPath, value, kind)}
	}
}

// configEditView renders the inline edit overlay.
func (m Model) configEditView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("edit " + m.configField.Label))
	b.WriteString("\n\n")
	b.WriteString(strings.Join(m.configField.KeyPath, ".") + "\n\n")
	b.WriteString(m.input.View())
	b.WriteByte('\n')
	if m.configActionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.configActionErr) + "\n")
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("writing…"))
	} else {
		b.WriteString(mutedStyle.Render("enter save  esc cancel"))
	}
	return b.String()
}

// configContent renders the Config page: each parsed section as a key/value
// table, the config file path, and any validation problems from the last edit.
func (m Model) configContent() string {
	var b strings.Builder

	cv := m.snap.Config
	if cv.Path != "" {
		b.WriteString(mutedStyle.Render("file: "+cv.Path) + "\n\n")
	}
	if len(cv.Sections) == 0 {
		b.WriteString(m.loadingOr("No config.", !m.loadedAt.IsZero()))
		b.WriteByte('\n')
	}
	for _, section := range cv.Sections {
		b.WriteString(headerStyle.Render(section.Title))
		b.WriteByte('\n')
		if len(section.Rows) == 0 {
			b.WriteString(mutedStyle.Render("(default)") + "\n")
		} else {
			b.WriteString(renderRows(section.Rows))
		}
		b.WriteByte('\n')
	}

	// Inline-editable scalars as a cursor list (enter to change one), grouped
	// under their config section so related settings stay together. m.configCursor
	// still indexes the flat configEditableFields() order — these same sections
	// concatenated — so the running index stays in lockstep while the per-section
	// sub-headers are display-only.
	if len(m.configEditableFields()) > 0 {
		b.WriteString(headerStyle.Render("editable settings"))
		b.WriteByte('\n')
		idx := 0
		for _, section := range cv.Sections {
			if len(section.Editable) == 0 {
				continue
			}
			b.WriteString("  " + mutedStyle.Render(section.Title) + "\n")
			for _, field := range section.Editable {
				cursor, label := "  ", field.Label
				if idx == m.configCursor {
					cursor, label = "▸ ", selectedRowStyle.Render(field.Label)
				}
				b.WriteString("    " + cursor + label + "  " + mutedStyle.Render(dash(field.Value)) + "\n")
				idx++
			}
		}
		b.WriteByte('\n')
	}

	if m.configEditErr != "" {
		b.WriteString(errorStyle.Render("editor: "+m.configEditErr) + "\n")
	}
	if len(m.configProblems) > 0 {
		b.WriteString(redStyle.Render("config problems after edit:") + "\n")
		for _, problem := range m.configProblems {
			b.WriteString(redStyle.Render("│ "+problem) + "\n")
		}
		b.WriteString(mutedStyle.Render("e to fix") + "\n")
	}

	hint := "e edit in $EDITOR · structural edits stay in the editor"
	if len(m.configEditableFields()) > 0 {
		hint = "↑/↓ select · enter change · " + hint
	}
	b.WriteString("\n" + mutedStyle.Render(hint))
	b.WriteByte('\n')
	return b.String()
}
