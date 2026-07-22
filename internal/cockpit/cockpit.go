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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
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
	// GetCockpitPaneByKey returns the live pane for a (workspaceID, paneKey) seat,
	// if one exists. The bool reports found; a not-found row is (Pane{}, false, nil)
	// — never a surfaced sql.ErrNoRows — so the seat-reuse fail-open path can treat
	// "no pane yet" as a clean miss rather than an error. (Task 7, seat mode.)
	GetCockpitPaneByKey(ctx context.Context, workspaceID, paneKey string) (Pane, bool, error)
	ListCockpitPanesByRoot(ctx context.Context, rootJobID string) ([]Pane, error)
	// ListAllCockpitPanes returns every recorded pane across all roots. The
	// reconcile GC uses it to find orphaned rows (pane gone from herdr + owning
	// root terminal). (Task 7, reconcile.)
	ListAllCockpitPanes(ctx context.Context) ([]Pane, error)
	DeleteCockpitPane(ctx context.Context, id string) error
	DeleteCockpitPaneByJob(ctx context.Context, jobID string) error
	// GetOrCreateWorkspaceForRoot serializes one workspace per root: create is
	// called only on first use for a given root and its workspace id is reused
	// thereafter. create returns both the workspace id and its root pane id; the
	// root pane id is persisted and returned on every reuse so subsequent children
	// can split off it (herdr's pane split needs a PANE id, not a workspace id).
	GetOrCreateWorkspaceForRoot(ctx context.Context, rootJobID string, create func() (workspaceID string, rootPaneID string, err error)) (workspaceID string, rootPaneID string, err error)
	// GetWorkspaceForRoot returns the registered workspace id for a root, if one
	// exists. The bool reports found; a not-found root is ("", false, nil) — never a
	// surfaced sql.ErrNoRows — so FinalizeRoot's fail-open path treats "no workspace"
	// as a clean miss. It lets job-mode finalize close the per-root workspace from
	// the registry even after every pane row has been deleted per-Deliver.
	GetWorkspaceForRoot(ctx context.Context, rootJobID string) (workspaceID string, found bool, err error)
	// DeleteWorkspaceForRoot removes the workspace registry row for a root so a
	// second finalize finds nothing and no-ops (idempotent). Deleting a missing row
	// is not an error.
	DeleteWorkspaceForRoot(ctx context.Context, rootJobID string) error
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
	// GraceClose delays the per-Deliver pane teardown in JOB mode so the last
	// output stays readable instead of vanishing the instant a child finishes
	// (Task 8). It is non-blocking: the close fires from a detached goroutine after
	// the grace, so the job never waits on it. A non-positive value defaults to
	// defaultGraceClose; seat mode ignores it (those panes persist until root
	// finalize).
	GraceClose time.Duration
}

// JobMeta describes the delegation job a pane is opened for. The fields are
// frozen; the cockpit derives the gitmoot binary and home internally (they are
// intentionally not part of the contract).
type JobMeta struct {
	JobID     string
	RootJobID string
	Agent     string
	Runtime   string
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

	// removeAll / sleepAfter back the seat-log teardown and grace-close timing;
	// they default to os.RemoveAll / time.AfterFunc and are overridable in tests so
	// the filesystem and timer behavior can be asserted deterministically.
	removeAll  func(string) error
	sleepAfter func(time.Duration, func()) *time.Timer
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
		client:     herdrClient{run: newExecRunner(opts.HerdrBin), runCombined: newExecRunnerCombined(opts.HerdrBin), bin: opts.HerdrBin, lookPath: exec.LookPath},
		store:      store,
		opts:       opts,
		gitmootBin: bin,
		home:       os.Getenv("GITMOOT_HOME"),
		logger:     slog.Default(),
		now:        time.Now,
		removeAll:  os.RemoveAll,
		sleepAfter: time.AfterFunc,
	}
}

const defaultMaxPanes = 4

// defaultGraceClose is the default delay before a JOB-mode pane is torn down on
// Deliver return (Task 8). It keeps the child's last output readable for a beat
// rather than vanishing the instant the job finishes. The close runs detached so
// the job never blocks on the grace.
const defaultGraceClose = 8 * time.Second

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

