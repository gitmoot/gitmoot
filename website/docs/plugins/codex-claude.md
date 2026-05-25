# Codex And Claude Plugins

Gitmoot plugins package the Gitmoot Agent Skill for Codex and Claude Code. They
help runtimes discover Gitmoot commands, safety rules, and workflow
expectations.

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

The plugins do not replace the CLI. Use the CLI for agent registration, daemon
management, status checks, and direct agent asks.

From a runtime chat that supports command execution:

```text
$gitmoot:gitmoot agent ask planner --repo owner/repo "Write the implementation plan and goal file."
```

Without the command bridge, ask the runtime to run:

```sh
gitmoot agent ask planner --repo owner/repo "Write the implementation plan and goal file."
```

