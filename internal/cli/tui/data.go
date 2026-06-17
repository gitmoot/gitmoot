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
	JobRows        []JobRow
	Activity       []ActivityRoot
	Config         ConfigView
}

// ActivityRoot is one live delegation tree on the Activity page: a root
// coordinator job that has queued/running work somewhere in its tree, its direct
// delegation children (which agent is doing what, and their state), the
// coordinator continuation job, and a progress summary.
type ActivityRoot struct {
	JobID             string
	Agent             string
	Action            string
	State             string
	Repo              string
	UpdatedAt         string
	Children          []JobChild
	ContinuationID    string
	ContinuationState string
	Total             int // direct delegation children
	Running           int // children actively running
	Queued            int // children waiting to start
	Blocked           int
	Done              int // terminal children (succeeded/failed/cancelled)
}

// Daemon mirrors cli.dashboardDaemon.
type Daemon struct {
	Running bool
	PID     int
	LogFile string

	// Persisted detail from daemon.json, shown on the Health page. Populated
	// from cheap file reads on each snapshot build (never serialized — the
	// --json output uses the cli dashboardDaemon, so these stay byte-stable).
	Flags     []string // start flags (workers/poll/watch…)
	WorkDir   string
	StartedAt string
	LogErrors []string // tail of recent error-ish lines from the daemon log
}

// ConfigView is the parsed config the Config page renders.
type ConfigView struct {
	Path     string
	Sections []ConfigSection
}

// ConfigSection is one titled group of key/value rows on the Config page.
type ConfigSection struct {
	Title string
	Rows  [][]string // each row is {key, value}; for tables, the first row is a header

	// Editable lists the inline-editable scalar fields in this section, in the
	// order they should be cycled. Sections without editable fields (paths,
	// daemon) omit it; structural edits stay in $EDITOR.
	Editable []ConfigField
}

// ConfigKind classifies how an inline config edit is validated.
type ConfigKind int

const (
	ConfigText       ConfigKind = iota // free text (e.g. owner/repo, checked by Validate)
	ConfigInt                          // integer ≥ 1
	ConfigDuration                     // Go duration string (10m, 45m)
	ConfigStringList                   // bracketed list literal, e.g. ["ask", "review"]
)

// ConfigField is one inline-editable scalar on the Config page.
type ConfigField struct {
	Label   string     // human label, e.g. "planner · max_background"
	KeyPath []string   // full dotted path for the writer, e.g. {agents, planner, max_background}
	Kind    ConfigKind //
	Value   string     // current value (prefilled in the editor)
	// Allowed, when non-empty, is the closed set of permitted tokens. For
	// ConfigText the whole value must be one of them; for ConfigStringList every
	// list item must be. Validated inline so a bad value re-asks in the overlay
	// instead of writing then reverting.
	Allowed []string
}

// ConfigEditedMsg is delivered when the external editor (Deps.EditConfig) exits.
type ConfigEditedMsg struct {
	Err error
}

// AgentPromptEditedMsg is delivered when the external editor (Deps.EditAgentPrompt)
// exits, carrying the saved prompt content (or the launch/read error).
type AgentPromptEditedMsg struct {
	Content string
	Err     error
}

// configWriteMsg carries the outcome of an inline Deps.SetConfigScalar write.
type configWriteMsg struct {
	err error
}

// HealthCheck is one environment/runtime diagnostic for the Health page.
type HealthCheck struct {
	Name     string
	Status   string // "ok", "warn", or "fail"
	Detail   string
	Required bool
}

// Repo mirrors cli.dashboardRepo.
type Repo struct {
	Name    string
	Enabled bool
}

// Agent mirrors cli.dashboardAgent.
type Agent struct {
	Name       string
	Runtime    string
	Role       string
	Health     string
	TemplateID string
}

// Session mirrors cli.dashboardSession. Type/Role/Template/LastUsed/Expires
// back the interactive session detail view only (never serialized).
type Session struct {
	Name     string
	Runtime  string
	Repo     string
	State    string
	Type     string
	Role     string
	Template string
	LastUsed string
	Expires  string
}

// Jobs mirrors cli.dashboardJobs.
type Jobs struct {
	Total   int
	ByState map[string]int
}

// JobRow is one job the Jobs page can act on. LatestEvent is filled for
// blocked/failed jobs (the "why" shown in the attention list). Repo is filled
// for reportable (blocked/failed/cancelled) jobs so the Attention page can group
// them by repository.
type JobRow struct {
	ID          string
	Agent       string
	Type        string
	State       string
	UpdatedAt   string
	LatestEvent string
	Repo        string
}

