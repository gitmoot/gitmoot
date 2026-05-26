---
name: gitmoot
description: Use Gitmoot for local-first AI agent coordination across repositories, goals, reviews, PR comments, daemon jobs, branch locks, presets, custom prompt agents, and Codex or Claude Code runtime workflows.
version: 0.1.0
license: Apache-2.0
compatibility: Requires the gitmoot CLI, git, GitHub CLI authentication, network access to GitHub, and a supported runtime such as Codex or Claude Code.
metadata:
  gitmoot-version: "0.1.0"
  source: "jerryfane/gitmoot"
  openclaw:
    requires:
      bins:
        - gitmoot
        - git
        - gh
    envVars:
      - name: GH_TOKEN
        required: false
        description: Optional GitHub token used by gh when GitHub CLI is not already authenticated.
---

# Gitmoot Agent Skill

This root `SKILL.md` is kept as a raw compatibility entrypoint for agents and
`gitmoot.io/SKILL.md`. The canonical Agent Skills package lives at
`skills/gitmoot/`, with deeper reference files under `skills/gitmoot/references/`.

Gitmoot is a local-first coordinator for AI agents working across repositories,
goals, reviews, PR comments, and runtime workflows. Use this skill when the
user wants PR-comment agent workflows, repo-scoped agent subscriptions,
background daemon checks, Codex or Claude Code agent startup, structured
implementation plans, standard goal files, preset agents, custom prompt agents,
job status, or branch lock inspection.

For fast planning, "use the Gitmoot planner here" means read and apply the
repo `skills/gitmoot/presets/gitmoot-plan-lite.md` instructions in this current
chat. In an installed Gitmoot skill package, use the packaged
`presets/gitmoot-plan-lite.md` path. Do not route that request through a
background `gitmoot agent ask` unless the user explicitly asks for background
execution, PR-comment routing, or job tracking.

## Before Acting

1. Check whether `gitmoot` is installed with `gitmoot version`.
2. Confirm GitHub CLI access with `gh auth status` before using PR workflows.
3. Detect or ask for the target repo before starting daemons, subscribing agents,
   or routing jobs.
4. Do not start daemons, create agents, update presets, or change subscriptions
   unless the user asks or the current task clearly requires it.
5. Prefer read-only status commands before mutating Gitmoot state.

## Install And Update

If `gitmoot` is not installed, install the latest beta:

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
```

Verify the install and GitHub access before using Gitmoot:

```sh
gitmoot version
gitmoot update --check
gh auth status
```

Install runtime plugin guidance when the user wants Codex or Claude Code to
discover Gitmoot from its plugin system:

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

## Common Local Commands

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
gitmoot daemon start --repo owner/repo --poll 30s
gitmoot daemon status
gitmoot plugin doctor
gitmoot agent list
gitmoot agent doctor <agent>
gitmoot agent ask <agent> --repo owner/repo "question or instructions"
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

Use `gitmoot daemon start` for the background daemon. Use `gitmoot daemon run`
only when the user explicitly wants a foreground process.

Use `gitmoot agent ask` when the user wants to invoke a registered Gitmoot
agent from the current local chat. Add `--background` only when the user wants a
queued background job. This is the same agent registry and runtime adapter path
used by PR-comment ask jobs; the plugin only helps the runtime discover this
skill and does not replace the `gitmoot` CLI.

## PR Comment Commands

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

## Preset Agents

Install or refresh the built-in thermo review preset:

```sh
gitmoot preset update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --preset thermo-nuclear-code-quality-review \
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

After editing a local prompt file, refresh Gitmoot's cached snapshot explicitly:

```sh
gitmoot preset diff frontend-reviewer
gitmoot preset update frontend-reviewer
```

## Agent Job Contract

Every Gitmoot job should return a concise and truthful `gitmoot_result` JSON
object:

```json
{
  "gitmoot_result": {
    "decision": "approved|changes_requested|blocked|implemented|failed",
    "summary": "Brief outcome.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": [],
    "next_agents": []
  }
}
```

Use `blocked` when work cannot continue without human input or external state.
Use `failed` when the attempted action errored. Do not report tests, changes, or
approvals that were not actually verified.

## Safety Rules

Preserve existing behavior unless the job explicitly changes it. Keep work
scoped to the target repo. Do not commit generated data, caches, logs, secrets,
session archives, cloned helper repos, or large outputs unless explicitly
requested. Respect Gitmoot branch locks for implementation jobs.

Implementation jobs must not edit or push an implementation branch unless
Gitmoot assigned the job and the branch lock is held by the assigned agent.
Review and ask jobs should inspect and report without mutating branches unless
the task explicitly instructs otherwise.

Redact secrets from GitHub comments, job summaries, raw examples, and copied
command output.

## When Unsure

Reread this `SKILL.md`, then inspect `/gitmoot help`, `gitmoot status`, and the
relevant job events before acting.
