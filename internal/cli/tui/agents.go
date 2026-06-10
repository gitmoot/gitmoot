package tui

import (
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

// agentUnderCursor returns the agent under the Agents cursor, if any.
func (m Model) agentUnderCursor() (Agent, bool) {
	if pages[m.selected].page != pageAgents || len(m.snap.Agents) == 0 {
		return Agent{}, false
	}
	return m.snap.Agents[m.agentCursor], true
}

// openAgentDetail enters the detail view for an agent and lazily loads its
// template's version history.
func (m *Model) openAgentDetail(agent Agent) tea.Cmd {
	m.activeAgent = agent
	m.agentVersions = nil
	m.agentVersionsLoaded = false
	m.agentVersionsErr = ""
	m.actionErr = ""
	m.actionBusy = false
	m.mode = modeAgentDetail
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
		case "esc", "enter", "q":
			m.mode = modeNormal
		case "v":
			if len(m.revertableVersions()) > 0 {
				m.versionCursor = 0
				m.mode = modeAgentRevertPick
			}
		case "D":
			m.openAgentDelete(m.activeAgent)
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
	}
	return m, nil
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
			Name:    "runtime",
			Label:   "Runtime",
			Kind:    FieldChoice,
			Choices: []Choice{{Value: "codex", Label: "codex"}, {Value: "claude", Label: "claude"}},
			Default: "codex",
			Prompt: db.InteractivePrompt{
				ID:            "agent-create-runtime",
				Question:      "Runtime for the new agent?",
				Choices:       []string{"codex", "claude"},
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
			Choices: templates,
			Prompt: db.InteractivePrompt{
				ID:            "agent-create-template",
				Question:      "Template for the new agent?",
				Choices:       choiceValues(templates),
				Required:      true,
				AnswerFormat:  "choice",
				SourceCommand: "dashboard agent create",
				State:         db.InteractivePromptStatePending,
			},
		},
	}
	summary := func(answers map[string]string) [][]string {
		return [][]string{
			{"name", answers["name"]},
			{"runtime", answers["runtime"]},
			{"template", answers["template"]},
		}
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
	if len(m.snap.Agents) == 0 {
		var b strings.Builder
		b.WriteString(m.loadingOr("No registered agents.", !m.loadedAt.IsZero()))
		b.WriteByte('\n')
		if m.agentErr != "" {
			b.WriteString("\n" + errorStyle.Render(m.agentErr) + "\n")
		}
		b.WriteString("\n" + mutedStyle.Render("n new agent"))
		return b.String()
	}
	var b strings.Builder
	rows := [][]string{{"", "NAME", "RUNTIME", "ROLE", "HEALTH"}}
	for i, a := range m.snap.Agents {
		cursor, name := "  ", a.Name
		if i == m.agentCursor {
			cursor, name = "▸ ", selectedRowStyle.Render(a.Name)
		}
		rows = append(rows, []string{cursor, name, a.Runtime, dash(a.Role), dash(a.Health)})
	}
	b.WriteString(renderRows(rows))
	if m.agentErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.agentErr) + "\n")
	}
	if m.optimizeBusy {
		b.WriteString("\n" + mutedStyle.Render("starting optimization…") + "\n")
	}
	b.WriteString(mutedStyle.Render("enter detail  n new  o optimize  D delete"))
	b.WriteByte('\n')
	return b.String()
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

	b.WriteString(headerStyle.Render("recent jobs"))
	b.WriteByte('\n')
	jobs := agentJobs(m.snap.JobRows, a.Name, 5)
	if len(jobs) == 0 {
		b.WriteString(mutedStyle.Render("none") + "\n")
	} else {
		for _, job := range jobs {
			b.WriteString(job.ID + "  " + job.Type + "  " + jobStateColor(job.State) + "\n")
		}
	}
	b.WriteByte('\n')

	b.WriteString(headerStyle.Render("template versions"))
	b.WriteByte('\n')
	switch {
	case a.TemplateID == "":
		b.WriteString(mutedStyle.Render("no template") + "\n")
	case m.agentVersionsErr != "":
		b.WriteString(errorStyle.Render(m.agentVersionsErr) + "\n")
	case !m.agentVersionsLoaded:
		b.WriteString(mutedStyle.Render("loading…") + "\n")
	case len(m.agentVersions) == 0:
		b.WriteString(mutedStyle.Render("no versions") + "\n")
	default:
		for _, v := range m.agentVersions {
			line := "v" + strconv.Itoa(v.Number) + "  " + versionStateColor(v.State)
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
