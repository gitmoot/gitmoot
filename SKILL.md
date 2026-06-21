---
name: gitmoot
description: Use Gitmoot for local-first AI agent coordination across repositories, goals, reviews, PR comments, daemon jobs, branch locks, agent templates, template capture, custom prompt agents, and Codex, Claude Code, or Kimi Code runtime workflows.
version: 0.1.0
license: Apache-2.0
compatibility: Requires the gitmoot CLI, git, GitHub CLI authentication, network access to GitHub, and a supported runtime such as Codex, Claude Code, or Kimi Code.
metadata:
  gitmoot-version: "0.4.1"
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
background daemon checks, Codex, Claude Code, or Kimi Code agent startup,
structured implementation plans, standard goal files, agent template workflows,
template capture, custom prompt agents, job status, or branch lock inspection.

For current-chat prompt import, "use <agent> here" means run
`gitmoot agent prompt <agent>` and apply the returned prompt content in this
current chat. This is prompt import, not true system-prompt injection. Do not
route a "here" request through a background `gitmoot agent ask` unless the user
explicitly asks for background execution, PR-comment routing, or job tracking.

For fast planning, "use the Gitmoot planner here" is the natural-language
shortcut for `gitmoot agent prompt planner`. If the planner template is not
cached, read and apply the packaged `skills/gitmoot/agent-templates/planner.md`
instructions directly.

For template capture, phrases like "capture this session as a Gitmoot agent
template", "turn this workflow into a Gitmoot template", or "draft a reusable
agent template from this chat" mean read
`skills/gitmoot/references/TEMPLATE_CAPTURE.md` and distill the visible
current-chat context into a draft template. Gitmoot cannot read hidden model
memory or runtime internals. Do not install, overwrite, or update a permanent
template unless the user explicitly approves that step.
Use `gitmoot agent template draft <id>` for a blank scaffold,
`gitmoot agent template validate <file>` for a structural check,
`gitmoot agent template add <id> --file <file>` to install a snapshot, and
`gitmoot agent prompt <id>` to reuse the installed template in the current chat.

For background work, keep Gitmoot's resource model explicit: repo checkout
locks protect local checkouts, runtime session locks serialize delivery for the
same Codex, Claude, or Kimi session, and branch locks protect implementation
ownership.
The daemon default is `--workers 1`; raise it only for independent runtime
sessions or managed agent types with `max_background` greater than one.

For runtime selection, `gitmoot agent start <name> --runtime <runtime>` accepts
`codex`, `claude`, or `kimi`. Kimi Code is a first-class runtime adapter
alongside Codex and Claude Code. To use it, run `kimi login`, then restart the
Gitmoot daemon so it inherits the session.

For Gitmoot health or status questions, run the relevant read-only Gitmoot CLI
checks and answer directly from the results. Mention `gitmoot dashboard` only
after that answer, as a live monitoring follow-up. Do not start daemons, create
agents, change subscriptions, update templates, or release locks unless the user
asks for that action.

## Before Acting

1. Check whether `gitmoot` is installed with `gitmoot version`.
2. Confirm GitHub CLI access with `gh auth status` before using PR workflows.
3. Detect or ask for the target repo before starting daemons, subscribing agents,
   or routing jobs.
4. Do not start daemons, create agents, update agent templates, change
   subscriptions, or release locks unless the user asks or the current task
   clearly requires it.
5. Prefer read-only status commands and answer directly before mutating Gitmoot
   state or pointing the user to live monitoring.

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

Install the separate Python optimizer before optimizer-backed SkillOpt train
continues:

```sh
python3 -m pip install --user pipx
python3 -m pipx ensurepath
pipx install https://github.com/jerryfane/gitmoot-skillopt/releases/download/v0.3.0/gitmoot_skillopt-0.3.0-py3-none-any.whl
gitmoot-skillopt --version
gitmoot-skillopt optimize --help
```

