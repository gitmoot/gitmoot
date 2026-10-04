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
- Include a compact local snapshot of daemon, task, job, and lock state when
  the hook can read the local Gitmoot store.
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

If Codex is sandboxed outside Gitmoot home, give the runtime explicit access to
the resolved `.gitmoot` directory:

```sh
gitmoot plugin codex-launch --repo .
```

The command prints a launch line like:

```sh
codex-face --cd /path/to/repo --add-dir /home/user/.gitmoot -s workspace-write
```

On Windows it prints PowerShell-safe quoting. For persistent Codex config,
print the matching snippet:

```sh
gitmoot plugin codex-launch --config-snippet
```

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

Doctor checks the canonical skill, generated package, plugin manifest JSON,
hook manifest JSON, copied skill, marketplace path, runtime CLI availability,
and runtime validation where supported.

## Presence Hooks

Generated Codex and Claude packages include a `SessionStart` command hook and
the inbox turn hooks described below. On
startup, resume, clear, and compact events, the runtime runs
`gitmoot plugin hook-context` with a 5-second timeout and passes the hook event
JSON on stdin. The command reads the session working directory when available,
uses local Git and Gitmoot metadata, and returns
`hookSpecificOutput.additionalContext` for the agent.

The hook is read-only context, not a control surface. When the working
directory belongs to a GitHub repo, it tries to open the local Gitmoot SQLite
database read-only and injects a compact "Current snapshot" with daemon,
task, job, and branch-lock counts for that repo. If the snapshot is unavailable,
the hook fails open and still provides the basic repo/context guidance.

Agents should answer Gitmoot health and status questions from the injected
snapshot when it is sufficient. For more detail they should run relevant
read-only CLI checks:

- `gitmoot status --repo owner/repo`
- `gitmoot task list --repo owner/repo --json`
- `gitmoot job list --repo owner/repo`
- `gitmoot lock list --repo owner/repo`
- `gitmoot dashboard --json`

They should not use nonexistent commands such as `gitmoot status --json` or
`gitmoot task show`. Agents should mention `gitmoot dashboard` only after the
direct answer, as a live monitoring follow-up for humans.

Role split:

- Hook: lightweight startup context that fails open when context is unavailable.
- Agent skill: guidance for choosing safe Gitmoot workflows and commands.
- Gitmoot CLI: source of truth for status, jobs, locks, agents, plugin doctor,
  and explicit actions.
- Dashboard: live monitoring for humans, not a substitute for an agent answer.

Hooks run local commands with the permissions of your runtime session. Review
the generated hook commands before enabling or trusting them. For Codex, plugin
hooks are skipped until you review and trust the current hook definitions in
`/hooks`; rebuilding or reinstalling the plugin changes the hash and asks again.
The `SessionStart` command is limited to `gitmoot plugin hook-context` and does
not mutate Gitmoot or repository state. The inbox hooks below run only
`gitmoot message pending`, whose sole write is marking the seat's own delivered
notifications.

## Inbox Turn Hooks

Claude Code and Codex cannot be woken safely from outside, so Gitmoot never
types into their terminals. Instead the generated packages add three command
hooks that deliver the seat's inbox mail at turn boundaries:

| Event | When it fires | Claude Code output | Codex output |
| --- | --- | --- | --- |
| `UserPromptSubmit` | the operator (or a scheduled prompt) starts a turn | `hookSpecificOutput.additionalContext` | `hookSpecificOutput.additionalContext` |
| `PostToolUse` (every tool) | after each tool call during a turn | `hookSpecificOutput.additionalContext` | `hookSpecificOutput.additionalContext` |
| `Stop` | the turn is about to end | `hookSpecificOutput.additionalContext`, which continues the turn | `decision: "block"` with the mail as `reason`, which Codex runs as a continuation prompt |

Each hook runs
`gitmoot message pending --claim --hook <event> --runtime claude|codex` with a
15-second timeout and no status message. The acting role comes from
`GITMOOT_ORG_ROLE`, else the registered Herdr pane in `HERDR_PANE_ID`; a
session with neither prints nothing. In one transaction the command moves that
role's queued notifications to submitted with a
`turn-hook:<runtime>:<event>` receipt, so each item is shown once even when
hooks race, then lists each message's ID, kind, sender, a short scrubbed
preview and `gitmoot message show ID`. At most 10 items are listed per hook;
the rest wait for the next hook point. Directive notifications also record the
same delivery receipt the daemon writes. Routing policy is the daemon's: a
muted or unroutable notification is left queued. Notifications already being
submitted or marked uncertain are never touched.

The command always exits 0. Any problem is reported on stderr, which neither
runtime shows to the model, and stdout stays empty so the turn proceeds.

What each case looks like:

- Mail sent while the seat is idle is seen when the operator next submits a
  prompt. Neither runtime has a hook that starts a turn on its own.
- Mail sent while the seat is working is seen after its next tool call.
- Mail that arrives after the last tool call is seen at `Stop`, which continues
  the turn once. A `Stop` that is already a stop-hook continuation
  (`stop_hook_active: true`) prints nothing and leaves mail queued for the next
  prompt, so the hook cannot loop. Claude Code also caps consecutive stop-hook
  continuations at eight.
- Hooks fired inside a subagent (`agent_id` in the hook input) print nothing,
  because a subagent's context is not the seat's conversation.
- Text is limited by the runtimes: Claude Code saves `additionalContext` over
  10,000 characters to a file, and Codex spills it over about 2,500 tokens.
  Ten short previews stay well below both.
- A turn interrupted with Esc does not run `Stop`; its mail waits for the next
  hook point.
- Once claimed, mail is marked submitted even if the runtime then discards the
  hook output, for example on its own timeout. It is not resent. It remains in
  `gitmoot message inbox`.

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

When you want the current Codex chat to invoke a registered background-capable
Gitmoot agent, route that request through the CLI:

```text
$gitmoot:gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan."
```

Without the chat command bridge, ask Codex to run the same shell command:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan."
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
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan."
gitmoot job watch <job-id>
```

The plugin is discovery and guidance. The `gitmoot` CLI is still the execution
path.

If Claude Code needs an OAuth token or other runtime credential, create it with
the Claude CLI and install it with `gitmoot auth set claude`. Gitmoot reads the
owner-only credential file on each delivery, so no daemon restart is needed.
Never paste tokens into PR comments, issue bodies, tracked files, generated
plugin packages, or logs.

### Claude background auth

Claude background jobs run **non-interactively**. Store their long-lived token
in Gitmoot's authoritative owner-only file:

```sh
claude setup-token
gitmoot auth set claude
gitmoot auth status
gitmoot auth probe claude
```

`auth set` reads stdin without echo on a TTY and atomically updates
`~/.gitmoot/runtime-auth.env` (mode `0600`). Rotation is visible on the next
delivery. `auth status` is local and masked; `auth probe` and `gitmoot doctor`
perform fresh live checks. Clear credentials with `gitmoot auth unset claude`,
which writes an explicit-empty file. For systemd deployments, keep only
operational settings such as `PATH` in `daemon.env`; Claude secrets belong in
`runtime-auth.env`.

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

If a daemon or runtime was already running when environment variables changed,
restart it before retrying a job. Plugin discovery confirms instructions are
installed; it does not prove runtime auth, GitHub auth, or model credentials are
available to long-lived processes.
