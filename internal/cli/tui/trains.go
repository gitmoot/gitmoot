package tui

import (
	"regexp"
	"strconv"
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

// trainUnderCursor returns the session under the Trains cursor, if the cursor is
// on a session row (not a collapsible header). The cursor indexes the visible
// rows; a leaf resolves through its itemIdx into orderedTrains().
func (m Model) trainUnderCursor() (TrainSession, bool) {
	if pages[m.selected].page != pageTrains {
		return TrainSession{}, false
	}
	idx := selectedItemIndex(m.trainVisibleRows(), m.trainCursor)
	ordered := m.orderedTrains()
	if idx < 0 || idx >= len(ordered) {
		return TrainSession{}, false
	}
	return ordered[idx], true
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

// Train status categories. A session lands in exactly one section; the ordering
// of the constants is also the top-to-bottom render order of the sections.
const (
	trainCatActive = iota
	trainCatBlocked
	trainCatDone
)

var trainSectionTitles = map[int]string{
	trainCatActive:  "Active",
	trainCatBlocked: "Blocked",
	trainCatDone:    "Done",
}

// trainStatusCategory buckets a phase into Active/Blocked/Done for the grouped
// list. Blocked is checked before the broad Active default so a stalled
// heartbeat or stale lock surfaces under Blocked rather than masquerading as
// live work. Done covers the terminal states.
func trainStatusCategory(phase string) int {
	p := skillopt.NormalizeTrainState(phase)
	switch p {
	case skillopt.TrainStateRunAbandoned,
		skillopt.TrainStateCandidatePromoted,
		skillopt.TrainStateCandidateRejected,
		skillopt.TrainStateOptimizerCompletedNoCandidate:
		return trainCatDone
	}
	switch p {
	case "blocked_stale_lock", "failed_unrecoverable", "blocked_config":
		return trainCatBlocked
	}
	if strings.HasSuffix(p, "_heartbeat_stale") {
		return trainCatBlocked
	}
	// Everything else — the ordered in-progress phases plus the live lock
	// phases (generating_options/optimizer_running/preflight_running/
	// recovery_available) — is Active.
	return trainCatActive
}

// Only an explicit "-vN" suffix marks a version lineage. A bare "-N" (e.g. a
// timestamp tail or an independently named run) is NOT treated as a version, so
// unrelated sessions like nightly-1/nightly-2 do not collapse together.
var trainLineageSuffix = regexp.MustCompile(`-v\d+$`)

// trainLineageBase strips an explicit version suffix ("-v3") from a session id,
// returning the shared base name and whether a suffix was present. Sessions in
// the same status section that share a base collapse under one lineage line.
func trainLineageBase(id string) (string, bool) {
	loc := trainLineageSuffix.FindStringIndex(id)
	if loc == nil {
		return id, false
	}
	return id[:loc[0]], true
}

// trainGroup is one unit of the Trains display: either a lone session
// (len(members)==1, base "") or a collapsed lineage (members share base).
type trainGroup struct {
	base    string
	members []TrainSession
}

// trainRepoBucket is one repository's lineage groups within a status section.
type trainRepoBucket struct {
	repo   string
	groups []trainGroup
}

// trainRepoLabel is a session's target repo, or "(no repo)" when it has none.
func trainRepoLabel(repo string) string {
	if strings.TrimSpace(repo) == "" {
		return "(no repo)"
	}
	return repo
}

// lineageGroups buckets sessions into display groups: a base shared by more than
// one session collapses into a single group whose members stay contiguous (in
// the given order, emitted at the first member's position); everything else is a
// lone group in place. This keeps every lineage child under its own parent even
// when members are not adjacent.
func lineageGroups(rows []TrainSession) []trainGroup {
	counts := map[string]int{}
	for _, t := range rows {
		if base, ok := trainLineageBase(t.ID); ok {
			counts[base]++
		}
	}
	emitted := map[string]bool{}
	var groups []trainGroup
	for _, t := range rows {
		base, ok := trainLineageBase(t.ID)
		if ok && counts[base] > 1 {
			if emitted[base] {
				continue
			}
			emitted[base] = true
			g := trainGroup{base: base}
			for _, u := range rows {
				if b, o := trainLineageBase(u.ID); o && b == base {
					g.members = append(g.members, u)
				}
			}
			groups = append(groups, g)
			continue
		}
		groups = append(groups, trainGroup{members: []TrainSession{t}})
	}
	return groups
}

// trainSectionRepos returns the display-ordered repo buckets for one status
// category: the category's sessions (snapshot order) bucketed by repo in
// first-appearance order, each with its lineage groups.
func (m Model) trainSectionRepos(cat int) []trainRepoBucket {
	var rows []TrainSession
	for _, t := range m.snap.Trains {
		if trainStatusCategory(t.Phase) == cat {
			rows = append(rows, t)
		}
	}
	order := []string{}
	byRepo := map[string][]TrainSession{}
	for _, t := range rows {
		label := trainRepoLabel(t.Repo)
		if _, ok := byRepo[label]; !ok {
			order = append(order, label)
		}
		byRepo[label] = append(byRepo[label], t)
	}
	out := make([]trainRepoBucket, 0, len(order))
	for _, label := range order {
		out = append(out, trainRepoBucket{repo: label, groups: lineageGroups(byRepo[label])})
	}
	return out
}

// bucketMemberCount is the total sessions across a repo bucket's lineage groups.
func bucketMemberCount(rb trainRepoBucket) int {
	n := 0
	for _, g := range rb.groups {
		n += len(g.members)
	}
	return n
}

// orderedTrains is the flat, display-ordered list of sessions — the exact
// top-to-bottom order rows render in (sections Active→Blocked→Done, then by repo,
// then lineages contiguous). The Trains cursor indexes into THIS slice, so ↑/↓
// steps through the visible list in order and selection matches the highlight.
func (m Model) orderedTrains() []TrainSession {
	var out []TrainSession
	for _, cat := range []int{trainCatActive, trainCatBlocked, trainCatDone} {
		for _, rb := range m.trainSectionRepos(cat) {
			for _, g := range rb.groups {
				out = append(out, g.members...)
			}
		}
	}
	return out
}

// trainRowContent is the per-session row text (id + phase, dimmed when the run
// is terminal) used as the leaf content in the collapsible Trains list.
func trainRowContent(t TrainSession) string {
	phase := t.Phase
	if deadTrainPhase(t.Phase) {
		phase = mutedStyle.Render(t.Phase)
	}
	return t.ID + "  " + phase
}

// trainListRows builds the Trains page's collapsible row tree:
// status section (Active/Blocked/Done) → repo → lineage → session. Leaf itemIdx
// indexes orderedTrains() (built in the same order), so the cursor resolves to
// the right session.
func (m Model) trainListRows() []listRow {
	var rows []listRow
	idx := 0
	for _, cat := range []int{trainCatActive, trainCatBlocked, trainCatDone} {
		repos := m.trainSectionRepos(cat)
		if len(repos) == 0 {
			continue
		}
		title := trainSectionTitles[cat]
		rows = append(rows, staticRow(0, title)) // status section: display-only
		for _, rb := range repos {
			repoKey := "trains:" + title + ":" + rb.repo
			rows = append(rows, headerRow(repoKey, 1, rb.repo+"  ×"+strconv.Itoa(bucketMemberCount(rb))))
			for _, g := range rb.groups {
				if len(g.members) > 1 {
					rows = append(rows, staticRow(2, g.base+"  ×"+strconv.Itoa(len(g.members)))) // lineage: display-only
					for _, t := range g.members {
						rows = append(rows, leafRow(3, trainRowContent(t), idx, repoKey))
						idx++
					}
					continue
				}
				rows = append(rows, leafRow(2, trainRowContent(g.members[0]), idx, repoKey))
				idx++
			}
		}
	}
	return rows
}

// trainVisibleRows is the Trains tree filtered by the collapse state — the rows
// rendered, and the index space m.trainCursor walks.
func (m Model) trainVisibleRows() []listRow {
	return visibleListRows(m.trainListRows(), m.collapsed)
}

func (m Model) trainsContent() string {
	if m.mode == modeTrainDetail {
		return m.trainDetail()
	}
	if len(m.snap.Trains) == 0 {
		return m.loadingOr("No train sessions.", !m.loadedAt.IsZero())
	}
	var b strings.Builder
	renderListRows(&b, m.trainVisibleRows(), m.trainCursor)
	b.WriteString("\n" + mutedStyle.Render("↑/↓ move · space fold repo · enter open · s stop · d delete") + "\n")
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
