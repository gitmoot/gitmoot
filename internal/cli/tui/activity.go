package tui

import (
	"strconv"
	"strings"
)

// activityUnderCursor returns the active delegation root under the Activity
// cursor, if any.
func (m Model) activityUnderCursor() (ActivityRoot, bool) {
	if pages[m.selected].page != pageActivity || m.activityCursor < 0 || m.activityCursor >= len(m.snap.Activity) {
		return ActivityRoot{}, false
	}
	return m.snap.Activity[m.activityCursor], true
}

// activityContent renders the Activity page: every delegation tree with
// queued/running work, newest first. Each root shows the coordinator line, a
// progress summary, and the delegation children (which agent is doing what, and
// its state) plus the continuation job. enter opens the root's full detail.
func (m Model) activityContent() string {
	roots := m.snap.Activity
	if len(roots) == 0 {
		return m.loadingOr("No active jobs — nothing is running right now.", !m.loadedAt.IsZero())
	}
	var b strings.Builder
	b.WriteString(mutedStyle.Render("Delegation trees with queued/running work (live, refreshes every 5s).") + "\n\n")
	for i, r := range roots {
		cursor := "  "
		id := r.JobID
		if i == m.activityCursor {
			cursor = "▸ "
			id = selectedRowStyle.Render(id)
		}
		line := cursor + id + "  " + r.Agent + "  " + r.Action + "  " + jobStateColor(r.State)
		if r.Repo != "" {
			line += "  " + mutedStyle.Render(r.Repo)
		}
		b.WriteString(line + "\n")

		if r.Total > 0 {
			b.WriteString("    " + mutedStyle.Render(
				strconv.Itoa(r.Total)+" delegations · "+
					strconv.Itoa(r.Running)+" running · "+
					strconv.Itoa(r.Queued)+" queued · "+
					strconv.Itoa(r.Blocked)+" blocked · "+
					strconv.Itoa(r.Done)+" done") + "\n")
			for _, c := range r.Children {
				b.WriteString("      " + dash(c.Agent) + "  " + truncate(c.Action, 30) + "  " + jobStateColor(c.State) + "\n")
			}
			if r.ContinuationID != "" {
				b.WriteString("      " + mutedStyle.Render("continuation") + "  " + jobStateColor(r.ContinuationState) + "\n")
			}
		}
		b.WriteByte('\n')
	}
	b.WriteString(mutedStyle.Render("↑/↓ select · enter open root detail"))
	b.WriteByte('\n')
	return b.String()
}
