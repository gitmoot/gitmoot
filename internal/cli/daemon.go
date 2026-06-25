package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/cockpit"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/presence"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
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
	fmt.Fprintln(w, "  gitmoot daemon start [--repo owner/repo] [--poll 30s] [--workers 1] [--watch-skillopt-reviews] [--watch-issues]")
	fmt.Fprintln(w, "  gitmoot daemon run [--repo owner/repo] [--poll 30s] [--workers 1] [--watch-skillopt-reviews] [--watch-issues]")
	fmt.Fprintln(w, "  gitmoot daemon stop")
	fmt.Fprintln(w, "  gitmoot daemon restart")
	fmt.Fprintln(w, "  gitmoot daemon status")
	fmt.Fprintln(w, "  gitmoot daemon logs")
}

func runDaemonStart(args []string, stdout, stderr io.Writer) int {
	return runDaemonStartWithWorkDir(args, "", stdout, stderr)
}

func runDaemonStartWithWorkDir(args []string, workDir string, stdout, stderr io.Writer) int {
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
	if cfg.RepoSet {
		if err := preflightDaemonRepoStart(context.Background(), cfg.Home, cfg.Repo, cfg.Poll.String(), resolvedWorkDir); err != nil {
			fmt.Fprintf(stderr, "daemon start: %v\n", err)
			return 1
		}
	}

	started, err := startDaemonChild(cfg.Home, cfg.Poll.String(), cfg.Workers, cfg.WatchSkillOptReviews, cfg.WatchIssues, cfg.Scheduler, cfg.RepoFlag, cfg.Session, state, resolvedWorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "daemon start: %v\n", err)
		return 1
	}
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

func runDaemonRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "GitHub repository as owner/repo")
	var session string
	fs.StringVar(&session, "session", "", "scope the daemon worker to a delegation root job id")
	fs.StringVar(&session, "root", "", "alias for --session")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	workers := fs.Int("workers", 1, "worker count")
	dryRun := fs.Bool("dry-run", false, "run without mutating external systems")
	watchSkillOptReviews := fs.Bool("watch-skillopt-reviews", false, "poll watched SkillOpt review issue comments and import valid feedback")
	watchIssues := fs.Bool("watch-issues", false, "poll open issues and route @<agent> ask comments to jobs (#389)")
	scheduler := fs.String("scheduler", "barrier", "queued-job scheduler: barrier (default) or pool (#394 opt-in continuous worker pool)")
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
	if *poll <= 0 {
		fmt.Fprintln(stderr, "poll interval must be positive")
		return 2
	}
	if *workers <= 0 {
		fmt.Fprintln(stderr, "workers must be positive")
		return 2
	}
	usePool, schedErr := parseSchedulerMode(*scheduler)
	if schedErr != nil {
		fmt.Fprintln(stderr, schedErr.Error())
		return 2
	}
	if *repoFlag != "" && *dryRun {
		fmt.Fprintln(stderr, "daemon run --dry-run is only supported without --repo")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *repoFlag == "" {
		err := runRegisteredRepoSupervisor(ctx, *home, *poll, *workers, *dryRun, *watchSkillOptReviews, *watchIssues, usePool, session, stdout)
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
		engine := workflow.Engine{
			Store:     store,
			MergeGate: newDaemonPolicyMergeGate(store, gh, checkout),
		}
		// Honor the opt-in [orchestrate] policy (artifact-body inlining + per-root
		// delegation token budget) on this single-repo engine too; fail-safe to the
		// defaults if the policy cannot load.
		defaultJobWorker(store, stdout, *home).applyOrchestratePolicy(&engine)
		fmt.Fprintf(stdout, "watching %s every %s\n", repo.FullName(), poll.String())
		return runSingleRepoSupervisor(ctx, *home, daemon.Daemon{
			Repo:          repo,
			PollInterval:  *poll,
			Store:         store,
			GitHub:        gh,
			Workflow:      &engine,
			WatchIssues:   *watchIssues,
			EscalationTTL: resolveEscalationTTL(*home),
		}, store, *workers, usePool, session, stdout)
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
	if stale {
		writeLine(stdout, "removed stale daemon pid file")
		return 0
	}
	if pid == 0 {
		writeLine(stdout, "daemon not running")
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
	return runDaemonStartWithWorkDir(targetArgs, targetWorkDir, stdout, stderr)
}

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
		writeLine(stdout, "%s", daemonClaudeAuthLine(paths))
	}
	writeLine(stdout, "%s", daemonAdmissionLine(paths))
	writeLine(stdout, "%s", daemonPreflightFailureLine(*home))
	return 0
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

// daemonClaudeAuthLine reports the running daemon's Claude background-auth state
// for `gitmoot daemon status` (#427). It is best-effort and OS-gated: when the
// daemon's environment can't be read (non-Linux, hardened /proc) it says so
// rather than implying the daemon is unauthenticated. Secrets are never printed
// — only the masked set/unset booleans.
func daemonClaudeAuthLine(paths config.Paths) string {
	daemon := presence.InspectDaemonClaudeAuth(paths)
	if !daemon.Detected {
		return "claude auth: unknown (daemon environment not readable on this host)"
	}
	if daemon.Auth.Ready() {
		return "claude auth: ok (" + daemon.Auth.MaskedDetail() + ")"
	}
	return "claude auth: warn (" + daemon.Auth.MaskedDetail() + "); " + runtime.ClaudeBackgroundTokenMessage
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
	repoFlag := fs.String("repo", "", "GitHub repository as owner/repo")
	var session string
	fs.StringVar(&session, "session", "", "scope the daemon worker to a delegation root job id")
	fs.StringVar(&session, "root", "", "alias for --session")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	workers := fs.Int("workers", 1, "worker count")
	watchSkillOptReviews := fs.Bool("watch-skillopt-reviews", false, "poll watched SkillOpt review issue comments and import valid feedback")
	watchIssues := fs.Bool("watch-issues", false, "poll open issues and route @<agent> ask comments to jobs (#389)")
	scheduler := fs.String("scheduler", "barrier", "queued-job scheduler: barrier (default) or pool (#394 opt-in continuous worker pool)")
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
		}
	})
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

