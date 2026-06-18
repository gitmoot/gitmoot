package tui

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jerryfane/gitmoot/internal/db"
)

type page int

const (
	pageAttention page = iota
	pageActivity
	pageTrains
	pageAgents
	pageSessions
	pageJobs
	pageLocks
	pageHealth
	pageConfig
)

// label is the (short) sidebar entry; title is the page heading. The sidebar
// is 10-14 columns wide, so labels must stay short or they wrap.
var pages = []struct {
	page  page
	label string
	title string
}{
	{pageAttention, "Attention", "Attention"},
	{pageActivity, "Activity", "Activity — live agent work"},
	{pageTrains, "Trains", "Trains"},
	{pageAgents, "Agents", "Agents"},
	{pageSessions, "Workers", "Runtime workers (agent sessions)"},
	{pageJobs, "Jobs", "Jobs"},
	{pageLocks, "Locks", "Locks"},
	{pageHealth, "Health", "Health"},
	{pageConfig, "Config", "Config"},
}

// mode is the interaction mode; modeNormal navigates pages, the others are
// modal overlays that capture keys until dismissed.
type mode int

const (
	modeNormal mode = iota
	modeAnswerChoice
	modeAnswerText
	modeConfirmDismiss
	modeTrainDetail
	modeJobDetail
	modeSessionDetail
	modeConfirmSessionStop
	modeConfirmJobRetry
	modeConfirmJobCancel
	modeBugReportPreview
	modeTrainStopReason
	modeConfirmTrainDelete
	modeConfirmTrainRepoCleanup
	modeAgentDetail
	modeAgentRevertPick
	modeConfirmAgentRevert
	modeConfirmAgentDelete
	modeConfirmAgentGroupDelete
	modeAgentVersionView
	modeAgentRuntimePick
	modeConfigEdit
)

const defaultInterval = 5 * time.Second

// Model is the bubbletea model for the dashboard TUI. One Deps.Load pass fills
// every page, so a single in-flight flag (not a per-page map) guards refreshes.
type Model struct {
	deps     Deps
	selected int
	width    int
	height   int
	viewport viewport.Model

	snap     Snapshot
	loadedAt time.Time
	loadErr  string
	inFlight bool

	// expanded records explicit user open(true)/close(false) decisions for
	// collapsible group headers (Attention / Trains), keyed by a page-qualified
	// group key; it overrides collapseByDefault and persists across refresh ticks
	// while the key is stable (a Trains group's key includes its status section,
	// so a session changing section moves to that section's group). collapseByDefault
	// is the state for groups the user has not touched — the live dashboard sets it
	// true so groups start folded.
	expanded          map[string]bool
	collapseByDefault bool

	// Attention / Activity / Trains page interaction state.
	mode           mode
	promptCursor   int                  // selected row in snap.Prompts on the Attention page
	activityCursor int                  // selected active root on the Activity page
	trainCursor    int                  // selected row in snap.Trains on the Trains page
	active         db.InteractivePrompt // prompt being answered/dismissed in an overlay
	activeTrain    TrainSession         // train shown in modeTrainDetail
	choiceIdx      int                  // selected choice in modeAnswerChoice
	input          textinput.Model      // free-text answer in modeAnswerText
	actionErr      string               // inline error from the last Answer/Dismiss attempt
	actionBusy     bool                 // an action is in flight; suppress re-submit
	showHelp       bool                 // '?' help overlay
	tickGen        int                  // current tick chain; stale generations are dropped

	// Sessions page interaction state.
	sessionCursor      int     // selected row in sessionRows()
	activeSession      Session // session shown in modeSessionDetail / being stopped
	activeSessionCount int     // members collapsed into the selected row (>1 for a bg group)
	sessionNotice      string  // muted note on the Sessions page (e.g. group not stoppable)

	// Jobs page interaction state.
	jobCursor int // selected row in jobsVisibleRows(); resolve to a job via
	// selectedItemIndex(jobsVisibleRows(), jobCursor) -> jobsOrdered(), NOT
	// snap.JobRows (grouping reorders by status and collapsed headers take slots).
	jobGroups         []jobStatusGroup    // per-snapshot status grouping cache (see jobsByStatusGroup)
	activeJob         JobRow              // job shown in detail / being confirmed
	jobEvents         []JobEventView      // lazy-loaded event history for the detail view
	jobEventsLoaded   bool                // the detail's event load has returned (possibly empty)
	jobEventsErr      string              // error from the detail's event load
	activeJobDetail   JobDetail           // lazy-loaded parsed payload (request/result)
	jobDetailLoaded   bool                // the detail's payload load has returned
	bugReport         BugReportPreview    // redacted issue draft for modeBugReportPreview
	bugReportLoaded   bool                // the preview build has returned
	bugReportErr      string              // error from building the preview
	bugReportURL      string              // created issue URL
	bugReportExisting bool                // bugReportURL points to an existing duplicate
	cancelling        map[string]struct{} // jobs with a cancel requested, until settled
	daemonBusy        bool                // a daemon start is in flight; suppress re-submit
	daemonErr         string              // error from the last daemon start attempt

	// Health page state (lazy: the checks shell out, so they load on first
	// open and on r, not on the refresh tick).
	healthChecks  []HealthCheck
	healthLoaded  bool
	healthLoading bool
	healthErr     string

	// Config page state.
	configProblems  []string    // validation problems after an $EDITOR edit (empty = valid)
	configEditErr   string      // the editor itself failed to launch/run
	configCursor    int         // selected editable field on the Config page
	configField     ConfigField // field being edited inline
	configActionErr string      // inline write error in the edit overlay

	// Trains page action state.
	pendingRepos []string // gitmoot-created repos offered for cleanup after a delete

	// Agents page interaction state.
	agentCursor         int               // selected row in snap.Agents
	showAllAgents       bool              // Agents page: include hidden skillopt-* training agents
	groupDeleteLabel    string            // template-group label being bulk-deleted (confirm)
	groupDeleteNames    []string          // agent names in the group being bulk-deleted
	activeAgent         Agent             // agent shown in detail / being confirmed
	agentVersions       []TemplateVersion // lazy-loaded template version history
	agentVersionsLoaded bool              // the version load has returned (possibly empty)
	agentVersionsErr    string            // error from the version load
	versionCursor       int               // selected row in the revert pick list
	revertVersion       TemplateVersion   // version being confirmed for revert
	agentDetailCursor   int               // selected row in the agent detail (recent jobs then versions)
	jobDetailReturn     mode              // mode to return to when a job detail closes (modeNormal, or modeAgentDetail when opened from the agent detail)
	runtimePickCursor   int               // selected runtime in the switch-runtime overlay
	activeAgentVersion  TemplateVersion   // version shown in the content pager
	versionView         viewport.Model    // pager for a version's content
	versionViewID       string            // version id the pager content belongs to
	versionViewErr      string            //
	versionViewLoaded   bool              //
	agentErr            string            // inline error on the Agents page (e.g. create failed)
	agentNotice         string            // non-blocking note on the Agents page (e.g. prompt missing contract)
	optimizeBusy        bool              // a train session is being scaffolded/started
	formPending         bool              // a form push is in flight; suppress re-dispatch

	// Pending custom-prompt agent creation: the create form collected these,
	// then handed off to $EDITOR; the edited content completes the create.
	pendingAgentName    string
	pendingAgentRuntime string
}

