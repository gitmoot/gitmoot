package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// openTrainDetail shows the detail view for the train under the cursor.
func (m *Model) openTrainDetail() {
	if t, ok := m.trainUnderCursor(); ok {
		m.activeTrain = t
		m.mode = modeTrainDetail
	}
}

// trainUnderCursor returns the session under the Trains cursor, if any.
func (m Model) trainUnderCursor() (TrainSession, bool) {
	if pages[m.selected].page != pageTrains || len(m.snap.Trains) == 0 {
		return TrainSession{}, false
	}
	return m.snap.Trains[m.trainCursor], true
}

// openTrainStop enters the stop-reason overlay for a live session.
func (m *Model) openTrainStop(t TrainSession) tea.Cmd {
	m.activeTrain = t
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeTrainStopReason
	ti := textinput.New()
	ti.Placeholder = "why is this run being abandoned?"
	m.input = ti
	return m.input.Focus()
}

// openTrainDelete enters the delete confirmation for a terminal session.
func (m *Model) openTrainDelete(t TrainSession) {
	m.activeTrain = t
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeConfirmTrainDelete
}

// updateTrainOverlay handles keys in the stop/delete/repo-cleanup modes. Like
// the job confirms, an overlay stays open while its action is in flight so the
// eventual error is never dropped silently.
func (m Model) updateTrainOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeTrainStopReason:
		switch msg.String() {
		case "esc":
			if m.actionBusy {
				return m, nil
			}
			m.mode = modeNormal
			m.actionErr = ""
		case "enter":
			if m.actionBusy {
				return m, nil
			}
			reason := strings.TrimSpace(m.input.Value())
			if reason == "" {
				m.actionErr = "a reason is required"
			} else {
				m.actionBusy = true
				m.actionErr = ""
				m.viewport.SetContent(m.content())
				return m, trainStopCmd(m.deps, m.activeTrain.ID, reason)
			}
		default:
			// Freeze the reason while the stop is in flight so a retry after an
			// error submits exactly what is rendered.
			if m.actionBusy {
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			m.viewport.SetContent(m.content())
			return m, cmd
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeConfirmTrainDelete:
		switch msg.String() {
		case "y", "Y":
			if m.actionBusy {
				return m, nil
			}
			m.actionBusy = true
			m.actionErr = ""
			m.viewport.SetContent(m.content())
			return m, trainDeleteCmd(m.deps, m.activeTrain.ID)
		default:
			if m.actionBusy {
				return m, nil
			}
			m.mode = modeNormal
			m.actionErr = ""
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeConfirmTrainRepoCleanup:
		switch msg.String() {
		case "y", "Y":
			if m.actionBusy {
				return m, nil
			}
			m.actionBusy = true
			m.actionErr = ""
			m.viewport.SetContent(m.content())
			return m, trainRepoCleanupCmd(m.deps, m.pendingRepos)
		default:
			if m.actionBusy {
				return m, nil
			}
			// The session is already gone; declining just keeps the repos.
			m.mode = modeNormal
			m.actionErr = ""
			m.pendingRepos = nil
		}
		m.viewport.SetContent(m.content())
		return m, nil
	}
	return m, nil
}

func trainStopCmd(deps Deps, id, reason string) tea.Cmd {
	return func() tea.Msg {
		if deps.StopTrain == nil {
			return trainStopMsg{}
		}
		return trainStopMsg{err: deps.StopTrain(id, reason)}
	}
}

func trainDeleteCmd(deps Deps, id string) tea.Cmd {
	return func() tea.Msg {
		if deps.DeleteTrain == nil {
			return trainDeleteMsg{}
		}
		repos, err := deps.DeleteTrain(id)
		return trainDeleteMsg{repos: repos, err: err}
	}
}

func trainRepoCleanupCmd(deps Deps, repos []string) tea.Cmd {
	return func() tea.Msg {
		if deps.DeleteTrainRepo == nil {
			return trainRepoCleanupMsg{}
		}
		var failed, errs []string
		for _, repo := range repos {
			if err := deps.DeleteTrainRepo(repo); err != nil {
				failed = append(failed, repo)
				errs = append(errs, err.Error())
			}
		}
		return trainRepoCleanupMsg{failed: failed, errs: errs}
	}
}

func (m Model) trainStopView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("stop " + m.activeTrain.ID))
	b.WriteString("\n\n")
	b.WriteString("Stopping abandons the current run (phase " + dash(m.activeTrain.Phase) + ").\n\n")
	b.WriteString("reason: " + m.input.View())
	b.WriteByte('\n')
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("stopping…"))
	} else {
		b.WriteString(mutedStyle.Render("enter stop  esc cancel"))
	}
	return b.String()
}

func (m Model) trainDeleteConfirmView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Delete train " + m.activeTrain.ID))
	b.WriteString("\n\n")
	b.WriteString("phase " + dash(m.activeTrain.Phase) + " · " + dash(m.activeTrain.Repo) + "\n\n")
	b.WriteString("Delete this session and all its history? (y/n)\n")
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("deleting…"))
	} else {
		b.WriteString(mutedStyle.Render("y delete  n/esc cancel"))
	}
	return b.String()
}

func (m Model) trainRepoCleanupView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("session deleted"))
	b.WriteString("\n\n")
	b.WriteString("gitmoot created these GitHub repos for it:\n")
	for _, repo := range m.pendingRepos {
		b.WriteString("  " + repo + "\n")
	}
	b.WriteString("\nAlso delete them from GitHub? (y/n)\n")
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("deleting repos…"))
	} else {
		b.WriteString(mutedStyle.Render("y delete repos  n/esc keep them"))
	}
	return b.String()
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
	b.WriteString(mutedStyle.Render("enter open  s stop  d delete"))
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

// deadTrainPhase reports whether a session is in a terminal state (the
// canonical list lives in skillopt, so new terminal states gate correctly
// here without a second hand-kept copy).
func deadTrainPhase(phase string) bool {
	return skillopt.IsTerminalTrainState(phase)
}
