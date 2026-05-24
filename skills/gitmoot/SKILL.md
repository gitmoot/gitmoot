---
name: gitmoot
description: Use Gitmoot for local-first multi-agent coordination through GitHub PR comments, repo-scoped agent subscriptions, daemon checks, jobs, branch locks, presets, custom prompt agents, and Codex or Claude Code runtime workflows.
license: MIT
compatibility: Requires the gitmoot CLI, git, GitHub CLI authentication, network access to GitHub, and a supported runtime such as Codex or Claude Code.
metadata:
  gitmoot-version: "0.1.0"
  source: "jerryfane/gitmoot"
---

# Gitmoot Agent Skill

Gitmoot is a local-first coordinator for AI agents working through GitHub pull
requests. Use this skill when the user wants PR-comment agent workflows,
repo-scoped agent subscriptions, background daemon checks, Codex or Claude Code
agent startup, preset agents, custom prompt agents, job status, or branch lock
inspection.

## Before Acting

1. Check whether `gitmoot` is installed with `gitmoot version`.
2. Confirm GitHub CLI access with `gh auth status` before using PR workflows.
3. Detect or ask for the target repo before starting daemons, subscribing agents,
   or routing jobs.
4. Do not start daemons, create agents, update presets, or change subscriptions
   unless the user asks or the current task clearly requires it.
5. Prefer read-only status commands before mutating Gitmoot state.

## Common Commands

Use `gitmoot status --repo owner/repo` for repo status, `gitmoot daemon status`
for daemon state, `gitmoot agent list` for registered agents, and
`gitmoot job list --repo owner/repo` for queued or recent jobs.

For complete command examples, read [CLI.md](references/CLI.md).
For end-to-end workflows, read [WORKFLOWS.md](references/WORKFLOWS.md).

## Agent Job Contract

Every Gitmoot job should return a concise and truthful `gitmoot_result` JSON
object. Use `blocked` when work cannot continue without human input or external
state, and `failed` when an attempted action errored.

For the required result shape and decision meanings, read
[RESULT_CONTRACT.md](references/RESULT_CONTRACT.md).

## Safety Rules

Preserve existing behavior unless the job explicitly changes it. Keep work
scoped to the target repo. Do not commit generated data, caches, logs, secrets,
session archives, cloned helper repos, or large outputs unless explicitly
requested. Respect Gitmoot branch locks for implementation jobs.

For detailed safety and lock rules, read [SAFETY.md](references/SAFETY.md).

## When Unsure

Reread this `SKILL.md`, then inspect `/gitmoot help`, `gitmoot status`, and the
relevant job events before acting.
