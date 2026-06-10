package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// dashboardFlags is the subset of dashboard flags that decide whether the
// interactive TUI launches.
type dashboardFlags struct {
	plain      bool
	jsonOutput bool
	all        bool
	watch      bool
	answerID   string
	dismissID  string
}

// shouldLaunchTUI reports whether `gitmoot dashboard` should open the interactive
// TUI rather than print a one-shot snapshot. The TUI needs a real terminal on
// both stdout (to draw) and stdin (raw-mode keys), and must yield to every
// machine-output or one-shot mutation flag so existing behavior is preserved.
func shouldLaunchTUI(f dashboardFlags, stdoutTTY, stdinTTY bool) bool {
	if !stdoutTTY || !stdinTTY {
		return false
	}
	if f.plain || f.jsonOutput || f.all || f.watch {
		return false
	}
	if strings.TrimSpace(f.answerID) != "" || strings.TrimSpace(f.dismissID) != "" {
		return false
	}
	return true
}

// stdinIsCharDevice reports whether stdin is a terminal, so the TUI is not
// launched when stdin is piped or redirected (bubbletea needs raw-mode keys).
func stdinIsCharDevice() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// runDashboardTUI launches the bubbletea dashboard inside the Root router so
// pages can push full-screen child views (e.g. a train session's phase view).
// It returns a process exit code like the other dashboard paths.
func runDashboardTUI(home string, interval time.Duration, stdout, stderr io.Writer) int {
	deps := dashboardTUIDeps(home, interval)
	deps.OpenTrain = func(sessionID string) tea.Model {
		trainDeps := skillOptTrainRunDeps(home, func() string { return sessionID })
		trainDeps.Embedded = true
		return tui.NewTrainRun(trainDeps)
	}
	program := tea.NewProgram(
		tui.NewRoot(tui.New(deps)),
		tea.WithAltScreen(),
		tea.WithOutput(stdout),
	)
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}
	return 0
}

// dashboardTUIDeps adapts the existing snapshot builder and prompt store APIs
// into the tui.Deps the model consumes. Each Load reopens the store (cheap, pure
// DB) so the TUI always reflects current state, including live train phases.
func dashboardTUIDeps(home string, interval time.Duration) tui.Deps {
	return tui.Deps{
		Interval: interval,
		Load: func() (tui.Snapshot, error) {
			paths, err := initializedPaths(home)
			if err != nil {
				return tui.Snapshot{}, err
			}
			snapshot, err := buildDashboardSnapshot(home, paths)
			if err != nil {
				return tui.Snapshot{}, err
			}
			return toTUISnapshot(snapshot), nil
		},
		Answer: func(id, value string) error {
			return withStore(home, func(store *db.Store) error {
				_, err := store.AnswerInteractivePrompt(context.Background(), id, value, "dashboard-tui")
				return err
			})
		},
		Dismiss: func(id string) error {
			return withStore(home, func(store *db.Store) error {
				return store.DeleteInteractivePrompt(context.Background(), id)
			})
		},
		JobEvents: func(id string) ([]tui.JobEventView, error) {
			var views []tui.JobEventView
			err := withStore(home, func(store *db.Store) error {
				events, err := store.ListJobEvents(context.Background(), id)
				if err != nil {
					return err
				}
				for _, event := range events {
					views = append(views, tui.JobEventView{Kind: event.Kind, Message: event.Message})
				}
				return nil
			})
			return views, err
		},
		RetryJob: func(id string) error {
			// workflow.RetryJob is the same path `gitmoot job retry` takes: it
			// clears the stale payload.Result and emits the retry_queued event
			// the daemon's retry bookkeeping keys on.
			return withStore(home, func(store *db.Store) error {
				_, err := workflow.RetryJob(context.Background(), store, id)
				return err
			})
		},
		CancelJob: func(id string) error {
			return withStore(home, func(store *db.Store) error {
				_, err := workflow.CancelJob(context.Background(), store, id)
				return err
			})
		},
		StartDaemon: func() error {
			// Restart rather than start: it tolerates a stopped daemon and
			// restores the previously persisted flags (workers, poll, watch)
			// instead of silently reverting to defaults.
			var out bytes.Buffer
			if code := runDaemonRestart([]string{"--home", home}, &out, &out); code != 0 {
				return fmt.Errorf("daemon start failed: %s", strings.TrimSpace(out.String()))
			}
			return nil
		},
	}
}

// toTUISnapshot copies the cli dashboardSnapshot into the tui-facing Snapshot.
func toTUISnapshot(s dashboardSnapshot) tui.Snapshot {
	out := tui.Snapshot{
		Home:           s.Home,
		DatabaseExists: s.DatabaseExists,
		Daemon:         tui.Daemon{Running: s.Daemon.Running, PID: s.Daemon.PID, LogFile: s.Daemon.LogFile},
		Jobs:           tui.Jobs{Total: s.Jobs.Total, ByState: s.Jobs.ByState},
		Prompts:        s.promptDetails,
	}
	for _, r := range s.Repos {
		out.Repos = append(out.Repos, tui.Repo{Name: r.Name, Enabled: r.Enabled})
	}
	for _, a := range s.Agents {
		out.Agents = append(out.Agents, tui.Agent{Name: a.Name, Runtime: a.Runtime, Role: a.Role, Health: a.Health})
	}
	for _, sess := range s.RuntimeSessions {
		out.Sessions = append(out.Sessions, tui.Session{Name: sess.Name, Runtime: sess.Runtime, Repo: sess.Repo, State: sess.State})
	}
	for _, w := range s.Worktrees {
		out.Worktrees = append(out.Worktrees, tui.Worktree{Task: w.Task, Repo: w.Repo, Path: w.Path})
	}
	for _, l := range s.BranchLocks {
		out.BranchLocks = append(out.BranchLocks, tui.BranchLock{Repo: l.Repo, Branch: l.Branch, Owner: l.Owner})
	}
	for _, t := range s.TrainSessions {
		out.Trains = append(out.Trains, tui.TrainSession{ID: t.ID, Phase: t.Phase, Candidate: t.Candidate, Repo: t.Repo})
	}
	for _, l := range s.ResourceLocks {
		out.ResourceLocks = append(out.ResourceLocks, tui.ResourceLock{Key: l.Key, Owner: l.Owner, Stale: l.Stale})
	}
	for _, row := range s.jobRows {
		out.JobRows = append(out.JobRows, tui.JobRow{
			ID:          row.ID,
			Agent:       row.Agent,
			Type:        row.Type,
			State:       row.State,
			UpdatedAt:   row.UpdatedAt,
			LatestEvent: row.LatestEvent,
		})
	}
	return out
}
