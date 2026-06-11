package tui

import "strings"

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

	b.WriteString("\n" + mutedStyle.Render("e edit in $EDITOR · structural edits stay in the editor"))
	b.WriteByte('\n')
	return b.String()
}
