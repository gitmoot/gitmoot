package tui

import (
	"fmt"
	"strings"
)

// View renders the current form step.
func (m TrainInitModel) View() string {
	switch m.state {
	case tiConfirm:
		return m.confirmView()
	case tiDone:
		return ""
	case tiCustomPath:
		return m.customPathView()
	case tiRepoCheck:
		return m.repoCheckView()
	case tiRepoMissing:
		return m.repoMissingView()
	default:
		return m.fieldView()
	}
}

func (m TrainInitModel) repoCheckView() string {
	return headerStyle.Render(m.fields[m.idx].Label) + "\n\n" +
		mutedStyle.Render("checking "+m.pendingValue+" on GitHub…")
}

func (m TrainInitModel) repoMissingView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render(m.fields[m.idx].Label))
	b.WriteString("\n\n")
	b.WriteString("repo " + m.pendingValue + " does not exist on GitHub\n")
	if m.inlineErr != "" {
		b.WriteString(errorStyle.Render(m.inlineErr) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("c create private repo  e re-enter  esc re-enter"))
	return b.String()
}

func (m TrainInitModel) fieldView() string {
	field := m.fields[m.idx]
	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf("[%d/%d] %s", m.idx+1, len(m.fields), field.Label)))
	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("answer from another terminal: gitmoot interactive answer " + field.Prompt.ID + " <value>"))
	b.WriteString("\n\n")

	if field.Kind == FieldText {
		b.WriteString(m.input.View())
		b.WriteByte('\n')
	} else {
		for i, choice := range field.Choices {
			cursor, label := "  ", choice.Label
			if i == m.choiceIdx {
				cursor, label = "▸ ", selectedRowStyle.Render(choice.Label)
			}
			if choice.Value != "" && choice.Value == field.Default {
				label += mutedStyle.Render(" (default)")
			}
			b.WriteString(cursor + label + "\n")
		}
	}

	b.WriteString(m.statusLines())
	b.WriteString("\n")
	if field.Kind == FieldText {
		b.WriteString(mutedStyle.Render("enter submit  esc abort"))
	} else {
		b.WriteString(mutedStyle.Render("↑/↓ choose  enter select  esc abort"))
	}
	return b.String()
}

func (m TrainInitModel) customPathView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Custom template"))
	b.WriteString("\n\n")
	b.WriteString(m.input.View())
	b.WriteByte('\n')
	b.WriteString(m.statusLines())
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("enter use  esc back"))
	return b.String()
}

func (m TrainInitModel) confirmView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Review"))
	b.WriteString("\n\n")
	for _, row := range m.summary(m.answers) {
		b.WriteString("  " + strings.Join(row, ": ") + "\n")
	}
	b.WriteByte('\n')
	b.WriteString("Create scaffold? [Y/n]")
	return b.String()
}

// statusLines renders the inline error and the external-answer flash, each on
// its own line when present.
func (m TrainInitModel) statusLines() string {
	var b strings.Builder
	if m.inlineErr != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render(m.inlineErr))
		b.WriteByte('\n')
	}
	if m.flash != "" {
		b.WriteString(greenStyle.Render(m.flash))
		b.WriteByte('\n')
	}
	return b.String()
}
