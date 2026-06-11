package cli

import (
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/config"
)

// buildDashboardConfigView parses the config file into the read-only sections
// the Config page renders. Every parser tolerates a missing file (the daemon
// flags come from daemon.json). Cheap file reads, so it rides the snapshot.
func buildDashboardConfigView(paths config.Paths, daemon dashboardDaemonDetail) tui.ConfigView {
	view := tui.ConfigView{Path: paths.ConfigFile}

	view.Sections = append(view.Sections, tui.ConfigSection{
		Title: "paths",
		Rows: [][]string{
			{"database", paths.Database},
			{"logs", paths.Logs},
			{"workspaces", paths.Workspaces},
		},
	})

	if types, err := config.LoadAgentTypes(paths); err == nil && len(types) > 0 {
		names := make([]string, 0, len(types))
		for name := range types {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := [][]string{{"NAME", "RUNTIME", "TEMPLATE", "ROLE", "MAX_BG", "IDLE", "JOB"}}
		for _, name := range names {
			t := types[name]
			rows = append(rows, []string{
				name, t.Runtime, t.Template, dashConfig(t.Role),
				strconv.Itoa(t.MaxBackground), dashConfig(t.IdleTimeout), dashConfig(t.JobTimeout),
			})
		}
		view.Sections = append(view.Sections, tui.ConfigSection{Title: "agent types", Rows: rows})
	}

	if policy, err := config.LoadParallelSessionPolicy(paths); err == nil {
		view.Sections = append(view.Sections, tui.ConfigSection{
			Title: "parallel sessions",
			Rows: [][]string{
				{"same_session", policy.SameSession},
				{"merge_back", policy.MergeBack},
				{"max_temp_sessions_per_agent", strconv.Itoa(policy.MaxTempSessionsPerAgent)},
				{"eligible_actions", strings.Join(policy.EligibleActions, ", ")},
			},
		})
	}

	if repo, err := config.LoadDefaultFeedbackRepo(paths); err == nil {
		view.Sections = append(view.Sections, tui.ConfigSection{
			Title: "feedback",
			Rows:  [][]string{{"repo", dashConfig(repo)}},
		})
	}

	daemonRows := [][]string{}
	if len(daemon.Flags) > 0 {
		daemonRows = append(daemonRows, []string{"flags", strings.Join(daemon.Flags, " ")})
	}
	if daemon.WorkDir != "" {
		daemonRows = append(daemonRows, []string{"workdir", daemon.WorkDir})
	}
	if len(daemonRows) > 0 {
		view.Sections = append(view.Sections, tui.ConfigSection{Title: "daemon (persisted)", Rows: daemonRows})
	}

	return view
}

func dashConfig(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// validateDashboardConfig re-runs the config parsers and returns
// human-readable problems (empty when the file parses cleanly). It is the
// safety net after an external edit.
func validateDashboardConfig(paths config.Paths) []string {
	var problems []string
	if _, err := config.LoadAgentTypes(paths); err != nil {
		problems = append(problems, "[agents.*] "+err.Error())
	}
	if _, err := config.LoadParallelSessionPolicy(paths); err != nil {
		problems = append(problems, "[parallel_sessions] "+err.Error())
	}
	if _, err := config.LoadDefaultFeedbackRepo(paths); err != nil {
		problems = append(problems, "[feedback] "+err.Error())
	}
	return problems
}

// editConfigCmd opens the config file in $EDITOR (fallback vi) via
// tea.ExecProcess, which suspends the program for the editor and resumes on
// exit, delivering a tui.ConfigEditedMsg.
func editConfigCmd(configFile string) tea.Cmd {
	editor := strings.TrimSpace(os.Getenv("EDITOR"))
	if editor == "" {
		editor = "vi"
	}
	// Honor a multi-word EDITOR ("code --wait"), splitting on spaces.
	parts := strings.Fields(editor)
	args := append(parts[1:], configFile)
	cmd := exec.Command(parts[0], args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return tui.ConfigEditedMsg{Err: err}
	})
}
