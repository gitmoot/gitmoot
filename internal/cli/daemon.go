package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/buildinfo"
	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/sandbox"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func runDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printDaemonUsage(stdout)
		return 0
	}
	switch args[0] {
	case "start":
		return runDaemonStart(args[1:], stdout, stderr)
	case "run":
		return runDaemonRun(args[1:], stdout, stderr)
	case "stop":
		return runDaemonStop(args[1:], stdout, stderr)
	case "restart":
		return runDaemonRestart(args[1:], stdout, stderr)
	case "status":
		return runDaemonStatus(args[1:], stdout, stderr)
	case "logs":
		return runDaemonLogs(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown daemon command %q\n\n", args[0])
		printDaemonUsage(stderr)
		return 2
	}
}

func printDaemonUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot daemon start [--repo owner/repo] [--poll 30s] [--workers 1 | --parallel N] [--scheduler barrier|pool] [--watch-skillopt-reviews] [--watch-issues]")
	fmt.Fprintln(w, "  gitmoot daemon run [--repo owner/repo] [--poll 30s] [--workers 1 | --parallel N] [--scheduler barrier|pool] [--watch-skillopt-reviews] [--watch-issues]")
	fmt.Fprintln(w, "  gitmoot daemon stop")
	fmt.Fprintln(w, "  gitmoot daemon restart")
	fmt.Fprintln(w, "  gitmoot daemon status")
	fmt.Fprintln(w, "  gitmoot daemon logs")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  --repo owner/repo SCOPES the daemon to a SINGLE repo: it polls only that repo's PRs and")
	fmt.Fprintln(w, "  claims only that repo's queued jobs. Omit --repo to supervise ALL enabled registered repos.")
}

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
		mergeGate := newDaemonPolicyMergeGate(store, gh, checkout)
		applyMergeGatePolicy(&mergeGate, *home, repo.FullName())
		engine := workflow.Engine{
			Store:           store,
			ProduceCheckDir: checkout,
			MergeGate:       mergeGate,
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
// daemon process (github.DefaultLimiterSnapshot), not this separate status CLI.
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
// /proc cmdline still matches the recorded daemon meta (processLooksLikeDaemon).
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
	running, err := processRunning(pid)
	if err != nil {
		return 0, false, err
	}
	if !running || !processLooksLikeDaemon(pid, state) {
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
// processLooksLikeDaemon. It refuses to clobber a DIFFERENT live daemon's state
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

func processLooksLikeDaemon(pid int, state daemonState) bool {
	contents, err := os.ReadFile(state.MetaFile)
	if err != nil {
		return false
	}
	var meta daemonMeta
	if err := json.Unmarshal(contents, &meta); err != nil {
		return false
	}
	if meta.PID != pid {
		return false
	}
	if processCmdlineLooksLikeDaemon(pid, meta) {
		return true
	}
	return processPSLooksLikeDaemon(pid, meta)
}

func processCmdlineLooksLikeDaemon(pid int, meta daemonMeta) bool {
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	parts := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	return daemonProcessArgsMatch(parts, meta)
}

func processPSLooksLikeDaemon(pid int, meta daemonMeta) bool {
	if hasWhitespace(meta.Executable) {
		return false
	}
	for _, arg := range meta.Args {
		if hasWhitespace(arg) {
			return false
		}
	}
	result, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	command := strings.TrimSpace(string(result))
	if command == "" {
		return false
	}
	return daemonProcessArgsMatch(strings.Fields(command), meta)
}

func daemonProcessArgsMatch(argv []string, meta daemonMeta) bool {
	if len(argv) != len(meta.Args)+1 {
		return false
	}
	if meta.Executable != "" && argv[0] != meta.Executable {
		return false
	}
	return equalStringSlices(argv[1:], meta.Args)
}

func equalStringSlices(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func hasWhitespace(value string) bool {
	return strings.ContainsAny(value, " \t\r\n")
}

func runRegisteredRepoSupervisor(ctx context.Context, home string, live *daemonReloadableConfig, dryRun bool, watchSkillOptReviews bool, watchIssues bool, rootFilter string, stdout io.Writer) error {
	return withStoreAndPaths(home, func(paths config.Paths, store *db.Store) error {
		schedule := registeredRepoSchedule{
			NextPoll:    map[string]time.Time{},
			ErrorStreak: map[string]int{},
			IdleStreak:  map[string]int{},
		}
		_, initialWorkers, _ := live.snapshot()
		// rawHome (the function's own param) feeds the read-only policy loaders;
		// paths.Home (the resolved <home>/.gitmoot root) feeds the engine wiring (#459).
		poller := defaultRegisteredRepoPoller(store, initialWorkers, dryRun, stdout, home, paths.Home)
		poller.WatchIssues = watchIssues
		blobStore := artifact.NewStore(paths.ArtifactBlobs)
		reviewGitHub := newSkillOptGitHubClient()
		worker := defaultJobWorker(store, stdout, home)
		worker.CommenterFactory = worker.defaultCommenter
		worker.Admission = worker.loadAdmissionBudget()
		checkoutLocks := &repoCheckoutLocks{}
		poller.CheckoutLocks = checkoutLocks
		var workerErr <-chan error
		if !dryRun {
			if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, time.Now().UTC()); err != nil {
				return err
			}
			if err := recoverForeignBootRunners(ctx, store, stdout); err != nil {
				return err
			}
			if err := recoverCancelledRunningJobsForEnabledRepos(ctx, store, rootFilter, stdout); err != nil {
				return err
			}
			// The in-flight tracker (#562) keeps one hung/long job on any repo
			// from wedging the whole sweep: ticks claim + dispatch async and
			// return promptly. The poller consults it so a repo with in-flight
			// jobs degrades to the recovery-only poll instead of mutating the
			// checkout under a running job. The deferred drain cancels + waits
			// (bounded) for in-flight jobs on exit.
			tracker := newInflightJobTracker(ctx)
			defer tracker.drain(stdout, daemonShutdownDrainTimeout)
			poller.Inflight = tracker
			workerErr = startSupervisorWorkerLoopRecovering(ctx, daemonWorkerLoopInterval, stdout, func(now time.Time) error {
				// Read the warm-reloadable worker count + scheduler mode each tick
				// (#577). The worker is copied per tick so setting UsePool is race-free
				// with the SIGHUP reload goroutine, and the pool is re-dispatched each
				// tick so a resize applies live without disturbing in-flight jobs.
				_, workers, usePool := live.snapshot()
				w := worker
				w.UsePool = usePool
				return runEnabledRepoWorkerTicksTracked(ctx, store, w, workers, rootFilter, stdout, now, checkoutLocks, tracker)
			})
			// #884 home-scoped post-terminal insight harvest. This owns one
			// sequential classifier lane for the daemon process and is deliberately
			// outside daemonWorkflowEngine, which is rebuilt per repo/tick.
			startMemoryHarvestLoop(ctx, paths, home, store, stdout)
			startCockpitReconcileLoop(ctx, store, paths.Home, stdout)
		}
		// Heartbeat schedules (#533) reuse the normal job queue. Off-by-default: with
		// no heartbeat sections the scan returns before any store touch. Skip it under
		// --dry-run (no worker loop runs there, so an enqueued job would just sit).
		heartbeatEnqueue := newHeartbeatEnqueuer(store, home)
		// Pipeline schedules (#681) run the same way: the scan advances in-flight runs
		// and fires due interval schedules, reusing the normal job queue. Off-by-default
		// (no pipelines => an empty list before any state touch) and skipped under
		// --dry-run for the same reason.
		pipelineEnqueue := newPipelineStageEnqueuer(store, home)
		if !dryRun {
			installDefaultMemoryPipelinesForDaemon(ctx, store, paths, home, stdout)
		}
		for {
			if err := receiveSupervisorWorkerError(workerErr); err != nil {
				return err
			}
			baseInterval := live.pollInterval()
			pollerForTick := poller
			pollerForTick.IdleGraceTicks, pollerForTick.IdleMaxMultiplier = live.idleCadence()
			wait, err := pollRegisteredReposWithPoller(ctx, pollerForTick, schedule, time.Now().UTC(), baseInterval)
			if err != nil {
				return err
			}
			// Per-repo idle decay gates GitHub calls only. Heartbeats, pipelines,
			// chat scans, and other supervisor maintenance still wake at base cadence.
			if wait > baseInterval {
				wait = baseInterval
			}
			if !dryRun {
				if err := runHeartbeatScanOnce(ctx, paths, store, heartbeatEnqueue, time.Now().UTC()); err != nil {
					writeLine(stdout, "heartbeat scan error: %s", err)
				}
				if err := runPipelineScanOnce(ctx, store, pipelineEnqueue, time.Now().UTC()); err != nil {
					writeLine(stdout, "pipeline scan error: %s", err)
				}
				// Chat auto-respond sweep (#534 V1.5). Off-by-default: with
				// [chat].auto_respond unset (or no agent enrolled) it returns before any
				// chat-table query, so the tick hot path is byte-identical.
				if err := runChatAutoRespondScanOnce(ctx, paths, home, store, dispatchLocalAgentJob, time.Now().UTC()); err != nil {
					writeLine(stdout, "chat auto-respond scan error: %s", err)
				}
				// Decouple the pipeline-advance cadence from the repo-poll backoff
				// (#697): `wait` is the poller's cadence, which grows to minutes when
				// repo polling backs off, and it would otherwise throttle the pipeline
				// advancer to that same rate. While any run is in flight, cap the sleep
				// to the configured (non-backed-off) poll interval so settled stages
				// fold promptly. NOTE (#911): the idle-decay clamp above already caps
				// `wait` at the base interval on every pass, so this guard is now a
				// second, narrower ceiling; it still only reduces the sleep, never
				// extends it.
				if inFlight, err := pipelineRunsInFlight(ctx, store); err != nil {
					writeLine(stdout, "pipeline in-flight check error: %s", err)
				} else {
					wait = pipelineAdvanceWait(wait, live.pollInterval(), inFlight)
				}
			}
			if watchSkillOptReviews {
				if _, err := pollSkillOptReviewWatches(ctx, paths, store, blobStore, reviewGitHub, stdout, dryRun, home); err != nil {
					writeLine(stdout, "skillopt review watch poll error: %s", err)
				}
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case err := <-workerErr:
				timer.Stop()
				if err != nil {
					return err
				}
			case <-timer.C:
			}
		}
	})
}

func runSingleRepoSupervisor(ctx context.Context, home string, d daemon.Daemon, store *db.Store, live *daemonReloadableConfig, rootFilter string, stdout io.Writer) error {
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, time.Now().UTC()); err != nil {
		return err
	}
	if err := recoverForeignBootRunners(ctx, store, stdout); err != nil {
		return err
	}
	if err := recoverRunningJobsForRepo(ctx, store, stdout, d.Repo.FullName(), rootFilter); err != nil {
		return err
	}
	if err := recoverCancelledRunningJobsForRepo(ctx, store, stdout, d.Repo.FullName(), rootFilter); err != nil {
		return err
	}
	worker := defaultJobWorker(store, stdout, home)
	worker.CommenterFactory = worker.defaultCommenter
	worker.Admission = worker.loadAdmissionBudget()
	var checkoutLock sync.Mutex
	// The in-flight tracker (#562) is what keeps ONE hung/long job from wedging
	// this whole loop: ticks claim + dispatch async and return, while the tracker
	// preserves same-repo serialization, concurrency caps, and poller exclusion.
	// The deferred drain cancels + waits (bounded) for in-flight jobs on exit.
	tracker := newInflightJobTracker(ctx)
	defer tracker.drain(stdout, daemonShutdownDrainTimeout)
	workerErr := startSingleRepoWorkerLoop(ctx, daemonWorkerLoopInterval, store, worker, live, &checkoutLock, tracker, d.Repo.FullName(), rootFilter, stdout)
	startCockpitReconcileLoop(ctx, store, home, stdout)
	// Heartbeat schedules (#533) must also fire in the single-repo daemon, or a
	// single-repo daemon would silently never run them. Off-by-default: with no
	// heartbeat sections the scan returns before any store touch. A failure to
	// resolve paths only disables heartbeats (logged once); it never aborts the loop.
	heartbeatPaths, heartbeatPathsErr := pathsFromFlag(home)
	if heartbeatPathsErr != nil {
		writeLine(stdout, "heartbeat scan disabled: %s", heartbeatPathsErr)
	}
	heartbeatEnqueue := newHeartbeatEnqueuer(store, home)
	if heartbeatPathsErr == nil {
		// The single-repo daemon gets the same one-per-home sweep owner as the
		// registered-repo supervisor; it is not attached to the per-tick engine.
		startMemoryHarvestLoop(ctx, heartbeatPaths, home, store, stdout)
	}
	// Pipeline schedules (#681) fire in the single-repo daemon too, or a single-repo
	// daemon would silently never advance/schedule pipelines. Off-by-default: with no
	// pipelines the scan returns an empty list before any state touch. Unlike the
	// heartbeat scan it needs no config paths (it reads the DB), so a paths failure
	// does not disable it.
	pipelineEnqueue := newPipelineStageEnqueuer(store, home)
	if heartbeatPathsErr == nil {
		installDefaultMemoryPipelinesForDaemon(ctx, store, heartbeatPaths, home, stdout)
	} else {
		writeLine(stdout, "default memory pipeline install disabled: %s", heartbeatPathsErr)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := receiveSupervisorWorkerError(workerErr); err != nil {
			return err
		}
		// A full PollOnce may mutate the shared checkout (merge retries, task
		// reconciles), so it needs BOTH the checkout lock (excludes the tick, and
		// — because dispatch happens under that lock — excludes NEW jobs starting
		// mid-poll) AND an idle tracker (excludes already in-flight jobs, which
		// the tick no longer holds the lock for while they run, #562). Otherwise
		// fall back to the recovery-command-only poll, exactly as before.
		polledFull := false
		if checkoutLock.TryLock() {
			if !tracker.busy(d.Repo.FullName()) {
				_ = runDaemonPollWithTimeout(ctx, daemonPollTimeout, d.PollOnce)
				polledFull = true
			}
			checkoutLock.Unlock()
		}
		if !polledFull {
			_ = runDaemonPollWithTimeout(ctx, daemonPollTimeout, d.PollRecoveryCommandsOnce)
		}
		if heartbeatPathsErr == nil {
			if err := runHeartbeatScanOnce(ctx, heartbeatPaths, store, heartbeatEnqueue, time.Now().UTC()); err != nil {
				writeLine(stdout, "heartbeat scan error: %s", err)
			}
			// Chat auto-respond sweep (#534 V1.5) needs the same resolved config paths
			// as the heartbeat scan, so gate it on the same paths resolution.
			// Off-by-default: returns before any chat-table query unless enabled.
			if err := runChatAutoRespondScanOnce(ctx, heartbeatPaths, home, store, dispatchLocalAgentJob, time.Now().UTC()); err != nil {
				writeLine(stdout, "chat auto-respond scan error: %s", err)
			}
		}
		if err := runPipelineScanOnce(ctx, store, pipelineEnqueue, time.Now().UTC()); err != nil {
			writeLine(stdout, "pipeline scan error: %s", err)
		}
		// Read the warm-reloadable poll interval each cycle (#577) so a SIGHUP
		// change takes effect on the next tick. Fall back to the historical 30s
		// default if it was somehow left non-positive.
		interval := live.pollInterval()
		if interval <= 0 {
			interval = 30 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-workerErr:
			timer.Stop()
			if err != nil {
				return err
			}
		case <-timer.C:
		}
	}
}

// heartbeatEnqueuer enqueues one heartbeat job. In production it wraps
// workflow.Mailbox.Enqueue; tests inject a fake to assert the request shape (and
// to fail loudly if an off-by-default scan ever enqueues).
type heartbeatEnqueuer func(ctx context.Context, request workflow.JobRequest) (db.Job, error)

// newHeartbeatEnqueuer builds the production enqueuer: a Mailbox bound to the
// store and the daemon's canary-routing policy, matching dispatchLocalAgentJob's
// construction so a heartbeat job is indistinguishable from a normal background
// job once enqueued.
func newHeartbeatEnqueuer(store *db.Store, home string) heartbeatEnqueuer {
	mailbox := workflow.Mailbox{Store: store, CanaryEnabled: canaryRoutingEnabled(home)}
	return func(ctx context.Context, request workflow.JobRequest) (db.Job, error) {
		return mailbox.Enqueue(ctx, request)
	}
}

func heartbeatFingerprint(agent, name string) string {
	return fmt.Sprintf("heartbeat:%s/%s", agent, name)
}

// heartbeatRepoManaged reports whether repo is one the daemon worker tick can
// actually run a job for: registered (a repos row exists), enabled, and with a
// non-empty checkout_path. A heartbeat for any other repo must be skipped (with
// next_due advanced) rather than enqueued into a job no worker will ever claim.
func heartbeatRepoManaged(ctx context.Context, store *db.Store, repo string) (bool, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return false, nil
	}
	record, err := store.GetRepo(ctx, repo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return record.Enabled && strings.TrimSpace(record.CheckoutPath) != "", nil
}

// heartbeatAgentHasCapability reports whether the named agent currently holds the
// given capability (e.g. "review"). A missing agent is NOT an error: it returns
// false so a review heartbeat for an unknown/unstarted agent is skipped rather
// than aborting the whole scan. A real store error propagates.
func heartbeatAgentHasCapability(ctx context.Context, store *db.Store, agent, capability string) (bool, error) {
	record, err := store.GetAgent(ctx, strings.TrimSpace(agent))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return agentHasCapability(record.Capabilities, capability), nil
}

// heartbeatImplementPermitted reports whether the named agent may run an implement
// heartbeat: it must hold the "implement" capability AND carry a write-granting
// autonomy policy (workspace-write / danger-full-access). A missing agent is NOT
// an error — it returns false so an implement heartbeat for an unknown/unstarted
// agent is skipped (and next_due advanced) rather than aborting the scan. It
// reuses the exact runtime predicate the direct-implement dispatch gate uses, so
// the two can never drift. A real store error propagates (#611).
func heartbeatImplementPermitted(ctx context.Context, store *db.Store, agent string) (bool, error) {
	record, err := store.GetAgent(ctx, strings.TrimSpace(agent))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !agentHasCapability(record.Capabilities, "implement") {
		return false, nil
	}
	return runtime.PolicyGrantsImplementWrite(record.AutonomyPolicy), nil
}

func heartbeatJobID(agent, name string, now time.Time) string {
	return fmt.Sprintf("heartbeat-%s-%s-%x", agent, name, now.UTC().UnixNano())
}

// heartbeatJitter returns a uniformly random delay in [0, jitter] so concurrent
// heartbeats with the same interval do not thunder. jitter<=0 returns 0, which
// keeps a no-jitter (the default) schedule deterministic.
func heartbeatJitter(jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(jitter) + 1))
}

// runHeartbeatScanOnce checks every configured heartbeat schedule once and
// enqueues a normal background job for each enabled+due entry (#533), reusing the
// standard job queue: the existing worker tick then runs the job (no new
// execution code).
//
// OFF BY DEFAULT: a config with no [agents.<agent>.heartbeats.<name>] sections
// makes LoadHeartbeats return an empty slice, so this returns nil BEFORE touching
// the store or the enqueuer. It is wired into BOTH supervisor loops; the caller
// logs a scan error and never aborts the loop. A per-heartbeat error is collected
// (first wins) but does not stop the remaining heartbeats.
func runHeartbeatScanOnce(ctx context.Context, paths config.Paths, store *db.Store, enqueue heartbeatEnqueuer, now time.Time) error {
	heartbeats, err := config.LoadHeartbeats(paths)
	if err != nil {
		return err
	}
	if len(heartbeats) == 0 {
		return nil
	}
	agentTypes, err := config.LoadAgentTypes(paths)
	if err != nil {
		return err
	}
	now = now.UTC()
	var firstErr error
	for _, heartbeat := range heartbeats {
		if err := runOneHeartbeat(ctx, store, enqueue, agentTypes, heartbeat, paths.Home, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func runOneHeartbeat(ctx context.Context, store *db.Store, enqueue heartbeatEnqueuer, agentTypes map[string]config.AgentType, heartbeat config.Heartbeat, home string, now time.Time) error {
	if !heartbeat.Enabled {
		return nil
	}
	interval, err := time.ParseDuration(heartbeat.Interval)
	if err != nil {
		return fmt.Errorf("heartbeat %s/%s interval: %w", heartbeat.Agent, heartbeat.Name, err)
	}
	jitter, err := time.ParseDuration(heartbeat.Jitter)
	if err != nil {
		return fmt.Errorf("heartbeat %s/%s jitter: %w", heartbeat.Agent, heartbeat.Name, err)
	}
	state, _, err := store.GetHeartbeatState(ctx, heartbeat.Agent, heartbeat.Name)
	if err != nil {
		return err
	}
	// Not yet due: a zero next_due (the first-ever scan) is in the past, so a fresh
	// heartbeat runs immediately; a future next_due is skipped without advancing.
	if !state.NextDueAt.IsZero() && now.Before(state.NextDueAt) {
		return nil
	}
	// Repo guard: the worker tick only claims jobs for a registered+enabled repo
	// with a checkout. A heartbeat targeting an unmanaged/disabled/uncheckout repo
	// would enqueue a job no worker ever claims, which would then permanently wedge
	// the heartbeat (the zombie 'queued' job trips the overlap guard every tick).
	// So skip the enqueue but ADVANCE next_due (record last_status) so no zombie is
	// created and the heartbeat self-recovers once the repo becomes managed.
	// dispatchLocalAgentJob does an equivalent resolve/upsert; this path bypasses it.
	managed, err := heartbeatRepoManaged(ctx, store, heartbeat.Repo)
	if err != nil {
		return err
	}
	if !managed {
		state.Agent = heartbeat.Agent
		state.Name = heartbeat.Name
		state.LastRunAt = now
		state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
		state.LastStatus = "repo_unmanaged"
		return store.UpsertHeartbeatState(ctx, state)
	}
	// Capability guard: a review heartbeat enqueues an Action="review" job, which
	// the worker only runs for an agent that HOLDS the review capability. Validate
	// here (the enqueue path bypasses ensureLocalAgentAccess) so we never enqueue a
	// review job the agent is not permitted to run. Skip but ADVANCE next_due with a
	// clear status so it self-recovers once the capability is granted (rather than
	// hot-looping or wedging). LoadHeartbeats already rejects any action other than
	// ask/review at config-load; this is the agent-aware half of that check.
	if heartbeat.Action == "review" {
		hasReview, err := heartbeatAgentHasCapability(ctx, store, heartbeat.Agent, "review")
		if err != nil {
			return err
		}
		if !hasReview {
			state.Agent = heartbeat.Agent
			state.Name = heartbeat.Name
			state.LastRunAt = now
			state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
			state.LastStatus = "capability_missing"
			return store.UpsertHeartbeatState(ctx, state)
		}
	}
	// Policy gate: an implement heartbeat enqueues a WRITE job. The worker only
	// produces files for an agent that holds the implement capability AND carries a
	// write-granting autonomy policy (workspace-write / danger-full-access); under
	// auto/read-only the job runs and produces nothing (and is separately blocked by
	// readOnlyImplementationBlocked at dispatch). Validate here — the enqueue path
	// bypasses ensureLocalAgentAccess — so an implement heartbeat for a read-only
	// agent NO-OPs rather than churning doomed jobs. Skip but ADVANCE next_due with a
	// clear status so it self-recovers once a write policy is granted. This is the
	// agent-aware half of the config-level action gate (#611).
	if heartbeat.Action == "implement" {
		permitted, err := heartbeatImplementPermitted(ctx, store, heartbeat.Agent)
		if err != nil {
			return err
		}
		if !permitted {
			state.Agent = heartbeat.Agent
			state.Name = heartbeat.Name
			state.LastRunAt = now
			state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
			state.LastStatus = "policy_readonly"
			return store.UpsertHeartbeatState(ctx, state)
		}
	}
	// Overlap protection: a still-active heartbeat job (>= max_concurrent) means the
	// previous run has not finished. Skip WITHOUT advancing so it is retried next
	// tick (this is also the restart-safe dedup: a restart sees the active job).
	fingerprint := heartbeatFingerprint(heartbeat.Agent, heartbeat.Name)
	active, err := store.CountActiveJobsByFingerprint(ctx, fingerprint)
	if err != nil {
		return err
	}
	if active >= heartbeat.MaxConcurrent {
		return nil
	}
	// Respect agent capacity: when the agent is already at its max_background, skip
	// this tick WITHOUT advancing so the heartbeat is retried once capacity frees up
	// rather than being silently dropped for a full interval.
	if agentType, ok := agentTypes[heartbeat.Agent]; ok && agentType.MaxBackground > 0 {
		busy, err := store.AgentActiveJobCount(ctx, heartbeat.Agent)
		if err != nil {
			return err
		}
		if busy >= agentType.MaxBackground {
			return nil
		}
	}
	// Per-heartbeat runtime override (#611): when the heartbeat names a runtime,
	// resolve it through the same seam the per-job --runtime override uses (#531) —
	// it validates the runtime and mints a FRESH session ref so the scheduled job
	// neither resumes nor writes the agent's default-runtime session. An empty
	// heartbeat runtime yields ("", "") and the job runs on the agent default,
	// byte-identical to a pre-#611 heartbeat.
	overrideRuntime, overrideRef, overrideErr := resolveJobRuntimeOverride(heartbeat.Runtime, "")
	if overrideErr != nil {
		// A bad runtime override is a config error; skip but ADVANCE next_due so a
		// broken heartbeat does not hot-loop, and self-recovers once corrected.
		state.Agent = heartbeat.Agent
		state.Name = heartbeat.Name
		state.LastRunAt = now
		state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
		state.LastStatus = "runtime_invalid"
		if err := store.UpsertHeartbeatState(ctx, state); err != nil {
			return err
		}
		return overrideErr
	}
	// Implement heartbeats need the SAME isolated task/branch/worktree the direct
	// `agent implement` path allocates (#611). Without it the enqueued job carries
	// Branch="",TaskID="",WorktreePath="" and the daemon worker fails its checkout
	// pre-flight ("checkout branch is main, not job branch ") on the shared checkout —
	// a false-green that never runs the agent, creates a branch, or opens a PR. Do the
	// allocation here (AFTER the overlap/capacity guards so a skipped tick allocates
	// nothing) so taskWorktreeCheckout resolves the on-branch worktree and
	// validateTargetCheckout passes, exactly like a foreground implement. Read-only
	// actions (ask/review) carry no branch identity and keep their bare-enqueue path.
	var implementFields heartbeatImplementFields
	if heartbeat.Action == "implement" {
		implementFields, err = allocateHeartbeatImplement(ctx, store, home, heartbeat)
		if err != nil {
			// Allocation failure (e.g. a dirty checkout or a taken branch) is handled
			// like an enqueue failure: skip but ADVANCE next_due with a clear status so a
			// broken implement heartbeat does not hot-loop, and self-recovers next tick.
			state.Agent = heartbeat.Agent
			state.Name = heartbeat.Name
			state.LastRunAt = now
			state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
			state.LastStatus = "implement_alloc_failed"
			if upsertErr := store.UpsertHeartbeatState(ctx, state); upsertErr != nil {
				return upsertErr
			}
			return err
		}
	}
	job, enqueueErr := enqueue(ctx, workflow.JobRequest{
		ID:                 heartbeatJobID(heartbeat.Agent, heartbeat.Name, now),
		Agent:              heartbeat.Agent,
		Action:             heartbeat.Action,
		Repo:               heartbeat.Repo,
		Branch:             implementFields.Branch,
		TaskID:             implementFields.TaskID,
		TaskTitle:          implementFields.TaskTitle,
		GoalID:             implementFields.GoalID,
		HeadSHA:            implementFields.HeadSHA,
		Sender:             "heartbeat",
		Instructions:       heartbeat.Prompt,
		Fingerprint:        fingerprint,
		RuntimeOverride:    overrideRuntime,
		RuntimeOverrideRef: overrideRef,
	})
	// Advance exactly one interval whether or not the enqueue succeeded. Anchoring
	// next_due to `now` (not the old due time) coalesces missed ticks into a single
	// run (no backlog replay), and advancing on failure (recording last_status)
	// stops a broken heartbeat from hot-looping every tick.
	state.Agent = heartbeat.Agent
	state.Name = heartbeat.Name
	state.LastRunAt = now
	state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
	if enqueueErr != nil {
		state.LastStatus = "enqueue_failed"
	} else {
		state.LastJobID = job.ID
		state.LastStatus = "enqueued"
	}
	if err := store.UpsertHeartbeatState(ctx, state); err != nil {
		return err
	}
	return enqueueErr
}

// heartbeatImplementFields is the task/branch/worktree identity an implement
// heartbeat job must carry so the daemon worker's checkout pre-flight
// (taskWorktreeCheckout + validateTargetCheckout) resolves the freshly allocated
// on-branch worktree and passes — the exact set the direct `agent implement` path
// stamps onto its JobRequest (#611).
type heartbeatImplementFields struct {
	Branch    string
	TaskID    string
	TaskTitle string
	GoalID    string
	HeadSHA   string
}

// allocateHeartbeatImplement performs the SAME task/branch/worktree allocation the
// direct `agent implement` dispatch does (prepareLocalImplementDispatchRequest →
// workflow.Engine.AllocateTaskWorktree): it upserts a fresh adhoc task on a
// gitmoot/<taskID> branch and adds an isolated git worktree checked out on that
// branch, returning the identity fields the enqueued job needs. It reuses the
// direct path verbatim so the scheduled and foreground implement flows can never
// drift. It uses the STORED repo record (whose DefaultBranch is the allocation
// base) rather than re-deriving the base from the possibly-off-branch shared
// checkout (#611).
func allocateHeartbeatImplement(ctx context.Context, store *db.Store, home string, heartbeat config.Heartbeat) (heartbeatImplementFields, error) {
	repo, err := daemon.ParseRepository(heartbeat.Repo)
	if err != nil {
		return heartbeatImplementFields{}, err
	}
	record, err := store.GetRepo(ctx, heartbeat.Repo)
	if err != nil {
		return heartbeatImplementFields{}, err
	}
	_, prepared, err := prepareLocalImplementDispatchRequest(ctx, store, record, repo, localAgentDispatchRequest{
		RepoFlag:     heartbeat.Repo,
		Agent:        heartbeat.Agent,
		Action:       "implement",
		Instructions: heartbeat.Prompt,
		Home:         home,
	})
	if err != nil {
		return heartbeatImplementFields{}, err
	}
	return heartbeatImplementFields{
		Branch:    prepared.Branch,
		TaskID:    prepared.TaskID,
		TaskTitle: prepared.TaskTitle,
		GoalID:    prepared.GoalID,
		HeadSHA:   prepared.HeadSHA,
	}, nil
}

// startSingleRepoWorkerLoop wires the single-repo supervisor's per-tick worker
// closure — the checkout-lock discipline, the warm-reloadable worker/scheduler
// snapshot (#577), and the tracked worker tick (#562) — and starts it on the
// recovering loop. It is the ONE production wiring runSingleRepoSupervisor
// uses; the #562 wedge E2E drives this exact function so the tested loop can
// never drift from the deployed one.
func startSingleRepoWorkerLoop(ctx context.Context, interval time.Duration, store *db.Store, worker jobWorker, live *daemonReloadableConfig, checkoutLock *sync.Mutex, tracker *inflightJobTracker, repo string, rootFilter string, stdout io.Writer) <-chan error {
	return startSupervisorWorkerLoopRecovering(ctx, interval, stdout, func(now time.Time) error {
		// The checkout lock now guards the TICK (maintenance + claim/dispatch),
		// not whole job runs: dispatched jobs execute on their own goroutines
		// after this returns, tracked by the in-flight tracker (#562). Holding it
		// across dispatch is still what makes the poller's full-PollOnce gate
		// race-free (no new checkout-occupying job can start mid-poll).
		checkoutLock.Lock()
		defer checkoutLock.Unlock()
		// Read the warm-reloadable worker count + scheduler mode each tick (#577);
		// the worker is copied per tick so UsePool is race-free with the SIGHUP
		// reload goroutine.
		_, workers, usePool := live.snapshot()
		w := worker
		w.UsePool = usePool
		// nil carrier: this single-repo supervisor tick self-computes the shared
		// candidate sets once for its own tick (#619).
		return runDaemonWorkerTickTracked(ctx, store, w, workers, false, repo, rootFilter, stdout, now, tracker, nil)
	})
}

func startSupervisorWorkerLoop(ctx context.Context, interval time.Duration, run func(time.Time) error) <-chan error {
	return startSupervisorWorkerLoopInternal(ctx, interval, nil, run, false)
}

func startSupervisorWorkerLoopRecovering(ctx context.Context, interval time.Duration, stdout io.Writer, run func(time.Time) error) <-chan error {
	return startSupervisorWorkerLoopInternal(ctx, interval, stdout, run, true)
}

// maxConsecutiveWorkerTickFailures bounds how many CONSECUTIVE recovering
// worker-tick failures the supervisor tolerates before it stops retrying and
// surfaces the error on its channel so the caller (the daemon) exits and
// systemd restarts/alerts. gaijinjoe's #555 made a single transient tick error
// survivable (the daemon no longer wedges), but retry-forever turned a
// PERMANENT infra fault — job-level failures already return nil, so what bubbles
// up here is disk-full / corrupt-or-locked SQLite / a failed migration — into a
// silent false-green daemon: status still "running", zero progress, ~86k log
// lines/day. The streak resets to 0 on any successful tick, so only a genuinely
// stuck daemon escalates. At the capped backoff below this spans a few minutes
// before escalating (1+2+4+8+16 then 30s each ≈ 4m for a 1s base interval).
const maxConsecutiveWorkerTickFailures = 12

// maxWorkerTickBackoff caps the exponential retry sleep applied to a persistent
// recovering-loop fault, so a permanent error backs off to the poll cadence
// instead of spinning at the 1s base interval and flooding the journal.
const maxWorkerTickBackoff = 30 * time.Second

func startSupervisorWorkerLoopInternal(ctx context.Context, interval time.Duration, stdout io.Writer, run func(time.Time) error, recoverErrors bool) <-chan error {
	errCh := make(chan error, 1)
	if interval <= 0 {
		interval = daemonWorkerLoopInterval
	}
	go func() {
		defer close(errCh)
		consecutiveFailures := 0
		for {
			if err := ctx.Err(); err != nil {
				return
			}
			if err := run(time.Now().UTC()); err != nil {
				// A cancellation observed mid-tick is a clean shutdown, never a
				// fault: return WITHOUT logging or counting it toward the
				// escalation streak, so graceful shutdown stays quiet and never
				// pushes context.Canceled onto errCh.
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return
				}
				if !recoverErrors {
					errCh <- err
					return
				}
				consecutiveFailures++
				// A PERSISTENT fault (a bounded streak of consecutive tick
				// errors) is infra-level, not a transient blip: escalate it so
				// the daemon exits instead of spinning silently forever.
				if consecutiveFailures >= maxConsecutiveWorkerTickFailures {
					writeLine(stdout, "daemon worker tick error: %v; %d consecutive failures, escalating", err, consecutiveFailures)
					errCh <- err
					return
				}
				writeLine(stdout, "daemon worker tick error: %v; retrying (%d/%d)", err, consecutiveFailures, maxConsecutiveWorkerTickFailures)
				if sleepErr := sleepSupervisorWorkerLoop(ctx, workerTickBackoff(interval, consecutiveFailures)); sleepErr != nil {
					return
				}
				continue
			}
			// A successful tick clears the streak: only CONSECUTIVE failures
			// escalate, so one bad pass between good ones never trips the ceiling.
			consecutiveFailures = 0
			if err := sleepSupervisorWorkerLoop(ctx, interval); err != nil {
				return
			}
		}
	}()
	return errCh
}

// workerTickBackoff returns the retry sleep for the Nth consecutive recovering
// worker-tick failure: the base interval doubled per failure, capped at
// maxWorkerTickBackoff. consecutiveFailures==1 returns the base interval (his
// original single-transient-error cadence is preserved).
func workerTickBackoff(base time.Duration, consecutiveFailures int) time.Duration {
	if base <= 0 {
		base = daemonWorkerLoopInterval
	}
	backoff := base
	for i := 1; i < consecutiveFailures; i++ {
		if backoff >= maxWorkerTickBackoff {
			return maxWorkerTickBackoff
		}
		backoff *= 2
	}
	if backoff > maxWorkerTickBackoff {
		return maxWorkerTickBackoff
	}
	return backoff
}

func sleepSupervisorWorkerLoop(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func receiveSupervisorWorkerError(errCh <-chan error) error {
	if errCh == nil {
		return nil
	}
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// startCockpitReconcileLoop runs the low-frequency cockpit reconcile GC in the
// background until ctx is cancelled (Task 7). Each tick drops cockpit_pane rows
// whose herdr pane is gone AND whose owning root is terminal, complementing the
// per-Deliver / root-finalize teardown and report-metadata --ttl-ms self-expiry.
// It is entirely best-effort: it is gated on herdr availability (so a host without
// herdr never sweeps), uses the auto-policy cockpit, and swallows every error. It
// never blocks the supervisor's poll/worker loops. A policy load failure or a
// disabled cockpit simply skips the sweep.
func startCockpitReconcileLoop(ctx context.Context, store *db.Store, home string, stdout io.Writer) {
	worker := defaultJobWorker(store, stdout, home)
	go func() {
		ticker := time.NewTicker(cockpitReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcileCockpitPanesOnce(ctx, worker)
			}
		}
	}()
}

// reconcileCockpitPanesOnce performs one best-effort cockpit reconcile sweep. It
// builds the cockpit from the host orchestrate policy, skips when the cockpit is
// disabled or herdr is unreachable, and otherwise asks cockpit.Reconcile to drop
// orphaned rows (pane gone + root terminal). All errors are swallowed.
func reconcileCockpitPanesOnce(ctx context.Context, worker jobWorker) {
	policy, err := worker.orchestratePolicy()
	if err != nil || policy.CockpitMode == config.CockpitModeOff {
		return
	}
	cp := worker.newCockpit(policy)
	if cp == nil || !cp.Available(ctx) {
		return
	}
	cp.Reconcile(ctx, func(rootJobID string) bool {
		terminal, terr := worker.rootTreeTerminal(ctx, rootJobID)
		return terr == nil && terminal
	})
}

type repoCheckoutLocks struct {
	locks sync.Map
}

func (l *repoCheckoutLocks) For(repo string) *sync.Mutex {
	if l == nil {
		return nil
	}
	value, _ := l.locks.LoadOrStore(repo, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func pollRegisteredRepos(ctx context.Context, store *db.Store, workers int, dryRun bool, stdout io.Writer, nextPoll map[string]time.Time, now time.Time, fallbackPoll time.Duration) (time.Duration, error) {
	return pollRegisteredReposWithPoller(ctx, defaultRegisteredRepoPoller(store, workers, dryRun, stdout, "", ""), registeredRepoSchedule{NextPoll: nextPoll}, now, fallbackPoll)
}

type registeredRepoSchedule struct {
	NextPoll    map[string]time.Time
	ErrorStreak map[string]int
	IdleStreak  map[string]int
}

func (s registeredRepoSchedule) ensure() registeredRepoSchedule {
	if s.NextPoll == nil {
		s.NextPoll = map[string]time.Time{}
	}
	if s.ErrorStreak == nil {
		s.ErrorStreak = map[string]int{}
	}
	if s.IdleStreak == nil {
		s.IdleStreak = map[string]int{}
	}
	return s
}

type registeredRepoPoller struct {
	Store                  *db.Store
	Workers                int
	DryRun                 bool
	Stdout                 io.Writer
	RecoveryOnly           bool
	WatchIssues            bool
	EscalationTTL          time.Duration
	RevertDetectionEnabled bool
	CheckoutLocks          *repoCheckoutLocks
	// Inflight is the supervisor's in-flight job tracker (#562). A repo with
	// dispatched jobs still running gets the recovery-command-only poll: a full
	// PollOnce may mutate the shared checkout, which used to be excluded by the
	// per-repo lock being held for entire job runs. nil (legacy/test callers)
	// behaves as always-idle.
	Inflight          *inflightJobTracker
	IdleGraceTicks    int
	IdleMaxMultiplier int
	GitHubClient      func(checkout string) github.Client
	WorkflowFactory   func(store *db.Store, gh github.Client, checkout string) *workflow.Engine
}

// defaultRegisteredRepoPoller wires the registered-repo supervisor's per-tick
// poller. It takes TWO home values with DISTINCT, documented shapes (#459):
//
//   - rawHome: the RAW --home (NOT <home>/.gitmoot). It feeds the READ-ONLY policy
//     loaders — resolveEscalationTTL and jobWorker.ConfigHome (orchestratePolicy) —
//     each of which resolves it to the config.toml exactly once. Passing a resolved
//     root here would re-append ".gitmoot" inside those loaders and create the
//     phantom <home>/.gitmoot/.gitmoot.
//   - resolvedRoot: the already-resolved <home>/.gitmoot root (config.Paths.Home).
//     It feeds the engine wiring — daemonWorkflowEngine's ArtifactRoot/Home and
//     daemonEventSink — which expect the resolved root and do NOT re-resolve.
//
// The legacy/test caller (pollRegisteredRepos) passes "" for both, which is a
// no-op: resolveEscalationTTL("") returns the default and daemonWorkflowEngine("")
// leaves ArtifactRoot/Home/EventSink unset.
func defaultRegisteredRepoPoller(store *db.Store, workers int, dryRun bool, stdout io.Writer, rawHome, resolvedRoot string) registeredRepoPoller {
	return registeredRepoPoller{
		Store:                  store,
		Workers:                workers,
		DryRun:                 dryRun,
		Stdout:                 stdout,
		EscalationTTL:          resolveEscalationTTL(rawHome),
		RevertDetectionEnabled: resolveRevertDetectionEnabled(rawHome),
		IdleGraceTicks:         config.DefaultDaemonIdleGraceTicks,
		IdleMaxMultiplier:      config.DefaultDaemonIdleMaxMultiplier,
		GitHubClient:           func(checkout string) github.Client { return github.NewClient(checkout) },
		WorkflowFactory: func(store *db.Store, gh github.Client, checkout string) *workflow.Engine {
			engine := daemonWorkflowEngine(store, gh, checkout, resolvedRoot)
			// Apply only the escalate_human notifier handle from policy (#340),
			// keeping the budget/inlining knobs out of this path so its existing
			// behavior is unchanged. The notifier itself is already wired by
			// daemonWorkflowEngine; this just sets the configured @-handle.
			// orchestratePolicy reads via jobWorker.ConfigHome, which is ALWAYS the
			// RAW --home (#459) so it never re-resolves into a phantom doubled home.
			if notifier, ok := engine.EscalationNotifier.(*daemonEscalationNotifier); ok && notifier != nil {
				if policy, err := defaultJobWorker(store, stdout, rawHome).orchestratePolicy(); err == nil {
					notifier.Handle = policy.EscalationHandle
				}
			}
			return &engine
		},
	}
}

// resolveEscalationTTL reads the [orchestrate].escalation_ttl policy (#340),
// falling back to DefaultEscalationTTL when unset and to 0 (scan disabled) only
// on a hard parse failure, so the auto-finalize backstop is on by default.
//
// It is READ-ONLY and shape-tolerant (#459): it resolves the config.toml for
// EITHER a raw --home or an already-resolved <home>/.gitmoot root via
// resolveConfigFile, then LoadOrchestratePolicy (which only ReadFile-s, never
// MkdirAll-s). It MUST NOT call initializedPaths/config.Initialize: the real home
// is already initialized upstream by withStore/withStoreAndPaths, and Initializing
// here on a resolved root would re-append ".gitmoot" and create the phantom
// <home>/.gitmoot/.gitmoot. Being side-effect-free makes it phantom-free even if a
// caller hands it the resolved root by mistake — defense in depth.
func resolveEscalationTTL(home string) time.Duration {
	policy := config.DefaultOrchestratePolicy()
	if cfg := resolveConfigFile(home); cfg != "" {
		if loaded, err := config.LoadOrchestratePolicy(config.Paths{ConfigFile: cfg}); err == nil {
			policy = loaded
		}
	}
	raw := strings.TrimSpace(policy.EscalationTTL)
	if raw == "" {
		raw = config.DefaultEscalationTTL
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl <= 0 {
		return 0
	}
	return ttl
}

// resolveBlockedTTL reads the [orchestrate].blocked_ttl policy (#631) and returns
// the blocked-job sweep window, or 0 when the sweep is DISABLED. Unlike
// resolveEscalationTTL it has NO default fallback: an unset/empty (or zero, or
// unparseable) value resolves to 0 so the sweep stays OFF by default — a blocked
// job is a human-awaiting decision and is never auto-dismissed unless the operator
// opted in with a positive duration. It mirrors resolveEscalationTTL's read-only,
// shape-tolerant config resolution (resolveConfigFile + LoadOrchestratePolicy,
// never config.Initialize) so it is phantom-free for either a raw --home or an
// already-resolved <home>/.gitmoot root.
func resolveBlockedTTL(home string) time.Duration {
	policy := config.DefaultOrchestratePolicy()
	if cfg := resolveConfigFile(home); cfg != "" {
		if loaded, err := config.LoadOrchestratePolicy(config.Paths{ConfigFile: cfg}); err == nil {
			policy = loaded
		}
	}
	raw := strings.TrimSpace(policy.BlockedTTL)
	if raw == "" {
		return 0
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl <= 0 {
		return 0
	}
	return ttl
}

func pollRegisteredReposWithPoller(ctx context.Context, poller registeredRepoPoller, schedule registeredRepoSchedule, now time.Time, fallbackPoll time.Duration) (time.Duration, error) {
	schedule = schedule.ensure()
	repos, err := poller.Store.ListRepos(ctx)
	if err != nil {
		return fallbackPoll, err
	}
	enabled := 0
	polled := 0
	decayed := 0
	wait := fallbackPoll
	waitSet := false
	for _, repoRecord := range repos {
		if !repoRecord.Enabled {
			// Drop any idle streak so a disabled repo re-earns its grace ticks
			// when re-enabled instead of resuming a stale decayed cadence.
			delete(schedule.IdleStreak, repoRecord.FullName())
			continue
		}
		enabled++
		fullName := repoRecord.FullName()
		interval := repoPollInterval(repoRecord.PollInterval, fallbackPoll)
		// Local promotion is a ONE-SHOT exit from idle decay: it applies only
		// to a repo that actually decayed (IdleStreak > 0) and is not in error
		// backoff. Without the streak guard a busy repo would re-poll on every
		// supervisor tick instead of at its configured interval, and without
		// the error guard queued local jobs would override repoBackoffInterval
		// and hammer a failing API at base cadence.
		promoted := false
		if schedule.IdleStreak[fullName] > 0 && schedule.ErrorStreak[fullName] == 0 {
			queued, err := poller.Store.CountQueuedJobsForRepo(ctx, fullName)
			if err != nil {
				return wait, err
			}
			if queued > 0 || poller.Inflight.busy(fullName) {
				promoted = true
				delete(schedule.IdleStreak, fullName)
			}
		}
		dueAt := schedule.NextPoll[fullName]
		if !promoted && !dueAt.IsZero() && dueAt.After(now) {
			if repoIdleMultiplier(schedule.IdleStreak[fullName], poller.IdleGraceTicks, poller.IdleMaxMultiplier) > 1 {
				decayed++
			}
			wait = shorterWait(wait, dueAt.Sub(now), &waitSet)
			continue
		}
		polled++
		result, err := poller.pollRepo(ctx, repoRecord, now)
		if err != nil {
			return wait, err
		}
		nextInterval := interval
		if result.LastError != "" {
			schedule.ErrorStreak[fullName]++
			delete(schedule.IdleStreak, fullName)
			nextInterval = repoBackoffInterval(interval, schedule.ErrorStreak[fullName])
		} else {
			delete(schedule.ErrorStreak, fullName)
			queued, err := poller.Store.CountQueuedJobsForRepo(ctx, fullName)
			if err != nil {
				return wait, err
			}
			idle := result.Conditional.Calls > 0 && result.Conditional.Misses == 0 &&
				!poller.Inflight.busy(fullName) && queued == 0
			if idle {
				schedule.IdleStreak[fullName]++
			} else {
				delete(schedule.IdleStreak, fullName)
			}
			multiplier := repoIdleMultiplier(schedule.IdleStreak[fullName], poller.IdleGraceTicks, poller.IdleMaxMultiplier)
			if multiplier > 1 {
				decayed++
				nextInterval = interval * time.Duration(multiplier)
			}
		}
		schedule.NextPoll[fullName] = now.Add(nextInterval)
		wait = shorterWait(wait, nextInterval, &waitSet)
	}
	writeLine(poller.Stdout, "supervised %d enabled repos, polled %d, decayed %d", enabled, polled, decayed)
	if wait <= 0 {
		wait = fallbackPoll
	}
	return wait, nil
}

type registeredRepoPollResult struct {
	LastError   string
	Conditional github.ConditionalRequestStats
}

type conditionalRequestStatsProvider interface {
	ConditionalRequestStats() github.ConditionalRequestStats
}

func (p registeredRepoPoller) pollRepo(ctx context.Context, repoRecord db.Repo, now time.Time) (registeredRepoPollResult, error) {
	store := p.Store
	repo, err := daemon.ParseRepository(repoRecord.FullName())
	if err != nil {
		lastError := err.Error()
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), lastError)
		return registeredRepoPollResult{LastError: lastError}, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), now.Format(time.RFC3339), lastError)
	}
	lastPollAt := now.Format(time.RFC3339)
	if strings.TrimSpace(repoRecord.CheckoutPath) == "" {
		message := "registered repo has no checkout path"
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), message)
		return registeredRepoPollResult{LastError: message}, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, message)
	}
	writeLine(p.Stdout, "polling %s with %d workers dry_run=%t", repoRecord.FullName(), p.Workers, p.DryRun)
	if p.DryRun {
		return registeredRepoPollResult{}, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, "")
	}
	gh := p.GitHubClient(repoRecord.CheckoutPath)
	engine := p.WorkflowFactory(store, gh, repoRecord.CheckoutPath)
	recoveryOnly := p.RecoveryOnly
	if lock := p.CheckoutLocks.For(repoRecord.FullName()); lock != nil {
		if lock.TryLock() {
			defer lock.Unlock()
		} else {
			recoveryOnly = true
		}
	}
	// In-flight gate (#562): jobs no longer run under the per-repo lock, so a
	// held lock alone can't prove the checkout is quiet. While this repo has
	// dispatched jobs still running, degrade to the recovery-only poll — the
	// same exclusion inline execution used to give. Checked AFTER TryLock: new
	// checkout-occupying jobs dispatch only under the lock we now hold, so an
	// idle verdict cannot be raced by a fresh dispatch mid-poll.
	if p.Inflight.busy(repoRecord.FullName()) {
		recoveryOnly = true
	}
	d := daemon.Daemon{
		Repo:                   repo,
		Store:                  store,
		GitHub:                 gh,
		Workflow:               engine,
		WatchIssues:            p.WatchIssues,
		EscalationTTL:          p.EscalationTTL,
		RevertDetectionEnabled: p.RevertDetectionEnabled,
	}
	// Bound the poll the same way the single-repo supervisor does (#555 / #536):
	// this call runs while HOLDING the per-repo checkout lock (deferred Unlock
	// above), so a wedged PollOnce would hold that lock forever and freeze this
	// repo's worker ticks — the exact stall #555 targets. The timeout only
	// bounds the poll; the lock's per-repo checkout semantics are unchanged
	// because Unlock still runs via defer once the (now-bounded) poll returns.
	if recoveryOnly {
		err = runDaemonPollWithTimeout(ctx, daemonPollTimeout, d.PollRecoveryCommandsOnce)
	} else {
		err = runDaemonPollWithTimeout(ctx, daemonPollTimeout, d.PollOnce)
	}
	lastError := ""
	if err != nil {
		lastError = err.Error()
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), lastError)
	}
	result := registeredRepoPollResult{LastError: lastError}
	if provider, ok := gh.(conditionalRequestStatsProvider); ok {
		result.Conditional = provider.ConditionalRequestStats()
	}
	return result, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, lastError)
}

