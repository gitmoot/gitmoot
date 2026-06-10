package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/jerryfane/gitmoot/internal/cli/style"
)

var (
	sidebarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")).
			Background(lipgloss.Color("236")).
			Padding(1, 1)
	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("16")).
			Background(lipgloss.Color("42")).
			Bold(true).
			Padding(0, 1)
	sidebarItemStyle = lipgloss.NewStyle().
				Padding(0, 1)
	bodyStyle = lipgloss.NewStyle().
			Padding(1, 1)
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39"))
	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("244"))
	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("203"))
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("81"))
	redStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	greenStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cyanStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("44"))
	selectedRowStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
)

func renderSidebar(selected, width, height int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("gitmoot"))
	b.WriteString("\n\n")
	for i, item := range pages {
		if i == selected {
			b.WriteString(selectedStyle.Width(max(1, width-4)).Render(item.label))
		} else {
			b.WriteString(sidebarItemStyle.Width(max(1, width-4)).Render(item.label))
		}
		b.WriteByte('\n')
	}
	for strings.Count(b.String(), "\n") < max(0, height-2) {
		b.WriteByte('\n')
	}
	return sidebarStyle.Width(max(10, width-2)).Height(max(1, height-2)).Render(b.String())
}

func (m Model) content() string {
	if m.showHelp {
		return m.helpContent()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(pages[m.selected].label))
	if !m.loadedAt.IsZero() {
		b.WriteString("  ")
		b.WriteString(mutedStyle.Render("updated " + m.loadedAt.Format("15:04:05")))
	}
	b.WriteString("\n\n")
	if m.loadErr != "" {
		b.WriteString(errorStyle.Render("refresh error: " + m.loadErr))
		b.WriteString("\n\n")
	}
	// Job overlays can be entered from more than one page (Jobs, Attention), so
	// they are dispatched once here rather than inside each page renderer.
	switch m.mode {
	case modeJobDetail:
		b.WriteString(m.jobDetailView())
	case modeConfirmJobRetry, modeConfirmJobCancel:
		b.WriteString(m.jobConfirmView())
	default:
		switch pages[m.selected].page {
		case pageAttention:
			b.WriteString(m.attentionContent())
		case pageTrains:
			b.WriteString(m.trainsContent())
		case pageAgents:
			b.WriteString(m.agentsContent())
		case pageSessions:
			b.WriteString(m.sessionsContent())
		case pageJobs:
			b.WriteString(m.jobsContentInteractive())
		case pageLocks:
			b.WriteString(m.locksContent())
		}
	}
	b.WriteString("\n\n")
	b.WriteString(mutedStyle.Render(m.footerHelp()))
	return b.String()
}

// helpContent is the '?' overlay: every key for the current page plus globals.
func (m Model) helpContent() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Help — " + pages[m.selected].label))
	b.WriteString("\n\n")
	switch pages[m.selected].page {
	case pageAttention:
		b.WriteString("↑/↓  select a row (pending prompts, then blocked/failed jobs)\n")
		b.WriteString("a    answer the selected prompt (choices or text)\n")
		b.WriteString("d    dismiss (delete) the selected prompt\n")
		b.WriteString("enter open the selected job's detail (events)\n")
		b.WriteString("R    retry the selected job\n")
		b.WriteString("s    start the daemon when it is stopped\n")
	case pageTrains:
		b.WriteString("↑/↓  select a train session\n")
		b.WriteString("enter open the session (live phase view; esc returns)\n")
	case pageJobs:
		b.WriteString("↑/↓  select a job\n")
		b.WriteString("enter open the job's detail (events)\n")
		b.WriteString("R    retry the selected job (failed/blocked/cancelled)\n")
		b.WriteString("c    cancel the selected job (queued/running)\n")
		b.WriteString("pgup/pgdn  scroll a long list\n")
	default:
		b.WriteString("j/k or wheel  scroll\n")
	}
	b.WriteString("\nGlobal:\n")
	b.WriteString("tab/shift+tab or ←/→  switch page\n")
	b.WriteString("r  refresh now\n")
	b.WriteString("?  close this help\n")
	b.WriteString("q  quit (background work keeps running)\n")
	return b.String()
}

// footerHelp is the key-hint line for the current page/mode.
func (m Model) footerHelp() string {
	switch m.mode {
	case modeAnswerChoice:
		return "↑/↓ choose  enter submit  esc cancel"
	case modeAnswerText:
		return "type answer  enter submit  esc cancel"
	case modeConfirmDismiss:
		return "y delete  n/esc cancel"
	case modeTrainDetail:
		return "enter/esc back"
	case modeJobDetail:
		return "R retry  c cancel  esc back"
	case modeConfirmJobRetry, modeConfirmJobCancel:
		return "y confirm  n/esc cancel"
	}
	switch pages[m.selected].page {
	case pageAttention:
		return "tab/←→ page  ↑/↓ select  a answer  d dismiss  enter/R jobs  ? help  q quit"
	case pageTrains:
		return "tab/←→ page  ↑/↓ select  enter open  r refresh  q quit"
	case pageJobs:
		return "tab/←→ page  ↑/↓ select  enter detail  R retry  c cancel  ? help  q quit"
	}
	return "tab/←→ page  j/k or wheel scroll  r refresh  q quit"
}

