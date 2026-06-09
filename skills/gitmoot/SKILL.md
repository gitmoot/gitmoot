---
name: gitmoot
description: Use Gitmoot for local-first AI agent coordination across repositories, goals, reviews, GitHub PR comments, agent subscriptions, daemon checks, jobs, branch locks, agent-templates, template capture, custom prompt agents, and Codex or Claude Code runtime workflows.
license: Apache-2.0
compatibility: Requires the gitmoot CLI, git, GitHub CLI authentication, network access to GitHub, and a supported runtime such as Codex or Claude Code.
metadata:
  gitmoot-version: "0.1.0"
  source: "jerryfane/gitmoot"
---

# Gitmoot Agent Skill

Gitmoot is a local-first coordinator for AI agents working across repositories,
goals, reviews, PR comments, and runtime workflows. Use this skill when the
user wants PR-comment agent workflows, repo-scoped agent subscriptions,
background daemon checks, Codex or Claude Code agent startup, structured
implementation plans, standard goal files, agent template workflows, custom
prompt agents, template capture, job status, or branch lock inspection.

For current-chat prompt import, "use <agent> here" or "use Gitmoot agent
<agent> here" means run `gitmoot agent prompt <agent>` and apply the returned
prompt content in this current chat. This is prompt import, not true
system-prompt injection. The natural phrase "use the Gitmoot planner here" maps
to the same `planner` template used by managed planner agents. If the planner
template is not cached, read and apply the packaged
`agent-templates/planner.md` instructions directly. Do not route a "here"
request through a background `gitmoot agent ask` unless the user explicitly
asks for background execution, PR-comment routing, or job tracking.

For template capture, phrases like "capture this session as a Gitmoot agent
template", "turn this workflow into a Gitmoot template", or "draft a reusable
agent template from this chat" mean read [TEMPLATE_CAPTURE.md](references/TEMPLATE_CAPTURE.md)
and distill the visible current-chat context into a draft template. Gitmoot
cannot read hidden model memory or runtime internals. Do not install, overwrite,
or update a permanent template unless the user explicitly approves that step.
Use `gitmoot agent template draft <id>` for a blank scaffold,
`gitmoot agent template validate <file>` for a structural check,
`gitmoot agent template add <id> --file <file>` to install a snapshot, and
`gitmoot agent prompt <id>` to reuse the installed template in the current chat.

For background work, keep Gitmoot's resource model explicit: repo checkout
locks protect local checkouts, runtime session locks serialize delivery for the
same Codex or Claude session, and branch locks protect implementation ownership.
The daemon default is `--workers 1`; raise it only for independent runtime
sessions or managed agent types with `max_background` greater than one.

## Before Acting

1. Check whether `gitmoot` is installed with `gitmoot version`.
2. Confirm GitHub CLI access with `gh auth status` before using PR workflows.
3. Detect or ask for the target repo before starting daemons, subscribing agents,
   or routing jobs.
4. Do not start daemons, create agents, update agent templates, or change
   subscriptions unless the user asks or the current task clearly requires it.
5. Prefer read-only status commands before mutating Gitmoot state.

## Common Commands

Use `gitmoot status --repo owner/repo` for repo status, `gitmoot daemon status`
for daemon state, `gitmoot agent list` and `gitmoot agent show <agent>` for
registered agents, `gitmoot task list --repo owner/repo` for imported task
state, and `gitmoot agent prompt <agent-or-template>` to import an agent prompt
into the current chat. Use `gitmoot agent run <agent> --repo owner/repo "..."`
for coordinator delegation so Gitmoot can route to ask, review, or implement
and own worktrees, branch locks, commits, pushes, PRs, and workflow
advancement. Use `gitmoot agent ask <agent> --repo owner/repo "..."` only for
analysis, planning, or questions. Use `gitmoot agent review <agent> --repo
owner/repo --pr <number> "..."` for PR review decisions and `gitmoot agent
implement <agent> --repo owner/repo --task <task-id> "..."` for file changes.
Add `--background` only when the user wants a queued background job. Use
`gitmoot job list --repo owner/repo` for queued or recent jobs. Use
`gitmoot plugin doctor` when checking whether Codex or Claude Code can discover
Gitmoot through an installed runtime plugin. Use `gitmoot goal template` when
writing a standard task-by-task goal file.

The plugin is only the runtime discovery surface for this skill. Local agent
invocation still goes through the `gitmoot` CLI and the same registered agent,
repo access, runtime adapter, and job history model used by PR-comment jobs.

For SkillOpt template learning, prefer the high-level
`gitmoot skillopt train start/status/continue/stop` workflow when the user wants
Gitmoot to enforce the full feedback, optimizer, candidate-review, and
promotion loop. Use low-level `gitmoot skillopt review`, `feedback`, `export`,
`import`, and `candidate` commands for advanced/debug work or when recovering a
specific step. In train mode, collect enough ranked feedback and trait notes
before optimizer handoff, keep promotion decisions explicit, and start follow-up
iterations only through `train continue --start-next`.

For complete command examples, read [CLI.md](references/CLI.md).
For end-to-end workflows, read [WORKFLOWS.md](references/WORKFLOWS.md).
For current-chat template capture, read
[TEMPLATE_CAPTURE.md](references/TEMPLATE_CAPTURE.md).
For the canonical goal prompt template, read
[GOAL_TEMPLATE.md](references/GOAL_TEMPLATE.md) only when the user asks for a
goal file.

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
