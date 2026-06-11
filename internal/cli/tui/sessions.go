package tui

import (
	"fmt"
	"strings"

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
			label:   fmt.Sprintf("%s [%s] %s %s", s.Name, s.Runtime, dash(s.Repo), s.State),
			session: s,
			count:   1,
		})
	}
	return rows
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
	b.WriteString(mutedStyle.Render("The live codex/claude processes backing your Agents — one warm session per delivered job (up to max_background); idle ones expire on their own."))
	b.WriteString("\n\n")
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
	b.WriteString(mutedStyle.Render("enter detail"))
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