func (m Model) loadingOr(empty string, loaded bool) string {
	if loaded || !m.inFlight {
		return empty
	}
	return mutedStyle.Render("Loading…")
}

func (m Model) agentsContent() string {
	if len(m.snap.Agents) == 0 {
		return m.loadingOr("No registered agents.", !m.loadedAt.IsZero())
	}
	rows := [][]string{{"NAME", "RUNTIME", "ROLE", "HEALTH"}}
	for _, a := range m.snap.Agents {
		rows = append(rows, []string{a.Name, a.Runtime, dash(a.Role), dash(a.Health)})
	}
	return renderRows(rows)
}

func (m Model) sessionsContent() string {
	if len(m.snap.Sessions) == 0 {
		return m.loadingOr("No runtime sessions.", !m.loadedAt.IsZero())
	}
	var b strings.Builder
	for _, line := range groupedSessions(m.snap.Sessions) {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func (m Model) locksContent() string {
	if len(m.snap.BranchLocks) == 0 && len(m.snap.ResourceLocks) == 0 {
		return m.loadingOr("No active locks.", !m.loadedAt.IsZero())
	}
	var b strings.Builder
	b.WriteString(headerStyle.Render("branch locks"))
	b.WriteByte('\n')
	if len(m.snap.BranchLocks) == 0 {
		b.WriteString(mutedStyle.Render("none"))
		b.WriteByte('\n')
	} else {
		rows := [][]string{{"REPO", "BRANCH", "OWNER"}}
		for _, l := range m.snap.BranchLocks {
			rows = append(rows, []string{l.Repo, l.Branch, dash(l.Owner)})
		}
		b.WriteString(renderRows(rows))
	}
	b.WriteByte('\n')
	b.WriteString(headerStyle.Render("resource locks"))
	b.WriteByte('\n')
	if len(m.snap.ResourceLocks) == 0 {
		b.WriteString(mutedStyle.Render("none"))
		b.WriteByte('\n')
	} else {
		rows := [][]string{{"KEY", "OWNER", "STATE"}}
		for _, l := range m.snap.ResourceLocks {
			state := "active"
			if l.Stale {
				state = redStyle.Render("stale")
			}
			rows = append(rows, []string{l.Key, dash(l.Owner), state})
		}
		b.WriteString(renderRows(rows))
	}
	return b.String()
}

// groupedSessions collapses generated "<type>-bg-<hex>" sessions sharing a
// type/runtime/state into one counted line, mirroring cli.groupedRuntimeSessions.
func groupedSessions(sessions []Session) []string {
	type groupKey struct{ prefix, runtime, state string }
	order := []groupKey{}
	counts := map[groupKey]int{}
	singles := []Session{}
	for _, s := range sessions {
		if prefix, ok := style.GroupSuffix(s.Name); ok {
			key := groupKey{prefix: prefix, runtime: s.Runtime, state: s.State}
			if counts[key] == 0 {
				order = append(order, key)
			}
			counts[key]++
		} else {
			singles = append(singles, s)
		}
	}
	lines := make([]string, 0, len(order)+len(singles))
	for _, key := range order {
		lines = append(lines, fmt.Sprintf("%s [%s] ×%d %s", key.prefix, key.runtime, counts[key], key.state))
	}
	for _, s := range singles {
		lines = append(lines, fmt.Sprintf("%s [%s] %s %s", s.Name, s.Runtime, dash(s.Repo), s.State))
	}
	return lines
}

func jobStateColor(state string) string {
	switch state {
	case "failed", "blocked":
		return redStyle.Render(state)
	case "succeeded":
		return greenStyle.Render(state)
	case "running":
		return cyanStyle.Render(state)
	default:
		return state
	}
}

// renderRows aligns a table of cells into space-padded columns (two spaces
// between columns), header row bold. Widths are measured in runes and ignore
// ANSI escapes in cells, so color cells after alignment, not before.
func renderRows(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	widths := map[int]int{}
	for _, row := range rows {
		for i, cell := range row {
			if w := displayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	var b strings.Builder
	for rowIndex, row := range rows {
		for i, cell := range row {
			if i > 0 {
				b.WriteString("  ")
			}
			padded := padRight(cell, widths[i])
			if rowIndex == 0 {
				b.WriteString(headerStyle.Render(padded))
			} else {
				b.WriteString(padded)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// displayWidth is the printable cell width, ignoring ANSI escapes (so colored
// cells align with plain ones) and accounting for wide runes.
func displayWidth(s string) int {
	return ansi.StringWidth(s)
}

func padRight(value string, width int) string {
	pad := width - displayWidth(value)
	if pad <= 0 {
		return value
	}
	return value + strings.Repeat(" ", pad)
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sidebarWidth(total int) int {
	if total < 70 {
		return 14
	}
	return 18
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// truncate collapses internal whitespace and shortens value to limit runes with
// a trailing ellipsis.
func truncate(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return string(runes[:max(0, limit)])
	}
	return string(runes[:limit-1]) + "…"
}