func repoIdleMultiplier(streak, graceTicks, maxMultiplier int) int {
	if maxMultiplier <= 0 {
		maxMultiplier = config.DefaultDaemonIdleMaxMultiplier
	}
	if maxMultiplier <= 1 {
		return 1
	}
	if graceTicks <= 0 {
		graceTicks = config.DefaultDaemonIdleGraceTicks
	}
	if streak < graceTicks {
		return 1
	}
	if streak == graceTicks {
		if maxMultiplier < 2 {
			return maxMultiplier
		}
		return 2
	}
	return maxMultiplier
}

func repoBackoffInterval(base time.Duration, streak int) time.Duration {
	if streak <= 0 {
		return base
	}
	maxBackoff := base * 8
	if maxBackoff < 5*time.Minute {
		maxBackoff = 5 * time.Minute
	}
	backoff := base
	for i := 0; i < streak; i++ {
		if backoff >= maxBackoff/2 {
			return maxBackoff
		}
		backoff *= 2
	}
	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}

func repoPollInterval(value string, fallback time.Duration) time.Duration {
	interval, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || interval <= 0 {
		return fallback
	}
	return interval
}

func shorterWait(current time.Duration, candidate time.Duration, set *bool) time.Duration {
	if candidate <= 0 {
		return current
	}
	if !*set || candidate < current {
		*set = true
		return candidate
	}
	return current
}

type jobWorker struct {
	Store  *db.Store
	Stdout io.Writer
	// ConfigHome is ALWAYS the RAW --home (never the resolved <home>/.gitmoot
	// root) — INVARIANT (#459). The read-only policy loaders below
	// (orchestratePolicy/parallelSessionPolicy/admissionPolicy via configPaths())
	// resolve it through pathsFromFlag -> PathsForHome, which appends ".gitmoot"
	// exactly once. Passing the already-resolved root here would append it a SECOND
	// time and read a phantom <home>/.gitmoot/.gitmoot/config.toml. workflowHome()
	// likewise resolves ConfigHome once to the engine's resolved root. The loaders
	// are side-effect-free (no config.Initialize), so even a mistaken resolved-root
	// ConfigHome can never MkdirAll the phantom — but every construction site must
	// still pass the raw --home so the config is actually found.
	ConfigHome         string
	ConfigHomeExplicit bool
	AdapterFactory     func(runtime.Agent, string) (workflow.DeliveryAdapter, error)
	// OutputAdapterFactory rebuilds a production runtime adapter around the one
	// shared live-output writer used by pipeline progress and cockpit. Tests that
	// inject an opaque fake AdapterFactory may leave this nil and still exercise
	// elapsed-only progress without replacing their fake.
	OutputAdapterFactory func(runtime.Agent, string, io.Writer) (workflow.DeliveryAdapter, error)
	StartAdapterFactory  func(string, string) (runtime.Adapter, error)
	CheckoutValidator    func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error)
	WorkflowFactory      func(string) workflow.Engine
	CommenterFactory     func(string) github.Client
	// UsePool selects the opt-in continuous worker-pool scheduler (#394,
	// --scheduler=pool) over the default per-tick wg.Wait() barrier.
	UsePool bool
	// Admission is the opt-in, host-global memory-aware concurrency budget (#365)
	// the scheduler consults before dispatching each session job. nil means the
	// feature is OFF (no [admission] config) ⇒ scheduling is byte-identical to a
	// build without admission accounting. The supervisors attach it at startup;
	// it is a shared pointer across all per-repo dispatch passes so the cap is
	// process-global (host-global for the normal single-daemon deployment).
	Admission *admissionBudget
	// EventSinkOverride lets a test inject a recording events.Sink (#446) without
	// a config file / webhook. When nil (production), eventSink() resolves the
	// shared process-global webhook sink from [events] config instead.
	EventSinkOverride events.Sink
	// RelayServer is the daemon's #732 chat relay. When non-nil AND a job payload is
	// a `gitmoot moot` seat (payload.MootSeat), run() mints a per-seat token on it and
	// injects GITMOOT_CHAT_RELAY[_AUTH] into the seat's runtime subprocess so the
	// sandboxed seat's `gitmoot chat send/wait` routes through the (unsandboxed)
	// daemon. nil (foreground CLI, and every non-daemon construction) means no relay
	// injection — the job's adapter is byte-identical to pre-#732.
	RelayServer *chatRelayServer
	// AuthProbe is the injected doctor-style live credential probe (#532 slice B).
	// It gates re-dispatch of a runtime_auth deferral: once the coarse hold elapses
	// the scheduler only releases the job when the probe reports the credential is
	// VALID again (an Invalid verdict extends the hold WITHOUT burning a retry
	// attempt; an Unknown/transient verdict falls back to the coarse cadence). When
	// nil (foreground CLI, and every construction that does not opt in) the gate is
	// byte-identical to slice A: the coarse cadence alone governs re-dispatch. Tests
	// inject a fake verdict; the daemon wires defaultAuthProbe (a bounded
	// runtime.ClaudeLiveCheck for claude agents, Unknown for other runtimes).
	AuthProbe func(context.Context, db.Job, workflow.JobPayload) authProbeVerdict
	// SandboxProbe is the cached host capability check used only for Claude/Kimi
	// produce stages. nil selects sandbox.SandboxProbe; tests inject deterministic
	// supported/unsupported results without depending on the test binary's argv.
	SandboxProbe func() sandbox.ProbeResult
	// Progress timing seams keep unit/E2E tests deterministic and short. Zero/nil
	// values select the package defaults and real timer implementation.
	ProgressThreshold  time.Duration
	ProgressInterval   time.Duration
	ProgressTickSource func(context.Context, time.Duration, time.Duration) <-chan time.Time
}

// eventSink resolves the best-effort outbound event Sink (#446) for the
// worker's home, or nil when [events] is OFF (the default). It is the seam
// finishQueuedJob / handleRunJobError use to emit the DAEMON-owned terminal
// cases (pre-flight queued->failed/blocked and permission-blocked
// running->blocked) that never pass through the engine's Mailbox chokepoint. The
// underlying webhook sink is a process-global singleton, so this is a cheap
// cache hit on the hot path. A test override short-circuits config resolution.
func (w jobWorker) eventSink() events.Sink {
	if w.EventSinkOverride != nil {
		return w.EventSinkOverride
	}
	return daemonEventSink(w.Store, w.workflowHome())
}

const daemonRunningJobStaleAfter = 30 * time.Minute
const defaultDaemonRunningJobStaleAfter = daemonRunningJobStaleAfter

// daemonRunningJobStaleFloor is the smallest GITMOOT_STALE_RUNNING_AFTER we
// honor. The stale-running window is a CRASH BACKSTOP, not a job timeout: it is
// how long a job may sit in 'running' with no lease progress before the daemon
// assumes the worker died and requeues it. A tiny value (e.g. 1s) turns that
// backstop into an aggressive killer — especially for NON-resumable runtimes
// (shell/no runtime-session lock) where runtimeOwnerLeaseHeld is always false,
// so there is no lease to protect a live worker and the coarse threshold is the
// ONLY gate. Sub-floor values are rejected in favor of the default (#560).
const daemonRunningJobStaleFloor = 1 * time.Minute

// runtimeLeaseTeardownGrace is added to a job's timeout when sizing its
// runtime-session lock lease so the lease strictly OUTLIVES the run-context
// deadline plus worst-case terminal teardown (runtime subprocess kill + worktree
// force-clean). run() arms the run context at exactly jobTimeout but holds the
// lease through that teardown, which happens AFTER the deadline fires. Without
// this margin the lease would expire in the window [t0+jobTimeout,
// t0+jobTimeout+teardown] while the worker is STILL ALIVE finishing — and
// recoverExpiredRuntimeSessionLocks + DeleteExpiredResourceLocks (runtime:%
// bypasses the not-running guard) would reap that live worker's lock and requeue
// its still-'running' owner, starting a SECOND worker on the dirty in-flight
// worktree: the exact #536 clobber. With the grace a NORMALLY-terminating worker
// always releases its lease before it expires; only a genuinely stuck/crashed
// worker past jobTimeout+grace is reaped and requeued.
const runtimeLeaseTeardownGrace = 2 * time.Minute
const daemonJobCancelPollInterval = 250 * time.Millisecond
const daemonWorkerLoopInterval = 1 * time.Second

// daemonPollTimeout bounds a single repo's PollOnce / PollRecoveryCommandsOnce.
// The poll runs while HOLDING that repo's checkout lock, and both supervisors
// take each repo's lock SEQUENTIALLY, so a wedged (ctx-respecting-but-slow)
// poll on one repo freezes that repo's — and, in the multi-repo sweep, every
// later repo's — worker ticks until it returns (#555 / #536). It is therefore a
// hard STALL bound, not the expected poll duration: reusing
// daemonRunningJobStaleAfter (30 min) here left the sweep frozen for up to half
// an hour, largely defeating #555's anti-stall goal, so it is deliberately much
// tighter. A healthy poll finishes well inside this; exceeding it means the poll
// is wedged and cancelling it (the deferred checkout Unlock still runs, so no
// lock leak) is the correct recovery.
const daemonPollTimeout = 2 * time.Minute

// cockpitReconcileInterval is the low-frequency cadence of the cockpit reconcile
// GC sweep (Task 7): it drops cockpit_pane rows whose herdr pane is gone and whose
// owning root is terminal. It runs rarely because it is a backstop for the
// per-Deliver / root-finalize teardown plus report-metadata --ttl-ms self-expiry.
const cockpitReconcileInterval = 5 * time.Minute

var errRuntimeSessionBusy = errors.New("runtime session is busy")

// runtimeLockWaitEpisodes dedups the runtime_lock_wait job_event: a job that
// bounces busy every dispatcher pass records ONE event per wait EPISODE — the
// first busy since it last acquired its runtime lock, or since daemon start — not
// one per attempt. Before #598 a permanently-contended job wrote a runtime_lock_wait
// row on EVERY dispatch pass (~76k rows / 56% of the whole job_events table), which
// then bloated every per-job ListJobEvents scan the retry/recovery passes run.
//
// The map records, per job id, WHEN that job's episode event was last EMITTED. An
// episode is "open" (suppress further writes) while an entry exists AND is younger
// than runtimeLockWaitEpisodeTTL; the id is cleared outright when the job acquires
// its runtime lock, so the next wait re-emits immediately. For a job that stays
// contended longer than the TTL the episode re-opens and re-emits at most one event
// per TTL — a deliberate liveness signal that the wait is still ongoing, not a
// per-pass flood. Entries also expire: a job that terminates WITHOUT ever acquiring
// (so endRuntimeLockWaitEpisode never runs for it) leaves a stale entry that ages
// past the "open" window and is pruned once the map grows beyond
// runtimeLockWaitEpisodeMax, so terminal-without-acquire jobs can no longer grow the
// map unboundedly. In-memory ⇒ resets on daemon start (matching the "since daemon
// start" episode boundary). Mirrors the preflightWarnByRepo/preflightWarnMu throttle
// style above.
const (
	// runtimeLockWaitEpisodeTTL re-opens a still-contended job's wait episode after
	// this long, so a very long wait re-emits one liveness event per TTL rather than
	// staying silent forever.
	runtimeLockWaitEpisodeTTL = 15 * time.Minute
	// runtimeLockWaitEpisodeMax bounds the episode map: once it exceeds this many
	// entries, markRuntimeLockWaitEpisode prunes every entry older than the TTL
	// (terminal-without-acquire leftovers that endRuntimeLockWaitEpisode will never
	// clear).
	runtimeLockWaitEpisodeMax = 512
)

var (
	runtimeLockWaitMu       sync.Mutex
	runtimeLockWaitEpisodes = map[string]time.Time{}
)

// runtimeLockWaitEpisodeOpen reports whether jobID currently has an open, already-
// emitted wait episode: an entry exists AND its event was emitted within the last
// runtimeLockWaitEpisodeTTL. It is READ-ONLY — it never mutates the map — so a
// failed event write (which skips markRuntimeLockWaitEpisode) leaves the episode
// closed and the next bounce re-attempts the write. Call it BEFORE writing; write
// the event iff it returns false.
func runtimeLockWaitEpisodeOpen(jobID string) bool {
	runtimeLockWaitMu.Lock()
	defer runtimeLockWaitMu.Unlock()
	emitted, ok := runtimeLockWaitEpisodes[jobID]
	if !ok {
		return false
	}
	return time.Since(emitted) < runtimeLockWaitEpisodeTTL
}

// markRuntimeLockWaitEpisode records that a runtime_lock_wait event was just emitted
// for jobID, opening (or refreshing) its wait episode. Call it ONLY AFTER AddJobEvent
// succeeds, so a failed write is retried on the next bounce instead of being
// suppressed. It also opportunistically bounds the map: once it exceeds
// runtimeLockWaitEpisodeMax entries, every entry older than the TTL is dropped (these
// are terminal-without-acquire leftovers past their liveness window — a live episode
// is refreshed on each re-emit and so never ages out here).
func markRuntimeLockWaitEpisode(jobID string) {
	runtimeLockWaitMu.Lock()
	defer runtimeLockWaitMu.Unlock()
	runtimeLockWaitEpisodes[jobID] = time.Now()
	if len(runtimeLockWaitEpisodes) > runtimeLockWaitEpisodeMax {
		for id, emitted := range runtimeLockWaitEpisodes {
			if time.Since(emitted) >= runtimeLockWaitEpisodeTTL {
				delete(runtimeLockWaitEpisodes, id)
			}
		}
	}
}

// endRuntimeLockWaitEpisode clears jobID's wait episode once it acquires its
// runtime lock, so a later wait is recorded as a fresh episode.
func endRuntimeLockWaitEpisode(jobID string) {
	runtimeLockWaitMu.Lock()
	defer runtimeLockWaitMu.Unlock()
	delete(runtimeLockWaitEpisodes, jobID)
}

type tempWorkerEligibility struct {
	Eligible bool
	Reason   string
}

func defaultJobWorker(store *db.Store, stdout io.Writer, home ...string) jobWorker {
	configHome := ""
	configHomeExplicit := false
	if len(home) > 0 {
		configHome = home[0]
		configHomeExplicit = true
	}
	worker := jobWorker{Store: store, Stdout: stdout, ConfigHome: configHome, ConfigHomeExplicit: configHomeExplicit}
	worker.RelayServer = activeChatRelayServer()
	worker.AdapterFactory = worker.defaultAdapter
	worker.OutputAdapterFactory = worker.outputAdapter
	worker.StartAdapterFactory = worker.defaultStartAdapter
	worker.CheckoutValidator = worker.defaultCheckout
	worker.WorkflowFactory = worker.defaultWorkflow
	worker.AuthProbe = worker.defaultAuthProbe
	return worker
}

// staleFloorWarnOnce keeps the sub-floor warning from flooding the log: the
// recovery path that reads this runs once per worker-loop tick (~1s), so we warn
// at most once per daemon process.
var staleFloorWarnOnce sync.Once

// configuredDaemonRunningJobStaleAfter resolves the crash-backstop window from
// GITMOOT_STALE_RUNNING_AFTER, falling back to the default when unset, malformed,
// non-positive, OR below daemonRunningJobStaleFloor. This is a CRASH BACKSTOP,
// not a timeout: it bounds how long a job may sit 'running' with no lease
// progress before the daemon assumes the worker crashed and requeues it. A
// sub-floor value (e.g. 1s) would let the backstop requeue live workers — most
// dangerously for non-resumable runtimes that hold no lease — so it is rejected
// with a one-time warning rather than honored (#560).
func configuredDaemonRunningJobStaleAfter(stdout io.Writer) time.Duration {
	raw := strings.TrimSpace(os.Getenv("GITMOOT_STALE_RUNNING_AFTER"))
	if raw == "" {
		return defaultDaemonRunningJobStaleAfter
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultDaemonRunningJobStaleAfter
	}
	if d < daemonRunningJobStaleFloor {
		staleFloorWarnOnce.Do(func() {
			writeLine(stdout, "GITMOOT_STALE_RUNNING_AFTER=%s is below the %s crash-backstop floor; using default %s", raw, daemonRunningJobStaleFloor, defaultDaemonRunningJobStaleAfter)
		})
		return defaultDaemonRunningJobStaleAfter
	}
	return d
}

func recoverRunningJobs(ctx context.Context, store *db.Store, stdout io.Writer) error {
	now := time.Now().UTC()
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, now, now.Add(-configuredDaemonRunningJobStaleAfter(stdout)), "", "")
}

func recoverExpiredRuntimeSessionLocks(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time) error {
	return recoverExpiredRuntimeSessionLocksSkipping(ctx, store, stdout, now, nil)
}

// recoverForeignBootRunners is the #651 cross-boot recovery pass, run at daemon
// startup AND every worker tick. When this host's boot id differs from the boot id
// recorded on a running job / held runtime-session lock, that owner was claimed on
// a PREVIOUS boot and its in-process worker died when the host rebooted — so it is
// recovered IMMEDIATELY, regardless of any runtime-session lease (which survives a
// reboot in the DB and would otherwise keep the job "held" until it expired: the
// AC2 gap #536's lease gate cannot close by itself).
//
// It requeues the foreign-boot running jobs (this covers non-resumable/shell jobs
// too, which hold no lease at all) and reclaims the foreign-boot runtime-session
// locks so a requeued resumable owner can re-acquire its session on re-dispatch.
// It is a STRICT no-op off Linux (BootID()=="") — preserving today's age/lease
// behavior — and never touches a SAME-boot owner, so a live in-process worker is
// never double-run (the #536 protection is untouched). Cheap and idempotent: after
// the first pass reclaims them there are no foreign-boot rows left, so re-running
// it per repo per tick is a near-empty indexed scan.
func recoverForeignBootRunners(ctx context.Context, store *db.Store, stdout io.Writer) error {
	bootID := db.BootID()
	if bootID == "" {
		return nil
	}
	requeued, err := store.RequeueRunningJobsFromForeignBoot(ctx, bootID)
	if err != nil {
		return err
	}
	for _, id := range requeued {
		writeLine(stdout, "requeued running job %s claimed on a previous boot (host rebooted)", id)
	}
	released, err := store.ReleaseRuntimeSessionLocksFromForeignBoot(ctx, bootID)
	if err != nil {
		return err
	}
	if released > 0 {
		writeLine(stdout, "reclaimed %d runtime session lock(s) held on a previous boot", released)
	}
	return nil
}

// recoverExpiredRuntimeSessionLocksSkipping is recoverExpiredRuntimeSessionLocks
// with an in-flight owner exclusion (#562): a lock whose owner job is currently
// being run BY THIS PROCESS is neither requeued nor reaped even if its lease has
// expired — the owning goroutine is still alive (a ctx-deaf runtime overrunning
// its timeout), and releasing its lock would let a second run of the same
// session start beside it (the #536 hazard). A nil skip set is byte-identical
// to the unskipped recovery.
func recoverExpiredRuntimeSessionLocksSkipping(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, skipOwners map[string]bool) error {
	expiredRuntimeLocks, err := store.ListExpiredRuntimeSessionLocks(ctx, now)
	if err != nil {
		return err
	}
	// Requeue owners BEFORE reaping the lock rows. An expired runtime lease means
	// the job's real timeout + teardown grace has elapsed, so a still-'running'
	// owner is genuinely stale (a normally-terminating worker releases its lock
	// before the grace-padded lease expires — see runtimeLeaseTeardownGrace).
	// Ordering requeue-then-delete keeps the two durable: if a mid-loop DB error
	// aborts the sweep, the un-processed locks are still expired and get retried
	// next tick, instead of being deleted up front and losing the requeue signal
	// (which would strand those owners as 'running' until the coarse 30m window).
	// TransitionJobStateWithEvent is a no-op unless the owner is still 'running'.
	for _, lock := range expiredRuntimeLocks {
		if strings.TrimSpace(lock.OwnerJobID) == "" || skipOwners[lock.OwnerJobID] {
			continue
		}
		recovered, err := store.TransitionJobStateWithEvent(ctx, lock.OwnerJobID, string(workflow.JobRunning), string(workflow.JobQueued), db.JobEvent{
			JobID:   lock.OwnerJobID,
			Kind:    string(workflow.JobQueued),
			Message: "recovered running job after runtime session lock expired",
		})
		if err != nil {
			return err
		}
		if recovered {
			writeLine(stdout, "requeued running job %s after runtime session lock expired", lock.OwnerJobID)
		}
	}
	deleted, err := store.DeleteExpiredResourceLocksExcludingOwners(ctx, now, sortedStringSetKeys(skipOwners))
	if err != nil {
		return err
	}
	if deleted > 0 {
		writeLine(stdout, "recovered %d expired runtime session locks", deleted)
	}
	return nil
}

// jobEventBlockedTTLExpired is the job_event kind the blocked_ttl sweep appends
// after it dismisses a blocked job (#631). It is DISTINCT from the bare
// "cancelled" event workflow.CancelJob writes so a job's history tells a TTL
// auto-expiry apart from an operator's explicit `job cancel`.
const jobEventBlockedTTLExpired = "blocked_ttl_expired"

// sweepExpiredBlockedJobs is the opt-in blocked-job TTL reaper (#631), mirroring
// recoverExpiredRuntimeSessionLocks's tick cadence. With ttl <= 0 — the DEFAULT,
// [orchestrate].blocked_ttl unset — it is an immediate no-op: a blocked job is
// paused awaiting a human, so it is NEVER auto-dismissed unless the operator opted
// in with a positive duration (so the default path is byte-identical).
//
// Otherwise it dismisses every blocked job whose last transition — updated_at,
// stamped by the blocked transition, falling back to created_at — is older than
// now-ttl. It routes each dismissal through workflow.CancelJob, the SAME single-row
// abandon verb an operator's `job cancel` uses, so the job's best-effort lock
// releases fire; it NEVER raw-writes the cancelled state, which would strand those
// locks. Each successful dismissal appends a distinct jobEventBlockedTTLExpired
// event naming the TTL.
//
// It is resilient: one job's cancel (or event-append) failure is logged and
// skipped so it can never abort the rest of the sweep. A job with no parseable
// timestamp is left alone rather than treated as infinitely old.
func sweepExpiredBlockedJobs(ctx context.Context, store *db.Store, ttl time.Duration, stdout io.Writer, now time.Time) error {
	if ttl <= 0 {
		return nil
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return err
	}
	cutoff := now.Add(-ttl).UnixMilli()
	swept := 0
	for _, job := range jobs {
		if job.State != string(workflow.JobBlocked) {
			continue
		}
		stamped := parseJobTimeMillis(job.UpdatedAt)
		if stamped == 0 {
			stamped = parseJobTimeMillis(job.CreatedAt)
		}
		if stamped == 0 || stamped >= cutoff {
			continue
		}
		if _, err := workflow.CancelJob(ctx, store, job.ID); err != nil {
			writeLine(stdout, "blocked_ttl sweep: cancel of blocked job %s failed: %v", job.ID, err)
			continue
		}
		// The cancel already succeeded; the history marker is best-effort (like
		// CancelJob's own lock-release events) so a failed append is logged but never
		// undoes the dismissal or aborts the rest of the sweep.
		if err := store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    jobEventBlockedTTLExpired,
			Message: fmt.Sprintf("dismissed after blocked_ttl %s elapsed", ttl),
		}); err != nil {
			writeLine(stdout, "blocked_ttl sweep: recording expiry event for job %s failed: %v", job.ID, err)
		}
		swept++
	}
	if swept > 0 {
		writeLine(stdout, "blocked_ttl sweep: dismissed %d blocked job(s) idle longer than %s", swept, ttl)
	}
	return nil
}

