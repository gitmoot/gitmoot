package tui

import (
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

// agentCustomPromptValue is the sentinel template choice that routes agent
// creation through the $EDITOR custom-prompt flow instead of an existing
// template. The leading sentinel form keeps it from colliding with a real id.
const agentCustomPromptValue = "__custom_prompt__"

// agentUnderCursor returns the agent under the Agents cursor, if any. The cursor
// indexes the display-ordered list (orderedAgents, grouped by template), so it
// resolves to the row the user sees highlighted even when templates interleave.
func (m Model) agentUnderCursor() (Agent, bool) {
	ordered := m.orderedAgents()
	if pages[m.selected].page != pageAgents || m.agentCursor < 0 || m.agentCursor >= len(ordered) {
		return Agent{}, false
	}
	return ordered[m.agentCursor], true
}

// orderedAgents is the flat, display-ordered list of visible agents — the exact
// top-to-bottom order rows render in (grouped by template, groups in
// first-appearance order). The Agents cursor indexes into THIS slice so ↑/↓ steps
// through the visible list in order.
func (m Model) orderedAgents() []Agent {
	var out []Agent
	for _, g := range groupAgentsByTemplate(m.visibleAgents()) {
		out = append(out, g.agents...)
	}
	return out
}

// isManagedTrainingAgent reports whether an agent name is internal skillopt
// training plumbing — the per-option target agents and the generator workers —
// that the user never acts on, so the Agents page hides them.
func isManagedTrainingAgent(name string) bool {
	name = strings.TrimSpace(name)
	return strings.HasPrefix(name, "skillopt-target-") || strings.HasPrefix(name, "skillopt-generator")
}

// visibleAgents are the user-facing agents (training plumbing filtered out). The
// Agents page renders, cursors, and acts on this list; the serialized snapshot
// keeps every agent, so --json/--plain are unaffected.
func (m Model) visibleAgents() []Agent {
	if m.showAllAgents {
		return m.snap.Agents
	}
	out := make([]Agent, 0, len(m.snap.Agents))
	for _, a := range m.snap.Agents {
		if !isManagedTrainingAgent(a.Name) {
			out = append(out, a)
		}
	}
	return out
}

// openAgentDetail enters the detail view for an agent and lazily loads its
// template's version history.
func (m *Model) openAgentDetail(agent Agent) tea.Cmd {
	m.activeAgent = agent
	m.agentVersions = nil
	m.agentVersionsLoaded = false
	m.agentVersionsErr = ""
	m.agentDetailCursor = 0
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeAgentDetail
	// Start at the top so a scroll position from the agents list does not carry over.
	m.viewport.GotoTop()
	if agent.TemplateID == "" {
		m.agentVersionsLoaded = true
		return nil
	}
	return agentVersionsCmd(m.deps, agent.TemplateID)
}

// openAgentDelete enters the delete confirmation for an agent.
func (m *Model) openAgentDelete(agent Agent) {
	m.activeAgent = agent
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeConfirmAgentDelete
}

// agentRuntimeOptions are the runtimes an agent can be switched between.
var agentRuntimeOptions = []string{"codex", "claude", "kimi"}

// openAgentRuntimePick enters the switch-runtime overlay, preselecting the
// agent's current runtime.
func (m *Model) openAgentRuntimePick(agent Agent) {
	m.activeAgent = agent
	m.actionErr = ""
	m.actionBusy = false
	m.runtimePickCursor = 0
	for i, rt := range agentRuntimeOptions {
		if rt == agent.Runtime {
			m.runtimePickCursor = i
			break
		}
	}
	m.mode = modeAgentRuntimePick
}

func agentSetRuntimeCmd(deps Deps, name, runtime string) tea.Cmd {
	return func() tea.Msg {
		if deps.SetAgentRuntime == nil {
			return agentActionMsg{verb: "runtime"}
		}
		return agentActionMsg{verb: "runtime", err: deps.SetAgentRuntime(name, runtime)}
	}
}

// agentRuntimePickView renders the switch-runtime choice overlay.
func (m Model) agentRuntimePickView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("switch runtime · " + m.activeAgent.Name))
	b.WriteString("\n\n")
	b.WriteString(mutedStyle.Render("Pick the runtime this agent runs on. The warm session resets; the next job starts fresh."))
	b.WriteString("\n\n")
	for i, rt := range agentRuntimeOptions {
		cursor, label := "  ", rt
		if i == m.runtimePickCursor {
			cursor, label = "▸ ", selectedRowStyle.Render(rt)
		}
		if rt == m.activeAgent.Runtime {
			label += "  " + mutedStyle.Render("(current)")
		}
		b.WriteString(cursor + label + "\n")
	}
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("switching…"))
	} else {
		b.WriteString(mutedStyle.Render("↑/↓ pick  enter apply  esc cancel"))
	}
	return b.String()
}

