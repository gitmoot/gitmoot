package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/buildinfo"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/presence"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func runDaemonStart(args []string, stdout, stderr io.Writer) int {
	return runDaemonStartWithWorkDir(args, "", stdout, stderr)
}

func runDaemonStartWithWorkDir(args []string, workDir string, stdout, stderr io.Writer) int {
	return runDaemonStartWithWorkDirRestart(args, workDir, false, false, stdout, stderr)
}

// runDaemonStartWithWorkDirRestart is the shared (re)start body. The legacy
// restart parameters remain in the internal signature for existing callers, but
// runtime auth is now loaded independently for every Claude adapter build.
func runDaemonStartWithWorkDirRestart(args []string, workDir string, _ bool, _ bool, stdout, stderr io.Writer) int {
	cfg, code := parseDaemonStartConfig("daemon start", args, stderr)
	if code == daemonHelp {
		return 0
	}
	if code != 0 {
		return code
	}
	resolvedWorkDir, err := daemonWorkDir(workDir)
	if err != nil {
		fmt.Fprintf(stderr, "daemon start: %v\n", err)
		return 1
	}

	paths, err := initializedPaths(cfg.Home)
	if err != nil {
		fmt.Fprintf(stderr, "daemon start: %v\n", err)
		return 1
	}
	state := daemonProcessState(paths)
	pid, stale, err := currentDaemonPID(state)
	if err != nil {
		fmt.Fprintf(stderr, "daemon start: %v\n", err)
		return 1
	}
	if pid > 0 {
		fmt.Fprintf(stderr, "daemon already running with pid %d\n", pid)
		return 1
	}
	if stale {
		writeLine(stdout, "removed stale daemon pid file")
	}
	// #556 aftermath (#597 review): the pidfile check above cannot see a live
	// daemon that flock-holds <home>/daemon.lock WITHOUT being registered (e.g.
	// the corpse of a concurrent-start race clobbered daemon.pid, which the
	// next status check then removed as stale). Spawning against such a holder
	// produces a child that instantly loses the flock and dies with its refusal
	// visible only in daemon.log while this parent printed "daemon started" —
	// a false green. Probe the flock the same way the child will and refuse up
	// front, naming the holder, instead of spawning a doomed child.
	if code := refuseWhenDaemonRunLockHeld("daemon start", cfg.Home, stderr); code != 0 {
		return code
	}
	if cfg.RepoSet {
		if err := preflightDaemonRepoStart(context.Background(), cfg.Home, cfg.Repo, cfg.Poll.String(), resolvedWorkDir); err != nil {
			fmt.Fprintf(stderr, "daemon start: %v\n", err)
			return 1
		}
	}

	if _, err := bootstrapRuntimeAuth(paths.Home, runtimeAuthEnvLookup, func(format string, args ...any) {
		fmt.Fprintf(stderr, "daemon start: %s\n", fmt.Sprintf(format, args...))
	}); err != nil {
		fmt.Fprintf(stderr, "daemon start: warning: could not bootstrap runtime auth: %v\n", err)
	}

	started, err := startDaemonChildFn(cfg.Home, cfg.Poll.String(), cfg.Workers, cfg.WatchSkillOptReviews, cfg.WatchIssues, cfg.Scheduler, cfg.RepoFlag, cfg.Session, state, resolvedWorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "daemon start: %v\n", err)
		return 1
	}
	started = daemonMetaWithCurrentBuild(started)
	if err := writeDaemonState(state, started); err != nil {
		_ = stopDaemonPID(started.PID)
		fmt.Fprintf(stderr, "daemon start: %v\n", err)
		return 1
	}
	writeLine(stdout, "daemon started pid %d", started.PID)
	writeLine(stdout, "log: %s", state.LogFile)
	return 0
}

// parseSchedulerMode maps the --scheduler flag to the worker-pool toggle (#394).
func parseSchedulerMode(mode string) (bool, error) {
	switch strings.TrimSpace(mode) {
	case "", "barrier":
		return false, nil
	case "pool":
		return true, nil
	default:
		return false, fmt.Errorf("invalid --scheduler %q: want \"barrier\" or \"pool\"", mode)
	}
}

// autoSelectScheduler makes `--workers > 1` actually deliver parallelism (#444).
// Requesting multiple workers under the default `barrier` scheduler almost never
// does what the user wants — same-repo jobs serialize on one per-tick wg.Wait()
// and one per-repo checkout lock — so when more than one worker is requested and
// the scheduler was left at its default, auto-select `pool`. An explicit
// `--scheduler barrier` is always honored (explicitScheduler == true), preserving
// the old per-tick behavior for callers who deliberately want it.
//
// It returns the resolved scheduler mode string ("barrier"/"pool") so callers can
// surface it and persist it in the daemon's child args.
func autoSelectScheduler(scheduler string, workers int, explicitScheduler bool) string {
	if explicitScheduler {
		return scheduler
	}
	if workers > 1 && strings.TrimSpace(scheduler) != "pool" {
		return "pool"
	}
	return scheduler
}

func runDaemonRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "GitHub repository as owner/repo: scopes the daemon to a SINGLE repo (polls only that repo's PRs; claims only that repo's queued jobs). Omit to supervise ALL enabled registered repos")
	var session string
	fs.StringVar(&session, "session", "", "scope the daemon worker to a delegation root job id")
	fs.StringVar(&session, "root", "", "alias for --session")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	workers := fs.Int("workers", 1, "worker count")
	dryRun := fs.Bool("dry-run", false, "run without mutating external systems")
	watchSkillOptReviews := fs.Bool("watch-skillopt-reviews", false, "poll watched SkillOpt review issue comments and import valid feedback")
	watchIssues := fs.Bool("watch-issues", false, "poll open issues and route @<agent> ask comments to jobs (#389)")
	scheduler := fs.String("scheduler", "barrier", "queued-job scheduler: barrier (default) or pool (#394 opt-in continuous worker pool)")
	parallel := fs.Int("parallel", 0, "run jobs in parallel: sets --workers N and --scheduler pool together (#444)")
	pprofAddr := fs.String("pprof-addr", "", "off by default; if set (use a loopback addr, e.g. 127.0.0.1:6060) serve net/http/pprof on it for live diagnostics (#1111)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "daemon run does not accept positional arguments")
		return 2
	}
	explicitPoll, explicitWorkers, explicitScheduler, explicitParallel := false, false, false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "poll":
			explicitPoll = true
		case "workers":
			explicitWorkers = true
		case "scheduler":
			explicitScheduler = true
		case "parallel":
			explicitParallel = true
		}
	})
	// --parallel N is intent-level sugar for --workers N + --scheduler pool (#444).
	if explicitParallel {
		if explicitWorkers || explicitScheduler {
			fmt.Fprintln(stderr, "--parallel cannot be combined with --workers or --scheduler")
			return 2
		}
		if *parallel <= 0 {
			fmt.Fprintln(stderr, "--parallel must be positive")
			return 2
		}
		*workers = *parallel
		*scheduler = "pool"
		// --parallel makes BOTH knobs explicit for warm-reload override purposes
		// (#577): the operator named the goal, so a start-time [daemon].workers /
		// scheduler must not silently override the launch flag.
		explicitWorkers = true
		explicitScheduler = true
	}
	if *poll <= 0 {
		fmt.Fprintln(stderr, "poll interval must be positive")
		return 2
	}
	if *workers <= 0 {
		fmt.Fprintln(stderr, "workers must be positive")
		return 2
	}
	// Auto-select pool when multiple workers are requested without an explicit
	// scheduler (#444); explicit --scheduler barrier still forces per-tick.
	*scheduler = autoSelectScheduler(*scheduler, *workers, explicitScheduler)
	usePool, schedErr := parseSchedulerMode(*scheduler)
	if schedErr != nil {
		fmt.Fprintln(stderr, schedErr.Error())
		return 2
	}
	if *repoFlag != "" && *dryRun {
		fmt.Fprintln(stderr, "daemon run --dry-run is only supported without --repo")
		return 2
	}

	// Singleton guard (#550). `daemon run` is the inner worker that `daemon start`
	// execs, but it is ALSO a public command and the ExecStart of `systemd --user`
	// units, so a stray `daemon run` could previously coexist silently with the
	// managed daemon: two daemons polling the same repos and racing on the same
	// WAL'd SQLite store, with the newcomer clobbering daemon.json so the prior one
	// ran untracked for days. `daemon start` already refuses when a live daemon is
	// registered; extend the SAME check to `daemon run` so the second one refuses
	// up front instead of entering its loop. The check must run BEFORE the deferred
	// self-registration so a refused run never touches the existing daemon's state.
	if code := guardDaemonRunSingleton(*home, stderr); code != 0 {
		return code
	}
	// Test seam (#556): deterministically widen the window between the liveness
	// guard above and the flock/self-registration below so the singleton E2E can
	// observe the simultaneous-launch TOCTOU. No-op outside tests (env unset).
	daemonRunWidenGuardWindow()

	// Airtight singleton backstop (#556). The liveness guard above is friendly
	// diagnostics but racy: two `daemon run` launched simultaneously on a clean
	// home can BOTH pass it before either self-registers. flock(LOCK_EX|LOCK_NB)
	// on a dedicated lockfile is atomic in the kernel — exactly one process ever
	// holds it — and the fd stays open for the process lifetime, so the kernel
	// releases the lock on ANY death (SIGKILL included): no stale-lock lockout,
	// no unlink dance. The loser exits with the same "daemon already running"
	// UX as the guard.
	releaseDaemonRunLock, lockCode := acquireDaemonRunLock(*home, stderr)
	if lockCode != 0 {
		return lockCode
	}
	defer releaseDaemonRunLock()

	// Bootstrap the authoritative runtime-auth file for direct systemd launches.
	// Existing files are never overwritten; adapter builds reload the file.
	if paths, err := initializedPaths(*home); err == nil {
		if _, bootstrapErr := bootstrapRuntimeAuth(paths.Home, runtimeAuthEnvLookup, func(format string, args ...any) {
			fmt.Fprintf(stderr, "daemon run: %s\n", fmt.Sprintf(format, args...))
		}); bootstrapErr != nil {
			fmt.Fprintf(stderr, "daemon run: warning: could not bootstrap runtime auth: %v\n", bootstrapErr)
		}
		defer closeModelGatewayHome(paths.Home)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Off-by-default live-profiling listener (#1111). When --pprof-addr is set it
	// serves net/http/pprof on a DEDICATED mux (never DefaultServeMux, so pprof can
	// never leak onto another server) so a running daemon can be profiled without a
	// restart; unset = no listener and byte-identical behavior. Tied to ctx so it
	// shuts down with the daemon.
	if addr := strings.TrimSpace(*pprofAddr); addr != "" {
		stopPprof := startDaemonPprofServer(ctx, addr, stdout)
		defer stopPprof()
	}

	// Warm-reconfigure path (#577): the live supervisor settings (poll, worker-pool
	// size, scheduler mode) are held behind a mutex so a SIGHUP can re-read the
	// [daemon] config section and apply the changes WITHOUT a `daemon restart`. A
	// restart tears down in-flight supervision and re-inherits the launching shell's
	// environment, dropping runtime auth (the #559 disaster); a warm reload never
	// touches the process, its env, or in-flight jobs. CLI flags remain the initial
	// value: a start-time [daemon] key is applied only where the operator did not pass
	// the matching flag (flag = override). SIGHUP is deliberately kept OUT of the
	// SIGINT/SIGTERM shutdown context above so a reload never cancels the daemon.
	live := newDaemonReloadableConfig(*poll, *workers, usePool)
	if reloadPaths, reloadErr := initializedPaths(*home); reloadErr == nil {
		if start, cfgErr := config.LoadDaemonRuntimeConfig(reloadPaths); cfgErr != nil {
			writeLine(stdout, "daemon: [daemon] config load error, using CLI flags: %v", cfgErr)
		} else if applied := live.applyStart(start, explicitPoll, explicitWorkers, explicitScheduler); applied != "" {
			writeLine(stdout, "daemon: applied [daemon] config at start (%s); CLI flags override", applied)
		}
		// Install the GitHub call budget + secondary-rate-limit backoff (#683) so the
		// gh calls gitmoot issues from THIS daemon process (polling, comments, merges,
		// status) share one limiter. It is in-process: foreground gitmoot processes and
		// runtime-subprocess gh calls run outside it.
		configureGitHubLimiter(reloadPaths, stdout)
		installDaemonReloadHandler(ctx, reloadPaths, live, stdout)
	} else {
		writeLine(stdout, "daemon: warm reload disabled (paths unavailable): %v", reloadErr)
	}

	// Self-register so `daemon status` / the dashboard recognize a `daemon run`
	// launched directly (e.g. under `systemd --user`), not only one spawned by
	// `daemon start` (#505 gap 3), and reconcile any phantom "running" runtime
	// sessions a previously-crashed daemon left behind (#505 gap 2). Both are
	// best-effort: a failure here never blocks the daemon from running. The work
	// lives in daemonRunStartupReconcile so the integration — daemon run actually
	// self-registers AND reconciles at startup — is exercised by a test, not only
	// the leaf helpers (a revert of this wiring would otherwise stay green).
	defer daemonRunStartupReconcile(ctx, *home, os.Args, stdout)()

	// #732 chat relay: start a daemon-owned unix-socket relay so a sandboxed moot
	// seat (whose read-only home blocks a direct `gitmoot chat send`) can route
	// send/wait to THIS daemon, which owns the store unsandboxed. It uses a
	// dedicated store bound to the daemon ctx (the per-repo supervisors open their
	// own stores per pass) and is torn down on ctx.Done. Best-effort: a relay start
	// failure never blocks the daemon — a moot simply degrades to the pre-#732
	// parallel-conclusions behavior.
	if relayPaths, perr := initializedPaths(*home); perr != nil {
		writeLine(stdout, "daemon: chat relay disabled (paths unavailable): %v", perr)
	} else if relayStore, serr := db.Open(relayPaths.Database); serr != nil {
		writeLine(stdout, "daemon: chat relay disabled (store unavailable): %v", serr)
	} else {
		relay := newChatRelayServer(relayStore, chatRelaySocketDir(relayPaths.Home), stdout)
		if rerr := relay.Start(ctx); rerr != nil {
			writeLine(stdout, "daemon: chat relay disabled: %v", rerr)
			_ = relayStore.Close()
		} else {
			setActiveChatRelayServer(relay)
			writeLine(stdout, "daemon: chat relay listening at %s", relay.SocketPath())
			go func() {
				<-ctx.Done()
				setActiveChatRelayServer(nil)
				_ = relayStore.Close()
			}()
		}
	}

	if *repoFlag == "" {
		err := runRegisteredRepoSupervisor(ctx, *home, live, *dryRun, *watchSkillOptReviews, *watchIssues, session, stdout)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0
		}
		if err != nil {
			fmt.Fprintf(stderr, "daemon run: %v\n", err)
			return 1
		}
		return 0
	}

	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}

	err = withStore(*home, func(store *db.Store) error {
		repoRecord, err := resolveDaemonStartRepo(ctx, store, repo, ".")
		if err != nil {
			return err
		}
		repoRecord.PollInterval = poll.String()
		if err := store.UpsertRepo(ctx, repoRecord); err != nil {
			return err
		}
		checkout := repoRecord.CheckoutPath
		gh := github.NewClient(checkout)
		resolvedHome := config.PathsForHome(*home).Home
		engine := workflow.Engine{
			Store:                 store,
			RequireWorkflowPolicy: requireWorkflowPolicyResolverRoot(resolvedHome),
			OrgPolicy:             orgPolicyResolverRoot(resolvedHome),
			ProduceCheckDir:       checkout,
			// Resolve [merge_gate] inside daemonMergeGate for every evaluation so a
			// live auto_merge flip both re-arms parked tasks and changes their next
			// gate decision without a daemon restart.
			MergeGate: daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout, Home: resolvedHome},
			// Registry default model/effort fallbacks, home-aware and fail-open — see
			// daemonWorkflowEngine. Empty by default => byte-identical.
			RuntimeDefaultModel:  runtimeDefaultModelResolver(*home),
			RuntimeDefaultEffort: runtimeDefaultEffortResolver(*home),
			// Result-check audit (#526), home-aware and fail-safe to the default warn.
			ResultCheckMode: resultChecksMode(*home),
		}
		// Honor the opt-in [orchestrate] policy (artifact-body inlining + per-root
		// delegation token budget) on this single-repo engine too; fail-safe to the
		// defaults if the policy cannot load.
		defaultJobWorker(store, stdout, *home).applyOrchestratePolicy(&engine)
		// Opt-in risk-tiered adaptive review (#650); off by default so this is a
		// no-op unless the home config enables it.
		applyReviewPolicy(&engine, *home)
		wireReviewRiskSignals(&engine, gh)
		fmt.Fprintf(stdout, "watching %s every %s\n", repo.FullName(), poll.String())
		return runSingleRepoSupervisor(ctx, *home, daemon.Daemon{
			Repo:                   repo,
			PollInterval:           *poll,
			Store:                  store,
			GitHub:                 gh,
			Workflow:               &engine,
			WatchIssues:            *watchIssues,
			EscalationTTL:          resolveEscalationTTL(*home),
			RevertDetectionEnabled: resolveRevertDetectionEnabled(*home),
			AutoMergeEnabled:       autoMergeEnabledResolver(*home),
		}, store, live, session, stdout)
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "daemon run: %v\n", err)
		return 1
	}
	return 0
}

func runDaemonStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "daemon stop does not accept positional arguments")
		return 2
	}
	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "daemon stop: %v\n", err)
		return 1
	}
	state := daemonProcessState(paths)
	pid, stale, err := currentDaemonPID(state)
	if err != nil {
		fmt.Fprintf(stderr, "daemon stop: %v\n", err)
		return 1
	}
	if stale || pid == 0 {
		if stale {
			writeLine(stdout, "removed stale daemon pid file")
		}
		// #597 review: an absent/stale pidfile is NOT proof no daemon is running
		// — an untracked `daemon run` may still flock-hold <home>/daemon.lock
		// (e.g. after a concurrent-start race clobbered its registration). Stop
		// the verified holder instead of lying "daemon not running", so `daemon
		// restart` can actually terminate it.
		if handled, code := stopUntrackedDaemonLockHolder(paths, stdout, stderr); handled {
			return code
		}
		if pid == 0 && !stale {
			writeLine(stdout, "daemon not running")
		}
		return 0
	}
	if err := stopDaemonPID(pid); err != nil {
		fmt.Fprintf(stderr, "daemon stop: %v\n", err)
		return 1
	}
	_ = os.Remove(state.PIDFile)
	writeLine(stdout, "daemon stopped pid %d", pid)
	return 0
}

func runDaemonRestart(args []string, stdout, stderr io.Writer) int {
	restartCfg, code := parseDaemonStartConfig("daemon restart", args, stderr)
	if code == daemonHelp {
		return 0
	}
	if code != 0 {
		return code
	}
	targetArgs := args
	targetWorkDir := ""
	paths, err := initializedPaths(restartCfg.Home)
	if err != nil {
		fmt.Fprintf(stderr, "daemon restart: %v\n", err)
		return 1
	}
	state := daemonProcessState(paths)
	if meta, err := readDaemonMeta(state); err == nil {
		targetArgs = daemonStartArgsFromRunArgs(meta.Args)
		targetWorkDir = meta.WorkingDir
		if restartCfg.Home != "" {
			targetArgs = withDaemonHomeArg(targetArgs, restartCfg.Home)
		}
		targetArgs = overlayDaemonStartArgs(targetArgs, restartCfg)
		if restartCfg.ExplicitRepo {
			targetWorkDir = ""
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "daemon restart: %v\n", err)
		return 1
	}
	targetCfg, code := parseDaemonStartConfig("daemon restart", targetArgs, stderr)
	if code == daemonHelp {
		return 0
	}
	if code != 0 {
		return code
	}
	if targetCfg.RepoSet {
		resolvedWorkDir, err := daemonWorkDir(targetWorkDir)
		if err != nil {
			fmt.Fprintf(stderr, "daemon restart: %v\n", err)
			return 1
		}
		if err := preflightDaemonRepoCheckout(context.Background(), targetCfg.Repo, resolvedWorkDir); err != nil {
			fmt.Fprintf(stderr, "daemon restart: %v\n", err)
			return 1
		}
	}
	stopArgs := []string{}
	if restartCfg.Home != "" {
		stopArgs = append(stopArgs, "--home", restartCfg.Home)
	}
	stopCode := runDaemonStop(stopArgs, stdout, stderr)
	if stopCode != 0 {
		return stopCode
	}
	return runDaemonStartWithWorkDirRestart(targetArgs, targetWorkDir, true, false, stdout, stderr)
}

// startDaemonChildFn is the daemon child-spawn indirection. It defaults to the
// real startDaemonChild; tests swap it so the start/restart command body can be
// driven end-to-end without launching an actual daemon process.
var startDaemonChildFn = startDaemonChild

func runDaemonStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "daemon status does not accept positional arguments")
		return 2
	}
	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "daemon status: %v\n", err)
		return 1
	}
	state := daemonProcessState(paths)
	pid, stale, err := currentDaemonPID(state)
	if err != nil {
		fmt.Fprintf(stderr, "daemon status: %v\n", err)
		return 1
	}
	switch {
	case pid > 0:
		writeLine(stdout, "daemon running pid %d", pid)
	case stale:
		writeLine(stdout, "daemon stopped (removed stale pid file)")
	default:
		writeLine(stdout, "daemon stopped")
	}
	writeLine(stdout, "log: %s", state.LogFile)
	if pid > 0 {
		if meta, err := readDaemonMeta(state); err == nil {
			writeLine(stdout, "build: %s", daemonBuildLabel(meta))
			if check := daemonBuildCheck(paths); !check.OK {
				writeLine(stdout, "WARNING: %s", check.Detail)
			}
			writeLine(stdout, "%s", daemonSchedulerStatusLine(meta.Args))
		} else {
			writeLine(stdout, "build: unknown")
		}
		writeLine(stdout, "%s", daemonClaudeAuthLine(paths))
	}
	writeLine(stdout, "%s", daemonAdmissionLine(paths))
	writeLine(stdout, "%s", daemonGitHubLimiterLine(paths))
	writeLine(stdout, "%s", daemonPreflightFailureLine(*home))
	if line := daemonMemoryHarvestLine(paths, *home); line != "" {
		writeLine(stdout, "%s", line)
	}
	for _, line := range daemonHeartbeatLines(paths, *home) {
		writeLine(stdout, "%s", line)
	}
	return 0
}

// daemonMemoryHarvestLine surfaces the fail-closed uncertain-receipt count. It
// stays absent for installations that never enable harvest and have no uncertain
// history, preserving the default status surface.
func daemonMemoryHarvestLine(paths config.Paths, home string) string {
	settings, err := config.LoadMemorySettings(paths)
	if err != nil {
		return "memory harvest: unavailable"
	}
	uncertain := 0
	active := settings.HarvestEnabled && !settings.Disabled
	if err := withStore(home, func(store *db.Store) error {
		var err error
		uncertain, err = store.CountMemoryHarvestRunsByState(context.Background(), db.MemoryHarvestUncertain)
		return err
	}); err != nil {
		if active {
			return "memory harvest: unavailable"
		}
		return ""
	}
	if !active && uncertain == 0 {
		return ""
	}
	state := "off"
	if active {
		state = "enabled"
	}
	return fmt.Sprintf("memory harvest: %s, uncertain receipts: %d", state, uncertain)
}

