package tui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// openAnswer enters the answer overlay for the prompt under the cursor. Choice
// prompts get a selectable list with the default preselected; free-text prompts
// get a text input prefilled with the default. Returns the textinput blink cmd
// when relevant.
func (m *Model) openAnswer() tea.Cmd {
	if pages[m.selected].page != pageAttention || len(m.snap.Prompts) == 0 {
		return nil
	}
	p := m.snap.Prompts[m.promptCursor]
	m.active = p
	m.actionErr = ""
	m.actionBusy = false
	if len(p.Choices) > 0 {
		m.mode = modeAnswerChoice
		m.choiceIdx = indexOf(p.Choices, p.Default)
		return nil
	}
	m.mode = modeAnswerText
	ti := textinput.New()
	ti.SetValue(p.Default)
	ti.CursorEnd()
	m.input = ti
	return m.input.Focus()
}

// updateOverlay handles keys while an answer overlay is active.
func (m Model) updateOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.mode = modeNormal
		m.actionErr = ""
		m.actionBusy = false
		m.viewport.SetContent(m.content())
		return m, nil
	}

	switch m.mode {
	case modeAnswerChoice:
		switch msg.String() {
		case "up", "k":
			if m.choiceIdx > 0 {
				m.choiceIdx--
			}
		case "down", "j":
			if m.choiceIdx < len(m.active.Choices)-1 {
				m.choiceIdx++
			}
		case "enter":
			if !m.actionBusy && len(m.active.Choices) > 0 {
				m.actionBusy = true
				m.actionErr = ""
				cmd := answerCmd(m.deps, m.active.ID, m.active.Choices[m.choiceIdx])
				m.viewport.SetContent(m.content())
				return m, cmd
			}
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeAnswerText:
		if msg.String() == "enter" {
			if !m.actionBusy {
				m.actionBusy = true
				m.actionErr = ""
				cmd := answerCmd(m.deps, m.active.ID, strings.TrimSpace(m.input.Value()))
				m.viewport.SetContent(m.content())
				return m, cmd
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.viewport.SetContent(m.content())
		return m, cmd
	}
	return m, nil
}

func answerCmd(deps Deps, id, value string) tea.Cmd {
	return func() tea.Msg {
		if deps.Answer == nil {
			return answerResultMsg{id: id}
		}
		return answerResultMsg{id: id, err: deps.Answer(id, value)}
	}
}

// attentionContent renders the Attention page: pending prompts (selectable),
// then blocked/failed job counts, stale resource locks, and branch locks.
func (m Model) attentionContent() string {
	if m.mode == modeAnswerChoice || m.mode == modeAnswerText {
		return m.answerOverlay()
	}
	var b strings.Builder
	wrote := false

	b.WriteString(headerStyle.Render("pending prompts"))
	b.WriteByte('\n')
	if len(m.snap.Prompts) == 0 {
		b.WriteString(mutedStyle.Render("none"))
		b.WriteByte('\n')
	} else {
		for i, p := range m.snap.Prompts {
			cursor := "  "
			line := p.ID + "  " + truncate(p.Question, 60)
			if i == m.promptCursor {
				cursor = "▸ "
				line = selectedRowStyle.Render(line)
			}
			b.WriteString(cursor)
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteString(mutedStyle.Render("a answer  d dismiss"))
		b.WriteByte('\n')
		wrote = true
	}

	if blocked := m.snap.Jobs.ByState["blocked"]; blocked > 0 {
		b.WriteString("\n")
		b.WriteString(redStyle.Render(plural(blocked, "blocked job", "blocked jobs")))
		b.WriteByte('\n')
		wrote = true
	}
	if failed := m.snap.Jobs.ByState["failed"]; failed > 0 {
		b.WriteString(redStyle.Render(plural(failed, "failed job", "failed jobs")))
		b.WriteByte('\n')
		wrote = true
	}

	stale := staleLocks(m.snap.ResourceLocks)
	if len(stale) > 0 {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("stale locks"))
		b.WriteByte('\n')
		for _, l := range stale {
			b.WriteString(redStyle.Render(l.Key))
			b.WriteByte('\n')
		}
		wrote = true
	}

	if len(m.snap.BranchLocks) > 0 {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("branch locks"))
		b.WriteByte('\n')
		for _, l := range m.snap.BranchLocks {
			b.WriteString(l.Repo + " " + l.Branch + " (" + dash(l.Owner) + ")")
			b.WriteByte('\n')
		}
		wrote = true
	}

	if !wrote {
		return "Nothing needs attention."
	}
	return b.String()
}

func (m Model) answerOverlay() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("answer " + m.active.ID))
	b.WriteString("\n\n")
	b.WriteString(m.active.Question)
	b.WriteString("\n\n")
	switch m.mode {
	case modeAnswerChoice:
		for i, choice := range m.active.Choices {
			cursor, label := "  ", choice
			if i == m.choiceIdx {
				cursor, label = "▸ ", selectedRowStyle.Render(choice)
			}
			if choice == m.active.Default {
				label += mutedStyle.Render(" (default)")
			}
			b.WriteString(cursor + label + "\n")
		}
	case modeAnswerText:
		b.WriteString(m.input.View())
		b.WriteByte('\n')
	}
	if m.actionErr != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render(m.actionErr))
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("submitting…"))
	} else {
		b.WriteString(mutedStyle.Render("enter submit  esc cancel"))
	}
	return b.String()
}

func indexOf(items []string, value string) int {
	for i, item := range items {
		if item == value {
			return i
		}
	}
	return 0
}

func staleLocks(locks []ResourceLock) []ResourceLock {
	out := []ResourceLock{}
	for _, l := range locks {
		if l.Stale {
			out = append(out, l)
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
