package tui

import (
	"errors"
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
	pageTrains
	pageAgents
	pageSessions
	pageJobs
	pageLocks
)

var pages = []struct {
	page  page
	label string
}{
	{pageAttention, "Attention"},
	{pageTrains, "Trains"},
	{pageAgents, "Agents"},
	{pageSessions, "Sessions"},
	{pageJobs, "Jobs"},
	{pageLocks, "Locks"},
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
	modeConfirmJobRetry
	modeConfirmJobCancel
	modeTrainStopReason
	modeConfirmTrainDelete
	modeConfirmTrainRepoCleanup
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

	// Attention / Trains page interaction state.
	mode         mode
	promptCursor int                  // selected row in snap.Prompts on the Attention page
	trainCursor  int                  // selected row in snap.Trains on the Trains page
	active       db.InteractivePrompt // prompt being answered/dismissed in an overlay
	activeTrain  TrainSession         // train shown in modeTrainDetail
	choiceIdx    int                  // selected choice in modeAnswerChoice
	input        textinput.Model      // free-text answer in modeAnswerText
	actionErr    string               // inline error from the last Answer/Dismiss attempt
	actionBusy   bool                 // an action is in flight; suppress re-submit
	showHelp     bool                 // '?' help overlay
	tickGen      int                  // current tick chain; stale generations are dropped

	// Jobs page interaction state.
	jobCursor       int                 // selected row in snap.JobRows
	activeJob       JobRow              // job shown in detail / being confirmed
	jobEvents       []JobEventView      // lazy-loaded event history for the detail view
	jobEventsLoaded bool                // the detail's event load has returned (possibly empty)
	jobEventsErr    string              // error from the detail's event load
	cancelling      map[string]struct{} // jobs with a cancel requested, until settled
	daemonBusy      bool                // a daemon start is in flight; suppress re-submit
	daemonErr       string              // error from the last daemon start attempt

	// Trains page action state.
	pendingRepos []string // gitmoot-created repos offered for cleanup after a delete
}

// New returns a Model ready for tea.NewProgram. It starts in the loading state;
// Init issues the first Load and arms the refresh tick.
func New(deps Deps) Model {
	m := Model{
		deps:       deps,
		width:      100,
		height:     30,
		viewport:   viewport.New(80, 20),
		inFlight:   true,
		cancelling: map[string]struct{}{},
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
		}
	case tea.KeyMsg:
		// Modal overlays capture keys first; only ctrl+c stays global.
		switch m.mode {
		case modeJobDetail, modeConfirmJobRetry, modeConfirmJobCancel:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m.updateJobOverlay(msg)
		case modeTrainStopReason, modeConfirmTrainDelete, modeConfirmTrainRepoCleanup:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m.updateTrainOverlay(msg)
		}
		if m.mode != modeNormal {
			return m.updateOverlay(msg)
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "tab", "right":
			m.selected = (m.selected + 1) % len(pages)
			m.viewport.GotoTop()
			m.viewport.SetContent(m.content())
			return m, tea.Batch(cmds...)
		case "shift+tab", "left":
			m.selected--
			if m.selected < 0 {
				m.selected = len(pages) - 1
			}
			m.viewport.GotoTop()
			m.viewport.SetContent(m.content())
			return m, tea.Batch(cmds...)
		case "r":
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
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "down", "j":
			if cursor, n := m.pageCursor(); cursor != nil && n > 0 {
				if *cursor < n-1 {
					*cursor++
				}
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
			if pages[m.selected].page == pageTrains {
				// With a Root router, open the full train-run view; otherwise the
				// inline detail (keeps the model usable standalone and old tests
				// green).
				if m.deps.OpenTrain != nil && len(m.snap.Trains) > 0 {
					session := m.snap.Trains[m.trainCursor].ID
					return m, Push(m.deps.OpenTrain(session))
				}
				m.openTrainDetail()
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
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
			if pages[m.selected].page == pageAttention && !m.snap.Daemon.Running &&
				!m.loadedAt.IsZero() && !m.daemonBusy {
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
	case snapshotMsg:
		m.inFlight = false
		if msg.err != nil {
			m.loadErr = msg.err.Error()
		} else {
			m.loadErr = ""
			m.snap = msg.snap
			m.loadedAt = msg.at
			m.clampPromptCursor()
			m.clampTrainCursor()
			m.clampJobCursor()
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
	sidebar := renderSidebar(m.selected, sidebarWidth(m.width), m.height)
	body := bodyStyle.
		Width(max(0, m.width-sidebarWidth(m.width)-1)).
		Height(max(0, m.height)).
		Render(m.viewport.View())
	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, body)
}

func (m *Model) resizeViewport() {
	sidebar := sidebarWidth(m.width)
	m.viewport.Width = max(20, m.width-sidebar-3)
	m.viewport.Height = max(5, m.height-2)
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

// pageCursor returns the selected page's cursor and list length, or nil for
// pages without a selectable list. New cursored pages only need a case here.
func (m *Model) pageCursor() (*int, int) {
	switch pages[m.selected].page {
	case pageAttention:
		return &m.promptCursor, len(m.attentionItems())
	case pageTrains:
		return &m.trainCursor, len(m.snap.Trains)
	case pageJobs:
		return &m.jobCursor, len(m.snap.JobRows)
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

// clampPromptCursor keeps the Attention cursor within the current item list
// (prompts plus blocked/failed jobs) after a refresh removes rows.
func (m *Model) clampPromptCursor() {
	m.promptCursor = clampCursor(m.promptCursor, len(m.attentionItems()))
}

// clampTrainCursor keeps the Trains cursor within the current session list.
func (m *Model) clampTrainCursor() {
	m.trainCursor = clampCursor(m.trainCursor, len(m.snap.Trains))
}

// clampJobCursor keeps the Jobs cursor within the current job list.
func (m *Model) clampJobCursor() {
	m.jobCursor = clampCursor(m.jobCursor, len(m.snap.JobRows))
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
