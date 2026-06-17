package tui

import "strings"

// listRow is one row of a collapsible grouped list (Attention, Trains). Rows
// come in three kinds:
//
//   - leaf: a selectable item. itemIdx indexes the page's ordered-items slice so
//     actions resolve to the right item; groupKey names the collapsible group it
//     belongs to (empty if none), so collapsing from a leaf knows what to fold.
//   - header: a collapsible group title (e.g. a repo). It is a cursor stop ONLY
//     when collapsed (so it can be re-expanded); when expanded it is display-only
//     and the cursor flows straight to its items.
//   - static: a display-only line (a section title, a lineage parent, a lock).
//     Never a cursor stop.
//
// Keeping the cursor on items (plus collapsed headers) means the common,
// all-expanded view navigates item→item exactly as an ungrouped list would.
type listRow struct {
	header   bool
	static   bool
	key      string // header: stable collapse key
	groupKey string // leaf: the collapse key of its group (space folds it)
	depth    int
	text     string // rendered content (no indent / cursor marker)
	itemIdx  int    // leaf: index into the page's ordered items; -1 otherwise

	collapsed bool // header: current collapse state, stamped by visibleListRows
}

func staticRow(depth int, text string) listRow {
	return listRow{static: true, depth: depth, text: text, itemIdx: -1}
}

func headerRow(key string, depth int, text string) listRow {
	return listRow{header: true, key: key, depth: depth, text: text, itemIdx: -1}
}

func leafRow(depth int, text string, itemIdx int, groupKey string) listRow {
	return listRow{depth: depth, text: text, itemIdx: itemIdx, groupKey: groupKey}
}

// selectable reports whether the cursor can land on this row: leaves always, and
// headers only while collapsed (so a folded group can be reopened). Static rows
// and expanded headers are display-only.
func (r listRow) selectable() bool {
	if r.header {
		return r.collapsed
	}
	return !r.static
}

// visibleListRows returns the rows visible given the collapsed set: a row is
// hidden when an ancestor header (a shallower header before it) is collapsed.
// Header rows stay visible and are stamped with their collapse state.
func visibleListRows(rows []listRow, collapsed map[string]bool) []listRow {
	out := make([]listRow, 0, len(rows))
	hideDepth := -1 // when >=0, hide rows deeper than this until we return to it
	for _, r := range rows {
		if hideDepth >= 0 {
			if r.depth > hideDepth {
				continue
			}
			hideDepth = -1
		}
		if r.header {
			r.collapsed = collapsed[r.key]
		}
		out = append(out, r)
		if r.header && collapsed[r.key] {
			hideDepth = r.depth
		}
	}
	return out
}

// selectableCount is how many of the visible rows the cursor can land on.
func selectableCount(rows []listRow) int {
	n := 0
	for _, r := range rows {
		if r.selectable() {
			n++
		}
	}
	return n
}

// selectableRowAt returns the cursor-th selectable row among the visible rows.
func selectableRowAt(rows []listRow, cursor int) (listRow, bool) {
	if cursor < 0 {
		return listRow{}, false
	}
	n := 0
	for _, r := range rows {
		if !r.selectable() {
			continue
		}
		if n == cursor {
			return r, true
		}
		n++
	}
	return listRow{}, false
}

// selectedItemIndex returns the page-items index of the leaf under the cursor,
// or -1 when the cursor is on a (collapsed) header or out of range.
func selectedItemIndex(rows []listRow, cursor int) int {
	r, ok := selectableRowAt(rows, cursor)
	if !ok || r.header {
		return -1
	}
	return r.itemIdx
}

// renderListRows writes the visible rows into b. cursor is the index among the
// SELECTABLE rows of the highlighted row. Headers show a [-]/[+] collapse marker;
// the selected row gets the "▸ " cursor marker. Indent follows depth.
func renderListRows(b *strings.Builder, rows []listRow, cursor int) {
	sel := 0
	for _, r := range rows {
		indent := strings.Repeat("  ", r.depth)
		selected := r.selectable() && sel == cursor
		marker := "  "
		if selected {
			marker = "▸ "
		}
		switch {
		case r.header:
			collapseMark := "[-] "
			if r.collapsed {
				collapseMark = "[+] "
			}
			label := r.text
			if !selected {
				if r.depth == 0 {
					label = headerStyle.Render(label)
				} else {
					label = mutedStyle.Render(label)
				}
			}
			b.WriteString(indent + marker + collapseMark + label + "\n")
		case r.static:
			label := r.text
			if r.depth == 0 {
				label = headerStyle.Render(label)
			} else {
				label = mutedStyle.Render(label)
			}
			b.WriteString(indent + "  " + label + "\n")
		default:
			b.WriteString(indent + marker + r.text + "\n")
		}
		if r.selectable() {
			sel++
		}
	}
}
