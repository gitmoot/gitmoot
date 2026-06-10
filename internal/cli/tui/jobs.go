package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// openJobDetail enters the job detail view for the job under the Jobs cursor
// and lazily loads its event history.
func (m *Model) openJobDetail(job JobRow) tea.Cmd {
	m.activeJob = job
	m.jobEvents = nil
	m.jobEventsLoaded = false
	m.jobEventsErr = ""
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeJobDetail
	return jobEventsCmd(m.deps, job.ID)
}

// openJobConfirm enters the retry or cancel confirmation for the given job.
func (m *Model) openJobConfirm(job JobRow, cancel bool) {
	m.activeJob = job
	m.actionErr = ""
	m.actionBusy = false
	if cancel {
		m.mode = modeConfirmJobCancel
	} else {
		m.mode = modeConfirmJobRetry
	}
}

// jobUnderCursor returns the selected job for the current page (Jobs page
// cursor, or an attention job item), if any.
func (m Model) jobUnderCursor() (JobRow, bool) {
	switch pages[m.selected].page {
	case pageJobs:
		if len(m.snap.JobRows) == 0 {
			return JobRow{}, false
		}
		return m.snap.JobRows[m.jobCursor], true
	case pageAttention:
		items := m.attentionItems()
		if m.promptCursor < len(items) && items[m.promptCursor].job != nil {
			return *items[m.promptCursor].job, true
		}
	}
	return JobRow{}, false
}

// jobRetryable / jobCancelable mirror workflow.RetryJob / workflow.CancelJob's
// accepted source states.
func jobRetryable(state string) bool {
	return state == "failed" || state == "blocked" || state == "cancelled"
}

func jobCancelable(state string) bool {
	return state == "queued" || state == "running"
}

// updateJobOverlay handles keys in the job detail and confirm modes.
func (m Model) updateJobOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeJobDetail:
		switch msg.String() {
		case "esc", "enter", "q":
			m.mode = modeNormal
		case "R":
			if jobRetryable(m.activeJob.State) {
				m.mode = modeConfirmJobRetry
			}
		case "c":
			if jobCancelable(m.activeJob.State) {
				m.mode = modeConfirmJobCancel
			}
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeConfirmJobRetry, modeConfirmJobCancel:
		switch msg.String() {
		case "y", "Y":
			if m.actionBusy {
				return m, nil
			}
			m.actionBusy = true
			m.actionErr = ""
			var cmd tea.Cmd
			if m.mode == modeConfirmJobCancel {
				cmd = jobActionCmd(m.deps.CancelJob, "cancel", m.activeJob.ID)
			} else {
				cmd = jobActionCmd(m.deps.RetryJob, "retry", m.activeJob.ID)
			}
			m.viewport.SetContent(m.content())
			return m, cmd
		default:
			// While the action is in flight, keep the confirm open so its
			// eventual result (especially an error) is not silently dropped.
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

func jobEventsCmd(deps Deps, id string) tea.Cmd {
	return func() tea.Msg {
		if deps.JobEvents == nil {
			return jobEventsMsg{id: id}
		}
		events, err := deps.JobEvents(id)
		return jobEventsMsg{id: id, events: events, err: err}
	}
}

func jobActionCmd(action func(string) error, verb, id string) tea.Cmd {
	return func() tea.Msg {
		if action == nil {
			return jobActionMsg{verb: verb, id: id}
		}
		return jobActionMsg{verb: verb, id: id, err: action(id)}
	}
}

func daemonStartCmd(deps Deps) tea.Cmd {
	return func() tea.Msg {
		if deps.StartDaemon == nil {
			return daemonStartMsg{}
		}
		return daemonStartMsg{err: deps.StartDaemon()}
	}
}

// jobsContentInteractive renders the Jobs page as a selectable list. Job
// overlays (detail/confirm) are dispatched once in content(), not here.
func (m Model) jobsContentInteractive() string {
	if len(m.snap.JobRows) == 0 {
		return m.loadingOr("No jobs.", !m.loadedAt.IsZero())
	}
	var b strings.Builder
	summary := []string{}
	for _, state := range sortedKeys(m.snap.Jobs.ByState) {
		summary = append(summary, jobStateColor(state)+" "+strconv.Itoa(m.snap.Jobs.ByState[state]))
	}
	b.WriteString(mutedStyle.Render(strconv.Itoa(m.snap.Jobs.Total)+" total") + "  " + strings.Join(summary, "  "))
	b.WriteString("\n\n")
	for i, job := range m.snap.JobRows {
		cursor, id := "  ", job.ID
		state := job.State
		if _, pending := m.cancelling[job.ID]; pending && jobCancelable(job.State) {
			state = "cancelling…"
		}
		if i == m.jobCursor {
			cursor, id = "▸ ", selectedRowStyle.Render(job.ID)
		}
		b.WriteString(cursor + id + "  " + job.Agent + "  " + job.Type + "  " + jobStateColor(state) + "\n")
	}
	b.WriteString(mutedStyle.Render("enter detail  R retry  c cancel"))
	b.WriteByte('\n')
	return b.String()
}

func (m Model) jobDetailView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("job " + m.activeJob.ID))
	b.WriteString("\n\n")
	rows := [][]string{
		{"agent", dash(m.activeJob.Agent)},
		{"type", dash(m.activeJob.Type)},
		{"state", m.activeJob.State},
		{"updated", dash(m.activeJob.UpdatedAt)},
	}
	b.WriteString(renderRows(rows))
	b.WriteByte('\n')
	b.WriteString(headerStyle.Render("events"))
	b.WriteByte('\n')
	switch {
	case m.jobEventsErr != "":
		b.WriteString(errorStyle.Render(m.jobEventsErr) + "\n")
	case !m.jobEventsLoaded:
		b.WriteString(mutedStyle.Render("loading…") + "\n")
	case len(m.jobEvents) == 0:
		b.WriteString(mutedStyle.Render("no events") + "\n")
	default:
		const maxEvents = 20
		events := m.jobEvents
		if len(events) > maxEvents {
			b.WriteString(mutedStyle.Render(strconv.Itoa(len(events)-maxEvents)+" earlier events omitted") + "\n")
			events = events[len(events)-maxEvents:]
		}
		for _, event := range events {
			line := event.Kind
			if event.Message != "" {
				line += "  " + truncate(event.Message, 80)
			}
			b.WriteString(line + "\n")
		}
	}
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	return b.String()
}

func (m Model) jobConfirmView() string {
	verb := "Retry"
	if m.mode == modeConfirmJobCancel {
		verb = "Cancel"
	}
	var b strings.Builder
	b.WriteString(headerStyle.Render(verb + " job " + m.activeJob.ID))
	b.WriteString("\n\n")
	b.WriteString(m.activeJob.Type + " · " + m.activeJob.Agent + " · " + m.activeJob.State + "\n\n")
	b.WriteString(verb + " this job? (y/n)\n")
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("working…"))
	} else {
		b.WriteString(mutedStyle.Render("y confirm  n/esc cancel"))
	}
	return b.String()
}