If `pipx` is unavailable, install `gitmoot-skillopt` in a venv and pass
`--skillopt-bin /path/to/venv/bin/gitmoot-skillopt` to `gitmoot skillopt train
continue`.

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
gitmoot agent prompt <agent-or-template>
gitmoot agent run <agent> --repo owner/repo "question, review, or implementation request"
gitmoot agent review <agent> --repo owner/repo --pr <number> "review request"
gitmoot agent implement <agent> --repo owner/repo --task <task-id> "implementation request"
gitmoot agent ask <agent> --repo owner/repo "question or instructions"
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot report bug --job <job-id> --preview
gitmoot report bug --job <job-id> --create --yes
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
gitmoot skillopt export --run <run-id> [--output training.json]
gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]
gitmoot skillopt candidate list [--template id]
gitmoot skillopt candidate show <version-id>
gitmoot skillopt candidate promote <version-id>
gitmoot skillopt candidate reject <version-id> [--reason text]
gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>
gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id>
gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]
gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)
```

Use `gitmoot daemon start` for the background daemon. Use `gitmoot daemon run`
only when the user explicitly wants a foreground process.

Use `gitmoot agent prompt <agent-or-template>` when the user wants to reuse a
Gitmoot agent prompt in the current chat. Use `gitmoot agent run` for
coordinator delegation through a registered Gitmoot agent; Gitmoot will route to
ask, review, or implement and own worktrees, branch locks, commits, pushes, PRs,
and workflow advancement. To orchestrate an orchestra of agents,
`gitmoot orchestrate <agent> "..." [--repo R]` is sugar for `gitmoot agent run
<agent> --background "..."`. Use `gitmoot agent ask` only for analysis, planning,
or questions. Add `--background` only when the user wants a queued background
job. This is the same agent registry and runtime adapter path used by PR-comment
jobs; the plugin only helps the runtime discover this skill and does not replace
the `gitmoot` CLI.

Use `gitmoot report bug --job <job-id> --preview` when a failed, blocked, or
cancelled Gitmoot job needs a user-shareable bug report. Preview first by
default. Create with `--create --yes` only when the user explicitly asks or the
active workflow policy permits filing reports, then report the created or
existing GitHub issue URL back to the user.

SkillOpt train generation is durable and idempotent on resume. Each review
item's artifacts, item row, and options commit in one transaction the moment
that item finishes, so an interrupted generate phase loses only the item that
was in flight. Re-running `gitmoot skillopt train continue` regenerates ONLY the
items that are not yet complete; fully-generated items are skipped, so no
duplicate options are produced and completed work is never rewritten. If a single
item has some but not all of its options/artifacts persisted, resume returns a
hard error (`item <id> has partial generated options; inspect or clear review
options before continuing`) rather than guessing.

Use `gitmoot skillopt train recover --session <id> [--out-root path] [--json]`
to recover the OPTIMIZER phase only. It re-imports or repairs the optimizer
candidate package and classifies the iteration (for example
`already_completed_candidate`, `already_completed_no_candidate`,
`optimizer_active`, or `corrupted_unrecoverable`). It does NOT release the
generation-phase lock or rebuild generation options; use `train continue` for
generation resume.

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

## Template Agents

Install or refresh the built-in thermo review template:

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --start-daemon
```

Coordinator recipes are built-in templates for the Orchestra pattern: a
coordinator that fans work out to ephemeral workers (no pre-registration) and
reconvenes them in one continuation. `review-panel` convenes a panel of
diverse-lens reviewers over a PR and synthesizes their verdict;
`decompose-and-verify` splits an implementation task into parallel file-disjoint
legs and runs a verify step that depends on all of them. Run them with
`gitmoot orchestrate`:

```sh
gitmoot orchestrate review-panel "Review PR #123 in this repo." --repo owner/repo
gitmoot orchestrate decompose-and-verify "Implement the export feature described in the task." --repo owner/repo
```

Create a local custom prompt template:

```sh
mkdir -p agents
gitmoot agent template draft frontend-reviewer --output agents/frontend-reviewer.md
$EDITOR agents/frontend-reviewer.md
gitmoot agent template validate agents/frontend-reviewer.md
gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
gitmoot agent start frontend-reviewer \
  --runtime codex \
  --repo owner/repo \
  --template frontend-reviewer \
  --role reviewer \
  --capability ask \
  --capability review
```

After editing a local template file, refresh Gitmoot's cached snapshot explicitly:

```sh
gitmoot agent template diff frontend-reviewer
gitmoot agent template update frontend-reviewer
```

Template updates are versioned locally. `gitmoot agent template show <id>`
prints the current version, content hash, source commit, and promotion state.
Agents use the current promoted version by default, or a pinned version when
configured with a reference such as `--template frontend-reviewer@v1`.
Queued jobs keep the exact template content snapshot they were created with.

Discover templates by metadata:

```sh
gitmoot agent template list --runtime codex --output goal_file
gitmoot agent template list --tag review --capability ask
gitmoot agent template show frontend-reviewer
```

Draft and validate a captured template before installing it:

```sh
gitmoot agent template draft release-planner
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add release-planner --file .gitmoot/templates/release-planner.md
gitmoot agent prompt release-planner
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
    "delegations": []
  }
}
```

Use `blocked` when work cannot continue without human input or external state.
Use `failed` when the attempted action errored. Do not report tests, changes, or
approvals that were not actually verified. Use `delegations` to request follow-up
work by named Gitmoot agents.

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