// openVersionView opens the preview pager for a template version, reusing the
// cache when it already holds this version's content (the train-run skill-pager
// pattern, keyed by version id).
func (m *Model) openVersionView(version TemplateVersion) tea.Cmd {
	m.activeAgentVersion = version
	m.mode = modeAgentVersionView
	if m.versionViewLoaded && m.versionViewErr == "" && m.versionViewID == version.ID {
		return nil
	}
	m.versionViewID = version.ID
	m.versionViewLoaded = false
	m.versionViewErr = ""
	return versionContentCmd(m.deps, version.ID)
}

func versionContentCmd(deps Deps, versionID string) tea.Cmd {
	return func() tea.Msg {
		if deps.TemplateVersionContent == nil {
			return versionContentMsg{versionID: versionID}
		}
		content, err := deps.TemplateVersionContent(versionID)
		return versionContentMsg{versionID: versionID, content: content, err: err}
	}
}

// agentVersionView renders the version content pager.
func (m Model) agentVersionView() string {
	var b strings.Builder
	v := m.activeAgentVersion
	b.WriteString(headerStyle.Render("version v" + strconv.Itoa(v.Number) + "  " + dash(m.activeAgent.TemplateID)))
	b.WriteString("\n\n")
	switch {
	case m.versionViewErr != "":
		b.WriteString(errorStyle.Render(m.versionViewErr) + "\n")
	case !m.versionViewLoaded:
		b.WriteString(mutedStyle.Render("loading…") + "\n")
	default:
		b.WriteString(m.versionView.View())
		b.WriteByte('\n')
	}
	b.WriteString("\n" + mutedStyle.Render("↑/↓ scroll  esc back"))
	return b.String()
}

// revertableVersions are the superseded versions an agent's template can be
// reverted to.
func (m Model) revertableVersions() []TemplateVersion {
	out := []TemplateVersion{}
	for _, v := range m.agentVersions {
		if v.State == "superseded" {
			out = append(out, v)
		}
	}
	return out
}

