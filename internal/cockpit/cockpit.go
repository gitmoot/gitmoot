// Package cockpit renders one live herdr pane per delegation subagent for
// `gitmoot orchestrate --cockpit`. It talks to the single herdr server over the
// CLI only (no Go import of herdr) and is strictly opt-in: with the cockpit off
// or herdr unreachable, Wrap returns the inner adapter untouched and
// orchestration is byte-identical to today. Every herdr call is best-effort and
// timeout-bounded — a herdr failure never fails or stalls the underlying job
// (fail-open on work, fail-closed on the pane).
package cockpit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

const (
	// herdrSource is the report-agent/report-metadata --source tag the spike
	// verified for gitmoot-owned panes.
	herdrSource = "custom:gitmoot"

	// PaneKeyModeJob keys one pane per delegation job (the only P0 mode).
	PaneKeyModeJob = "job"
	// PaneKeyModeSeat keys one pane per logical seat (root, agent); reserved
	// for P2 (Task 7). Treated as job mode in P0.
	PaneKeyModeSeat = "seat"

	// agentStateWorking / agentStateIdle are the report-agent --state values
	// used to bracket a delivery.
	agentStateWorking = "working"
	agentStateIdle    = "idle"

	// herdrCallTimeout bounds every individual herdr CLI call so a hung herdr
	// never stalls a job. Pane-open may be synchronous (only when streaming,
	// P1); status/label/close are async best-effort.
	herdrCallTimeout = 5 * time.Second

	// availableTTL bounds how long a cached herdr-availability result is reused.
	// Wrap is called once per job under the daemon's held runtime-session lock
	// (SetMaxOpenConns(1)); shelling out to `herdr status` on every job would
	// serialize a process spawn onto that hot path. A short TTL keeps the check
	// cheap while still re-probing if herdr is started/stopped mid-run.
	availableTTL = 30 * time.Second
)

// Pane is the cockpit's store-facing record for a single open pane. Its fields
// mirror the frozen internal/db.CockpitPane row 1:1; integration adapts a
// *db.Store to PaneStore over these fields (see the PaneStore doc).
type Pane struct {
	ID          string
	JobID       string
	PaneKey     string
	RootJobID   string
	PaneID      string
	WorkspaceID string
	Source      string
	CreatedAt   string
}

// PaneStore is the narrow persistence surface the cockpit needs. It is the
// frozen contract satisfied by *db.Store (Task 2): the method set and semantics
// match db.Store's CockpitPane CRUD + per-root workspace registry. The cockpit
// package keeps its own Pane record so it builds in isolation; integration
// bridges *db.Store with a trivial shim over identical fields.
type PaneStore interface {
	InsertCockpitPane(ctx context.Context, pane Pane) error
	GetCockpitPaneByJob(ctx context.Context, jobID string) (Pane, error)
	ListCockpitPanesByRoot(ctx context.Context, rootJobID string) ([]Pane, error)
	DeleteCockpitPane(ctx context.Context, id string) error
	DeleteCockpitPaneByJob(ctx context.Context, jobID string) error
	// GetOrCreateWorkspaceForRoot serializes one workspace per root: create is
	// called only on first use for a given root and its workspace id is reused
	// thereafter.
	GetOrCreateWorkspaceForRoot(ctx context.Context, rootJobID string, create func() (workspaceID string, err error)) (string, error)
}

// Options configures a Cockpit. The fields are frozen.
type Options struct {
	// HerdrBin is the herdr binary name or path (default "herdr").
	HerdrBin string
	// MaxPanes caps the number of split panes per cockpit run; jobs beyond the
	// cap are status-only (no pane).
	MaxPanes int
	// PaneKeyMode is "job" (P0) or "seat" (P2).
	PaneKeyMode string
}

// JobMeta describes the delegation job a pane is opened for. The fields are
// frozen; the cockpit derives the gitmoot binary and home internally (they are
// intentionally not part of the contract).
type JobMeta struct {
	JobID     string
	RootJobID string
	Agent     string
	Action    string
	Branch    string
	Worktree  string
	PaneKey   string
	Depth     int
	// LogPath is the per-job log file the daemon tees the child's live
	// stdout/stderr into (P1, Task 6). When set, the pane tails it for live
	// output; when empty the pane falls back to the P0 `gitmoot job watch`
	// command. It is optional and best-effort: an empty LogPath leaves the pane
	// behaving exactly as P0.
	LogPath string
}

