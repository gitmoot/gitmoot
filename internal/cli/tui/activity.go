package tui

import (
	"strconv"
	"strings"
)

// activitySelectable is the flat list of jobs the Activity cursor can land on —
// each active root followed by its delegation children — so enter can open the
// detail (request + result) of a root OR a specific delegate.
func (m Model) activitySelectable() []JobRow {
	var out []JobRow
	for _, r := range m.snap.Activity {
		out = append(out, JobRow{ID: r.JobID, Agent: r.Agent, Type: r.Action, State: r.State, UpdatedAt: r.UpdatedAt})
		for _, c := range r.Children {
			out = append(out, JobRow{ID: c.ID, Agent: c.Agent, Type: c.Action, State: c.State})
		}
	}
	return out
}

// activitySelectableLen counts the selectable rows (root + children per tree)
// without materializing the []JobRow — the cursor length needed on every
// up/down keystroke and refresh clamp.
func (m Model) activitySelectableLen() int {
	n := 0
	for _, r := range m.snap.Activity {
		n += 1 + len(r.Children)
	}
	return n
}

// activityUnderCursor returns the job (root or delegate) under the Activity
// cursor, if any.
func (m Model) activityUnderCursor() (JobRow, bool) {
	if pages[m.selected].page != pageActivity {
		return JobRow{}, false
	}
	sel := m.activitySelectable()
	if m.activityCursor < 0 || m.activityCursor >= len(sel) {
		return JobRow{}, false
	}
	return sel[m.activityCursor], true
}

// activityWindowCap is how many display rows fit the Activity page. The viewport
// is height-4; the page also renders the title block (2), the intro (2), both
// scroll markers (2), and the footer (1) = 7 lines of chrome, so the window keeps
// to height-11 to stay inside the viewport even when both markers show.
func activityWindowCap(height int) int {
	if height-11 < 3 {
		return 3
	}
	return height - 11
}

// activityRow is one rendered line of the Activity page. Selectable rows (a root
// or a delegation child) carry the cursor marker and map to an activitySelectable
// index; adornment rows (the progress summary, the continuation, blank
// separators) are static.
type activityRow struct {
	selectable bool
	sel        int    // activitySelectable index, when selectable
	indent     string // spaces before the marker
	target     string // text highlighted when this row is selected
	rest       string // text after the target
	static     string // full text for non-selectable rows
}

// activityRows flattens the delegation trees into display rows, assigning each
// selectable row the same index activitySelectable() produces (root, then its
// children, per tree) so the cursor and the rendered marker stay in lockstep.
func (m Model) activityRows() []activityRow {
	var rows []activityRow
	sel := 0
	for ri, r := range m.snap.Activity {
		if ri > 0 {
			rows = append(rows, activityRow{static: ""}) // blank separator between trees
		}
		rootRest := "  " + r.Agent + "  " + r.Action + "  " + jobStateColor(r.State)
		if r.Repo != "" {
			rootRest += "  " + mutedStyle.Render(r.Repo)
		}
		rows = append(rows, activityRow{selectable: true, sel: sel, target: r.JobID, rest: rootRest})
		sel++
		if r.Total > 0 {
			rows = append(rows, activityRow{static: "    " + mutedStyle.Render(
				strconv.Itoa(r.Total)+" delegations · "+
					strconv.Itoa(r.Running)+" running · "+
					strconv.Itoa(r.Queued)+" queued · "+
					strconv.Itoa(r.Blocked)+" blocked · "+
					strconv.Itoa(r.Done)+" done")})
		}
		for _, c := range r.Children {
			rows = append(rows, activityRow{
				selectable: true,
				sel:        sel,
				indent:     "    ",
				target:     dash(c.Agent),
				rest:       "  " + truncate(c.Action, 24) + "  " + jobStateColor(c.State),
			})
			sel++
		}
		// Show the continuation whenever one exists — including the corrective
		// path where the coordinator re-enqueues a continuation with no fresh
		// delegations (Total == 0), so its live work is never hidden.
		if r.ContinuationID != "" {
			rows = append(rows, activityRow{static: "      " + mutedStyle.Render("continuation") + "  " + jobStateColor(r.ContinuationState)})
		}
	}
	return rows
}

// activityContent renders the Activity page: delegation trees with
// queued/running work, newest first. Each root shows the coordinator line, a
// progress summary, and the delegation children (which agent is doing what, and
// its state) plus the continuation job. The cursor walks roots AND children;
// enter opens the selected job's detail (its request + result). Display rows are
// windowed around the cursor so the page always fits and the selected row stays
// visible — even for a single wide fan-out — with "↑/↓ N more rows" markers for
// what is scrolled off.
func (m Model) activityContent() string {
	if len(m.snap.Activity) == 0 {
		return m.loadingOr("No active jobs — nothing is running right now.", !m.loadedAt.IsZero())
	}

	rows := m.activityRows()
	capacity := activityWindowCap(m.height)

	// Clamp the effective cursor into the selectable range so the row search
	// below always matches — even if a future code path mutates m.snap.Activity
	// without re-clamping m.activityCursor.
	selCount := 0
	for _, row := range rows {
		if row.selectable {
			selCount++
		}
	}
	cursor := m.activityCursor
	if cursor < 0 {
		cursor = 0
	}
	if selCount > 0 && cursor > selCount-1 {
		cursor = selCount - 1
	}

	// Locate the selected display row, then window around it (mirrors the Jobs
	// page) so the cursor is always within the rendered slice.
	cursorRow := 0
	for i, row := range rows {
		if row.selectable && row.sel == cursor {
			cursorRow = i
			break
		}
	}
	start := 0
	if len(rows) > capacity {
		start = cursorRow - capacity/2
		if start < 0 {
			start = 0
		}
		if start > len(rows)-capacity {
			start = len(rows) - capacity
		}
	}
	end := start + capacity
	if end > len(rows) {
		end = len(rows)
	}

	var b strings.Builder
	b.WriteString(mutedStyle.Render("Delegation trees with queued/running work (live, refreshes every 5s).") + "\n\n")
	if start > 0 {
		b.WriteString(mutedStyle.Render("  ↑ "+strconv.Itoa(start)+" more rows above") + "\n")
	}
	for i := start; i < end; i++ {
		row := rows[i]
		if !row.selectable {
			b.WriteString(row.static + "\n")
			continue
		}
		marker := "  "
		target := row.target
		if row.sel == cursor {
			marker = "▸ "
			target = selectedRowStyle.Render(target)
		}
		b.WriteString(row.indent + marker + target + row.rest + "\n")
	}
	if end < len(rows) {
		b.WriteString(mutedStyle.Render("  ↓ "+strconv.Itoa(len(rows)-end)+" more rows below") + "\n")
	}
	b.WriteString(mutedStyle.Render("↑/↓ select root or delegate · enter open its detail (request + result)"))
	b.WriteByte('\n')
	return b.String()
}