// updateAgentOverlay handles keys in the agent detail/revert/delete modes.
func (m Model) updateAgentOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeAgentDetail:
		switch msg.String() {
		case "esc", "q":
			m.mode = modeNormal
		case "up", "k":
			if m.agentDetailCursor > 0 {
				m.agentDetailCursor--
			}
		case "down", "j":
			if m.agentDetailCursor < m.agentDetailSelectableCount()-1 {
				m.agentDetailCursor++
			}
		case "enter":
			// Recent jobs come first in the cursor space, then template versions.
			// Enter on a job opens its detail (returning here on esc); enter on a
			// version opens its prompt in the pager.
			jobs := m.agentDetailJobs()
			if m.agentDetailCursor < len(jobs) {
				cmd := m.openJobDetail(jobs[m.agentDetailCursor])
				m.jobDetailReturn = modeAgentDetail
				m.viewport.SetContent(m.content())
				return m, cmd
			}
			vi := m.agentDetailCursor - len(jobs)
			if vi >= 0 && vi < len(m.agentVersions) && m.deps.TemplateVersionContent != nil {
				return m, m.openVersionView(m.agentVersions[vi])
			}
		case "v":
			if len(m.revertableVersions()) > 0 {
				m.versionCursor = 0
				m.mode = modeAgentRevertPick
			}
		case "D":
			m.openAgentDelete(m.activeAgent)
		default:
			// Forward unmapped keys (pgup/pgdn, space, u/d, wheel) to the viewport
			// so a tall agent detail (a full recent-jobs window plus many template
			// versions) can be scrolled past what ↑/↓ selection reaches.
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			m.viewport.SetContent(m.content())
			return m, cmd
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeAgentVersionView:
		switch msg.String() {
		case "esc", "q":
			m.mode = modeAgentDetail
			m.viewport.SetContent(m.content())
			return m, nil
		}
		var cmd tea.Cmd
		// Only steer the pager once its content is loaded — before that (or after
		// a failed load) versionView is the zero viewport and scroll keys are noise.
		if m.versionViewLoaded && m.versionViewErr == "" {
			m.versionView, cmd = m.versionView.Update(msg)
		}
		m.viewport.SetContent(m.content())
		return m, cmd
	case modeAgentRuntimePick:
		switch msg.String() {
		case "esc", "q":
			if m.actionBusy {
				return m, nil
			}
			m.mode = modeNormal
			m.actionErr = ""
		case "up", "k":
			if m.runtimePickCursor > 0 {
				m.runtimePickCursor--
			}
		case "down", "j":
			if m.runtimePickCursor < len(agentRuntimeOptions)-1 {
				m.runtimePickCursor++
			}
		case "enter":
			if m.actionBusy {
				return m, nil
			}
			runtime := agentRuntimeOptions[m.runtimePickCursor]
			if runtime == m.activeAgent.Runtime {
				// Nothing to change; just close.
				m.mode = modeNormal
				m.actionErr = ""
				break
			}
			m.actionBusy = true
			m.actionErr = ""
			m.viewport.SetContent(m.content())
			return m, agentSetRuntimeCmd(m.deps, m.activeAgent.Name, runtime)
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeAgentRevertPick:
		versions := m.revertableVersions()
		switch msg.String() {
		case "esc", "q":
			m.mode = modeAgentDetail
		case "up", "k":
			if m.versionCursor > 0 {
				m.versionCursor--
			}
		case "down", "j":
			if m.versionCursor < len(versions)-1 {
				m.versionCursor++
			}
		case "enter":
			if m.versionCursor < len(versions) {
				m.revertVersion = versions[m.versionCursor]
				m.actionErr = ""
				m.actionBusy = false
				m.mode = modeConfirmAgentRevert
			}
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeConfirmAgentRevert:
		switch msg.String() {
		case "y", "Y":
			if m.actionBusy {
				return m, nil
			}
			m.actionBusy = true
			m.actionErr = ""
			m.viewport.SetContent(m.content())
			return m, agentRevertCmd(m.deps, m.activeAgent.TemplateID, m.revertVersion.ID)
		default:
			if m.actionBusy {
				return m, nil
			}
			m.mode = modeAgentRevertPick
			m.actionErr = ""
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeConfirmAgentDelete:
		switch msg.String() {
		case "y", "Y":
			if m.actionBusy {
				return m, nil
			}
			m.actionBusy = true
			m.actionErr = ""
			m.viewport.SetContent(m.content())
			return m, agentDeleteCmd(m.deps, m.activeAgent.Name)
		default:
			if m.actionBusy {
				return m, nil
			}
			m.mode = modeNormal
			m.actionErr = ""
		}
		m.viewport.SetContent(m.content())
		return m, nil
	case modeConfirmAgentGroupDelete:
		switch msg.String() {
		case "y", "Y":
			if m.actionBusy {
				return m, nil
			}
			m.actionBusy = true
			m.actionErr = ""
			m.viewport.SetContent(m.content())
			return m, agentGroupDeleteCmd(m.deps, m.groupDeleteNames)
		default:
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

// openAgentGroupDelete opens the bulk-delete confirm for the template group of
// the agent under the cursor.
func (m *Model) openAgentGroupDelete() {
	a, ok := m.agentUnderCursor()
	if !ok {
		return
	}
	var names []string
	for _, g := range groupAgentsByTemplate(m.visibleAgents()) {
		if g.templateID == a.TemplateID {
			for _, ga := range g.agents {
				names = append(names, ga.Name)
			}
			break
		}
	}
	m.groupDeleteLabel = agentGroupLabel(a.TemplateID)
	m.groupDeleteNames = names
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeConfirmAgentGroupDelete
}

// agentsWithActiveJobs counts how many of the named agents currently have a
// queued/running job in the snapshot — a best-effort heads-up before a bulk
// delete (the store is authoritative and skips them).
func (m Model) agentsWithActiveJobs(names []string) int {
	active := map[string]bool{}
	for _, j := range m.snap.JobRows {
		if j.State == "queued" || j.State == "running" {
			active[j.Agent] = true
		}
	}
	n := 0
	for _, name := range names {
		if active[name] {
			n++
		}
	}
	return n
}

func (m Model) agentGroupDeleteConfirmView() string {
	var b strings.Builder
	n := len(m.groupDeleteNames)
	b.WriteString(headerStyle.Render("Delete " + strconv.Itoa(n) + " agents in group \"" + m.groupDeleteLabel + "\""))
	b.WriteString("\n\n")
	const showMax = 8
	for i, name := range m.groupDeleteNames {
		if i >= showMax {
			b.WriteString(mutedStyle.Render("… and "+strconv.Itoa(n-showMax)+" more") + "\n")
			break
		}
		b.WriteString("  " + name + "\n")
	}
	b.WriteByte('\n')
	if k := m.agentsWithActiveJobs(m.groupDeleteNames); k > 0 {
		b.WriteString(mutedStyle.Render(strconv.Itoa(k)+" currently have active jobs and will be skipped.") + "\n")
	}
	b.WriteString("Unregister the whole group? (y/n)\n")
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteByte('\n')
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("deleting…"))
	} else {
		b.WriteString(mutedStyle.Render("y delete all  n/esc cancel"))
	}
	return b.String()
}

func agentGroupDeleteCmd(deps Deps, names []string) tea.Cmd {
	return func() tea.Msg {
		if deps.DeleteAgents == nil {
			return agentGroupDeleteMsg{}
		}
		deleted, skipped, err := deps.DeleteAgents(names)
		return agentGroupDeleteMsg{deleted: deleted, skipped: skipped, err: err}
	}
}

func agentVersionsCmd(deps Deps, templateID string) tea.Cmd {
	return func() tea.Msg {
		if deps.TemplateVersions == nil {
			return agentVersionsMsg{templateID: templateID}
		}
		versions, err := deps.TemplateVersions(templateID)
		return agentVersionsMsg{templateID: templateID, versions: versions, err: err}
	}
}

func agentDeleteCmd(deps Deps, name string) tea.Cmd {
	return func() tea.Msg {
		if deps.DeleteAgent == nil {
			return agentActionMsg{verb: "delete"}
		}
		return agentActionMsg{verb: "delete", err: deps.DeleteAgent(name)}
	}
}

func agentRevertCmd(deps Deps, templateID, versionID string) tea.Cmd {
	return func() tea.Msg {
		if deps.RevertTemplate == nil {
			return agentActionMsg{verb: "revert"}
		}
		return agentActionMsg{verb: "revert", err: deps.RevertTemplate(templateID, versionID)}
	}
}

// openAgentFormCmd builds the create form off the UI thread and pushes it; a
// construction error renders inline on the Agents page instead.
func openAgentFormCmd(deps Deps) tea.Cmd {
	return func() tea.Msg {
		form, err := deps.OpenAgentCreate()
		if err != nil {
			return agentActionMsg{verb: "form", err: err}
		}
		return PushModelMsg{Model: form}
	}
}

// openAgentOptimizeCmd builds the pre-filled optimize form off the UI thread
// and pushes it.
func openAgentOptimizeCmd(deps Deps, agent Agent) tea.Cmd {
	return func() tea.Msg {
		form, err := deps.OpenAgentOptimize(agent)
		if err != nil {
			return agentActionMsg{verb: "form", err: err}
		}
		return PushModelMsg{Model: form}
	}
}

// startOptimizeCmd scaffolds and starts the train session from the form's
// answers.
func startOptimizeCmd(deps Deps, templateID string, values map[string]string) tea.Cmd {
	return func() tea.Msg {
		if deps.StartOptimize == nil {
			return optimizeStartedMsg{}
		}
		sessionID, err := deps.StartOptimize(templateID, values)
		return optimizeStartedMsg{sessionID: sessionID, err: err}
	}
}

// NewAgentOptimizeForm wraps the train-init form for the optimize flow: on
// completion it pops itself and delivers the answers (bound to the template it
// was opened for) to the dashboard, which starts the session and opens its
// phase view.
func NewAgentOptimizeForm(store PromptStore, templateID string, fields []Field, summary func(map[string]string) [][]string, interpret Interpret) TrainInitModel {
	form := NewTrainInit(store, fields, summary, interpret, 0)
	form.Done = func(res Result) tea.Cmd {
		return PopWith(agentOptimizeFormResultMsg{templateID: templateID, result: res})
	}
	return form
}

func agentCreateCmd(deps Deps, values map[string]string) tea.Cmd {
	return func() tea.Msg {
		if deps.CreateAgent == nil {
			return agentActionMsg{verb: "create"}
		}
		return agentActionMsg{verb: "create", err: deps.CreateAgent(values["name"], values["runtime"], values["template"])}
	}
}

func agentCreateWithPromptCmd(deps Deps, name, runtime, content string) tea.Cmd {
	return func() tea.Msg {
		if deps.CreateAgentWithPrompt == nil {
			return agentActionMsg{verb: "create"}
		}
		return agentActionMsg{verb: "create", err: deps.CreateAgentWithPrompt(name, runtime, content)}
	}
}

// NewAgentCreateForm builds the create-agent form on the train-init field
// widgets. On completion it pops itself off the Root stack and delivers the
// answers to the dashboard, which runs Deps.CreateAgent.
func NewAgentCreateForm(store PromptStore, templates []Choice) TrainInitModel {
	fields := []Field{
		{
			Name:  "name",
			Label: "Agent name",
			Kind:  FieldText,
			Prompt: db.InteractivePrompt{
				ID:            "agent-create-name",
				Question:      "Name for the new agent?",
				Required:      true,
				AnswerFormat:  "text",
				SourceCommand: "dashboard agent create",
				State:         db.InteractivePromptStatePending,
			},
		},
		{
			Name:  "runtime",
			Label: "Runtime",
			Kind:  FieldChoice,
			Choices: []Choice{
				{Value: "codex", Label: "codex"},
				{Value: "claude", Label: "claude"},
				{Value: "kimi", Label: "kimi"},
			},
			Default: "codex",
			Prompt: db.InteractivePrompt{
				ID:            "agent-create-runtime",
				Question:      "Runtime for the new agent?",
				Choices:       []string{"codex", "claude", "kimi"},
				Default:       "codex",
				Required:      true,
				AnswerFormat:  "choice",
				SourceCommand: "dashboard agent create",
				State:         db.InteractivePromptStatePending,
			},
		},
		{
			Name:    "template",
			Label:   "Template",
			Kind:    FieldChoice,
			Choices: append(append([]Choice{}, templates...), Choice{Value: agentCustomPromptValue, Label: "✎ write a custom prompt…"}),
			Prompt: db.InteractivePrompt{
				ID:            "agent-create-template",
				Question:      "Template for the new agent?",
				Choices:       append(choiceValues(templates), agentCustomPromptValue),
				Required:      true,
				AnswerFormat:  "choice",
				SourceCommand: "dashboard agent create",
				State:         db.InteractivePromptStatePending,
			},
		},
		{
			Name:  "seed",
			Label: "Seed the prompt from",
			Kind:  FieldChoice,
			// Blank first (default) → minimal scaffold; then each base template.
			Choices: append([]Choice{{Value: "", Label: "blank (minimal scaffold)"}}, templates...),
			// Only asked when the custom-prompt sentinel was chosen.
			Skip: func(answers map[string]string) bool { return answers["template"] != agentCustomPromptValue },
			Prompt: db.InteractivePrompt{
				ID:            "agent-create-seed",
				Question:      "Seed the custom prompt from which template? (blank = minimal scaffold)",
				Choices:       append([]string{""}, choiceValues(templates)...),
				AnswerFormat:  "choice",
				SourceCommand: "dashboard agent create",
				State:         db.InteractivePromptStatePending,
			},
		},
	}
	summary := func(answers map[string]string) [][]string {
		rows := [][]string{
			{"name", answers["name"]},
			{"runtime", answers["runtime"]},
		}
		if answers["template"] == agentCustomPromptValue {
			seed := answers["seed"]
			if seed == "" {
				seed = "blank scaffold"
			}
			rows = append(rows, []string{"prompt", "custom (seed: " + seed + ")"})
		} else {
			rows = append(rows, []string{"template", answers["template"]})
		}
		return rows
	}
	form := NewTrainInit(store, fields, summary, nil, 0)
	form.Done = func(res Result) tea.Cmd {
		return PopWith(agentFormResultMsg{result: res})
	}
	return form
}

func choiceValues(choices []Choice) []string {
	values := make([]string, 0, len(choices))
	for _, c := range choices {
		values = append(values, c.Value)
	}
	return values
}

// agentsContentInteractive renders the Agents page as a selectable list. Agent
// overlays are dispatched once in content(), not here.
func (m Model) agentsContentInteractive() string {
	visible := m.visibleAgents()
	hidden := len(m.snap.Agents) - len(visible)
	// LIVE = warm runtime sessions per agent. Count once over all sessions (keyed
	// by owning agent) so the render stays O(agents + sessions); also tally the
	// sessions owned by hidden training agents to annotate the hidden line.
	liveByAgent := map[string]int{}
	hiddenLive, hiddenRunning := 0, 0
	for _, s := range m.snap.Sessions {
		name := sessionAgentName(s)
		if strings.TrimSpace(name) == "" {
			continue
		}
		liveByAgent[name]++
		if isManagedTrainingAgent(name) {
			hiddenLive++
			if s.State == "running" {
				hiddenRunning++
			}
		}
	}
	hiddenLine := func(b *strings.Builder) {
		if m.showAllAgents {
			b.WriteString("\n" + mutedStyle.Render("showing all agents · a hide training") + "\n")
			return
		}
		if hidden > 0 {
			line := strconv.Itoa(hidden) + " training agents hidden (skillopt-*)"
			if hiddenLive > 0 {
				line += " · " + strconv.Itoa(hiddenLive) + " live"
				if hiddenRunning > 0 {
					line += " (" + strconv.Itoa(hiddenRunning) + " running)"
				}
			}
			line += " · a show all"
			b.WriteString("\n" + mutedStyle.Render(line) + "\n")
		}
	}
	if len(visible) == 0 {
		var b strings.Builder
		b.WriteString(m.loadingOr("No registered agents.", !m.loadedAt.IsZero()))
		b.WriteByte('\n')
		if m.agentErr != "" {
			b.WriteString("\n" + errorStyle.Render(m.agentErr) + "\n")
		}
		if m.agentNotice != "" {
			b.WriteString("\n" + mutedStyle.Render(m.agentNotice) + "\n")
		}
		hiddenLine(&b)
		b.WriteString("\n" + mutedStyle.Render("n new agent"))
		return b.String()
	}
	var b strings.Builder
	// Render the visible agents grouped by template. The cursor indexes the
	// display order (orderedAgents), so pos advances per agent row in lockstep
	// and the highlight matches the visible position. The column header prints
	// once at the top; template labels are display-only and consume no position.
	b.WriteString(renderRows([][]string{{"", "NAME", "RUNTIME", "ROLE", "HEALTH", "LIVE"}}))

	// Build the group + agent lines once, then window them around the cursor so a
	// long list (e.g. 'a' showing the training agents) stays scrollable with the
	// selection on-screen. The column header above is always shown; agentCursor
	// indexes the flat agent order.
	var lines []string
	cursorLine := 0
	pos := 0
	for _, g := range groupAgentsByTemplate(visible) {
		lines = append(lines, "", headerStyle.Render(agentGroupLabel(g.templateID)))
		rows := [][]string{}
		for _, a := range g.agents {
			cursor, name := "  ", a.Name
			if pos == m.agentCursor {
				cursor, name = "▸ ", selectedRowStyle.Render(a.Name)
				cursorLine = len(lines) + len(rows)
			}
			live := "-"
			if n := liveByAgent[a.Name]; n > 0 {
				live = strconv.Itoa(n)
			}
			rows = append(rows, []string{cursor, name, a.Runtime, dash(a.Role), dash(a.Health), live})
			pos++
		}
		lines = append(lines, strings.Split(strings.TrimRight(renderRows(rows), "\n"), "\n")...)
	}

	capacity := agentsWindowCap(m.height)
	start, end := 0, len(lines)
	if len(lines) > capacity {
		start = cursorLine - capacity/2
		if start < 0 {
			start = 0
		}
		if start > len(lines)-capacity {
			start = len(lines) - capacity
		}
		end = start + capacity
	}
	if start > 0 {
		b.WriteString(mutedStyle.Render("  ↑ "+strconv.Itoa(start)+" more above") + "\n")
	}
	for i := start; i < end; i++ {
		b.WriteString(lines[i] + "\n")
	}
	if end < len(lines) {
		b.WriteString(mutedStyle.Render("  ↓ "+strconv.Itoa(len(lines)-end)+" more below") + "\n")
	}
	if m.agentErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.agentErr) + "\n")
	}
	if m.agentNotice != "" {
		b.WriteString("\n" + mutedStyle.Render(m.agentNotice) + "\n")
	}
	hiddenLine(&b)
	if m.optimizeBusy {
		b.WriteString("\n" + mutedStyle.Render("starting optimization…") + "\n")
	}
	b.WriteString(mutedStyle.Render("enter detail  n new  o optimize  D delete  X delete group  a show all/hide"))
	b.WriteByte('\n')
	return b.String()
}