// daemonHeartbeatLines surfaces the configured heartbeat schedules and their
// last-run/next-due/last-status for `gitmoot daemon status` (#533). It is
// OFF BY DEFAULT: with no [agents.<agent>.heartbeats.<name>] sections it returns
// no lines, so status output is byte-identical for users without heartbeats. A
// config-parse or store error degrades to a single "unavailable" line rather than
// failing status. Each schedule renders as one line so it composes with the other
// additive status lines.
func daemonHeartbeatLines(paths config.Paths, home string) []string {
	heartbeats, err := config.LoadHeartbeats(paths)
	if err != nil {
		return []string{"heartbeats: unavailable"}
	}
	if len(heartbeats) == 0 {
		return nil
	}
	states := map[string]db.HeartbeatState{}
	if storeErr := withStore(home, func(store *db.Store) error {
		for _, heartbeat := range heartbeats {
			state, ok, err := store.GetHeartbeatState(context.Background(), heartbeat.Agent, heartbeat.Name)
			if err != nil {
				return err
			}
			if ok {
				states[heartbeat.Agent+"/"+heartbeat.Name] = state
			}
		}
		return nil
	}); storeErr != nil {
		return []string{"heartbeats: unavailable"}
	}
	lines := make([]string, 0, len(heartbeats)+1)
	lines = append(lines, fmt.Sprintf("heartbeats: %d configured", len(heartbeats)))
	for _, heartbeat := range heartbeats {
		enabled := "disabled"
		if heartbeat.Enabled {
			enabled = "enabled"
		}
		line := fmt.Sprintf("  %s/%s: %s action=%s interval=%s repo=%s",
			heartbeat.Agent, heartbeat.Name, enabled, heartbeat.Action, heartbeat.Interval, heartbeat.Repo)
		// A runtime override (#611) is surfaced only when set, so heartbeats without
		// one print byte-identically to the pre-#611 status line.
		if heartbeat.Runtime != "" {
			line += fmt.Sprintf(" runtime=%s", heartbeat.Runtime)
		}
		if state, ok := states[heartbeat.Agent+"/"+heartbeat.Name]; ok {
			line += fmt.Sprintf(" last_run=%s next_due=%s last_status=%s",
				heartbeatTimeForStatus(state.LastRunAt), heartbeatTimeForStatus(state.NextDueAt), firstNonEmpty(state.LastStatus, "-"))
		} else {
			line += " last_run=never"
		}
		lines = append(lines, line)
	}
	return lines
}

// heartbeatTimeForStatus renders a heartbeat-state timestamp for `daemon status`,
// printing "-" for the zero time (never run / not yet scheduled).
func heartbeatTimeForStatus(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

// daemonSchedulerStatusLine reports the running daemon's worker count and
// scheduler mode (#444) by reading the resolved child args persisted at start.
// Surfacing both answers "is the daemon configured for the parallelism I asked
// for?" from `daemon status` without re-deriving it from launch flags, and notes
// that same-repo parallelism still needs distinct runtime sessions (the runtime
// session lock is a second serialization layer beyond the checkout lock).
func daemonSchedulerStatusLine(args []string) string {
	workers := daemonArgValue(args, "workers")
	if workers == "" {
		workers = "1"
	}
	scheduler := daemonArgValue(args, "scheduler")
	if scheduler == "" {
		scheduler = "barrier"
	}
	line := fmt.Sprintf("scheduler: %s, workers: %s", scheduler, workers)
	if scheduler == "barrier" && workers != "1" {
		line += " (barrier serializes same-repo jobs; relaunch with --scheduler pool for parallelism)"
	}
	return line
}

// daemonArgValue extracts the value of a `--name value` or `--name=value` flag
// from a persisted daemon child-arg slice, returning "" when absent.
func daemonArgValue(args []string, name string) string {
	long := "--" + name
	prefix := long + "="
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == long && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(args[i], prefix):
			return strings.TrimPrefix(args[i], prefix)
		}
	}
	return ""
}

// daemonPreflightFailureLine reports how many coordinator jobs currently carry a
// delegation_preflight_failed event (#451) for `gitmoot daemon status`. A
// delegation fan-out that named a runtime instead of a registered agent no longer
// terminal-blocks the coordinator, so this is the cheap at-a-glance signal that a
// fan-out produced zero children and is being corrected. It is a single additive
// line reusing the JobIDsWithEventKind helper (no parallel plumbing); a store-open
// or query error degrades to "unavailable" rather than failing status.
func daemonPreflightFailureLine(home string) string {
	var count int
	err := withStore(home, func(store *db.Store) error {
		failed, err := store.JobIDsWithEventKind(context.Background(), "delegation_preflight_failed")
		if err != nil {
			return err
		}
		count = len(failed)
		return nil
	})
	if err != nil {
		return "delegation preflight failures: unavailable"
	}
	return fmt.Sprintf("delegation preflight failures: %d", count)
}

// configureGitHubLimiter installs the process-wide GitHub call budget + secondary-
// rate-limit backoff (#683) from the [github] config section onto the shared
// github.DefaultLimiter used by every gh call this daemon PROCESS issues (it does
// not extend to foreground gitmoot processes or runtime subprocesses). It is
// best-effort: a
// config-load error leaves the inert default (byte-identical, no backoff) and logs
// a warning rather than blocking the daemon. On success it logs one line so an
// operator can see the active budget.
func configureGitHubLimiter(paths config.Paths, stdout io.Writer) {
	policy, err := config.LoadGitHubLimiterPolicy(paths)
	if err != nil {
		writeLine(stdout, "daemon: [github] limiter config error, using defaults: %v", err)
		policy = config.DefaultGitHubLimiterPolicy()
	}
	github.ConfigureDefault(github.RateLimiterConfig{
		MaxConcurrent:    policy.MaxConcurrent,
		MinInterval:      policy.MinInterval,
		BackoffEnabled:   policy.SecondaryBackoffEnabled,
		BaseBackoff:      policy.BackoffBase,
		MaxBackoff:       policy.BackoffMax,
		CallsPerHourWarn: policy.CallsPerHourWarn,
		Logf:             log.Printf,
	})
	github.ConfigureConditional(policy.ConditionalRequests)
	writeLine(stdout, "daemon: %s", githubLimiterSummary(policy))
}

// githubLimiterSummary renders the [github] limiter policy as one status/log line.
func githubLimiterSummary(policy config.GitHubLimiterPolicy) string {
	concurrent := "unlimited"
	if policy.MaxConcurrent > 0 {
		concurrent = fmt.Sprintf("%d", policy.MaxConcurrent)
	}
	interval := "off"
	if policy.MinInterval > 0 {
		interval = policy.MinInterval.String()
	}
	backoff := "off"
	if policy.SecondaryBackoffEnabled {
		backoff = fmt.Sprintf("on (base=%s max=%s)", policy.BackoffBase, policy.BackoffMax)
	}
	conditional := "off"
	if policy.ConditionalRequests {
		conditional = "on"
	}
	return fmt.Sprintf("github limiter: max_concurrent=%s min_interval=%s secondary_backoff=%s conditional_requests=%s calls_per_hour_warn=%d",
		concurrent, interval, backoff, conditional, policy.CallsPerHourWarn)
}

// daemonGitHubLimiterLine reports the configured [github] call budget (#683) for
// `gitmoot daemon status`. Like daemonAdmissionLine it is a single additive line
// showing the CONFIGURED policy; the live backoff window lives in the running
// daemon process (github.DefaultLimiter().Snapshot()), not this separate status CLI.
func daemonGitHubLimiterLine(paths config.Paths) string {
	policy, err := config.LoadGitHubLimiterPolicy(paths)
	if err != nil {
		return "github limiter: unavailable"
	}
	return githubLimiterSummary(policy)
}