func runDaemonPollWithTimeout(ctx context.Context, timeout time.Duration, poll func(context.Context) error) error {
	if timeout <= 0 {
		return poll(ctx)
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return poll(pollCtx)
}

func recoverRunningJobsBefore(ctx context.Context, store *db.Store, stdout io.Writer, before time.Time) error {
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, time.Now().UTC(), before, "", "")
}

func recoverRunningJobsForRepo(ctx context.Context, store *db.Store, stdout io.Writer, repoFilter string, rootFilter string) error {
	now := time.Now().UTC()
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, now, now.Add(-configuredDaemonRunningJobStaleAfter(stdout)), repoFilter, rootFilter)
}

func recoverCancelledRunningJobsForEnabledRepos(ctx context.Context, store *db.Store, rootFilter string, stdout io.Writer) error {
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		if err := recoverCancelledRunningJobsForRepo(ctx, store, stdout, repo.FullName(), rootFilter); err != nil {
			return err
		}
	}
	return nil
}

func recoverCancelledRunningJobsForRepo(ctx context.Context, store *db.Store, stdout io.Writer, repoFilter string, rootFilter string) error {
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.State != string(workflow.JobCancelled) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		settled, err := workflow.SettleCancelledRunningJob(ctx, store, job.ID, "cancelled job recovered after daemon restart")
		if err != nil {
			return err
		}
		if settled {
			writeLine(stdout, "settled cancelled running job %s", job.ID)
		}
	}
	return nil
}

func recoverRunningJobsBeforeForRepo(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, before time.Time, repoFilter string, rootFilter string) error {
	return recoverRunningJobsBeforeForRepoSkipping(ctx, store, stdout, now, before, repoFilter, rootFilter, nil)
}

// recoverRunningJobsBeforeForRepoSkipping is recoverRunningJobsBeforeForRepo
// with an in-flight exclusion (#562): a job THIS process is currently running is
// never treated as crashed-stale, even past the coarse 30m backstop with no
// runtime lease (e.g. a long shell-runtime job holds no lease at all). Inline
// execution used to guarantee this by never scanning while a job ran; the async
// dispatcher must guarantee it explicitly. A nil skip set is byte-identical.
func recoverRunningJobsBeforeForRepoSkipping(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, before time.Time, repoFilter string, rootFilter string, skipJobs map[string]bool) error {
	jobs, err := store.ListRunningJobsUpdatedBefore(ctx, before)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if skipJobs[job.ID] {
			continue
		}
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if err := recoverRunningJobIfLeaseExpired(ctx, store, stdout, now, job); err != nil {
			return err
		}
	}
	return nil
}

func recoverRunningJobIfLeaseExpired(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, job db.Job) error {
	// Liveness gate (#536): the coarse `updated_at < before` threshold (30m) is a
	// crash backstop, NOT a timeout. A long-running job (e.g. a 4h delegation)
	// holds a runtime-session lock whose LEASE reflects its real job timeout. If
	// that lease has not elapsed the job's timeout has not elapsed, so leave it
	// running — requeuing it would start a second copy that fails on the dirty
	// in-flight worktree and then force-cleans it out from under the live worker.
	//
	// This keys on the lease, NOT on the lock's owner PID: the recorded PID is the
	// gitmoot DAEMON's, not the spawned runtime worker's, so on a daemon restart it
	// is the dead prior daemon even while the reparented worker keeps running — the
	// exact path this recovery is named for. Honoring the lease is correct across a
	// restart (the lease survives in the DB) and immune to PID reuse. The trade-off:
	// a genuinely-crashed worker whose daemon also died is recovered only once its
	// lease expires (recoverExpiredRuntimeSessionLocks reclaims it, then a later
	// tick requeues it) rather than at the 30m threshold — promptness traded for
	// never failing live work, the unattended-reliability goal of #536.
	leaseHeld, err := runtimeOwnerLeaseHeld(ctx, store, job.ID, now)
	if err != nil {
		return err
	}
	if leaseHeld {
		return nil
	}
	recovered, err := store.TransitionJobStateWithEvent(ctx, job.ID, string(workflow.JobRunning), string(workflow.JobQueued), db.JobEvent{
		JobID:   job.ID,
		Kind:    string(workflow.JobQueued),
		Message: "recovered stale running job on daemon startup",
	})
	if err != nil {
		return err
	}
	if recovered {
		writeLine(stdout, "requeued stale running job %s", job.ID)
	}
	return nil
}

func runQueuedJobs(ctx context.Context, worker jobWorker, limit int) error {
	return runQueuedJobsForRepo(ctx, worker, limit, "", "")
}

// tickCandidates memoizes the three per-tick job-candidate GROUP BY queries
// (advance-retry / comment-retry / delegation-worktree-reclaim) so they run ONCE
// per supervisor tick instead of once per enabled repo (#619). Each query takes
// NO repo argument — they scan the whole job_events table and return the global
// candidate set — yet the retry passes ran them inside runDaemonWorkerTickTracked,
// which the multi-repo supervisor invokes once per enabled repo (18×/tick on the
// affected VPS). The most expensive of the three (JobIDsWithPendingAdvanceRetry)
// materialized ~23.67 MiB of row fetches per call, so re-running it per repo was
// the single largest source of the daemon's idle read volume. Hoisting it here
// keeps per-repo filtering exactly where it was (in Go, in the retry passes) while
// collapsing the shared query to one execution.
//
// Two memoization properties, both implemented once in candidateMemo.get:
//
//  1. SUCCESSES are computed once per tick and shared across every repo's pass, so
//     each query runs once per tick, not once per enabled repo. A job that begins
//     qualifying mid-sweep is therefore not observed until the next tick's fresh
//     carrier — a deliberate, bounded one-tick staleness that self-corrects on the
//     following tick. The carrier is created FRESH each tick (so a candidate that
//     stops qualifying next tick is re-evaluated) and MUST NOT be stored on the
//     long-lived tracker/worker.
//  2. ERRORS are NOT memoized. A failed query leaves the memo unset so the next
//     repo's pass RE-RUNS it. This preserves the per-repo fault isolation the
//     pre-#619 per-repo queries had: a transient store fault (e.g. a single
//     SQLITE_BUSY) fails only the repo that hit it and can self-heal for the rest
//     of the sweep, instead of being replayed to all 18 repos — which would make
//     failed==enabled, error the whole sweep, and feed the consecutive-tick daemon
//     self-exit streak #619 is closing.
//
// No mutex/sync.Once: it is consumed ONLY on the synchronous tick goroutine — the
// per-repo loop in runEnabledRepoWorkerTicksTracked is sequential, and dispatched
// jobs run on their own goroutines and never touch it.
//
// The store dependency is the narrow tickCandidateStore interface (satisfied by
// *db.Store) purely so a counting fake can pin the once-per-tick property in tests;
// production always threads the real *db.Store, so behavior is byte-identical.
type tickCandidateStore interface {
	JobIDsWithPendingAdvanceRetry(ctx context.Context) ([]string, error)
	JobIDsWithPendingCommentRetry(ctx context.Context) ([]string, error)
	JobIDsWithPendingDelegationWorktreeReclaim(ctx context.Context) ([]string, error)
}

// candidateMemo lazily runs one per-tick candidate query and shares its RESULT
// across the tick's repos, memoizing ONLY a success: get caches the ids on the first
// successful fetch and returns them on every later call, but on a query error it
// returns the error and leaves the memo unset so the next call RE-RUNS fetch
// (retry-on-error — see tickCandidates for why per-repo fault isolation matters). It
// is consumed only on the synchronous tick goroutine, so it needs no synchronization.
type candidateMemo struct {
	done bool
	ids  []string
}

func (m *candidateMemo) get(fetch func() ([]string, error)) ([]string, error) {
	if m.done {
		return m.ids, nil
	}
	ids, err := fetch()
	if err != nil {
		return nil, err
	}
	m.ids = ids
	m.done = true
	return m.ids, nil
}

type tickCandidates struct {
	store   tickCandidateStore
	advance candidateMemo
	comment candidateMemo
	reclaim candidateMemo
}

// newTickCandidates is a package var (not a plain func) only so the once-per-tick
// regression test can substitute a carrier backed by a counting store; production
// never reassigns it.
var newTickCandidates = func(store tickCandidateStore) *tickCandidates {
	return &tickCandidates{store: store}
}

func (c *tickCandidates) advanceRetryCandidates(ctx context.Context) ([]string, error) {
	return c.advance.get(func() ([]string, error) {
		return c.store.JobIDsWithPendingAdvanceRetry(ctx)
	})
}

func (c *tickCandidates) commentRetryCandidates(ctx context.Context) ([]string, error) {
	return c.comment.get(func() ([]string, error) {
		return c.store.JobIDsWithPendingCommentRetry(ctx)
	})
}

func (c *tickCandidates) delegationReclaimCandidates(ctx context.Context) ([]string, error) {
	return c.reclaim.get(func() ([]string, error) {
		return c.store.JobIDsWithPendingDelegationWorktreeReclaim(ctx)
	})
}

// retryPendingJobAdvancements re-fires the post-delivery advancement for any
// terminal job whose latest advancement event is still an unreconciled attempt
// marker (advance_started/advance_retry). It is BOUNDED, not a full-table scan
// (#598): rather than list EVERY job and re-read each terminal job's full event
// history (ListJobEvents) on every 1s worker tick — O(jobs × events), which burned
// a core once a few hundred terminal jobs had accumulated — it asks the store for
// ONLY the (small) set of jobs whose latest tracked advancement event is a pending
// marker, and GetJob's just those. Each candidate is then re-verified with the Go
// predicate jobNeedsAdvanceRetry, so behavior is identical to the old per-job walk;
// the state/repo/session filters and the checkoutHeld gate are preserved verbatim.
// The candidate set comes from the per-tick tickCandidates carrier (#619) so the
// underlying GROUP BY query runs once per tick, not once per enabled repo.
//
// checkoutHeld (nil ⇒ no gate, the legacy inline-tick behavior) reports whether an
// in-flight dispatched job currently holds a checkout key: a candidate whose own
// checkout key is held is skipped this tick (#562 review) — advancement mutates that
// checkout, and the live path only ever runs it under the job's own key — and
// retried on a later tick once the key frees, instead of gating ALL retries on
// whole-repo idleness (which a steady backlog can prevent indefinitely, freezing
// merge retries).
func retryPendingJobAdvancements(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, checkoutHeld func(string) bool, cand *tickCandidates) error {
	jobIDs, err := cand.advanceRetryCandidates(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		job, err := worker.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			// A marker event with no surviving job row (e.g. a pruned job): nothing
			// to advance, and erroring here would abort the whole tick.
			continue
		}
		if err != nil {
			return err
		}
		if !jobStateCanRetryAdvancement(job.State) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		needsRetry, err := worker.jobNeedsAdvanceRetry(ctx, job.ID)
		if err != nil {
			return err
		}
		if !needsRetry {
			continue
		}
		if checkoutHeld != nil && checkoutHeld(queuedJobCheckoutKey(ctx, worker.Store, job)) {
			continue
		}
		if err := worker.advanceJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

// delegationWorktreeCleanupPending reports whether jobID's last terminal cleanup
// outcome was a PRESERVE (delegation_worktree_cleanup_skipped) with no subsequent
// reclamation (delegation_worktree_removed) — i.e. its per-delegation worktree and
// gitmoot-delegation-* branch are still on disk awaiting reclaim once the foreign
// runtime owner that blocked the cleanup releases/expires (#536). The order of the
// two event kinds matters (a worktree can be preserved, then later removed), so the
// LAST one wins.
func (w jobWorker) delegationWorktreeCleanupPending(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	pending := false
	for _, event := range events {
		switch event.Kind {
		case "delegation_worktree_cleanup_skipped":
			pending = true
		case "delegation_worktree_removed":
			pending = false
		}
	}
	return pending, nil
}

// reclaimSkippedDelegationWorktrees re-fires the terminal worktree cleanup for any
// terminal delegation child whose cleanup was previously SKIPPED because a foreign
// runtime owner was still active (#536). The cleanup is idempotent and itself
// liveness-gated, so this is a no-op while the owner remains active; once the
// owner's lock releases or its lease expires (recoverExpiredRuntimeSessionLocks
// runs earlier in the tick), the preserved worktree+branch are reclaimed rather
// than leaked forever.
//
// It is BOUNDED, not a full-table scan (#549): rather than list every job and
// re-read each terminal job's full event history (ListJobEvents) on every 1s
// supervisor tick — O(jobs × events), which burned a core once a few hundred
// terminal jobs had accumulated — it asks the store for ONLY the (small) set of
// jobs whose latest cleanup outcome is still an unreconciled preserve marker, and
// reads just those. Correctness is unchanged: a worktree that genuinely needs
// reclaiming still carries that marker and is still reclaimed; once reclaimed it
// emits delegation_worktree_removed and drops out of the candidate set.
// checkoutHeld (nil ⇒ no gate, the legacy inline-tick behavior) skips a
// candidate while an in-flight job holds either the terminal child's own
// worktree key (someone is running in the worktree being reclaimed — e.g. a
// continuation reusing it) or the repo's shared checkout key (the reclaim's git
// commands run from the parent checkout). Skipped candidates keep their pending
// marker and are reclaimed on a later tick, so under a steady backlog preserved
// worktrees are still reclaimed instead of leaking until full idleness (#562
// review).
func reclaimSkippedDelegationWorktrees(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, checkoutHeld func(string) bool, cand *tickCandidates) error {
	jobIDs, err := cand.delegationReclaimCandidates(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		job, err := worker.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			// A cleanup-marker event with no surviving job row (e.g. a pruned job):
			// nothing to reclaim, and erroring here would abort the whole tick.
			continue
		}
		if err != nil {
			return err
		}
		if !jobStateEligibleForWorktreeReclaim(job.State) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if checkoutHeld != nil {
			if checkoutHeld(queuedJobCheckoutKey(ctx, worker.Store, job)) {
				continue
			}
			if repoFilter != "" && checkoutHeld("repo:"+repoFilter) {
				continue
			}
		}
		engine := worker.WorkflowFactory(worker.delegationParentCheckout(ctx, job))
		if err := engine.ReclaimTerminalDelegationWorktree(ctx, jobID); err != nil {
			return err
		}
	}
	return nil
}

func runDaemonWorkerTick(ctx context.Context, store *db.Store, worker jobWorker, workers int, dryRun bool, repoFilter string, rootFilter string, stdout io.Writer, now time.Time) error {
	return runDaemonWorkerTickTracked(ctx, store, worker, workers, dryRun, repoFilter, rootFilter, stdout, now, nil, nil)
}

// runDaemonWorkerTickTracked is the per-tick worker pass. With a nil tracker it
// is byte-identical to the historical runDaemonWorkerTick: maintenance scans,
// then a BLOCKING runQueuedJobsForRepo dispatch. The supervisors pass a live
// tracker (#562), which changes the tick to claim-and-dispatch-async:
//
//   - recovery scans skip jobs THIS process is running (an in-flight >30m job
//     with no runtime lease — e.g. a shell-runtime job — must not be requeued
//     out from under its own live worker by its own daemon's tick);
//   - the expired runtime-lock reaper likewise skips locks owned by in-flight
//     jobs (their goroutine is alive; releasing the lock could double-run the
//     session);
//   - comment retries (no checkout touched) run every tick; checkout-mutating
//     maintenance (advancement retries, delegation worktree reclaims) skips any
//     candidate whose checkout key an in-flight job holds — so it never mutates
//     a checkout under a running job, without being starved forever by a repo
//     that always has SOMETHING in flight;
//   - dispatch goes through dispatchQueuedJobsTracked, which returns promptly
//     and bounds in-flight jobs by both the repo limit and the host-global
//     --workers cap.
func runDaemonWorkerTickTracked(ctx context.Context, store *db.Store, worker jobWorker, workers int, dryRun bool, repoFilter string, rootFilter string, stdout io.Writer, now time.Time, tracker *inflightJobTracker, cand *tickCandidates) error {
	if dryRun {
		return nil
	}
	// A nil carrier means this is a standalone tick (single-repo supervisor or the
	// runDaemonWorkerTick wrapper): compute the shared candidate sets once for THIS
	// tick. The multi-repo supervisor passes a carrier it created once per tick, so
	// the three GROUP BY queries run once per tick rather than once per enabled repo
	// (#619).
	if cand == nil {
		cand = newTickCandidates(worker.Store)
	}
	inflightIDs := tracker.inflightIDs()
	// Cross-boot recovery (#651): requeue jobs / reclaim runtime-session locks whose
	// recorded boot id proves a reboot happened, before the lease-gated recovery
	// below (which alone would leave a rebooted long-lease job "held"). It is
	// boot-scoped and repo-agnostic — no in-flight skip is needed because a foreign
	// boot id can never belong to a job this process is currently running.
	if err := recoverForeignBootRunners(ctx, store, stdout); err != nil {
		return err
	}
	if err := recoverRunningJobsBeforeForRepoSkipping(ctx, store, stdout, now, now.Add(-configuredDaemonRunningJobStaleAfter(stdout)), repoFilter, rootFilter, inflightIDs); err != nil {
		return err
	}
	if err := recoverExpiredRuntimeSessionLocksSkipping(ctx, store, stdout, now, inflightIDs); err != nil {
		return err
	}
	// Opt-in blocked-job TTL reaper (#631): dismiss blocked jobs (paused awaiting a
	// human) idle longer than [orchestrate].blocked_ttl. Disabled by default (ttl 0
	// ⇒ immediate no-op), so the default path is byte-identical. A sweep fault is
	// LOGGED, not returned: this optional housekeeping reaper must never abort the
	// tick's dispatch or escalate the daemon the way the store-fault recovery scans
	// above (deliberately) do. Resolved per tick, so the TTL is live-tunable like the
	// per-repo scheduler override below.
	if err := sweepExpiredBlockedJobs(ctx, store, resolveBlockedTTL(worker.workflowHome()), stdout, now); err != nil {
		writeLine(stdout, "blocked_ttl sweep failed: %v", err)
	}
	// Checkout-mutating maintenance (advancement/merge retries, delegation
	// worktree reclaims) is gated on the ACTUAL mutation hazard — each
	// candidate is skipped while an in-flight job holds the checkout key the
	// retry would touch — not on whole-repo idleness, which a steady staggered
	// backlog can prevent indefinitely (main's blocking barrier guaranteed an
	// idle point between batches; tracked dispatch does not). The per-key gate
	// mirrors the live path: a finishing job runs this same advancement inline
	// under its own key while other keys stay busy. It is race-free on the
	// barrier because begin() only ever runs on THIS goroutine (in the dispatch
	// below) and end() only frees keys; a live background POOL pass begins jobs
	// on its own goroutine, so it still defers the whole block (matching main,
	// where a live pool pass blocked the tick entirely).
	if !tracker.poolRunning(repoFilter) {
		if err := retryPendingJobAdvancements(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand); err != nil {
			return err
		}
		if err := reclaimSkippedDelegationWorktrees(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand); err != nil {
			return err
		}
	}
	// Comment retries only post PR comments through the commenter — they never
	// touch a checkout — so they run EVERY tick regardless of in-flight work,
	// exactly main's cadence (and main's advancements→reclaims→comments order).
	// Gating them on an idle repo would let one multi-hour in-flight job delay a
	// transiently-failed result comment (and any downstream automation waiting
	// on it) for the job's whole duration.
	if err := retryPendingJobComments(ctx, worker, repoFilter, rootFilter, cand); err != nil {
		return err
	}
	// Per-repo concurrency override (#576): a [repos."owner/repo"] section caps
	// THIS repo's in-flight concurrency (and may flip its scheduler) without a
	// global daemon restart. With no matching section this returns (workers,
	// worker.UsePool) unchanged, so the run path is byte-identical to today. The
	// override is re-read here every tick, which is precisely what makes it
	// tunable live. worker is passed by value, so the per-repo UsePool flip is
	// local to this tick's dispatch and never leaks to sibling repos.
	limit, usePool := worker.resolveRepoScheduler(repoFilter, workers)
	worker.UsePool = usePool
	if tracker == nil {
		return runQueuedJobsForRepo(ctx, worker, limit, repoFilter, rootFilter)
	}
	return dispatchQueuedJobsTracked(ctx, worker, limit, workers, repoFilter, rootFilter, tracker)
}

func runEnabledRepoWorkerTicks(ctx context.Context, store *db.Store, worker jobWorker, workers int, stdout io.Writer, now time.Time) error {
	return runEnabledRepoWorkerTicksTracked(ctx, store, worker, workers, "", stdout, now, nil, nil)
}

func runEnabledRepoWorkerTicksWithLocks(ctx context.Context, store *db.Store, worker jobWorker, workers int, rootFilter string, stdout io.Writer, now time.Time, locks *repoCheckoutLocks) error {
	return runEnabledRepoWorkerTicksTracked(ctx, store, worker, workers, rootFilter, stdout, now, locks, nil)
}

func runEnabledRepoWorkerTicksTracked(ctx context.Context, store *db.Store, worker jobWorker, workers int, rootFilter string, stdout io.Writer, now time.Time, locks *repoCheckoutLocks, tracker *inflightJobTracker) error {
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	// Compute the shared per-tick job-candidate sets ONCE for this whole sweep and
	// pass the carrier into every enabled repo's tick (#619). The three GROUP BY
	// candidate queries take no repo argument — they return the global candidate set
	// that each repo's retry pass then filters in Go — so running them once here
	// instead of once inside each runDaemonWorkerTickTracked collapses 18×/tick down
	// to 1×/tick on a multi-repo daemon. Fresh each sweep; never retained.
	cand := newTickCandidates(worker.Store)
	// Scope tick faults per repo (#555 follow-up): the recovering supervisor
	// treats a returned error as one fleet-wide failure unit and, after a bounded
	// streak, exits the WHOLE daemon. Returning on the first repo's error would
	// let a single repo-local fault (e.g. a broken/permission-denied checkout
	// dir) both starve every later repo in ListRepos order AND escalate/kill the
	// healthy repos' daemon with it. So log a single repo's tick error and keep
	// sweeping; only a fault hitting EVERY enabled repo — a shared/store-level
	// fault such as locked/corrupt SQLite or disk-full, the genuine global fault
	// #555's escalation targets — is returned so the supervisor can escalate.
	enabled := 0
	failed := 0
	var lastErr error
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		enabled++
		lock := locks.For(repo.FullName())
		if lock != nil {
			lock.Lock()
		}
		tickErr := runDaemonWorkerTickTracked(ctx, store, worker, workers, false, repo.FullName(), rootFilter, stdout, now, tracker, cand)
		if lock != nil {
			lock.Unlock()
		}
		if tickErr != nil {
			// A cancellation observed mid-sweep is a clean shutdown, not a repo
			// fault: propagate it immediately so the supervisor treats it as such
			// (and it never counts toward or masks the escalation streak).
			if errors.Is(tickErr, context.Canceled) || ctx.Err() != nil {
				return tickErr
			}
			failed++
			lastErr = tickErr
			writeLine(stdout, "%s: worker tick error: %v", repo.FullName(), tickErr)
		}
	}
	// Every enabled repo failing is the global-fault signal: return it so the
	// recovering supervisor's streak can trip and escalate. A single-repo daemon
	// (enabled==1) still escalates on its own persistent fault, matching the
	// single-repo supervisor.
	if enabled > 0 && failed == enabled {
		return lastErr
	}
	return nil
}

func jobStateCanRetryAdvancement(state string) bool {
	switch state {
	case string(workflow.JobSucceeded), string(workflow.JobFailed), string(workflow.JobBlocked):
		return true
	default:
		return false
	}
}

// jobStateEligibleForWorktreeReclaim gates the delegation/read-only worktree
// reclaim pass. It is the advancement-retry set PLUS cancelled: a job aborted
// (cancel / kill / supersede) before its terminal AdvanceJob leaves a
// dispatch-allocated read-only worktree (#739) on disk with a
// delegation_worktree_cleanup_skipped marker but a JobCancelled state, so the
// reclaim pass must still dispose it. Cancelled is intentionally NOT added to
// jobStateCanRetryAdvancement (a cancelled job must never RE-ADVANCE) — only its
// worktree is reclaimed here, via the same idempotent, liveness-gated cleanup.
func jobStateEligibleForWorktreeReclaim(state string) bool {
	return jobStateCanRetryAdvancement(state) || state == string(workflow.JobCancelled)
}

// retryPendingJobComments re-posts the result comment for any terminal job whose
// latest comment event is comment_post_failed. Like retryPendingJobAdvancements it
// is BOUNDED (#598): it asks the store for ONLY the jobs whose latest comment event
// is a failure marker instead of listing EVERY job and re-reading each terminal
// job's full event history on every 1s worker tick. Each candidate is re-verified
// with the Go predicate jobNeedsCommentRetry, so behavior is identical. Comment
// retries never touch a checkout, so (unlike advancements) they take no checkoutHeld
// gate.
func retryPendingJobComments(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, cand *tickCandidates) error {
	jobIDs, err := cand.commentRetryCandidates(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		job, err := worker.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			// A marker event with no surviving job row (e.g. a pruned job): skip
			// rather than abort the tick.
			continue
		}
		if err != nil {
			return err
		}
		if !jobStateCanRetryComment(job.State) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		needsRetry, err := worker.jobNeedsCommentRetry(ctx, job.ID)
		if err != nil {
			return err
		}
		if !needsRetry {
			continue
		}
		agent := runtime.Agent{Name: job.Agent}
		if dbAgent, err := worker.Store.GetAgent(ctx, job.Agent); err == nil {
			agent = runtimeAgent(dbAgent)
		}
		if err := worker.postJobResultComment(ctx, job.ID, agent, "", nil); err != nil {
			return err
		}
	}
	return nil
}

func jobStateCanRetryComment(state string) bool {
	switch state {
	case string(workflow.JobSucceeded), string(workflow.JobFailed), string(workflow.JobBlocked):
		return true
	default:
		return false
	}
}

// dispatchLimitObserver, when non-nil, is invoked with the concurrency limit that
// each repo dispatch pass actually uses, at the exact point production dispatch
// reads it. Test-only seam (#577): it lets a warm-reload E2E prove a SIGHUP change
// to the live worker count is what the RUNNING dispatch reads on its next pass,
// without a restart. It is nil in production, so the dispatch path is byte-identical.
var dispatchLimitObserver func(limit int)

func runQueuedJobsForRepo(ctx context.Context, worker jobWorker, limit int, repoFilter string, rootFilter string) error {
	if obs := dispatchLimitObserver; obs != nil {
		obs(limit)
	}
	if limit <= 0 {
		return nil
	}
	// Preflight (#444): if the config can't actually run same-repo jobs in
	// parallel (single worker, or the per-tick barrier scheduler) yet ≥2
	// parallelizable jobs are queued, surface the exact relaunch command instead
	// of silently serializing them. "Parallelizable" = same repo, dep-unblocked
	// (already true of queued jobs), and DISTINCT runtime sessions — same-session
	// jobs serialize on the runtime session lock even under pool, so counting raw
	// same-repo jobs would over-warn.
	if serializingConfig(worker.UsePool, limit) {
		warnSerializedParallelJobs(ctx, worker, limit, repoFilter, rootFilter)
	}
	if worker.UsePool {
		return runQueuedJobsForRepoPool(ctx, worker, limit, repoFilter, rootFilter)
	}
	pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, true)
	if err != nil {
		return err
	}
	for len(pending) > 0 {
		policy, err := worker.parallelSessionPolicy()
		if err != nil {
			policy = config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue}
		}
		queued, remaining := selectRunnableQueuedJobsWithPolicy(ctx, worker.Store, pending, limit, policy)
		if len(queued) == 0 {
			return nil
		}
		pending = remaining

		// Host-global admission gate (#365): reserve a session slot + RAM estimate
		// for each selected job BEFORE dispatching it. A job that does not fit the
		// budget is left queued — defer it back to `pending` so it is retried on the
		// next loop iteration once this batch's reservations are released in the
		// goroutine defers (worker.Admission is nil ⇒ Reserve always admits, so the
		// default path is byte-identical). If nothing was admitted this pass we
		// return: the deferred jobs stay queued in the DB for the next daemon tick,
		// when a freed slot can admit them (avoids spinning on an unfittable job).
		admitted := make([]db.Job, 0, len(queued))
		for _, job := range queued {
			job := job
			if worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
				admitted = append(admitted, job)
				continue
			}
			pending = append([]db.Job{job}, pending...)
		}
		if len(admitted) == 0 {
			return nil
		}

		errs := make(chan error, len(admitted))
		var wg sync.WaitGroup
		for _, job := range admitted {
			job := job
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer worker.Admission.Release(job.ID)
				errs <- worker.run(ctx, job)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && !errors.Is(err, errRuntimeSessionBusy) {
				return err
			}
		}
	}
	return nil
}

// serializingConfig reports whether the daemon's scheduler config cannot run
// same-repo jobs in parallel (#444): a single worker, or the per-tick barrier
// scheduler (which serializes same-repo jobs on one wg.Wait() + checkout lock).
func serializingConfig(usePool bool, limit int) bool {
	return limit <= 1 || !usePool
}

// parallelizableSerialJobs counts the queued jobs for this repo/session filter
// that could run concurrently but won't under a serializing config (#444):
// distinct runtime sessions among same-repo dep-unblocked queued jobs. Jobs with
// no resolvable runtime session key are counted individually (each is its own
// would-be parallel slot). The count is what the preflight warns on (≥2). The
// returned signature uniquely identifies the parallelizable session set so the
// preflight can de-duplicate an unchanged backlog across ticks.
func parallelizableSerialJobs(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string) (int, string) {
	// forDispatch=false: this is a preflight COUNT for the serialization warning, not
	// a dispatch. It must stay a pure read — no live auth probe (`claude -p`
	// subprocess) and no payload mutation — so the warning path keeps its documented
	// off-hot-path contract (#532).
	pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, false)
	if err != nil {
		return 0, ""
	}
	// Cheap short-circuit: with fewer than 2 pending same-repo jobs there can
	// never be ≥2 parallelizable slots, so skip the per-job session lookups
	// (queuedJobRuntimeResourceKey → Store.GetAgent) entirely. This keeps the
	// common-case (default single-worker, empty/small backlog) off the DB hot
	// path beyond the single ListQueuedJobs the listing already performs.
	if len(pending) < 2 {
		return 0, ""
	}
	sessions := map[string]bool{}
	for _, job := range pending {
		key := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
		if key == "" {
			// Each session-less job is its own parallel slot; key it by job ID
			// so the dedup signature still reflects backlog changes. The job-ID
			// key already makes it a distinct entry in `sessions`, so it must NOT
			// also be counted separately or the slot would be double-counted.
			sessions["job:"+job.ID] = true
			continue
		}
		sessions[key] = true
	}
	count := len(sessions)
	if count < 2 {
		return count, ""
	}
	keys := make([]string, 0, len(sessions))
	for k := range sessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return count, strings.Join(keys, "\n")
}

// preflightWarnThrottle de-duplicates the serializing-config preflight warning
// (#444) across worker ticks. runQueuedJobsForRepo is called once per poll
// (default 30s), so a steady backlog under a serializing config would otherwise
// re-log the identical line every tick. We re-emit only when the parallelizable
// session set changes or a quiet interval has elapsed, keyed by repo filter.
type preflightWarnState struct {
	signature string
	at        time.Time
}

var (
	preflightWarnMu     sync.Mutex
	preflightWarnByRepo = map[string]preflightWarnState{}
	preflightWarnReWarn = 30 * time.Minute
)

// resetPreflightWarnThrottle clears the preflight de-dup state. Test-only.
func resetPreflightWarnThrottle() {
	preflightWarnMu.Lock()
	defer preflightWarnMu.Unlock()
	preflightWarnByRepo = map[string]preflightWarnState{}
}

// shouldEmitPreflightWarn reports whether the warning for this repo/signature
// should be emitted now, recording the decision so an unchanged backlog stays
// quiet until either the session set changes or preflightWarnReWarn elapses.
func shouldEmitPreflightWarn(repoKey string, signature string, now time.Time) bool {
	preflightWarnMu.Lock()
	defer preflightWarnMu.Unlock()
	prev, ok := preflightWarnByRepo[repoKey]
	if ok && prev.signature == signature && now.Sub(prev.at) < preflightWarnReWarn {
		return false
	}
	preflightWarnByRepo[repoKey] = preflightWarnState{signature: signature, at: now}
	return true
}

// warnSerializedParallelJobs emits an actionable preflight warning when ≥2
// parallelizable jobs are queued under a serializing config (#444), printing the
// exact relaunch command. It is best-effort and never blocks the tick, and is
// rate-limited so an unchanged backlog does not re-log every poll.
func warnSerializedParallelJobs(ctx context.Context, worker jobWorker, limit int, repoFilter string, rootFilter string) {
	count, signature := parallelizableSerialJobs(ctx, worker, repoFilter, rootFilter)
	if count < 2 {
		return
	}
	repo := strings.TrimSpace(repoFilter)
	target := "the daemon"
	repoKey := "*"
	if repo != "" {
		target = repo
		repoKey = repo
	}
	if !shouldEmitPreflightWarn(repoKey, signature, time.Now()) {
		return
	}
	workers := limit
	if workers < count {
		workers = count
	}
	writeLine(worker.Stdout, "warning: %d parallelizable jobs queued for %s will run serially under the current scheduler config; relaunch with: gitmoot daemon restart --parallel %d", count, target, workers)
	writeLine(worker.Stdout, "         %s", daemonRestartEnvCaveat)
}

// daemonRestartEnvCaveat is appended to the serialized-jobs relaunch hint.
// Runtime auth reloads per delivery; only scheduler state is restart-sensitive.
const daemonRestartEnvCaveat = "note: Claude runtime auth is read per delivery from runtime-auth.env and does not require a restart; a restart resets in-flight scheduler state."

// listPendingQueuedJobs returns the queued jobs eligible to run for this
// repo/session filter, dropping children of a killed root.
//
// Operator kill switch (#341): once a tree's root is killed, do not start any of
// its queued children. The coordinator's own continuation still runs so the
// engine can route through the graceful finalize path; in-flight children finish
// normally. Only children (payload.RootJobID points at another root) are skipped
// here — the root job itself is never skipped.
//
// forDispatch (#532 slice B) gates the LIVE runtime_auth credential probe: only a
// caller that is actually about to dispatch jobs runs it. The preflight
// serialization-warning path (parallelizableSerialJobs) passes false so counting
// queued jobs stays a pure read — no `claude -p` subprocess, no job-payload
// mutation — keeping that path off the DB/subprocess hot path as its contract
// promises. A within-pass cache dedupes the probe across auth-held jobs of the same
// runtime so one outage costs at most one live probe per pass.
func listPendingQueuedJobs(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, forDispatch bool) ([]db.Job, error) {
	jobs, err := worker.Store.ListQueuedJobs(ctx)
	if err != nil {
		return nil, err
	}
	var probeCache authProbeCache
	if forDispatch {
		probeCache = authProbeCache{}
	}
	pending := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if queuedChildOfKilledRoot(ctx, worker.Store, job) {
			continue
		}
		// Operational-blocker hold (#532): a job deferred behind a classified
		// blocker is not eligible until its earliest-retry-at passes. Both
		// schedulers (barrier and pool) funnel through this listing, so the hold
		// is honored everywhere; jobs without the payload field are unaffected.
		if queuedJobBlockerHeld(job, time.Now().UTC()) {
			continue
		}
		// Auth-probe gate (#532 slice B): once a runtime_auth deferral's coarse hold
		// elapses, only re-dispatch when a live doctor-style probe says the credential
		// is VALID again — an Invalid verdict extends the hold (re-probe next cadence,
		// no attempt burned). Non-auth deferrals and jobs with no probe wired pass
		// straight through (coarse cadence only, byte-identical to slice A). The live
		// probe runs ONLY for a dispatching caller (forDispatch); the warning-count
		// path skips it so it never spawns a subprocess or mutates the payload.
		if forDispatch && !authProbeAllowsRedispatch(ctx, worker, job, time.Now().UTC(), probeCache) {
			continue
		}
		pending = append(pending, job)
	}
	return pending, nil
}

// runQueuedJobsForRepoPool is the opt-in (--scheduler=pool) continuous scheduler
// for #394. Unlike the per-tick barrier it never blocks the tick on a whole
// batch: it keeps up to `limit` workers busy and RE-QUERIES the queue as each
// worker frees, so a job queued *after* dispatch began (e.g. a running job that
// kicks off a follow-up same-repo job and polls it) is picked up without waiting
// for the in-flight batch to drain (layer 1).
//
// Working-tree safety is preserved by live in-flight checkout accounting: a job
// whose checkout key is already held by a running job is never dispatched
// concurrently (layer 2). Same-repo no-worktree jobs therefore still serialize;
// only distinct checkout keys (e.g. isolated worktrees) run in parallel — a
// follow-up PR makes the awaited follow-up carry one so the chain can complete.
//
// inflightCheckouts/inflightRuntimes/running/firstErr are owned solely by this
// dispatcher goroutine; worker goroutines communicate only via the done channel,
// so no lock is required.
func runQueuedJobsForRepoPool(ctx context.Context, worker jobWorker, limit int, repoFilter string, rootFilter string) error {
	return runQueuedJobsForRepoPoolTracked(ctx, worker, limit, limit, repoFilter, rootFilter, nil)
}

// poolDispatchSlots is a pool pass's dispatch budget: the repo's free slots,
// clamped by the host-global remainder (hostCap − tracked in-flight across ALL
// repos) so concurrent per-repo passes never exceed the daemon-wide cap. With a
// nil tracker total() is 0 and hostCap == limit, so the clamp is inert and the
// result is exactly the historical limit − running.
func poolDispatchSlots(limit, running, hostCap int, tracker *inflightJobTracker) int {
	slots := limit - running
	if hostSlots := hostCap - tracker.total(); hostSlots < slots {
		slots = hostSlots
	}
	return slots
}

