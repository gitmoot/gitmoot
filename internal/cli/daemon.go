package cli

import (
	"context"
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
	"syscall"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
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
	fmt.Fprintln(w, "  gitmoot daemon start [--repo owner/repo] [--poll 30s]")
	fmt.Fprintln(w, "  gitmoot daemon run [--repo owner/repo] [--poll 30s]")
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

	started, err := startDaemonChild(cfg.Home, cfg.Poll.String(), cfg.Workers, state, resolvedWorkDir)
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
		err := runRegisteredRepoSupervisor(ctx, *home, *poll, *workers, *dryRun, stdout)
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
			Store: store,
			MergeGate: workflow.PolicyMergeGate{
				Store:        store,
				GitHub:       gh,
				Git:          gitutil.Client{Dir: checkout},
				DeleteBranch: true,
			},
		}
		fmt.Fprintf(stdout, "watching %s every %s\n", repo.FullName(), poll.String())
		return (daemon.Daemon{
			Repo:         repo,
			PollInterval: *poll,
			Store:        store,
			GitHub:       gh,
			Workflow:     &engine,
		}).Run(ctx)
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
	Home                string
	RepoFlag            string
	Repo                github.Repository
	RepoSet             bool
	Poll                time.Duration
	Workers             int
	ExplicitStartConfig bool
	ExplicitRepo        bool
	ExplicitPoll        bool
	ExplicitWorkers     bool
}

const daemonHelp = -1

func parseDaemonStartConfig(command string, args []string, stderr io.Writer) (daemonStartConfig, int) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "GitHub repository as owner/repo")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	workers := fs.Int("workers", 1, "worker count")
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
		Home:     *home,
		RepoFlag: *repoFlag,
		Poll:     *poll,
		Workers:  *workers,
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

func startDaemonChild(home string, poll string, workers int, state daemonState, workDir string) (daemonMeta, error) {
	executable, err := os.Executable()
	if err != nil {
		return daemonMeta{}, err
	}
	logFile, err := os.OpenFile(state.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return daemonMeta{}, err
	}
	defer logFile.Close()
	args := daemonChildArgs(home, poll, workers)
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

func daemonChildArgs(home string, poll string, workers int) []string {
	args := []string{"daemon", "run", "--poll", poll, "--workers", strconv.Itoa(workers)}
	if home != "" {
		args = append(args, "--home", home)
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

func runRegisteredRepoSupervisor(ctx context.Context, home string, poll time.Duration, workers int, dryRun bool, stdout io.Writer) error {
	return withStore(home, func(store *db.Store) error {
		nextPoll := map[string]time.Time{}
		for {
			wait, err := pollRegisteredRepos(ctx, store, workers, dryRun, stdout, nextPoll, time.Now().UTC(), poll)
			if err != nil {
				return err
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	})
}

func pollRegisteredRepos(ctx context.Context, store *db.Store, workers int, dryRun bool, stdout io.Writer, nextPoll map[string]time.Time, now time.Time, fallbackPoll time.Duration) (time.Duration, error) {
	repos, err := store.ListRepos(ctx)
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
		dueAt := nextPoll[fullName]
		if !dueAt.IsZero() && dueAt.After(now) {
			wait = shorterWait(wait, dueAt.Sub(now), &waitSet)
			continue
		}
		polled++
		nextPoll[fullName] = now.Add(interval)
		wait = shorterWait(wait, interval, &waitSet)
		repo, err := daemon.ParseRepository(repoRecord.FullName())
		if err != nil {
			return wait, err
		}
		lastPollAt := now.Format(time.RFC3339)
		if strings.TrimSpace(repoRecord.CheckoutPath) == "" {
			message := "registered repo has no checkout path"
			writeLine(stdout, "%s: %s", repoRecord.FullName(), message)
			if err := store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, message); err != nil {
				return wait, err
			}
			continue
		}
		writeLine(stdout, "polling %s with %d workers dry_run=%t", repoRecord.FullName(), workers, dryRun)
		if dryRun {
			if err := store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, ""); err != nil {
				return wait, err
			}
			continue
		}
		gh := github.NewClient(repoRecord.CheckoutPath)
		engine := workflow.Engine{
			Store: store,
			MergeGate: workflow.PolicyMergeGate{
				Store:        store,
				GitHub:       gh,
				Git:          gitutil.Client{Dir: repoRecord.CheckoutPath},
				DeleteBranch: true,
			},
		}
		err = (daemon.Daemon{
			Repo:     repo,
			Store:    store,
			GitHub:   gh,
			Workflow: &engine,
		}).PollOnce(ctx)
		lastError := ""
		if err != nil {
			lastError = err.Error()
			writeLine(stdout, "%s: %s", repoRecord.FullName(), lastError)
		}
		if updateErr := store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, lastError); updateErr != nil {
			return wait, updateErr
		}
	}
	writeLine(stdout, "supervised %d enabled repos, polled %d", enabled, polled)
	if wait <= 0 {
		wait = fallbackPoll
	}
	return wait, nil
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