// JobDetail is the job's parsed payload, loaded lazily when its detail opens
// (the request/result is only ever shown for one job, so it is not parsed for
// the whole list on every refresh tick).
type JobDetail struct {
	Repo           string
	PullRequest    int
	Request        string // the human instructions that drove the job
	ResultDecision string // gitmoot_result.decision, when the job has settled
	ResultSummary  string // gitmoot_result.summary
	// Children are the delegation child jobs this job spawned (empty for a
	// non-coordinator job). They render as a "delegations" tree in the detail.
	Children []JobChild
	// ContinuationID/ContinuationState describe the coordinator continuation job
	// enqueued once every delegation settled; ContinuationID is empty when none.
	ContinuationID    string
	ContinuationState string
}

// JobChild is one delegated child job in a coordinator's delegation tree, shown
// in the job-detail "delegations" section.
type JobChild struct {
	ID            string
	DelegationID  string
	Agent         string
	Action        string
	State         string
	Deps          []string
	DepsSatisfied bool
}

// BugReportPreview is a redacted issue draft shown before creating a bug report.
type BugReportPreview struct {
	Title       string
	Body        string
	Labels      []string
	Fingerprint string
}

// BugReportCreateResult is the GitHub issue selected by create action.
type BugReportCreateResult struct {
	URL      string
	Existing bool
}

// JobEventView is one entry of a job's event history shown in the detail view.
type JobEventView struct {
	Kind    string
	Message string
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

	// CollapseGroupsByDefault folds collapsible repo groups (Attention / Trains)
	// on first show, so the live dashboard opens uncluttered; the user expands
	// what they want with space. Tests leave it false (groups start expanded).
	CollapseGroupsByDefault bool

	// OpenTrain, when set, builds the embedded train-run model for a session;
	// the Trains page pushes it onto the Root stack instead of the inline
	// detail view.
	OpenTrain func(sessionID string) tea.Model

	// Job actions: event history + parsed payload (detail view), retry a
	// failed/blocked job, cancel a queued/running one (cooperative — the
	// daemon settles it).
	JobEvents func(id string) ([]JobEventView, error)
	JobDetail func(id string) (JobDetail, error)
	RetryJob  func(id string) error
	CancelJob func(id string) error
	// BugReportPreview builds the redacted report preview. CreateBugReport posts
	// the exact loaded preview draft and returns the selected issue URL.
	BugReportPreview func(id string) (BugReportPreview, error)
	CreateBugReport  func(id string, preview BugReportPreview) (BugReportCreateResult, error)

	// StartDaemon starts the background daemon when the attention list shows it
	// stopped.
	StartDaemon func() error

	// Train session actions. StopTrain abandons a live session's current run
	// with a reason. DeleteTrain removes a terminal session and its history,
	// returning the GitHub repos gitmoot recorded as created for it (still
	// pending cleanup). DeleteTrainRepo deletes one such repo and its record.
	StopTrain       func(id, reason string) error
	DeleteTrain     func(id string) ([]string, error)
	DeleteTrainRepo func(repo string) error

	// Agent actions. TemplateVersions lazily loads a template's version history
	// for the agent detail view. OpenAgentCreate builds the create-agent form
	// pushed onto the Root stack; the dashboard then runs CreateAgent with the
	// collected answers. DeleteAgent refuses while jobs reference the agent.
	// RevertTemplate makes a superseded template version current again.
	TemplateVersions func(templateID string) ([]TemplateVersion, error)
	// TemplateVersionContent loads a specific version's prompt content for the
	// agent-detail preview pager.
	TemplateVersionContent func(versionID string) (string, error)
	OpenAgentCreate        func() (tea.Model, error)
	CreateAgent            func(name, runtime, template string) error
	DeleteAgent            func(name string) error
	RevertTemplate         func(templateID, versionID string) error
	// SetAgentRuntime switches a registered agent's runtime (codex/claude/kimi),
	// preserving its role/capabilities/repos and clearing the warm session.
	SetAgentRuntime func(name, runtime string) error

	// StopSession removes a runtime session (warm agent_instance) by name,
	// refusing one that is mid-job. Wired to store.StopAgentInstance.
	StopSession func(name string) error

	// EditAgentPrompt opens $EDITOR seeded from the given base template (empty =
	// a minimal scaffold) and returns a command whose completion delivers an
	// AgentPromptEditedMsg with the saved content (it is a tea.ExecProcess that
	// suspends the program for the editor). CreateAgentWithPrompt then creates a
	// template from that content and registers the agent against it.
	EditAgentPrompt       func(seedTemplateID string) tea.Cmd
	CreateAgentWithPrompt func(name, runtime, content string) error

	// Optimize an agent: OpenAgentOptimize builds the pre-filled training form
	// for the agent's template; StartOptimize scaffolds and starts the train
	// session from the collected answers and returns its id, which the
	// dashboard opens via OpenTrain.
	OpenAgentOptimize func(agent Agent) (tea.Model, error)
	StartOptimize     func(templateID string, values map[string]string) (string, error)

	// HealthChecks runs the environment/runtime diagnostics for the Health
	// page. It shells out (gh/codex/claude version calls), so it is dispatched
	// lazily on first open and on r, never on the refresh tick.
	HealthChecks func() ([]HealthCheck, error)

	// EditConfig opens the config file in $EDITOR and returns a command whose
	// completion delivers a ConfigEditedMsg (it is a tea.ExecProcess, which
	// suspends the program for the editor). ValidateConfig re-parses the file
	// and returns human-readable problems (empty when valid).
	EditConfig     func() tea.Cmd
	ValidateConfig func() []string

	// SetConfigScalar writes one scalar config field (comment-preserving) and
	// returns an error on an invalid value or a write that fails to re-parse.
	// The kind tells the writer how to type the TOML value (int vs string),
	// so the field's classification is the single source of truth.
	SetConfigScalar func(keyPath []string, value string, kind ConfigKind) error
}

