package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/cli/style"
)

// sessionRow is one rendered line on the Sessions page. Generated background
// sessions sharing a type/runtime/state collapse into one counted group; named
// sessions stand alone. session is the row's representative instance (the first
// member of a group), used by the detail view.
type sessionRow struct {
	label   string
	session Session
	count   int // >1 means a collapsed background group
}

// sessionRows collapses generated "<type>-bg-<hex>" sessions sharing a
// type/runtime/state into one counted line (mirroring cli.groupedRuntimeSessions)
// while keeping each row's representative session for the detail view.
func (m Model) sessionRows() []sessionRow {
	type groupKey struct{ prefix, runtime, state string }
	order := []groupKey{}
	counts := map[groupKey]int{}
	rep := map[groupKey]Session{}
	singles := []Session{}
	for _, s := range m.snap.Sessions {
		if prefix, ok := style.GroupSuffix(s.Name); ok {
			key := groupKey{prefix: prefix, runtime: s.Runtime, state: s.State}
			if counts[key] == 0 {
				order = append(order, key)
				rep[key] = s
			}
			counts[key]++
		} else {
			singles = append(singles, s)
		}
	}
	rows := make([]sessionRow, 0, len(order)+len(singles))
	for _, key := range order {
		rows = append(rows, sessionRow{
			label:   fmt.Sprintf("%s [%s] ×%d %s", key.prefix, key.runtime, counts[key], key.state),
			session: rep[key],
			count:   counts[key],
		})
	}
	for _, s := range singles {
		rows = append(rows, sessionRow{
			label:   fmt.Sprintf("%s (%s) [%s] %s %s", s.Name, dash(s.Type), s.Runtime, dash(s.Repo), s.State),
			session: s,
			count:   1,
		})
	}
	return rows
}

// updateSessionOverlay handles keys in the session detail and stop-confirm
// modes. Like the job overlay it guards esc/keys while a stop is in flight so
// the result (especially a refusal) is never dropped.
func (m Model) updateSessionOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeSessionDetail:
		if s := msg.String(); s == "enter" || s == "q" || s == "esc" {
			m.mode = modeNormal
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeConfirmSessionStop:
		switch msg.String() {
		case "y", "Y":
			if m.actionBusy {
				return m, nil
			}
			m.actionBusy = true
			m.actionErr = ""
			cmd := sessionStopCmd(m.deps, m.activeSession.Name)
			m.viewport.SetContent(m.content())
			return m, cmd
		default:
			// While the stop is in flight, keep the confirm open so its result
			// (especially an error like "running a job") is not dropped — this is
			// why the overlay has its own handler rather than the shared esc path.
			if m.actionBusy {
				return m, nil
			}
			m.mode = modeNormal
			m.actionErr = ""
		}
		m.viewport.SetContent(m.content())
		return m, nil
	}
	return m, nil
}

// openSessionStop opens the stop confirmation for a single session. Collapsed
// background groups aren't individually addressable, so a group row instead
// sets a muted hint pointing at `gitmoot agent gc`.
func (m *Model) openSessionStop(row sessionRow) {
	m.actionErr = ""
	m.actionBusy = false
	if row.count > 1 {
		m.sessionNotice = "background workers expire on their own — clear them with `gitmoot agent gc`"
		return
	}
	m.sessionNotice = ""
	m.activeSession = row.session
	m.activeSessionCount = row.count
	m.mode = modeConfirmSessionStop
}

func sessionStopCmd(deps Deps, name string) tea.Cmd {
	return func() tea.Msg {
		if deps.StopSession == nil {
			return sessionActionMsg{}
		}
		return sessionActionMsg{err: deps.StopSession(name)}
	}
}

func (m Model) sessionStopConfirmView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Stop session " + m.activeSession.Name))
	b.WriteString("\n\n")
	b.WriteString(dash(m.activeSession.Type) + " · " + dash(m.activeSession.Runtime) + " · " + dash(m.activeSession.State) + "\n\n")
	b.WriteString("Stop this runtime session? The next job starts a fresh one. (y/n)\n")
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("stopping…"))
	} else {
		b.WriteString(mutedStyle.Render("y confirm  n/esc cancel"))
	}
	return b.String()
}

// openSessionDetail enters the detail view for the session under the cursor.
func (m *Model) openSessionDetail() {
	rows := m.sessionRows()
	if m.sessionCursor >= len(rows) || m.sessionCursor < 0 {
		return
	}
	m.activeSession = rows[m.sessionCursor].session
	m.activeSessionCount = rows[m.sessionCursor].count
	m.mode = modeSessionDetail
}

func (m Model) sessionsContent() string {
	var b strings.Builder
	// Kept as short lines so the key phrases survive viewport wrapping.
	b.WriteString(mutedStyle.Render("Live runtime processes (codex/claude/kimi) that execute jobs for your agents.") + "\n")
	b.WriteString(mutedStyle.Render("Each row shows its owning agent type.") + "\n")
	b.WriteString(mutedStyle.Render("Stopping one frees the warm process; it does NOT cancel a running job.") + "\n")
	b.WriteString(mutedStyle.Render("Idle sessions expire on their own.") + "\n")
	b.WriteString("\n")
	rows := m.sessionRows()
	if len(rows) == 0 {
		b.WriteString(m.loadingOr("No runtime sessions.", !m.loadedAt.IsZero()))
		return b.String()
	}
	for i, row := range rows {
		cursor, label := "  ", row.label
		if i == m.sessionCursor {
			cursor, label = "▸ ", selectedRowStyle.Render(row.label)
		}
		b.WriteString(cursor + label + "\n")
	}
	b.WriteByte('\n')
	hint := "enter detail"
	if m.deps.StopSession != nil {
		hint += "  s stop"
	}
	b.WriteString(mutedStyle.Render(hint))
	if m.sessionNotice != "" {
		b.WriteString("\n" + mutedStyle.Render(m.sessionNotice))
	}
	return b.String()
}

func (m Model) sessionDetailView() string {
	var b strings.Builder
	s := m.activeSession
	if m.activeSessionCount > 1 {
		b.WriteString(headerStyle.Render(fmt.Sprintf("runtime sessions  %s ×%d", dash(s.Type), m.activeSessionCount)))
	} else {
		b.WriteString(headerStyle.Render("runtime session " + s.Name))
	}
	b.WriteString("\n\n")
	rows := [][]string{
		{"type", dash(s.Type)},
		{"runtime", dash(s.Runtime)},
		{"role", dash(s.Role)},
		{"repo", dash(s.Repo)},
		{"state", dash(s.State)},
		{"template", dash(s.Template)},
		{"last used", dash(s.LastUsed)},
		{"expires", dash(s.Expires)},
	}
	b.WriteString(renderRows(rows))
	b.WriteByte('\n')
	if m.activeSessionCount > 1 {
		b.WriteString("\n" + mutedStyle.Render(fmt.Sprintf("%d ephemeral background workers share this type/runtime/state; the fields above are the first one's.", m.activeSessionCount)) + "\n")
	}
	b.WriteString("\n" + mutedStyle.Render("esc back"))
	return b.String()
}