// runQueuedJobsForRepoPoolTracked is the pool pass with the supervisor's
// in-flight tracker mirrored in (#562): each dispatched job is registered so the
// poller/maintenance gates and the shutdown drain see pool work, the tracker's
// keys are unioned into the selection seeds so a pool pass never dispatches
// beside a tracked non-pool job holding the same checkout/runtime key (a warm
// scheduler flip mid-run), and jobs already in flight are filtered out. hostCap
// additionally clamps each dispatch by the tracker's HOST-global in-flight
// count, so concurrent per-repo pool passes cannot multiply --workers by the
// number of enabled repos. A nil tracker (hostCap == limit, total() == 0) is
// byte-identical to the historical pool.
func runQueuedJobsForRepoPoolTracked(ctx context.Context, worker jobWorker, limit int, hostCap int, repoFilter string, rootFilter string, tracker *inflightJobTracker) error {
	if limit <= 0 {
		return nil
	}
	policy, perr := worker.parallelSessionPolicy()
	if perr != nil {
		policy = config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue}
	}

	type finished struct {
		jobID        string
		checkoutKey  string
		runtimeKey   string
		worktreePath string
		repoCheckout string
		// payloadBeforeIsolation is the job's payload as it was before an
		// isolation worktree was allocated and written into it; non-empty only
		// for isolation-dispatched jobs.
		payloadBeforeIsolation string
		err                    error
	}
	inflightCheckouts := map[string]bool{}
	inflightRuntimes := map[string]bool{}
	running := 0
	// bouncedBusy / bouncedBusyRuntimes track jobs (and their runtime-session keys)
	// that returned errRuntimeSessionBusy earlier in THIS pool invocation (#598).
	// A busy job re-queues immediately once reaped, so without this it was
	// re-selected and re-dispatched every pass in a tight spin (~36 attempts/s,
	// poisoning job_events with runtime_lock_wait rows). Dispatcher-goroutine-owned
	// (like inflightCheckouts), so no lock; reset each invocation, so a bounced job
	// is retried on a later worker tick.
	bouncedBusy := map[string]bool{}
	bouncedBusyRuntimes := map[string]bool{}
	// runtimeKeyMemo caches queuedJobRuntimeResourceKey per job id for the lifetime of
	// this dispatcher invocation (#615 review). excludeBouncedBusy re-derives the key
	// for every still-pending job on every dispatch pass, and each miss is a GetAgent
	// read, so a job that stays pending across N passes otherwise costs N GetAgent
	// reads. The key is stable while a job sits queued (it depends only on the job's
	// agent + payload runtime override), so caching it bounds the cost at one read per
	// job per invocation. Dispatcher-goroutine-owned like the sets above.
	runtimeKeyMemo := map[string]string{}
	done := make(chan finished, limit)
	var firstErr error

	reap := func(f finished) {
		delete(inflightCheckouts, f.checkoutKey)
		if f.runtimeKey != "" {
			delete(inflightRuntimes, f.runtimeKey)
		}
		// An isolation-dispatched job that bounced errRuntimeSessionBusy was never
		// claimed and stays queued — but its payload was rewritten to point at the
		// isolation worktree this reap is about to delete. Restore the
		// pre-isolation payload (best-effort, non-cancellable like the worktree
		// removal) so its next dispatch re-evaluates cleanly instead of
		// preflight-failing terminally on a reaped path. Done before tracker.end
		// so no other selector can re-dispatch it mid-restore.
		if f.payloadBeforeIsolation != "" && errors.Is(f.err, errRuntimeSessionBusy) {
			_ = worker.Store.UpdateJobPayload(context.WithoutCancel(ctx), f.jobID, f.payloadBeforeIsolation)
		} else if f.payloadBeforeIsolation != "" {
			// Operational-blocker deferral (#532) × pool isolation: a deferred job
			// is queued again but its payload still points at the isolation
			// worktree this reap is about to delete. Restore the pre-isolation
			// payload (carrying the blocker hold fields over) so its re-dispatch
			// after the hold re-evaluates cleanly instead of preflight-failing on
			// a reaped path. No-op for any job that is not queued with a blocker
			// hold, so terminal outcomes are byte-identical.
			restorePreIsolationPayloadForDeferredJob(context.WithoutCancel(ctx), worker.Store, f.jobID, f.payloadBeforeIsolation)
		}
		tracker.end(f.jobID)
		// Release the host-global admission reservation (#365) keyed by job ID,
		// alongside the checkout/runtime release. Release is idempotent and a nil
		// budget is a no-op, so this is safe on every reap path incl. panic
		// recovery and shutdown (mirrors the worktree cleanup discipline).
		worker.Admission.Release(f.jobID)
		running--
		// Dispose an auto-created isolation worktree (#394 part 2). Best-effort and
		// on a non-cancellable context so it still runs during daemon shutdown; both
		// the add (in allocatePoolIsolationWorktree) and this remove run on the
		// dispatcher goroutine under the tick's per-repo lock, so they never race.
		if f.worktreePath != "" && f.repoCheckout != "" {
			_ = gitutil.Client{Dir: f.repoCheckout}.RemoveWorktreeForce(context.WithoutCancel(ctx), f.worktreePath)
		}
		if f.err != nil && firstErr == nil && !errors.Is(f.err, errRuntimeSessionBusy) {
			firstErr = f.err
		}
		// A job that bounced busy must not be re-selected/re-dispatched again in
		// THIS invocation — by id, and by runtime-session key so every sibling
		// contending the same busy session is held back too (#598). It stays queued
		// and is retried on a later worker tick (a fresh invocation with fresh sets).
		if errors.Is(f.err, errRuntimeSessionBusy) {
			bouncedBusy[f.jobID] = true
			if f.runtimeKey != "" {
				bouncedBusyRuntimes[f.runtimeKey] = true
			}
		}
	}

	for {
		// Reap finished workers (non-blocking) so freed checkout keys and slots are
		// visible to this dispatch pass.
		for reaping := true; reaping; {
			select {
			case f := <-done:
				reap(f)
			default:
				reaping = false
			}
		}

		// Stop dispatching promptly on cancellation rather than relying on the next
		// store query to observe it; in-flight workers return as their own ctx is
		// cancelled (parity with the barrier's wg.Wait()), then we drain and exit.
		if firstErr == nil && ctx.Err() != nil {
			firstErr = ctx.Err()
		}

		dispatched := 0
		if firstErr == nil {
			pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, true)
			if err != nil {
				firstErr = err
			} else if slots := poolDispatchSlots(limit, running, hostCap, tracker); slots > 0 {
				// Drop jobs that already bounced busy this invocation before ANY
				// selection (#598). pending is fresh each pass and feeds BOTH the
				// primary selection and the isolation `remaining` loop below, so
				// filtering it here excludes bounced jobs from both. A bounced job
				// removed from pending is never re-selected, so dispatched is not
				// re-incremented for it: once every remaining pending job is
				// busy-excluded the loop reaches dispatched==0 && running==0 and
				// returns, so "busy must not count as progress" holds structurally.
				pending = excludeBouncedBusy(ctx, worker, pending, bouncedBusy, bouncedBusyRuntimes, runtimeKeyMemo)
				// Union in the supervisor tracker's in-flight keys (#562): a tracked
				// non-pool job (e.g. dispatched before a warm scheduler flip) must
				// block same-key pool dispatch exactly like a pool-local one. Jobs
				// already in flight anywhere in this process are filtered by ID.
				// The union is a fresh per-pass COPY: the pool's own maps stay
				// reap-owned, so a foreign key never lingers after its job ends.
				seedCheckouts, seedRuntimes := inflightCheckouts, inflightRuntimes
				if tracker != nil {
					trackerCheckouts, trackerRuntimes := tracker.seeds()
					eligible := pending[:0]
					for _, job := range pending {
						if !tracker.inflightJob(job.ID) {
							eligible = append(eligible, job)
						}
					}
					pending = eligible
					seedCheckouts = unionStringSets(inflightCheckouts, trackerCheckouts)
					seedRuntimes = unionStringSets(inflightRuntimes, trackerRuntimes)
				}
				queued, remaining := selectRunnableQueuedJobsSeeded(ctx, worker.Store, pending, slots, policy, seedCheckouts, seedRuntimes)
				for _, job := range queued {
					job := job
					// Host-global admission gate (#365): reserve a session slot + RAM
					// estimate before claiming any checkout/runtime key or a worker slot.
					// A job that does not fit the budget is skipped (left queued) and the
					// pool re-queries on the next pass once a reap frees a slot — never
					// failed/dropped. A nil budget always admits ⇒ byte-identical default.
					if !worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
						if tracker != nil {
							warnJobHeldBack(worker.Stdout, job.ID, admissionSkipReason(worker.Admission, worker.admissionEstimate(ctx, job)))
						}
						continue
					}
					checkoutKey := queuedJobCheckoutKey(ctx, worker.Store, job)
					runtimeKey := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
					// beginWithin re-checks the host-global cap atomically with
					// registration: a concurrent pass for another repo may have consumed
					// the headroom this pass's slot computation saw.
					if !tracker.beginWithin(hostCap, job.ID, repoFilter, checkoutKey, runtimeKey) {
						worker.Admission.Release(job.ID)
						continue
					}
					inflightCheckouts[checkoutKey] = true
					if runtimeKey != "" {
						inflightRuntimes[runtimeKey] = true
					}
					running++
					dispatched++
					go func() {
						done <- finished{jobID: job.ID, checkoutKey: checkoutKey, runtimeKey: runtimeKey, err: runPoolJobRecovered(ctx, worker, job)}
					}()
				}
				// #394 part 2: a read-only job left blocked ONLY by a contended same-repo
				// checkout (its repo:<repo> key is held by an in-flight job) can run beside
				// the holder in an auto-created detached worktree — the distinct
				// worktree:<path> key is safe to parallelize. This is what lets an awaited
				// same-repo follow-up (the #394 deadlock) make progress.
				// The checks below consult the LIVE inflightCheckouts/inflightRuntimes
				// maps as well as the seed unions: seedCheckouts/seedRuntimes are
				// per-pass COPIES when a tracker is present, so a job dispatched by the
				// loop above (which mutates only the live maps) would otherwise be
				// invisible here — letting a same-runtime-session job be
				// isolation-dispatched beside its just-started sibling. The loser of
				// that session-lock race would bounce busy AFTER its payload was
				// rewritten to the isolation worktree, which reap() then deletes,
				// leaving a queued job pointing at a reaped path that terminally fails
				// on its next run. With a nil tracker the seed and live maps are the
				// same object, so this is byte-identical to the historical pool.
				for _, job := range remaining {
					if running >= limit || tracker.total() >= hostCap {
						break
					}
					payload, perr := daemonJobPayload(job)
					if perr != nil || !poolIsolationEligible(job, payload) {
						continue
					}
					if queuedJobCheckoutKey(ctx, worker.Store, job) != "repo:"+payload.Repo ||
						!(inflightCheckouts["repo:"+payload.Repo] || seedCheckouts["repo:"+payload.Repo]) {
						continue // not blocked by a contended same-repo checkout
					}
					runtimeKey := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
					if runtimeKey != "" && (inflightRuntimes[runtimeKey] || seedRuntimes[runtimeKey] || runtimeResourceLocked(ctx, worker.Store, runtimeKey)) {
						continue // also runtime-contended; leave it to the runtime/temp-worker path
					}
					// Host-global admission gate (#365): reserve before creating the
					// isolation worktree so a deferred job leaves no orphan worktree behind.
					if !worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
						if tracker != nil {
							warnJobHeldBack(worker.Stdout, job.ID, admissionSkipReason(worker.Admission, worker.admissionEstimate(ctx, job)))
						}
						continue
					}
					payloadBeforeIsolation := job.Payload
					iso, ok, allocErr := worker.allocatePoolIsolationWorktree(ctx, job, payload)
					if !ok {
						worker.Admission.Release(job.ID)
						// #739: the reactive isolation was silent on failure — the exact
						// reason #739 was hard to diagnose (a seat went queued→running with no
						// worktree event and serialized on the shared checkout). Emit a loud
						// skip event so a lost-parallelism serialize is observable. A nil
						// allocErr means the job was simply not isolable (no home/checkout) —
						// not a failure — so stay quiet there.
						if allocErr != nil {
							_ = worker.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "pool_isolation_skipped", Message: fmt.Sprintf("pool read-only isolation skipped (#739); job stays serialized in the shared checkout: %v", allocErr)})
						}
						continue
					}
					if !tracker.beginWithin(hostCap, iso.job.ID, repoFilter, iso.checkoutKey, iso.runtimeKey) {
						worker.Admission.Release(iso.job.ID)
						// Undo the allocation completely: the payload now points at the
						// isolation worktree being removed, and the job stays queued — a
						// host-cap or double-dispatch refusal must not strand it on a
						// reaped path.
						_ = worker.Store.UpdateJobPayload(context.WithoutCancel(ctx), iso.job.ID, payloadBeforeIsolation)
						_ = gitutil.Client{Dir: iso.repoCheckout}.RemoveWorktreeForce(context.WithoutCancel(ctx), iso.worktreePath)
						continue
					}
					inflightCheckouts[iso.checkoutKey] = true
					if iso.runtimeKey != "" {
						inflightRuntimes[iso.runtimeKey] = true
					}
					// #739: make the reactive isolation observable on SUCCESS too (it was
					// silent both ways). Emitted only past the host-cap/double-dispatch gate
					// above, so the event means the job is truly dispatched in its own
					// worktree:<path> key — running beside the same-repo checkout holder,
					// not serialized behind it.
					_ = worker.Store.AddJobEvent(ctx, db.JobEvent{JobID: iso.job.ID, Kind: "pool_isolation_worktree_allocated", Message: fmt.Sprintf("read-only pool-isolation worktree %s allocated (#739); job keyed %s to run beside the same-repo checkout holder", iso.worktreePath, iso.checkoutKey)})
					running++
					dispatched++
					go func() {
						done <- finished{jobID: iso.job.ID, checkoutKey: iso.checkoutKey, runtimeKey: iso.runtimeKey, worktreePath: iso.worktreePath, repoCheckout: iso.repoCheckout, payloadBeforeIsolation: payloadBeforeIsolation, err: runPoolJobRecovered(ctx, worker, iso.job)}
					}()
				}
			}
		}

		if running == 0 {
			// Nothing running: if we also dispatched nothing this pass the queue is
			// drained (or everything left is un-runnable for now) — return, surfacing
			// any worker error. On firstErr we reach here once inflight has drained.
			if dispatched == 0 {
				return firstErr
			}
			continue
		}
		if dispatched == 0 {
			// No progress is possible until a running worker frees a resource; block
			// for one, then re-query (which may now include newly-queued jobs).
			reap(<-done)
		}
	}
}

// runPoolJobRecovered runs a pool job and converts a panic into an error so the
// worker goroutine ALWAYS sends its result to the done channel. This keeps the
// pool's resource accounting and worktree cleanup (in reap) intact even on a
// panicking job, and prevents one bad job from crashing an unattended daemon.
func runPoolJobRecovered(ctx context.Context, worker jobWorker, job db.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pool worker panicked on job %s: %v", job.ID, r)
		}
	}()
	return worker.run(ctx, job)
}

// poolIsolationEligible reports whether a queued job blocked by a contended
// same-repo checkout key may be safely run in an ephemeral detached worktree
// (#394 part 2). Scope: read-only actions (ask/review) with no existing worktree.
// implement jobs are excluded — they either already carry a task worktree
// (already keyed) or must not run detached without the finalize/merge wiring.
func poolIsolationEligible(job db.Job, payload workflow.JobPayload) bool {
	switch strings.TrimSpace(job.Type) {
	case "ask", "review", "produce":
	default:
		return false
	}
	return strings.TrimSpace(payload.WorktreePath) == "" && strings.TrimSpace(payload.TaskID) == ""
}

type poolIsolatedDispatch struct {
	job          db.Job
	checkoutKey  string
	runtimeKey   string
	worktreePath string
	repoCheckout string
}

// allocatePoolIsolationWorktree creates a detached read-only worktree for a
// read-capable job otherwise blocked behind a contended same-repo checkout,
// rewrites the job's payload to run in it (so its checkout key becomes
// worktree:<path>), and returns the dispatch handle incl. cleanup info. ok=false
// means the job is not isolable or the worktree could not be created — the caller
// then leaves it queued to serialize as before (graceful, no deadlock-for-safety
// trade). Runs on the dispatcher goroutine under the tick's per-repo lock.
func (w jobWorker) allocatePoolIsolationWorktree(ctx context.Context, job db.Job, payload workflow.JobPayload) (poolIsolatedDispatch, bool, error) {
	if strings.TrimSpace(w.ConfigHome) == "" {
		return poolIsolatedDispatch{}, false, nil
	}
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil || strings.TrimSpace(repoRecord.CheckoutPath) == "" {
		return poolIsolatedDispatch{}, false, err
	}
	client := gitutil.Client{Dir: repoRecord.CheckoutPath}
	// #739: route through the shared read-only allocator so this reactive top-level
	// isolation path resolves the ref to HEAD (a committed tip that is always
	// resolvable — NOT the stale current branch the researchers flagged), holds the
	// checkout mutation lock, and returns errors LOUDLY. This keeps it behaviorally
	// aligned with the read-only delegation fan-out and the dispatch-time allocation,
	// and turns the previously-silent worktree-add failure into a returned error the
	// caller emits as a pool_isolation_skipped event. It runs SYNCHRONOUSLY on the
	// per-repo dispatch loop, so it passes the short ReadOnlyWorktreeDispatchLockWaitBudget
	// (not the 2-minute default) to fail open fast under merge-gate lock contention
	// rather than freezing this repo's dispatch+reap loop.
	path, err := workflow.AllocateReadOnlyWorktree(ctx, w.Store, w.ConfigHome, payload.Repo, repoRecord.CheckoutPath, job.ID, "pool-isolation", 0, "", workflow.ReadOnlyWorktreeDispatchLockWaitBudget, client)
	if err != nil {
		return poolIsolatedDispatch{}, false, err
	}
	if strings.TrimSpace(path) == "" {
		return poolIsolatedDispatch{}, false, nil
	}
	payload.WorktreePath = path
	// The detached worktree is the COMMITTED TIP of the base ref, so it omits
	// gitignored paths (e.g. vendored repos/**) and uncommitted working-tree
	// changes. Point the isolated read-only job at the canonical repo checkout so an
	// analysis task does not silently report working-tree state as missing (#654),
	// exactly as read-only delegation fan-out does (engine.go, #394 part 2). Append
	// to Instructions so the note is carried in the delivered prompt; the reap path
	// restores payloadBeforeIsolation on a bounce/defer, reverting this too. A blank
	// checkout path yields "" ⇒ byte-identical (no note).
	if note := workflow.ReadOnlyWorktreeContextNote(repoRecord.CheckoutPath); note != "" {
		payload.Instructions += note
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		_ = client.RemoveWorktreeForce(context.WithoutCancel(ctx), path)
		return poolIsolatedDispatch{}, false, err
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		_ = client.RemoveWorktreeForce(context.WithoutCancel(ctx), path)
		return poolIsolatedDispatch{}, false, err
	}
	job.Payload = string(encoded)
	return poolIsolatedDispatch{
		job:          job,
		checkoutKey:  queuedJobCheckoutKey(ctx, w.Store, job),
		runtimeKey:   queuedJobRuntimeResourceKey(ctx, w.Store, job),
		worktreePath: path,
		repoCheckout: repoRecord.CheckoutPath,
	}, true, nil
}

type queuedJobResourceSelector struct {
	limit            int
	policy           config.ParallelSessionPolicy
	checkouts        map[string]bool
	runtimes         map[string]bool
	tempReservations map[string]int
}

func selectRunnableQueuedJobs(ctx context.Context, store *db.Store, pending []db.Job, limit int) ([]db.Job, []db.Job) {
	return selectRunnableQueuedJobsWithPolicy(ctx, store, pending, limit, config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue})
}

func selectRunnableQueuedJobsWithPolicy(ctx context.Context, store *db.Store, pending []db.Job, limit int, policy config.ParallelSessionPolicy) ([]db.Job, []db.Job) {
	return selectRunnableQueuedJobsSeeded(ctx, store, pending, limit, policy, nil, nil)
}

// selectRunnableQueuedJobsSeeded is selectRunnableQueuedJobsWithPolicy with the
// checkout/runtime resource sets pre-seeded from already-running jobs. The
// barrier path passes nil seeds (empty, == the original behavior); the pool path
// (#394) seeds the live in-flight keys so a job whose checkout key is already
// held by a running job is not selected. The seed maps are copied, never mutated.
func selectRunnableQueuedJobsSeeded(ctx context.Context, store *db.Store, pending []db.Job, limit int, policy config.ParallelSessionPolicy, seedCheckouts map[string]bool, seedRuntimes map[string]bool) ([]db.Job, []db.Job) {
	if limit <= 0 {
		return nil, pending
	}
	selector := queuedJobResourceSelector{
		limit:            limit,
		policy:           policy,
		checkouts:        copyStringSet(seedCheckouts),
		runtimes:         copyStringSet(seedRuntimes),
		tempReservations: map[string]int{},
	}
	queued := make([]db.Job, 0, min(limit, len(pending)))
	remaining := make([]db.Job, 0, len(pending))
	for _, job := range pending {
		if selector.selects(ctx, store, job, len(queued)) {
			queued = append(queued, job)
			continue
		}
		remaining = append(remaining, job)
	}
	return queued, remaining
}

func copyStringSet(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		if v {
			dst[k] = true
		}
	}
	return dst
}

// excludeBouncedBusy drops queued jobs that already bounced errRuntimeSessionBusy
// earlier in THIS pool invocation — by job id, and by runtime-session key so every
// job contending the same busy session is held back too (#598). They stay queued
// and are retried on a later worker tick (a fresh invocation, fresh sets), instead
// of being re-selected every pass in a tight, event-table-poisoning spin. Empty
// sets ⇒ pending is returned unchanged, so the no-busy common case pays nothing.
//
// runtimeKeyMemo caches queuedJobRuntimeResourceKey across the invocation's dispatch
// passes so each still-pending job costs at most one GetAgent read per invocation
// (#615 review) rather than one per pass; a nil memo disables caching.
func excludeBouncedBusy(ctx context.Context, worker jobWorker, pending []db.Job, bouncedIDs, bouncedRuntimes map[string]bool, runtimeKeyMemo map[string]string) []db.Job {
	if len(bouncedIDs) == 0 && len(bouncedRuntimes) == 0 {
		return pending
	}
	kept := pending[:0]
	for _, job := range pending {
		if bouncedIDs[job.ID] {
			continue
		}
		if len(bouncedRuntimes) > 0 {
			if rk := memoizedRuntimeResourceKey(ctx, worker.Store, job, runtimeKeyMemo); rk != "" && bouncedRuntimes[rk] {
				continue
			}
		}
		kept = append(kept, job)
	}
	return kept
}

// memoizedRuntimeResourceKey returns queuedJobRuntimeResourceKey for job, caching the
// result in memo keyed by job id so repeated lookups for the same job across a
// dispatcher invocation's passes reuse the single GetAgent read. A nil memo bypasses
// the cache and calls through directly.
func memoizedRuntimeResourceKey(ctx context.Context, store *db.Store, job db.Job, memo map[string]string) string {
	if memo == nil {
		return queuedJobRuntimeResourceKey(ctx, store, job)
	}
	if key, ok := memo[job.ID]; ok {
		return key
	}
	key := queuedJobRuntimeResourceKey(ctx, store, job)
	memo[job.ID] = key
	return key
}

func (s queuedJobResourceSelector) selects(ctx context.Context, store *db.Store, job db.Job, selected int) bool {
	if selected >= s.limit {
		return false
	}
	checkoutKey := queuedJobCheckoutKey(ctx, store, job)
	runtimeKey := queuedJobRuntimeResourceKey(ctx, store, job)
	if s.checkouts[checkoutKey] {
		return false
	}
	runtimeAlreadySelected := runtimeKey != "" && s.runtimes[runtimeKey]
	runtimeAlreadyLocked := runtimeKey != "" && !runtimeAlreadySelected && runtimeResourceLocked(ctx, store, runtimeKey)
	if runtimeAlreadySelected || runtimeAlreadyLocked {
		if !s.canUseTempWorker(ctx, store, job) && runtimeAlreadySelected {
			return false
		}
	}
	s.checkouts[checkoutKey] = true
	if runtimeKey != "" {
		s.runtimes[runtimeKey] = true
	}
	return true
}

func runtimeResourceLocked(ctx context.Context, store *db.Store, runtimeKey string) bool {
	if store == nil || strings.TrimSpace(runtimeKey) == "" {
		return false
	}
	_, err := store.GetResourceLock(ctx, runtimeKey)
	return err == nil
}

func (s queuedJobResourceSelector) canUseTempWorker(ctx context.Context, store *db.Store, job db.Job) bool {
	if store == nil {
		return false
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return false
	}
	dbAgent, err := store.GetAgent(ctx, job.Agent)
	if err != nil {
		return false
	}
	agent := runtimeAgent(dbAgent)
	typ := tempWorkerAgentType(agent.Name)
	count, err := store.CountActiveAgentInstances(ctx, typ, agent.AutonomyPolicy, time.Now().UTC())
	if err != nil {
		return false
	}
	if count+s.tempReservations[typ] >= s.policy.MaxTempSessionsPerAgent {
		return false
	}
	eligible := tempWorkerEligible(ctx, store, job, payload, agent, s.policy, time.Now().UTC())
	if !eligible.Eligible {
		return false
	}
	s.tempReservations[typ]++
	return true
}

func queuedJobMatchesRepo(job db.Job, repoFilter string) bool {
	repoFilter = strings.TrimSpace(repoFilter)
	if repoFilter == "" {
		return true
	}
	payload, err := daemonJobPayload(job)
	return err == nil && payload.Repo == repoFilter
}

// queuedJobMatchesSession reports whether a job belongs to the delegation tree
// rooted at rootFilter. An empty filter matches everything (the default daemon
// behavior). Otherwise a job matches iff it is the root coordinator job itself
// (job.ID == rootFilter) or carries the root id in its payload
// (payload.RootJobID == rootFilter); children and continuations inherit the
// root id via the payload.
func queuedJobMatchesSession(job db.Job, rootFilter string) bool {
	rootFilter = strings.TrimSpace(rootFilter)
	if rootFilter == "" {
		return true
	}
	if job.ID == rootFilter {
		return true
	}
	payload, err := daemonJobPayload(job)
	return err == nil && payload.RootJobID == rootFilter
}

// queuedChildOfKilledRoot reports whether a queued job is a delegation child leg
// of a tree whose root has been killed by an operator (#341). Only child legs are
// matched and skipped. Two classes are deliberately exempted so the graceful
// finalize can still run:
//   - the root coordinator itself (payload.RootJobID == "" or == job.ID); and
//   - any continuation (coordinator reconvene or the #305 graceful finalize),
//     which carries no DelegationID — it MUST run so the engine routes the killed
//     tree through enqueueFinalizeContinuation and emits a terminal result.
//
// Delegation child legs set DelegationID (delegationRequest), so a non-empty
// DelegationID is what marks a job as skippable work. A payload-parse miss or
// store error fails open (returns false) so a hiccup never silently strands a job.
//
// NOTE: the same child-leg classification invariant (RootJobID != "" &&
// RootJobID != job.ID && DelegationID != "") is re-implemented inline in
// workflow.KillDelegationTree (internal/workflow/job_kill.go, #480) to eagerly
// cancel queued child legs at kill time. The cli->workflow import direction
// prevents sharing one helper, so if the classification rules here change, update
// the workflow site too — the two MUST stay in lockstep.
func queuedChildOfKilledRoot(ctx context.Context, store *db.Store, job db.Job) bool {
	if store == nil {
		return false
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return false
	}
	rootJobID := strings.TrimSpace(payload.RootJobID)
	if rootJobID == "" || rootJobID == job.ID {
		return false
	}
	// Continuations (DelegationID == "") reconvene the coordinator / finalize the
	// tree and must always run, even for a killed root. Only actual child legs are
	// skipped.
	if strings.TrimSpace(payload.DelegationID) == "" {
		return false
	}
	killed, err := store.IsRootJobKilled(ctx, rootJobID)
	return err == nil && killed
}

func queuedJobCheckoutKey(ctx context.Context, store *db.Store, job db.Job) string {
	payload, err := daemonJobPayload(job)
	if err != nil || strings.TrimSpace(payload.Repo) == "" {
		return "job:" + job.ID
	}
	if path, ok := queuedJobTaskWorktreePath(ctx, store, payload); ok {
		return "worktree:" + path
	}
	return "repo:" + payload.Repo
}

func queuedJobTaskWorktreePath(ctx context.Context, store *db.Store, payload workflow.JobPayload) (string, bool) {
	// Sibling delegations share a task id but run in distinct per-delegation
	// worktrees; key off the payload worktree path so they schedule as separate
	// checkout keys and can run in parallel.
	if delegationPath := strings.TrimSpace(payload.WorktreePath); delegationPath != "" {
		path, err := normalizeTaskWorktreePath(delegationPath)
		return path, err == nil && path != ""
	}
	if store == nil || strings.TrimSpace(payload.TaskID) == "" {
		return "", false
	}
	task, err := store.GetTask(ctx, payload.TaskID)
	if err != nil {
		return "", false
	}
	if strings.TrimSpace(task.RepoFullName) != "" && task.RepoFullName != payload.Repo {
		return "", false
	}
	path, err := normalizeTaskWorktreePath(task.WorktreePath)
	return path, err == nil && path != ""
}

func queuedJobRuntimeResourceKey(ctx context.Context, store *db.Store, job db.Job) string {
	if store == nil {
		return ""
	}
	// A per-job runtime override (#531) runs on ITS OWN session key, so schedule
	// it under that key (fully payload-derived — no GetAgent needed) rather than
	// the agent's default-runtime session it will never take.
	if payload, err := daemonJobPayload(job); err == nil && strings.TrimSpace(payload.RuntimeOverride) != "" {
		key, ok := overrideRuntimeSessionResourceKey(applyJobRuntimeOverride(runtime.Agent{}, payload))
		if !ok {
			return ""
		}
		return key
	}
	agent, err := store.GetAgent(ctx, job.Agent)
	if err != nil {
		return ""
	}
	key, ok := runtimeSessionResourceKey(runtimeAgent(agent))
	if !ok {
		return ""
	}
	return key
}

