package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
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

// TrainRunActionResult carries the output lines from a short in-process phase
// action (publish review, sync, promote, …).
type TrainRunActionResult struct {
	Lines []string
}

// TrainRunDeps are the data source and action callbacks the cli injects. The tui
// package stays store-free. Continue advances a short (seconds) phase in
// process; SpawnContinue launches the detached child for the long phases
// (generation, optimizer) so quitting the TUI does not kill the run.
type TrainRunDeps struct {
	Interval      time.Duration
	Load          func() (TrainRunSnapshot, error)
	Continue      func() (TrainRunActionResult, error)
	Decide        func(promote bool, candidate, reason string) (TrainRunActionResult, error)
	StartNext     func() (TrainRunActionResult, error)
	SpawnContinue func() (logPath string, err error)
}

type trainRunMode int

const (
	trainModeNormal trainRunMode = iota
	trainModeReject
)

type trainSnapshotMsg struct {
	snap TrainRunSnapshot
	err  error
	at   time.Time
}

type trainTickMsg struct{}

type trainActionMsg struct {
	result TrainRunActionResult
	err    error
}

type trainSpawnMsg struct {
	logPath string
	err     error
}

// TrainRunModel renders a single train session as a live phase view.
type TrainRunModel struct {
	deps     TrainRunDeps
	snap     TrainRunSnapshot
	loadedAt time.Time
	loadErr  string
	inFlight bool
	width    int
	height   int

	mode        trainRunMode
	actionBusy  bool
	actionErr   string
	resultLines []string
	rejectInput textinput.Model
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
		return m.updateKey(msg)
	case trainActionMsg:
		m.actionBusy = false
		if msg.err != nil {
			m.actionErr = msg.err.Error()
		} else {
			m.actionErr = ""
			m.resultLines = msg.result.Lines
			if cmd := m.queueLoad(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case trainSpawnMsg:
		m.actionBusy = false
		if msg.err != nil {
			m.actionErr = msg.err.Error()
		} else {
			m.actionErr = ""
			m.resultLines = []string{"started in the background — watch the phase above (q quits, the run keeps going)"}
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

// updateKey handles all key input: the reject-reason sub-mode first, then the
// global keys, then the per-phase action keys.
func (m TrainRunModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.mode == trainModeReject {
		switch msg.String() {
		case "esc":
			m.mode = trainModeNormal
			m.actionErr = ""
			return m, nil
		case "enter":
			reason := strings.TrimSpace(m.rejectInput.Value())
			if reason == "" {
				m.actionErr = "a reject reason is required"
				return m, nil
			}
			m.mode = trainModeNormal
			m.actionBusy = true
			m.actionErr = ""
			return m, decideCmd(m.deps, false, m.snap.CandidateVersion, reason)
		}
		var cmd tea.Cmd
		m.rejectInput, cmd = m.rejectInput.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "r":
		return m, m.queueLoad()
	}
	if m.actionBusy {
		return m, nil // an action is in flight; ignore until it returns
	}
	switch msg.String() {
	case "enter":
		switch m.snap.Phase {
		case "items_ready", "feedback_synced", "training_package_created":
			m.actionBusy, m.actionErr = true, ""
			return m, spawnCmd(m.deps)
		case "options_generated", "review_published", "candidate_created":
			m.actionBusy, m.actionErr = true, ""
			return m, continueCmd(m.deps)
		}
	case "p":
		if m.snap.Phase == "candidate_review_published" {
			m.actionBusy, m.actionErr = true, ""
			return m, decideCmd(m.deps, true, m.snap.CandidateVersion, "")
		}
	case "x":
		if m.snap.Phase == "candidate_review_published" {
			m.mode = trainModeReject
			m.actionErr = ""
			ti := textinput.New()
			ti.Placeholder = "reason for rejecting"
			m.rejectInput = ti
			return m, m.rejectInput.Focus()
		}
	case "n":
		if m.snap.Terminal {
			m.actionBusy, m.actionErr = true, ""
			return m, startNextCmd(m.deps)
		}
	}
	return m, nil
}

func continueCmd(d TrainRunDeps) tea.Cmd {
	return func() tea.Msg {
		if d.Continue == nil {
			return trainActionMsg{}
		}
		res, err := d.Continue()
		return trainActionMsg{result: res, err: err}
	}
}

func decideCmd(d TrainRunDeps, promote bool, candidate, reason string) tea.Cmd {
	return func() tea.Msg {
		if d.Decide == nil {
			return trainActionMsg{}
		}
		res, err := d.Decide(promote, candidate, reason)
		return trainActionMsg{result: res, err: err}
	}
}

func startNextCmd(d TrainRunDeps) tea.Cmd {
	return func() tea.Msg {
		if d.StartNext == nil {
			return trainActionMsg{}
		}
		res, err := d.StartNext()
		return trainActionMsg{result: res, err: err}
	}
}

func spawnCmd(d TrainRunDeps) tea.Cmd {
	return func() tea.Msg {
		if d.SpawnContinue == nil {
			return trainSpawnMsg{}
		}
		path, err := d.SpawnContinue()
		return trainSpawnMsg{logPath: path, err: err}
	}
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
