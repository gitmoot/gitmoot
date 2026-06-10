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

// TrainRunPlan is shown on the confirm screen when there is no session yet for a
// --config. CreateSession (below) writes it.
type TrainRunPlan struct {
	Name              string
	Template          string // "<id>@<version>"
	ReviewRepo        string
	WorkspaceRepo     string // pre-supplied via --workspace-repo; else collected
	NeedWorkspaceRepo bool
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

	// Plan, when non-nil, opens the confirm screen first (no session yet for the
	// --config). CreateSession writes the session and returns its id; afterwards
	// Load/Continue/… operate on the new session.
	Plan          *TrainRunPlan
	CreateSession func(workspaceRepo string) (sessionID string, err error)

	// TailLog reads the current session's detached-child log from a byte offset,
	// returning any new lines and the next offset. Used to show live progress
	// during the long generate/optimizer phases.
	TailLog func(offset int64) (lines []string, next int64, err error)

	// Embedded marks the model as a child of the Root router: q/esc pop back to
	// the dashboard instead of quitting the program.
	Embedded bool
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

type trainCreatedMsg struct {
	sessionID string
	err       error
}

type trainLogMsg struct {
	lines  []string
	offset int64
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

	// Confirm-screen state (deps.Plan != nil and no session yet).
	confirming bool
	creating   bool
	createErr  string
	wsInput    textinput.Model

	// Log tail (long phases): a monotonic byte offset into the per-session child
	// log (generation and optimizer append to the same file, so the offset is
	// never reset on phase change — only on truncation, handled by the reader),
	// and the last few complete lines to show.
	logOffset int64
	logLines  []string
}

const trainLogTailLines = 8

// isLongTrainPhase reports whether the phase runs in the detached child and so
// has a log worth tailing.
func isLongTrainPhase(phase string) bool {
	switch phase {
	case "generating_options", "generating_options_heartbeat_stale",
		"optimizer_running", "optimizer_heartbeat_stale":
		return true
	default:
		return false
	}
}

// NewTrainRun returns a model ready for tea.NewProgram.
func NewTrainRun(deps TrainRunDeps) TrainRunModel {
	m := TrainRunModel{deps: deps, width: 100, height: 30, inFlight: true}
	if deps.Plan != nil {
		m.confirming = true
		m.inFlight = false
		if deps.Plan.NeedWorkspaceRepo {
			ti := textinput.New()
			ti.Placeholder = "owner/repo"
			m.wsInput = ti
			m.wsInput.Focus() // focus state must be set here (Init's value receiver cannot persist it)
		}
	}
	return m
}

func (m TrainRunModel) interval() time.Duration {
	if m.deps.Interval <= 0 {
		return defaultInterval
	}
	return m.deps.Interval
}

// Init issues the first load and arms the refresh tick — unless a confirm screen
// is pending (no session yet), in which case it waits for [enter] to create one
// (the workspace-repo input, if any, was already focused in NewTrainRun).
func (m TrainRunModel) Init() tea.Cmd {
	if m.confirming {
		if m.deps.Plan != nil && m.deps.Plan.NeedWorkspaceRepo {
			return textinput.Blink
		}
		return nil
	}
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
	case trainCreatedMsg:
		m.creating = false
		if msg.err != nil {
			m.createErr = msg.err.Error()
		} else {
			// Session created — leave the confirm screen and start the live view.
			m.confirming = false
			m.createErr = ""
			m.inFlight = true
			cmds = append(cmds, loadTrainSnapshot(m.deps), trainTick(m.interval()))
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
			// Clear the displayed lines when not in a long phase so stale output
			// does not linger; the byte offset stays monotonic (one per-session
			// log spans generation and optimizer).
			if !isLongTrainPhase(msg.snap.Phase) {
				m.logLines = nil
			}
		}
	case trainLogMsg:
		if len(msg.lines) > 0 {
			m.logLines = append(m.logLines, msg.lines...)
			if len(m.logLines) > trainLogTailLines {
				m.logLines = m.logLines[len(m.logLines)-trainLogTailLines:]
			}
		}
		m.logOffset = msg.offset
	case trainTickMsg:
		if cmd := m.queueLoad(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if isLongTrainPhase(m.snap.Phase) && m.deps.TailLog != nil {
			cmds = append(cmds, tailLogCmd(m.deps, m.logOffset))
		}
		cmds = append(cmds, trainTick(m.interval()))
	}
	return m, tea.Batch(cmds...)
}

func tailLogCmd(d TrainRunDeps, offset int64) tea.Cmd {
	return func() tea.Msg {
		lines, next, err := d.TailLog(offset)
		if err != nil {
			return trainLogMsg{offset: offset} // keep the offset; transient read errors are ignored
		}
		return trainLogMsg{lines: lines, offset: next}
	}
}

// updateKey handles all key input: the reject-reason sub-mode first, then the
// global keys, then the per-phase action keys.
func (m TrainRunModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.confirming {
		switch msg.String() {
		case "esc", "q":
			if m.deps.Embedded {
				return m, Pop()
			}
			return m, tea.Quit
		case "enter":
			if m.creating {
				return m, nil
			}
			ws := m.deps.Plan.WorkspaceRepo
			if m.deps.Plan.NeedWorkspaceRepo {
				ws = strings.TrimSpace(m.wsInput.Value())
				if ws == "" {
					m.createErr = "workspace repo (owner/repo) is required"
					return m, nil
				}
			}
			m.creating = true
			m.createErr = ""
			return m, createSessionCmd(m.deps, ws)
		}
		if m.deps.Plan.NeedWorkspaceRepo && !m.creating {
			var cmd tea.Cmd
			m.wsInput, cmd = m.wsInput.Update(msg)
			return m, cmd
		}
		return m, nil
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
		if m.deps.Embedded {
			return m, Pop()
		}
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

func createSessionCmd(d TrainRunDeps, workspaceRepo string) tea.Cmd {
	return func() tea.Msg {
		if d.CreateSession == nil {
			return trainCreatedMsg{}
		}
		id, err := d.CreateSession(workspaceRepo)
		return trainCreatedMsg{sessionID: id, err: err}
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
