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
gitmoot daemon start --repo owner/repo --poll 30s
gitmoot daemon status
gitmoot daemon logs
gitmoot daemon stop
```

Use `daemon start` for the background daemon. Use `daemon run` only when the
user explicitly wants a foreground process.

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

## Presets

Install or refresh the built-in thermo review preset:

```sh
gitmoot preset update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --preset thermo-nuclear-code-quality-review \
  --start-daemon
```

Install or refresh the built-in planner preset:

```sh
gitmoot preset update gitmoot-plan-and-goal
gitmoot agent start planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --preset gitmoot-plan-and-goal \
  --start-daemon
```

Create a local custom prompt preset:

```sh
gitmoot preset add frontend-reviewer --file agents/frontend-reviewer.md
gitmoot agent start frontend-reviewer \
  --runtime codex \
  --repo owner/repo \
  --preset frontend-reviewer \
  --role reviewer \
  --capability ask \
  --capability review
```

After editing a local prompt file, refresh Gitmoot's cached snapshot:

```sh
gitmoot preset diff frontend-reviewer
gitmoot preset update frontend-reviewer
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
gitmoot job events <job-id>
gitmoot job retry <job-id>
gitmoot job cancel <job-id>
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```
