package tui

import (
	"errors"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// openJobDetail enters the job detail view for the job under the Jobs cursor
// and lazily loads its event history and parsed payload (request/result).
func (m *Model) openJobDetail(job JobRow) tea.Cmd {
	m.activeJob = job
	m.jobEvents = nil
	m.jobEventsLoaded = false
	m.jobEventsErr = ""
	m.activeJobDetail = JobDetail{}
	m.jobDetailLoaded = false
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeJobDetail
	return tea.Batch(jobEventsCmd(m.deps, job.ID), jobDetailCmd(m.deps, job.ID))
}

// openBugReportPreview enters the redacted report preview for a reportable job.
func (m *Model) openBugReportPreview(job JobRow) tea.Cmd {
	m.activeJob = job
	m.bugReport = BugReportPreview{}
	m.bugReportLoaded = false
	m.bugReportErr = ""
	m.bugReportURL = ""
	m.bugReportExisting = false
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeBugReportPreview
	return bugReportPreviewCmd(m.deps, job.ID)
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

func jobReportable(state string) bool {
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
		case "B":
			if jobReportable(m.activeJob.State) {
				cmd := m.openBugReportPreview(m.activeJob)
				m.viewport.SetContent(m.content())
				return m, cmd
			}
		case "c":
			if jobCancelable(m.activeJob.State) {
				m.mode = modeConfirmJobCancel
			}
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeBugReportPreview:
		switch msg.String() {
		case "esc", "q":
			if m.actionBusy {
				return m, nil
			}
			m.mode = modeNormal
			m.actionErr = ""
			m.actionBusy = false
		case "g":
			if m.deps.CreateBugReport == nil || m.actionBusy || !m.bugReportLoaded || m.bugReportErr != "" || m.bugReportURL != "" {
				return m, nil
			}
			m.actionBusy = true
			m.actionErr = ""
			m.viewport.SetContent(m.content())
			return m, bugReportCreateCmd(m.deps, m.activeJob.ID, m.bugReport)
		default:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			m.viewport.SetContent(m.content())
			return m, cmd
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

func jobDetailCmd(deps Deps, id string) tea.Cmd {
	return func() tea.Msg {
		if deps.JobDetail == nil {
			return jobDetailMsg{id: id}
		}
		detail, err := deps.JobDetail(id)
		return jobDetailMsg{id: id, detail: detail, err: err}
	}
}

func bugReportPreviewCmd(deps Deps, id string) tea.Cmd {
	return func() tea.Msg {
		if deps.BugReportPreview == nil {
			return bugReportPreviewMsg{id: id, err: errors.New("bug report preview is not available")}
		}
		preview, err := deps.BugReportPreview(id)
		return bugReportPreviewMsg{id: id, preview: preview, err: err}
	}
}

func bugReportCreateCmd(deps Deps, id string, preview BugReportPreview) tea.Cmd {
	return func() tea.Msg {
		if deps.CreateBugReport == nil {
			return bugReportCreateMsg{id: id, err: errors.New("bug report issue creation is not available in this build")}
		}
		result, err := deps.CreateBugReport(id, preview)
		return bugReportCreateMsg{id: id, url: result.URL, existing: result.Existing, err: err}
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

// clampLines preserves newlines (unlike truncate, which collapses whitespace)
// but caps the block to maxLines, appending an elision marker when it overflows.
func clampLines(value string, maxLines int) string {
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n")
	}
	kept := lines[:maxLines]
	return strings.Join(kept, "\n") + "\n" + mutedStyle.Render("… "+strconv.Itoa(len(lines)-maxLines)+" more lines")
}

// jobDecisionColor colors a gitmoot_result decision: blocked/failed red,
// changes_requested neutral, everything else (approved/implemented) green.
func jobDecisionColor(decision string) string {
	switch decision {
	case "blocked", "failed":
		return redStyle.Render(decision)
	case "approved", "implemented":
		return greenStyle.Render(decision)
	default:
		return decision
	}
}

// depsLabel renders a delegation child's deps for the delegation tree: "-" when
// it has none, else the comma-joined dep ids with a satisfied/pending suffix.
func depsLabel(c JobChild) string {
	if len(c.Deps) == 0 {
		return "-"
	}
	suffix := " (pending)"
	if c.DepsSatisfied {
		suffix = " (satisfied)"
	}
	return strings.Join(c.Deps, ",") + suffix
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
	// Window the list so the cursor stays on-screen even with hundreds of jobs;
	// the cursor is kept roughly centered (stateless — derived from jobCursor).
	rows := m.snap.JobRows
	n := len(rows)
	capacity := jobsWindowCap(m.height)
	start := 0
	if n > capacity {
		start = m.jobCursor - capacity/2
		if start < 0 {
			start = 0
		}
		if start > n-capacity {
			start = n - capacity
		}
	}
	end := start + capacity
	if end > n {
		end = n
	}
	if start > 0 {
		b.WriteString(mutedStyle.Render("↑ "+strconv.Itoa(start)+" earlier") + "\n")
	}
	for i := start; i < end; i++ {
		job := rows[i]
		cursor, id := "  ", job.ID
		state := job.State
		if _, pending := m.cancelling[job.ID]; pending && jobCancelable(job.State) {
			state = "cancelling…"
		}
		if i == m.jobCursor {
			cursor, id = "▸ ", selectedRowStyle.Render(job.ID)
		}
		b.WriteString(cursor + id + "  " + job.Agent + "  " + job.Type + "  " + jobStateColor(state) +
			"  " + mutedStyle.Render(formatJobTime(job.UpdatedAt)) + "\n")
	}
	if end < n {
		b.WriteString(mutedStyle.Render("↓ "+strconv.Itoa(n-end)+" more") + "\n")
	}
	help := "enter detail  R retry  c cancel"
	if job, ok := m.jobUnderCursor(); ok && jobReportable(job.State) {
		help += "  B report bug"
	}
	b.WriteString(mutedStyle.Render(help))
	b.WriteByte('\n')
	return b.String()
}

// jobsWindowCap is how many job rows fit the Jobs page, leaving room for the
// title, summary, more/earlier markers, and footer.
func jobsWindowCap(height int) int {
	if height-9 < 3 {
		return 3
	}
	return height - 9
}

// formatJobTime renders a stored ISO timestamp ("2006-01-02 15:04:05", UTC from
// SQLite) as a compact "MM-DD HH:MM". A value it can't parse is trimmed to its
// first 16 chars so the column never balloons.
func formatJobTime(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("01-02 15:04")
		}
	}
	if r := []rune(value); len(r) > 16 {
		return string(r[:16])
	}
	return value
}

func (m Model) jobDetailView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("job " + m.activeJob.ID))
	b.WriteString("\n\n")
	rows := [][]string{
		{"agent", dash(m.activeJob.Agent)},
		{"type", dash(m.activeJob.Type)},
		{"state", m.activeJob.State},
		{"updated", formatJobTime(m.activeJob.UpdatedAt)},
	}
	d := m.activeJobDetail
	if d.Repo != "" {
		repo := d.Repo
		if d.PullRequest > 0 {
			repo += "  #" + strconv.Itoa(d.PullRequest)
		}
		rows = append(rows, []string{"repo", repo})
	}
	b.WriteString(renderRows(rows))
	b.WriteByte('\n')

	// What was asked (newlines preserved, capped to a few lines).
	if d.Request != "" {
		b.WriteString(headerStyle.Render("request"))
		b.WriteByte('\n')
		b.WriteString(clampLines(d.Request, 8) + "\n\n")
	}
	// What came back (settled jobs).
	if d.ResultDecision != "" || d.ResultSummary != "" {
		b.WriteString(headerStyle.Render("result"))
		b.WriteByte('\n')
		if d.ResultDecision != "" {
			b.WriteString("decision  " + jobDecisionColor(d.ResultDecision) + "\n")
		}
		if d.ResultSummary != "" {
			b.WriteString(clampLines(d.ResultSummary, 8) + "\n")
		}
		b.WriteByte('\n')
	}

	// Delegation tree (coordinator jobs): one row per delegated child plus the
	// continuation job, if any.
	if len(d.Children) > 0 {
		b.WriteString(headerStyle.Render("delegations"))
		b.WriteByte('\n')
		rows := [][]string{{"DELEGATION", "AGENT", "ACTION", "DEPS", "STATE"}}
		const maxChildren = 20
		children := d.Children
		omitted := 0
		if len(children) > maxChildren {
			omitted = len(children) - maxChildren
			children = children[:maxChildren]
		}
		for _, c := range children {
			rows = append(rows, []string{
				dash(c.DelegationID),
				dash(c.Agent),
				truncate(c.Action, 30),
				depsLabel(c),
				jobStateColor(c.State),
			})
		}
		b.WriteString(renderRows(rows))
		if omitted > 0 {
			b.WriteString(mutedStyle.Render(strconv.Itoa(omitted)+" more delegations omitted") + "\n")
		}
		if d.ContinuationID != "" {
			b.WriteString("continuation  " + d.ContinuationID + "  " + jobStateColor(d.ContinuationState) + "\n")
		}
		b.WriteByte('\n')
	}

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

func (m Model) bugReportPreviewView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("bug report preview for job " + m.activeJob.ID))
	b.WriteString("\n\n")
	switch {
	case m.bugReportErr != "":
		b.WriteString(errorStyle.Render(m.bugReportErr))
		b.WriteByte('\n')
	case !m.bugReportLoaded:
		b.WriteString(mutedStyle.Render("building redacted preview…"))
		b.WriteByte('\n')
	default:
		if m.bugReport.Title != "" {
			b.WriteString(headerStyle.Render(m.bugReport.Title))
			b.WriteString("\n\n")
		}
		if len(m.bugReport.Labels) > 0 {
			b.WriteString("labels  " + strings.Join(m.bugReport.Labels, ", ") + "\n")
		}
		if m.bugReport.Fingerprint != "" {
			b.WriteString("fingerprint  " + m.bugReport.Fingerprint + "\n")
		}
		if m.bugReportURL != "" {
			label := "created: "
			if m.bugReportExisting {
				label = "existing: "
			}
			b.WriteString("\n" + greenStyle.Render(label+m.bugReportURL) + "\n")
		}
		if m.actionErr != "" {
			b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
		}
		if m.actionBusy {
			b.WriteString("\n" + mutedStyle.Render("creating issue…") + "\n")
		}
		if m.bugReport.Body != "" {
			b.WriteString("\n")
			b.WriteString(m.bugReport.Body)
			if !strings.HasSuffix(m.bugReport.Body, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}
