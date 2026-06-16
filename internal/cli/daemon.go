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
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
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
	fmt.Fprintln(w, "  gitmoot daemon start [--repo owner/repo] [--poll 30s] [--workers 1] [--watch-skillopt-reviews]")
	fmt.Fprintln(w, "  gitmoot daemon run [--repo owner/repo] [--poll 30s] [--workers 1] [--watch-skillopt-reviews]")
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

	started, err := startDaemonChild(cfg.Home, cfg.Poll.String(), cfg.Workers, cfg.WatchSkillOptReviews, state, resolvedWorkDir)
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

func runDaemonRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "GitHub repository as owner/repo")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	workers := fs.Int("workers", 1, "worker count")
	dryRun := fs.Bool("dry-run", false, "run without mutating external systems")
	watchSkillOptReviews := fs.Bool("watch-skillopt-reviews", false, "poll watched SkillOpt review issue comments and import valid feedback")
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
	if *repoFlag != "" && *dryRun {
		fmt.Fprintln(stderr, "daemon run --dry-run is only supported without --repo")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *repoFlag == "" {
		err := runRegisteredRepoSupervisor(ctx, *home, *poll, *workers, *dryRun, *watchSkillOptReviews, stdout)
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
		repoRecord, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
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
		fmt.Fprintf(stdout, "watching %s every %s\n", repo.FullName(), poll.String())
		return runSingleRepoSupervisor(ctx, *home, daemon.Daemon{
			Repo:         repo,
			PollInterval: *poll,
			Store:        store,
			GitHub:       gh,
			Workflow:     &engine,
		}, store, *workers, stdout)
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
	return 0
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
	Poll                         time.Duration
	Workers                      int
	ExplicitStartConfig          bool
	ExplicitRepo                 bool
	ExplicitPoll                 bool
	ExplicitWorkers              bool
	WatchSkillOptReviews         bool
	ExplicitWatchSkillOptReviews bool
}

const daemonHelp = -1

func parseDaemonStartConfig(command string, args []string, stderr io.Writer) (daemonStartConfig, int) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "GitHub repository as owner/repo")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	workers := fs.Int("workers", 1, "worker count")
	watchSkillOptReviews := fs.Bool("watch-skillopt-reviews", false, "poll watched SkillOpt review issue comments and import valid feedback")
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
		Poll:                 *poll,
		Workers:              *workers,
		WatchSkillOptReviews: *watchSkillOptReviews,
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "repo":
			cfg.ExplicitRepo = true
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
		repoRecord, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: workDir})
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
	if cfg.ExplicitPoll {
		args = withDaemonFlagArg(args, "poll", cfg.Poll.String())
	}
	if cfg.ExplicitWorkers {
		args = withDaemonFlagArg(args, "workers", strconv.Itoa(cfg.Workers))
	}
	if cfg.ExplicitWatchSkillOptReviews {
		args = withDaemonBoolFlagArg(args, "watch-skillopt-reviews", cfg.WatchSkillOptReviews)
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

func startDaemonChild(home string, poll string, workers int, watchSkillOptReviews bool, state daemonState, workDir string) (daemonMeta, error) {
	executable, err := os.Executable()
	if err != nil {
		return daemonMeta{}, err
	}
	logFile, err := os.OpenFile(state.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return daemonMeta{}, err
	}
	defer logFile.Close()
	args := daemonChildArgs(home, poll, workers, watchSkillOptReviews)
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

func daemonChildArgs(home string, poll string, workers int, watchSkillOptReviews bool) []string {
	args := []string{"daemon", "run", "--poll", poll, "--workers", strconv.Itoa(workers)}
	if home != "" {
		args = append(args, "--home", home)
	}
	if watchSkillOptReviews {
		args = append(args, "--watch-skillopt-reviews")
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

func runRegisteredRepoSupervisor(ctx context.Context, home string, poll time.Duration, workers int, dryRun bool, watchSkillOptReviews bool, stdout io.Writer) error {
	return withStoreAndPaths(home, func(paths config.Paths, store *db.Store) error {
		schedule := registeredRepoSchedule{
			NextPoll:    map[string]time.Time{},
			ErrorStreak: map[string]int{},
		}
		poller := defaultRegisteredRepoPoller(store, workers, dryRun, stdout)
		blobStore := artifact.NewStore(paths.ArtifactBlobs)
		reviewGitHub := newSkillOptGitHubClient()
		worker := defaultJobWorker(store, stdout, home)
		worker.CommenterFactory = worker.defaultCommenter
		checkoutLocks := &repoCheckoutLocks{}
		poller.CheckoutLocks = checkoutLocks
		var workerErr <-chan error
		if !dryRun {
			if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, time.Now().UTC()); err != nil {
				return err
			}
			if err := recoverCancelledRunningJobsForEnabledRepos(ctx, store, stdout); err != nil {
				return err
			}
			workerErr = startSupervisorWorkerLoop(ctx, daemonWorkerLoopInterval, func(now time.Time) error {
				return runEnabledRepoWorkerTicksWithLocks(ctx, store, worker, workers, stdout, now, checkoutLocks)
			})
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

func runSingleRepoSupervisor(ctx context.Context, home string, d daemon.Daemon, store *db.Store, workers int, stdout io.Writer) error {
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, time.Now().UTC()); err != nil {
		return err
	}
	if err := recoverRunningJobsForRepo(ctx, store, stdout, d.Repo.FullName()); err != nil {
		return err
	}
	if err := recoverCancelledRunningJobsForRepo(ctx, store, stdout, d.Repo.FullName()); err != nil {
		return err
	}
	interval := d.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	worker := defaultJobWorker(store, stdout, home)
	worker.CommenterFactory = worker.defaultCommenter
	var checkoutLock sync.Mutex
	workerErr := startSupervisorWorkerLoop(ctx, daemonWorkerLoopInterval, func(now time.Time) error {
		checkoutLock.Lock()
		defer checkoutLock.Unlock()
		return runDaemonWorkerTick(ctx, store, worker, workers, false, d.Repo.FullName(), stdout, now)
	})
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
	return pollRegisteredReposWithPoller(ctx, defaultRegisteredRepoPoller(store, workers, dryRun, stdout), registeredRepoSchedule{NextPoll: nextPoll}, now, fallbackPoll)
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
	CheckoutLocks   *repoCheckoutLocks
	GitHubClient    func(checkout string) github.Client
	WorkflowFactory func(store *db.Store, gh github.Client, checkout string) *workflow.Engine
}

func defaultRegisteredRepoPoller(store *db.Store, workers int, dryRun bool, stdout io.Writer) registeredRepoPoller {
	return registeredRepoPoller{
		Store:        store,
		Workers:      workers,
		DryRun:       dryRun,
		Stdout:       stdout,
		GitHubClient: func(checkout string) github.Client { return github.NewClient(checkout) },
		WorkflowFactory: func(store *db.Store, gh github.Client, checkout string) *workflow.Engine {
			engine := daemonWorkflowEngine(store, gh, checkout)
			return &engine
		},
	}
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
		Repo:     repo,
		Store:    store,
		GitHub:   gh,
		Workflow: engine,
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
}

const daemonRunningJobStaleAfter = 30 * time.Minute
const daemonJobCancelPollInterval = 250 * time.Millisecond
const daemonWorkerLoopInterval = 1 * time.Second

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
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, time.Now().UTC().Add(-daemonRunningJobStaleAfter), "")
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
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, before, "")
}

func recoverRunningJobsForRepo(ctx context.Context, store *db.Store, stdout io.Writer, repoFilter string) error {
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, time.Now().UTC().Add(-daemonRunningJobStaleAfter), repoFilter)
}

func recoverCancelledRunningJobsForEnabledRepos(ctx context.Context, store *db.Store, stdout io.Writer) error {
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		if err := recoverCancelledRunningJobsForRepo(ctx, store, stdout, repo.FullName()); err != nil {
			return err
		}
	}
	return nil
}

func recoverCancelledRunningJobsForRepo(ctx context.Context, store *db.Store, stdout io.Writer, repoFilter string) error {
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.State != string(workflow.JobCancelled) || !queuedJobMatchesRepo(job, repoFilter) {
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

func recoverRunningJobsBeforeForRepo(ctx context.Context, store *db.Store, stdout io.Writer, before time.Time, repoFilter string) error {
	jobs, err := store.ListRunningJobsUpdatedBefore(ctx, before)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if !queuedJobMatchesRepo(job, repoFilter) {
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
	return runQueuedJobsForRepo(ctx, worker, limit, "")
}

func retryPendingJobAdvancements(ctx context.Context, worker jobWorker, repoFilter string) error {
	jobs, err := worker.Store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if !jobStateCanRetryAdvancement(job.State) || !queuedJobMatchesRepo(job, repoFilter) {
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

func runDaemonWorkerTick(ctx context.Context, store *db.Store, worker jobWorker, workers int, dryRun bool, repoFilter string, stdout io.Writer, now time.Time) error {
	if dryRun {
		return nil
	}
	if err := recoverRunningJobsBeforeForRepo(ctx, store, stdout, now.Add(-daemonRunningJobStaleAfter), repoFilter); err != nil {
		return err
	}
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, now); err != nil {
		return err
	}
	if err := retryPendingJobAdvancements(ctx, worker, repoFilter); err != nil {
		return err
	}
	if err := retryPendingJobComments(ctx, worker, repoFilter); err != nil {
		return err
	}
	return runQueuedJobsForRepo(ctx, worker, workers, repoFilter)
}

func runEnabledRepoWorkerTicks(ctx context.Context, store *db.Store, worker jobWorker, workers int, stdout io.Writer, now time.Time) error {
	return runEnabledRepoWorkerTicksWithLocks(ctx, store, worker, workers, stdout, now, nil)
}

func runEnabledRepoWorkerTicksWithLocks(ctx context.Context, store *db.Store, worker jobWorker, workers int, stdout io.Writer, now time.Time, locks *repoCheckoutLocks) error {
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
		if err := runDaemonWorkerTick(ctx, store, worker, workers, false, repo.FullName(), stdout, now); err != nil {
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

func retryPendingJobComments(ctx context.Context, worker jobWorker, repoFilter string) error {
	jobs, err := worker.Store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if !jobStateCanRetryComment(job.State) || !queuedJobMatchesRepo(job, repoFilter) {
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

func runQueuedJobsForRepo(ctx context.Context, worker jobWorker, limit int, repoFilter string) error {
	if limit <= 0 {
		return nil
	}
	jobs, err := worker.Store.ListQueuedJobs(ctx)
	if err != nil {
		return err
	}
	pending := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if queuedJobMatchesRepo(job, repoFilter) {
			pending = append(pending, job)
		}
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

		errs := make(chan error, len(queued))
		var wg sync.WaitGroup
		for _, job := range queued {
			job := job
			wg.Add(1)
			go func() {
				defer wg.Done()
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
	if limit <= 0 {
		return nil, pending
	}
	selector := queuedJobResourceSelector{
		limit:            limit,
		policy:           policy,
		checkouts:        map[string]bool{},
		runtimes:         map[string]bool{},
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
		_ = w.postJobResultComment(ctx, job.ID, agent, "", errors.New(agentPermissionBlockedMessage))
		writeLine(w.Stdout, "job %s blocked: %s", job.ID, agentPermissionBlockedMessage)
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
	lockTTL := daemonRunningJobStaleAfter
	if managed.OK {
		lockTTL = managed.JobTimeout
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
	if managed.OK {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, managed.JobTimeout)
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

func tempWorkerEligible(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload, agent runtime.Agent, policy config.ParallelSessionPolicy, now time.Time) tempWorkerEligibility {
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
	switch agent.Runtime {
	case runtime.CodexRuntime:
		return runtime.CodexAdapter{Dir: checkout}, nil
	case runtime.ClaudeRuntime:
		return runtime.ClaudeAdapter{Dir: checkout}, nil
	case runtime.KimiRuntime:
		return runtime.KimiAdapter{Dir: checkout}, nil
	case runtime.ShellRuntime:
		return runtime.ShellAdapter{Dir: checkout}, nil
	default:
		return nil, fmt.Errorf("unsupported runtime: %s", agent.Runtime)
	}
}

func (w jobWorker) defaultStartAdapter(runtimeName string, checkout string) (runtime.Adapter, error) {
	return runtimeStartAdapter(newRuntimeFactory(), runtimeName, checkout)
}

func (w jobWorker) defaultWorkflow(checkout string) workflow.Engine {
	return daemonWorkflowEngine(w.Store, github.NewClient(checkout), checkout)
}

func (w jobWorker) defaultCommenter(_ string) github.Client {
	return github.NewClient("")
}

func daemonWorkflowEngine(store *db.Store, gh github.Client, checkout string) workflow.Engine {
	return workflow.Engine{
		Store:                   store,
		MergeGate:               daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout},
		ImplementationFinalizer: daemonImplementationFinalizer{Store: store, GitHub: gh, FallbackCheckout: checkout},
		PayloadRefresher: func(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
			return refreshDaemonJobPayload(ctx, store, checkout, job, payload)
		},
	}
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
	pr, err := f.githubClient(task.WorktreePath).CreatePullRequest(ctx, github.CreatePullRequestInput{
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
		if err := w.validateTargetCheckout(ctx, payload, checkout); err != nil {
			return "", err
		}
		if err := w.validateImplementationLock(ctx, payload, implementationLockOwner(agent, payload)); err != nil {
			return "", err
		}
	case "review":
		if payload.PullRequest > 0 && strings.TrimSpace(payload.TaskID) != "" {
			if err := w.validateReviewCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		} else {
			if err := w.validateTargetCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		}
	case "ask":
		if payload.PullRequest > 0 {
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
				return blockTaskForPermissionBlockedJob(ctx, w.Store, latest)
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
		var blocked workflow.BlockedError
		if errors.As(cause, &blocked) {
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: latest.ID, Kind: "advance_blocked", Message: cause.Error()})
		}
		if err := w.recordPostDeliveryWorkflowError(ctx, latest, cause); err != nil {
			return err
		}
		return nil
	}
	if latest.State == string(workflow.JobFailed) || latest.State == string(workflow.JobBlocked) {
		return nil
	}
	return cause
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
