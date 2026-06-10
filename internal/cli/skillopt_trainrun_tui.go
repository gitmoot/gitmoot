package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// skillOptTrainRunTUICapable reports whether the train run TUI may launch: both
// stdin and stdout must be terminals and the user must not have opted out. A var
// so tests can stub it.
var skillOptTrainRunTUICapable = func() bool {
	if os.Getenv("GITMOOT_NO_TUI") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	in, errIn := os.Stdin.Stat()
	out, errOut := os.Stdout.Stat()
	return errIn == nil && errOut == nil &&
		in.Mode()&os.ModeCharDevice != 0 && out.Mode()&os.ModeCharDevice != 0
}

// runSkillOptTrainRunTUI launches the bubbletea program for an existing session.
// A var so dispatch tests can stub the actual run.
var runSkillOptTrainRunTUI = func(home, sessionID string, stdout, stderr io.Writer) int {
	deps := skillOptTrainRunDeps(home, func() string { return sessionID })
	program := tea.NewProgram(tui.NewTrainRun(deps), tea.WithOutput(stdout))
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(stderr, "skillopt train run: %v\n", err)
		return 1
	}
	return 0
}

// runSkillOptTrainRunConfirmTUI launches the bubbletea program on the confirm
// screen for a config with no session yet: it creates the session (via
// train start --yes) and then shows the live phase view. A var so tests can stub it.
var runSkillOptTrainRunConfirmTUI = func(home, configPath, workspaceRepo string, stdout, stderr io.Writer) int {
	plan, err := buildSkillOptTrainRunPlan(configPath, workspaceRepo)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train run: %v\n", err)
		return 1
	}
	created := &skillOptTrainRunSessionRef{}
	deps := skillOptTrainRunDeps(home, created.get)
	deps.Plan = plan
	deps.CreateSession = func(ws string) (string, error) {
		id, err := createSkillOptTrainRunSession(home, configPath, ws, stderr)
		if err != nil {
			return "", err
		}
		created.set(id)
		return id, nil
	}
	program := tea.NewProgram(tui.NewTrainRun(deps), tea.WithOutput(stdout))
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(stderr, "skillopt train run: %v\n", err)
		return 1
	}
	return 0
}

// skillOptTrainRunSessionRef holds the session id the confirm flow creates, read
// concurrently by tea.Cmd goroutines (Load/Continue/…).
type skillOptTrainRunSessionRef struct {
	mu sync.Mutex
	id string
}

func (r *skillOptTrainRunSessionRef) get() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id
}

func (r *skillOptTrainRunSessionRef) set(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.id = id
}

func runSkillOptTrainRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	configPath := fs.String("config", "", "train init config path to resolve the latest session")
	sessionID := fs.String("session", "", "train session id")
	workspaceRepo := fs.String("workspace-repo", "", "workspace repository to use when creating a session from --config")
	plain := fs.Bool("plain", false, "print a one-shot status snapshot instead of the interactive TUI")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train run does not accept positional arguments")
		return 2
	}

	resolved, err := resolveSkillOptTrainRunSession(*home, *sessionID, *configPath)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train run: %v\n", err)
		return 1
	}
	if resolved == "" {
		// No session yet for this config. On a terminal, open the confirm screen
		// to create one; otherwise point at train start.
		if !*plain && strings.TrimSpace(*configPath) != "" && skillOptTrainRunTUICapable() {
			return runSkillOptTrainRunConfirmTUI(*home, *configPath, strings.TrimSpace(*workspaceRepo), stdout, stderr)
		}
		writeLine(stdout, "no train session yet for this config")
		if strings.TrimSpace(*configPath) != "" {
			writeLine(stdout, "next: gitmoot skillopt train start --config %s --workspace-repo <owner/repo>", *configPath)
		}
		return 0
	}

	if !*plain && skillOptTrainRunTUICapable() {
		return runSkillOptTrainRunTUI(*home, resolved, stdout, stderr)
	}
	return runSkillOptTrainRunPlain(*home, resolved, stdout, stderr)
}