// AgentPrompt sends a delivery-verified prompt to an existing Herdr pane. It is
// the narrow exported seam used by the organization event-rule evaluator; pane
// lifecycle remains owned by Cockpit's regular Wrap path.
func (c *Cockpit) AgentPrompt(ctx context.Context, pane, prompt, until string) (delivered bool, stalled bool, err error) {
	if c == nil {
		return false, false, fmt.Errorf("cockpit is nil")
	}
	return c.client.agentPrompt(ctx, pane, prompt, until)
}

// ResolvePaneByLabel resolves a herdr pane label to its current pane id. It backs
// the org event-rule role→pane binding when the configured value is a stable
// label rather than a wX:pY id, so a recycled pane is still reached. It is
// best-effort: any herdr error reports "not found" so a wake stays a no-op.
func (c *Cockpit) ResolvePaneByLabel(ctx context.Context, label string) (string, bool) {
	if c == nil {
		return "", false
	}
	pane, ok, err := c.client.resolvePaneByLabel(ctx, label)
	if err != nil {
		return "", false
	}
	return pane, ok
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
		// Seat mode: the pane persists for reuse across the seat's rounds — the
		// delivery only marks the agent idle on return; the pane (and its row,
		// workspace, seat log) is torn down on ROOT FINALIZE, not here (Task 7).
		// Job mode: tear the pane down per delivery as in P0/P1, but after a short
		// grace so the last output stays readable (Task 8).
		if a.cockpit.seatMode() {
			defer a.markIdle(pane)
		} else {
			defer a.cockpit.graceClose(pane, a.meta.JobID)
		}
	}
	return a.inner.Deliver(ctx, agent, job)
}

// open resolves the per-root workspace and either reuses the seat's existing pane
// (seat mode) or splits a fresh child pane (job mode / first seat open), labels
// it, marks the agent working, runs the watch command, and persists the pane. It
// returns the pane id, or "" when no pane was opened (over MaxPanes, herdr error,
// or missing root). Every failure is swallowed (fail-closed on the pane) so
// delivery proceeds unwrapped.
func (a *paneAdapter) open(ctx context.Context) string {
	c := a.cockpit
	rootJob := a.meta.RootJobID
	if rootJob == "" {
		rootJob = a.meta.JobID
	}

	source := herdrSource
	agentLabel := "gm-" + shortJobID(a.meta.JobID)

	// List the root's panes once: seat mode uses it to find a seat to reuse, and
	// both modes use it for the MaxPanes cap. Doing this BEFORE resolving the
	// workspace keeps job mode byte-identical to P0/P1 (the cap is checked before
	// any `workspace create`).
	existing, listErr := c.store.ListCockpitPanesByRoot(ctx, rootJob)
	if listErr != nil {
		c.logf("cockpit: list panes for root %s: %v", rootJob, listErr)
	}

	// Seat mode: reuse an existing live pane for this seat (pane_key) instead of
	// splitting a new one. The same agent across delegation rounds shares one pane
	// that tails one accumulating seat log, so there is no second split and no
	// second row. A reuse is NOT a new pane, so it bypasses the MaxPanes cap (the
	// cap bounds distinct seats, enforced on the split path below).
	if c.seatMode() {
		for _, p := range existing {
			if p.PaneKey == a.paneKey() && p.PaneID != "" {
				c.bestEffort(ctx, "report-agent working", func(cctx context.Context) error {
					return c.client.reportAgent(cctx, p.PaneID, source, agentLabel, agentStateWorking)
				})
				// Ensure the seat's pane is tailing its accumulating log. The seat log
				// is the same file across rounds (O_APPEND, daemon-managed), so
				// re-running the tail is idempotent and survives a pane that was
				// started before the log existed.
				c.bestEffort(ctx, "pane run", func(cctx context.Context) error {
					return c.client.paneRun(cctx, p.PaneID, a.watchCommand())
				})
				return p.PaneID
			}
		}
	}

	// Honor MaxPanes: a fresh split beyond the cap is status-only (no pane).
	if len(existing) >= c.opts.MaxPanes {
		c.logf("cockpit: max panes reached for root %s; status-only", rootJob)
		return ""
	}

	workspaceID, rootPaneID, err := c.store.GetOrCreateWorkspaceForRoot(ctx, rootJob, func() (string, string, error) {
		callCtx, cancel := context.WithTimeout(ctx, herdrCallTimeout)
		defer cancel()
		wsID, rootPane, createErr := c.client.workspaceCreate(callCtx, a.meta.Worktree, a.workspaceLabel())
		return wsID, rootPane, createErr
	})
	if err != nil || workspaceID == "" {
		c.logf("cockpit: resolve workspace for root %s: %v", rootJob, err)
		return ""
	}

	// The parent of every split is the workspace's ROOT PANE. herdr's `pane split`
	// requires a pane id as the parent target — passing the workspace id fails with
	// pane_not_found — so we split off the root pane captured at workspace create
	// (persisted in the registry and returned on reuse). rootPaneFor is a defensive
	// fallback for legacy registry rows whose root pane id predates this capture.
	splitCtx, cancel := context.WithTimeout(ctx, herdrCallTimeout)
	defer cancel()
	splitParent := rootPaneID
	if splitParent == "" {
		splitParent = c.rootPaneFor(workspaceID)
	}
	paneID, err := c.client.paneSplit(splitCtx, splitParent, a.meta.Worktree)
	if err != nil || paneID == "" {
		c.logf("cockpit: pane split for job %s: %v", a.meta.JobID, err)
		return ""
	}

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
		// A UNIQUE(workspace_id, pane_key) violation means a concurrent same-seat
		// open already persisted the seat's pane (the get-or-create convergence the
		// frozen contract guarantees). In seat mode that is the expected race: the
		// pane we just split is the redundant loser, so close it and reuse the
		// winner's pane so the seat stays a single pane. In job mode the key is the
		// (unique) job id, so this only fires on a genuine re-run and is harmless.
		c.logf("cockpit: persist pane for job %s: %v", a.meta.JobID, err)
		if c.seatMode() {
			if winner, found, lookErr := c.store.GetCockpitPaneByKey(ctx, workspaceID, a.paneKey()); lookErr == nil && found && winner.PaneID != "" && winner.PaneID != paneID {
				c.bestEffort(ctx, "pane close", func(cctx context.Context) error {
					return c.client.paneClose(cctx, paneID)
				})
				return winner.PaneID
			}
		}
		// The pane is open and useful even if persistence failed; keep it.
	}
	return paneID
}

