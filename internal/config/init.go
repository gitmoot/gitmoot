package config

import (
	"fmt"
	"os"
)

func Initialize(paths Paths) error {
	for _, dir := range []string{paths.Home, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}

	if _, err := os.Stat(paths.ConfigFile); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config file: %w", err)
	}

	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)), 0o600); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

func DefaultConfig(paths Paths) string {
	return fmt.Sprintf(`# Gitmoot local configuration.

[paths]
database = %q
logs = %q
workspaces = %q
evals = %q
artifact_blobs = %q

[parallel_sessions]
same_session = "fork_temp_session"
merge_back = "summary"
max_temp_sessions_per_agent = 4
eligible_actions = ["ask", "review", "implement"]

[orchestrate]
# Render one live herdr pane per delegation subagent when a job opts in with
# --cockpit. cockpit_mode: on | off | auto (auto gates on herdr reachability).
# cockpit_max_panes caps concurrent panes (constrained hosts ~4); beyond the cap
# a job runs status-only with no pane. cockpit_pane_key: job (one pane per job)
# or seat (reuse one pane per seat). cockpit_session is an optional named session.
cockpit_mode = "auto"
cockpit_session = ""
cockpit_max_panes = 4
cockpit_pane_key = "job"
`, paths.Database, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs)
}
