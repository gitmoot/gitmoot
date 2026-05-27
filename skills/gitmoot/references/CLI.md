# Gitmoot CLI Reference

Use these commands from an agent session only when the user asks for Gitmoot
setup, status, agent coordination, or PR-comment workflow help.

## Install And Update

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot version
gitmoot update --check
gitmoot update --restart-daemon
```

Verify GitHub access before PR workflows:

```sh
gh auth status
```

## Runtime Plugins

Install Gitmoot's Agent Skill into Codex or Claude Code when the user wants the
runtime to discover Gitmoot workflow guidance from its plugin system:

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

Inspect or build packages without installing:

```sh
gitmoot plugin build codex
gitmoot plugin build claude
gitmoot plugin path codex
gitmoot plugin path claude
gitmoot plugin doctor codex
gitmoot plugin doctor claude
```

Claude scopes are supported with `--scope user|project|local`. Codex ignores
`--scope` because the current Codex plugin install command does not use it.

## Repo And Daemon Status

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
gitmoot daemon start --repo owner/repo --poll 30s --workers 1
gitmoot daemon status
gitmoot daemon logs
gitmoot daemon stop
```

Use `daemon start` for the background daemon. Use `daemon run` only when the
user explicitly wants a foreground process. Keep the default `--workers 1`
unless the Gitmoot home has multiple independent runtime sessions or managed
agent types with `max_background` greater than one.

## Agent Setup

Start a new runtime session managed by Gitmoot:

```sh
gitmoot agent start reviewer \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --role reviewer \
  --capability ask \
  --capability review \
  --start-daemon
```

Subscribe an existing runtime session:

```sh
gitmoot agent subscribe reviewer \
  --runtime codex \
  --session <session-id-or-last> \
  --repo owner/repo \
  --role reviewer \
  --capability ask \
  --capability review
```

Inspect agents:

```sh
gitmoot agent list
gitmoot agent repos reviewer
gitmoot agent doctor reviewer
```

Ask a registered agent from the current local chat:

```sh
gitmoot agent ask project-planner --repo owner/repo "Return the plan status."
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

This uses the same agent registry, repo access grants, cached template snapshot,
runtime adapter, and local job history as PR-comment ask jobs. The runtime
plugin helps Codex or Claude Code discover Gitmoot guidance, but it does not
replace `gitmoot agent ask`. Synchronous asks and queued jobs both use the same
runtime session locks.

Configure managed background agent types:

```sh
gitmoot agent type list
gitmoot agent type show planner
gitmoot agent type set planner --runtime codex --template planner-here --max-background 2 --idle-timeout 20m
gitmoot agent gc
```

## Agent Templates

Install or refresh the built-in thermo review template:

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --start-daemon
```

Install or refresh the built-in full planner/goal template:

```sh
gitmoot agent template update planner
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

Install or refresh the lightweight planner template when you want to run a
planner as a registered background-capable agent:

```sh
gitmoot agent template update planner-here
gitmoot agent start project-planner-lite \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner-here \
  --start-daemon
```

For fast current-chat planning, use the Gitmoot skill with the packaged
`agent-templates/planner-here.md` instructions instead of starting a background
job. The CLI cannot control an already-running Codex or Claude chat.

Create a local custom prompt template:

```sh
gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
gitmoot agent start frontend-reviewer \
  --runtime codex \
  --repo owner/repo \
  --template frontend-reviewer \
  --role reviewer \
  --capability ask \
  --capability review
```

After editing a local prompt file, refresh Gitmoot's cached snapshot:

```sh
gitmoot agent template diff frontend-reviewer
gitmoot agent template update frontend-reviewer
```

## Goals

Print the standard Gitmoot goal prompt template:

```sh
gitmoot goal template
```

Import a goal file into local Gitmoot state:

```sh
gitmoot goal import --file GOAL-feature.md --repo owner/repo
```

## PR Comments

Use GitHub PR comments as the public audit trail:

```text
/gitmoot help
/gitmoot status
/gitmoot <agent> review [instructions]
/gitmoot <agent> implement [instructions]
/gitmoot ask <agent> [question]
/gitmoot retry <job-id>
/gitmoot cancel <job-id>
/gitmoot merge
```

## Jobs And Locks

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job watch <job-id>
gitmoot job events <job-id>
gitmoot job retry <job-id>
gitmoot job cancel <job-id>
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```