// daemonAdmissionLine reports the configured host-global admission budget caps
// (#365) for `gitmoot daemon status`. It is intentionally a single additive line
// (no edits to existing lines) so it composes with #444's status edits. When the
// budget is off (both caps 0/unset, the default) it says so; the in-flight
// reservation gauge lives in the daemon process, not the status CLI, so only the
// configured caps are surfaced here.
func daemonAdmissionLine(paths config.Paths) string {
	policy, err := config.LoadAdmissionPolicy(paths)
	if err != nil || !policy.Enabled() {
		return "admission budget: off"
	}
	sessions := "off"
	if policy.MaxConcurrentSessions > 0 {
		sessions = fmt.Sprintf("%d", policy.MaxConcurrentSessions)
	}
	memory := "off"
	if policy.MaxMemoryGB > 0 {
		memory = fmt.Sprintf("%gGB", policy.MaxMemoryGB)
	}
	return fmt.Sprintf("admission budget: max_concurrent_sessions=%s max_memory_gb=%s", sessions, memory)
}

// daemonClaudeAuthLine reports the authoritative per-delivery Claude auth source.
// It never inspects the daemon process environment because adapter builds reload
// runtime-auth.env independently of daemon lifetime.
func daemonClaudeAuthLine(paths config.Paths) string {
	state, err := loadRuntimeAuthFile(paths.Home)
	if err != nil {
		return "claude auth: warn (" + err.Error() + ")"
	}
	if state.Exists && len(state.Values) > 0 {
		auth := runtime.InspectClaudeAuthEnv(runtimeAuthEffectiveLookup(state, nil))
		return "claude auth: configured via " + runtimeAuthFileName + " (" + auth.MaskedDetail() + ")"
	}
	if state.Exists {
		return "claude auth: fallback (runtime-auth.env explicitly empty); verify with `gitmoot auth probe claude`"
	}
	return "claude auth: fallback (runtime-auth.env missing); configure with `gitmoot auth set claude`"
}

func runDaemonLogs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "daemon logs does not accept positional arguments")
		return 2
	}
	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "daemon logs: %v\n", err)
		return 1
	}
	state := daemonProcessState(paths)
	contents, err := os.ReadFile(state.LogFile)
	if errors.Is(err, os.ErrNotExist) {
		writeLine(stdout, "daemon log is empty")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "daemon logs: %v\n", err)
		return 1
	}
	_, _ = stdout.Write(contents)
	return 0
}

type daemonState struct {
	PIDFile  string
	MetaFile string
	LogFile  string
}

type daemonMeta struct {
	PID        int      `json:"pid"`
	StartedAt  string   `json:"started_at"`
	Args       []string `json:"args"`
	LogFile    string   `json:"log_file"`
	Executable string   `json:"executable"`
	WorkingDir string   `json:"working_dir"`
	Version    string   `json:"version,omitempty"`
	Commit     string   `json:"commit,omitempty"`
}

type daemonStartConfig struct {
	Home                         string
	RepoFlag                     string
	Repo                         github.Repository
	RepoSet                      bool
	Session                      string
	ExplicitSession              bool
	Poll                         time.Duration
	Workers                      int
	ExplicitStartConfig          bool
	ExplicitRepo                 bool
	ExplicitPoll                 bool
	ExplicitWorkers              bool
	WatchSkillOptReviews         bool
	ExplicitWatchSkillOptReviews bool
	WatchIssues                  bool
	ExplicitWatchIssues          bool
	Scheduler                    string
	ExplicitScheduler            bool
}

const daemonHelp = -1

func parseDaemonStartConfig(command string, args []string, stderr io.Writer) (daemonStartConfig, int) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "GitHub repository as owner/repo: scopes the daemon to a SINGLE repo (polls only that repo's PRs; claims only that repo's queued jobs). Omit to supervise ALL enabled registered repos")
	var session string
	fs.StringVar(&session, "session", "", "scope the daemon worker to a delegation root job id")
	fs.StringVar(&session, "root", "", "alias for --session")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	workers := fs.Int("workers", 1, "worker count")
	watchSkillOptReviews := fs.Bool("watch-skillopt-reviews", false, "poll watched SkillOpt review issue comments and import valid feedback")
	watchIssues := fs.Bool("watch-issues", false, "poll open issues and route @<agent> ask comments to jobs (#389)")
	scheduler := fs.String("scheduler", "barrier", "queued-job scheduler: barrier (default) or pool (#394 opt-in continuous worker pool)")
	parallel := fs.Int("parallel", 0, "run jobs in parallel: sets --workers N and --scheduler pool together (#444)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return daemonStartConfig{}, daemonHelp
		}
		return daemonStartConfig{}, 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s does not accept positional arguments\n", command)
		return daemonStartConfig{}, 2
	}
	cfg := daemonStartConfig{
		Home:                 *home,
		RepoFlag:             *repoFlag,
		Session:              session,
		Poll:                 *poll,
		Workers:              *workers,
		WatchSkillOptReviews: *watchSkillOptReviews,
		WatchIssues:          *watchIssues,
		Scheduler:            *scheduler,
	}
	explicitParallel := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "repo":
			cfg.ExplicitRepo = true
			cfg.ExplicitStartConfig = true
		case "session", "root":
			cfg.ExplicitSession = true
			cfg.ExplicitStartConfig = true
		case "poll":
			cfg.ExplicitPoll = true
			cfg.ExplicitStartConfig = true
		case "workers":
			cfg.ExplicitWorkers = true
			cfg.ExplicitStartConfig = true
		case "watch-skillopt-reviews":
			cfg.ExplicitWatchSkillOptReviews = true
			cfg.ExplicitStartConfig = true
		case "watch-issues":
			cfg.ExplicitWatchIssues = true
			cfg.ExplicitStartConfig = true
		case "scheduler":
			cfg.ExplicitScheduler = true
			cfg.ExplicitStartConfig = true
		case "parallel":
			explicitParallel = true
			cfg.ExplicitStartConfig = true
		}
	})
	// --parallel N is intent-level sugar: it sets workers=N + scheduler=pool
	// together (#444), so the user names the goal rather than two orthogonal knobs.
	// It conflicts with explicit --workers/--scheduler to avoid silently winning.
	if explicitParallel {
		if cfg.ExplicitWorkers || cfg.ExplicitScheduler {
			fmt.Fprintln(stderr, "--parallel cannot be combined with --workers or --scheduler")
			return daemonStartConfig{}, 2
		}
		if *parallel <= 0 {
			fmt.Fprintln(stderr, "--parallel must be positive")
			return daemonStartConfig{}, 2
		}
		cfg.Workers = *parallel
		cfg.ExplicitWorkers = true
		cfg.Scheduler = "pool"
		cfg.ExplicitScheduler = true
	}
	// Auto-select pool when multiple workers are requested without an explicit
	// scheduler, so --workers N actually parallelizes (#444). An explicit
	// --scheduler barrier still forces the old per-tick behavior.
	cfg.Scheduler = autoSelectScheduler(cfg.Scheduler, cfg.Workers, cfg.ExplicitScheduler)
	if cfg.RepoFlag != "" {
		repo, err := daemon.ParseRepository(cfg.RepoFlag)
		if err != nil {
			fmt.Fprintf(stderr, "invalid repo: %v\n", err)
			return daemonStartConfig{}, 2
		}
		cfg.Repo = repo
		cfg.RepoSet = true
	}
	if cfg.RepoFlag != "" && !cfg.RepoSet {
		return daemonStartConfig{}, 2
	}
	if cfg.Poll <= 0 {
		fmt.Fprintln(stderr, "poll interval must be positive")
		return daemonStartConfig{}, 2
	}
	if cfg.Workers <= 0 {
		fmt.Fprintln(stderr, "workers must be positive")
		return daemonStartConfig{}, 2
	}
	if _, err := parseSchedulerMode(cfg.Scheduler); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return daemonStartConfig{}, 2
	}
	return cfg, 0
}

func initializedPaths(home string) (config.Paths, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return config.Paths{}, err
	}
	if err := config.Initialize(paths); err != nil {
		return config.Paths{}, err
	}
	return paths, nil
}

func daemonProcessState(paths config.Paths) daemonState {
	return daemonState{
		PIDFile:  filepath.Join(paths.Home, "daemon.pid"),
		MetaFile: filepath.Join(paths.Home, "daemon.json"),
		LogFile:  filepath.Join(paths.Logs, "daemon.log"),
	}
}

