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
- Add a read-only `SessionStart` presence hook that provides local Gitmoot
  context when the runtime supports hooks.
- Point agents to the `gitmoot` CLI for status, jobs, locks, and daemon
  management.

## What Plugins Do Not Do

- They do not start a hosted service, webhook receiver, or cloud runner.
- They do not replace `gitmoot daemon start`.
- They do not install Codex, Claude Code, Git, or GitHub CLI.
- They do not silently subscribe agents or mutate repository state.
- Their startup hook does not start daemons, poll GitHub, create jobs, release
  locks, or act as a slash-command/control surface.

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

## Presence Hooks

Generated Codex and Claude packages include a `SessionStart` command hook. On
startup, resume, clear, and compact events, the runtime runs
`gitmoot plugin hook-context` with a 5-second timeout and passes the hook event
JSON on stdin. The command reads the session working directory when available,
uses local Git and Gitmoot metadata, and returns
`hookSpecificOutput.additionalContext` for the agent.

The hook is read-only context, not a control surface. It helps agents answer
Gitmoot health and status questions directly by running relevant read-only CLI
checks. Agents should mention `gitmoot dashboard` only after the direct answer,
as a live monitoring follow-up for humans.

Role split:

- Hook: lightweight startup context that fails open when context is unavailable.
- Agent skill: guidance for choosing safe Gitmoot workflows and commands.
- Gitmoot CLI: source of truth for status, jobs, locks, agents, plugin doctor,
  and explicit actions.
- Dashboard: live monitoring for humans, not a substitute for an agent answer.

Hooks run local commands with the permissions of your runtime session. Review
the generated hook command before enabling or trusting it. For Codex, plugin
hooks are skipped until you review and trust the current hook definition. The
expected Gitmoot command is limited to `gitmoot plugin hook-context` and does
not mutate Gitmoot or repository state.

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

Codex should apply the same `planner` template used by managed planner agents,
inspect the relevant repo files, search only for current external contracts when
needed, and return the plan directly in the current conversation.

For any cached custom agent or template prompt, ask Codex to use that agent
here. The Gitmoot skill should load the prompt with:

```sh
gitmoot agent prompt <agent-or-template>
```

Then Codex should apply the returned prompt content in the current chat without
creating a Gitmoot job.

For template capture, keep the work in the current chat. Ask Codex to read the
Gitmoot template-capture instructions and draft a file from visible context:

```text
Use Gitmoot to capture this session as agent template release-planner. Draft only.
```

Codex should not call `gitmoot agent ask`, start a daemon, or install the
template unless the user explicitly asks. After review, install the draft with:

```sh
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add release-planner --file .gitmoot/templates/release-planner.md
```

When you want the current Codex chat to invoke a registered background-capable
Gitmoot agent, route that request through the CLI:

```text
$gitmoot:gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
```

Without the chat command bridge, ask Codex to run the same shell command:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

This keeps background asks on the same Gitmoot agent registry, repo access,
runtime adapter, cached template, and job history path as PR-comment ask jobs.

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
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

The plugin is discovery and guidance. The `gitmoot` CLI is still the execution
path.

For fast current-chat planning, ask Claude Code to use the Gitmoot planner here
instead of starting a background `gitmoot agent ask` job.

For current-chat template capture, ask Claude Code:

```text
Use Gitmoot to capture this session as agent template release-planner. Draft only.
```

Claude Code should apply the bundled template-capture instructions locally,
write or return a draft, and wait for explicit approval before validation or
installation.

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