// Cockpit opens and tears down herdr panes around delegation deliveries.
type Cockpit struct {
	client herdrClient
	store  PaneStore
	opts   Options

	// gitmootBin / home back the P0 `pane run "<gitmoot> job watch <job>
	// --home <home>"` command. They are resolved once in New rather than
	// threaded through the frozen JobMeta/Options.
	gitmootBin string
	home       string

	logger *slog.Logger

	// availMu guards the memoized availability check. Wrap consults Available
	// once per job on the locked hot path; caching the result for availableTTL
	// avoids a `herdr status` shell-out per delivery while still re-probing
	// periodically so a herdr that starts/stops mid-run is eventually noticed.
	availMu     sync.Mutex
	availCached bool
	availOK     bool
	availAt     time.Time
	// now is the clock used for TTL expiry; overridable in tests.
	now func() time.Time
}

// New builds a Cockpit. A zero HerdrBin defaults to "herdr"; a non-positive
// MaxPanes defaults to a sane cap; an empty PaneKeyMode defaults to job.
func New(opts Options, store PaneStore) *Cockpit {
	if opts.HerdrBin == "" {
		opts.HerdrBin = "herdr"
	}
	if opts.MaxPanes <= 0 {
		opts.MaxPanes = defaultMaxPanes
	}
	if opts.PaneKeyMode == "" {
		opts.PaneKeyMode = PaneKeyModeJob
	}
	bin, err := os.Executable()
	if err != nil {
		bin = "gitmoot"
	}
	return &Cockpit{
		client:     herdrClient{run: newExecRunner(opts.HerdrBin), bin: opts.HerdrBin, lookPath: exec.LookPath},
		store:      store,
		opts:       opts,
		gitmootBin: bin,
		home:       os.Getenv("GITMOOT_HOME"),
		logger:     slog.Default(),
		now:        time.Now,
	}
}

// newWithRunner builds a Cockpit around an injected herdr runner and lookPath.
// It backs the package tests, which drive the full pane lifecycle against a fake
// runner (no real herdr server), and keeps gitmootBin/home deterministic.
func newWithRunner(opts Options, store PaneStore, run runner, lookPath func(string) (string, error)) *Cockpit {
	if opts.HerdrBin == "" {
		opts.HerdrBin = "herdr"
	}
	if opts.MaxPanes <= 0 {
		opts.MaxPanes = defaultMaxPanes
	}
	if opts.PaneKeyMode == "" {
		opts.PaneKeyMode = PaneKeyModeJob
	}
	return &Cockpit{
		client:     herdrClient{run: run, bin: opts.HerdrBin, lookPath: lookPath},
		store:      store,
		opts:       opts,
		gitmootBin: "gitmoot",
		home:       "",
		logger:     slog.Default(),
		now:        time.Now,
	}
}

const defaultMaxPanes = 4

// Available reports whether the cockpit can render panes: the herdr binary is
// on PATH and `herdr status` reports the server running. It is timeout-bounded
// so a hung herdr cannot stall gating, and the result is memoized for
// availableTTL so the daemon's locked hot path does not shell out to `herdr
// status` on every job. Fail-open: any probe error reports unavailable, which
// makes Wrap return the inner adapter untouched.
func (c *Cockpit) Available(ctx context.Context) bool {
	if c == nil {
		return false
	}
	c.availMu.Lock()
	defer c.availMu.Unlock()
	clock := c.now
	if clock == nil {
		clock = time.Now
	}
	if c.availCached && clock().Sub(c.availAt) < availableTTL {
		return c.availOK
	}
	probeCtx, cancel := context.WithTimeout(ctx, herdrCallTimeout)
	defer cancel()
	ok := c.client.available(probeCtx)
	c.availCached = true
	c.availOK = ok
	c.availAt = clock()
	return ok
}