// New returns a Model ready for tea.NewProgram. It starts in the loading state;
// Init issues the first Load and arms the refresh tick.
func New(deps Deps) Model {
	m := Model{
		deps:              deps,
		width:             100,
		height:            30,
		viewport:          viewport.New(80, 20),
		inFlight:          true,
		cancelling:        map[string]struct{}{},
		expanded:          map[string]bool{},
		collapseByDefault: deps.CollapseGroupsByDefault,
	}
	m.resizeViewport()
	m.viewport.SetContent(m.content())
	return m
}

func (m Model) interval() time.Duration {
	if m.deps.Interval <= 0 {
		return defaultInterval
	}
	return m.deps.Interval
}

// Init satisfies tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(loadSnapshot(m.deps), tick(m.interval(), m.tickGen))
}

// Update satisfies tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 && msg.Height > 0 {
			m.width = msg.Width
			m.height = msg.Height
			m.resizeViewport()
			if m.versionViewLoaded {
				m.versionView.Width = max(20, m.width-4)
				m.versionView.Height = max(5, m.height-6)
			}
		}
	case tea.KeyMsg:
		// Modal overlays capture keys first; only ctrl+c stays global.
		switch m.mode {
		case modeJobDetail, modeConfirmJobRetry, modeConfirmJobCancel, modeBugReportPreview:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m.updateJobOverlay(msg)
		case modeTrainStopReason, modeConfirmTrainDelete, modeConfirmTrainRepoCleanup:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m.updateTrainOverlay(msg)
		case modeAgentDetail, modeAgentRevertPick, modeConfirmAgentRevert, modeConfirmAgentDelete, modeConfirmAgentGroupDelete, modeAgentVersionView, modeAgentRuntimePick:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m.updateAgentOverlay(msg)
		case modeConfigEdit:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m.updateConfigOverlay(msg)
		case modeSessionDetail, modeConfirmSessionStop:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m.updateSessionOverlay(msg)
		}
		if m.mode != modeNormal {
			return m.updateOverlay(msg)
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "tab", "right":
			m.selected = (m.selected + 1) % len(pages)
			m.sessionNotice = ""
			m.viewport.GotoTop()
			if cmd := m.maybeLoadHealth(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			m.viewport.SetContent(m.content())
			return m, tea.Batch(cmds...)
		case "shift+tab", "left":
			m.selected--
			if m.selected < 0 {
				m.selected = len(pages) - 1
			}
			m.sessionNotice = ""
			m.viewport.GotoTop()
			if cmd := m.maybeLoadHealth(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			m.viewport.SetContent(m.content())
			return m, tea.Batch(cmds...)
		case "r":
			if pages[m.selected].page == pageHealth {
				// Force a re-run of the checks (they shell out, so they are not
				// on the snapshot tick).
				if cmd := m.loadHealth(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			m.viewport.SetContent(m.content())
			return m, tea.Batch(cmds...)
		case "up", "k":
			if cursor, n := m.pageCursor(); cursor != nil && n > 0 {
				if *cursor > 0 {
					*cursor--
				}
				m.sessionNotice = ""
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "down", "j":
			if cursor, n := m.pageCursor(); cursor != nil && n > 0 {
				if *cursor < n-1 {
					*cursor++
				}
				m.sessionNotice = ""
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case " ":
			// Toggle a collapsible group header (Attention / Trains).
			if m.toggleCurrentGroup() {
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "a":
			if pages[m.selected].page == pageAttention {
				if cmd := m.openAnswer(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if pages[m.selected].page == pageAgents {
				// Toggle the hidden skillopt-* training agents in/out of the list.
				m.showAllAgents = !m.showAllAgents
				m.agentCursor = clampCursor(m.agentCursor, len(m.visibleAgents()))
				m.viewport.GotoTop()
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "d":
			if pages[m.selected].page == pageAttention {
				m.openDismiss()
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if t, ok := m.trainUnderCursor(); ok && deadTrainPhase(t.Phase) {
				m.openTrainDelete(t)
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "enter":
			// On a collapsible page, enter on a group header toggles it (so the
			// header is a usable target without leaving the keyboard home keys).
			if m.cursorOnHeader() {
				m.toggleCurrentGroup()
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if pages[m.selected].page == pageConfig {
				fields := m.configEditableFields()
				if m.deps.SetConfigScalar != nil && m.configCursor < len(fields) {
					cmd := m.openConfigEdit(fields[m.configCursor])
					m.viewport.SetContent(m.content())
					return m, cmd
				}
				return m, tea.Batch(cmds...)
			}
			if pages[m.selected].page == pageTrains {
				// With a Root router, open the full train-run view; otherwise the
				// inline detail (keeps the model usable standalone and old tests
				// green). Pushing is suppressed while an optimize start is in
				// flight: its result message routes to the top of the stack, so a
				// covering view would swallow it. Resolve the session through the
				// cursor (display order), not raw snapshot order.
				if t, ok := m.trainUnderCursor(); ok {
					if m.deps.OpenTrain != nil && !m.optimizeBusy {
						return m, Push(m.deps.OpenTrain(t.ID))
					}
					m.openTrainDetail()
				}
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if pages[m.selected].page == pageSessions {
				m.openSessionDetail()
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if r, ok := m.activityUnderCursor(); ok {
				// Open the selected job's detail — a root coordinator (full request
				// + delegation tree) or a delegate (the prompt it received + result).
				cmd := m.openJobDetail(r)
				m.viewport.SetContent(m.content())
				return m, cmd
			}
			if agent, ok := m.agentUnderCursor(); ok {
				cmd := m.openAgentDetail(agent)
				m.viewport.SetContent(m.content())
				return m, cmd
			}
			if job, ok := m.jobUnderCursor(); ok {
				cmd := m.openJobDetail(job)
				m.viewport.SetContent(m.content())
				return m, cmd
			}
		case "R":
			if job, ok := m.jobUnderCursor(); ok && jobRetryable(job.State) {
				m.openJobConfirm(job, false)
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "B":
			if job, ok := m.jobUnderCursor(); ok && jobReportable(job.State) {
				cmd := m.openBugReportPreview(job)
				m.viewport.SetContent(m.content())
				return m, cmd
			}
		case "c":
			if job, ok := m.jobUnderCursor(); ok && jobCancelable(job.State) {
				m.openJobConfirm(job, true)
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "s":
			// Only once the first snapshot confirmed the daemon is down (the
			// zero-value snapshot also reads as not-running), and never while a
			// start is already in flight.
			if (pages[m.selected].page == pageAttention || pages[m.selected].page == pageHealth) &&
				!m.snap.Daemon.Running && !m.loadedAt.IsZero() && !m.daemonBusy {
				m.daemonBusy = true
				m.daemonErr = ""
				cmds = append(cmds, daemonStartCmd(m.deps))
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if t, ok := m.trainUnderCursor(); ok && !deadTrainPhase(t.Phase) {
				cmd := m.openTrainStop(t)
				m.viewport.SetContent(m.content())
				return m, cmd
			}
			if pages[m.selected].page == pageSessions && m.deps.StopSession != nil {
				rows := m.sessionRows()
				if m.sessionCursor < len(rows) {
					m.openSessionStop(rows[m.sessionCursor])
				}
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "n":
			// Form construction touches the database, so it runs as a command
			// (a synchronous call here would freeze the UI on a busy store).
			if pages[m.selected].page == pageAgents && m.deps.OpenAgentCreate != nil &&
				!m.formPending && !m.optimizeBusy {
				m.formPending = true
				m.agentErr = ""
				return m, openAgentFormCmd(m.deps)
			}
		case "D":
			if agent, ok := m.agentUnderCursor(); ok {
				m.openAgentDelete(agent)
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "X":
			if pages[m.selected].page == pageAgents {
				if agent, ok := m.agentUnderCursor(); ok {
					// The "Standalone agents" bucket is a heterogeneous catch-all
					// (every template-less agent), not a coherent set — bulk-deleting
					// it would be a footgun, so fall back to single-agent delete.
					if agent.TemplateID == "" {
						m.openAgentDelete(agent)
						m.viewport.SetContent(m.content())
						return m, tea.Batch(cmds...)
					}
					if m.deps.DeleteAgents != nil {
						m.openAgentGroupDelete()
						m.viewport.SetContent(m.content())
						return m, tea.Batch(cmds...)
					}
				}
			}
		case "o":
			if agent, ok := m.agentUnderCursor(); ok && agent.TemplateID != "" &&
				m.deps.OpenAgentOptimize != nil && !m.formPending && !m.optimizeBusy {
				m.formPending = true
				m.activeAgent = agent
				m.agentErr = ""
				return m, openAgentOptimizeCmd(m.deps, agent)
			}
		case "e":
			if agent, ok := m.agentUnderCursor(); ok && m.deps.SetAgentRuntime != nil {
				m.openAgentRuntimePick(agent)
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if pages[m.selected].page == pageConfig && m.deps.EditConfig != nil {
				m.configEditErr = ""
				m.configProblems = nil
				// tea.ExecProcess suspends the program for the editor; the
				// result returns as a ConfigEditedMsg.
				return m, m.deps.EditConfig()
			}
		case "?":
			m.showHelp = !m.showHelp
			m.viewport.SetContent(m.content())
			return m, tea.Batch(cmds...)
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	case answerResultMsg:
		// Apply only while the answer overlay is still open: the prompt
		// overlays allow esc while busy, so a stale result must not close or
		// pollute whatever overlay the user opened since. Refresh regardless.
		if m.mode == modeAnswerChoice || m.mode == modeAnswerText {
			m.actionBusy = false
			if msg.err != nil {
				m.actionErr = msg.err.Error()
			} else {
				m.mode = modeNormal
				m.actionErr = ""
			}
		}
		if msg.err == nil {
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case dismissResultMsg:
		// A prompt already gone (removed by another terminal or a prior refresh)
		// is treated as success, since the goal was to remove it.
		dismissed := msg.err == nil || errors.Is(msg.err, db.ErrInteractivePromptNotFound)
		if m.mode == modeConfirmDismiss {
			m.actionBusy = false
			if dismissed {
				m.mode = modeNormal
				m.actionErr = ""
			} else {
				m.actionErr = msg.err.Error()
			}
		}
		if dismissed {
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case jobEventsMsg:
		if msg.id == m.activeJob.ID {
			m.jobEventsLoaded = true
			if msg.err != nil {
				m.jobEventsErr = msg.err.Error()
			} else {
				m.jobEventsErr = ""
				m.jobEvents = msg.events
			}
		}
	case jobDetailMsg:
		// A malformed/absent payload yields a zero JobDetail and no error; the
		// detail simply omits the request/result blocks.
		if msg.id == m.activeJob.ID {
			m.jobDetailLoaded = true
			m.activeJobDetail = msg.detail
		}
	case bugReportPreviewMsg:
		if msg.id == m.activeJob.ID {
			m.bugReportLoaded = true
			if msg.err != nil {
				m.bugReportErr = msg.err.Error()
			} else {
				m.bugReportErr = ""
				m.bugReport = msg.preview
			}
		}
	case healthChecksMsg:
		m.healthLoading = false
		m.healthLoaded = true
		if msg.err != nil {
			m.healthErr = msg.err.Error()
		} else {
			m.healthErr = ""
			m.healthChecks = msg.checks
		}
	case configWriteMsg:
		if m.mode == modeConfigEdit {
			m.actionBusy = false
			if msg.err != nil {
				m.configActionErr = msg.err.Error()
			} else {
				m.mode = modeNormal
				m.configActionErr = ""
				if cmd := m.queueLoad(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	case ConfigEditedMsg:
		if msg.Err != nil {
			m.configEditErr = msg.Err.Error()
		} else {
			m.configEditErr = ""
			// Validate the edited file and reload so the page shows the new
			// content; problems render inline until the next clean edit.
			if m.deps.ValidateConfig != nil {
				m.configProblems = m.deps.ValidateConfig()
			}
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case AgentPromptEditedMsg:
		// The custom-prompt editor exited; create the agent from the saved text.
		name, runtime := m.pendingAgentName, m.pendingAgentRuntime
		m.pendingAgentName, m.pendingAgentRuntime = "", ""
		m.agentNotice = ""
		switch {
		case msg.Err != nil:
			m.agentErr = msg.Err.Error()
		case strings.TrimSpace(msg.Content) == "":
			m.agentErr = "custom prompt was empty; agent not created"
		default:
			m.agentErr = ""
			if !strings.Contains(msg.Content, "gitmoot_result") {
				// Non-blocking: the prompt omits the result contract. A dedicated
				// notice survives the create-success handler's agentErr reset.
				m.agentNotice = "note: prompt has no gitmoot_result contract; created anyway"
			}
			cmds = append(cmds, agentCreateWithPromptCmd(m.deps, name, runtime, msg.Content))
		}
	case sessionActionMsg:
		if m.mode == modeConfirmSessionStop {
			m.actionBusy = false
			if msg.err != nil {
				m.actionErr = msg.err.Error()
			} else {
				m.mode = modeNormal
				m.actionErr = ""
			}
		}
		if msg.err == nil {
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case trainStopMsg:
		if m.mode == modeTrainStopReason {
			m.actionBusy = false
			if msg.err != nil {
				m.actionErr = msg.err.Error()
			} else {
				m.mode = modeNormal
				m.actionErr = ""
			}
		}
		if msg.err == nil {
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case trainDeleteMsg:
		if m.mode == modeConfirmTrainDelete {
			m.actionBusy = false
			if msg.err != nil {
				// e.g. the lock-refusal from DeleteSkillOptTrainSession; stays
				// in the confirm so the user reads it.
				m.actionErr = msg.err.Error()
			} else {
				m.actionErr = ""
				if len(msg.repos) > 0 {
					m.pendingRepos = msg.repos
					m.mode = modeConfirmTrainRepoCleanup
				} else {
					m.mode = modeNormal
				}
			}
		}
		if msg.err == nil {
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case trainRepoCleanupMsg:
		if m.mode == modeConfirmTrainRepoCleanup {
			m.actionBusy = false
			if len(msg.errs) > 0 {
				// Scope errors carry their remedy verbatim; keep the confirm
				// open with only the still-failing repos on offer, so a retry
				// does not replay repos that were already deleted.
				m.actionErr = strings.Join(msg.errs, "\n")
				m.pendingRepos = msg.failed
			} else {
				m.mode = modeNormal
				m.actionErr = ""
				m.pendingRepos = nil
			}
		}
		if cmd := m.queueLoad(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case agentVersionsMsg:
		if m.activeAgent.TemplateID == msg.templateID {
			m.agentVersionsLoaded = true
			if msg.err != nil {
				m.agentVersionsErr = msg.err.Error()
			} else {
				m.agentVersionsErr = ""
				m.agentVersions = msg.versions
				m.agentDetailCursor = clampCursor(m.agentDetailCursor, m.agentDetailSelectableCount())
			}
		}
	case versionContentMsg:
		if msg.versionID == m.versionViewID {
			m.versionViewLoaded = true
			if msg.err != nil {
				m.versionViewErr = msg.err.Error()
			} else {
				m.versionViewErr = ""
				content := msg.content
				if strings.TrimSpace(content) == "" {
					content = mutedStyle.Render("(no content)")
				}
				m.versionView = viewport.New(max(20, m.width-4), max(5, m.height-6))
				m.versionView.SetContent(content)
			}
		}
	case agentFormResultMsg:
		// The pushed create form popped itself; run the registration unless it
		// was aborted.
		if !msg.result.Aborted {
			m.agentErr = ""
			m.agentNotice = ""
			if msg.result.Values["template"] == agentCustomPromptValue && m.deps.EditAgentPrompt != nil {
				// Custom prompt: stash the answers and hand off to $EDITOR; the
				// edited content completes the create (AgentPromptEditedMsg).
				m.pendingAgentName = msg.result.Values["name"]
				m.pendingAgentRuntime = msg.result.Values["runtime"]
				cmds = append(cmds, m.deps.EditAgentPrompt(msg.result.Values["seed"]))
			} else {
				cmds = append(cmds, agentCreateCmd(m.deps, msg.result.Values))
			}
		}
	case agentOptimizeFormResultMsg:
		m.formPending = false
		if !msg.result.Aborted {
			m.agentErr = ""
			m.optimizeBusy = true
			cmds = append(cmds, startOptimizeCmd(m.deps, msg.templateID, msg.result.Values))
		}
	case optimizeStartedMsg:
		m.optimizeBusy = false
		if msg.err != nil {
			m.agentErr = msg.err.Error()
		} else {
			m.agentErr = ""
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			// Straight into the new session's live phase view.
			if m.deps.OpenTrain != nil && msg.sessionID != "" {
				cmds = append(cmds, Push(m.deps.OpenTrain(msg.sessionID)))
			}
		}
	case agentActionMsg:
		switch msg.verb {
		case "create", "form":
			// No overlay is open by the time a create (or a failed form
			// construction) settles; surface errors inline on the Agents page.
			if msg.verb == "form" {
				m.formPending = false
			}
			if msg.err != nil {
				m.agentErr = msg.err.Error()
			} else {
				m.agentErr = ""
			}
		case "delete":
			if m.mode == modeConfirmAgentDelete {
				m.actionBusy = false
				if msg.err != nil {
					m.actionErr = msg.err.Error()
				} else {
					m.mode = modeNormal
					m.actionErr = ""
				}
			}
		case "runtime":
			if m.mode == modeAgentRuntimePick {
				m.actionBusy = false
				if msg.err != nil {
					m.actionErr = msg.err.Error()
				} else {
					m.mode = modeNormal
					m.actionErr = ""
				}
			}
		case "revert":
			if m.mode == modeConfirmAgentRevert {
				m.actionBusy = false
				if msg.err != nil {
					m.actionErr = msg.err.Error()
				} else {
					// Back to the detail with a fresh version history.
					m.mode = modeAgentDetail
					m.actionErr = ""
					m.agentVersionsLoaded = false
					m.agentVersions = nil
					if m.activeAgent.TemplateID != "" {
						cmds = append(cmds, agentVersionsCmd(m.deps, m.activeAgent.TemplateID))
					}
				}
			}
		}
		if msg.err == nil {
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case agentGroupDeleteMsg:
		if m.mode == modeConfirmAgentGroupDelete {
			// The deletes are already committed (each is its own tx), so always
			// close the overlay, report what actually happened, and refresh — even
			// on a partial error — so the list never shows already-deleted agents
			// and a retry can't wedge re-deleting them.
			m.actionBusy = false
			m.mode = modeNormal
			note := "deleted " + strconv.Itoa(msg.deleted) + " agent(s)"
			if len(msg.skipped) > 0 {
				note += " · " + strconv.Itoa(len(msg.skipped)) + " skipped (active jobs)"
			}
			m.agentNotice = note
			if msg.err != nil {
				m.agentErr = msg.err.Error()
			} else {
				m.agentErr = ""
			}
			m.agentCursor = clampCursor(m.agentCursor, len(m.visibleAgents()))
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case daemonStartMsg:
		// Daemon start has its own busy/error state so its result cannot close
		// or pollute an unrelated job confirm that opened in the meantime.
		m.daemonBusy = false
		if msg.err != nil {
			m.daemonErr = msg.err.Error()
		} else {
			m.daemonErr = ""
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case jobActionMsg:
		if msg.err == nil && msg.verb == "cancel" && msg.id != "" {
			m.cancelling[msg.id] = struct{}{}
		}
		if m.mode == modeConfirmJobRetry || m.mode == modeConfirmJobCancel {
			m.actionBusy = false
			if msg.err != nil {
				m.actionErr = msg.err.Error()
			} else {
				m.mode = modeNormal
				m.actionErr = ""
			}
		}
		if msg.err == nil {
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case bugReportCreateMsg:
		if m.mode == modeBugReportPreview && msg.id == m.activeJob.ID {
			m.actionBusy = false
			if msg.err != nil {
				m.actionErr = msg.err.Error()
			} else {
				m.actionErr = ""
				m.bugReportURL = msg.url
				m.bugReportExisting = msg.existing
			}
		}
	case snapshotMsg:
		m.inFlight = false
		if msg.err != nil {
			m.loadErr = msg.err.Error()
		} else {
			m.loadErr = ""
			m.snap = msg.snap
			m.loadedAt = msg.at
			m.jobGroups = computeJobsByStatusGroup(m.snap.JobRows)
			m.clampPromptCursor()
			m.clampTrainCursor()
			m.clampJobCursor()
			m.activityCursor = clampCursor(m.activityCursor, m.activitySelectableLen())
			m.agentCursor = clampCursor(m.agentCursor, len(m.visibleAgents()))
			m.agentDetailCursor = clampCursor(m.agentDetailCursor, m.agentDetailSelectableCount())
			m.sessionCursor = clampCursor(m.sessionCursor, len(m.sessionRows()))
			m.configCursor = clampCursor(m.configCursor, len(m.configEditableFields()))
			// A cancel-requested job that has settled no longer needs the
			// transitional "cancelling…" label.
			for id := range m.cancelling {
				settled := true
				for _, job := range m.snap.JobRows {
					if job.ID == id && jobCancelable(job.State) {
						settled = false
						break
					}
				}
				if settled {
					delete(m.cancelling, id)
				}
			}
		}
	case refreshNudgeMsg:
		// Resumed after a pop: the old tick chain died unhandled under the child
		// view, so refresh now and start a NEW chain. Bumping the generation also
		// kills a stale pre-push tick that would otherwise re-arm a second chain.
		// A pop also means any pushed form is gone — and any snapshot load that
		// was in flight when the push happened had its result routed to the
		// covering view and dropped, so the in-flight latch must reset or
		// queueLoad would suppress every future refresh.
		m.formPending = false
		m.inFlight = false
		m.tickGen++
		if cmd := m.queueLoad(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, tick(m.interval(), m.tickGen))
	case tickMsg:
		if msg.gen != m.tickGen {
			break // a tick from a dead chain; do not re-arm
		}
		if cmd := m.queueLoad(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, tick(m.interval(), m.tickGen))
	}
	m.viewport.SetContent(m.content())
	return m, tea.Batch(cmds...)
}

// View satisfies tea.Model.
func (m Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}
	sidebarWidth := sidebarWidth(m.width)
	bodyWidth := max(0, m.width-sidebarWidth-1)
	sidebar := renderSidebar(m.selected, sidebarWidth, m.height)
	bodyContent := lipgloss.JoinVertical(
		lipgloss.Left,
		m.viewport.View(),
		mutedStyle.Width(max(1, bodyWidth-2)).Render(m.footerHelp()),
	)
	body := bodyStyle.
		Width(bodyWidth).
		Height(max(0, m.height)).
		Render(bodyContent)
	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, body)
}

func (m *Model) resizeViewport() {
	sidebar := sidebarWidth(m.width)
	m.viewport.Width = max(20, m.width-sidebar-3)
	m.viewport.Height = max(5, m.height-4)
}

// queueLoad starts a refresh unless one is already in flight, mirroring the
// agent-tools suppression pattern so overlapping ticks/keys do not stack loads.
func (m *Model) queueLoad() tea.Cmd {
	if m.inFlight {
		return nil
	}
	m.inFlight = true
	return loadSnapshot(m.deps)
}

// maybeLoadHealth dispatches the health checks the first time the Health page
// is shown; subsequent visits reuse the cache until r forces a reload.
func (m *Model) maybeLoadHealth() tea.Cmd {
	if pages[m.selected].page != pageHealth || m.healthLoaded || m.healthLoading {
		return nil
	}
	return m.loadHealth()
}

// loadHealth (re)dispatches the health checks.
func (m *Model) loadHealth() tea.Cmd {
	if m.deps.HealthChecks == nil || m.healthLoading {
		return nil
	}
	m.healthLoading = true
	m.healthErr = ""
	deps := m.deps
	return func() tea.Msg {
		checks, err := deps.HealthChecks()
		return healthChecksMsg{checks: checks, err: err}
	}
}

// healthRequiredFailed reports whether any loaded required check failed; the
// Attention page surfaces a cross-link when it does.
func (m Model) healthRequiredFailed() bool {
	if !m.healthLoaded {
		return false
	}
	for _, check := range m.healthChecks {
		if check.Required && check.Status == "fail" {
			return true
		}
	}
	return false
}

// pageCursor returns the selected page's cursor and list length, or nil for
// pages without a selectable list. New cursored pages only need a case here.
func (m *Model) pageCursor() (*int, int) {
	switch pages[m.selected].page {
	case pageAttention:
		return &m.promptCursor, selectableCount(m.attentionVisibleRows())
	case pageActivity:
		return &m.activityCursor, m.activitySelectableLen()
	case pageTrains:
		return &m.trainCursor, selectableCount(m.trainVisibleRows())
	case pageAgents:
		return &m.agentCursor, len(m.visibleAgents())
	case pageSessions:
		return &m.sessionCursor, len(m.sessionRows())
	case pageJobs:
		return &m.jobCursor, selectableCount(m.jobsVisibleRows())
	case pageConfig:
		return &m.configCursor, len(m.configEditableFields())
	}
	return nil, 0
}

// clampCursor keeps a list cursor within [0, n-1] after a refresh removes rows.
func clampCursor(cursor, n int) int {
	if cursor >= n {
		cursor = n - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	return cursor
}

// clampPromptCursor keeps the Attention cursor within the current selectable
// rows (prompt/job leaves plus collapsed repo headers) after a refresh/collapse.
func (m *Model) clampPromptCursor() {
	m.promptCursor = clampCursor(m.promptCursor, selectableCount(m.attentionVisibleRows()))
}

// clampTrainCursor keeps the Trains cursor within the current selectable rows.
func (m *Model) clampTrainCursor() {
	m.trainCursor = clampCursor(m.trainCursor, selectableCount(m.trainVisibleRows()))
}

// currentListRows returns the visible collapsible rows for the current page, if
// it is a collapsible-list page (Attention or Trains).
func (m Model) currentListRows() ([]listRow, bool) {
	switch pages[m.selected].page {
	case pageAttention:
		return m.attentionVisibleRows(), true
	case pageTrains:
		return m.trainVisibleRows(), true
	case pageJobs:
		return m.jobsVisibleRows(), true
	}
	return nil, false
}

// selectedListRow returns the row under the current page's cursor (among the
// selectable rows), if the page is a collapsible list.
func (m Model) selectedListRow() (listRow, bool) {
	rows, ok := m.currentListRows()
	if !ok {
		return listRow{}, false
	}
	cursor, _ := m.pageCursor()
	if cursor == nil {
		return listRow{}, false
	}
	return selectableRowAt(rows, *cursor)
}

// cursorOnHeader reports whether the cursor sits on a collapsed group header
// (the only kind of header the cursor can land on).
func (m Model) cursorOnHeader() bool {
	r, ok := m.selectedListRow()
	return ok && r.header
}

// groupCollapsed reports whether a group is collapsed. An explicit user
// open/close (stored in m.expanded) wins; otherwise the page default applies
// (collapseByDefault — the live dashboard starts groups folded to reduce
// clutter, while tests default to expanded).
func (m Model) groupCollapsed(key string) bool {
	if v, ok := m.expanded[key]; ok {
		return !v
	}
	return m.collapseByDefault
}

// toggleCurrentGroup opens or closes a repo group: on a collapsed header it
// expands; on a leaf it collapses that leaf's group. The state is stored
// explicitly (not as a delete), so it holds regardless of the default. Since the
// cursor stays in place, pressing space on a just-opened group's first row
// closes it again. Returns true when it changed something.
func (m *Model) toggleCurrentGroup() bool {
	r, ok := m.selectedListRow()
	if !ok {
		return false
	}
	if m.expanded == nil {
		m.expanded = map[string]bool{}
	}
	switch {
	case r.header && r.collapsed:
		m.expanded[r.key] = true
	case !r.header && r.groupKey != "":
		m.expanded[r.groupKey] = false
		// Folding from a leaf removes that leaf (and its siblings) from the
		// selectable set; move the cursor onto the now-collapsed header so the
		// highlight follows the group instead of clamping onto an unrelated row.
		m.focusHeader(r.groupKey)
	default:
		return false
	}
	m.clampPromptCursor()
	m.clampTrainCursor()
	m.clampJobCursor()
	return true
}

// focusHeader points the current page's cursor at the selectable header whose
// group key matches, after that group was just collapsed.
func (m *Model) focusHeader(key string) {
	rows, ok := m.currentListRows()
	if !ok {
		return
	}
	cursor, _ := m.pageCursor()
	if cursor == nil {
		return
	}
	sel := 0
	for _, row := range rows {
		if !row.selectable() {
			continue
		}
		if row.header && row.key == key {
			*cursor = sel
			return
		}
		sel++
	}
}

// clampJobCursor keeps the Jobs cursor within the current selectable rows.
func (m *Model) clampJobCursor() {
	m.jobCursor = clampCursor(m.jobCursor, selectableCount(m.jobsVisibleRows()))
}

func loadSnapshot(deps Deps) tea.Cmd {
	return func() tea.Msg {
		snap, err := deps.Load()
		return snapshotMsg{snap: snap, err: err, at: time.Now()}
	}
}

func tick(d time.Duration, gen int) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{gen: gen} })
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
