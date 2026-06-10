package tui

import "strings"

// openTrainDetail shows the detail view for the train under the cursor.
func (m *Model) openTrainDetail() {
	if pages[m.selected].page != pageTrains || len(m.snap.Trains) == 0 {
		return
	}
	m.activeTrain = m.snap.Trains[m.trainCursor]
	m.mode = modeTrainDetail
}

func (m Model) trainsContent() string {
	if m.mode == modeTrainDetail {
		return m.trainDetail()
	}
	if len(m.snap.Trains) == 0 {
		return m.loadingOr("No train sessions.", !m.loadedAt.IsZero())
	}
	var b strings.Builder
	for i, t := range m.snap.Trains {
		cursor := "  "
		phase := t.Phase
		if deadTrainPhase(t.Phase) {
			phase = mutedStyle.Render(t.Phase)
		}
		line := t.ID + "  " + phase
		if i == m.trainCursor {
			cursor = "▸ "
			line = selectedRowStyle.Render(t.ID) + "  " + phase
		}
		b.WriteString(cursor + line + "\n")
	}
	b.WriteString(mutedStyle.Render("enter detail"))
	b.WriteByte('\n')
	return b.String()
}

func (m Model) trainDetail() string {
	t := m.activeTrain
	var b strings.Builder
	b.WriteString(headerStyle.Render(t.ID))
	b.WriteString("\n\n")
	rows := [][]string{
		{"phase", dash(t.Phase)},
		{"candidate", dash(t.Candidate)},
		{"repo", dash(t.Repo)},
	}
	b.WriteString(renderRows(rows))
	b.WriteByte('\n')
	b.WriteString(headerStyle.Render("locks"))
	b.WriteByte('\n')
	locks := trainLocks(m.snap.ResourceLocks, t.ID)
	if len(locks) == 0 {
		b.WriteString(mutedStyle.Render("none"))
		b.WriteByte('\n')
	} else {
		for _, l := range locks {
			state := "active"
			if l.Stale {
				state = redStyle.Render("stale")
			}
			b.WriteString(l.Key + "  " + state + "\n")
		}
	}
	return b.String()
}

// trainLocks returns the resource locks for a session. Train lock keys have the
// form "<resource>:<sessionID>[:<iterationID>]", so the session id is matched as
// a whole colon-delimited segment — substring matching would cross-match
// sessions whose ids are prefixes of one another (e.g. "s1" vs "s12").
func trainLocks(locks []ResourceLock, sessionID string) []ResourceLock {
	out := []ResourceLock{}
	for _, l := range locks {
		for _, seg := range strings.Split(l.Key, ":") {
			if seg == sessionID {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

func deadTrainPhase(phase string) bool {
	switch phase {
	case "run_abandoned", "candidate_rejected", "candidate_promoted":
		return true
	default:
		return false
	}
}