// Wrap returns a DeliveryAdapter that opens a pane (best-effort), runs
// inner.Deliver, and closes the pane on return. When the cockpit is unavailable
// it returns inner untouched so no herdr calls are made and behavior is
// byte-identical to today.
func (c *Cockpit) Wrap(inner workflow.DeliveryAdapter, meta JobMeta) workflow.DeliveryAdapter {
	if c == nil || inner == nil {
		return inner
	}
	if !c.Available(context.Background()) {
		return inner
	}
	return &paneAdapter{cockpit: c, inner: inner, meta: meta}
}

// paneAdapter brackets inner.Deliver with herdr pane open/close. All herdr
// errors are logged and swallowed: the job's result is inner's, unchanged.
type paneAdapter struct {
	cockpit *Cockpit
	inner   workflow.DeliveryAdapter
	meta    JobMeta
}

func (a *paneAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	pane := a.open(ctx)
	if pane != "" {
		defer a.close(pane)
	}
	return a.inner.Deliver(ctx, agent, job)
}

// open resolves the per-root workspace, splits a child pane, labels it, marks
// the agent working, runs the job-watch command, and persists the pane. It
// returns the new pane id, or "" when no pane was opened (over MaxPanes, herdr
// error, or missing root). Every failure is swallowed (fail-closed on the
// pane) so delivery proceeds unwrapped.
func (a *paneAdapter) open(ctx context.Context) string {
	c := a.cockpit
	rootJob := a.meta.RootJobID
	if rootJob == "" {
		rootJob = a.meta.JobID
	}

	// Honor MaxPanes: beyond the cap the job is status-only (no pane).
	if existing, err := c.store.ListCockpitPanesByRoot(ctx, rootJob); err == nil {
		if len(existing) >= c.opts.MaxPanes {
			c.logf("cockpit: max panes reached for root %s; status-only", rootJob)
			return ""
		}
	}

	workspaceID, err := c.store.GetOrCreateWorkspaceForRoot(ctx, rootJob, func() (string, error) {
		callCtx, cancel := context.WithTimeout(ctx, herdrCallTimeout)
		defer cancel()
		wsID, _, createErr := c.client.workspaceCreate(callCtx, a.meta.Worktree, a.workspaceLabel())
		return wsID, createErr
	})
	if err != nil || workspaceID == "" {
		c.logf("cockpit: resolve workspace for root %s: %v", rootJob, err)
		return ""
	}

	// The parent pane for the first split is the workspace's root pane. herdr
	// accepts the workspace id as the split parent target for the root pane.
	splitCtx, cancel := context.WithTimeout(ctx, herdrCallTimeout)
	defer cancel()
	paneID, err := c.client.paneSplit(splitCtx, c.rootPaneFor(workspaceID), a.meta.Worktree)
	if err != nil || paneID == "" {
		c.logf("cockpit: pane split for job %s: %v", a.meta.JobID, err)
		return ""
	}

	source := herdrSource
	shortID := shortJobID(a.meta.JobID)
	agentLabel := "gm-" + shortID

	// Label + status + watch are best-effort; a failure here still yields a
	// usable pane, so we keep it and persist it.
	c.bestEffort(ctx, "rename", func(cctx context.Context) error {
		return c.client.paneRename(cctx, paneID, a.paneLabel())
	})
	c.bestEffort(ctx, "report-agent working", func(cctx context.Context) error {
		return c.client.reportAgent(cctx, paneID, source, agentLabel, agentStateWorking)
	})
	c.bestEffort(ctx, "pane run", func(cctx context.Context) error {
		return c.client.paneRun(cctx, paneID, a.watchCommand())
	})

	record := Pane{
		JobID:       a.meta.JobID,
		PaneKey:     a.paneKey(),
		RootJobID:   rootJob,
		PaneID:      paneID,
		WorkspaceID: workspaceID,
		Source:      source,
	}
	if err := c.store.InsertCockpitPane(ctx, record); err != nil {
		c.logf("cockpit: persist pane for job %s: %v", a.meta.JobID, err)
		// The pane is open and useful even if persistence failed; keep it.
	}
	return paneID
}

