---
name: gitmoot-agent
description: Use Gitmoot to coordinate local AI agent sessions through GitHub pull requests, including PR commands, repo access, branch locks, job results, and safe agent behavior.
version: 0.1.0
metadata:
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

Gitmoot is a local-first coordinator for AI agents working through GitHub pull
requests. A local `gitmoot daemon` watches PR comments, routes jobs to allowed
agents, records job state in local SQLite, and posts attributed results back to
the PR.

## Install Gitmoot

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

## Core Workflow

Use GitHub PR comments as the public audit trail. Local Gitmoot state is the
workflow source of truth.

Common PR commands:

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

When unsure which agents or commands are available, ask for `/gitmoot help` or
run local status commands before acting.

## Thermo Review Preset

Gitmoot can register the built-in `thermo-nuclear-code-quality-review` preset
as a strict review-only agent. Preset content is updated explicitly and cached
locally; queued jobs keep the preset snapshot they were created with.

```sh
gitmoot preset update thermo-nuclear-code-quality-review
gitmoot agent subscribe thermo-review \
  --runtime codex \
  --session <session-id-or-last> \
  --repo owner/repo \
  --preset thermo-nuclear-code-quality-review
gitmoot agent doctor thermo-review
```

Use it from a PR comment:

```text
/gitmoot thermo-review review
```

Check upstream changes before refreshing the cached preset:

```sh
gitmoot preset diff thermo-nuclear-code-quality-review
gitmoot preset update thermo-nuclear-code-quality-review
```

## Custom Prompt Presets

Users can install local prompt files as custom presets. Gitmoot snapshots the
file content into local SQLite at add/update time; queued jobs use that cached
snapshot, not the live file.

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

Use `agent start` to create a new Codex or Claude runtime session. Use
`agent subscribe` when the runtime session already exists. Custom presets do
not provide default role or capabilities for subscribed agents, so pass
`--role` and one or more `--capability` flags.

After editing a local prompt file, refresh the cached snapshot explicitly:

```sh
gitmoot preset diff frontend-reviewer
gitmoot preset update frontend-reviewer
```

## Required Result Contract

Every agent job must return a `gitmoot_result` JSON object. Keep it concise and
truthful.

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
Use `failed` when the attempted action errored. Do not report tests or changes
that were not actually run or made.

## Repo Access And Locks

Agents are global identities with explicit per-repo access. A PR's repository is
the routing context for `/gitmoot <agent> ...`.

Implementation jobs must respect branch locks. Do not edit or push an
implementation branch unless Gitmoot assigned the job and the branch lock is
held by the assigned agent. Review and ask jobs should inspect and report
without mutating the branch unless the task explicitly instructs otherwise.

Useful local commands:

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

## Safe Agent Behavior

- Preserve existing behavior unless the job explicitly changes it.
- Keep changes scoped to the job and the target repo.
- Do not commit generated data, caches, logs, secrets, session archives, cloned
  helper repos, or large outputs unless explicitly requested.
- Verify external APIs, CLIs, env vars, generated scripts, and service launchers
  with local commands or official docs before editing.
- Redact secrets from summaries, comments, and raw examples.
- If a Gitmoot instruction is unclear, reread this `SKILL.md`, then inspect
  `/gitmoot help`, `gitmoot status`, and relevant job events.