func (w jobWorker) run(ctx context.Context, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err)
	}
	// An ephemeral child carries an inline worker spec instead of a
	// pre-registered agent. Materialize a throwaway agent + runtime session
	// from the spec before the normal flow runs (which assumes the agent
	// already exists via GetAgent below), and register a cleanup defer so the
	// worker is auto-disposed on every exit path — success, failure, or block.
	if payload.Ephemeral != nil {
		if err := w.startEphemeralWorker(ctx, job, payload); err != nil {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "ephemeral_worker_failed", Message: err.Error()}); eventErr != nil {
				return eventErr
			}
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
			return nil
		}
		// Idempotent removal of the agent row + instance regardless of how run
		// returns; uses a background context so cleanup survives ctx cancel.
		defer w.cleanupTempWorker(context.Background(), job.Agent)
	}
	dbAgent, err := w.Store.GetAgent(ctx, job.Agent)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
		return nil
	}
	agent := runtimeAgent(dbAgent)
	// An ephemeral worker's runtime session exists solely for this job — it was
	// started by startEphemeralWorker above and disposed by the cleanup defer.
	// Mark it single-use so adapters whose CLIs report session-cumulative usage
	// (codex, #658) can attribute that usage to the job: the whole session is
	// this job's cost. In-memory only — GetAgent never returns the flag.
	if payload.Ephemeral != nil {
		agent.SingleUseSession = true
	}
	// Per-job runtime override (#531): the payload carries the override, so a
	// background/daemon job honors it identically to a foreground dispatch. The
	// effective agent swaps in the override runtime + the job's own session ref;
	// the stored agent row (and its default-runtime session) is never touched.
	defaultRuntime := agent.Runtime
	overridden := strings.TrimSpace(payload.RuntimeOverride) != ""
	if overridden {
		agent = applyJobRuntimeOverride(agent, payload)
		if err := runtime.ValidateAgent(agent); err != nil {
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
			return nil
		}
	}
	if !overridden {
		agent = scopeRegisteredFreshRefForJob(agent, job.ID)
	}
	if err := w.produceDispatchError(job.Type, agent); err != nil {
		w.recordProduceSandboxDiagnostic(ctx, job.ID, job.Type, agent)
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobBlocked, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
		return nil
	}
	if readOnlyImplementationBlocked(job.Type, agent) {
		transitioned, err := markJobPermissionBlocked(ctx, w.Store, job.ID)
		if err != nil {
			return err
		}
		if !transitioned {
			return nil
		}
		if err := blockTaskForPermissionBlockedJob(ctx, w.Store, job); err != nil {
			return err
		}
		// Best-effort outbound emit (#446): this PRE-FLIGHT queued->blocked
		// permission transition is daemon-owned (it never reaches the engine's
		// Mailbox chokepoint), exactly like the MID-RUN permission block in
		// handleRunJobError which already emits job.blocked. Emit here too so both
		// halves of the permission-blocked terminal case are covered; gated on the
		// genuine transition above, nil-safe when [events] is OFF. The following
		// finalizePreflightDelegationChild only attaches a synthetic result
		// (savePayload, no transition), so it never re-emits.
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, job.ID, events.EventJobBlocked, string(workflow.JobBlocked), agentPermissionBlockedMessage)
		_ = w.postJobResultComment(ctx, job.ID, agent, "", errors.New(agentPermissionBlockedMessage))
		writeLine(w.Stdout, "job %s blocked: %s", job.ID, agentPermissionBlockedMessage)
		// A read-only implement DELEGATION child short-circuits to blocked here,
		// BEFORE finishQueuedJob, via markJobPermissionBlocked (a direct transition)
		// — and blockTaskForPermissionBlockedJob only blocks the task, it never
		// advances the parent DAG. So without this the parent strands exactly like
		// #409. Route the delegation child through the SAME finalize helper so its
		// failure_policy fires. Gated strictly on a delegation child (ParentJobID set,
		// Result nil), so a NON-delegation permission-blocked job is byte-identical.
		if err := w.finalizePreflightDelegationChild(ctx, job.ID, errors.New(agentPermissionBlockedMessage)); err != nil {
			return err
		}
		return nil
	}
	checkout, err := w.CheckoutValidator(ctx, job, payload, agent)
	if err != nil {
		// Checkout-contention deferral (#532 slice C): a NON-delegation job whose
		// daemon pre-flight checkout failed on a classified contention string (a
		// branch-lock conflict that self-heals, or a dirty/wrong-head checkout that
		// needs a human) is HELD with a backoff instead of terminally failing —
		// pre-terminal, so no job.failed precedes the additive job.deferred. Every
		// other checkout error (and every delegation child) falls through to the
		// existing terminal path byte-identically.
		if deferred, deferErr := w.deferCheckoutContention(ctx, job, payload, err); deferErr != nil {
			writeLine(w.Stdout, "job %s checkout-contention deferral failed: %v", job.ID, deferErr)
		} else if deferred {
			writeLine(w.Stdout, "job %s deferred on checkout contention: %v", job.ID, err)
			return nil
		}
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
		return nil
	}
	// #732 moot-seat relay injection: a `gitmoot moot` SEAT (payload.MootSeat) must
	// converse via `gitmoot chat send/wait` mid-run, but its runtime sandbox makes
	// the home read-only. buildSeatAwareAdapter mints a per-seat token bound to
	// (agent, thread), builds the adapter with an env-injecting runner so the seat's
	// runtime subprocess inherits GITMOOT_CHAT_RELAY[_AUTH] and routes those writes
	// to this daemon, AND — only when it actually injects that relay env — elevates
	// the agent to ChatSeat (a codex seat then gets workspace-write+network to reach
	// the socket; the home stays read-only, so the relay does the write). Coupling
	// the elevation to real injection is deliberate: a seat is NEVER left with the
	// extra codex privilege but no working relay (the pre-#732-review bug). Gating on
	// MootSeat — not ThreadID — keeps chat-task promotions and ThreadID-carrying
	// continuations/children byte-identical (unelevated, no relay env). The token is
	// released on every exit path so it cannot be replayed after the seat ends.
	var progressTracker *pipelineProgressLineTracker
	if payload.Sender == workflow.PipelineJobSender {
		progressTracker = &pipelineProgressLineTracker{}
	}
	var adapter workflow.DeliveryAdapter
	var relayToken string
	if progressTracker != nil {
		adapter, relayToken, err = w.buildSeatAwareAdapter(&agent, checkout, payload, progressTracker)
	} else {
		adapter, relayToken, err = w.buildSeatAwareAdapter(&agent, checkout, payload)
	}
	if relayToken != "" {
		defer w.RelayServer.ReleaseSeat(relayToken)
	}
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	managed, err := w.managedJobConfig(ctx, agent.Name)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	jobTimeout := effectiveJobTimeout(payload, managed)
	// Size the runtime-session lease to jobTimeout PLUS a teardown grace so the
	// lease strictly OUTLIVES the run-context deadline (armed at exactly jobTimeout
	// below) and the terminal worktree teardown that runs while this lock is still
	// held. A normally-terminating worker therefore releases its lease before it
	// expires; without the grace the lease would expire in the live-worker teardown
	// window and the expired-lock reaper would requeue the still-'running' owner
	// onto its dirty worktree — the #536 clobber. See runtimeLeaseTeardownGrace.
	lockTTL := defaultDaemonRunningJobStaleAfter
	if jobTimeout > 0 {
		lockTTL = jobTimeout + runtimeLeaseTeardownGrace
	}
	// SESSION SAFETY (#531): the lock is taken on the EFFECTIVE agent, so an
	// overridden job locks the OVERRIDE runtime's session key and can never
	// collide with (or occupy) the agent's default-runtime session lock.
	releaseLock, acquired, lockKey, ownerToken, err := acquireJobRuntimeSessionLock(ctx, w.Store, job.ID, agent, overridden, time.Now().UTC(), lockTTL)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	if !acquired {
		message := fmt.Sprintf("runtime session %s is busy", lockKey)
		policy, policyErr := w.parallelSessionPolicy()
		if policyErr != nil {
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, policyErr); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, policyErr)
			return nil
		}
		eligibility := tempWorkerEligible(ctx, w.Store, job, payload, agent, policy, time.Now().UTC())
		if eligibility.Eligible {
			eligibleMessage := fmt.Sprintf("%s; temp worker eligible", message)
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "temp_worker_eligible", Message: eligibleMessage}); eventErr != nil {
				return eventErr
			}
			return w.runWithTempWorker(ctx, job, payload, agent, checkout, policy, eligibleMessage)
		} else if strings.TrimSpace(eligibility.Reason) != "" {
			message = fmt.Sprintf("%s; temp worker ineligible: %s", message, eligibility.Reason)
		}
		// Dedup the runtime_lock_wait row + flood log to once per wait episode
		// (#598): a permanently-contended job otherwise wrote one row per dispatch
		// pass. The busy error is returned UNCONDITIONALLY (outside the episode
		// gate) so the pool dispatcher still sees the bounce and holds the job back.
		if !runtimeLockWaitEpisodeOpen(job.ID) {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: message}); eventErr != nil {
				return eventErr
			}
			markRuntimeLockWaitEpisode(job.ID)
			writeLine(w.Stdout, "job %s waiting: %s", job.ID, message)
		}
		return fmt.Errorf("%w: %s", errRuntimeSessionBusy, message)
	}
	// Acquired the runtime lock: close any open wait episode so a future contention
	// is recorded as a fresh episode.
	endRuntimeLockWaitEpisode(job.ID)
	defer func() {
		if err := releaseLock(context.Background()); err != nil {
			writeLine(w.Stdout, "job %s runtime lock release failed: %v", job.ID, err)
		}
	}()
	// Thread the owner token into the context so the terminal worktree cleanup
	// (which runs inside RunJob -> AdvanceJob while THIS lock is still held — it is
	// released only by the defer above, after RunJob returns) recognizes the run's
	// OWN lock and does not refuse the healthy-path cleanup as if a foreign live
	// owner held it (#536 / #478). Covers RunJob and the handleRunJobError finalize
	// path below, both of which derive from this ctx.
	ctx = workflow.WithRuntimeSelfOwnerToken(ctx, ownerToken)
	// Expose the effective runtime (and the session lock it runs under) in job
	// history so an overridden background job is observable (#531).
	if overridden {
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_override", Message: jobRuntimeOverrideEventMessage(defaultRuntime, agent, lockKey)}); eventErr != nil {
			writeLine(w.Stdout, "job %s runtime_override event failed: %v", job.ID, eventErr)
		}
	}
	// This is the last filesystem authorization check before adapter delivery.
	// It runs after runtime-session admission so a symlink retargeted while the job
	// waited cannot inherit stale grants. The adapter is then rebuilt in-place with
	// sandbox-exec as the innermost runner for Claude/Kimi produce only.
	if err := applyProduceRuntimeGrants(ctx, w.Store, w.ConfigHome, job, payload, &agent); err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	adapter, err = wrapProduceSandboxAdapter(job.Type, agent, adapter)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	// Cockpit wrapping happens AFTER the runtime-session lock + checkout
	// resolution so at most one live pane exists per held runtime session and the
	// pane's CWD is the resolved worktree. It is strictly opt-in and best-effort:
	// when --cockpit is off (or herdr is unavailable) the adapter is unchanged and
	// behavior is byte-identical to today. A policy load failure degrades to no
	// cockpit rather than failing the job.
	if payload.Cockpit {
		policy, policyErr := w.orchestratePolicy()
		// A policy LOAD error is not the same as the user opting out (mode off): the
		// user asked for a cockpit, so degrade to cockpit-unavailable (run unwrapped
		// AND emit the single cockpit_unavailable event) rather than silently
		// dropping the pane. Only an explicit mode-off opts out without an event.
		userOptedOff := policyErr == nil && policy.CockpitMode == config.CockpitModeOff
		var cp *cockpit.Cockpit
		if policyErr == nil && !userOptedOff {
			cp = w.newCockpit(policy)
		}
		meta := cockpitJobMeta(job, payload, agent, checkout, policy.CockpitPaneKey)
		seatMode := policy.CockpitPaneKey == config.CockpitPaneKeySeat
		// Only when the cockpit will actually wrap (herdr available) do we tee the
		// child's live output into a log the pane tails (Task 6). The tee rebuilds
		// the inner adapter with a group-kill-preserving TeeRunner and sets
		// meta.LogPath; on any log-setup failure it falls back to no LogPath (the P0
		// `job watch` pane). The non-cockpit / unavailable paths never create a log
		// file or tee — they stay byte-identical.
		//
		// Job mode uses a per-job truncate log removed when the job finishes. Seat
		// mode (Task 7) uses a STABLE per-seat append log so the one seat pane tails
		// one file that accumulates the seat's history across delegation rounds — it
		// is opened O_APPEND and is NOT removed per job (it persists for the root's
		// life and is torn down by FinalizeRoot).
		if maybeWrapCockpitAvailable(cp, payload.Cockpit, userOptedOff) {
			var teeAdapter workflow.DeliveryAdapter
			var logPath string
			var logFile *os.File
			if progressTracker != nil {
				teeAdapter, logPath, logFile = w.cockpitLogAdapter(cp, agent, checkout, job.ID, meta.RootJobID, meta.PaneKey, seatMode, progressTracker)
			} else {
				teeAdapter, logPath, logFile = w.cockpitLogAdapter(cp, agent, checkout, job.ID, meta.RootJobID, meta.PaneKey, seatMode)
			}
			if logFile != nil {
				defer func() {
					if err := logFile.Close(); err != nil {
						writeLine(w.Stdout, "job %s cockpit log close failed: %v", job.ID, err)
					}
					// Job mode: the per-job log only backs a per-job pane torn down with
					// the job, so remove it. Seat mode: keep the append log — it backs the
					// persisted seat pane and is removed on root finalize.
					if !seatMode {
						_ = os.Remove(logPath)
					}
				}()
				adapter = teeAdapter
				meta.LogPath = logPath
			}
		}
		var unavailable bool
		adapter, unavailable = maybeWrapCockpit(cp, payload.Cockpit, userOptedOff, adapter, meta)
		if unavailable {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "cockpit_unavailable", Message: "cockpit requested but herdr is unavailable; running without a pane"}); eventErr != nil {
				writeLine(w.Stdout, "job %s cockpit_unavailable event failed: %v", job.ID, eventErr)
			}
		}
		// On the job's return, check whether the root coordination tree has now
		// terminated and, if so, tear its panes / workspace / seat logs down once and
		// surface the reconvene view (Task 7/8). This runs in BOTH modes: seat mode
		// closes the persisted seat panes + workspace here, and job mode (whose panes
		// already close per-Deliver) still needs the per-root WORKSPACE closed at
		// root-terminal — the cockpit_workspaces registry is the only remaining handle
		// once the pane rows are gone. finalizeCockpitRootIfDone's cheap guard
		// short-circuits when there is neither a pane row nor a registered workspace,
		// so a non-cockpit tree makes no extra herdr calls.
		if cp != nil && !userOptedOff {
			defer w.finalizeCockpitRootIfDone(cp, job, payload, meta.RootJobID)
		}
	}
	if managed.OK {
		if err := w.Store.MarkAgentInstanceRunning(ctx, agent.Name, time.Now().UTC(), managed.JobTimeout); err != nil {
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
			return nil
		}
		defer func() {
			if err := w.Store.TouchAgentInstance(context.Background(), agent.Name, time.Now().UTC(), managed.IdleTimeout); err != nil {
				writeLine(w.Stdout, "job %s managed agent state update failed: %v", job.ID, err)
			}
		}()
	}
	writeLine(w.Stdout, "running job %s for %s in %s", job.ID, agent.Name, payload.Repo)
	adapter = wrapPipelineEnvDeliveryAdapter(w.Store, w.ConfigHome, payload, adapter)
	engine := w.WorkflowFactory(checkout)
	// Wire the PRE-TERMINAL operational-blocker deferrer (#532 slice E) on the LIVE
	// worker (not the WorkflowFactory-captured copy) so it observes this worker's
	// EventSink for the first-class job.deferred emit. When a delivery-seam failure
	// classifies as a retryable operational blocker the mailbox re-queues the job
	// BEFORE the terminal transition, so no job.failed reaches the [events] sink.
	engine.BlockerDeferrer = w.deferOperationalBlockerPreTerminal
	runCtx, stopRun := w.runningJobContext(ctx, job.ID)
	defer stopRun()
	if jobTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, jobTimeout)
		defer cancel()
	}
	stopProgress := func() {}
	if progressTracker != nil {
		progressCtx, cancelProgress := context.WithCancel(runCtx)
		done := make(chan struct{})
		threshold := w.ProgressThreshold
		if threshold <= 0 {
			threshold = pipelineProgressThreshold
		}
		interval := w.ProgressInterval
		if interval <= 0 {
			interval = pipelineProgressInterval
		}
		tickSource := w.ProgressTickSource
		if tickSource == nil {
			tickSource = pipelineProgressTicks
		}
		startedAt := time.Now().UTC()
		go func() {
			defer close(done)
			emitPipelineProgress(progressCtx, w.Store, w.Stdout, job.ID, startedAt, progressTracker, tickSource(progressCtx, threshold, interval))
		}()
		stopProgress = func() {
			cancelProgress()
			<-done
		}
	}
	_, err = engine.RunJob(runCtx, job.ID, agent, adapter)
	stopProgress()
	if err != nil {
		// Operational-blocker deferral (#532 slice E): a run whose delivery failed on
		// a classified OPERATIONAL blocker (runtime auth rejected, rate limit/quota,
		// network/GitHub outage) is re-queued PRE-terminally by the mailbox's injected
		// BlockerDeferrer — running→queued with a hold + a first-class job.deferred,
		// and NO job.failed. RunJob reports ErrJobDeferred; short-circuit the entire
		// terminal path (no handleRunJobError, no failure comment) since the run
		// already resolved to a deferral. Every other failure takes the path below
		// byte-identically.
		if errors.Is(err, workflow.ErrJobDeferred) {
			writeLine(w.Stdout, "job %s deferred on operational blocker (pre-terminal): %v", job.ID, err)
			return nil
		}
		if markErr := w.handleRunJobError(ctx, job.ID, err); markErr != nil {
			return markErr
		}
		commentErr := err
		if job.Type == "implement" && runtimePermissionFailure(err) {
			latest, latestErr := w.Store.GetJob(ctx, job.ID)
			if latestErr == nil && latest.State == string(workflow.JobBlocked) {
				commentErr = errors.New(agentPermissionBlockedMessage)
			}
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, commentErr)
		writeLine(w.Stdout, "job %s failed: %v", job.ID, err)
		return nil
	}
	_ = w.postJobResultComment(ctx, job.ID, agent, checkout, nil)
	writeLine(w.Stdout, "job %s completed", job.ID)
	return nil
}

// applyProduceRuntimeGrants performs the final delivery-time path check and only
// then copies produce-only grants onto the in-memory runtime agent. Non-produce
// jobs remain byte-identical and can never inherit persisted produce fields.
func applyProduceRuntimeGrants(ctx context.Context, store *db.Store, home string, job db.Job, payload workflow.JobPayload, agent *runtime.Agent) error {
	if strings.TrimSpace(job.Type) != "produce" {
		return nil
	}
	if agent == nil {
		return errors.New("produce runtime agent is required")
	}
	subject := fmt.Sprintf("job %q", job.ID)
	writable, err := canonicalizePipelineProducePaths(ctx, store, home, subject, payload.WritablePaths)
	if err != nil {
		return fmt.Errorf("produce writable path preflight failed: %w", err)
	}
	envFile := ""
	if len(payload.ReadablePaths) > 0 {
		if strings.TrimSpace(payload.PipelineName) == "" {
			return errors.New("produce readable path preflight failed: pipeline name is required")
		}
		record, found, err := store.GetPipeline(ctx, payload.PipelineName)
		if err != nil {
			return fmt.Errorf("produce readable path preflight failed: load pipeline: %w", err)
		}
		if !found {
			return fmt.Errorf("produce readable path preflight failed: pipeline %q is unavailable", payload.PipelineName)
		}
		spec, err := pipeline.Load([]byte(record.SpecYAML))
		if err != nil {
			return fmt.Errorf("produce readable path preflight failed: load pipeline spec: %w", err)
		}
		envFile = spec.EnvFile
	}
	readable, err := canonicalizePipelineProduceReadPaths(ctx, store, home, subject, payload.ReadablePaths, writable, envFile)
	if err != nil {
		return fmt.Errorf("produce readable path preflight failed: %w", err)
	}
	var readableFiles []string
	if len(payload.ReadablePaths) > 0 && agent.Runtime == runtime.ClaudeRuntime {
		var warnings []runtime.ClaudeHookWarning
		readable, readableFiles, warnings, err = claudeProduceRuntimeReadAccess(ctx, store, home, envFile, readable)
		recordClaudeProduceHookWarnings(ctx, store, job.ID, warnings)
		if err != nil {
			return fmt.Errorf("produce Claude runtime resource preflight failed: %w", err)
		}
	}
	agent.WritablePaths = writable
	agent.ReadablePaths = readable
	agent.ReadableFiles = readableFiles
	agent.ProduceNetwork = payload.Network
	return nil
}

func claudeProduceRuntimeReadAccess(ctx context.Context, store *db.Store, homeFlag, envFile string, declared []string) ([]string, []string, []runtime.ClaudeHookWarning, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve Claude operator home: %w", err)
	}
	home = filepath.Clean(home)
	configDir := realClaudeConfigDir()
	if strings.TrimSpace(configDir) == "" {
		configDir = filepath.Join(home, ".claude")
	}
	configDir, err = resolveProduceSafetyPath(configDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve Claude config directory: %w", err)
	}
	protected, err := resolveProduceReadProtectedPaths(ctx, store, homeFlag, envFile)
	if err != nil {
		return nil, nil, nil, err
	}

	resources, warnings := runtime.DiscoverClaudeHookResources(home, configDir)
	readable := compactCleanPaths(declared)
	readableFiles := []string{}
	addDir := func(path, resource string) error {
		resolved, resolveErr := resolveProduceSafetyPath(path)
		if resolveErr != nil {
			return fmt.Errorf("resolve Claude runtime resource %q: %w", resource, resolveErr)
		}
		if label, excluded := protected.exclusion(resolved); excluded {
			return fmt.Errorf("Claude runtime resource %q cannot be read because its parent %q overlaps %s; move it outside protected state, then add reads: [%q] if needed", resource, resolved, label, resolved)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("inspect Claude runtime resource directory %q: %w", resolved, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("Claude runtime resource parent %q is not a directory", resolved)
		}
		readable = compactCleanPaths(append(readable, resolved))
		return nil
	}
	if info, statErr := os.Stat(configDir); statErr == nil && info.IsDir() {
		if err := addDir(configDir, configDir); err != nil {
			return nil, nil, warnings, err
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, nil, warnings, fmt.Errorf("inspect Claude config directory %q: %w", configDir, statErr)
	}

	userState := filepath.Join(home, ".claude.json")
	if _, statErr := os.Stat(userState); statErr == nil {
		resolved, err := resolveProduceSafetyPath(userState)
		if err != nil {
			return nil, nil, warnings, fmt.Errorf("resolve Claude user settings %q: %w", userState, err)
		}
		if label, excluded := protected.exclusion(resolved); excluded {
			return nil, nil, warnings, fmt.Errorf("Claude user settings %q cannot be read because it overlaps %s", userState, label)
		}
		readableFiles = compactCleanPaths(append(readableFiles, resolved))
	} else if !os.IsNotExist(statErr) {
		return nil, nil, warnings, fmt.Errorf("inspect Claude user settings %q: %w", userState, statErr)
	}

	for _, resource := range resources {
		resolved, err := resolveProduceSafetyPath(resource.Path)
		if err != nil {
			return nil, nil, warnings, fmt.Errorf("resolve Claude hook path %q: %w", resource.Path, err)
		}
		parent := filepath.Dir(resolved)
		if err := addDir(parent, resource.Path); err != nil {
			return nil, nil, warnings, err
		}
		if info, statErr := os.Stat(resolved); statErr == nil {
			if info.IsDir() {
				return nil, nil, warnings, fmt.Errorf("Claude hook path %q is a directory, not a readable script", resource.Path)
			}
			file, openErr := os.Open(resolved)
			if openErr != nil {
				return nil, nil, warnings, fmt.Errorf("Claude hook path %q is not readable: %w", resource.Path, openErr)
			}
			_ = file.Close()
		} else if os.IsNotExist(statErr) {
			return nil, nil, warnings, fmt.Errorf("Claude hook path %q does not exist; fix the hook or add a readable absolute script", resource.Path)
		} else {
			return nil, nil, warnings, fmt.Errorf("inspect Claude hook path %q: %w", resource.Path, statErr)
		}
		if !pathCoveredByRuntimeReads(resolved, readable, readableFiles) {
			return nil, nil, warnings, fmt.Errorf("Claude hook path %q is outside the final read allowlist; add reads: [%q]", resource.Path, parent)
		}
	}
	return readable, readableFiles, warnings, nil
}

func pathCoveredByRuntimeReads(path string, dirs, files []string) bool {
	for _, dir := range dirs {
		if pathWithin(path, dir) {
			return true
		}
	}
	for _, file := range files {
		if path == file {
			return true
		}
	}
	return false
}

func recordClaudeProduceHookWarnings(ctx context.Context, store *db.Store, jobID string, warnings []runtime.ClaudeHookWarning) {
	if store == nil || strings.TrimSpace(jobID) == "" {
		return
	}
	for _, warning := range warnings {
		origin := warning.SettingsPath
		if warning.Event != "" {
			origin += " (" + warning.Event + ")"
		}
		_ = store.AddJobEvent(ctx, db.JobEvent{
			JobID:   jobID,
			Kind:    "produce_runtime_resource_warning",
			Message: fmt.Sprintf("Claude hook settings %s: %s", origin, warning.Reason),
		})
	}
}

func (w jobWorker) produceDispatchError(action string, agent runtime.Agent) error {
	if err := runtime.ProduceDispatchError(action, agent); err != nil {
		return err
	}
	if strings.TrimSpace(action) != "produce" || agent.Runtime == runtime.CodexRuntime {
		return nil
	}
	if agent.Runtime != runtime.ClaudeRuntime && agent.Runtime != runtime.KimiRuntime {
		return nil
	}
	result, _ := w.produceSandboxProbe(action, agent)
	if result.Supported {
		return nil
	}
	return fmt.Errorf("produce stages require the codex runtime; agent %q uses runtime %q", agent.Name, agent.Runtime)
}

func (w jobWorker) produceSandboxProbe(action string, agent runtime.Agent) (sandbox.ProbeResult, bool) {
	if strings.TrimSpace(action) != "produce" || (agent.Runtime != runtime.ClaudeRuntime && agent.Runtime != runtime.KimiRuntime) {
		return sandbox.ProbeResult{}, false
	}
	probe := w.SandboxProbe
	if probe == nil {
		probe = sandbox.SandboxProbe
	}
	return probe(), true
}

func (w jobWorker) recordProduceSandboxDiagnostic(ctx context.Context, jobID, action string, agent runtime.Agent) {
	// Only annotate the probe-gated refusal. Capability/policy/runtime validation
	// errors from the legacy preflight keep their existing event surface.
	if err := runtime.ProduceDispatchError(action, agent); err != nil {
		return
	}
	result, applicable := w.produceSandboxProbe(action, agent)
	if !applicable || result.Supported || w.Store == nil {
		return
	}
	detail := "Landlock enforcement self-test failed"
	if result.Err != nil {
		detail = result.Err.Error()
	}
	if result.ABI > 0 {
		detail = fmt.Sprintf("Landlock ABI v%d: %s", result.ABI, detail)
	}
	message := fmt.Sprintf("Gitmoot Landlock sandbox unavailable for %s produce: %s; run gitmoot sandbox probe", agent.Runtime, detail)
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "produce_sandbox_unsupported", Message: message}); err != nil {
		writeLine(w.Stdout, "job %s produce_sandbox_unsupported event failed: %v", jobID, err)
	}
}

// wrapProduceSandboxAdapter rewrites only Claude/Kimi produce adapters. Codex
// keeps its existing native sandbox and every non-produce adapter is returned
// byte-for-byte unchanged.
func wrapProduceSandboxAdapter(action string, agent runtime.Agent, adapter workflow.DeliveryAdapter) (workflow.DeliveryAdapter, error) {
	if strings.TrimSpace(action) != "produce" || agent.Runtime == runtime.CodexRuntime {
		return adapter, nil
	}
	if agent.Runtime != runtime.ClaudeRuntime && agent.Runtime != runtime.KimiRuntime {
		return adapter, nil
	}
	reads, readFiles, writes, env, err := produceRuntimeSandboxGrants(agent.Runtime, agent.ReadablePaths, agent.ReadableFiles, agent.WritablePaths)
	if err != nil {
		return nil, err
	}
	switch a := adapter.(type) {
	case modelGatewayRuntimeAdapter:
		wrapped, err := wrapProduceSandboxAdapter(action, agent, a.Adapter)
		if err != nil {
			return nil, err
		}
		runtimeAdapter, ok := wrapped.(runtime.Adapter)
		if !ok {
			return nil, fmt.Errorf("produce Landlock sandbox returned incompatible %T adapter", wrapped)
		}
		a.Adapter = runtimeAdapter
		return a, nil
	case runtime.ClaudeAdapter:
		a.Runner = landlockProduceRunner(a.Runner, reads, readFiles, writes, env)
		return a, nil
	case *runtime.ClaudeAdapter:
		a.Runner = landlockProduceRunner(a.Runner, reads, readFiles, writes, env)
		return a, nil
	case runtime.KimiAdapter:
		a.Runner = landlockProduceRunner(a.Runner, reads, readFiles, writes, env)
		return a, nil
	case *runtime.KimiAdapter:
		a.Runner = landlockProduceRunner(a.Runner, reads, readFiles, writes, env)
		return a, nil
	default:
		return nil, fmt.Errorf("produce Landlock sandbox cannot wrap %s adapter %T", agent.Runtime, adapter)
	}
}

func produceRuntimeSandboxGrants(runtimeName string, readable, readFiles, writable []string) ([]string, []string, []string, []string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("resolve runtime state home: %w", err)
	}
	home = filepath.Clean(home)
	var statePaths []string
	var env []string
	switch runtimeName {
	case runtime.ClaudeRuntime:
		stateDir := filepath.Join(home, ".claude")
		cacheRoot, err := os.UserCacheDir()
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("resolve Claude cache root: %w", err)
		}
		cacheDir := filepath.Join(cacheRoot, "claude-cli-nodejs")
		statePaths = []string{stateDir, cacheDir}
		env = []string{"CLAUDE_CONFIG_DIR=" + stateDir}
	case runtime.KimiRuntime:
		statePaths = []string{filepath.Join(home, ".kimi-code")}
	default:
		return compactCleanPaths(readable), compactCleanPaths(readFiles), compactCleanPaths(writable), nil, nil
	}
	for _, path := range statePaths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("create %s runtime state directory %q: %w", runtimeName, path, err)
		}
	}
	reads := compactCleanPaths(readable)
	files := compactCleanPaths(readFiles)
	writes := compactCleanPaths(append(append([]string(nil), writable...), statePaths...))
	return reads, files, writes, env, nil
}

func compactCleanPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

func landlockProduceRunner(runner subprocess.Runner, reads, readFiles, writes, env []string) subprocess.Runner {
	readable := append([]string(nil), reads...)
	files := append([]string(nil), readFiles...)
	writable := append([]string(nil), writes...)
	runtimeEnv := append([]string(nil), env...)
	if tee, ok := runner.(subprocess.TeeRunner); ok {
		inner := tee.Inner
		if inner == nil {
			inner = subprocess.GroupRunner{}
		}
		if _, wrapped := inner.(subprocess.WrappingRunner); !wrapped {
			tee.Inner = subprocess.WrappingRunner{Inner: inner, ReadablePaths: readable, ReadableFiles: files, WritablePaths: writable, Env: runtimeEnv}
		}
		return tee
	}
	if tee, ok := runner.(*subprocess.TeeRunner); ok {
		inner := tee.Inner
		if inner == nil {
			inner = subprocess.GroupRunner{}
		}
		if _, wrapped := inner.(subprocess.WrappingRunner); !wrapped {
			tee.Inner = subprocess.WrappingRunner{Inner: inner, ReadablePaths: readable, ReadableFiles: files, WritablePaths: writable, Env: runtimeEnv}
		}
		return tee
	}
	if runner == nil {
		runner = subprocess.GroupRunner{}
	}
	if _, wrapped := runner.(subprocess.WrappingRunner); wrapped {
		return runner
	}
	return subprocess.WrappingRunner{Inner: runner, ReadablePaths: readable, ReadableFiles: files, WritablePaths: writable, Env: runtimeEnv}
}

// configPaths resolves this worker's config.Paths for READ-ONLY policy loading
// WITHOUT calling config.Initialize (#459). ConfigHome is the raw --home invariant
// (see the struct field doc), so pathsFromFlag resolves it exactly once to the
// real <home>/.gitmoot, which withStore/withStoreAndPaths already initialized
// upstream. Using pathsFromFlag instead of initializedPaths here is the durable
// guard: even if a caller mistakenly passes the already-resolved root, this never
// MkdirAll-s the phantom <home>/.gitmoot/.gitmoot — it just reads (and degrades to
// an error the best-effort callers absorb). Initialize is the only dir-creator and
// the policy loaders only need to READ.
func (w jobWorker) configPaths() (config.Paths, error) {
	return pathsFromFlag(w.ConfigHome)
}

func (w jobWorker) parallelSessionPolicy() (config.ParallelSessionPolicy, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return config.DefaultParallelSessionPolicy(), nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return config.ParallelSessionPolicy{}, err
	}
	return config.LoadParallelSessionPolicy(paths)
}

// repoConcurrency loads the per-repo [repos."owner/repo"] scheduler overrides
// (#576), mirroring parallelSessionPolicy: an implicit/empty config home has no
// overrides (nil ⇒ every repo uses the global default), and an explicit home
// loads them from the config file. Errors are surfaced to the caller, which
// fails safe to the global default.
func (w jobWorker) repoConcurrency() ([]config.RepoConcurrency, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return nil, nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return nil, err
	}
	return config.LoadRepoConcurrency(paths)
}

// resolveRepoScheduler resolves the effective worker limit and pool toggle for a
// repo's queued-job run (#576). It is behavior-preserving by default: with no
// repoFilter, no [repos."owner/repo"] section, an implicit config home, or a
// config-load error, it returns (globalLimit, w.UsePool) unchanged. A configured
// max_parallel>0 caps THAT repo's concurrency; max_parallel<=0/missing keeps the
// global default (never zero ⇒ never a stalled repo). A configured scheduler
// ("pool"/"barrier") overrides the pool toggle for that repo only.
func (w jobWorker) resolveRepoScheduler(repoFilter string, globalLimit int) (int, bool) {
	limit := globalLimit
	usePool := w.UsePool
	repo := strings.TrimSpace(repoFilter)
	if repo == "" {
		return limit, usePool
	}
	configs, err := w.repoConcurrency()
	if err != nil || len(configs) == 0 {
		return limit, usePool
	}
	entry, ok := config.RepoConcurrencyFor(configs, repo)
	if !ok {
		return limit, usePool
	}
	if entry.MaxParallel > 0 {
		limit = entry.MaxParallel
	}
	switch entry.Scheduler {
	case "pool":
		usePool = true
	case "barrier":
		usePool = false
	}
	return limit, usePool
}

// admissionPolicy loads the host-level [admission] budget config, mirroring
// parallelSessionPolicy: an implicit/empty config home uses the defaults
// (both caps 0 ⇒ off), and an explicit home loads from the config file.
func (w jobWorker) admissionPolicy() (config.AdmissionPolicy, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return config.DefaultAdmissionPolicy(), nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return config.AdmissionPolicy{}, err
	}
	return config.LoadAdmissionPolicy(paths)
}

// loadAdmissionBudget builds the opt-in *admissionBudget from the [admission]
// config, returning nil when the feature is off (both caps 0/unset) or the
// config cannot be loaded — nil keeps scheduling byte-identical to today. The
// supervisors call this once at startup and share the returned pointer across all
// per-repo dispatch passes so the cap is process-global.
func (w jobWorker) loadAdmissionBudget() *admissionBudget {
	policy, err := w.admissionPolicy()
	if err != nil {
		return nil
	}
	return newAdmissionBudget(policy)
}

// perJobAdmissionEstimate maps a queued job's runtime to its admission cost
// (#365): whether it holds a resumable runtime session (so it counts against
// max_concurrent_sessions) and its configured RAM estimate (GB). A job whose
// runtime has no resumable session key — exactly the runtimes already exempt from
// the runtime session lock (queuedJobRuntimeResourceKey returns "") — is "not
// session-counted" and contributes 0 RAM, per the frozen goal. Otherwise the job
// is session-counted and its RAM is the per-runtime prior, falling back to
// default_memory_gb for a session runtime not explicitly mapped.
func perJobAdmissionEstimate(ctx context.Context, store *db.Store, job db.Job, policy config.AdmissionPolicy) admissionEstimate {
	if queuedJobRuntimeResourceKey(ctx, store, job) == "" {
		return admissionEstimate{session: false, memGB: 0}
	}
	if store == nil {
		return admissionEstimate{session: true, memGB: policy.DefaultMemoryGB}
	}
	agent, err := store.GetAgent(ctx, job.Agent)
	if err != nil {
		return admissionEstimate{session: true, memGB: policy.DefaultMemoryGB}
	}
	switch strings.TrimSpace(runtimeAgent(agent).Runtime) {
	case runtime.CodexRuntime:
		return admissionEstimate{session: true, memGB: policy.CodexMemoryGB}
	case runtime.ClaudeRuntime:
		return admissionEstimate{session: true, memGB: policy.ClaudeMemoryGB}
	case runtime.KimiRuntime, runtime.KimiCLIRuntime:
		return admissionEstimate{session: true, memGB: policy.KimiMemoryGB}
	default:
		return admissionEstimate{session: true, memGB: policy.DefaultMemoryGB}
	}
}

// admissionEstimate resolves the per-job admission cost (session-ness + RAM) for
// THIS worker's configured admission policy. It is the thunk handed to
// admissionBudget.Reserve at the dispatch reserve points: Reserve invokes it ONLY
// when the budget is active (non-nil) and the job is not already in flight, so on
// the default (no [admission] config) off path it is never called and the
// dispatch loop does ZERO extra config-file I/O or DB lookups — keeping that path
// byte-identical. A load error degrades to the default policy so a transient
// config read never silently disables a gate.
func (w jobWorker) admissionEstimate(ctx context.Context, job db.Job) admissionEstimate {
	policy, err := w.admissionPolicy()
	if err != nil {
		policy = config.DefaultAdmissionPolicy()
	}
	return perJobAdmissionEstimate(ctx, w.Store, job, policy)
}

// orchestratePolicy loads the host-level [orchestrate] cockpit policy, mirroring
// parallelSessionPolicy: an implicit/empty config home uses the defaults, and an
// explicit home loads from the config file. It is best-effort at the call site —
// a load error degrades to no cockpit (the job runs unwrapped).
func (w jobWorker) orchestratePolicy() (config.OrchestratePolicy, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return config.DefaultOrchestratePolicy(), nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return config.OrchestratePolicy{}, err
	}
	return config.LoadOrchestratePolicy(paths)
}

// newCockpit constructs a *cockpit.Cockpit from the orchestrate policy, backed by
// the db store via the cockpitPaneStore shim. When the policy disables cockpit
// panes (mode "off") it returns nil so the caller skips wrapping entirely. The
// herdr binary is taken from HERDR_BIN (falling back to "herdr").
func (w jobWorker) newCockpit(policy config.OrchestratePolicy) *cockpit.Cockpit {
	if policy.CockpitMode == config.CockpitModeOff {
		return nil
	}
	return cockpit.New(cockpit.Options{
		HerdrBin:    firstNonEmpty(os.Getenv("HERDR_BIN"), "herdr"),
		MaxPanes:    policy.CockpitMaxPanes,
		PaneKeyMode: policy.CockpitPaneKey,
	}, cockpitPaneStore{store: w.Store})
}

// cockpitJobMeta builds the cockpit.JobMeta for a delegation job from the decoded
// payload, the runtime agent, and the resolved checkout dir. The pane key follows
// the policy pane-key mode: "seat" keys by agent (one pane per logical seat),
// otherwise the job id (one pane per job, the P0 default).
func cockpitJobMeta(job db.Job, payload workflow.JobPayload, agent runtime.Agent, checkout string, paneKeyMode string) cockpit.JobMeta {
	paneKey := job.ID
	if paneKeyMode == config.CockpitPaneKeySeat {
		paneKey = agent.Name
	}
	// A root coordinator job has an empty payload.RootJobID; its own id IS the
	// root (mirrors Engine.rootJobID). Without this every root collides into one
	// herdr workspace keyed by "".
	root := payload.RootJobID
	if strings.TrimSpace(root) == "" {
		root = job.ID
	}
	return cockpit.JobMeta{
		JobID:     job.ID,
		RootJobID: root,
		Agent:     agent.Name,
		Runtime:   agent.Runtime,
		Action:    job.Type,
		Branch:    payload.Branch,
		Worktree:  checkout,
		PaneKey:   paneKey,
		Depth:     payload.DelegationDepth,
	}
}

// cockpitTeeAdapter creates the per-job log the cockpit pane tails and rebuilds
// the runtime adapter to tee the child's live stdout/stderr into it. It is called
// ONLY on the wrapping path (herdr available), so non-cockpit and cockpit-off
// jobs never create a log file or tee and stay byte-identical. The log lives at
// <home>/logs/jobs/<jobid>.log and is created+truncated so each run starts fresh.
// The tee uses a TeeRunner whose inner is GroupRunner{}, so process-group kill is
// preserved and the buffered Result the adapter consumes is unchanged.
//
// It is fail-open: any failure (paths unresolved, mkdir, create, or an
// unsupported runtime) returns a nil *os.File so the caller skips teeing and the
// pane falls back to the P0 `job watch` command. The returned *os.File is the
// caller's to Close after the job runs; when nil the adapter/path are ignored.
func (w jobWorker) cockpitTeeAdapter(agent runtime.Agent, checkout string, jobID string, additionalOutput ...io.Writer) (workflow.DeliveryAdapter, string, *os.File) {
	paths, err := pathsFromFlag(w.ConfigHome)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit log path resolve failed: %v", jobID, err)
		return nil, "", nil
	}
	dir := filepath.Join(paths.Logs, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeLine(w.Stdout, "job %s cockpit log dir create failed: %v", jobID, err)
		return nil, "", nil
	}
	// Sanitize the job id into a flat, path-safe filename: delegation/continuation
	// job ids contain '/' (e.g. "root/delegation/haiku-ocean", ".../continuation"),
	// which would nest the log into dirs that are never created and fail os.Create →
	// the live tail silently falls back to the P0 pane. A flat slug keeps it one
	// file in this dir (no deep per-job dir trees).
	logPath := filepath.Join(dir, cockpit.SafeLogName(jobID)+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit log create failed: %v", jobID, err)
		return nil, "", nil
	}
	if err := logFile.Chmod(0o600); err != nil {
		_ = logFile.Close()
		writeLine(w.Stdout, "job %s cockpit log chmod failed: %v", jobID, err)
		return nil, "", nil
	}
	return w.cockpitTeeOnFile(agent, checkout, jobID, logPath, logFile, additionalOutput...)
}

// cockpitTeeOnFile rebuilds the runtime adapter to tee the child's live
// stdout/stderr into an already-open log file, shared by the per-job (truncate)
// and per-seat (append) log paths. It is fail-open: an unsupported runtime closes
// the file and returns nils so the caller falls back to the P0 pane.
func (w jobWorker) cockpitTeeOnFile(agent runtime.Agent, checkout, jobID, logPath string, logFile *os.File, additionalOutput ...io.Writer) (workflow.DeliveryAdapter, string, *os.File) {
	outputs := append([]io.Writer{logFile}, additionalOutput...)
	adapter, err := buildRuntimeAdapter(w.ConfigHome, agent, checkout, subprocess.TeeRunner{Inner: subprocess.GroupRunner{}, Out: runtimeOutputWriter(outputs...)})
	if err != nil {
		// Unsupported runtime: this should never happen (AdapterFactory already
		// built one above), but stay fail-open rather than leak the open file.
		_ = logFile.Close()
		writeLine(w.Stdout, "job %s cockpit tee adapter build failed: %v", jobID, err)
		return nil, "", nil
	}
	return adapter, logPath, logFile
}

// cockpitLogAdapter picks the live-output log per PaneKeyMode (Task 7): seat mode
// uses the stable per-seat append log so the one seat pane tails one accumulating
// file across rounds; job mode keeps the per-job truncate log (byte-identical to
// P1). It is called only on the wrapping path (herdr available); a nil *os.File
// means fall back to the P0 pane.
func (w jobWorker) cockpitLogAdapter(cp *cockpit.Cockpit, agent runtime.Agent, checkout, jobID, rootJobID, paneKey string, seatMode bool, additionalOutput ...io.Writer) (workflow.DeliveryAdapter, string, *os.File) {
	if seatMode {
		return w.cockpitSeatLogAdapter(cp, agent, checkout, jobID, rootJobID, paneKey, additionalOutput...)
	}
	return w.cockpitTeeAdapter(agent, checkout, jobID, additionalOutput...)
}

