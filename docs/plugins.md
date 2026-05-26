# Codex And Claude Plugins

Gitmoot plugins package the canonical Gitmoot Agent Skill for Codex and Claude
Code. They make the runtime aware of Gitmoot commands, safety rules, and
workflow expectations without changing Gitmoot's local-first architecture.

The Gitmoot CLI remains the engine. The daemon still polls GitHub pull
requests, local SQLite remains the workflow source of truth, and PR comments
remain the public audit trail.

## What Plugins Do

- Install Gitmoot's agent skill into a local runtime plugin package.
- Register a local marketplace named `gitmoot-local`.
- Help Codex or Claude discover Gitmoot workflow instructions.
- Point agents to the `gitmoot` CLI for status, jobs, locks, and daemon
  management.

## What Plugins Do Not Do

- They do not start a hosted service, webhook receiver, or cloud runner.
- They do not replace `gitmoot daemon start`.
- They do not install Codex, Claude Code, Git, or GitHub CLI.
- They do not silently subscribe agents or mutate repository state.

## Install Gitmoot

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot version
gh auth status
```

## Install The Codex Plugin

```sh
gitmoot plugin install codex
gitmoot plugin doctor codex
```

`plugin install codex` builds the Codex package under Gitmoot home, writes a
local Codex marketplace manifest, runs `codex plugin marketplace add`, and runs
`codex plugin add gitmoot@gitmoot-local` when the `codex` CLI is available.

Use `gitmoot plugin path codex` to print the generated package path.

## Install The Claude Plugin

```sh
gitmoot plugin install claude
gitmoot plugin doctor claude
```

`plugin install claude` builds the Claude package under Gitmoot home, validates
the package when the `claude` CLI is available, registers the local marketplace,
refreshes any existing installed copy, and installs
`gitmoot@gitmoot-local`.

Claude supports installation scopes:

```sh
gitmoot plugin install claude --scope user
gitmoot plugin install claude --scope project
gitmoot plugin install claude --scope local
```

Use `gitmoot plugin path claude` to print the generated package path.

## Verify

```sh
gitmoot plugin doctor
gitmoot plugin doctor codex
gitmoot plugin doctor claude
```

Doctor checks the canonical skill, generated package, manifest JSON, copied
skill, marketplace path, runtime CLI availability, and runtime validation where
supported.

## Use From Codex

After installing the Codex plugin, ask Codex to use the Gitmoot skill when the
task involves local PR-comment agent coordination:

```text
Use the Gitmoot skill. Check gitmoot status for this repo.
```

The agent should read the bundled skill, verify `gitmoot version`, check
`gh auth status` before PR workflows, and use read-only Gitmoot status commands
before mutating daemon, agent, job, or lock state.

For fast planning in the current Codex chat, do not route through a background
planner unless the user asks for a queued job. Ask Codex:

```text
Use the Gitmoot planner here. Write the implementation plan.
```

Codex should apply the packaged `presets/gitmoot-plan-lite.md` instructions,
inspect the relevant repo files, search only for current external contracts when
needed, and return the plan directly in the current conversation.

When you want the current Codex chat to invoke a registered background-capable
Gitmoot agent, route that request through the CLI:

```text
$gitmoot:gitmoot agent ask planner --repo owner/repo --background "Write the implementation plan and goal file."
```

Without the chat command bridge, ask Codex to run the same shell command:

```sh
gitmoot agent ask planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

This keeps background asks on the same Gitmoot agent registry, repo access,
runtime adapter, cached preset, and job history path as PR-comment ask jobs.

## Use From Claude Code

After installing the Claude plugin, ask Claude Code to use the Gitmoot skill for
the current workflow:

```text
Use the Gitmoot skill. Check gitmoot status for this repo.
```

Claude should use the bundled Gitmoot skill content as guidance, then call the
local `gitmoot` CLI only when the user asks for setup, status, agent
coordination, or PR-comment workflow help.

For a registered background-agent ask from Claude Code, use the same CLI
command:

```sh
gitmoot agent ask planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

The plugin is discovery and guidance. The `gitmoot` CLI is still the execution
path.

For fast current-chat planning, ask Claude Code to use the Gitmoot planner here
instead of starting a background `gitmoot agent ask` job.

## Troubleshooting

If the runtime CLI is missing, `gitmoot plugin install` keeps generated files
and prints manual install commands. Install the missing runtime, then rerun:

```sh
gitmoot plugin install codex
gitmoot plugin install claude
```

If a package looks stale, rebuild and reinstall:

```sh
gitmoot plugin install codex --force
gitmoot plugin install claude --force
```

If Claude validation fails, inspect the generated package and rerun validation
directly:

```sh
claude plugin validate "$(gitmoot plugin path claude)"
```

If Codex or Claude does not show the plugin after install, run:

```sh
gitmoot plugin doctor
gitmoot plugin path codex
gitmoot plugin path claude
```

Then confirm the runtime uses the same home directory as the shell where
`gitmoot plugin install` ran.