// markIdle reports the agent idle on a seat pane without closing it (seat mode):
// the pane persists for reuse and is torn down only on root finalize. It is the
// seat-mode analogue of close()'s first step.
func (a *paneAdapter) markIdle(paneID string) {
	c := a.cockpit
	c.bestEffort(context.Background(), "report-agent idle", func(cctx context.Context) error {
		return c.client.reportAgent(cctx, paneID, herdrSource, "gm-"+shortJobID(a.meta.JobID), agentStateIdle)
	})
}

// graceClose schedules a JOB-mode pane teardown after the configured grace so the
// child's last output stays readable, then marks idle, releases, closes, and
// deletes the row — all best-effort. The agent is marked idle immediately (so a
// finished job never reads as "working" during the grace) and the actual close
// fires from a detached timer, so the job never blocks on the grace window. The
// per-job log only backs the live tail and is the daemon's to remove.
func (c *Cockpit) graceClose(paneID, jobID string) {
	// Mark idle now: a finished child must not read as "working" while its pane
	// lingers during the grace window.
	c.bestEffort(context.Background(), "report-agent idle", func(cctx context.Context) error {
		return c.client.reportAgent(cctx, paneID, herdrSource, "gm-"+shortJobID(jobID), agentStateIdle)
	})
	grace := c.opts.GraceClose
	if grace <= 0 {
		grace = defaultGraceClose
	}
	after := c.sleepAfter
	if after == nil {
		after = time.AfterFunc
	}
	after(grace, func() { c.teardownJobPane(paneID, jobID) })
}