func runRegisteredRepoSupervisor(ctx context.Context, home string, poll time.Duration, workers int, dryRun bool, watchSkillOptReviews bool, watchIssues bool, usePool bool, rootFilter string, stdout io.Writer) error {
	return withStoreAndPaths(home, func(paths config.Paths, store *db.Store) error {
		schedule := registeredRepoSchedule{
			NextPoll:    map[string]time.Time{},
			ErrorStreak: map[string]int{},
		}
		poller := defaultRegisteredRepoPoller(store, workers, dryRun, stdout, paths.Home)
		poller.WatchIssues = watchIssues
		blobStore := artifact.NewStore(paths.ArtifactBlobs)
		reviewGitHub := newSkillOptGitHubClient()
		worker := defaultJobWorker(store, stdout, home)
		worker.CommenterFactory = worker.defaultCommenter
		worker.UsePool = usePool
		worker.Admission = worker.loadAdmissionBudget()
		checkoutLocks := &repoCheckoutLocks{}
		poller.CheckoutLocks = checkoutLocks
		var workerErr <-chan error
		if !dryRun {
			if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, time.Now().UTC()); err != nil {
				return err
			}
			if err := recoverCancelledRunningJobsForEnabledRepos(ctx, store, rootFilter, stdout); err != nil {
				return err
			}
			workerErr = startSupervisorWorkerLoop(ctx, daemonWorkerLoopInterval, func(now time.Time) error {
				return runEnabledRepoWorkerTicksWithLocks(ctx, store, worker, workers, rootFilter, stdout, now, checkoutLocks)
			})
			startCockpitReconcileLoop(ctx, store, paths.Home, stdout)
		}
		for {
			if err := receiveSupervisorWorkerError(workerErr); err != nil {
				return err
			}
			wait, err := pollRegisteredReposWithPoller(ctx, poller, schedule, time.Now().UTC(), poll)
			if err != nil {
				return err
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

func runSingleRepoSupervisor(ctx context.Context, home string, d daemon.Daemon, store *db.Store, workers int, usePool bool, rootFilter string, stdout io.Writer) error {
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, time.Now().UTC()); err != nil {
		return err
	}
	if err := recoverRunningJobsForRepo(ctx, store, stdout, d.Repo.FullName(), rootFilter); err != nil {
		return err
	}
	if err := recoverCancelledRunningJobsForRepo(ctx, store, stdout, d.Repo.FullName(), rootFilter); err != nil {
		return err
	}
	interval := d.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	worker := defaultJobWorker(store, stdout, home)
	worker.CommenterFactory = worker.defaultCommenter
	worker.UsePool = usePool
	worker.Admission = worker.loadAdmissionBudget()
	var checkoutLock sync.Mutex
	workerErr := startSupervisorWorkerLoop(ctx, daemonWorkerLoopInterval, func(now time.Time) error {
		checkoutLock.Lock()
		defer checkoutLock.Unlock()
		return runDaemonWorkerTick(ctx, store, worker, workers, false, d.Repo.FullName(), rootFilter, stdout, now)
	})
	startCockpitReconcileLoop(ctx, store, home, stdout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := receiveSupervisorWorkerError(workerErr); err != nil {
			return err
		}
		if checkoutLock.TryLock() {
			_ = d.PollOnce(ctx)
			checkoutLock.Unlock()
		} else {
			_ = d.PollRecoveryCommandsOnce(ctx)
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

func startSupervisorWorkerLoop(ctx context.Context, interval time.Duration, run func(time.Time) error) <-chan error {
	errCh := make(chan error, 1)
	if interval <= 0 {
		interval = daemonWorkerLoopInterval
	}
	go func() {
		defer close(errCh)
		for {
			if err := ctx.Err(); err != nil {
				return
			}
			if err := run(time.Now().UTC()); err != nil {
				errCh <- err
				return
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return errCh
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
	return pollRegisteredReposWithPoller(ctx, defaultRegisteredRepoPoller(store, workers, dryRun, stdout, ""), registeredRepoSchedule{NextPoll: nextPoll}, now, fallbackPoll)
}

type registeredRepoSchedule struct {
	NextPoll    map[string]time.Time
	ErrorStreak map[string]int
}

func (s registeredRepoSchedule) ensure() registeredRepoSchedule {
	if s.NextPoll == nil {
		s.NextPoll = map[string]time.Time{}
	}
	if s.ErrorStreak == nil {
		s.ErrorStreak = map[string]int{}
	}
	return s
}

type registeredRepoPoller struct {
	Store           *db.Store
	Workers         int
	DryRun          bool
	Stdout          io.Writer
	RecoveryOnly    bool
	WatchIssues     bool
	EscalationTTL   time.Duration
	CheckoutLocks   *repoCheckoutLocks
	GitHubClient    func(checkout string) github.Client
	WorkflowFactory func(store *db.Store, gh github.Client, checkout string) *workflow.Engine
}

func defaultRegisteredRepoPoller(store *db.Store, workers int, dryRun bool, stdout io.Writer, home string) registeredRepoPoller {
	return registeredRepoPoller{
		Store:         store,
		Workers:       workers,
		DryRun:        dryRun,
		Stdout:        stdout,
		EscalationTTL: resolveEscalationTTL(home),
		GitHubClient:  func(checkout string) github.Client { return github.NewClient(checkout) },
		WorkflowFactory: func(store *db.Store, gh github.Client, checkout string) *workflow.Engine {
			engine := daemonWorkflowEngine(store, gh, checkout, home)
			// Apply only the escalate_human notifier handle from policy (#340),
			// keeping the budget/inlining knobs out of this path so its existing
			// behavior is unchanged. The notifier itself is already wired by
			// daemonWorkflowEngine; this just sets the configured @-handle.
			if notifier, ok := engine.EscalationNotifier.(*daemonEscalationNotifier); ok && notifier != nil {
				if policy, err := defaultJobWorker(store, stdout, home).orchestratePolicy(); err == nil {
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
func resolveEscalationTTL(home string) time.Duration {
	policy := config.DefaultOrchestratePolicy()
	if strings.TrimSpace(home) != "" {
		if paths, err := initializedPaths(home); err == nil {
			if loaded, err := config.LoadOrchestratePolicy(paths); err == nil {
				policy = loaded
			}
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

func pollRegisteredReposWithPoller(ctx context.Context, poller registeredRepoPoller, schedule registeredRepoSchedule, now time.Time, fallbackPoll time.Duration) (time.Duration, error) {
	schedule = schedule.ensure()
	repos, err := poller.Store.ListRepos(ctx)
	if err != nil {
		return fallbackPoll, err
	}
	enabled := 0
	polled := 0
	wait := fallbackPoll
	waitSet := false
	for _, repoRecord := range repos {
		if !repoRecord.Enabled {
			continue
		}
		enabled++
		fullName := repoRecord.FullName()
		interval := repoPollInterval(repoRecord.PollInterval, fallbackPoll)
		dueAt := schedule.NextPoll[fullName]
		if !dueAt.IsZero() && dueAt.After(now) {
			wait = shorterWait(wait, dueAt.Sub(now), &waitSet)
			continue
		}
		polled++
		lastError, err := poller.pollRepo(ctx, repoRecord, now)
		if err != nil {
			return wait, err
		}
		nextInterval := interval
		if lastError != "" {
			schedule.ErrorStreak[fullName]++
			nextInterval = repoBackoffInterval(interval, schedule.ErrorStreak[fullName])
		} else {
			delete(schedule.ErrorStreak, fullName)
		}
		schedule.NextPoll[fullName] = now.Add(nextInterval)
		wait = shorterWait(wait, nextInterval, &waitSet)
	}
	writeLine(poller.Stdout, "supervised %d enabled repos, polled %d", enabled, polled)
	if wait <= 0 {
		wait = fallbackPoll
	}
	return wait, nil
}

func (p registeredRepoPoller) pollRepo(ctx context.Context, repoRecord db.Repo, now time.Time) (string, error) {
	store := p.Store
	repo, err := daemon.ParseRepository(repoRecord.FullName())
	if err != nil {
		lastError := err.Error()
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), lastError)
		return lastError, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), now.Format(time.RFC3339), lastError)
	}
	lastPollAt := now.Format(time.RFC3339)
	if strings.TrimSpace(repoRecord.CheckoutPath) == "" {
		message := "registered repo has no checkout path"
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), message)
		return message, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, message)
	}
	writeLine(p.Stdout, "polling %s with %d workers dry_run=%t", repoRecord.FullName(), p.Workers, p.DryRun)
	if p.DryRun {
		return "", store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, "")
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
	d := daemon.Daemon{
		Repo:          repo,
		Store:         store,
		GitHub:        gh,
		Workflow:      engine,
		WatchIssues:   p.WatchIssues,
		EscalationTTL: p.EscalationTTL,
	}
	if recoveryOnly {
		err = d.PollRecoveryCommandsOnce(ctx)
	} else {
		err = d.PollOnce(ctx)
	}
	lastError := ""
	if err != nil {
		lastError = err.Error()
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), lastError)
	}
	return lastError, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, lastError)
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
	Store               *db.Store
	Stdout              io.Writer
	ConfigHome          string
	ConfigHomeExplicit  bool
	AdapterFactory      func(runtime.Agent, string) (workflow.DeliveryAdapter, error)
	StartAdapterFactory func(string, string) (runtime.Adapter, error)
	CheckoutValidator   func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error)
	WorkflowFactory     func(string) workflow.Engine
	CommenterFactory    func(string) github.Client
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
const daemonJobCancelPollInterval = 250 * time.Millisecond
const daemonWorkerLoopInterval = 1 * time.Second

// cockpitReconcileInterval is the low-frequency cadence of the cockpit reconcile
// GC sweep (Task 7): it drops cockpit_pane rows whose herdr pane is gone and whose
// owning root is terminal. It runs rarely because it is a backstop for the
// per-Deliver / root-finalize teardown plus report-metadata --ttl-ms self-expiry.
const cockpitReconcileInterval = 5 * time.Minute

var errRuntimeSessionBusy = errors.New("runtime session is busy")

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
	worker.AdapterFactory = worker.defaultAdapter
	worker.StartAdapterFactory = worker.defaultStartAdapter
	worker.CheckoutValidator = worker.defaultCheckout
	worker.WorkflowFactory = worker.defaultWorkflow
	return worker
}

func recoverRunningJobs(ctx context.Context, store *db.Store, stdout io.Writer) error {
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, time.Now().UTC().Add(-daemonRunningJobStaleAfter), "", "")
}

func recoverExpiredRuntimeSessionLocks(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time) error {
	deleted, err := store.DeleteExpiredResourceLocks(ctx, now)
	if err != nil {
		return err
	}
	if deleted > 0 {
		writeLine(stdout, "recovered %d expired runtime session locks", deleted)
	}
	return nil
}

func recoverRunningJobsBefore(ctx context.Context, store *db.Store, stdout io.Writer, before time.Time) error {
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, before, "", "")
}

func recoverRunningJobsForRepo(ctx context.Context, store *db.Store, stdout io.Writer, repoFilter string, rootFilter string) error {
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, time.Now().UTC().Add(-daemonRunningJobStaleAfter), repoFilter, rootFilter)
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

func recoverRunningJobsBeforeForRepo(ctx context.Context, store *db.Store, stdout io.Writer, before time.Time, repoFilter string, rootFilter string) error {
	jobs, err := store.ListRunningJobsUpdatedBefore(ctx, before)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
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
	}
	return nil
}

func runQueuedJobs(ctx context.Context, worker jobWorker, limit int) error {
	return runQueuedJobsForRepo(ctx, worker, limit, "", "")
}

func retryPendingJobAdvancements(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string) error {
	jobs, err := worker.Store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
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
		if err := worker.advanceJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

func runDaemonWorkerTick(ctx context.Context, store *db.Store, worker jobWorker, workers int, dryRun bool, repoFilter string, rootFilter string, stdout io.Writer, now time.Time) error {
	if dryRun {
		return nil
	}
	if err := recoverRunningJobsBeforeForRepo(ctx, store, stdout, now.Add(-daemonRunningJobStaleAfter), repoFilter, rootFilter); err != nil {
		return err
	}
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, now); err != nil {
		return err
	}
	if err := retryPendingJobAdvancements(ctx, worker, repoFilter, rootFilter); err != nil {
		return err
	}
	if err := retryPendingJobComments(ctx, worker, repoFilter, rootFilter); err != nil {
		return err
	}
	return runQueuedJobsForRepo(ctx, worker, workers, repoFilter, rootFilter)
}

func runEnabledRepoWorkerTicks(ctx context.Context, store *db.Store, worker jobWorker, workers int, stdout io.Writer, now time.Time) error {
	return runEnabledRepoWorkerTicksWithLocks(ctx, store, worker, workers, "", stdout, now, nil)
}

func runEnabledRepoWorkerTicksWithLocks(ctx context.Context, store *db.Store, worker jobWorker, workers int, rootFilter string, stdout io.Writer, now time.Time, locks *repoCheckoutLocks) error {
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		lock := locks.For(repo.FullName())
		if lock != nil {
			lock.Lock()
		}
		if err := runDaemonWorkerTick(ctx, store, worker, workers, false, repo.FullName(), rootFilter, stdout, now); err != nil {
			if lock != nil {
				lock.Unlock()
			}
			return err
		}
		if lock != nil {
			lock.Unlock()
		}
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

func retryPendingJobComments(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string) error {
	jobs, err := worker.Store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
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

func runQueuedJobsForRepo(ctx context.Context, worker jobWorker, limit int, repoFilter string, rootFilter string) error {
	if limit <= 0 {
		return nil
	}
	if worker.UsePool {
		return runQueuedJobsForRepoPool(ctx, worker, limit, repoFilter, rootFilter)
	}
	pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter)
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

// listPendingQueuedJobs returns the queued jobs eligible to run for this
// repo/session filter, dropping children of a killed root.
//
// Operator kill switch (#341): once a tree's root is killed, do not start any of
// its queued children. The coordinator's own continuation still runs so the
// engine can route through the graceful finalize path; in-flight children finish
// normally. Only children (payload.RootJobID points at another root) are skipped
// here — the root job itself is never skipped.
func listPendingQueuedJobs(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string) ([]db.Job, error) {
	jobs, err := worker.Store.ListQueuedJobs(ctx)
	if err != nil {
		return nil, err
	}
	pending := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if queuedChildOfKilledRoot(ctx, worker.Store, job) {
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
		err          error
	}
	inflightCheckouts := map[string]bool{}
	inflightRuntimes := map[string]bool{}
	running := 0
	done := make(chan finished, limit)
	var firstErr error

	reap := func(f finished) {
		delete(inflightCheckouts, f.checkoutKey)
		if f.runtimeKey != "" {
			delete(inflightRuntimes, f.runtimeKey)
		}
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
			pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter)
			if err != nil {
				firstErr = err
			} else if slots := limit - running; slots > 0 {
				queued, remaining := selectRunnableQueuedJobsSeeded(ctx, worker.Store, pending, slots, policy, inflightCheckouts, inflightRuntimes)
				for _, job := range queued {
					job := job
					// Host-global admission gate (#365): reserve a session slot + RAM
					// estimate before claiming any checkout/runtime key or a worker slot.
					// A job that does not fit the budget is skipped (left queued) and the
					// pool re-queries on the next pass once a reap frees a slot — never
					// failed/dropped. A nil budget always admits ⇒ byte-identical default.
					if !worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
						continue
					}
					checkoutKey := queuedJobCheckoutKey(ctx, worker.Store, job)
					runtimeKey := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
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
				for _, job := range remaining {
					if running >= limit {
						break
					}
					payload, perr := daemonJobPayload(job)
					if perr != nil || !poolIsolationEligible(job, payload) {
						continue
					}
					if queuedJobCheckoutKey(ctx, worker.Store, job) != "repo:"+payload.Repo || !inflightCheckouts["repo:"+payload.Repo] {
						continue // not blocked by a contended same-repo checkout
					}
					runtimeKey := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
					if runtimeKey != "" && (inflightRuntimes[runtimeKey] || runtimeResourceLocked(ctx, worker.Store, runtimeKey)) {
						continue // also runtime-contended; leave it to the runtime/temp-worker path
					}
					// Host-global admission gate (#365): reserve before creating the
					// isolation worktree so a deferred job leaves no orphan worktree behind.
					if !worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
						continue
					}
					iso, ok := worker.allocatePoolIsolationWorktree(ctx, job, payload)
					if !ok {
						worker.Admission.Release(job.ID)
						continue
					}
					inflightCheckouts[iso.checkoutKey] = true
					if iso.runtimeKey != "" {
						inflightRuntimes[iso.runtimeKey] = true
					}
					running++
					dispatched++
					go func() {
						done <- finished{jobID: iso.job.ID, checkoutKey: iso.checkoutKey, runtimeKey: iso.runtimeKey, worktreePath: iso.worktreePath, repoCheckout: iso.repoCheckout, err: runPoolJobRecovered(ctx, worker, iso.job)}
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
	case "ask", "review":
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
func (w jobWorker) allocatePoolIsolationWorktree(ctx context.Context, job db.Job, payload workflow.JobPayload) (poolIsolatedDispatch, bool) {
	if strings.TrimSpace(w.ConfigHome) == "" {
		return poolIsolatedDispatch{}, false
	}
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil || strings.TrimSpace(repoRecord.CheckoutPath) == "" {
		return poolIsolatedDispatch{}, false
	}
	path, err := workflow.DelegationWorktreePath(w.ConfigHome, payload.Repo, job.ID, "pool-isolation", 0)
	if err != nil || strings.TrimSpace(path) == "" {
		return poolIsolatedDispatch{}, false
	}
	ref := firstNonEmpty(strings.TrimSpace(payload.HeadSHA), strings.TrimSpace(payload.Branch), "HEAD")
	client := gitutil.Client{Dir: repoRecord.CheckoutPath}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return poolIsolatedDispatch{}, false
	}
	if err := client.AddDetachedWorktree(ctx, path, ref); err != nil {
		return poolIsolatedDispatch{}, false
	}
	payload.WorktreePath = path
	encoded, err := json.Marshal(payload)
	if err != nil {
		_ = client.RemoveWorktreeForce(context.WithoutCancel(ctx), path)
		return poolIsolatedDispatch{}, false
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		_ = client.RemoveWorktreeForce(context.WithoutCancel(ctx), path)
		return poolIsolatedDispatch{}, false
	}
	job.Payload = string(encoded)
	return poolIsolatedDispatch{
		job:          job,
		checkoutKey:  queuedJobCheckoutKey(ctx, w.Store, job),
		runtimeKey:   queuedJobRuntimeResourceKey(ctx, w.Store, job),
		worktreePath: path,
		repoCheckout: repoRecord.CheckoutPath,
	}, true
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
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
		return nil
	}
	adapter, err := w.AdapterFactory(agent, checkout)
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
	lockTTL := daemonRunningJobStaleAfter
	if jobTimeout > 0 {
		lockTTL = jobTimeout
	}
	releaseLock, acquired, lockKey, err := acquireRuntimeSessionLock(ctx, w.Store, job.ID, agent, time.Now().UTC(), lockTTL)
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
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: message}); eventErr != nil {
			return eventErr
		}
		writeLine(w.Stdout, "job %s waiting: %s", job.ID, message)
		return fmt.Errorf("%w: %s", errRuntimeSessionBusy, message)
	}
	defer func() {
		if err := releaseLock(context.Background()); err != nil {
			writeLine(w.Stdout, "job %s runtime lock release failed: %v", job.ID, err)
		}
	}()
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
			teeAdapter, logPath, logFile := w.cockpitLogAdapter(cp, agent, checkout, job.ID, meta.RootJobID, meta.PaneKey, seatMode)
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
	engine := w.WorkflowFactory(checkout)
	runCtx, stopRun := w.runningJobContext(ctx, job.ID)
	defer stopRun()
	if jobTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, jobTimeout)
		defer cancel()
	}
	_, err = engine.RunJob(runCtx, job.ID, agent, adapter)
	if err != nil {
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

func (w jobWorker) parallelSessionPolicy() (config.ParallelSessionPolicy, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return config.DefaultParallelSessionPolicy(), nil
	}
	paths, err := initializedPaths(w.ConfigHome)
	if err != nil {
		return config.ParallelSessionPolicy{}, err
	}
	return config.LoadParallelSessionPolicy(paths)
}

// admissionPolicy loads the host-level [admission] budget config, mirroring
// parallelSessionPolicy: an implicit/empty config home uses the defaults
// (both caps 0 ⇒ off), and an explicit home loads from the config file.
func (w jobWorker) admissionPolicy() (config.AdmissionPolicy, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return config.DefaultAdmissionPolicy(), nil
	}
	paths, err := initializedPaths(w.ConfigHome)
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
	case runtime.KimiRuntime:
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
	paths, err := initializedPaths(w.ConfigHome)
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
func (w jobWorker) cockpitTeeAdapter(agent runtime.Agent, checkout string, jobID string) (workflow.DeliveryAdapter, string, *os.File) {
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
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit log create failed: %v", jobID, err)
		return nil, "", nil
	}
	return w.cockpitTeeOnFile(agent, checkout, jobID, logPath, logFile)
}

// cockpitTeeOnFile rebuilds the runtime adapter to tee the child's live
// stdout/stderr into an already-open log file, shared by the per-job (truncate)
// and per-seat (append) log paths. It is fail-open: an unsupported runtime closes
// the file and returns nils so the caller falls back to the P0 pane.
func (w jobWorker) cockpitTeeOnFile(agent runtime.Agent, checkout, jobID, logPath string, logFile *os.File) (workflow.DeliveryAdapter, string, *os.File) {
	adapter, err := buildRuntimeAdapter(agent, checkout, subprocess.TeeRunner{Inner: subprocess.GroupRunner{}, Out: logFile})
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
func (w jobWorker) cockpitLogAdapter(cp *cockpit.Cockpit, agent runtime.Agent, checkout, jobID, rootJobID, paneKey string, seatMode bool) (workflow.DeliveryAdapter, string, *os.File) {
	if seatMode {
		return w.cockpitSeatLogAdapter(cp, agent, checkout, jobID, rootJobID, paneKey)
	}
	return w.cockpitTeeAdapter(agent, checkout, jobID)
}

// cockpitSeatLogAdapter opens the stable per-seat append log the seat's one pane
// tails across delegation rounds (Task 7) and tees the child's stdout/stderr into
// it. The path is <home>/logs/seats/<rootShort>/<seatSlug>.log, opened O_APPEND so
// each round's output accumulates rather than truncating the prior round's — no
// tail re-pointing needed. The log is NOT removed per job; it persists for the
// root's life and is removed by FinalizeRoot. It is fail-open: any failure
// (unresolved path, mkdir, create, unsupported runtime) returns nils so the caller
// falls back to the P0 pane.
func (w jobWorker) cockpitSeatLogAdapter(cp *cockpit.Cockpit, agent runtime.Agent, checkout, jobID, rootJobID, paneKey string) (workflow.DeliveryAdapter, string, *os.File) {
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
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit seat log open failed: %v", jobID, err)
		return nil, "", nil
	}
	return w.cockpitTeeOnFile(agent, checkout, jobID, logPath, logFile)
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
		if !cockpitJobStateTerminal(j.State) {
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

// cockpitJobStateTerminal reports whether a job state is terminal for the purpose
// of root-tree finalization: nothing more will run for it.
func cockpitJobStateTerminal(state string) bool {
	switch state {
	case string(workflow.JobSucceeded), string(workflow.JobFailed),
		string(workflow.JobCancelled):
		return true
	default:
		// JobBlocked is deliberately NOT terminal: a blocked job (e.g. awaiting a
		// permission/approval or interactive answer) can resume, and finalizing the
		// root then would tear down a pane + seat log the job still needs. The
		// engine's graceful-finalize continuation provides the real terminal signal
		// for a stuck tree.
		return false
	}
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
	case runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime:
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
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: waitMessage}); eventErr != nil {
			return eventErr
		}
		writeLine(w.Stdout, "job %s waiting: %s", job.ID, waitMessage)
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
	releaseLock, acquired, lockKey, err := acquireRuntimeSessionLock(ctx, w.Store, delegatedJob.ID, started.Agent, time.Now().UTC(), started.JobTimeout)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	if !acquired {
		message := fmt.Sprintf("runtime session %s is busy", lockKey)
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: delegatedJob.ID, Kind: "runtime_lock_wait", Message: message}); eventErr != nil {
			return eventErr
		}
		writeLine(w.Stdout, "job %s waiting: %s", delegatedJob.ID, message)
		return fmt.Errorf("%w: %s", errRuntimeSessionBusy, message)
	}
	defer func() {
		if err := releaseLock(context.Background()); err != nil {
			writeLine(w.Stdout, "job %s temp runtime lock release failed: %v", delegatedJob.ID, err)
		}
	}()
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
	if _, err := (workflow.Mailbox{Store: w.Store}).Enqueue(ctx, request); err != nil {
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
	jobTimeout := daemonRunningJobStaleAfter
	if managed, err := w.managedJobConfig(ctx, original.Name); err == nil && managed.OK {
		idleTimeout = managed.IdleTimeout
		jobTimeout = managed.JobTimeout
	} else if err != nil {
		return tempWorkerStartResult{}, err
	}
	tempAgent := original
	tempAgent.Name = tempWorkerInstanceName(original.Name, job.ID)
	tempAgent.RuntimeRef = ""
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
	// write. The spec may still opt into a different (validated) policy.
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
		Capabilities:   ephemeralAgent.Capabilities,
		AutonomyPolicy: ephemeralAgent.AutonomyPolicy,
		State:          "starting",
		CreatedAt:      formatManagedAgentTime(now),
		LastUsedAt:     formatManagedAgentTime(now),
		ExpiresAt:      formatManagedAgentTime(now.Add(daemonRunningJobStaleAfter)),
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
// otherwise the agent-type managed.JobTimeout (which is zero when the agent is
// not managed). The same value drives both the runtime-session lock TTL and the
// run context deadline so the lock cannot expire before the job does.
func effectiveJobTimeout(payload workflow.JobPayload, managed managedJobRuntimeConfig) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(payload.JobTimeout)); err == nil && d > 0 {
		return d
	}
	return managed.JobTimeout
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
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retry", Message: "post-delivery workflow retry refresh failed: " + err.Error()})
	} else if ok {
		payload = refreshed
	}
	checkout, err := w.CheckoutValidator(ctx, job, payload, agent)
	if err != nil {
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retry", Message: "post-delivery workflow retry preflight failed: " + err.Error()})
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
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retry", Message: "post-delivery workflow retry failed: " + err.Error()})
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
	return buildRuntimeAdapter(agent, checkout, nil)
}