// agentsWindowCap is how many group/agent lines fit the Agents page, leaving
// room for the viewport chrome (title, column header, scroll markers, the
// hidden/all line, and the footer) so the windowed body stays on-screen.
func agentsWindowCap(height int) int {
	if height-12 < 3 {
		return 3
	}
	return height - 12
}

// agentGroupLabel is the section header for a template group; agents without a
// template — they run a custom prompt rather than a reusable template — share
// the "Standalone agents" group.
func agentGroupLabel(templateID string) string {
	if strings.TrimSpace(templateID) == "" {
		return "Standalone agents (custom prompt)"
	}
	return templateID
}

// agentGroup is a template's agents in their original visible order.
type agentGroup struct {
	templateID string
	agents     []Agent
}

// tempWorkerTypePrefix is the prefix the daemon stamps on a temp worker's agent
// type ("temp:<agent>", see tempWorkerAgentType). Stripping it rolls the temp
// worker's session up under the agent it serves.
const tempWorkerTypePrefix = "temp:"

// sessionAgentName is the registered-agent name a runtime session belongs to: its
// Type is the agent-type name (== the agent's Name), with the temp-worker
// "temp:" prefix stripped so a parallel temp worker counts toward the agent it
// serves rather than disappearing from the Agents view.
func sessionAgentName(s Session) string {
	if rest, ok := strings.CutPrefix(s.Type, tempWorkerTypePrefix); ok {
		return rest
	}
	return s.Type
}