func daemonWorkDir(workDir string) (string, error) {
	if strings.TrimSpace(workDir) != "" {
		return filepath.Abs(workDir)
	}
	return os.Getwd()
}

func preflightDaemonRepoStart(ctx context.Context, home string, repo github.Repository, pollInterval string, workDir string) error {
	return withStore(home, func(store *db.Store) error {
		repoRecord, err := resolveDaemonStartRepo(ctx, store, repo, workDir)
		if err != nil {
			return err
		}
		repoRecord.PollInterval = pollInterval
		return store.UpsertRepo(ctx, repoRecord)
	})
}

func preflightDaemonRepoCheckout(ctx context.Context, repo github.Repository, workDir string) error {
	_, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: workDir})
	return err
}

func readDaemonMeta(state daemonState) (daemonMeta, error) {
	contents, err := os.ReadFile(state.MetaFile)
	if err != nil {
		return daemonMeta{}, err
	}
	var meta daemonMeta
	if err := json.Unmarshal(contents, &meta); err != nil {
		return daemonMeta{}, err
	}
	return meta, nil
}

func daemonStartArgsFromRunArgs(args []string) []string {
	if len(args) >= 2 && args[0] == "daemon" && args[1] == "run" {
		args = args[2:]
	}
	startArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--dry-run" {
			continue
		}
		startArgs = append(startArgs, args[i])
	}
	return startArgs
}

func withDaemonHomeArg(args []string, home string) []string {
	if home == "" {
		return args
	}
	cleaned := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		if args[i] == "--home" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--home=") {
			continue
		}
		cleaned = append(cleaned, args[i])
	}
	return append(cleaned, "--home", home)
}

func overlayDaemonStartArgs(args []string, cfg daemonStartConfig) []string {
	if cfg.ExplicitRepo {
		args = withDaemonFlagArg(args, "repo", cfg.RepoFlag)
	}
	if cfg.ExplicitSession {
		args = withDaemonFlagArg(args, "session", cfg.Session)
	}
	if cfg.ExplicitPoll {
		args = withDaemonFlagArg(args, "poll", cfg.Poll.String())
	}
	if cfg.ExplicitWorkers {
		args = withDaemonFlagArg(args, "workers", strconv.Itoa(cfg.Workers))
	}
	if cfg.ExplicitWatchSkillOptReviews {
		args = withDaemonBoolFlagArg(args, "watch-skillopt-reviews", cfg.WatchSkillOptReviews)
	}
	if cfg.ExplicitWatchIssues {
		args = withDaemonBoolFlagArg(args, "watch-issues", cfg.WatchIssues)
	}
	if cfg.ExplicitScheduler {
		args = withDaemonFlagArg(args, "scheduler", cfg.Scheduler)
	}
	return args
}

func withDaemonFlagArg(args []string, name string, value string) []string {
	flagName := "--" + name
	cleaned := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		if args[i] == flagName {
			i++
			continue
		}
		if strings.HasPrefix(args[i], flagName+"=") {
			continue
		}
		cleaned = append(cleaned, args[i])
	}
	if value == "" {
		return cleaned
	}
	return append(cleaned, flagName, value)
}

func withDaemonBoolFlagArg(args []string, name string, enabled bool) []string {
	flagName := "--" + name
	cleaned := make([]string, 0, len(args)+1)
	for _, arg := range args {
		if arg == flagName || strings.HasPrefix(arg, flagName+"=") {
			continue
		}
		cleaned = append(cleaned, arg)
	}
	if enabled {
		return append(cleaned, flagName)
	}
	return cleaned
}

// guardDaemonRunSingleton refuses to start a second daemon against the same home
// (#550). It reuses currentDaemonPID — the exact liveness check `daemon start`
// uses and the #505 definition of "running": an owner-pid that is alive AND whose
// /proc cmdline still matches the recorded daemon meta (presence.ProbeDaemonProcess).
// Reusing that check is what makes this correct at the right depth:
//   - a STALE pidfile from a dead (or non-daemon) pid is treated as not-running,
//     so it is silently cleared and never blocks a fresh start; and
//   - a genuinely live registered daemon is detected and the new run refuses.
//
// A recorded pid equal to our own is never a conflict: when `daemon start` forks
// the detached child it writes the pidfile with the CHILD's pid, so the child's
// own `daemon run` guard sees itself and proceeds (the healthy start path).
//
// Returns 0 to proceed, or a non-zero exit code (with a clear message on stderr)
// to refuse.
func guardDaemonRunSingleton(home string, stderr io.Writer) int {
	paths, err := initializedPaths(home)
	if err != nil {
		fmt.Fprintf(stderr, "daemon run: %v\n", err)
		return 1
	}
	state := daemonProcessState(paths)
	pid, _, err := currentDaemonPID(state)
	if err != nil {
		fmt.Fprintf(stderr, "daemon run: %v\n", err)
		return 1
	}
	if pid > 0 && pid != os.Getpid() {
		fmt.Fprintf(stderr, "daemon already running with pid %d\n", pid)
		return 1
	}
	return 0
}

// daemonRunLockPath is the flock target for the daemon-run singleton backstop
// (#556): a dedicated sibling file under the resolved daemon home. Deliberately
// NOT gitmoot.db itself — SQLite manages its own fcntl/WAL locking on the
// database file and an unrelated flock there is asking for interference.
func daemonRunLockPath(paths config.Paths) string {
	return filepath.Join(paths.Home, "daemon.lock")
}

// daemonRunWidenGuardWindowEnv, when set to a positive Go duration, makes
// `daemon run` sleep between passing the liveness guard and taking the flock /
// self-registering. Test-only seam (#556): the guard→register TOCTOU window is
// normally microseconds, so the singleton E2E widens it deterministically to
// prove the flock backstop (and to fail loudly if the flock is ever removed).
const daemonRunWidenGuardWindowEnv = "GITMOOT_TEST_WIDEN_DAEMON_RUN_GUARD_WINDOW"

func daemonRunWidenGuardWindow() {
	raw := os.Getenv(daemonRunWidenGuardWindowEnv)
	if raw == "" {
		return
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		time.Sleep(d)
	}
}

// acquireDaemonRunLock closes the residual #550 TOCTOU (#556): two `daemon run`
// launched simultaneously on a clean home can both pass guardDaemonRunSingleton
// before either self-registers. flock(LOCK_EX|LOCK_NB) on <home>/daemon.lock is
// atomic, so exactly one acquires; the loser refuses with the guard's "daemon
// already running" UX. The winner keeps the fd open for its whole lifetime (the
// returned release closes it on clean shutdown) and the kernel drops the lock on
// ANY process death — SIGKILL included — so a stale lockfile can never lock a
// fresh daemon out, and no unlink is ever needed (flock on an existing file just
// works). The lockfile's pid content is best-effort diagnostics only; the LOCK
// is what is authoritative.
func acquireDaemonRunLock(home string, stderr io.Writer) (release func(), code int) {
	paths, err := initializedPaths(home)
	if err != nil {
		fmt.Fprintf(stderr, "daemon run: %v\n", err)
		return nil, 1
	}
	lockPath := daemonRunLockPath(paths)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "daemon run: open daemon lock: %v\n", err)
		return nil, 1
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := daemonRunLockHolder(f)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			fmt.Fprintf(stderr, "daemon already running%s (%s is flock-held by another daemon run)\n", holder, lockPath)
			return nil, 1
		}
		fmt.Fprintf(stderr, "daemon run: lock %s: %v\n", lockPath, err)
		return nil, 1
	}
	// Record the holder's pid for the loser's message. Best-effort: a loser can
	// race this write (empty read) or see a SIGKILLed predecessor's pid; either
	// way only the diagnostic suffix degrades, never the mutual exclusion.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() { _ = f.Close() }, 0
}