// close marks the agent idle, releases it, and closes the pane — all
// best-effort. Workspace close is deferred to root-finalize (P2), not here.
func (a *paneAdapter) close(paneID string) {
	c := a.cockpit
	source := herdrSource
	agentLabel := "gm-" + shortJobID(a.meta.JobID)
	ctx := context.Background()

	c.bestEffort(ctx, "report-agent idle", func(cctx context.Context) error {
		return c.client.reportAgent(cctx, paneID, source, agentLabel, agentStateIdle)
	})
	c.bestEffort(ctx, "release-agent", func(cctx context.Context) error {
		return c.client.releaseAgent(cctx, paneID, source, agentLabel)
	})
	c.bestEffort(ctx, "pane close", func(cctx context.Context) error {
		return c.client.paneClose(cctx, paneID)
	})
	if err := c.store.DeleteCockpitPaneByJob(ctx, a.meta.JobID); err != nil {
		c.logf("cockpit: delete pane record for job %s: %v", a.meta.JobID, err)
	}
}

// paneKey returns the pane's dedup key. In P0 (job mode) it is the job id; the
// caller-supplied PaneKey wins when set (forward-compat with seat mode, P2).
func (a *paneAdapter) paneKey() string {
	if a.meta.PaneKey != "" {
		return a.meta.PaneKey
	}
	return a.meta.JobID
}

// paneLabel is the verified rename label: "<agent> · d<depth> · <branch>".
func (a *paneAdapter) paneLabel() string {
	return fmt.Sprintf("%s · d%d · %s", a.meta.Agent, a.meta.Depth, a.meta.Branch)
}

// workspaceLabel labels the per-root workspace.
func (a *paneAdapter) workspaceLabel() string {
	return fmt.Sprintf("gitmoot · %s", shortJobID(a.workspaceRoot()))
}

func (a *paneAdapter) workspaceRoot() string {
	if a.meta.RootJobID != "" {
		return a.meta.RootJobID
	}
	return a.meta.JobID
}

// watchCommand is the pane payload. In P1 (Task 6), when the daemon tees the
// child's live stdout/stderr into a per-job log (meta.LogPath set), the pane
// tails that file so real output appears live: `tail -n +1 -F <log>` (-n +1
// replays from the start so output written before the pane attached is not lost;
// -F keeps following across the truncate-on-start and any rotation). With no log
// (LogPath empty — herdr unavailable on the wrapping path, or a log that could
// not be created) it falls back to the P0 `gitmoot job watch` command, so
// behavior is unchanged.
func (a *paneAdapter) watchCommand() string {
	if a.meta.LogPath != "" {
		return fmt.Sprintf("tail -n +1 -F %s", shellQuote(a.meta.LogPath))
	}
	cmd := fmt.Sprintf("%s job watch %s", a.cockpit.gitmootBin, a.meta.JobID)
	if a.cockpit.home != "" {
		cmd += fmt.Sprintf(" --home %s", a.cockpit.home)
	}
	return cmd
}

// shellQuote single-quotes s for safe interpolation into the single shell-string
// command the pane runs, so a log path with spaces or shell metacharacters is
// passed as one literal argument. Single quotes preserve everything except a
// single quote, which is closed, escaped, and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// rootPaneFor returns the split-parent target for a workspace. The workspace id
// addresses its root pane for the first split; subsequent splits target the
// root pane too (herdr lays them out under the workspace).
func (c *Cockpit) rootPaneFor(workspaceID string) string {
	return workspaceID
}

// bestEffort runs a timeout-bounded herdr call and swallows any error after
// logging it. Used for every non-load-bearing pane operation.
func (c *Cockpit) bestEffort(ctx context.Context, op string, fn func(context.Context) error) {
	cctx, cancel := context.WithTimeout(ctx, herdrCallTimeout)
	defer cancel()
	if err := fn(cctx); err != nil {
		c.logf("cockpit: %s: %v", op, err)
	}
}

func (c *Cockpit) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Debug(fmt.Sprintf(format, args...))
	}
}