// agentSessions returns the live runtime sessions belonging to an agent (the same
// warm process can serve any agent of that type, including its temp workers).
// Order is preserved from the snapshot.
func agentSessions(sessions []Session, agentName string) []Session {
	if strings.TrimSpace(agentName) == "" {
		return nil
	}
	var out []Session
	for _, s := range sessions {
		if sessionAgentName(s) == agentName {
			out = append(out, s)
		}
	}
	return out
}

// groupAgentsByTemplate buckets visible agents by TemplateID for display. Groups
// appear in first-appearance order and agents keep their visible order within
// each group. orderedAgents flattens these groups to define the cursor's index
// space, so the display order and the selectable order are one and the same.
func groupAgentsByTemplate(visible []Agent) []agentGroup {
	groups := []agentGroup{}
	at := map[string]int{} // templateID → index into groups
	for _, a := range visible {
		pos, ok := at[a.TemplateID]
		if !ok {
			pos = len(groups)
			at[a.TemplateID] = pos
			groups = append(groups, agentGroup{templateID: a.TemplateID})
		}
		groups[pos].agents = append(groups[pos].agents, a)
	}
	return groups
}

func (m Model) agentDetailView() string {
	a := m.activeAgent
	var b strings.Builder
	b.WriteString(headerStyle.Render("agent " + a.Name))
	b.WriteString("\n\n")
	rows := [][]string{
		{"runtime", dash(a.Runtime)},
		{"role", dash(a.Role)},
		{"health", dash(a.Health)},
		{"template", dash(a.TemplateID)},
	}
	b.WriteString(renderRows(rows))
	b.WriteByte('\n')

	// Recent jobs: selectable (enter opens the job's detail) and windowed so a
	// busy agent's history stays scrollable. The agent-detail cursor walks these
	// jobs first, then the template versions below.
	jobs := m.agentDetailJobs()
	b.WriteString(headerStyle.Render("recent jobs"))
	b.WriteByte('\n')
	if len(jobs) == 0 {
		b.WriteString(mutedStyle.Render("none") + "\n")
	} else {
		capacity := agentDetailJobsVisible(m.height)
		anchor := m.agentDetailCursor
		if anchor > len(jobs)-1 {
			anchor = len(jobs) - 1
		}
		start := 0
		if len(jobs) > capacity {
			start = anchor - capacity/2
			if start < 0 {
				start = 0
			}
			if start > len(jobs)-capacity {
				start = len(jobs) - capacity
			}
		}
		end := start + capacity
		if end > len(jobs) {
			end = len(jobs)
		}
		if start > 0 {
			b.WriteString(mutedStyle.Render("  ↑ "+strconv.Itoa(start)+" earlier") + "\n")
		}
		for i := start; i < end; i++ {
			job := jobs[i]
			cursor, id := "  ", job.ID
			if i == m.agentDetailCursor {
				cursor, id = "▸ ", selectedRowStyle.Render(job.ID)
			}
			b.WriteString(cursor + id + "  " + job.Type + "  " + jobStateColor(job.State) + "\n")
		}
		if end < len(jobs) {
			b.WriteString(mutedStyle.Render("  ↓ "+strconv.Itoa(len(jobs)-end)+" more") + "\n")
		}
	}
	b.WriteByte('\n')

	// Live runtime sessions (warm processes) serving this agent. Ephemeral — they
	// expire on idle — so they live here as a drill-in snapshot, not in the list.
	b.WriteString(headerStyle.Render("live sessions"))
	b.WriteByte('\n')
	sessions := agentSessions(m.snap.Sessions, a.Name)
	if len(sessions) == 0 {
		b.WriteString(mutedStyle.Render("none") + "\n")
	} else {
		for _, s := range sessions {
			line := s.Name + "  " + s.State
			if s.Repo != "" {
				line += "  " + s.Repo
			}
			if s.Expires != "" {
				line += "  " + mutedStyle.Render("expires "+formatJobTime(s.Expires))
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteByte('\n')

	b.WriteString(headerStyle.Render("template versions"))
	b.WriteByte('\n')
	switch {
	case a.TemplateID == "":
		b.WriteString(mutedStyle.Render("none — standalone agent, runs a custom prompt") + "\n")
	case m.agentVersionsErr != "":
		b.WriteString(errorStyle.Render(m.agentVersionsErr) + "\n")
	case !m.agentVersionsLoaded:
		b.WriteString(mutedStyle.Render("loading…") + "\n")
	case len(m.agentVersions) == 0:
		b.WriteString(mutedStyle.Render("no versions") + "\n")
	default:
		for i, v := range m.agentVersions {
			cursor := "  "
			label := "v" + strconv.Itoa(v.Number)
			if len(jobs)+i == m.agentDetailCursor {
				cursor, label = "▸ ", selectedRowStyle.Render(label)
			}
			line := cursor + label + "  " + versionStateColor(v.State)
			if v.Name != "" {
				line += "  " + truncate(v.Name, 48)
			}
			if v.Created != "" {
				line += "  " + mutedStyle.Render(v.Created)
			}
			b.WriteString(line + "\n")
		}
	}
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteString("\n")
	hint := "D delete  esc back"
	if len(m.revertableVersions()) > 0 {
		hint = "v revert  " + hint
	}
	if m.agentDetailSelectableCount() > 0 {
		hint = "↑/↓ select  enter open  " + hint
	}
	b.WriteString(mutedStyle.Render(hint))
	return b.String()
}

func (m Model) agentRevertPickView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("revert " + m.activeAgent.TemplateID))
	b.WriteString("\n\n")
	b.WriteString("Pick the version to make current again:\n\n")
	for i, v := range m.revertableVersions() {
		cursor, label := "  ", "v"+strconv.Itoa(v.Number)
		if v.Name != "" {
			label += "  " + truncate(v.Name, 48)
		}
		if i == m.versionCursor {
			cursor, label = "▸ ", selectedRowStyle.Render(label)
		}
		b.WriteString(cursor + label + "\n")
	}
	b.WriteString("\n" + mutedStyle.Render("enter pick  esc back"))
	return b.String()
}

func (m Model) agentRevertConfirmView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Revert " + m.activeAgent.TemplateID))
	b.WriteString("\n\n")
	b.WriteString("Make v" + strconv.Itoa(m.revertVersion.Number) + " the current version again? (y/n)\n")
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	b.WriteString("\n")
	if m.actionBusy {
		b.WriteString(mutedStyle.Render("reverting…"))
	} else {
		b.WriteString(mutedStyle.Render("y revert  n/esc cancel"))
	}
	return b.String()
}