// daemonRunLockHolder reads the pid the current lock holder recorded in the
// lockfile, returning a " with pid N" suffix for the refusal message, or "" when
// the content is absent/unreadable (best-effort diagnostics only).
func daemonRunLockHolder(f *os.File) string {
	pid := daemonRunLockHolderPID(f)
	if pid <= 0 {
		return ""
	}
	return fmt.Sprintf(" with pid %d", pid)
}

// daemonRunLockHolderPID reads the pid the current lock holder recorded in the
// lockfile, or 0 when the content is absent/unreadable (best-effort
// diagnostics only — the flock itself is what is authoritative).
func daemonRunLockHolderPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	if n <= 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// tryDaemonRunLock probes <home>/daemon.lock without keeping it: it reports
// whether the flock is currently held by another process — i.e. a `daemon run`
// is alive on this home regardless of what the registration files say — and
// the holder's best-effort recorded pid. When the probe acquires the lock it
// releases it immediately (held=false): the caller is NOT the daemon and must
// not keep it. flock is on the open file description, so the deferred Close
// releases an acquired probe with no residue.
func tryDaemonRunLock(paths config.Paths) (held bool, holderPID int, err error) {
	lockPath := daemonRunLockPath(paths)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, 0, fmt.Errorf("open daemon lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, daemonRunLockHolderPID(f), nil
		}
		return false, 0, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	return false, 0, nil
}

// refuseWhenDaemonRunLockHeld is the `daemon start`/`daemon restart` parent's
// pre-spawn probe (#597 review): it checks <home>/daemon.lock the same way the
// spawned `daemon run` child will and refuses — with the untracked holder's
// pid and the same "daemon already running" UX as the child's own #556 flock
// refusal — instead of spawning a child that is guaranteed to lose the flock
// and die with the refusal visible only in daemon.log. Returns 0 to proceed.
func refuseWhenDaemonRunLockHeld(cmdName, home string, stderr io.Writer) int {
	paths, err := initializedPaths(home)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)
		return 1
	}
	held, holderPID, err := tryDaemonRunLock(paths)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)
		return 1
	}
	if !held {
		return 0
	}
	holder := ""
	if holderPID > 0 {
		holder = fmt.Sprintf(" with pid %d", holderPID)
	}
	fmt.Fprintf(stderr, "daemon already running%s (%s is flock-held by another daemon run)\n", holder, daemonRunLockPath(paths))
	return 1
}

// daemonStartConfirmWindow bounds how long `daemon start` waits to confirm the
// spawned `daemon run` child survived its startup guards. The healthy path
// exits the window in milliseconds (the child self-registers, which is the
// early-out), and a flock loser dies in milliseconds (wait4 reaps it, which is
// the failure-out). The full window is only ever paid in the pathological
// "alive but never self-registered" case, where proceeding as success is the
// correct fail-safe: registration is best-effort inside the child.
const daemonStartConfirmWindow = 3 * time.Second

// confirmDaemonRunChildSurvived verifies a freshly spawned `daemon run` child
// did not die inside its startup guards (#597 review — the flock loser exits 1
// within milliseconds, refusal only in daemon.log). It polls wait4(WNOHANG):
// even after Process.Release the child is still OUR direct child (Setsid
// detaches the session, not the parent link), so a plain kill(pid, 0) liveness
// probe would see the unreaped zombie as "running" forever; wait4 both detects
// and reaps the corpse. Success is the child's self-registration (daemon.pid
// records its pid) or still-alive at the deadline; failure returns an error
// carrying the child's exit cause and the daemon.log tail (where its refusal
// was written).
func confirmDaemonRunChildSurvived(pid int, state daemonState) error {
	deadline := time.Now().Add(daemonStartConfirmWindow)
	for {
		var status syscall.WaitStatus
		reaped, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if err == nil && reaped == pid {
			detail := fmt.Sprintf("exit status %d", status.ExitStatus())
			if status.Signaled() {
				detail = "signal " + status.Signal().String()
			}
			return fmt.Errorf("daemon run child pid %d exited immediately (%s) — daemon NOT started%s", pid, detail, daemonLogTail(state.LogFile))
		}
		if raw, rerr := os.ReadFile(state.PIDFile); rerr == nil {
			if registered, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && registered == pid {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// daemonLogTail returns a "; <log> tail:\n..." suffix with the trailing bytes
// of the daemon log for start-failure errors — the dead child's stdout/stderr
// (including the #556 "daemon already running ... flock-held" refusal) go only
// there. Best-effort: an unreadable or empty log yields a pointer to the file.
func daemonLogTail(logFile string) string {
	const maxTail = 2048
	fallback := fmt.Sprintf("; see %s", logFile)
	f, err := os.Open(logFile)
	if err != nil {
		return fallback
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return fallback
	}
	offset := int64(0)
	if info.Size() > maxTail {
		offset = info.Size() - maxTail
	}
	buf := make([]byte, info.Size()-offset)
	n, _ := f.ReadAt(buf, offset)
	text := strings.TrimSpace(string(buf[:n]))
	if text == "" {
		return fallback
	}
	return fmt.Sprintf("; %s tail:\n%s", logFile, text)
}

// stopUntrackedDaemonLockHolder handles `daemon stop` for the "live but
// untracked daemon" state (#597 review): the registration files say nothing is
// running (pidfile absent or stale) yet <home>/daemon.lock is flock-held by a
// live `daemon run` — e.g. the aftermath of a concurrent-start race that
// clobbered daemon.pid. Reporting "daemon not running" there is a lie that
// also makes `daemon restart` a silent no-op against the real daemon.
//
// It returns handled=false when the lock is free or unprobeable (the normal
// "daemon not running" path proceeds). When the lock IS held it either stops
// the holder — but ONLY when the recorded pid can be positively verified as a
// gitmoot `daemon run` process (the lockfile's pid content is best-effort, and
// SIGTERMing an unverified pid risks hitting a recycled one) — or reports the
// untracked holder truthfully with a non-zero code.
func stopUntrackedDaemonLockHolder(paths config.Paths, stdout, stderr io.Writer) (handled bool, code int) {
	held, holderPID, err := tryDaemonRunLock(paths)
	if err != nil || !held {
		return false, 0
	}
	lockPath := daemonRunLockPath(paths)
	if holderPID > 0 && processIsGitmootDaemonRun(holderPID, lockPath) {
		if err := stopDaemonPID(holderPID); err != nil {
			fmt.Fprintf(stderr, "daemon stop: %v\n", err)
			return true, 1
		}
		writeLine(stdout, "daemon stopped pid %d (untracked %s holder)", holderPID, lockPath)
		return true, 0
	}
	fmt.Fprintf(stderr, "daemon stop: %s is flock-held by a live but UNTRACKED daemon (recorded holder pid %d could not be verified); refusing to signal an unverified pid — find and stop it manually\n", lockPath, holderPID)
	return true, 1
}

// processIsGitmootDaemonRun reports whether pid is positively identifiable as a
// live gitmoot `daemon run` process. It is a KILL-guard, so it errs on the side
// of false: the argv must contain adjacent "daemon run" tokens, and — where the
// host exposes /proc/<pid>/fd — the process must actually hold lockPath open
// (closing the microscopic window where the lockfile still carries a dead
// predecessor's since-recycled pid).
func processIsGitmootDaemonRun(pid int, lockPath string) bool {
	if running, err := processRunning(pid); err != nil || !running {
		return false
	}
	argv := processArgv(pid)
	looksLikeDaemonRun := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "daemon" && argv[i+1] == "run" {
			looksLikeDaemonRun = true
			break
		}
	}
	if !looksLikeDaemonRun {
		return false
	}
	if holds, verifiable := processHoldsFileOpen(pid, lockPath); verifiable && !holds {
		return false
	}
	return true
}

// processArgv returns pid's argv via /proc (exact) or a ps fallback
// (whitespace-split, best-effort), or nil when neither is available.
func processArgv(pid int) []string {
	if raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")); err == nil {
		return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	}
	if out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output(); err == nil {
		return strings.Fields(strings.TrimSpace(string(out)))
	}
	return nil
}

// processHoldsFileOpen reports whether pid has path among its open file
// descriptors, via /proc/<pid>/fd. verifiable=false when the host cannot
// answer (no /proc, or the fd table is unreadable) so callers can fall back to
// weaker evidence instead of hard-failing on non-Linux hosts.
func processHoldsFileOpen(pid int, path string) (holds bool, verifiable bool) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return false, false
	}
	for _, entry := range entries {
		if target, err := os.Readlink(filepath.Join(fdDir, entry.Name())); err == nil && target == path {
			return true, true
		}
	}
	return false, true
}