// cockpitSeatLogAdapter opens the stable per-seat append log the seat's one pane
// tails across delegation rounds (Task 7) and tees the child's stdout/stderr into
// it. The path is <home>/logs/seats/<rootShort>/<seatSlug>.log, opened O_APPEND so
// each round's output accumulates rather than truncating the prior round's — no
// tail re-pointing needed. The log is NOT removed per job; it persists for the
// root's life and is removed by FinalizeRoot. It is fail-open: any failure
// (unresolved path, mkdir, create, unsupported runtime) returns nils so the caller
// falls back to the P0 pane.
func (w jobWorker) cockpitSeatLogAdapter(cp *cockpit.Cockpit, agent runtime.Agent, checkout, jobID, rootJobID, paneKey string, additionalOutput ...io.Writer) (workflow.DeliveryAdapter, string, *os.File) {
	logPath := cp.SeatLogPath(rootJobID, paneKey)
	if logPath == "" {
		// Home unset (cockpit could not resolve GITMOOT_HOME): fall back to the P0
		// pane rather than an unstable seat log.
		return nil, "", nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		writeLine(w.Stdout, "job %s cockpit seat log dir create failed: %v", jobID, err)
		return nil, "", nil
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit seat log open failed: %v", jobID, err)
		return nil, "", nil
	}
	if err := logFile.Chmod(0o600); err != nil {
		_ = logFile.Close()
		writeLine(w.Stdout, "job %s cockpit seat log chmod failed: %v", jobID, err)
		return nil, "", nil
	}
	return w.cockpitTeeOnFile(agent, checkout, jobID, logPath, logFile, additionalOutput...)
}

// finalizeCockpitRootIfDone tears the root's cockpit down once the coordination
// tree it belongs to has terminated (Task 7/8, seat mode only). It runs on a
// wrapped seat-mode job's return: if every job sharing the root is terminal, it
// calls FinalizeRoot (close panes / workspace, delete rows, remove seat logs) and,
// when this job is the terminal coordinator continuation, FocusRoot to surface the
// reconvene view. Everything is best-effort: it is deferred on a detached context
// so a cockpit/herdr problem never affects the job. Job mode never reaches here, so
// its per-Deliver teardown stays byte-identical.
func (w jobWorker) finalizeCockpitRootIfDone(cp *cockpit.Cockpit, job db.Job, payload workflow.JobPayload, rootJobID string) {
	ctx := context.Background()
	// Cheap scoped guard before the full job-table scan: short-circuit only when the
	// root has NEITHER a live pane row NOR a registered workspace (none opened, or
	// already finalized) — there is then nothing to tear down, so the redundant
	// rootTreeTerminal scans on every in-tree job's completion are skipped. Job mode
	// deletes pane rows per-Deliver, so by root-terminal the pane list is empty while
	// a cockpit_workspaces row still needs closing; gating on the pane list alone
	// would skip that workspace teardown (the leftover-workspace bug). Any store error
	// falls through to the (idempotent, best-effort) finalize rather than skipping.
	if panes, perr := w.Store.ListCockpitPanesByRoot(ctx, rootJobID); perr == nil && len(panes) == 0 {
		if _, found, wsErr := w.Store.GetWorkspaceForRoot(ctx, rootJobID); wsErr == nil && !found {
			return
		}
	}
	done, err := w.rootTreeTerminal(ctx, rootJobID)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit root-finalize check failed: %v", job.ID, err)
		return
	}
	if !done {
		return
	}
	// A terminal continuation that absorbed the children (a finalize continuation,
	// or a coordinator continuation that returned no further delegations) is the
	// reconvene point: surface the root workspace so the synthesized verdict —
	// which lands in the coordinator's own pane (its continuation shares the
	// coordinator seat in seat mode) — is brought forward.
	if w.isReconveneContinuation(ctx, job, payload) {
		cp.FocusRoot(ctx, rootJobID)
	}
	cp.FinalizeRoot(ctx, rootJobID)
}

// rootTreeTerminal reports whether every job in the coordination tree rooted at
// rootJobID is terminal (succeeded/failed/cancelled) — i.e. nothing is still
// queued, running, or blocked (a blocked job can resume, so it is not terminal). It lists jobs and matches the root id against each
// job's own id (the root coordinator) or its payload RootJobID (children +
// continuations), mirroring the engine's per-root reasoning. It fails closed
// (returns false) on any unparseable payload so a transient hiccup never triggers
// a premature teardown.
func (w jobWorker) rootTreeTerminal(ctx context.Context, rootJobID string) (bool, error) {
	rootJobID = strings.TrimSpace(rootJobID)
	if rootJobID == "" {
		return false, nil
	}
	jobs, err := w.Store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		inTree := j.ID == rootJobID
		if !inTree {
			p, perr := daemonJobPayload(j)
			if perr != nil {
				// An unparseable job payload could belong to the tree; do not finalize
				// while its membership/state is unknown.
				return false, nil
			}
			inTree = strings.TrimSpace(p.RootJobID) == rootJobID
		}
		if !inTree {
			continue
		}
		// Root-tree finalization uses FINAL (resumability) semantics, NOT settled:
		// a blocked job is deliberately non-final (it can resume via RetryJob), so
		// the tree is not terminal while any in-tree job is blocked. Finalizing then
		// would tear down a pane + seat log the job still needs. The engine's
		// graceful-finalize continuation provides the real terminal signal for a
		// stuck tree. See #632 (IsFinalJobState vs IsSettledJobState).
		if !workflow.IsFinalJobState(j.State) {
			return false, nil
		}
	}
	// Every in-tree job (if any) is terminal — the tree is done. An already-pruned
	// root (no jobs found) is also terminal: a late finalize is a harmless no-op.
	return true, nil
}

// isReconveneContinuation reports whether this job is the coordinator's terminal
// reconvene point: a finalize continuation, or any coordinator continuation that
// returned no further delegations (so the tree stops here). It is the signal to
// refocus the root workspace on the synthesized verdict (Task 8).
func (w jobWorker) isReconveneContinuation(ctx context.Context, job db.Job, payload workflow.JobPayload) bool {
	if payload.DelegationFinalize {
		return true
	}
	// A continuation job carries a parent (the prior coordinator job in the chain).
	// When such a continuation returns no delegations, the coordination tree has
	// reconvened on it.
	if strings.TrimSpace(payload.ParentJobID) == "" {
		// The root coordinator itself: a reconvene point only if it spawned no
		// children (it ran to completion without delegating).
		children, err := w.Store.ListJobsByParent(ctx, job.ID)
		if err != nil {
			return false
		}
		return len(children) == 0
	}
	if payload.Result != nil && len(payload.Result.Delegations) > 0 {
		return false
	}
	return true
}

// maybeWrapCockpit decides whether a job's delivery is wrapped in a herdr pane.
// It is a pure helper (no daemon state) so the wrap-vs-passthrough decision is
// directly unit-testable. The returned unavailable flag is true exactly when the
// caller should emit a single cockpit_unavailable job event:
//   - not requested (payload.Cockpit false): inner unchanged, no event.
//   - requested but the policy mode is off: skip entirely, inner unchanged, no
//     event (an off host opted out, so there is nothing to warn about).
//   - requested, mode not off, but the cockpit is nil or herdr is not available:
//     inner unchanged, unavailable=true so the caller emits the event.
//   - requested and available: the wrapped adapter, no event.
//
// Cockpit construction/Available failures are fail-open by contract: cp.Wrap
// already returns inner untouched when Available is false.
func maybeWrapCockpit(cp *cockpit.Cockpit, requested bool, modeOff bool, inner workflow.DeliveryAdapter, meta cockpit.JobMeta) (workflow.DeliveryAdapter, bool) {
	if !requested || modeOff {
		return inner, false
	}
	if !maybeWrapCockpitAvailable(cp, requested, modeOff) {
		return inner, true
	}
	return cp.Wrap(inner, meta), false
}

// maybeWrapCockpitAvailable reports whether the cockpit will actually wrap this
// job's delivery in a pane: requested, the host did not opt out (mode off), and
// herdr is reachable. It is the single source of truth the daemon uses BOTH to
// decide whether to set up the per-job tee log (so logs/tees are created only on
// the wrapping path) and inside maybeWrapCockpit's final decision, so the two can
// never drift. Availability is cached (availableTTL) so the extra call is cheap.
func maybeWrapCockpitAvailable(cp *cockpit.Cockpit, requested bool, modeOff bool) bool {
	if !requested || modeOff || cp == nil {
		return false
	}
	return cp.Available(context.Background())
}

func tempWorkerEligible(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload, agent runtime.Agent, policy config.ParallelSessionPolicy, now time.Time) tempWorkerEligibility {
	if payload.Ephemeral != nil {
		// An ephemeral job already runs directly on its own throwaway worker;
		// forking it into a second temp worker would double-spawn.
		return tempWorkerEligibility{Reason: "ephemeral worker runs directly"}
	}
	if strings.TrimSpace(payload.RuntimeOverride) != "" {
		// An override job already runs on its own per-job session (a fresh ref or
		// an explicit --session); forking a temp worker from the effective agent
		// would re-derive a second session for the same one-shot job.
		return tempWorkerEligibility{Reason: "runtime override runs on its own session"}
	}
	if payload.DelegationReason == "temp_worker_merge_back" {
		return tempWorkerEligibility{Reason: "merge-back waits for original runtime session"}
	}
	if payload.DelegationReason == "runtime_session_busy" {
		return tempWorkerEligibility{Reason: "delegated temp worker waits for assigned runtime session"}
	}
	if policy.SameSession != config.ParallelSessionForkTempSession {
		return tempWorkerEligibility{Reason: "parallel_sessions.same_session is queue"}
	}
	switch agent.Runtime {
	case runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime, runtime.KimiCLIRuntime:
	default:
		return tempWorkerEligibility{Reason: fmt.Sprintf("runtime %s does not support temp workers", agent.Runtime)}
	}
	if !parallelSessionActionAllowed(job.Type, policy.EligibleActions) {
		return tempWorkerEligibility{Reason: fmt.Sprintf("action %s is not eligible", job.Type)}
	}
	if readOnlyImplementationBlocked(job.Type, agent) {
		return tempWorkerEligibility{Reason: "implementation requires writable agent policy"}
	}
	if strings.TrimSpace(job.Type) == "implement" {
		path, ok := queuedJobTaskWorktreePath(ctx, store, payload)
		if !ok {
			return tempWorkerEligibility{Reason: "implementation requires task worktree"}
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return tempWorkerEligibility{Reason: "implementation task worktree is missing"}
		}
	}
	if store != nil {
		count, err := store.CountActiveAgentInstances(ctx, tempWorkerAgentType(agent.Name), agent.AutonomyPolicy, now)
		if err != nil {
			return tempWorkerEligibility{Reason: fmt.Sprintf("count active temp workers: %v", err)}
		}
		if count >= policy.MaxTempSessionsPerAgent {
			return tempWorkerEligibility{Reason: fmt.Sprintf("max temp workers reached for %s", agent.Name)}
		}
	}
	return tempWorkerEligibility{Eligible: true}
}

func parallelSessionActionAllowed(action string, eligibleActions []string) bool {
	action = strings.TrimSpace(action)
	for _, candidate := range eligibleActions {
		if strings.TrimSpace(candidate) == action {
			return true
		}
	}
	return false
}

func tempWorkerAgentType(agentName string) string {
	return "temp:" + strings.TrimSpace(agentName)
}

type tempWorkerStartResult struct {
	Agent       runtime.Agent
	IdleTimeout time.Duration
	JobTimeout  time.Duration
}

func (w jobWorker) runWithTempWorker(ctx context.Context, job db.Job, payload workflow.JobPayload, original runtime.Agent, checkout string, policy config.ParallelSessionPolicy, reason string) error {
	started, err := w.startTempWorker(ctx, job, payload, original, checkout)
	if err != nil {
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "temp_worker_failed", Message: err.Error()}); eventErr != nil {
			return eventErr
		}
		waitMessage := fmt.Sprintf("%s; temp worker start failed: %v", reason, err)
		// Once per wait episode (#598); busy error returned unconditionally so the
		// pool dispatcher observes the bounce.
		if !runtimeLockWaitEpisodeOpen(job.ID) {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: waitMessage}); eventErr != nil {
				return eventErr
			}
			markRuntimeLockWaitEpisode(job.ID)
			writeLine(w.Stdout, "job %s waiting: %s", job.ID, waitMessage)
		}
		return fmt.Errorf("%w: %s", errRuntimeSessionBusy, waitMessage)
	}
	// A per-delegation timeout on the payload overrides the agent-type job
	// timeout for both the lock TTL and the run deadline below.
	if d, perr := time.ParseDuration(strings.TrimSpace(payload.JobTimeout)); perr == nil && d > 0 {
		started.JobTimeout = d
	}
	payload.OriginalAgent = original.Name
	payload.DelegatedAgent = started.Agent.Name
	payload.DelegationReason = "runtime_session_busy"
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	delegated, err := w.Store.DelegateQueuedJob(ctx, job.ID, original.Name, started.Agent.Name, string(encoded), db.JobEvent{
		JobID:   job.ID,
		Kind:    "temp_worker_delegated",
		Message: fmt.Sprintf("delegated from %s to %s: %s", original.Name, started.Agent.Name, reason),
	})
	if err != nil {
		w.cleanupTempWorker(context.Background(), started.Agent.Name)
		return err
	}
	if !delegated {
		w.cleanupTempWorker(context.Background(), started.Agent.Name)
		return nil
	}
	delegatedJob, err := w.Store.GetJob(ctx, job.ID)
	if err != nil {
		return err
	}
	adapter, err := w.AdapterFactory(started.Agent, checkout)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	writeLine(w.Stdout, "running job %s for temporary worker %s in %s", job.ID, started.Agent.Name, payload.Repo)
	// Same lease-outlives-context invariant as run(): the temp-worker run context is
	// armed at started.JobTimeout below, so the lease must be jobTimeout+grace to
	// cover teardown and avoid the #536 live-worker reap+requeue window.
	tempLockTTL := defaultDaemonRunningJobStaleAfter
	if started.JobTimeout > 0 {
		tempLockTTL = started.JobTimeout + runtimeLeaseTeardownGrace
	}
	releaseLock, acquired, lockKey, ownerToken, err := acquireRuntimeSessionLock(ctx, w.Store, delegatedJob.ID, started.Agent, time.Now().UTC(), tempLockTTL)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	if !acquired {
		message := fmt.Sprintf("runtime session %s is busy", lockKey)
		// Once per wait episode (#598); busy error returned unconditionally so the
		// pool dispatcher observes the bounce.
		if !runtimeLockWaitEpisodeOpen(delegatedJob.ID) {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: delegatedJob.ID, Kind: "runtime_lock_wait", Message: message}); eventErr != nil {
				return eventErr
			}
			markRuntimeLockWaitEpisode(delegatedJob.ID)
			writeLine(w.Stdout, "job %s waiting: %s", delegatedJob.ID, message)
		}
		return fmt.Errorf("%w: %s", errRuntimeSessionBusy, message)
	}
	// Acquired the temp worker's runtime lock (delegatedJob.ID == job.ID; the
	// delegation keeps the same job id): close any open wait episode.
	endRuntimeLockWaitEpisode(delegatedJob.ID)
	defer func() {
		if err := releaseLock(context.Background()); err != nil {
			writeLine(w.Stdout, "job %s temp runtime lock release failed: %v", delegatedJob.ID, err)
		}
	}()
	// See runQueuedJob: thread the owner token so terminal cleanup recognizes this
	// run's own still-held lock and does not refuse the healthy-path cleanup (#536).
	ctx = workflow.WithRuntimeSelfOwnerToken(ctx, ownerToken)
	// Produce temp workers use the same post-admission filesystem authorization
	// and Landlock adapter wrapping as the primary worker path. Without this seam,
	// runtime-session contention could route Claude/Kimi around the launch sandbox.
	if err := applyProduceRuntimeGrants(ctx, w.Store, w.ConfigHome, delegatedJob, payload, &started.Agent); err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	adapter, err = wrapProduceSandboxAdapter(delegatedJob.Type, started.Agent, adapter)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	if err := w.Store.MarkAgentInstanceRunning(ctx, started.Agent.Name, time.Now().UTC(), started.JobTimeout); err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	defer func() {
		if err := w.Store.TouchAgentInstance(context.Background(), started.Agent.Name, time.Now().UTC(), started.IdleTimeout); err != nil {
			writeLine(w.Stdout, "job %s temp worker state update failed: %v", delegatedJob.ID, err)
		}
	}()
	runCtx, stopRun := w.runningJobContext(ctx, job.ID)
	defer stopRun()
	if started.JobTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, started.JobTimeout)
		defer cancel()
	}
	engine := w.WorkflowFactory(checkout)
	_, err = engine.RunJob(runCtx, delegatedJob.ID, started.Agent, adapter)
	if err != nil {
		if markErr := w.handleRunJobError(ctx, delegatedJob.ID, err); markErr != nil {
			return markErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		writeLine(w.Stdout, "job %s failed: %v", delegatedJob.ID, err)
		return nil
	}
	if policy.MergeBack == config.ParallelSessionMergeBackSummary {
		if err := w.queueTempWorkerMergeBack(ctx, delegatedJob.ID, original, started.Agent); err != nil {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: delegatedJob.ID, Kind: "temp_worker_merge_back_failed", Message: err.Error()}); eventErr != nil {
				return eventErr
			}
			return err
		}
	}
	_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, nil)
	writeLine(w.Stdout, "job %s completed by temporary worker %s", delegatedJob.ID, started.Agent.Name)
	return nil
}

func (w jobWorker) queueTempWorkerMergeBack(ctx context.Context, completedJobID string, original runtime.Agent, tempAgent runtime.Agent) error {
	completedJob, err := w.Store.GetJob(ctx, completedJobID)
	if err != nil {
		return err
	}
	payload, err := daemonJobPayload(completedJob)
	if err != nil {
		return err
	}
	if payload.Result == nil {
		return fmt.Errorf("completed temp-worker job %s has no result", completedJob.ID)
	}
	mergeBackID := completedJob.ID + "-merge-back"
	if _, err := w.Store.GetJob(ctx, mergeBackID); err == nil {
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: completedJob.ID, Kind: "temp_worker_merge_back_existing", Message: fmt.Sprintf("summary merge-back job %s already exists", mergeBackID)})
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	request := workflow.JobRequest{
		ID:               mergeBackID,
		Agent:            original.Name,
		Action:           "ask",
		Model:            payload.Model,
		Effort:           payload.Effort,
		Repo:             payload.Repo,
		Branch:           payload.Branch,
		GoalID:           payload.GoalID,
		TaskID:           payload.TaskID,
		TaskTitle:        payload.TaskTitle,
		LeadAgent:        payload.LeadAgent,
		Reviewers:        payload.Reviewers,
		ReviewRound:      payload.ReviewRound,
		Sender:           tempAgent.Name,
		Instructions:     tempWorkerMergeBackInstructions(completedJob, payload, tempAgent.Name),
		OriginalAgent:    original.Name,
		DelegatedAgent:   tempAgent.Name,
		DelegationReason: "temp_worker_merge_back",
		Constraints: []string{
			"This is a temp-worker merge-back summary only.",
			"Do not edit files, create commits, open pull requests, or dispatch more agents unless the summary explicitly requires follow-up.",
		},
	}
	if _, err := (workflow.Mailbox{Store: w.Store, CanaryEnabled: canaryRoutingEnabled(w.workflowHome())}).Enqueue(ctx, request); err != nil {
		return err
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: completedJob.ID, Kind: "temp_worker_merge_back_queued", Message: fmt.Sprintf("queued summary merge-back job %s for %s", mergeBackID, original.Name)})
}

func tempWorkerMergeBackInstructions(job db.Job, payload workflow.JobPayload, tempAgentName string) string {
	result := payload.Result
	var builder strings.Builder
	fmt.Fprintf(&builder, "Temporary worker %s completed job %s.\n", tempAgentName, job.ID)
	fmt.Fprintf(&builder, "Repo: %s\n", payload.Repo)
	if strings.TrimSpace(payload.Branch) != "" {
		fmt.Fprintf(&builder, "Branch: %s\n", payload.Branch)
	}
	if payload.PullRequest > 0 {
		fmt.Fprintf(&builder, "Pull request: #%d\n", payload.PullRequest)
	}
	if strings.TrimSpace(payload.HeadSHA) != "" {
		fmt.Fprintf(&builder, "Head SHA: %s\n", payload.HeadSHA)
	}
	fmt.Fprintf(&builder, "Decision: %s\n", result.Decision)
	if strings.TrimSpace(result.Summary) != "" {
		fmt.Fprintf(&builder, "Summary: %s\n", result.Summary)
	}
	appendMergeBackList(&builder, "Changes made", result.ChangesMade)
	appendMergeBackList(&builder, "Tests run", result.TestsRun)
	appendMergeBackList(&builder, "Needs", result.Needs)
	builder.WriteString("\nAcknowledge the summary and keep any follow-up concise.")
	return builder.String()
}

func appendMergeBackList(builder *strings.Builder, label string, values []string) {
	values = compactMergeBackStrings(values)
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(builder, "%s:\n", label)
	for _, value := range values {
		fmt.Fprintf(builder, "- %s\n", value)
	}
}

func compactMergeBackStrings(values []string) []string {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (w jobWorker) startTempWorker(ctx context.Context, job db.Job, payload workflow.JobPayload, original runtime.Agent, checkout string) (tempWorkerStartResult, error) {
	idleTimeout := 20 * time.Minute
	jobTimeout := defaultDaemonRunningJobStaleAfter
	if managed, err := w.managedJobConfig(ctx, original.Name); err == nil && managed.OK {
		idleTimeout = managed.IdleTimeout
		jobTimeout = managed.JobTimeout
	} else if err != nil {
		return tempWorkerStartResult{}, err
	}
	tempAgent := original
	tempAgent.Name = tempWorkerInstanceName(original.Name, job.ID)
	tempAgent.RuntimeRef = ""
	// A temp worker's session is started for this one job and disposed after it
	// — single-use, so session-cumulative usage (codex, #658) is the job's cost.
	tempAgent.SingleUseSession = true
	var cachedTemplate db.AgentTemplate
	if tempAgent.TemplateID != "" {
		var err error
		cachedTemplate, err = loadInstalledTemplate(ctx, w.Store, tempAgent.TemplateID)
		if err != nil {
			return tempWorkerStartResult{}, err
		}
	}
	now := time.Now().UTC()
	reserved := db.AgentInstance{
		Name:           tempAgent.Name,
		Type:           tempWorkerAgentType(original.Name),
		Runtime:        tempAgent.Runtime,
		RuntimeRef:     "starting:" + tempAgent.Name,
		RepoFullName:   payload.Repo,
		Role:           tempAgent.Role,
		TemplateID:     tempAgent.TemplateID,
		Model:          tempAgent.Model,
		Effort:         tempAgent.Effort,
		Capabilities:   tempAgent.Capabilities,
		AutonomyPolicy: tempAgent.AutonomyPolicy,
		State:          "starting",
		CreatedAt:      formatManagedAgentTime(now),
		LastUsedAt:     formatManagedAgentTime(now),
		ExpiresAt:      formatManagedAgentTime(now.Add(jobTimeout)),
	}
	if err := w.Store.UpsertAgentInstance(ctx, reserved); err != nil {
		return tempWorkerStartResult{}, err
	}
	adapter, err := w.StartAdapterFactory(tempAgent.Runtime, checkout)
	if err != nil {
		_ = w.Store.DeleteAgentInstance(context.Background(), reserved.Name)
		return tempWorkerStartResult{}, err
	}
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: tempAgent, Prompt: agentStartupPrompt(tempAgent, cachedTemplate)})
	if err != nil {
		_ = w.Store.DeleteAgentInstance(context.Background(), reserved.Name)
		return tempWorkerStartResult{}, err
	}
	tempAgent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(tempAgent); err != nil {
		_ = w.Store.DeleteAgentInstance(context.Background(), reserved.Name)
		return tempWorkerStartResult{}, err
	}
	instance := reserved
	instance.RuntimeRef = tempAgent.RuntimeRef
	instance.State = "idle"
	if err := w.Store.UpsertAgentInstance(ctx, instance); err != nil {
		_ = w.Store.DeleteAgentInstance(context.Background(), reserved.Name)
		return tempWorkerStartResult{}, err
	}
	return tempWorkerStartResult{Agent: tempAgent, IdleTimeout: idleTimeout, JobTimeout: jobTimeout}, nil
}

// startEphemeralWorker materializes a throwaway agent for a job whose payload
// carries an inline worker spec, generalizing the temp-worker machinery from
// "fork an existing agent" to "spawn from a spec". It persists the agent (so the
// rest of run's flow — GetAgent, the engine's executor checks — finds it),
// associates payload.Repo via the agent's RepoScope, and reserves + starts a
// runtime session (mirroring startTempWorker). The agent name on the job is
// already the engine-assigned "-ephemeral-" name; callers register a deferred
// cleanupTempWorker to auto-dispose the worker on every exit path. The worker
// runs read-only unless the spec opts into a writable autonomy policy.
func (w jobWorker) startEphemeralWorker(ctx context.Context, job db.Job, payload workflow.JobPayload) (err error) {
	spec := payload.Ephemeral
	if spec == nil {
		return errors.New("ephemeral worker requires a spec")
	}
	capabilities := spec.Capabilities
	if len(capabilities) == 0 {
		capabilities = []string{job.Type}
	}
	// Least privilege: default read-only, except an implement must be able to
	// write. The spec may still opt into a different (validated) policy. Note an
	// EMPTY-policy implement spec is already refused upstream by
	// validateEphemeralSpec (#452), so this implement default is now defense in
	// depth for any path that reaches here without that validation.
	defaultPolicy := runtime.AutonomyPolicyReadOnly
	if job.Type == "implement" {
		defaultPolicy = runtime.AutonomyPolicyWorkspaceWrite
	}
	policy := firstNonEmpty(strings.TrimSpace(spec.AutonomyPolicy), defaultPolicy)
	// Role is required by runtime.ValidateAgent but optional on the spec; fall
	// back to the job action (e.g. "review"/"implement"), then a generic role.
	role := firstNonEmpty(strings.TrimSpace(spec.Role), strings.TrimSpace(job.Type), "worker")
	ephemeralAgent := runtime.Agent{
		Name:           job.Agent,
		Role:           role,
		Runtime:        spec.Runtime,
		Model:          spec.Model,
		Effort:         spec.Effort,
		TemplateID:     spec.Template,
		Capabilities:   capabilities,
		AutonomyPolicy: policy,
		RepoScope:      payload.Repo,
	}
	// Persisting with RepoScope set associates the worker with payload.Repo
	// (agent_repos), mirroring how a normal agent gains repo access.
	if err := w.Store.UpsertAgent(ctx, dbAgent(ephemeralAgent)); err != nil {
		return err
	}
	// The agent row (and, below, its instance + a live runtime session) now
	// exist. Dispose them if any later bring-up step fails so a partial
	// materialization cannot leak an agent/instance/session — mirroring
	// startTempWorker's cleanup-on-error. (The named return err is set by the
	// `return err` paths below.)
	defer func() {
		if err != nil {
			w.cleanupTempWorker(context.Background(), ephemeralAgent.Name)
		}
	}()
	// Normalize the stored policy back onto the in-memory agent so the runtime
	// session is started with the same sandbox the rest of run will use.
	ephemeralAgent.AutonomyPolicy = runtime.NormalizeStoredAutonomyPolicy(ephemeralAgent.AutonomyPolicy)
	checkout, err := w.CheckoutValidator(ctx, job, payload, ephemeralAgent)
	if err != nil {
		return err
	}
	var cachedTemplate db.AgentTemplate
	if ephemeralAgent.TemplateID != "" {
		cachedTemplate, err = loadInstalledTemplate(ctx, w.Store, ephemeralAgent.TemplateID)
		if err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	reserved := db.AgentInstance{
		Name:    ephemeralAgent.Name,
		Type:    tempWorkerAgentType(ephemeralWorkerInstanceOrigin),
		Runtime: ephemeralAgent.Runtime,
		// "starting:" placeholder ref keeps the reserved row valid before the
		// adapter returns the real runtime ref.
		RuntimeRef:     "starting:" + ephemeralAgent.Name,
		RepoFullName:   payload.Repo,
		Role:           ephemeralAgent.Role,
		TemplateID:     ephemeralAgent.TemplateID,
		Model:          ephemeralAgent.Model,
		Effort:         ephemeralAgent.Effort,
		Capabilities:   ephemeralAgent.Capabilities,
		AutonomyPolicy: ephemeralAgent.AutonomyPolicy,
		State:          "starting",
		CreatedAt:      formatManagedAgentTime(now),
		LastUsedAt:     formatManagedAgentTime(now),
		ExpiresAt:      formatManagedAgentTime(now.Add(defaultDaemonRunningJobStaleAfter)),
	}
	if err := w.Store.UpsertAgentInstance(ctx, reserved); err != nil {
		return err
	}
	adapter, err := w.StartAdapterFactory(ephemeralAgent.Runtime, checkout)
	if err != nil {
		return err
	}
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: ephemeralAgent, Prompt: agentStartupPrompt(ephemeralAgent, cachedTemplate)})
	if err != nil {
		return err
	}
	ephemeralAgent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(ephemeralAgent); err != nil {
		return err
	}
	// Persist the live runtime_ref on both the agent row (so GetAgent below
	// resolves a runnable session) and the instance.
	if err := w.Store.UpsertAgent(ctx, dbAgent(ephemeralAgent)); err != nil {
		return err
	}
	instance := reserved
	instance.RuntimeRef = ephemeralAgent.RuntimeRef
	instance.State = "idle"
	if err := w.Store.UpsertAgentInstance(ctx, instance); err != nil {
		return err
	}
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "ephemeral_worker_started", Message: fmt.Sprintf("materialized %s worker %s", ephemeralAgent.Runtime, ephemeralAgent.Name)}); err != nil {
		return err
	}
	writeLine(w.Stdout, "materialized ephemeral worker %s (%s) for job %s in %s", ephemeralAgent.Name, ephemeralAgent.Runtime, job.ID, payload.Repo)
	return nil
}

// ephemeralWorkerInstanceOrigin is the synthetic "original" agent name used in an
// ephemeral worker's instance type. It has no registered instance, so
// managedJobConfig treats the worker as unmanaged (no agent-type config), which
// is correct for a spec-spawned worker that does not belong to a managed pool.
const ephemeralWorkerInstanceOrigin = "gitmoot-ephemeral-spec"

func (w jobWorker) cleanupTempWorker(ctx context.Context, agentName string) {
	if err := w.Store.DeleteAgentInstance(ctx, agentName); err != nil {
		writeLine(w.Stdout, "temp worker %s instance cleanup failed: %v", agentName, err)
	}
	if removed, err := w.Store.RemoveAgent(ctx, agentName); err != nil {
		writeLine(w.Stdout, "temp worker %s agent cleanup failed: %v", agentName, err)
	} else if removed {
		writeLine(w.Stdout, "temp worker %s agent cleanup removed regular agent row", agentName)
	}
}

func tempWorkerInstanceName(agentName string, jobID string) string {
	base := strings.Trim(strings.ToLower(agentName), "-_ ")
	if base == "" {
		base = "agent"
	}
	job := strings.Trim(strings.ToLower(jobID), "-_ ")
	if job == "" {
		job = strconv.FormatInt(time.Now().UTC().UnixNano(), 16)
	}
	name := base + "-temp-" + job
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-_")
}

type managedJobRuntimeConfig struct {
	OK          bool
	JobTimeout  time.Duration
	IdleTimeout time.Duration
}

func (w jobWorker) managedJobConfig(ctx context.Context, agentName string) (managedJobRuntimeConfig, error) {
	instance, err := w.Store.GetAgentInstance(ctx, agentName)
	if errors.Is(err, sql.ErrNoRows) {
		return managedJobRuntimeConfig{}, nil
	}
	if err != nil {
		return managedJobRuntimeConfig{}, err
	}
	configType := instance.Type
	if original := originalAgentForTempWorkerType(instance.Type); original != "" {
		originalInstance, err := w.Store.GetAgentInstance(ctx, original)
		if errors.Is(err, sql.ErrNoRows) {
			return managedJobRuntimeConfig{}, nil
		}
		if err != nil {
			return managedJobRuntimeConfig{}, err
		}
		configType = originalInstance.Type
	}
	types, err := loadAgentTypeConfig(w.ConfigHome)
	if err != nil {
		return managedJobRuntimeConfig{}, err
	}
	agentType, ok := types[configType]
	if !ok {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %q not found for managed instance %s", configType, agentName)
	}
	jobTimeout, err := time.ParseDuration(agentType.JobTimeout)
	if err != nil {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %s job_timeout: %w", configType, err)
	}
	if jobTimeout <= 0 {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %s job_timeout must be positive", configType)
	}
	idleTimeout, err := time.ParseDuration(agentType.IdleTimeout)
	if err != nil {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %s idle_timeout: %w", configType, err)
	}
	if idleTimeout <= 0 {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %s idle_timeout must be positive", configType)
	}
	return managedJobRuntimeConfig{OK: true, JobTimeout: jobTimeout, IdleTimeout: idleTimeout}, nil
}

// effectiveJobTimeout returns the timeout to enforce for a job: the
// per-delegation payload.JobTimeout when it parses to a positive duration,
// otherwise the agent-type managed.JobTimeout, otherwise the daemon stale window
// for unmanaged jobs (so an unmanaged job still has a watchdog, #555). This value
// drives the run context deadline directly; the runtime-session lock TTL is this
// value PLUS runtimeLeaseTeardownGrace (#536/#560) so the lease strictly outlives
// the deadline and the terminal teardown — the lock cannot expire while the
// worker is still finishing, which would otherwise let the reaper requeue a live
// job onto its dirty worktree.
func effectiveJobTimeout(payload workflow.JobPayload, managed managedJobRuntimeConfig) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(payload.JobTimeout)); err == nil && d > 0 {
		return d
	}
	if managed.JobTimeout > 0 {
		return managed.JobTimeout
	}
	return daemonRunningJobStaleAfter
}

func originalAgentForTempWorkerType(typ string) string {
	original, ok := strings.CutPrefix(strings.TrimSpace(typ), "temp:")
	if !ok {
		return ""
	}
	return strings.TrimSpace(original)
}

func (w jobWorker) runningJobContext(ctx context.Context, jobID string) (context.Context, func()) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(daemonJobCancelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				job, err := w.Store.GetJob(ctx, jobID)
				if err == nil && job.State == string(workflow.JobCancelled) {
					cancel()
					return
				}
			}
		}
	}()
	return runCtx, func() {
		cancel()
		<-done
	}
}

func blockTaskForPermissionBlockedJob(ctx context.Context, store *db.Store, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return err
	}
	if strings.TrimSpace(payload.TaskID) == "" {
		return nil
	}
	task := db.Task{
		ID:           payload.TaskID,
		RepoFullName: payload.Repo,
		GoalID:       payload.GoalID,
		Title:        payload.TaskTitle,
		State:        string(workflow.TaskBlocked),
		Branch:       payload.Branch,
	}
	existing, err := store.GetTask(ctx, payload.TaskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if existing.State == string(workflow.TaskDismissed) {
			return fmt.Errorf("task %s is dismissed; permission-blocked job cannot resurrect it", existing.ID)
		}
		if task.RepoFullName == "" {
			task.RepoFullName = existing.RepoFullName
		}
		if task.GoalID == "" {
			task.GoalID = existing.GoalID
		}
		if task.Title == "" {
			task.Title = existing.Title
		}
		if task.Branch == "" {
			task.Branch = existing.Branch
		}
	}
	return store.UpsertTask(ctx, task)
}

func (w jobWorker) jobNeedsAdvanceRetry(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	needsRetry := false
	for _, event := range events {
		switch event.Kind {
		case "advance_started", "advance_retry":
			needsRetry = true
		case "advance_completed", "advance_retried", "advance_blocked", "advance_retry_skipped":
			needsRetry = false
		case "retry_queued":
			needsRetry = false
		}
	}
	return needsRetry, nil
}

// recordAdvanceRetryOnce appends an advance_retry marker UNLESS the job is already
// sitting on one. A terminal job whose post-delivery advancement keeps failing is
// re-attempted on every ~1s tick; appending a fresh advance_retry each time grew
// job_events without bound (a real install reached ~1.8M rows — 96% of the table —
// and the per-tick JobIDsWithPendingAdvanceRetry GROUP BY plus jobNeedsAdvanceRetry's
// per-job ListJobEvents pinned a core with zero jobs in flight). Only the latest
// marker per job is ever consulted (last-one-wins), so a job already on advance_retry
// stays a candidate and keeps retrying with no new row; any other latest marker
// (advance_started, or a prior terminal resolution before a re-trigger) still records
// the transition to advance_retry. jobNeedsAdvanceRetry and JobIDsWithPendingAdvanceRetry
// see an identical candidate set either way.
func (w jobWorker) recordAdvanceRetryOnce(ctx context.Context, jobID, message string) error {
	latest, err := w.Store.LatestAdvancementMarker(ctx, jobID)
	if err != nil {
		return err
	}
	if latest == "advance_retry" {
		// Keep the single-row bound but refresh the surviving row so the why-stuck
		// surface (#552) reports the current failure, not the first one.
		_, err := w.Store.RefreshLatestAdvanceRetry(ctx, jobID, message)
		return err
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_retry", Message: message})
}

func (w jobWorker) advanceJob(ctx context.Context, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retry_skipped", Message: err.Error()})
	}
	dbAgent, err := w.Store.GetAgent(ctx, job.Agent)
	if err != nil {
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retry_skipped", Message: err.Error()})
	}
	agent := runtimeAgent(dbAgent)
	if refreshed, ok, err := w.refreshImplementedPayloadForRetry(ctx, job, payload); err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry refresh failed: "+err.Error())
	} else if ok {
		payload = refreshed
	}
	checkout, err := w.CheckoutValidator(ctx, job, payload, agent)
	if err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry preflight failed: "+err.Error())
	}
	engine := w.WorkflowFactory(checkout)
	if err := engine.AdvanceJob(ctx, job.ID); err != nil {
		var awaiting workflow.AwaitingHumanError
		if errors.As(err, &awaiting) {
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_awaiting_human", Message: err.Error()})
		}
		var blocked workflow.BlockedError
		if errors.As(err, &blocked) {
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_blocked", Message: err.Error()})
		}
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry failed: "+err.Error())
	}
	writeLine(w.Stdout, "job %s advancement retried", job.ID)
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retried", Message: "post-delivery workflow retry completed"})
}

