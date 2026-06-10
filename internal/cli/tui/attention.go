package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

// attnItem is one selectable row on the Attention page: a pending prompt or a
// blocked/failed job.
type attnItem struct {
	prompt *db.InteractivePrompt
	job    *JobRow
}

// attentionItems lists the actionable attention rows: pending prompts first,
// then blocked/failed jobs.
func (m Model) attentionItems() []attnItem {
	items := make([]attnItem, 0, len(m.snap.Prompts))
	for i := range m.snap.Prompts {
		items = append(items, attnItem{prompt: &m.snap.Prompts[i]})
	}
	for i := range m.snap.JobRows {
		if m.snap.JobRows[i].State == "blocked" || m.snap.JobRows[i].State == "failed" {
			items = append(items, attnItem{job: &m.snap.JobRows[i]})
		}
	}
	return items
}

// attentionPrompt returns the prompt under the attention cursor, if the
// selected item is a prompt.
func (m Model) attentionPrompt() (db.InteractivePrompt, bool) {
	items := m.attentionItems()
	if m.promptCursor < len(items) && items[m.promptCursor].prompt != nil {
		return *items[m.promptCursor].prompt, true
	}
	return db.InteractivePrompt{}, false
}

// openAnswer enters the answer overlay for the prompt under the cursor. Choice
// prompts get a selectable list with the default preselected; free-text prompts
// get a text input prefilled with the default. Returns the textinput blink cmd
// when relevant.
func (m *Model) openAnswer() tea.Cmd {
	if pages[m.selected].page != pageAttention {
		return nil
	}
	p, ok := m.attentionPrompt()
	if !ok {
		return nil
	}
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

// openDismiss enters the dismiss-confirm overlay for the prompt under the cursor.
func (m *Model) openDismiss() {
	if pages[m.selected].page != pageAttention {
		return
	}
	p, ok := m.attentionPrompt()
	if !ok {
		return
	}
	m.active = p
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeConfirmDismiss
}

func dismissCmd(deps Deps, id string) tea.Cmd {
	return func() tea.Msg {
		if deps.Dismiss == nil {
			return dismissResultMsg{id: id}
		}
		return dismissResultMsg{id: id, err: deps.Dismiss(id)}
	}
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
	case modeConfirmDismiss:
		switch msg.String() {
		case "y", "Y":
			if !m.actionBusy {
				m.actionBusy = true
				m.actionErr = ""
				cmd := dismissCmd(m.deps, m.active.ID)
				m.viewport.SetContent(m.content())
				return m, cmd
			}
		default:
			// Any other key cancels the dismissal.
			m.mode = modeNormal
			m.actionErr = ""
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeTrainDetail:
		if msg.String() == "enter" {
			m.mode = modeNormal
			m.viewport.SetContent(m.content())
		}
		return m, nil
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
	switch m.mode {
	case modeAnswerChoice, modeAnswerText:
		return m.answerOverlay()
	case modeConfirmDismiss:
		return m.dismissOverlay()
	}
	var b strings.Builder
	wrote := false

	if !m.snap.Daemon.Running && !m.loadedAt.IsZero() {
		hint := "press s to start"
		if m.daemonBusy {
			hint = "starting…"
		}
		b.WriteString(redStyle.Render("daemon stopped — jobs will not run") + "  " + mutedStyle.Render(hint))
		b.WriteByte('\n')
		if m.daemonErr != "" {
			b.WriteString(errorStyle.Render(m.daemonErr))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		wrote = true
	}

	items := m.attentionItems()
	b.WriteString(headerStyle.Render("needs attention"))
	b.WriteByte('\n')
	if len(items) == 0 {
		b.WriteString(mutedStyle.Render("none"))
		b.WriteByte('\n')
	} else {
		for i, item := range items {
			cursor := "  "
			var line string
			switch {
			case item.prompt != nil:
				line = "prompt " + item.prompt.ID + "  " + truncate(item.prompt.Question, 56)
			case item.job != nil:
				line = "job " + item.job.ID + "  " + item.job.Type + "  " + jobStateColor(item.job.State)
				if item.job.LatestEvent != "" {
					line += "  " + mutedStyle.Render(truncate(item.job.LatestEvent, 48))
				}
			}
			if i == m.promptCursor {
				cursor = "▸ "
				line = selectedRowStyle.Render("• ") + line
			}
			b.WriteString(cursor + line + "\n")
		}
		// Attention only lists blocked/failed jobs, so cancel never applies here.
		b.WriteString(mutedStyle.Render("prompts: a answer · d dismiss   jobs: enter detail · R retry"))
		b.WriteByte('\n')
		wrote = true
	}

	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
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

func (m Model) dismissOverlay() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("dismiss " + m.active.ID))
	b.WriteString("\n\n")
	b.WriteString(truncate(m.active.Question, 70))
	b.WriteString("\n\n")
	b.WriteString("Delete this prompt? (y/n)")
	b.WriteByte('\n')
	if m.actionErr != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render(m.actionErr))
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("dismissing…"))
	} else {
		b.WriteString(mutedStyle.Render("y delete  n/esc cancel"))
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
