package tui

import (
	"errors"
	"sort"
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
	// Default: close back to the page. A caller (e.g. the agent detail) may set
	// jobDetailReturn afterward to return to its own overlay instead.
	m.jobDetailReturn = modeNormal
	m.mode = modeJobDetail
	// Start at the top so a scroll position from the page/agent detail it was
	// opened from does not carry over.
	m.viewport.GotoTop()
	return tea.Batch(jobEventsCmd(m.deps, job.ID), jobDetailCmd(m.deps, job.ID))
}

// openBugReportPreview enters the redacted report preview for a reportable job.
func (m *Model) openBugReportPreview(job JobRow) tea.Cmd {
	// Remember where we came from (the page, or a job detail) so esc returns there.
	m.jobActionReturn = m.mode
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
	// Remember where we came from (the page, or a job detail) so the confirm
	// returns there whether it is accepted or dismissed.
	m.jobActionReturn = m.mode
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
		idx := selectedItemIndex(m.jobsVisibleRows(), m.jobCursor)
		if idx < 0 {
			return JobRow{}, false
		}
		ordered := m.jobsOrdered()
		if idx < len(ordered) {
			return ordered[idx], true
		}
		return JobRow{}, false
	case pageAttention:
		idx := selectedItemIndex(m.attentionVisibleRows(), m.promptCursor)
		if idx < 0 {
			return JobRow{}, false
		}
		items := m.attentionItems()
		if idx < len(items) && items[idx].job != nil {
			return *items[idx].job, true
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
	return state == "queued" || state == "running" || state == "blocked"
}

// updateJobOverlay handles keys in the job detail and confirm modes.
func (m Model) updateJobOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeJobDetail:
		switch msg.String() {
		case "esc", "enter", "q":
			// Return to wherever the detail was opened from (the page, or the
			// agent detail when drilled in from there).
			m.mode = m.jobDetailReturn
			m.jobDetailReturn = modeNormal
		case "R":
			if jobRetryable(m.activeJob.State) {
				m.openJobConfirm(m.activeJob, false)
			}
		case "B":
			if jobReportable(m.activeJob.State) {
				cmd := m.openBugReportPreview(m.activeJob)
				m.viewport.SetContent(m.content())
				return m, cmd
			}
		case "c":
			if jobCancelable(m.activeJob.State) {
				m.openJobConfirm(m.activeJob, true)
			}
		default:
			// Forward unmapped keys (↑/↓, pgup/pgdn, space, u/d) to the viewport
			// so a long request/result/delegation list can be scrolled.
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			m.viewport.SetContent(m.content())
			return m, cmd
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeBugReportPreview:
		switch msg.String() {
		case "esc", "q":
			if m.actionBusy {
				return m, nil
			}
			// Return to wherever the preview was opened from (the page, or the
			// job detail when drilled in from there).
			m.mode = m.jobActionReturn
			m.jobActionReturn = modeNormal
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
			// Dismissed: return to wherever the confirm was opened from (the
			// page, or the job detail when drilled in from there).
			m.mode = m.jobActionReturn
			m.jobActionReturn = modeNormal
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

// jobStatusGroupOrder lists job states in the order the Jobs page groups them —
// active work first, the settled pile last. States outside this list (defensive)
// are appended in sorted order.
var jobStatusGroupOrder = []string{"running", "queued", "blocked", "failed", "succeeded", "cancelled"}

// jobStatusGroup is one status section of the Jobs page: a state and its jobs in
// snapshot order.
type jobStatusGroup struct {
	state string
	jobs  []JobRow
}

// computeJobsByStatusGroup buckets job rows by state into display-ordered groups
// (active first, then any unknown states sorted). Pure over its input so the
// result can be memoized per snapshot.
func computeJobsByStatusGroup(jobRows []JobRow) []jobStatusGroup {
	byState := map[string][]JobRow{}
	for _, j := range jobRows {
		byState[j.State] = append(byState[j.State], j)
	}
	var groups []jobStatusGroup
	used := map[string]bool{}
	for _, st := range jobStatusGroupOrder {
		if jobs := byState[st]; len(jobs) > 0 {
			groups = append(groups, jobStatusGroup{st, jobs})
			used[st] = true
		}
	}
	var rest []string
	for st := range byState {
		if !used[st] {
			rest = append(rest, st)
		}
	}
	sort.Strings(rest)
	for _, st := range rest {
		groups = append(groups, jobStatusGroup{st, byState[st]})
	}
	return groups
}

// jobsByStatusGroup returns the status groups, reusing the per-snapshot cache
// (m.jobGroups, set when the snapshot is applied) so jobsVisibleRows/jobsOrdered/
// jobUnderCursor don't each re-bucket every job on every keystroke. Falls back to
// computing on the fly when the cache is unset, so correctness never depends on
// it. Both jobsOrdered and jobsListRows derive from this, so the cursor's item
// index and the rendered rows can never drift apart.
func (m Model) jobsByStatusGroup() []jobStatusGroup {
	if m.jobGroups != nil {
		return m.jobGroups
	}
	return computeJobsByStatusGroup(m.snap.JobRows)
}

// jobsSummaryOrder lists the states present in byState in the same active-first
// order as the status groups, so the top summary line mirrors the sections.
func jobsSummaryOrder(byState map[string]int) []string {
	var out []string
	used := map[string]bool{}
	for _, st := range jobStatusGroupOrder {
		if byState[st] > 0 {
			out = append(out, st)
			used[st] = true
		}
	}
	var rest []string
	for st, n := range byState {
		if n > 0 && !used[st] {
			rest = append(rest, st)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// jobsOrdered is the jobs flattened in status-group order — the index space the
// Jobs cursor resolves through (leaf itemIdx in jobsListRows points here).
func (m Model) jobsOrdered() []JobRow {
	var out []JobRow
	for _, g := range m.jobsByStatusGroup() {
		out = append(out, g.jobs...)
	}
	return out
}

// jobsListRows builds the collapsible Jobs tree: one header per status group
// (×count) with its jobs as leaves. The state lives in the header, so leaves omit
// it — except a job being cancelled, which shows a transient inline label.
func (m Model) jobsListRows() []listRow {
	var rows []listRow
	idx := 0
	for _, g := range m.jobsByStatusGroup() {
		key := "jobs:status:" + g.state
		rows = append(rows, headerRow(key, 0, g.state+"  ×"+strconv.Itoa(len(g.jobs))))
		for _, j := range g.jobs {
			line := j.ID + "  " + j.Agent + "  " + j.Type + "  " + mutedStyle.Render(formatJobTime(j.UpdatedAt))
			if _, pending := m.cancelling[j.ID]; pending && jobCancelable(j.State) {
				line += "  " + "cancelling…"
			}
			rows = append(rows, leafRow(1, line, idx, key))
			idx++
		}
	}
	return rows
}

// jobsVisibleRows is the Jobs tree filtered by collapse state — the rows rendered
// and the index space m.jobCursor walks.
func (m Model) jobsVisibleRows() []listRow {
	return visibleListRows(m.jobsListRows(), m.groupCollapsed)
}

// jobsContentInteractive renders the Jobs page as a collapsible list grouped by
// status, windowed so the selection stays visible even when a status group has
// hundreds of jobs. Job overlays (detail/confirm) are dispatched in content().
func (m Model) jobsContentInteractive() string {
	if len(m.snap.JobRows) == 0 {
		return m.loadingOr("No jobs.", !m.loadedAt.IsZero())
	}
	var b strings.Builder
	summary := []string{}
	for _, state := range jobsSummaryOrder(m.snap.Jobs.ByState) {
		summary = append(summary, jobStateColor(state)+" "+strconv.Itoa(m.snap.Jobs.ByState[state]))
	}
	b.WriteString(mutedStyle.Render(strconv.Itoa(m.snap.Jobs.Total)+" total") + "  " + strings.Join(summary, "  "))
	b.WriteString("\n\n")

	rows := m.jobsVisibleRows()
	windowed, rel, above, below := windowListRows(rows, m.jobCursor, jobsWindowCap(m.height))
	if above > 0 {
		// above == the window start index; keep the status-group context when the
		// window opens mid-group (the group's header scrolled off).
		marker := "↑ " + strconv.Itoa(above) + " more rows above"
		if above < len(rows) && !rows[above].header {
			for i := above - 1; i >= 0; i-- {
				if rows[i].header {
					marker += " · " + rows[i].text + " (continued)"
					break
				}
			}
		}
		b.WriteString(mutedStyle.Render(marker) + "\n")
	}
	renderListRows(&b, windowed, rel)
	if below > 0 {
		b.WriteString(mutedStyle.Render("↓ "+strconv.Itoa(below)+" more rows below") + "\n")
	}

	// enter toggles a collapsed group header; it only opens a detail on a job leaf.
	help := "↑/↓ select · space open/close"
	if m.cursorOnHeader() {
		help += " · enter open/close"
	} else {
		help += " · enter detail · R retry · c cancel"
		if job, ok := m.jobUnderCursor(); ok && jobReportable(job.State) {
			help += " · B report bug"
		}
	}
	b.WriteString("\n" + mutedStyle.Render(help))
	b.WriteByte('\n')
	return b.String()
}

// jobsWindowCap is how many list rows fit the Jobs page. The viewport is
// height-4; the page also renders the title block (2), the summary (2), both
// scroll markers (2), and the blank-line+help footer (2) = 8 lines of chrome, so
// the window keeps to height-12 to stay inside the viewport even when both
// markers show.
func jobsWindowCap(height int) int {
	if height-12 < 3 {
		return 3
	}
	return height - 12
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

	// What was asked. Shown in full, soft-wrapped to the viewport width so long
	// lines aren't clipped at the right edge; the viewport scrolls vertically.
	width := m.viewport.Width
	if d.Request != "" {
		b.WriteString(headerStyle.Render("request"))
		b.WriteByte('\n')
		b.WriteString(wrapText(strings.TrimRight(d.Request, "\n"), width) + "\n\n")
	}
	// What came back (settled jobs). Shown in full, wrapped the same way.
	if d.ResultDecision != "" || d.ResultSummary != "" {
		b.WriteString(headerStyle.Render("result"))
		b.WriteByte('\n')
		if d.ResultDecision != "" {
			b.WriteString("decision  " + jobDecisionColor(d.ResultDecision) + "\n")
		}
		if d.ResultSummary != "" {
			b.WriteString(wrapText(strings.TrimRight(d.ResultSummary, "\n"), width) + "\n")
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