// runSkillOptTrainRunPlain prints a one-shot status snapshot plus the
// continue-from-github link and a next-step hint, for non-terminal/--plain use.
func runSkillOptTrainRunPlain(home, sessionID string, stdout, stderr io.Writer) int {
	var snapshot skillOptTrainStatusSnapshot
	if err := withStore(home, func(store *db.Store) error {
		loaded, err := loadSkillOptTrainStatusSnapshot(context.Background(), store, sessionID, true)
		if err != nil {
			return err
		}
		snapshot = loaded
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train run: %v\n", err)
		return 1
	}
	printSkillOptTrainStatusSnapshot(stdout, skillOptTrainStatusOutputSnapshot(snapshot, false), false)
	writeLine(stdout, "next: gitmoot skillopt train continue --session %s", sessionID)
	return 0
}

// resolveSkillOptTrainRunSession returns the session id to show. An explicit
// --session wins; otherwise --config resolves the newest session matching the
// config's template. Returns "" when a config has no session yet.
func resolveSkillOptTrainRunSession(home, sessionID, configPath string) (string, error) {
	if id := strings.TrimSpace(sessionID); id != "" {
		return id, nil
	}
	if strings.TrimSpace(configPath) == "" {
		return "", errors.New("requires --session or --config")
	}
	config, err := skillopt.LoadTrainInitConfig(configPath)
	if err != nil {
		return "", err
	}
	template := strings.TrimSpace(config.Template)
	var resolved string
	if err := withStore(home, func(store *db.Store) error {
		sessions, err := store.ListSkillOptTrainSessions(context.Background())
		if err != nil {
			return err
		}
		matching := make([]db.SkillOptTrainSession, 0, len(sessions))
		for _, session := range sessions {
			if template == "" || session.TemplateID == template {
				matching = append(matching, session)
			}
		}
		// Newest first by CreatedAt, then id as a stable tiebreaker.
		sort.Slice(matching, func(i, j int) bool {
			if matching[i].CreatedAt != matching[j].CreatedAt {
				return matching[i].CreatedAt > matching[j].CreatedAt
			}
			return matching[i].ID > matching[j].ID
		})
		if len(matching) > 0 {
			resolved = matching[0].ID
		}
		return nil
	}); err != nil {
		return "", err
	}
	return resolved, nil
}

// skillOptTrainRunDeps wires the snapshot loader and phase actions into
// tui.TrainRunDeps. sessionID is a getter so the confirm flow (which creates the
// session mid-program) and the resolved-session flow can share one builder.
func skillOptTrainRunDeps(home string, sessionID func() string) tui.TrainRunDeps {
	runContinue := func(mutate func(*skillOptTrainContinueRequest)) (tui.TrainRunActionResult, error) {
		paths, err := initializedPaths(home)
		if err != nil {
			return tui.TrainRunActionResult{}, err
		}
		var out skillOptTrainContinueOutput
		err = withStore(home, func(store *db.Store) error {
			request := skillOptTrainContinueRequest{Home: home, SessionID: sessionID()}
			if mutate != nil {
				mutate(&request)
			}
			var runErr error
			out, runErr = continueSkillOptTrain(context.Background(), paths, store, request)
			return runErr
		})
		return tui.TrainRunActionResult{Lines: out.Lines}, err
	}
	return tui.TrainRunDeps{
		Interval: 2 * time.Second,
		Load: func() (tui.TrainRunSnapshot, error) {
			var snapshot skillOptTrainStatusSnapshot
			if err := withStore(home, func(store *db.Store) error {
				loaded, err := loadSkillOptTrainStatusSnapshot(context.Background(), store, sessionID(), true)
				if err != nil {
					return err
				}
				snapshot = loaded
				return nil
			}); err != nil {
				return tui.TrainRunSnapshot{}, err
			}
			return toTrainRunSnapshot(snapshot), nil
		},
		Continue: func() (tui.TrainRunActionResult, error) { return runContinue(nil) },
		Decide: func(promote bool, candidate, reason string) (tui.TrainRunActionResult, error) {
			return runContinue(func(r *skillOptTrainContinueRequest) {
				if promote {
					r.PromoteCandidate = candidate
				} else {
					r.RejectCandidate = candidate
					r.DecisionReason = reason
				}
			})
		},
		StartNext: func() (tui.TrainRunActionResult, error) {
			return runContinue(func(r *skillOptTrainContinueRequest) { r.StartNext = true })
		},
		SpawnContinue: func() (string, error) { return spawnSkillOptTrainContinueChild(home, sessionID()) },
		TailLog: func(offset int64) ([]string, int64, error) {
			return tailSkillOptTrainLog(home, sessionID(), offset)
		},
	}
}

// tailSkillOptTrainLog reads new complete lines from the current session's
// detached-child log starting at offset, returning them and the next offset. A
// missing log (the child hasn't started) yields no lines. Partial trailing lines
// are left for the next read.
func tailSkillOptTrainLog(home, sessionID string, offset int64) ([]string, int64, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, offset, nil
	}
	paths, err := initializedPaths(home)
	if err != nil {
		return nil, offset, err
	}
	path := filepath.Join(paths.Logs, "skillopt-train-"+skillOptTrainLogSlug(sessionID)+".log")
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, nil // no log yet
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := info.Size()
	if offset > size {
		offset = 0 // truncated/rotated — restart
	}
	if offset >= size {
		return nil, offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	data := make([]byte, size-offset)
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, offset, err
	}
	data = data[:n]
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		return nil, offset, nil // no complete line yet
	}
	lines := []string{}
	for _, line := range strings.Split(string(data[:lastNL]), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, offset + int64(lastNL+1), nil
}