// TemplateVersion is one row of a template's version history.
type TemplateVersion struct {
	ID      string
	Number  int
	State   string // pending | current | superseded | rejected
	Name    string
	Created string
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

// jobEventsMsg carries a job's event history for the detail view.
type jobEventsMsg struct {
	id     string
	events []JobEventView
	err    error
}

// jobDetailMsg carries a job's parsed payload for the detail view.
type jobDetailMsg struct {
	id     string
	detail JobDetail
	err    error
}

// bugReportPreviewMsg carries a redacted report draft for the preview overlay.
type bugReportPreviewMsg struct {
	id      string
	preview BugReportPreview
	err     error
}

// bugReportCreateMsg carries the result of creating a bug report issue.
type bugReportCreateMsg struct {
	id       string
	url      string
	existing bool
	err      error
}

// sessionActionMsg carries the outcome of a Deps.StopSession call.
type sessionActionMsg struct {
	err error
}

// jobActionMsg carries the outcome of a retry/cancel action.
type jobActionMsg struct {
	verb string // "retry" or "cancel"
	id   string
	err  error
}

// daemonStartMsg carries the outcome of a daemon start, separate from
// jobActionMsg so it cannot close or pollute an open job confirm.
type daemonStartMsg struct {
	err error
}

// trainStopMsg carries the outcome of a Deps.StopTrain call.
type trainStopMsg struct {
	err error
}

// trainDeleteMsg carries the outcome of a Deps.DeleteTrain call; repos are the
// recorded gitmoot-created repos now eligible for cleanup.
type trainDeleteMsg struct {
	repos []string
	err   error
}

// trainRepoCleanupMsg carries the outcome of a cleanup pass: the repos that
// failed (so a retry only replays those) and their errors. Both empty on full
// success.
type trainRepoCleanupMsg struct {
	failed []string
	errs   []string
}

// agentVersionsMsg carries a template's version history for the agent detail.
type agentVersionsMsg struct {
	templateID string
	versions   []TemplateVersion
	err        error
}

// versionContentMsg carries a template version's content for the preview pager.
type versionContentMsg struct {
	versionID string
	content   string
	err       error
}

// agentActionMsg carries the outcome of an agent mutation.
type agentActionMsg struct {
	verb string // "create", "delete", "revert"
	err  error
}

// agentFormResultMsg is delivered to the dashboard when the pushed
// create-agent form pops (its Done callback is wired in NewAgentCreateForm).
type agentFormResultMsg struct {
	result Result
}

// agentOptimizeFormResultMsg is delivered when the pushed optimize form pops.
// It carries the template the form was opened for, so a cursor move between
// opening and completing the form cannot retarget the session.
type agentOptimizeFormResultMsg struct {
	templateID string
	result     Result
}

// healthChecksMsg carries the result of a Deps.HealthChecks dispatch.
type healthChecksMsg struct {
	checks []HealthCheck
	err    error
}

// optimizeStartedMsg carries the outcome of Deps.StartOptimize.
type optimizeStartedMsg struct {
	sessionID string
	err       error
}
