package tui

import (
	"errors"
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
}

// New returns a Model ready for tea.NewProgram. It starts in the loading state;
// Init issues the first Load and arms the refresh tick.
func New(deps Deps) Model {
	m := Model{
		deps:     deps,
		width:    100,
		height:   30,
		viewport: viewport.New(80, 20),
		inFlight: true,
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
	return tea.Batch(loadSnapshot(m.deps), tick(m.interval()))
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
			if pages[m.selected].page == pageAttention && len(m.snap.Prompts) > 0 {
				if m.promptCursor > 0 {
					m.promptCursor--
				}
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if pages[m.selected].page == pageTrains && len(m.snap.Trains) > 0 {
				if m.trainCursor > 0 {
					m.trainCursor--
				}
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		case "down", "j":
			if pages[m.selected].page == pageAttention && len(m.snap.Prompts) > 0 {
				if m.promptCursor < len(m.snap.Prompts)-1 {
					m.promptCursor++
				}
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
			if pages[m.selected].page == pageTrains && len(m.snap.Trains) > 0 {
				if m.trainCursor < len(m.snap.Trains)-1 {
					m.trainCursor++
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
		case "enter":
			if pages[m.selected].page == pageTrains {
				m.openTrainDetail()
				m.viewport.SetContent(m.content())
				return m, tea.Batch(cmds...)
			}
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	case answerResultMsg:
		m.actionBusy = false
		if msg.err != nil {
			m.actionErr = msg.err.Error()
		} else {
			m.mode = modeNormal
			m.actionErr = ""
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case dismissResultMsg:
		m.actionBusy = false
		// A prompt already gone (removed by another terminal or a prior refresh)
		// is treated as success, since the goal was to remove it.
		if msg.err != nil && !errors.Is(msg.err, db.ErrInteractivePromptNotFound) {
			m.actionErr = msg.err.Error()
		} else {
			m.mode = modeNormal
			m.actionErr = ""
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
		}
	case tickMsg:
		if cmd := m.queueLoad(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, tick(m.interval()))
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

// clampPromptCursor keeps the Attention cursor within the current prompt list
// after a refresh removes rows.
func (m *Model) clampPromptCursor() {
	if m.promptCursor >= len(m.snap.Prompts) {
		m.promptCursor = len(m.snap.Prompts) - 1
	}
	if m.promptCursor < 0 {
		m.promptCursor = 0
	}
}

// clampTrainCursor keeps the Trains cursor within the current session list.
func (m *Model) clampTrainCursor() {
	if m.trainCursor >= len(m.snap.Trains) {
		m.trainCursor = len(m.snap.Trains) - 1
	}
	if m.trainCursor < 0 {
		m.trainCursor = 0
	}
}

func loadSnapshot(deps Deps) tea.Cmd {
	return func() tea.Msg {
		snap, err := deps.Load()
		return snapshotMsg{snap: snap, err: err, at: time.Now()}
	}
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
