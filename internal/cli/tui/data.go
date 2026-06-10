// Package tui is the interactive terminal UI for gitmoot, built on the Charm
// stack (bubbletea/bubbles/lipgloss). It is a thin view layer: the cli package
// injects data and action callbacks through Deps, so this package imports only
// db (for the interactive-prompt record shape) and never touches the store,
// flags, or process state directly. Plain (non-terminal) output stays in the
// zero-dependency internal/cli/style package.
package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

// Snapshot is the TUI-facing view of local gitmoot state, mirroring the cli
// dashboardSnapshot. Prompts carries the full interactive-prompt records (not
// just id/question) so the Attention page can answer them inline.
type Snapshot struct {
	Home           string
	DatabaseExists bool
	Daemon         Daemon
	Repos          []Repo
	Agents         []Agent
	Sessions       []Session
	Jobs           Jobs
	Worktrees      []Worktree
	BranchLocks    []BranchLock
	Trains         []TrainSession
	ResourceLocks  []ResourceLock
	Prompts        []db.InteractivePrompt
}

// Daemon mirrors cli.dashboardDaemon.
type Daemon struct {
	Running bool
	PID     int
	LogFile string
}

// Repo mirrors cli.dashboardRepo.
type Repo struct {
	Name    string
	Enabled bool
}

// Agent mirrors cli.dashboardAgent.
type Agent struct {
	Name    string
	Runtime string
	Role    string
	Health  string
}

// Session mirrors cli.dashboardSession.
type Session struct {
	Name    string
	Runtime string
	Repo    string
	State   string
}

// Jobs mirrors cli.dashboardJobs.
type Jobs struct {
	Total   int
	ByState map[string]int
}

// Worktree mirrors cli.dashboardWorktree.
type Worktree struct {
	Task string
	Repo string
	Path string
}

// BranchLock mirrors cli.dashboardBranchLock.
type BranchLock struct {
	Repo   string
	Branch string
	Owner  string
}

// TrainSession mirrors cli.dashboardTrainSession.
type TrainSession struct {
	ID        string
	Phase     string
	Candidate string
	Repo      string
}

// ResourceLock mirrors cli.dashboardResourceLock.
type ResourceLock struct {
	Key   string
	Owner string
	Stale bool
}

// Deps are the data source and action callbacks the cli package injects.
type Deps struct {
	Load     func() (Snapshot, error)
	Answer   func(id, value string) error
	Dismiss  func(id string) error
	Interval time.Duration

	// OpenTrain, when set, builds the embedded train-run model for a session;
	// the Trains page pushes it onto the Root stack instead of the inline
	// detail view.
	OpenTrain func(sessionID string) tea.Model
}

// snapshotMsg carries the result of a Deps.Load call.
type snapshotMsg struct {
	snap Snapshot
	err  error
	at   time.Time
}

// tickMsg fires on the refresh interval. gen identifies the tick chain it
// belongs to: while a child view covers the dashboard its ticks go unhandled
// and the chain dies, so the pop-nudge starts a NEW generation — and a stale
// pre-push tick that fires after a fast pop must be dropped, not re-armed,
// or chains would accumulate.
type tickMsg struct {
	gen int
}

// refreshNudgeMsg asks a model to refresh once and restart its tick chain
// under a new generation — sent by the Root when a pop resumes a model.
type refreshNudgeMsg struct{}

// answerResultMsg carries the outcome of a Deps.Answer call.
type answerResultMsg struct {
	id  string
	err error
}

// dismissResultMsg carries the outcome of a Deps.Dismiss call.
type dismissResultMsg struct {
	id  string
	err error
}