// buildRuntimeAdapter constructs the concrete runtime adapter for a job. A nil
// runner leaves the adapter's Runner unset, so it falls through to the
// process-group GroupRunner{} via the adapter's runner() — byte-identical to the
// non-cockpit path. The cockpit path (Task 6) passes a non-nil tee runner so the
// child's stdout/stderr is also streamed live into the per-job log the pane
// tails; the tee preserves group-kill (its inner is GroupRunner{}) and returns
// the same buffered Result, so result capture, locks, and signals are unchanged.
func buildRuntimeAdapter(agent runtime.Agent, checkout string, runner subprocess.Runner) (workflow.DeliveryAdapter, error) {
	switch agent.Runtime {
	case runtime.CodexRuntime:
		return runtime.CodexAdapter{Dir: checkout, Runner: runner}, nil
	case runtime.ClaudeRuntime:
		return runtime.ClaudeAdapter{Dir: checkout, Runner: runner}, nil
	case runtime.KimiRuntime:
		return runtime.KimiAdapter{Dir: checkout, Runner: runner}, nil
	case runtime.ShellRuntime:
		return runtime.ShellAdapter{Dir: checkout, Runner: runner}, nil
	default:
		return nil, fmt.Errorf("unsupported runtime: %s", agent.Runtime)
	}
}