func (w jobWorker) refreshImplementedPayloadForRetry(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, bool, error) {
	if job.Type != "implement" || payload.Result == nil || payload.Result.Decision != "implemented" {
		return payload, false, nil
	}
	checkout, err := w.resolveJobCheckout(ctx, job, payload)
	if err != nil {
		return workflow.JobPayload{}, false, err
	}
	payload, err = refreshDaemonJobPayload(ctx, w.Store, checkout, job, payload)
	if err != nil {
		return workflow.JobPayload{}, false, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return workflow.JobPayload{}, false, err
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		return workflow.JobPayload{}, false, err
	}
	return payload, true, nil
}

func (w jobWorker) defaultAdapter(agent runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
	return buildRuntimeAdapter(w.ConfigHome, agent, checkout, nil)
}

func (w jobWorker) outputAdapter(agent runtime.Agent, checkout string, out io.Writer) (workflow.DeliveryAdapter, error) {
	return buildRuntimeAdapter(w.ConfigHome, agent, checkout, subprocess.TeeRunner{Inner: subprocess.GroupRunner{}, Out: runtimeOutputWriter(out)})
}

// buildSeatAwareAdapter builds the job's runtime adapter, injecting the #732 chat
// relay env for a `gitmoot moot` SEAT (payload.MootSeat) when a relay is running.
// For a seat that gets a working relay it mints a per-seat token bound to (agent,
// thread), returns an adapter whose runner appends GITMOOT_CHAT_RELAY[_AUTH] to the
// runtime subprocess env, and — only then — sets agent.ChatSeat so a codex seat is
// elevated to workspace-write+network to reach the socket. It takes *agent so this
// elevation propagates to the agent that RunJob delivers. Elevation is coupled to
// injection on PURPOSE: a seat is never granted the extra codex privilege without a
// working relay to use it (a no-relay / mint-failure seat stays unelevated and
// degrades to a job_result conclusion, exactly like a non-seat). For every non-seat
// job (or when no relay is running) it returns the byte-identical AdapterFactory
// adapter, no elevation, and an empty token.
//
// NOTE: moot seats are dispatched WITHOUT cockpit (runMoot sets no Cockpit flag),
// so the cockpit adapter-rebuild path in run() — which would replace this adapter
// and drop the env runner — never fires for a seat. If that ever changes, thread
// the relay env into the cockpit rebuild too.
func (w jobWorker) buildSeatAwareAdapter(agent *runtime.Agent, checkout string, payload workflow.JobPayload, output ...io.Writer) (workflow.DeliveryAdapter, string, error) {
	if w.RelayServer == nil || !payload.MootSeat || strings.TrimSpace(payload.ThreadID) == "" {
		if len(output) > 0 && output[0] != nil && w.OutputAdapterFactory != nil {
			adapter, err := w.OutputAdapterFactory(*agent, checkout, output[0])
			return adapter, "", err
		}
		adapter, err := w.AdapterFactory(*agent, checkout)
		return adapter, "", err
	}
	token, err := w.RelayServer.RegisterSeat(agent.Name, payload.ThreadID)
	if err != nil {
		// Fail-open: without a token the seat cannot relay, but a normal adapter
		// still lets the seat run (and degrade to a job_result conclusion). Leave the
		// agent UNELEVATED — elevation without a relay buys nothing and would leave a
		// codex seat with write+network it cannot use. Do not fail the job over a mint
		// error.
		writeLine(w.Stdout, "job seat %s relay token mint failed: %v", agent.Name, err)
		adapter, aerr := w.AdapterFactory(*agent, checkout)
		return adapter, "", aerr
	}
	relayEnv := []string{
		chatRelayEnvSocket + "=" + w.RelayServer.SocketPath(),
		chatRelayEnvToken + "=" + token,
	}
	// Elevate ONLY now that the seat will get a working relay env (see the coupling
	// rationale above). Mutates the caller's agent so RunJob delivers with ChatSeat.
	agent.ChatSeat = true
	adapter, err := buildRuntimeAdapter(w.ConfigHome, *agent, checkout, subprocess.EnvInjectingRunner{Env: relayEnv})
	if err != nil {
		agent.ChatSeat = false
		w.RelayServer.ReleaseSeat(token)
		return nil, "", err
	}
	return adapter, token, nil
}

// buildRuntimeAdapter constructs the concrete runtime adapter for a job. With
// credential curation off, a nil runner remains nil and the adapter falls through
// to GroupRunner exactly as before. With curation on, runtimeJobRunner installs
// the curated process-group base beneath any tee, relay, or Landlock wrapper. The
// wrappers still append their environment last and preserve result capture,
// cancellation, and live output.
func buildRuntimeAdapter(home string, agent runtime.Agent, checkout string, runner subprocess.Runner) (workflow.DeliveryAdapter, error) {
	var err error
	runner, err = runtimeJobRunner(home, agent.Runtime, runner)
	if err != nil {
		return nil, err
	}
	gatewayRunner, _ := runner.(*credgw.Runner)
	if (len(agent.WritablePaths) > 0 || len(agent.ReadablePaths) > 0 || len(agent.ReadableFiles) > 0) && (agent.Runtime == runtime.ClaudeRuntime || agent.Runtime == runtime.KimiRuntime) {
		reads, readFiles, writes, env, err := produceRuntimeSandboxGrants(agent.Runtime, agent.ReadablePaths, agent.ReadableFiles, agent.WritablePaths)
		if err != nil {
			return nil, err
		}
		runner = landlockProduceRunner(runner, reads, readFiles, writes, env)
	}
	var adapter runtime.Adapter
	switch agent.Runtime {
	case runtime.CodexRuntime:
		adapter = runtime.CodexAdapter{Dir: checkout, Runner: runner}
	case runtime.ClaudeRuntime:
		adapter = runtime.ClaudeAdapter{Dir: checkout, Runner: runner}
	case runtime.KimiRuntime:
		adapter = runtime.KimiAdapter{Dir: checkout, Runner: runner}
	case runtime.KimiCLIRuntime:
		adapter = runtime.KimiCLIAdapter{Dir: checkout, Runner: runner}
	case runtime.ShellRuntime:
		adapter = runtime.ShellAdapter{Dir: checkout, Runner: runner}
	default:
		return nil, fmt.Errorf("unsupported runtime: %s", agent.Runtime)
	}
	return wrapModelGatewayAdapter(adapter, gatewayRunner), nil
}

func (w jobWorker) defaultStartAdapter(runtimeName string, checkout string) (runtime.Adapter, error) {
	return runtimeAdapterFor(w.ConfigHome, runtimeName, checkout)
}

func (w jobWorker) defaultWorkflow(checkout string) workflow.Engine {
	engine := daemonWorkflowEngine(w.Store, github.NewClient(checkout), checkout, w.workflowHome())
	w.applyOrchestratePolicy(&engine)
	return engine
}

// applyOrchestratePolicy sets the engine's opt-in [orchestrate] fields — the
// artifact-body inlining knobs, the upstream-dep-context injection toggle (#419),
// the per-root delegation token (#338 Part B) and dollar-cost (#380) budgets, the
// result-aware non-progress streak threshold (#339), and the verify→replan
// attempt cap (#439) — from the host policy. It is fail-safe: any load error
// leaves the engine with its defaults (inlining off, upstream-dep injection off,
// both budgets 0 = unlimited, streak threshold and verify cap 0 = engine default)
// rather than failing engine construction.
func (w jobWorker) applyOrchestratePolicy(engine *workflow.Engine) {
	policy, err := w.orchestratePolicy()
	if err != nil {
		return
	}
	engine.InlineArtifactBodies = policy.InlineArtifactBodies
	engine.MaxInlineArtifactBytes = policy.InlineArtifactMaxBytes
	engine.InjectUpstreamDepContext = policy.InjectUpstreamDepContext
	engine.MaxDelegationTokenBudget = policy.MaxDelegationTokenBudget
	engine.MaxDelegationCostUSD = policy.MaxDelegationCostUSD
	engine.MaxDelegationNonProgressStreak = policy.MaxDelegationNonProgressStreak
	engine.MaxVerifyReplanAttempts = policy.MaxVerifyReplanAttempts
	engine.DelegationTimeoutDefaults = workflow.DelegationTimeoutDefaults{
		Default:   policy.DefaultDelegationTimeout,
		Plan:      policy.DefaultPlanTimeout,
		Implement: policy.DefaultImplementTimeout,
		Review:    policy.DefaultReviewTimeout,
		Gate:      policy.DefaultGateTimeout,
		Repair:    policy.DefaultRepairTimeout,
	}
	if notifier, ok := engine.EscalationNotifier.(*daemonEscalationNotifier); ok && notifier != nil {
		notifier.Handle = policy.EscalationHandle
	}
}

// workflowHome resolves the GITMOOT_HOME root used to place per-delegation
// worktrees, mirroring how the daemon resolves paths elsewhere. It returns an
// empty string when resolution fails so the engine falls back to legacy
// shared-checkout dispatch rather than failing the job.
func (w jobWorker) workflowHome() string {
	paths, err := pathsFromFlag(w.ConfigHome)
	if err != nil {
		return ""
	}
	return paths.Home
}

func (w jobWorker) defaultCommenter(_ string) github.Client {
	return github.NewClient("")
}

// The checkout-bound git client backs every per-delegation worktree role; assert
// at compile time so the engine's runtime type-assertions can never silently fall
// back (which would skip read-only-fanout or #332 integration worktrees).
var (
	_ workflow.WorktreeManager            = gitutil.Client{}
	_ workflow.ReadOnlyWorktreeManager    = gitutil.Client{}
	_ workflow.IntegrationWorktreeManager = gitutil.Client{}
	_ workflow.WorktreeCommitter          = gitutil.Client{}
)

// daemonWorkflowEngine builds the per-tick/per-repo workflow.Engine. Its `home`
// param is — by convention (#459) — the already-RESOLVED <home>/.gitmoot root
// (config.Paths.Home), NOT the raw --home. All three callers comply:
// jobWorker.workflowHome() (resolves ConfigHome once), the registered-repo
// supervisor (paths.Home), and local dispatch (paths.Home). The resolved root is
// used verbatim for engine.ArtifactRoot, engine.Home, and daemonEventSink — none
// of which re-resolve — so handing it the raw --home would misplace delegation
// artifacts and the event-sink config probe.
func daemonWorkflowEngine(store *db.Store, gh github.Client, checkout string, home string) workflow.Engine {
	engine := workflow.Engine{
		Store:                   store,
		ProduceCheckDir:         checkout,
		MergeGate:               daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout, Home: home},
		ImplementationFinalizer: daemonImplementationFinalizer{Store: store, GitHub: gh, FallbackCheckout: checkout},
		// escalate_human (#340): @-tag the human on the tree's PR/issue when a leg
		// pauses awaiting a decision. Best-effort and nil-safe in the engine; the
		// handle is filled in from policy by applyOrchestratePolicy.
		EscalationNotifier: &daemonEscalationNotifier{Store: store, GitHub: gh},
		// Off-by-default outbound event stream (#446): the engine emits
		// job.finished/job.failed/job.blocked on its terminal Mailbox path and
		// job.needs_attention on an escalate_human pause through this best-effort,
		// nil-safe sink. daemonEventSink returns nil unless [events].webhook_url is
		// set, so with no config NO sink is constructed and behavior is
		// byte-identical. The sink is a process-global shared singleton (one drain
		// goroutine), so re-building the engine per tick never leaks goroutines.
		EventSink: daemonEventSink(store, home),
		// Off-by-default Mode-A trace-harvester (#465): on a verifiable implement-job
		// outcome (merge merged/blocked, review changes_requested, revert) the engine
		// harvests a synthetic {score, feedback} FeedbackEvent for the job's template
		// version through this best-effort, nil-safe seam. daemonOutcomeHarvester
		// returns nil unless [skillopt].auto_trace_enabled is set, so with no config
		// NO harvester is constructed and behavior — and every human-run
		// TrainingPackage — is byte-identical. The harvester writes ONLY
		// eval/feedback rows; promotion stays 100% manual (the #484 canary wrapper
		// below is the only path that may graduate/roll back, and only when canary
		// mode is configured AND a live canary exists).
		//
		// Off-by-default #484 canary regression window: when [skillopt].auto_promote_canary
		// is configured with a valid sample, the base harvester is wrapped so that AFTER
		// a verifiable outcome it loads the active canary + prior champion auto-trace runs
		// and graduates (-> current) or auto-rolls-back (reusing RevertAgentTemplateVersion
		// to keep the champion live + rejecting the canary) on a material regression.
		// daemonOutcomeHarvesterWithCanary returns the bare base harvester when canary is
		// off and nil when auto_trace is off, so both default paths stay byte-identical.
		OutcomeHarvester: daemonOutcomeHarvesterWithCanary(store, gh, home),
		// Off-by-default cross-family review-agent soft signal (#469): on a MERGE the
		// engine additionally runs a read-only CROSS-FAMILY review leg (off the
		// blocking merge path, best-effort) whose subjective-quality + scope-fidelity
		// rubric is projected into a SECOND, judge-tagged, down-weighted FeedbackEvent
		// in the SAME auto-trace run. daemonReviewLegDispatcher returns nil unless BOTH
		// [skillopt].cross_family_review_enabled AND auto_trace_enabled are set, so with
		// no config NO review leg runs and NO review row is written — byte-identical.
		// A review-leg failure never blocks or fails a job; promotion stays manual.
		ReviewLegDispatcher: daemonReviewLegDispatcher(store, gh, checkout, home),
		// Off-by-default OBJECTIVE deterministic-checker signal (#485): on a MERGE the
		// engine additionally runs a best-effort, DETACHED leg of plain external tools
		// (duplication/lint/complexity) + a pure-Go diff-size metric whose tool-derived
		// [0,1] dimensions are projected into a THIRD, objective-tagged FeedbackEvent in
		// the SAME auto-trace run, distinct from the verifiable floor and the subjective
		// review. daemonDeterministicCheckerDispatcher returns nil unless BOTH
		// [skillopt].deterministic_checkers_enabled AND auto_trace_enabled are set, so
		// with no config NO checker leg runs and NO checker row is written —
		// byte-identical. A missing tool/checkout/timeout SKIPS that dimension and never
		// blocks or fails the merge; promotion stays manual.
		DeterministicCheckerDispatcher: daemonDeterministicCheckerDispatcher(store, gh, checkout, home),
		// Off-by-default deterministic HARD-verifier tier (#474): on a MERGE the engine
		// additionally runs the operator's configured build/test/lint commands in a
		// FRESH sandbox checkout at the merged head (exit 0 == pass), best-effort and
		// DETACHED, and projects the binary pass/fail as the authoritative
		// EvaluatorScore.Hard into the SAME auto-trace run — an un-gameable gate distinct
		// from the verifiable floor, the subjective review, and the objective checker.
		// daemonHardVerifierDispatcher returns nil unless [skillopt].hard_verifiers_enabled
		// AND auto_trace_enabled are set AND at least one command is configured, so with
		// no config NO verifier leg runs and NO hard row is written — byte-identical. A
		// slow suite / unprovisionable sandbox never blocks or fails the merge; promotion
		// stays manual.
		HardVerifierDispatcher: daemonHardVerifierDispatcher(store, checkout, home),
		// Off-by-default agent persistent memory (#626, Phase 1 observation mode):
		// when at least one agent is enrolled ([agents.<name>].memory = true) and the
		// global kill switch is off, the engine's Mailbox injects a "Prior learnings"
		// block into enrolled agents' prompts (READ) and shadow-logs their returned
		// learnings + writes mechanical facts at job terminal (WRITE). daemonMemory-
		// Controller returns nil when nothing is enrolled (or on any config-load
		// error), so with no config NO memory hook is wired and prompt assembly +
		// the terminal path are byte-identical. Non-enrolled agents are never touched
		// even when the controller is present.
		Memory: daemonMemoryController(store, home),
		// Registry default model/effort fallbacks: when a delivered job pins no
		// agent/job override, fall back to the HOME-AWARE resolved runtime registry
		// (built-in defaults overlaid with [runtimes.<name>] config). Fail-open and
		// empty by default, so with no config no model or effort is forced; an
		// agent/job override always wins.
		RuntimeDefaultModel:  runtimeDefaultModelResolver(home),
		RuntimeDefaultEffort: runtimeDefaultEffortResolver(home),
		// Off-restores-byte-identical result-check audit (#526): the deterministic
		// binary-checklist audit of a job's parsed gitmoot_result. resultChecksMode
		// resolves the [workflow] result_checks knob (default warn) from the
		// home-aware config; result_checks = off restores the exact pre-feature
		// terminal path (no event, no payload field, no feed-forward row). Fail-safe
		// to the documented default warn on any load error.
		ResultCheckMode: resultChecksMode(home),
		PayloadRefresher: func(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
			return refreshDaemonJobPayload(ctx, store, checkout, job, payload)
		},
		// Gate the #484 canary ROUTING seam on the SAME policy.CanaryEnabled() the
		// OutcomeHarvester's regression comparator above is gated on, so both seams
		// are consistent: with canary off NO traffic is sampled (Mailbox.routeCanary
		// returns before its query, byte-identical) AND no comparator runs, so a
		// stranded canary row can never keep serving traffic with no auto-rollback.
		CanaryEnabled: canaryRoutingEnabled(home),
		// Off-by-default #530 coordinator routing-context injection: when [router]
		// context_enabled is set, the engine's Mailbox appends a bounded advisory
		// observed-performance table to a top-level coordinator job's prompt.
		// routerContextEnabled returns false with no config (or any load error), so
		// with no config NO telemetry query runs during a job and prompt assembly is
		// byte-identical. Capture (routing_telemetry rows) is always on and additive.
		RouterContextEnabled: routerContextEnabled(home),
	}
	// Opt-in risk-tiered adaptive review (#650): copy the [review] policy onto the
	// engine. Off by default (RiskTiersEnabled false), so the review fan-out is
	// byte-identical unless a home config turns it on.
	applyReviewPolicy(&engine, home)
	wireReviewRiskSignals(&engine, gh)
	if strings.TrimSpace(home) != "" {
		// Root delegation artifacts under GITMOOT_HOME (alongside worktrees)
		// rather than inside the repo checkout, so generated briefs stay out of
		// the tracked tree and are never committed.
		engine.ArtifactRoot = home
	}
	if strings.TrimSpace(home) != "" && strings.TrimSpace(checkout) != "" {
		engine.Home = home
		engine.DelegationCheckout = checkout
		engine.DelegationWorktrees = gitutil.Client{Dir: checkout}
	}
	return engine
}

// daemonEscalationNotifier implements workflow.EscalationNotifier (#340): when a
// delegation tree pauses awaiting a human, it @-tags that human in a GitHub
// comment on the tree's PR (or the issue carrying the coordinator) with the
// resume instructions. Best-effort: any lookup/post failure is returned to the
// engine, which already treats notifier errors as non-fatal (the pause itself is
// durable via the task state + recorded event + dashboard Attention).
type daemonEscalationNotifier struct {
	Store  *db.Store
	GitHub github.Client
	// Handle is the configured escalation_handle (a GitHub login without the @).
	// Empty falls back to the PR author, then the repo owner.
	Handle string
}

func (n *daemonEscalationNotifier) NotifyEscalation(ctx context.Context, request workflow.EscalationRequest) error {
	if n == nil || n.Store == nil || n.GitHub == nil {
		return nil
	}
	repoFull := strings.TrimSpace(request.Repo)
	pull := request.PullRequest
	owner := ""
	// The engine seam leaves PR/repo best-effort; the coordinator job's payload is
	// the source of truth for both, so load it when either is missing.
	if repoFull == "" || pull <= 0 {
		if job, err := n.Store.GetJob(ctx, request.CoordinatorJobID); err == nil {
			if payload, perr := daemonJobPayload(job); perr == nil {
				if repoFull == "" {
					repoFull = strings.TrimSpace(payload.Repo)
				}
				if pull <= 0 {
					pull = payload.PullRequest
				}
			}
		}
	}
	if repoFull == "" || pull <= 0 {
		// No issue/PR to post on; the durable pause (state + event + Attention)
		// still stands. Nothing to notify.
		return nil
	}
	repo, err := daemon.ParseRepository(repoFull)
	if err != nil {
		return err
	}
	owner = repo.Owner

	// Default @-handle: the configured escalation_handle, else the repo owner (the
	// human who owns the tree). The PullRequest type carries no author field, so
	// the owner is the available, always-present human to tag.
	handle := strings.TrimPrefix(strings.TrimSpace(n.Handle), "@")
	if handle == "" {
		handle = owner
	}

	body := buildEscalationComment(handle, request)
	_, err = n.GitHub.PostIssueComment(ctx, repo, int64(pull), body)
	return err
}

// buildEscalationComment renders the @-tag escalation comment body (#340).
//
// The body must never begin a line with "@<handle>" or a bare "/gitmoot": the
// daemon ingests comments on its own PRs, and ParseCommand treats a line whose
// first token is "@<agent>" as a "@<agent> <action>" command — so a leading
// "@<handle> Gitmoot paused…" would make the daemon post a spurious "unsupported
// command action" ack on its own escalation notification. The human is mentioned
// mid-line ("cc @<handle>"), which still notifies them on GitHub but is not
// parsed as a command.
func buildEscalationComment(handle string, request workflow.EscalationRequest) string {
	if request.Ask {
		return buildAskGateComment(handle, request)
	}
	var b strings.Builder
	b.WriteString("Gitmoot paused a delegation tree awaiting your decision (escalate_human).\n")
	if h := strings.TrimPrefix(strings.TrimSpace(handle), "@"); h != "" {
		b.WriteString("cc @" + h + "\n")
	}
	b.WriteString("\n")
	if d := strings.TrimSpace(request.DelegationID); d != "" {
		b.WriteString(fmt.Sprintf("- failing leg: `%s`\n", d))
	}
	if r := strings.TrimSpace(request.Reason); r != "" {
		b.WriteString(fmt.Sprintf("- reason: %s\n", r))
	}
	if q := strings.TrimSpace(request.Question); q != "" {
		b.WriteString(fmt.Sprintf("- question: %s\n", q))
	}
	b.WriteString("\nResume with one of:\n")
	b.WriteString(fmt.Sprintf("- `/gitmoot resume %s retry <instructions>` — re-run the failing leg with your guidance\n", request.CoordinatorJobID))
	b.WriteString(fmt.Sprintf("- `/gitmoot resume %s continue` — proceed the coordinator with what completed\n", request.CoordinatorJobID))
	b.WriteString(fmt.Sprintf("- `/gitmoot resume %s abort` — stop and synthesize a best-effort final result\n", request.CoordinatorJobID))
	return b.String()
}

// buildAskGateComment renders the @-tag comment for a non-failure ask-gate pause
// (#445): a HEALTHY coordinator returned human_questions[] to ask a specific
// decision rather than guess. It quotes each question (id + prompt + choices) and
// gives the `answer` resume verb instead of the failure verbs. Like
// buildEscalationComment it never begins a line with "@<handle>" or "/gitmoot"
// (the human is mentioned mid-line) so the daemon does not parse its own
// notification as a command.
func buildAskGateComment(handle string, request workflow.EscalationRequest) string {
	var b strings.Builder
	b.WriteString("Gitmoot paused a job awaiting your answer to a question (no work failed; the agent chose to ask instead of guess).\n")
	if h := strings.TrimPrefix(strings.TrimSpace(handle), "@"); h != "" {
		b.WriteString("cc @" + h + "\n")
	}
	b.WriteString("\nQuestions:\n")
	if len(request.Questions) > 0 {
		for _, q := range request.Questions {
			line := fmt.Sprintf("- `%s`: %s", strings.TrimSpace(q.ID), strings.TrimSpace(q.Prompt))
			if len(q.Choices) > 0 {
				line += fmt.Sprintf(" (choices: %s)", strings.Join(q.Choices, ", "))
			}
			b.WriteString(line + "\n")
		}
	} else if q := strings.TrimSpace(request.Question); q != "" {
		b.WriteString(q + "\n")
	}
	b.WriteString("\nAnswer with:\n")
	b.WriteString(fmt.Sprintf("- `/gitmoot resume %s answer \"<id>: your answer\"` — one `<id>: ...` line per question\n", request.CoordinatorJobID))
	return b.String()
}

type daemonImplementationFinalizer struct {
	Store            *db.Store
	GitHub           github.Client
	FallbackCheckout string
}

func (f daemonImplementationFinalizer) FinalizeImplementation(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
	if f.Store == nil {
		return workflow.JobPayload{}, errors.New("implementation finalizer store is required")
	}
	if strings.TrimSpace(payload.TaskID) == "" {
		return payload, workflow.BlockedError{Reason: "implemented job has no task id; cannot finalize branch and PR"}
	}
	task, err := f.Store.GetTask(ctx, payload.TaskID)
	if err != nil {
		return payload, fmt.Errorf("load task %s for implementation finalizer: %w", payload.TaskID, err)
	}
	if strings.TrimSpace(task.WorktreePath) == "" {
		return payload, workflow.BlockedError{Reason: "implemented task has no worktree path; rerun through gitmoot task run or gitmoot agent implement"}
	}
	if strings.TrimSpace(task.Branch) == "" {
		return payload, workflow.BlockedError{Reason: "implemented task has no branch; cannot push or open PR"}
	}
	git := gitutil.Client{Dir: task.WorktreePath}
	branch, err := git.CurrentBranch(ctx)
	if err != nil {
		return payload, fmt.Errorf("resolve implementation branch: %w", err)
	}
	if branch != task.Branch {
		return payload, workflow.BlockedError{Reason: fmt.Sprintf("implemented task worktree is on branch %s, not %s", branch, task.Branch)}
	}
	validatedPR, hasValidatedPR, err := f.revalidateImplementationPullRequest(ctx, payload, task)
	if err != nil {
		return payload, err
	}
	// Write-ahead the skip-native-review-fanout flag onto the branch lock as soon
	// as the branch is confirmed — before EVERY downstream path that proceeds with
	// a PR: the no-changes-but-PR-exists early return below, the adopt path, and
	// the fresh EnsurePullRequest create. This closes the #390 TOCTOU: the daemon's
	// PR-watcher (trigger 2) must never observe a PR for this branch with the flag
	// still unpersisted. The branch lock already exists (acquired at job start);
	// SetBranchLockReviewFanout is an idempotent UPDATE keyed by repo+branch and a
	// no-op if the lock is somehow absent. Written only when set, mirroring the
	// engine path's default-fast on the common (false) case; the engine's
	// post-advance write now covers only the non-finalizer path (see engine.go).
	if payload.SkipNativeReviewFanout {
		if err := f.Store.SetBranchLockReviewFanout(ctx, payload.Repo, task.Branch, true); err != nil {
			return payload, fmt.Errorf("persist skip-native-review-fanout before opening PR: %w", err)
		}
	}
	status, err := git.StatusPorcelain(ctx)
	if err != nil {
		return payload, fmt.Errorf("inspect implementation diff: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		head, err := git.HeadSHA(ctx)
		if err != nil {
			return payload, fmt.Errorf("resolve clean implementation head: %w", err)
		}
		if strings.TrimSpace(payload.HeadSHA) == "" || head == payload.HeadSHA {
			if hasValidatedPR {
				return f.adoptValidatedImplementationPullRequest(ctx, payload, task, validatedPR, head)
			}
			if payload.PullRequest > 0 && head == payload.HeadSHA {
				payload.Branch = task.Branch
				return payload, nil
			}
			return payload, workflow.BlockedError{Reason: "implemented job produced no changes in the task worktree"}
		}
	} else {
		message := "Gitmoot implement " + task.ID
		if err := git.CommitAll(ctx, message); err != nil {
			return payload, workflow.BlockedError{Reason: "commit implementation changes failed: " + err.Error()}
		}
	}
	head, err := git.HeadSHA(ctx)
	if err != nil {
		return payload, fmt.Errorf("resolve implementation head after commit: %w", err)
	}
	if err := git.PushBranch(ctx, "origin", task.Branch); err != nil {
		return payload, workflow.BlockedError{Reason: "push implementation branch failed: " + err.Error()}
	}
	if hasValidatedPR {
		return f.adoptValidatedImplementationPullRequest(ctx, payload, task, validatedPR, head)
	}
	repo, err := daemon.ParseRepository(payload.Repo)
	if err != nil {
		return payload, err
	}
	record, err := f.Store.GetRepo(ctx, payload.Repo)
	if err != nil {
		return payload, err
	}
	base := strings.TrimSpace(record.DefaultBranch)
	if base == "" {
		base = "main"
	}
	if existing, ok, err := existingBranchPullRequest(ctx, f.Store, payload.Repo, task.Branch); err != nil {
		return payload, err
	} else if ok {
		payload.PullRequest = int(existing.Number)
		payload.HeadSHA = head
		payload.Branch = task.Branch
		if err := f.Store.UpsertPullRequest(ctx, db.PullRequest{
			RepoFullName: payload.Repo,
			Number:       existing.Number,
			URL:          existing.URL,
			HeadBranch:   task.Branch,
			BaseBranch:   firstNonEmpty(existing.BaseBranch, base),
			HeadSHA:      head,
			State:        firstNonEmpty(existing.State, "open"),
		}); err != nil {
			return payload, err
		}
		return payload, nil
	}
	// No local record yet: ensure the PR on GitHub idempotently. EnsurePullRequest
	// adopts an out-of-band/concurrent open PR for this head (and survives the 422
	// "already exists" create race) instead of erroring, so a benign race no longer
	// blocks the implementation after the work already landed.
	pr, err := f.githubClient(task.WorktreePath).EnsurePullRequest(ctx, github.CreatePullRequestInput{
		Repo:  repo,
		Title: finalizerPullRequestTitle(task),
		Body:  finalizerPullRequestBody(job, payload, task),
		Head:  task.Branch,
		Base:  base,
	})
	if err != nil {
		return payload, workflow.BlockedError{Reason: "open implementation PR failed: " + err.Error()}
	}
	payload.PullRequest = int(pr.Number)
	payload.Branch = task.Branch
	payload.HeadSHA = firstNonEmpty(pr.HeadSHA, head)
	if payload.TaskTitle == "" {
		payload.TaskTitle = task.Title
	}
	if payload.GoalID == "" {
		payload.GoalID = task.GoalID
	}
	if err := f.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: payload.Repo,
		Number:       pr.Number,
		URL:          pr.URL,
		HeadBranch:   firstNonEmpty(pr.HeadRef, task.Branch),
		BaseBranch:   firstNonEmpty(pr.BaseRef, base),
		HeadSHA:      payload.HeadSHA,
		State:        firstNonEmpty(pr.State, "open"),
	}); err != nil {
		return payload, err
	}
	return payload, nil
}

func (f daemonImplementationFinalizer) githubClient(checkout string) github.Client {
	if f.GitHub == nil {
		return github.NewClient(checkout)
	}
	if _, ok := f.GitHub.(*github.GhClient); ok {
		return github.NewClient(checkout)
	}
	return f.GitHub
}

func (f daemonImplementationFinalizer) revalidateImplementationPullRequest(ctx context.Context, payload workflow.JobPayload, task db.Task) (github.PullRequest, bool, error) {
	if !payload.ValidatedPullRequest {
		return github.PullRequest{}, false, nil
	}
	if payload.PullRequest <= 0 {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: "validated implementation payload has no pull request number"}
	}
	repo, err := daemon.ParseRepository(payload.Repo)
	if err != nil {
		return github.PullRequest{}, false, err
	}
	pr, err := f.githubClient(task.WorktreePath).GetPullRequest(ctx, repo, int64(payload.PullRequest))
	if err != nil {
		return github.PullRequest{}, false, fmt.Errorf("revalidate fix-pass pull request #%d: %w", payload.PullRequest, err)
	}
	if pr.Number != int64(payload.PullRequest) {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: fmt.Sprintf("fix-pass pull request revalidation returned #%d, want #%d", pr.Number, payload.PullRequest)}
	}
	if pr.Merged || strings.TrimSpace(pr.MergedAt) != "" || !strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: fmt.Sprintf("fix-pass pull request #%d is no longer open", payload.PullRequest)}
	}
	if strings.TrimSpace(pr.HeadRef) != task.Branch {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: fmt.Sprintf("fix-pass pull request #%d now targets head branch %s, not task branch %s", payload.PullRequest, firstNonEmpty(pr.HeadRef, "<missing>"), task.Branch)}
	}
	if headRepo := strings.TrimSpace(pr.HeadRepoFullName); headRepo != "" && !strings.EqualFold(headRepo, payload.Repo) {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: fmt.Sprintf("fix-pass pull request #%d head belongs to %s, not %s", payload.PullRequest, headRepo, payload.Repo)}
	}
	return pr, true, nil
}

func (f daemonImplementationFinalizer) adoptValidatedImplementationPullRequest(ctx context.Context, payload workflow.JobPayload, task db.Task, pr github.PullRequest, head string) (workflow.JobPayload, error) {
	base := strings.TrimSpace(pr.BaseRef)
	if base == "" {
		record, err := f.Store.GetRepo(ctx, payload.Repo)
		if err != nil {
			return payload, err
		}
		base = firstNonEmpty(strings.TrimSpace(record.DefaultBranch), "main")
	}
	payload.PullRequest = int(pr.Number)
	payload.Branch = task.Branch
	payload.HeadSHA = head
	if err := f.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: payload.Repo,
		Number:       pr.Number,
		URL:          pr.URL,
		HeadBranch:   task.Branch,
		BaseBranch:   base,
		HeadSHA:      head,
		State:        "open",
	}); err != nil {
		return payload, err
	}
	return payload, nil
}

func existingBranchPullRequest(ctx context.Context, store *db.Store, repo string, branch string) (db.PullRequest, bool, error) {
	pr, err := store.GetPullRequestByRepoBranch(ctx, repo, branch)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PullRequest{}, false, nil
	}
	if err != nil {
		return db.PullRequest{}, false, err
	}
	if strings.EqualFold(pr.State, "closed") || strings.EqualFold(pr.State, "merged") {
		return db.PullRequest{}, false, nil
	}
	return pr, true, nil
}

func finalizerPullRequestTitle(task db.Task) string {
	title := strings.TrimSpace(task.Title)
	if title == "" {
		title = task.ID
	}
	return "Gitmoot: " + title
}

func finalizerPullRequestBody(job db.Job, payload workflow.JobPayload, task db.Task) string {
	summary := ""
	if payload.Result != nil {
		summary = strings.TrimSpace(payload.Result.Summary)
	}
	if summary == "" {
		summary = "Implementation completed by " + job.Agent + "."
	}
	body, err := workflow.RenderPullRequestBody(workflow.PullRequestBody{
		TaskID:          task.ID,
		AgentNames:      []string{job.Agent},
		What:            summary,
		Why:             "Gitmoot finalized this implementation from a task worktree.",
		Changes:         []string{"Committed changes from " + task.WorktreePath},
		Results:         finalizerResults(payload),
		Risk:            "Review the generated diff before merging.",
		RawReviewOutput: rawFinalizerOutput(payload),
	})
	if err == nil {
		return body
	}
	return summary
}

func finalizerResults(payload workflow.JobPayload) []string {
	if payload.Result == nil || len(payload.Result.TestsRun) == 0 {
		return []string{"No tests reported by the implementing agent."}
	}
	return append([]string{}, payload.Result.TestsRun...)
}

func rawFinalizerOutput(payload workflow.JobPayload) string {
	if payload.Result != nil && strings.TrimSpace(payload.Result.Summary) != "" {
		return payload.Result.Summary
	}
	if len(payload.RawOutputs) > 0 {
		return payload.RawOutputs[len(payload.RawOutputs)-1]
	}
	return "Implementation completed."
}

type daemonMergeGate struct {
	Store            *db.Store
	GitHub           github.Client
	FallbackCheckout string
	// Home is the resolved <home>/.gitmoot root (or raw --home) used to load the
	// [merge_gate] policy (#596). Empty => the off-by-default merge-gate behavior.
	Home string
}

func (g daemonMergeGate) Evaluate(ctx context.Context, request workflow.MergeRequest) (workflow.MergeDecision, error) {
	if nativeMergeGateDisabled() {
		return workflow.MergeDecision{
			Ready:  false,
			Reason: "native Gitmoot merge gate disabled by GITMOOT_DISABLE_NATIVE_MERGE_GATE; use external gate",
		}, nil
	}
	checkout, err := mergeGateCheckout(ctx, g.Store, request.Repo, g.FallbackCheckout)
	if err != nil {
		return workflow.MergeDecision{}, err
	}
	gate := newDaemonPolicyMergeGate(g.Store, g.githubClient(checkout), checkout)
	applyMergeGatePolicy(&gate, g.Home, request.Repo)
	return gate.Evaluate(ctx, request)
}

func nativeMergeGateDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_DISABLE_NATIVE_MERGE_GATE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (g daemonMergeGate) githubClient(checkout string) github.Client {
	if g.GitHub == nil {
		return github.NewClient(checkout)
	}
	if _, ok := g.GitHub.(*github.GhClient); ok {
		return github.NewClient(checkout)
	}
	return g.GitHub
}

func mergeGateCheckout(ctx context.Context, store *db.Store, repo string, fallback string) (string, error) {
	if store == nil {
		return strings.TrimSpace(fallback), nil
	}
	record, err := store.GetRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	checkout := strings.TrimSpace(record.CheckoutPath)
	if checkout == "" {
		return "", fmt.Errorf("repo %s has no checkout path", repo)
	}
	return checkout, nil
}

