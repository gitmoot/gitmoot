# Gitmoot Workflows

## First Repo Setup

1. Confirm the repo identity and GitHub auth.
2. Start the daemon for the repo.
3. Start or subscribe at least one agent.
4. Verify the agent is healthy before asking PR comments to route jobs.

```sh
gh repo view --json nameWithOwner
gh auth status
gitmoot daemon start --repo owner/repo
gitmoot agent start reviewer --runtime codex --repo owner/repo --role reviewer --capability ask --capability review --start-daemon
gitmoot agent doctor reviewer
```

## Review Agent From A PR Comment

1. Register a reviewer agent for the target repo.
2. Ensure the daemon watches the same repo.
3. Comment on the PR.
4. Inspect job status if no result appears.

```text
/gitmoot reviewer review
```

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
```

## Built-In Thermo Review Agent

Use the thermo template for strict review-only work. It should not implement code
or request implementation capability.

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review --runtime codex --repo owner/repo --template thermo-nuclear-code-quality-review --start-daemon
```

PR comment:

```text
/gitmoot thermo-review review
```

## Custom Prompt Agent

Use custom prompt agent templates for project-specific reviewers or helpers.

```sh
mkdir -p agents
printf '%s\n' 'Review frontend changes for correctness and responsive behavior.' > agents/frontend-reviewer.md
gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
gitmoot agent start frontend-reviewer \
  --runtime codex \
  --repo owner/repo \
  --template frontend-reviewer \
  --role reviewer \
  --capability ask \
  --capability review
```

Custom template content is snapshotted into local Gitmoot state. After editing the
source prompt file, run `gitmoot agent template diff <id>` and `gitmoot agent template update
<id>` before expecting new jobs to use the changed prompt.

## Planner Here

Use the lightweight planner in the current Codex or Claude chat when the user
wants a fast implementation plan and the current session already has the repo
context. Read the packaged `agent-templates/planner-here.md` instructions, inspect
only the relevant files, use web search only for current external contracts or
best-practice claims, and return the plan directly in chat.

```text
Use the Gitmoot planner here. Write a task-by-task implementation plan for this feature.
```

If the user later asks for a standard goal file, read the canonical goal
template and write the goal file then. Do not create the goal file during the
lite planning pass unless explicitly requested.

## Background Planner Agent

Use the planner template when the user wants a structured implementation plan or a
standard Gitmoot goal file to run as a tracked Gitmoot background agent job.

```sh
gitmoot agent template update planner
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

Ask from a PR comment:

```text
/gitmoot project-planner ask Write a task-by-task implementation plan for this feature, then create the goal file prompt.
```

Ask directly from a local Codex or Claude Code chat by having the runtime call
the Gitmoot CLI when the user explicitly wants a registered background-capable
agent path:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write a task-by-task implementation plan for this feature, then create the goal file prompt."
gitmoot job watch <job-id>
```

If the Codex plugin exposes a Gitmoot command bridge in chat, the equivalent
form is `$gitmoot:gitmoot agent ask project-planner --repo owner/repo --background "..."`. The
important part is that background planner work goes through `gitmoot agent ask`;
fast "here" planning stays in the current chat and does not claim to control the
current chat through a CLI command.

If the planner writes a goal file and the user wants Gitmoot to track it, import
it explicitly:

```sh
gitmoot goal import --file GOAL-feature.md --repo owner/repo
```

## Execution Model

Use `here` when the current chat should answer directly from the Gitmoot skill.
Use `background` when Gitmoot should create a tracked job, store events, and run
through a runtime adapter.

Background jobs are scheduled against three distinct resources:

- repo checkout mutexes for daemon ticks that use the same local checkout;
- runtime session locks keyed as `runtime:<runtime>:<runtime_ref>` for Codex
  and Claude delivery;
- branch locks for implementation ownership and merge safety.

The daemon default is `--workers 1`. Users can raise it when jobs target
different runtime sessions or managed agent types with `max_background` greater
than one. Gitmoot still serializes jobs for the same runtime session or checkout
even when more workers are configured.

## Multi-Repo Work

Agents are global identities with explicit per-repo access. When working across
multiple repos, always pass `--repo owner/repo` to status, daemon, job, and event
commands so jobs are routed in the correct repository context.

```sh
gitmoot agent allow reviewer --repo owner/project-a
gitmoot agent allow reviewer --repo owner/project-b
gitmoot status --repo owner/project-a
gitmoot status --repo owner/project-b
```
