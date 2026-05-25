# Local-First Coordination

Gitmoot is intentionally local-first. The user's machine owns runtime sessions,
checkout paths, local SQLite state, and daemon polling. GitHub stays the visible
audit trail.

## Core Pieces

- **Repo**: a GitHub repository plus the local checkout path Gitmoot may use.
- **Daemon**: the background process that polls GitHub and routes jobs.
- **SQLite state**: local records for repos, agents, jobs, tasks, locks, and PRs.
- **Runtime adapter**: a runtime-specific bridge for Codex, Claude Code, shell,
  or future agents.
- **Pull request comments**: the shared human-visible command and result log.

This keeps a beta deployment simple: no webhook receiver, hosted dashboard,
remote runner, or centralized control plane is required.