func (m Model) agentDeleteConfirmView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Delete agent " + m.activeAgent.Name))
	b.WriteString("\n\n")
	b.WriteString(dash(m.activeAgent.Runtime) + " · template " + dash(m.activeAgent.TemplateID) + "\n\n")
	b.WriteString("Unregister this agent? (y/n)\n")
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

// agentJobs returns up to limit of the agent's most recent job rows, newest
// first by UpdatedAt (job ids are semantic strings, so id order is not
// recency).
func agentJobs(rows []JobRow, agent string, limit int) []JobRow {
	matched := []JobRow{}
	for _, row := range rows {
		if row.Agent == agent {
			matched = append(matched, row)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].UpdatedAt > matched[j].UpdatedAt })
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched
}

// agentDetailJobLimit caps how many of an agent's jobs the detail view keeps
// (and the cursor can reach); the visible slice windows within this.
const agentDetailJobLimit = 100

// agentDetailJobs is the agent's recent jobs, selectable in the detail view.
func (m Model) agentDetailJobs() []JobRow {
	return agentJobs(m.snap.JobRows, m.activeAgent.Name, agentDetailJobLimit)
}

// agentDetailSelectableCount is how many rows the agent-detail cursor walks: the
// recent jobs first, then the template versions.
func (m Model) agentDetailSelectableCount() int {
	return len(m.agentDetailJobs()) + len(m.agentVersions)
}

// agentDetailJobsVisible is how many recent-job rows render before the list
// windows around the cursor (keeps the detail on-screen for a busy agent).
func agentDetailJobsVisible(height int) int {
	n := height - 14
	if n < 4 {
		return 4
	}
	if n > 12 {
		return 12
	}
	return n
}

// AgentCreatePromptIDs are the interactive-prompt records the create form
// publishes; the dashboard deletes them on exit so a hard quit mid-form does
// not leave phantom prompts behind.
func AgentCreatePromptIDs() []string {
	return []string{"agent-create-name", "agent-create-runtime", "agent-create-template"}
}

func versionStateColor(state string) string {
	switch state {
	case "current":
		return greenStyle.Render(state)
	case "rejected":
		return redStyle.Render(state)
	default:
		return state
	}
}