// teardownJobPane releases the agent, closes the pane, and deletes its row by job
// id — the JOB-mode teardown body, run after the grace. All steps best-effort.
func (c *Cockpit) teardownJobPane(paneID, jobID string) {
	ctx := context.Background()
	c.bestEffort(ctx, "release-agent", func(cctx context.Context) error {
		return c.client.releaseAgent(cctx, paneID, herdrSource, "gm-"+shortJobID(jobID))
	})
	c.bestEffort(ctx, "pane close", func(cctx context.Context) error {
		return c.client.paneClose(cctx, paneID)
	})
	if err := c.store.DeleteCockpitPaneByJob(ctx, jobID); err != nil {
		c.logf("cockpit: delete pane record for job %s: %v", jobID, err)
	}
}

// seatMode reports whether the cockpit keys panes by logical seat (Task 7) rather
// than by job (P0). It centralizes the mode check so the open/Deliver/finalize
// paths stay in lockstep.
func (c *Cockpit) seatMode() bool {
	return c != nil && c.opts.PaneKeyMode == PaneKeyModeSeat
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

// watchCommand is the pane payload. When a cockpit tee log is available, the
// pane runs the human-readable transcript renderer. An external tail fallback
// covers fatal renderer exits and panics. Without a log, the original event
// watcher remains the best-effort fallback.
func (a *paneAdapter) watchCommand() string {
	if a.meta.LogPath != "" {
		path := shellQuote(a.meta.LogPath)
		cmd := fmt.Sprintf("%s job watch %s --transcript --log-path %s --runtime %s", a.cockpit.gitmootBin, shellQuote(a.meta.JobID), path, shellQuote(a.meta.Runtime))
		if a.cockpit.home != "" {
			cmd += fmt.Sprintf(" --home %s", shellQuote(a.cockpit.home))
		}
		return cmd + fmt.Sprintf(" || exec tail -n +1 -F %s", path)
	}
	cmd := fmt.Sprintf("%s job watch %s", a.cockpit.gitmootBin, shellQuote(a.meta.JobID))
	if a.cockpit.home != "" {
		cmd += fmt.Sprintf(" --home %s", shellQuote(a.cockpit.home))
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

// rootPaneFor derives a best-effort split-parent for a workspace whose root pane
// id was not captured at create time (legacy registry rows predating that
// capture). herdr names a workspace's root pane "<workspaceID>:p1", so we target
// that; the workspace id alone is NOT a valid split parent (pane_not_found).
// Freshly created workspaces pass the real root pane id from workspaceCreate and
// never reach this fallback.
func (c *Cockpit) rootPaneFor(workspaceID string) string {
	if workspaceID == "" {
		return ""
	}
	return workspaceID + ":p1"
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

// FinalizeRoot tears down every pane opened under one orchestration root when its
// coordination tree terminates (Task 7, seat mode): it closes each pane (and its
// workspace), deletes the rows, and removes the root's seat-log directory. It is
// the seat-mode teardown — job-mode panes already close per-Deliver — and is
// idempotent and entirely best-effort: a finalize on a root with no panes (job
// mode, or already finalized) is a cheap no-op, and a herdr/store failure on one
// pane never blocks finalizing the rest. The daemon calls it once when the root's
// tree reaches a terminal state.
func (c *Cockpit) FinalizeRoot(ctx context.Context, rootJobID string) {
	if c == nil {
		return
	}
	rootJobID = strings.TrimSpace(rootJobID)
	if rootJobID == "" {
		return
	}
	panes, err := c.store.ListCockpitPanesByRoot(ctx, rootJobID)
	if err != nil {
		c.logf("cockpit: finalize root %s list panes: %v", rootJobID, err)
		return
	}
	workspaces := map[string]struct{}{}
	for _, pane := range panes {
		if pane.PaneID != "" {
			c.bestEffort(ctx, "report-agent idle", func(cctx context.Context) error {
				return c.client.reportAgent(cctx, pane.PaneID, herdrSource, "gm-"+shortJobID(pane.JobID), agentStateIdle)
			})
			c.bestEffort(ctx, "release-agent", func(cctx context.Context) error {
				return c.client.releaseAgent(cctx, pane.PaneID, herdrSource, "gm-"+shortJobID(pane.JobID))
			})
			c.bestEffort(ctx, "pane close", func(cctx context.Context) error {
				return c.client.paneClose(cctx, pane.PaneID)
			})
		}
		if pane.WorkspaceID != "" {
			workspaces[pane.WorkspaceID] = struct{}{}
		}
		if pane.ID != "" {
			if err := c.store.DeleteCockpitPane(ctx, pane.ID); err != nil {
				c.logf("cockpit: finalize root %s delete pane %s: %v", rootJobID, pane.ID, err)
			}
		} else if pane.JobID != "" {
			if err := c.store.DeleteCockpitPaneByJob(ctx, pane.JobID); err != nil {
				c.logf("cockpit: finalize root %s delete pane for job %s: %v", rootJobID, pane.JobID, err)
			}
		}
	}
	// Also close the per-root workspace from the registry, even when no pane rows
	// remain. In JOB mode the pane rows are deleted per-Deliver, so by the time the
	// tree is terminal ListCockpitPanesByRoot is empty and the pane-row loop above
	// closes nothing — the cockpit_workspaces registry (populated by
	// GetOrCreateWorkspaceForRoot) is the only remaining handle on the workspace.
	// Dedup against the pane-row workspaces so a workspace is never closed twice.
	if registryWS, found, wsErr := c.store.GetWorkspaceForRoot(ctx, rootJobID); wsErr != nil {
		c.logf("cockpit: finalize root %s get workspace: %v", rootJobID, wsErr)
	} else if found && registryWS != "" {
		// The set is keyed by workspace id, so adding one already closed via a pane
		// row is a no-op — a workspace is never closed twice.
		workspaces[registryWS] = struct{}{}
	}
	for workspaceID := range workspaces {
		c.bestEffort(ctx, "workspace close", func(cctx context.Context) error {
			return c.client.workspaceClose(cctx, workspaceID)
		})
	}
	// Drop the workspace registry row so a second finalize finds nothing and
	// no-ops (idempotent). Best-effort: a delete failure never blocks finalizing.
	if err := c.store.DeleteWorkspaceForRoot(ctx, rootJobID); err != nil {
		c.logf("cockpit: finalize root %s delete workspace: %v", rootJobID, err)
	}
	// Remove the root's seat-log directory: the accumulating per-seat logs only
	// backed the live tails, which are now closed, so they persist no longer than
	// the root's life.
	if dir := c.seatLogRootDir(rootJobID); dir != "" {
		remove := c.removeAll
		if remove == nil {
			remove = os.RemoveAll
		}
		if err := remove(dir); err != nil {
			c.logf("cockpit: finalize root %s remove seat logs %s: %v", rootJobID, dir, err)
		}
	}
}

// FocusRoot surfaces the orchestration root's workspace (Task 8, coordinator
// reconvene): on the coordinator's terminal continuation the daemon calls this so
// the reconvene view — where the coordinator pane already shows the synthesized
// verdict, since its continuation shares the coordinator seat — is brought
// forward. It uses `herdr workspace focus <workspace_id>` (herdr has no
// focus-pane-by-id verb, only the directional `pane focus`, so workspace focus is
// the closest supported reconvene gesture). Best-effort: any failure (no live
// workspace, herdr down) is swallowed.
func (c *Cockpit) FocusRoot(ctx context.Context, rootJobID string) {
	if c == nil {
		return
	}
	rootJobID = strings.TrimSpace(rootJobID)
	if rootJobID == "" {
		return
	}
	panes, err := c.store.ListCockpitPanesByRoot(ctx, rootJobID)
	if err != nil || len(panes) == 0 {
		if err != nil {
			c.logf("cockpit: focus root %s list panes: %v", rootJobID, err)
		}
		return
	}
	workspaceID := ""
	for _, pane := range panes {
		if pane.WorkspaceID != "" {
			workspaceID = pane.WorkspaceID
			break
		}
	}
	if workspaceID == "" {
		return
	}
	c.bestEffort(ctx, "workspace focus", func(cctx context.Context) error {
		return c.client.workspaceFocus(cctx, workspaceID)
	})
}

// Reconcile is the low-frequency GC sweep (Task 7): it drops cockpit_pane rows
// whose herdr pane is gone AND whose owning root job is terminal, so orphaned
// rows (a daemon crash before teardown, a pane the user closed) do not accumulate.
// It pairs with report-metadata --ttl-ms self-expiry on the herdr side. It is
// cheap and entirely best-effort — every error is swallowed and the sweep simply
// skips that row. rootTerminal reports whether a root's coordination tree has
// fully terminated (the daemon supplies it from job state); a nil rootTerminal
// treats every root as non-terminal, so nothing is dropped.
func (c *Cockpit) Reconcile(ctx context.Context, rootTerminal func(rootJobID string) bool) {
	if c == nil || rootTerminal == nil {
		return
	}
	panes, err := c.store.ListAllCockpitPanes(ctx)
	if err != nil {
		c.logf("cockpit: reconcile list panes: %v", err)
		return
	}
	if len(panes) == 0 {
		return
	}
	live, ok := c.livePaneIDs(ctx)
	if !ok {
		// Could not enumerate live panes (herdr down / parse error): do nothing
		// rather than risk dropping rows for panes that are actually alive.
		return
	}
	terminalCache := map[string]bool{}
	for _, pane := range panes {
		if _, alive := live[pane.PaneID]; alive && pane.PaneID != "" {
			continue
		}
		terminal, cached := terminalCache[pane.RootJobID]
		if !cached {
			terminal = rootTerminal(pane.RootJobID)
			terminalCache[pane.RootJobID] = terminal
		}
		if !terminal {
			continue
		}
		if pane.ID != "" {
			if err := c.store.DeleteCockpitPane(ctx, pane.ID); err != nil {
				c.logf("cockpit: reconcile delete pane %s: %v", pane.ID, err)
			}
		} else if pane.JobID != "" {
			if err := c.store.DeleteCockpitPaneByJob(ctx, pane.JobID); err != nil {
				c.logf("cockpit: reconcile delete pane for job %s: %v", pane.JobID, err)
			}
		}
	}
}

// livePaneIDs returns the set of pane ids herdr currently reports (`herdr pane
// list`). The bool is false when the list could not be obtained (herdr down /
// parse error), so the reconcile caller can skip dropping rows rather than risk
// removing live panes.
func (c *Cockpit) livePaneIDs(ctx context.Context) (map[string]struct{}, bool) {
	cctx, cancel := context.WithTimeout(ctx, herdrCallTimeout)
	defer cancel()
	ids, err := c.client.paneList(cctx)
	if err != nil {
		c.logf("cockpit: reconcile pane list: %v", err)
		return nil, false
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set, true
}

// SeatLogPath returns the stable per-seat append-log path for a (root, paneKey)
// seat: <home>/logs/seats/<rootShort>/<seatSlug>.log. In seat mode the daemon
// opens this O_APPEND (not per-job O_TRUNC) so the seat's one pane tails one file
// that accumulates the seat's output across delegation rounds — no tail
// re-pointing needed. It returns "" when home is unset so the caller falls back
// to the P0 pane. paneKey is slugified so an agent name with path-unsafe
// characters cannot escape the seat-log directory.
func (c *Cockpit) SeatLogPath(rootJobID, paneKey string) string {
	dir := c.seatLogRootDir(rootJobID)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, seatSlug(paneKey)+".log")
}

// seatLogRootDir is the per-root seat-log directory removed on finalize:
// <home>/logs/seats/<rootShort>. It returns "" when home is unset.
func (c *Cockpit) seatLogRootDir(rootJobID string) string {
	if c == nil || c.home == "" {
		return ""
	}
	return filepath.Join(c.home, "logs", "seats", shortJobID(rootJobID))
}

// SafeLogName maps a job id to a flat, filesystem-safe filename component for the
// per-job log. Delegation/continuation job ids contain '/' (e.g.
// "root/delegation/haiku-ocean", ".../continuation"), which would otherwise nest
// the per-job log into directories that are never created and fail os.Create →
// the live tail silently falls back to the P0 pane. Slugifying to a single
// path-safe token keeps it one file in <home>/logs/jobs/. It reuses seatSlug so
// the per-job and per-seat sanitizers stay in lockstep.
func SafeLogName(jobID string) string {
	return transcript.LegacyLogName(jobID)
}

// seatSlug maps a pane key (e.g. an agent name) to a filesystem-safe slug for the
// seat-log filename. Any character outside [A-Za-z0-9._-] becomes '_', so a key
// like "owner/repo" or "" cannot traverse out of the seat-log directory.
func seatSlug(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "seat"
	}
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "seat"
	}
	return out
}