func currentDaemonPID(state daemonState) (pid int, stale bool, err error) {
	return currentDaemonPIDWithProbe(state, presence.ProbeDaemonProcess)
}

func currentDaemonPIDWithProbe(state daemonState, probe func(int, string) (string, error)) (pid int, stale bool, err error) {
	contents, err := os.ReadFile(state.PIDFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || pid <= 0 {
		_ = os.Remove(state.PIDFile)
		return 0, true, nil
	}
	probeState, err := probe(pid, state.MetaFile)
	if err != nil {
		return 0, false, err
	}
	if probeState == presence.DaemonUnknown {
		return 0, false, nil
	}
	if probeState != presence.DaemonRunning {
		_ = os.Remove(state.PIDFile)
		return 0, true, nil
	}
	return pid, false, nil
}

func startDaemonChild(home string, poll string, workers int, watchSkillOptReviews bool, watchIssues bool, scheduler string, repo string, session string, state daemonState, workDir string) (daemonMeta, error) {
	executable, err := os.Executable()
	if err != nil {
		return daemonMeta{}, err
	}
	logFile, err := os.OpenFile(state.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return daemonMeta{}, err
	}
	defer logFile.Close()
	args := daemonChildArgs(home, poll, workers, watchSkillOptReviews, watchIssues, scheduler, repo, session)
	cmd := exec.Command(executable, args...)
	cmd.Dir = workDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return daemonMeta{}, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return daemonMeta{}, err
	}
	// #597 review: a child that loses the daemon.lock flock (#556) — or dies for
	// any other startup reason — exits within milliseconds, with its refusal
	// written only to daemon.log. Without this check the caller would print
	// "daemon started pid N", exit 0, and register the corpse's pid, leaving an
	// operator with a false green and (in the flock case) a live daemon no CLI
	// command can see. Confirm the child survived before declaring success.
	if err := confirmDaemonRunChildSurvived(pid, state); err != nil {
		return daemonMeta{}, err
	}
	return daemonMeta{
		PID:        pid,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		Args:       args,
		LogFile:    state.LogFile,
		Executable: executable,
		WorkingDir: workDir,
	}, nil
}

func daemonChildArgs(home string, poll string, workers int, watchSkillOptReviews bool, watchIssues bool, scheduler string, repo string, session string) []string {
	args := []string{"daemon", "run", "--poll", poll, "--workers", strconv.Itoa(workers)}
	if home != "" {
		args = append(args, "--home", home)
	}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	if session != "" {
		args = append(args, "--session", session)
	}
	if watchSkillOptReviews {
		args = append(args, "--watch-skillopt-reviews")
	}
	if watchIssues {
		args = append(args, "--watch-issues")
	}
	if usePool, _ := parseSchedulerMode(scheduler); usePool {
		args = append(args, "--scheduler", "pool")
	}
	return args
}

func writeDaemonState(state daemonState, meta daemonMeta) error {
	if err := os.WriteFile(state.PIDFile, []byte(strconv.Itoa(meta.PID)+"\n"), 0o600); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(state.MetaFile, append(encoded, '\n'), 0o600)
}

func daemonMetaWithCurrentBuild(meta daemonMeta) daemonMeta {
	build := buildinfo.Current()
	meta.Version = build.Version
	meta.Commit = build.Commit
	return meta
}

// daemonRunStartupReconcile performs the two #505 startup steps for a directly
// launched `daemon run`: it self-registers THIS process's pid/meta so `daemon
// status` and the dashboard recognize it (gap 3), and it reconciles phantom
// "running" runtime sessions a previously-crashed daemon left behind (gap 2).
// Both are best-effort — a failure never blocks the daemon. It returns a cleanup
// func (never nil) the caller defers: it deregisters this process's state on exit
// only when this process actually self-registered (so a `daemon start`-spawned
// child, whose parent already owns the pid, is a no-op). Keeping the logic here —
// rather than inline in runDaemonRun — lets a test bind the regression to the
// integration point (status flips to "running", an orphaned row resets to idle),
// not just the leaf helpers it calls.
func daemonRunStartupReconcile(ctx context.Context, home string, argv []string, stdout io.Writer) func() {
	cleanup := func() {}
	paths, err := initializedPaths(home)
	if err != nil {
		return cleanup
	}
	state := daemonProcessState(paths)
	workDir, _ := os.Getwd()
	if registered, regErr := registerDaemonRunState(state, argv, workDir); regErr == nil && registered {
		cleanup = func() { deregisterDaemonRunState(state) }
	}
	_ = withStore(home, func(store *db.Store) error {
		if n, err := store.ReconcileOrphanedRunningInstances(ctx, time.Now().UTC()); err == nil && n > 0 {
			writeLine(stdout, "reconciled %d orphaned running runtime session(s)", n)
		}
		return nil
	})
	return cleanup
}

// registerDaemonRunState records THIS process as the running daemon so `daemon
// status` and the dashboard recognize a `daemon run` launched directly — e.g. as
// the ExecStart of a `systemd --user` unit — and not only one spawned by `daemon
// start` (#505 gap 3). `daemon start` forks a detached child and writes the
// pid/meta from the PARENT; a systemd-managed `daemon run` has no such parent, so
// without this self-registration daemon.json is never written and status falsely
// reports "stopped" while the daemon is alive and processing.
//
// args MUST be the process's own argv (os.Args) so the recorded meta.Executable
// (argv[0]) and meta.Args (argv[1:]) match /proc/<pid>/cmdline and pass
// presence.ProbeDaemonProcess. It refuses to clobber a DIFFERENT live daemon's state
// (returns ok=false), so the existing "daemon start then child self-registers"
// flow stays idempotent (the child owns the pid it finds) and an unrelated daemon
// is never overwritten.
func registerDaemonRunState(state daemonState, argv []string, workDir string) (ok bool, err error) {
	if existing, _, perr := currentDaemonPID(state); perr == nil && existing > 0 && existing != os.Getpid() {
		return false, nil
	}
	executable := ""
	if len(argv) > 0 {
		executable = argv[0]
	}
	meta := daemonMetaWithCurrentBuild(daemonMeta{
		PID:        os.Getpid(),
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		Args:       argv[1:],
		LogFile:    state.LogFile,
		Executable: executable,
		WorkingDir: workDir,
	})
	if err := writeDaemonState(state, meta); err != nil {
		return false, err
	}
	return true, nil
}

// deregisterDaemonRunState clears the running marker written by
// registerDaemonRunState when the daemon-run process exits, but ONLY when the
// recorded state still points at THIS process — so a daemon that was restarted out
// from under us (a fresh pid already recorded) never has its live state clobbered
// on our shutdown.
//
// It removes ONLY the PIDFile, mirroring `daemon stop` (which also leaves the meta
// in place): currentDaemonPID already treats an absent/dead pid as stopped, so a
// status check correctly reports "stopped" with the meta retained, and keeping
// daemon.json preserves `daemon restart`'s recovery of the prior daemon's
// --repo/--watch-issues/--workers/workdir (readDaemonMeta) that worked on main.
// Deleting the meta here would silently break that restart path (#505 review).
func deregisterDaemonRunState(state daemonState) {
	if meta, err := readDaemonMeta(state); err != nil || meta.PID != os.Getpid() {
		return
	}
	_ = os.Remove(state.PIDFile)
}

func stopDaemonPID(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running, err := processRunning(pid)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon pid %d did not stop after SIGTERM", pid)
}

func processRunning(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	return false, err
}