func (w jobWorker) defaultStartAdapter(runtimeName string, checkout string) (runtime.Adapter, error) {
	return runtimeStartAdapter(newRuntimeFactory(), runtimeName, checkout)
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

func daemonWorkflowEngine(store *db.Store, gh github.Client, checkout string, home string) workflow.Engine {
	engine := workflow.Engine{
		Store:                   store,
		MergeGate:               daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout},
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
		PayloadRefresher: func(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
			return refreshDaemonJobPayload(ctx, store, checkout, job, payload)
		},
	}
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
}

func (g daemonMergeGate) Evaluate(ctx context.Context, request workflow.MergeRequest) (workflow.MergeDecision, error) {
	checkout, err := mergeGateCheckout(ctx, g.Store, request.Repo, g.FallbackCheckout)
	if err != nil {
		return workflow.MergeDecision{}, err
	}
	return newDaemonPolicyMergeGate(g.Store, g.githubClient(checkout), checkout).Evaluate(ctx, request)
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
		if payload.PullRequest > 0 && strings.TrimSpace(payload.TaskID) != "" {
			if err := w.validateReviewCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		} else if !isWorktreeLessDelegationChild(payload) {
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
	checkout := strings.TrimSpace(repoRecord.CheckoutPath)
	if checkout == "" {
		return "", fmt.Errorf("repo %s has no checkout path", payload.Repo)
	}
	repo, err := daemon.ParseRepository(payload.Repo)
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
	return w.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "advance_retry",
		Message: "post-delivery workflow error; advancement will retry from stored result: " + cause.Error(),
	})
}

func (w jobWorker) postJobResultComment(ctx context.Context, jobID string, agent runtime.Agent, _ string, cause error) error {
	job, payload, err := daemonWorkerJobPayload(ctx, w.Store, jobID)
	if err != nil {
		return err
	}
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
// checkout path, it resolves against that REGISTERED checkout so the command
// works from any working directory (#202) — origin protection is still enforced,
// just against the registered checkout rather than the cwd. When the repo is not
// yet registered (or has no checkout path), it bootstraps from workDir (the
// current checkout), preserving the original behavior for first-time setup.
func resolveDaemonStartRepo(ctx context.Context, store *db.Store, repo github.Repository, workDir string) (db.Repo, error) {
	existing, err := store.GetRepo(ctx, repo.FullName())
	switch {
	case err == nil && strings.TrimSpace(existing.CheckoutPath) != "":
		record, rerr := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: existing.CheckoutPath})
		if rerr != nil {
			return db.Repo{}, fmt.Errorf("registered checkout for %s at %s is unusable (re-register with `gitmoot repo add`): %w", repo.FullName(), existing.CheckoutPath, rerr)
		}
		return record, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return db.Repo{}, err
	}
	return repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: workDir})
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