func newDaemonPolicyMergeGate(store *db.Store, gh github.Client, checkout string) workflow.PolicyMergeGate {
	return workflow.PolicyMergeGate{
		Store:        store,
		GitHub:       gh,
		Git:          gitutil.Client{Dir: checkout},
		Worktrees:    gitutil.Client{Dir: checkout},
		CheckoutPath: checkout,
		DeleteBranch: true,
	}
}

func refreshDaemonJobPayload(ctx context.Context, store *db.Store, checkout string, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
	if job.Type != "implement" || payload.Result == nil || payload.Result.Decision != "implemented" {
		return payload, nil
	}
	if !payloadHasTaskWorktree(ctx, store, payload) {
		head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
		if err != nil {
			return workflow.JobPayload{}, err
		}
		payload.HeadSHA = head
	}
	if len(payload.Reviewers) == 0 {
		reviewers, err := daemonReviewers(ctx, store, payload.Repo)
		if err != nil {
			return workflow.JobPayload{}, err
		}
		payload.Reviewers = reviewers
	}
	return payload, nil
}

func payloadHasTaskWorktree(ctx context.Context, store *db.Store, payload workflow.JobPayload) bool {
	if store == nil {
		return false
	}
	taskID := strings.TrimSpace(payload.TaskID)
	if taskID == "" {
		return false
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		return false
	}
	return strings.TrimSpace(task.WorktreePath) != ""
}

func daemonReviewers(ctx context.Context, store *db.Store, repo string) ([]string, error) {
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	reviewers := []string{}
	for _, agent := range agents {
		allowed, err := store.AgentCanAccessRepo(ctx, agent.Name, repo)
		if err != nil {
			return nil, err
		}
		if allowed && agentHasCapability(agent.Capabilities, "review") {
			reviewers = append(reviewers, agent.Name)
		}
	}
	return reviewers, nil
}

func agentHasCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

func (w jobWorker) defaultCheckout(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent) (string, error) {
	checkout, err := w.resolveJobCheckout(ctx, job, payload)
	if err != nil {
		return "", err
	}
	switch job.Type {
	case "implement":
		// A worktree-less delegation child (delegation leg, empty WorktreePath)
		// can only resolve the registered shared checkout, which sits on `main`,
		// never its inherited coordinator branch — so validating that checkout
		// against payload.Branch would reject it with "checkout branch is main, not
		// job branch <X>". Skip the branch-identity guard for that child only (the
		// engine's delegation_worktree_skipped fallback runs it against the shared
		// checkout and still holds its branch lock); mirror the #389 ask-arm escape.
		// validateImplementationLock stays UNCONDITIONAL — the branch lock, not this
		// identity guard, is the designed mutation-safety mechanism (#413).
		if !isWorktreeLessDelegationChild(payload) {
			if err := w.validateTargetCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		}
		if err := w.validateImplementationLock(ctx, payload, implementationLockOwner(agent, payload)); err != nil {
			return "", err
		}
	case "review":
		switch {
		case payload.PullRequest > 0 && strings.TrimSpace(payload.TaskID) != "":
			if err := w.validateReviewCheckout(ctx, payload, checkout); err != nil {
				// #684: the PR branch commonly advances between enqueue and execution
				// in an active dev loop, leaving the checkout on a NEWER head than the
				// one the review was pinned to. Re-target the review to the checkout's
				// current head (reviewing the newest commit is what a human reviewer
				// does) when the PR is still open, instead of failing on the mismatch.
				// A closed/merged PR, a dirty tree, or any other checkout error keeps
				// the existing terminal / deferral path.
				if resynced, resyncErr := w.resyncReviewHead(ctx, job, payload, checkout, err); resyncErr != nil {
					return "", resyncErr
				} else if resynced {
					return checkout, nil
				}
				return "", err
			}
		case payload.PullRequest <= 0 && strings.TrimSpace(payload.Branch) == "":
			// A PR-less, branchless review heartbeat (#564: Action="review",
			// PullRequest=0, Branch="") carries no branch identity to validate. Like a
			// PR-less ask it runs read-only against the registered checkout as-is, and
			// the engine's PR-less-review guard treats the delivered review as terminal.
			// Validating it against the empty payload.Branch would reject the registered
			// default-branch checkout ("checkout branch is main, not job branch "),
			// wedging the heartbeat at the worker before the engine ever sees it.
		case !isWorktreeLessDelegationChild(payload):
			// Same worktree-less delegation child escape as the implement arm; a
			// review is read-only ⇒ running it against the shared checkout is
			// trivially safe (#413).
			if err := w.validateTargetCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		}
	case "ask":
		// A PR ask carries BOTH the PR head branch and PullRequest>0, so the
		// registered checkout must be on that branch/head before the agent reads
		// the tree. An issue ask (#389) reuses PullRequest for the *issue number*
		// (PullRequest>0) but carries no branch, so the prior `PullRequest > 0`
		// gate wrongly validated it against the job branch and failed it with
		// "checkout branch is main, not job branch ". Require both a positive
		// PullRequest AND a branch so only a real PR ask is validated; a branchless
		// issue ask — and a branch-only PR-less CLI ask — run against the
		// registered checkout as-is.
		if payload.PullRequest > 0 && strings.TrimSpace(payload.Branch) != "" {
			if err := w.validateTargetCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		}
	}
	return checkout, nil
}

func (w jobWorker) resolveJobCheckout(ctx context.Context, job db.Job, payload workflow.JobPayload) (string, error) {
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil {
		return "", err
	}
	repo, err := daemon.ParseRepository(payload.Repo)
	if err != nil {
		return "", err
	}
	checkout, err := w.healRegisteredRepoCheckout(ctx, job, repo, repoRecord)
	if err != nil {
		return "", err
	}
	if err := preflightDaemonRepoCheckout(ctx, repo, checkout); err != nil {
		return "", err
	}
	taskCheckout, ok, err := w.taskWorktreeCheckout(ctx, payload)
	if err != nil {
		return "", err
	}
	if ok {
		checkout = taskCheckout
		if err := preflightDaemonRepoCheckout(ctx, repo, checkout); err != nil {
			return "", err
		}
	}
	return checkout, nil
}

func (w jobWorker) healRegisteredRepoCheckout(ctx context.Context, job db.Job, repo github.Repository, record db.Repo) (string, error) {
	checkout := strings.TrimSpace(record.CheckoutPath)
	resolved, healed, err := resolveRegisteredRepoRecord(ctx, w.Store, repo, record)
	if err != nil {
		return "", err
	}
	healedPath := strings.TrimSpace(resolved.CheckoutPath)
	if !healed {
		return healedPath, nil
	}
	message := repoCheckoutHealMessage(repo.FullName(), checkout, healedPath)
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "repo_checkout_self_healed", Message: message}); err != nil {
		return "", err
	}
	if w.Stdout != nil {
		writeLine(w.Stdout, "WARN: %s", message)
	}
	return healedPath, nil
}

func sameCheckoutPath(a, b string) bool {
	return filepath.Clean(strings.TrimSpace(a)) == filepath.Clean(strings.TrimSpace(b))
}

func (w jobWorker) taskWorktreeCheckout(ctx context.Context, payload workflow.JobPayload) (string, bool, error) {
	// Delegated jobs carry their own per-delegation worktree path in the payload
	// (an implement child's branch worktree, or a read-only fan-out child's
	// detached worktree); prefer it over the task-table worktree so the child runs
	// in its isolated checkout.
	if delegationPath := strings.TrimSpace(payload.WorktreePath); delegationPath != "" {
		checkout, err := normalizeTaskWorktreePath(delegationPath)
		if err != nil {
			return "", false, err
		}
		return checkout, checkout != "", nil
	}
	if strings.TrimSpace(payload.TaskID) == "" {
		return "", false, nil
	}
	task, err := w.Store.GetTask(ctx, payload.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(task.RepoFullName) != "" && task.RepoFullName != payload.Repo {
		return "", false, fmt.Errorf("task %s belongs to repo %s, not %s", payload.TaskID, task.RepoFullName, payload.Repo)
	}
	if strings.TrimSpace(task.Branch) != "" && task.Branch != payload.Branch {
		return "", false, fmt.Errorf("task %s branch is %s, not job branch %s", payload.TaskID, task.Branch, payload.Branch)
	}
	checkout := strings.TrimSpace(task.WorktreePath)
	if checkout == "" {
		return "", false, nil
	}
	checkout, err = normalizeTaskWorktreePath(checkout)
	if err != nil {
		return "", false, err
	}
	return checkout, true, nil
}

func normalizeTaskWorktreePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("normalize task worktree path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func (w jobWorker) validateTargetCheckout(ctx context.Context, payload workflow.JobPayload, checkout string) error {
	git := gitutil.Client{Dir: checkout}
	// A delegation worktree child runs in a gitmoot-managed worktree. An implement
	// child is on its delegation branch (created off the parent base, whose tip may
	// have advanced past the inherited HeadSHA — so its HeadSHA check is skipped),
	// while a read-only child uses a *detached* worktree with no branch at all (so
	// CurrentBranch errors). Validate the branch when the worktree has one (the
	// implement guard, preserved) and skip it for a detached read-only worktree;
	// both still require the freshly allocated worktree to be clean.
	if isDelegationWorktreeChild(payload) {
		if branch, err := git.CurrentBranch(ctx); err == nil && branch != payload.Branch {
			return fmt.Errorf("checkout branch is %s, not job branch %s", branch, payload.Branch)
		}
		clean, err := git.WorktreeClean(ctx)
		if err != nil {
			return err
		}
		if !clean {
			return fmt.Errorf("checkout %s has uncommitted changes", checkout)
		}
		return nil
	}
	branch, err := git.CurrentBranch(ctx)
	if err != nil {
		return err
	}
	if branch != payload.Branch {
		return fmt.Errorf("checkout branch is %s, not job branch %s", branch, payload.Branch)
	}
	clean, err := git.WorktreeClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("checkout %s has uncommitted changes", checkout)
	}
	expectedHead := strings.TrimSpace(payload.HeadSHA)
	if expectedHead == "" {
		// A delegation child can inherit an empty HeadSHA from a coordinator that
		// has no PR context (a local `gitmoot orchestrate`). It is gitmoot-dispatched
		// against the registered checkout, so run it against the current HEAD rather
		// than failing — e.g. a decompose-and-verify verify step, or any read-only
		// follow-up delegation. Implement children always run in a per-delegation
		// worktree (handled above), so only non-mutating shared-checkout delegation
		// children reach here. Non-delegation jobs (PR comments) still require a
		// HeadSHA.
		if strings.TrimSpace(payload.DelegationID) != "" {
			return nil
		}
		return fmt.Errorf("job for %s has no head SHA", payload.Branch)
	}
	head, err := git.HeadSHA(ctx)
	if err != nil {
		return err
	}
	if head != expectedHead {
		return fmt.Errorf("checkout head is %s, not job head %s", head, expectedHead)
	}
	return nil
}

// isDelegationWorktreeChild reports whether the job is a delegated child running
// in its own per-delegation worktree (it carries both a delegation id and an
// allocated worktree path). Such children are validated against their isolated
// worktree HEAD rather than the inherited parent HeadSHA.
func isDelegationWorktreeChild(payload workflow.JobPayload) bool {
	return strings.TrimSpace(payload.DelegationID) != "" && strings.TrimSpace(payload.WorktreePath) != ""
}

// isWorktreeLessDelegationChild is the exact complement of
// isDelegationWorktreeChild: a delegation child (it carries a delegation id — a
// delegation *leg*, NOT just a ParentJobID, which continuations also carry with
// an empty DelegationID and which route through the `ask` arm) that has no
// allocated worktree path. With no worktree it can only resolve the repo's
// registered shared checkout, which sits on `main` — never the inherited
// coordinator branch (delegationRequest sets Branch: payload.Branch) — so
// validating that checkout against payload.Branch would reject it with
// "checkout branch is main, not job branch <X>". A wrong-branch *task* worktree
// is already rejected upstream at taskWorktreeCheckout, so WorktreePath == ""
// cleanly means "the shared registered checkout is the only resolution."
// defaultCheckout's implement/review arms skip the validateTargetCheckout branch
// guard for such a child (the branch lock, not this identity guard, is the
// designed mutation-safety mechanism); see #389 (the ask-arm precedent) and #413.
func isWorktreeLessDelegationChild(payload workflow.JobPayload) bool {
	return strings.TrimSpace(payload.DelegationID) != "" && strings.TrimSpace(payload.WorktreePath) == ""
}

func (w jobWorker) validateReviewCheckout(ctx context.Context, payload workflow.JobPayload, checkout string) error {
	git := gitutil.Client{Dir: checkout}
	clean, err := git.WorktreeClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("checkout %s has uncommitted changes", checkout)
	}
	expectedHead := strings.TrimSpace(payload.HeadSHA)
	if expectedHead == "" {
		return fmt.Errorf("review job for PR #%d has no head SHA", payload.PullRequest)
	}
	head, err := git.HeadSHA(ctx)
	if err != nil {
		return err
	}
	if head != expectedHead {
		return fmt.Errorf("checkout head is %s, not review job head %s", head, expectedHead)
	}
	return nil
}

// isReviewHeadMismatch reports whether a checkout pre-flight error is specifically
// the review head-SHA drift emitted by validateReviewCheckout ("checkout head is
// X, not review job head Y") — NOT a dirty tree, a missing head, or a branch
// mismatch. Only that one condition is eligible for the #684 re-sync; every other
// checkout error keeps its existing terminal / deferral path.
func isReviewHeadMismatch(cause error) bool {
	if cause == nil {
		return false
	}
	return strings.Contains(cause.Error(), "not review job head")
}

// reviewPullRequestOpen reports whether the review's PR is KNOWN to be open, using
// the locally-tracked pull_requests record (the daemon's PR-watcher upserts an
// open record for every PR it watches before it fans out review jobs, so a genuine
// #684 review of an active PR has one). Re-sync is gated on a definitively-open PR:
//
//   - record found + state open (or any non-closed/-merged state) ⇒ open (re-sync).
//   - record found + state closed/merged ⇒ NOT open (a stale review of a dead PR
//     must not silently pass; keep the existing terminal path).
//   - NO record (sql.ErrNoRows) ⇒ NOT open. The store has no evidence the PR is
//     live, so it falls through to the existing #532 checkout-contention deferral
//     rather than re-targeting to a possibly-unrelated checkout head.
//   - a real DB error ⇒ surfaced; the caller declines to re-sync.
func (w jobWorker) reviewPullRequestOpen(ctx context.Context, repo string, number int) (bool, error) {
	pr, err := w.Store.GetPullRequest(ctx, repo, int64(number))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state := strings.ToLower(strings.TrimSpace(pr.State))
	return state != "closed" && state != "merged", nil
}

// resyncReviewHead handles #684 head-SHA drift for a PR review job. A review is
// pinned to the PR head SHA at enqueue; in an active dev loop the branch often
// advances (a newer commit is pushed) before the queued review runs, so the
// registered checkout sits on a NEWER head than the one the review was pinned to.
// validateReviewCheckout then rejects it with "checkout head is <new>, not review
// job head <old>", and the job ultimately fails — even though reviewing the
// checkout's current head is strictly more useful (it is exactly what a human
// reviewer does). resyncReviewHead re-targets the review to the checkout's current
// head instead of failing, but ONLY when:
//
//   - the validation failure was specifically the review head-SHA mismatch (a
//     dirty tree, a missing head, or a branch mismatch is left untouched), and
//   - the PR is still OPEN (a closed/merged PR keeps the existing terminal path so
//     a stale review of a dead PR does not silently pass).
//
// On a re-sync it persists the current head onto the job payload (RunJob re-reads
// the payload from the store, so the delivered review prompt and the posted PR
// comment carry the new head) and records a review_head_resynced event, then
// returns true so defaultCheckout proceeds with the review. Every declined case
// returns false so the caller's existing error path runs byte-identically.
func (w jobWorker) resyncReviewHead(ctx context.Context, job db.Job, payload workflow.JobPayload, checkout string, cause error) (bool, error) {
	if !isReviewHeadMismatch(cause) {
		return false, nil
	}
	if payload.PullRequest <= 0 {
		return false, nil
	}
	open, err := w.reviewPullRequestOpen(ctx, payload.Repo, payload.PullRequest)
	if err != nil {
		// Undeterminable PR state (a DB read error) ⇒ do not re-sync; fall through to
		// the existing deferral/terminal path rather than reviewing a possibly-dead PR.
		return false, nil
	}
	if !open {
		// A closed/merged PR, or one the store has no record of, keeps the existing
		// #532 deferral / terminal path — only a definitively-open PR is re-synced.
		return false, nil
	}
	// Confirm the resolved checkout is actually on the PR's head branch before
	// re-targeting. A review that falls back to the registered shared checkout (which
	// sits on `main`, not the PR branch) must NOT be re-synced to main's head — that
	// would review the wrong tree and could post an approval against a SHA that is not
	// the PR head. We only decline when we can POSITIVELY confirm the branch differs:
	// a detached-HEAD worktree (CurrentBranch errors) is a legitimate #684 target and
	// is left to proceed. We deliberately do NOT gate on head == pr.HeadSHA because the
	// PR-watcher can lag the push, which is exactly the drift #684 exists to tolerate.
	if b, err := (gitutil.Client{Dir: checkout}).CurrentBranch(ctx); err == nil &&
		strings.TrimSpace(b) != strings.TrimSpace(payload.Branch) {
		return false, nil
	}
	head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		return false, err
	}
	head = strings.TrimSpace(head)
	previous := strings.TrimSpace(payload.HeadSHA)
	if head == "" || head == previous {
		// Nothing to re-target to (empty or already-current head); let the caller's
		// existing path handle it.
		return false, nil
	}
	payload.HeadSHA = head
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		return false, err
	}
	if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{
		JobID: job.ID,
		Kind:  "review_head_resynced",
		Message: fmt.Sprintf("PR #%d branch advanced from %s to %s before the review ran; re-targeting the review to the current head",
			payload.PullRequest, previous, head),
	}); eventErr != nil {
		return false, eventErr
	}
	writeLine(w.Stdout, "job %s review head re-synced %s -> %s (PR #%d advanced)", job.ID, previous, head, payload.PullRequest)
	return true, nil
}

func implementationLockOwner(agent runtime.Agent, payload workflow.JobPayload) string {
	if payload.DelegationReason == "runtime_session_busy" && payload.DelegatedAgent == agent.Name && strings.TrimSpace(payload.OriginalAgent) != "" {
		return payload.OriginalAgent
	}
	return agent.Name
}

func (w jobWorker) validateImplementationLock(ctx context.Context, payload workflow.JobPayload, owner string) error {
	lock, err := w.Store.GetBranchLock(ctx, payload.Repo, payload.Branch)
	if err != nil {
		return err
	}
	if lock.Owner != owner {
		return fmt.Errorf("branch %s is locked by %s, not %s", payload.Branch, lock.Owner, owner)
	}
	return nil
}

func (w jobWorker) finishQueuedJob(ctx context.Context, jobID string, state workflow.JobState, cause error) error {
	transitioned, err := w.Store.TransitionJobStateWithEvent(ctx, jobID, string(workflow.JobQueued), string(state), db.JobEvent{
		JobID:   jobID,
		Kind:    string(state),
		Message: cause.Error(),
	})
	if err != nil {
		return err
	}
	if transitioned {
		writeLine(w.Stdout, "job %s %s: %v", jobID, state, cause)
		// Best-effort outbound emit (#446) for a DAEMON-owned pre-flight terminal
		// transition (queued->failed|blocked) — this never reaches the engine's
		// Mailbox.finishWithPayload chokepoint, so the daemon owns its emit. Gated
		// on transitioned==true so it fires exactly once per genuine transition;
		// nil-safe when [events] is OFF. The subsequent finalizePreflightDelegationChild
		// only attaches a synthetic result via savePayload (no further transition),
		// so it does not double-emit.
		if eventType, ok := daemonTerminalEventType(state); ok {
			emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, jobID, eventType, string(state), cause.Error())
		}
	}
	// A delegation child that fails in ANY pre-flight step (checkout/branch-lock
	// validation, adapter factory, managed config, runtime-session lock/busy,
	// ephemeral bring-up, delegated dispatch) is closed here straight from
	// JobQueued — it never reached queued→running, so handleRunJobError's
	// ParentJobID finalize branch (which fires only for non-queued children) is
	// bypassed and the child strands `failed` with Result == nil. Without this,
	// advanceDelegations never runs for the child and its failure_policy
	// (escalate_human / block_parent / continue / escalate) never fires (#409).
	//
	// finishQueuedJob is the single choke point all ~12 direct
	// finishQueuedJob(JobFailed) sites (and handleRunJobError's JobQueued branch)
	// funnel through, so finalizing here covers every pre-flight failure exactly
	// once. Gate on a genuine queued→(failed|blocked) transition + a delegation
	// child with no stored result, so non-delegation jobs (PR/issue asks) are
	// byte-identical and an already-terminal/cancelled child is never
	// force-finalized.
	//
	// JobBlocked is included alongside JobFailed: a queued delegation child that
	// fails an executor pre-flight check returning a BlockedError (a same-branch
	// sibling branch-lock conflict, an empty implement branch, a missing
	// action/repo capability, an unsubscribed agent — all from
	// ensureJobExecutorAllowed/e.block) is routed by handleRunJobError to
	// finishQueuedJob(..., JobBlocked, ...). Both failed and blocked are genuine
	// terminal failures the engine already finalizes (FinalizeTimedOutDelegationChild
	// accepts JobRunning/JobFailed/JobBlocked), so both must advance the parent DAG
	// or the blocked class strands the parent — the exact #409 bug. JobCancelled is
	// deliberately excluded: the engine switch rejects it and a cancelled child
	// must follow the cancelled path.
	if transitioned && (state == workflow.JobFailed || state == workflow.JobBlocked) {
		return w.finalizePreflightDelegationChild(ctx, jobID, cause)
	}
	return nil
}

// finalizePreflightDelegationChild drives the parent delegation DAG for a child
// that was just transitioned queued→failed in a daemon pre-flight step, so the
// delegation's failure_policy fires exactly as it would for a runtime failure.
// It is a no-op for a non-delegation job or a child that already stored a result
// (finalizeTimedOutDelegationChild / Engine.FinalizeTimedOutDelegationChild are
// idempotent), so a re-run (retry / stale-running recovery) re-enters cleanly. It
// mirrors handleRunJobError (~4169-4189): an AwaitingHumanError (escalate_human
// paused the tree awaiting a human, #340) and a BlockedError (block_parent blocked
// the shared parent task) are EXPECTED terminal outcomes of advancing the DAG, not
// errors to propagate.
func (w jobWorker) finalizePreflightDelegationChild(ctx context.Context, jobID string, cause error) error {
	job, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	payload, payloadErr := daemonJobPayload(job)
	if payloadErr != nil || strings.TrimSpace(payload.ParentJobID) == "" || payload.Result != nil {
		return nil
	}
	if _, finalizeErr := w.finalizeTimedOutDelegationChild(ctx, job, cause); finalizeErr != nil {
		var awaiting workflow.AwaitingHumanError
		if errors.As(finalizeErr, &awaiting) {
			return nil
		}
		var blocked workflow.BlockedError
		if errors.As(finalizeErr, &blocked) {
			return nil
		}
		return finalizeErr
	}
	return nil
}

func (w jobWorker) handleRunJobError(ctx context.Context, jobID string, cause error) error {
	latest, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if latest.Type == "implement" && runtimePermissionFailure(cause) {
		payload, payloadErr := daemonJobPayload(latest)
		if payloadErr != nil || payload.Result == nil {
			transitioned, err := markJobPermissionBlocked(ctx, w.Store, jobID)
			if err != nil {
				return err
			}
			if transitioned {
				if err := blockTaskForPermissionBlockedJob(ctx, w.Store, latest); err != nil {
					return err
				}
				// Best-effort outbound emit (#446): this running->blocked permission
				// transition is daemon-owned (it does not pass through the engine's
				// Mailbox.finishWithPayload chokepoint), so emit job.blocked exactly
				// once here. The following finalizePreflightDelegationChild only attaches
				// a synthetic result (savePayload, no transition), so it never re-emits.
				emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, jobID, events.EventJobBlocked, string(workflow.JobBlocked), agentPermissionBlockedMessage)
				// A WRITABLE implement DELEGATION child whose runtime fails MID-RUN
				// with a permission error (read-only FS / sandbox denies write) is
				// transitioned JobRunning->JobBlocked here and returns early — it never
				// reaches the ParentJobID finalize branch below, so the parent DAG
				// strands exactly like #409 (the mid-run sibling of the pre-flight
				// read-only-implement case fixed at ~2127). Route it through the SAME
				// finalize helper so its failure_policy fires. The helper no-ops for a
				// non-delegation job (ParentJobID empty) or one that already stored a
				// result, so the solo-implement case stays byte-identical.
				if err := w.finalizePreflightDelegationChild(ctx, jobID, errors.New(agentPermissionBlockedMessage)); err != nil {
					return err
				}
				return nil
			}
		}
	}
	if latest.State == string(workflow.JobQueued) {
		state := workflow.JobFailed
		var blocked workflow.BlockedError
		if errors.As(cause, &blocked) {
			state = workflow.JobBlocked
		}
		return w.finishQueuedJob(ctx, jobID, state, cause)
	}
	if latest.State == string(workflow.JobCancelled) {
		_, err := workflow.SettleCancelledRunningJob(ctx, w.Store, latest.ID, "cancelled job worker settled")
		return err
	}
	payload, payloadErr := daemonJobPayload(latest)
	if payloadErr == nil && payload.Result != nil {
		var awaiting workflow.AwaitingHumanError
		if errors.As(cause, &awaiting) {
			// escalate_human (#340): the parent tree paused durably awaiting a human;
			// the child delivered a result and the pause (task state + event +
			// notification) is already recorded. Treat this as the expected terminal
			// outcome, not a failure to propagate.
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: latest.ID, Kind: "advance_awaiting_human", Message: cause.Error()})
		}
		var blocked workflow.BlockedError
		if errors.As(cause, &blocked) {
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: latest.ID, Kind: "advance_blocked", Message: cause.Error()})
		}
		if err := w.recordPostDeliveryWorkflowError(ctx, latest, cause); err != nil {
			return err
		}
		return nil
	}
	if payloadErr == nil && strings.TrimSpace(payload.ParentJobID) != "" {
		// A delegation child killed by its per-delegation timeout (or any runtime
		// failure that yields no parseable gitmoot_result) lands here still
		// JobRunning, or JobFailed/JobBlocked WITHOUT a stored result: Mailbox.Run
		// errored, so RunJob returned before AdvanceJob and the parent's
		// advanceDelegations never ran. Finalize it as a terminal failed child and
		// drive the parent DAG so the delegation's retry/failure_policy/continuation
		// actually fire instead of the child stranding until the 30m stale-running
		// recovery blindly re-queues it.
		finalized, finalizeErr := w.finalizeTimedOutDelegationChild(ctx, latest, cause)
		if finalizeErr != nil {
			var awaiting workflow.AwaitingHumanError
			if errors.As(finalizeErr, &awaiting) {
				// escalate_human failure_policy paused the shared parent task awaiting
				// a human (#340); the child is finalized and the DAG advanced, so this
				// is the expected durable-pause outcome, not an error to propagate.
				return nil
			}
			var blocked workflow.BlockedError
			if errors.As(finalizeErr, &blocked) {
				// block_parent failure_policy blocked the shared parent task; the
				// child is finalized and the DAG advanced, so this is the expected
				// terminal outcome rather than an error to propagate.
				return nil
			}
			return finalizeErr
		}
		if finalized {
			return nil
		}
	}
	if latest.State == string(workflow.JobFailed) || latest.State == string(workflow.JobBlocked) {
		return nil
	}
	return cause
}

// finalizeTimedOutDelegationChild bridges the daemon run-error path into the
// engine's delegation DAG: it converts a timed-out/runtime-failed delegation
// child with no stored result into a terminal failed child and advances the
// parent. Advancing the parent can trigger a retry of an *implement* delegation,
// which must allocate a fresh per-delegation worktree; so it resolves the repo's
// main checkout and builds a fully-wired engine instead of a checkout-less one.
// A missing checkout degrades gracefully (the engine emits
// delegation_worktree_skipped and falls back to a shared-checkout branch lock).
// Returns whether the child was finalized.
func (w jobWorker) finalizeTimedOutDelegationChild(ctx context.Context, job db.Job, cause error) (bool, error) {
	reason := fmt.Sprintf("delegation child %s ended without a result: %v", job.ID, cause)
	engine := w.WorkflowFactory(w.delegationParentCheckout(ctx, job))
	return engine.FinalizeTimedOutDelegationChild(ctx, job.ID, reason)
}

// delegationParentCheckout returns the repo's main registered checkout for a
// delegation child job (NOT the child's own worktree), so the engine can
// `git worktree add` a retry's per-delegation worktree against it. It returns
// "" on any lookup failure, leaving the engine to advance the DAG without
// worktree isolation rather than blocking finalization.
func (w jobWorker) delegationParentCheckout(ctx context.Context, job db.Job) string {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return ""
	}
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(repoRecord.CheckoutPath)
}

func (w jobWorker) recordPostDeliveryWorkflowError(ctx context.Context, job db.Job, cause error) error {
	return w.recordAdvanceRetryOnce(ctx, job.ID,
		"post-delivery workflow error; advancement will retry from stored result: "+cause.Error())
}

func (w jobWorker) postJobResultComment(ctx context.Context, jobID string, agent runtime.Agent, _ string, cause error) error {
	job, payload, err := daemonWorkerJobPayload(ctx, w.Store, jobID)
	if err != nil {
		return err
	}
	// Chat back-link (#534): a chat-promoted (or ask-gate auto-linked) job posts
	// its result into the originating thread at the SAME terminal call sites as
	// the PR comment. It runs BEFORE the PR-scope guard below because a chat job
	// often has no PR; it is best-effort (a chat failure never fails the worker)
	// and idempotent (a chat_result_posted job event, mirroring comment_posted).
	// Gated on payload.ThreadID, so a non-chat job is byte-identical. We pass the
	// already-fetched (job, payload) so it does not re-read + re-parse the payload.
	_ = w.postChatThreadResult(ctx, job, payload, agent, cause)
	if job.State == string(workflow.JobCancelled) {
		return nil
	}
	if payload.PullRequest <= 0 || strings.TrimSpace(payload.Repo) == "" {
		return nil
	}
	if w.CommenterFactory == nil {
		return nil
	}
	posted, err := w.jobResultCommentPosted(ctx, jobID)
	if err != nil {
		return err
	}
	if posted {
		return nil
	}
	repo, err := daemon.ParseRepository(payload.Repo)
	if err != nil {
		return err
	}
	diagnostic := jobResultDiagnostic(cause)
	if diagnostic == "" && payload.Result == nil {
		diagnostic = w.storedJobFailureDiagnostic(ctx, job)
	}
	body := workflow.RenderJobResultComment(workflow.JobResultComment{
		AgentName:  firstNonEmpty(agent.Name, job.Agent),
		Runtime:    agent.Runtime,
		JobID:      job.ID,
		JobState:   job.State,
		Payload:    payload,
		Result:     payload.Result,
		Diagnostic: diagnostic,
	})
	if _, err := w.CommenterFactory("").PostIssueComment(ctx, repo, int64(payload.PullRequest), body); err != nil {
		_ = w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "comment_post_failed", Message: err.Error()})
		return nil
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "comment_posted", Message: "posted attributed PR result comment"})
}

// postChatThreadResult appends a compact, attributed job-result message into the
// chat thread a chat-promoted (or ask-gate auto-linked) job carries on its
// payload (#534). It reuses workflow.RenderJobResultComment for the body, authors
// the message as the agent with kind='job_result' (structurally non-promotable),
// links it back to the promoting message via reply_to, and attaches an
// origin-qualified job ref. It is idempotent via a chat_result_posted job event
// that mirrors the comment_posted bookkeeping EXACTLY (a retry_queued clears it),
// so a retried/re-advanced job posts at most once. Every step is best-effort: a
// chat write failure is recorded and swallowed, never failing the worker.
//
// It takes the already-fetched (job, payload) from the caller so the terminal
// path parses the payload once, not twice.
func (w jobWorker) postChatThreadResult(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, cause error) error {
	// A job PAUSING at awaiting_human is NOT terminal: it returned an
	// AwaitingHumanError and its answer-driven *continuation* (a separate job that
	// inherits ThreadID) posts the real result once the human answers. Posting here
	// would drop a misleading, out-of-order "job result" into the answer thread
	// BEFORE the human has even answered — corrupting the keystone answer channel.
	var awaiting workflow.AwaitingHumanError
	if errors.As(cause, &awaiting) {
		return nil
	}
	threadID := strings.TrimSpace(payload.ThreadID)
	if threadID == "" {
		return nil
	}
	if job.State == string(workflow.JobCancelled) {
		return nil
	}
	posted, err := w.chatThreadResultPosted(ctx, job.ID)
	if err != nil {
		return err
	}
	if posted {
		return nil
	}
	diagnostic := jobResultDiagnostic(cause)
	if diagnostic == "" && payload.Result == nil {
		diagnostic = w.storedJobFailureDiagnostic(ctx, job)
	}
	agentName := firstNonEmpty(agent.Name, job.Agent)
	body := workflow.RenderJobResultComment(workflow.JobResultComment{
		AgentName:  agentName,
		Runtime:    agent.Runtime,
		JobID:      job.ID,
		JobState:   job.State,
		Payload:    payload,
		Result:     payload.Result,
		Diagnostic: diagnostic,
	})
	if _, err := w.Store.AddChatMessage(ctx, db.ChatMessage{
		ThreadID:   threadID,
		AuthorKind: db.ChatAuthorKindAgent,
		AuthorName: agentName,
		Kind:       db.ChatKindJobResult,
		Body:       body,
		ReplyTo:    strings.TrimSpace(payload.ChatMessageID),
		Refs:       []db.ChatRef{{Kind: "job", Repo: payload.Repo, ID: job.ID}},
	}); err != nil {
		_ = w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "chat_result_post_failed", Message: err.Error()})
		return nil
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "chat_result_posted", Message: "posted job result into chat thread " + threadID})
}

func (w jobWorker) chatThreadResultPosted(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	posted := false
	for _, event := range events {
		switch event.Kind {
		case "retry_queued":
			posted = false
		case "chat_result_posted":
			posted = true
		}
	}
	return posted, nil
}

func (w jobWorker) jobResultCommentPosted(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	posted := false
	for _, event := range events {
		switch event.Kind {
		case "retry_queued":
			posted = false
		case "comment_posted":
			posted = true
		}
	}
	return posted, nil
}

func (w jobWorker) jobNeedsCommentRetry(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	needsRetry := false
	for _, event := range events {
		switch event.Kind {
		case "comment_post_failed":
			needsRetry = true
		case "comment_posted":
			needsRetry = false
		case "retry_queued":
			needsRetry = false
		}
	}
	return needsRetry, nil
}

func (w jobWorker) storedJobFailureDiagnostic(ctx context.Context, job db.Job) string {
	if job.State != string(workflow.JobFailed) && job.State != string(workflow.JobBlocked) {
		return ""
	}
	events, err := w.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Kind == job.State && strings.TrimSpace(event.Message) != "" {
			return event.Message
		}
	}
	return ""
}

func daemonWorkerJobPayload(ctx context.Context, store *db.Store, jobID string) (db.Job, workflow.JobPayload, error) {
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, workflow.JobPayload{}, err
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return db.Job{}, workflow.JobPayload{}, err
	}
	return job, payload, nil
}

func jobResultDiagnostic(cause error) string {
	if cause == nil {
		return ""
	}
	return cause.Error()
}

func daemonJobPayload(job db.Job) (workflow.JobPayload, error) {
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return workflow.JobPayload{}, fmt.Errorf("parse job payload %q: %w", job.ID, err)
	}
	return payload, nil
}

func resolveDaemonCheckout(ctx context.Context, repo github.Repository, client gitutil.Client) (string, error) {
	record, err := repoRecordForCheckout(ctx, repo, client)
	if err != nil {
		return "", err
	}
	return record.CheckoutPath, nil
}

// resolveDaemonStartRepo resolves the repo record that `daemon start/run --repo
// owner/repo` should run against. When the repo is already registered with a
// checkout path, it validates that checkout and self-heals through its recorded
// primary when necessary, so the command works from any working directory
// (#202/#959). When the repo is not yet registered, it bootstraps from workDir;
// an implicit linked checkout is pinned to its primary.
func resolveDaemonStartRepo(ctx context.Context, store *db.Store, repo github.Repository, workDir string) (db.Repo, error) {
	return resolveRepoRecord(ctx, store, repo, workDir)
}

func repoRecordForCheckout(ctx context.Context, repo github.Repository, client gitutil.Client) (db.Repo, error) {
	root, err := client.Root(ctx)
	if err != nil {
		return db.Repo{}, fmt.Errorf("resolve repo checkout: %w", err)
	}
	remote, err := client.OriginRemote(ctx)
	if err != nil {
		return db.Repo{}, fmt.Errorf("resolve repo checkout remote: %w", err)
	}
	remoteRepo, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return db.Repo{}, err
	}
	if remoteRepo.String() != repo.FullName() {
		return db.Repo{}, fmt.Errorf("current checkout origin is %s, not %s", remoteRepo.String(), repo.FullName())
	}
	defaultBranch := ""
	if branch, err := client.CurrentBranch(ctx); err == nil {
		defaultBranch = branch
	}
	return db.Repo{
		Owner:         repo.Owner,
		Name:          repo.Name,
		DefaultBranch: defaultBranch,
		RemoteURL:     remote,
		CheckoutPath:  root,
	}, nil
}
