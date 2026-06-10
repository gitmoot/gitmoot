package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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

// runSkillOptTrainRunTUI launches the bubbletea program. A var so dispatch tests
// can stub the actual run.
var runSkillOptTrainRunTUI = func(home, sessionID string, stdout, stderr io.Writer) int {
	program := tea.NewProgram(tui.NewTrainRun(skillOptTrainRunDeps(home, sessionID)), tea.WithOutput(stdout))
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(stderr, "skillopt train run: %v\n", err)
		return 1
	}
	return 0
}

func runSkillOptTrainRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	configPath := fs.String("config", "", "train init config path to resolve the latest session")
	sessionID := fs.String("session", "", "train session id")
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
		// No session yet for this config. Task 4 adds an interactive confirm
		// screen; for now point at train start.
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

// skillOptTrainRunDeps wires the snapshot loader into tui.TrainRunDeps.
func skillOptTrainRunDeps(home, sessionID string) tui.TrainRunDeps {
	return tui.TrainRunDeps{
		Interval: 2 * time.Second,
		Load: func() (tui.TrainRunSnapshot, error) {
			var snapshot skillOptTrainStatusSnapshot
			if err := withStore(home, func(store *db.Store) error {
				loaded, err := loadSkillOptTrainStatusSnapshot(context.Background(), store, sessionID, true)
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
	}
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
