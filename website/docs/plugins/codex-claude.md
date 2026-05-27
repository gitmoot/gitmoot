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
management, status checks, and background agent asks.

For fast planning in the current chat, ask the runtime:

```text
Use the Gitmoot planner here. Write the implementation plan.
```

For registered background-agent work from a runtime chat that supports command
execution:

```text
$gitmoot:gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
```

Without the command bridge, ask the runtime to run:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```
