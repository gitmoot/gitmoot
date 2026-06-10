package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TrainRunSnapshot is the TUI-facing view of one train session's live state,
// adapted by the cli from the skillopt status snapshot.
type TrainRunSnapshot struct {
	SessionID         string
	IterationID       string
	Template          string // "<id>@<version>"
	ReviewRepo        string
	WorkspaceRepo     string
	Phase             string // lock-aware stable status phase
	NextAction        string
	IssueURL          string // review issue (or candidate review issue) URL
	CandidateVersion  string
	NoCandidateReason string
	FeedbackCount     int
	ReviewItems       int
	GeneratedOptions  int
	JobsRunning       int
	JobsSucceeded     int
	JobsFailed        int
	ETA               string
	Elapsed           string
	Terminal          bool
}

// TrainRunDeps are the data source (and, from Task 4, action callbacks) the cli
// injects. The tui package stays store-free.
type TrainRunDeps struct {
	Interval time.Duration
	Load     func() (TrainRunSnapshot, error)
}

type trainSnapshotMsg struct {
	snap TrainRunSnapshot
	err  error
	at   time.Time
}

type trainTickMsg struct{}

// TrainRunModel renders a single train session as a live phase view.
type TrainRunModel struct {
	deps     TrainRunDeps
	snap     TrainRunSnapshot
	loadedAt time.Time
	loadErr  string
	inFlight bool
	width    int
	height   int
}

// NewTrainRun returns a model ready for tea.NewProgram.
func NewTrainRun(deps TrainRunDeps) TrainRunModel {
	return TrainRunModel{deps: deps, width: 100, height: 30, inFlight: true}
}

func (m TrainRunModel) interval() time.Duration {
	if m.deps.Interval <= 0 {
		return defaultInterval
	}
	return m.deps.Interval
}

// Init issues the first load and arms the refresh tick.
func (m TrainRunModel) Init() tea.Cmd {
	return tea.Batch(loadTrainSnapshot(m.deps), trainTick(m.interval()))
}

func (m TrainRunModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 && msg.Height > 0 {
			m.width, m.height = msg.Width, msg.Height
		}
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "r":
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case trainSnapshotMsg:
		m.inFlight = false
		if msg.err != nil {
			m.loadErr = msg.err.Error()
		} else {
			m.loadErr = ""
			m.snap = msg.snap
			m.loadedAt = msg.at
		}
	case trainTickMsg:
		if cmd := m.queueLoad(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, trainTick(m.interval()))
	}
	return m, tea.Batch(cmds...)
}

func (m *TrainRunModel) queueLoad() tea.Cmd {
	if m.inFlight {
		return nil
	}
	m.inFlight = true
	return loadTrainSnapshot(m.deps)
}

func loadTrainSnapshot(deps TrainRunDeps) tea.Cmd {
	return func() tea.Msg {
		snap, err := deps.Load()
		return trainSnapshotMsg{snap: snap, err: err, at: time.Now()}
	}
}

func trainTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return trainTickMsg{} })
}