// buildSkillOptTrainRunPlan loads the config into a confirm-screen plan.
func buildSkillOptTrainRunPlan(configPath, workspaceRepo string) (*tui.TrainRunPlan, error) {
	config, err := skillopt.LoadTrainInitConfig(configPath)
	if err != nil {
		return nil, err
	}
	template := strings.TrimSpace(config.Template)
	if v := strings.TrimSpace(config.TemplateVersion); v != "" {
		template += " @" + strings.TrimPrefix(v, strings.TrimSpace(config.Template)+"@")
	}
	return &tui.TrainRunPlan{
		Name:              strings.TrimSpace(config.Name),
		Template:          template,
		ReviewRepo:        strings.TrimSpace(config.ReviewRepo),
		WorkspaceRepo:     strings.TrimSpace(workspaceRepo),
		NeedWorkspaceRepo: strings.TrimSpace(workspaceRepo) == "",
	}, nil
}

// createSkillOptTrainRunSession creates a session from a config by invoking the
// real `train start --yes` in process (no plan-building duplication), capturing
// its output so it does not corrupt the TUI. Missing repos are created
// (--create-repos). Returns the new session id.
func createSkillOptTrainRunSession(home, configPath, workspaceRepo string, stderr io.Writer) (string, error) {
	if strings.TrimSpace(workspaceRepo) == "" {
		return "", errors.New("workspace repo is required")
	}
	config, err := skillopt.LoadTrainInitConfig(configPath)
	if err != nil {
		return "", err
	}
	sessionID := generatedSkillOptTrainSessionID(config.Template)
	args := []string{
		"--home", home,
		"--config", configPath,
		"--workspace-repo", workspaceRepo,
		"--session", sessionID,
		"--create-repos",
		"--yes",
	}
	var buf bytes.Buffer
	if code := runSkillOptTrainStart(args, &buf, &buf); code != 0 {
		return "", fmt.Errorf("train start failed: %s", strings.TrimSpace(buf.String()))
	}
	return sessionID, nil
}

// spawnSkillOptTrainContinueChild launches `gitmoot skillopt train continue` as a
// detached background process so the long generation/optimizer phases survive
// the TUI quitting. It is a var so tests can stub it. Returns the log path the
// child's stdout/stderr are appended to.
var spawnSkillOptTrainContinueChild = func(home, sessionID string) (string, error) {
	paths, err := initializedPaths(home)
	if err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	logPath := filepath.Join(paths.Logs, "skillopt-train-"+skillOptTrainLogSlug(sessionID)+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer logFile.Close()
	args := []string{"skillopt", "train", "continue", "--session", sessionID}
	if strings.TrimSpace(home) != "" {
		args = append(args, "--home", home)
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	if err := cmd.Process.Release(); err != nil {
		return "", err
	}
	return logPath, nil
}

// skillOptTrainLogSlug makes a session id safe for a log filename.
func skillOptTrainLogSlug(sessionID string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, sessionID)
}

// toTrainRunSnapshot maps the cli status snapshot to the tui-facing shape.
func toTrainRunSnapshot(s skillOptTrainStatusSnapshot) tui.TrainRunSnapshot {
	// TemplateVersion is the full version id (e.g. "planner@v3"); strip the
	// redundant "<id>@" prefix so the label reads "planner @v3" rather than
	// doubling the template id.
	template := strings.TrimSpace(s.TemplateID)
	if v := strings.TrimSpace(s.TemplateVersion); v != "" {
		template += " @" + strings.TrimPrefix(v, strings.TrimSpace(s.TemplateID)+"@")
	}
	out := tui.TrainRunSnapshot{
		SessionID:         s.SessionID,
		IterationID:       s.IterationID,
		Template:          template,
		ReviewRepo:        strings.TrimSpace(s.TargetRepo),
		WorkspaceRepo:     strings.TrimSpace(s.WorkspaceRepo),
		Phase:             s.StatusPhase,
		NextAction:        s.NextAction,
		IssueURL:          skillOptTrainContinueFromGitHubURL(s.CurrentPhase, s.IssueURL),
		CandidateVersion:  s.CandidateVersion,
		NoCandidateReason: s.NoCandidateReason,
		ReviewItems:       s.Counts.ReviewItems,
		FeedbackCount:     s.Counts.FeedbackEvents + s.Counts.RankedFeedbackEvents,
		GeneratedOptions:  s.Progress.GeneratedOptions,
		ETA:               s.Progress.ETA,
		Terminal:          skillOptTrainRunTerminalPhase(s.StatusPhase),
	}
	if s.Verbose != nil {
		out.Elapsed = s.Verbose.Elapsed
		out.JobsRunning = s.Verbose.Jobs.Running
		out.JobsSucceeded = s.Verbose.Jobs.Succeeded
		out.JobsFailed = s.Verbose.Jobs.Failed
	}
	return out
}

func skillOptTrainRunTerminalPhase(phase string) bool {
	switch phase {
	case "candidate_promoted", "candidate_rejected", "run_abandoned", "optimizer_completed_no_candidate":
		return true
	default:
		return false
	}
}
