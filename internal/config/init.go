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
# escalate_human failure_policy (#340): when a delegation pauses awaiting a human,
# the daemon @-tags escalation_handle (default: the repo owner) in a comment with
# the resume instructions. escalation_ttl auto-finalizes a never-answered pause
# (Go duration; default 24h). Both optional.
escalation_handle = ""
escalation_ttl = ""

# [admission] is an OPT-IN, off-by-default host-global concurrency budget the
# daemon applies BEFORE starting each agent session, on top of --workers/pool
# and the per-repo checkout / runtime-session locks (issue #365). With both caps
# 0 (the default, below) it is DISABLED and scheduling is byte-identical to a
# config with no [admission] section. Set max_concurrent_sessions to cap total
# in-flight sessions across all repos in the daemon process; set max_memory_gb to
# cap the summed per-runtime RAM estimate of in-flight sessions (a job is admitted
# only if it fits BOTH). A job that does not fit is left queued and retried next
# tick — never failed. The per-runtime *_memory_gb values are operator-tunable
# RAM priors; a non-session runtime contributes 0. Note: the budget is enforced
# per daemon process (host-global for the normal single-daemon deployment).
# [admission]
# max_concurrent_sessions = 0
# max_memory_gb = 0
# codex_memory_gb = 0.2
# claude_memory_gb = 0.85
# kimi_memory_gb = 0.5
# default_memory_gb = 0.5
`, paths.Database, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs)
}
