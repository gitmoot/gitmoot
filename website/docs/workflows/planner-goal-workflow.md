# Planner And Goal Workflow

Gitmoot includes the `planner` template for structured plans and
standard goal files.

```sh
gitmoot agent template update planner
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --model gpt-5-codex \
  --start-daemon
```

`--runtime` accepts `codex`, `claude`, or `kimi` (Kimi Code CLI). The optional
`--model <name>` flag sets the agent's default runtime model; it is a free-form,
runtime-scoped string with no allow-list, and an omitted `--model` preserves the
runtime's own default.

For fast planning in the current Codex or Claude chat, ask the runtime:

```text
Use the Gitmoot planner here. Write the implementation plan.
```

This uses the same `planner` template as the background agent, imported into
the current chat with `gitmoot agent prompt planner` when cached or from the
packaged skill instructions.

Ask the registered background planner when you want a queued Gitmoot job:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot agent ask project-planner --repo owner/repo --model gpt-5-codex --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

`agent ask` (and `run`, `implement`, `review`) accept an optional
`--model <name>` flag that pins the runtime model for that one job, overriding
the agent's configured default.

Goal files should use task headings shaped like:

```md
### Task 1: Task Title
```

Then import and run tasks through Gitmoot:

```sh
gitmoot goal import --file GOAL.md --repo owner/repo
gitmoot task run task-001 --repo owner/repo --owner planner --base main
```

Imported plans remain `planned` until explicitly run. Repositories may opt into
retiring old never-started plans with `[workflow].planned_ttl = "720h"`, but it
is disabled by default (unset, empty, zero, or invalid means off): automatic
dismissal can destroy human planning context a goal-file re-import cannot
reconstruct. Enabled sweeps skip live jobs, open PRs, present remote branches,
and uncertain remote checks and record `task_dismissed_planned_ttl`. The
`task run` allocation uses a write-time state claim, so it cannot resurrect a
plan that a concurrent sweep dismissed; use explicit `task recover` instead.

Once work starts, lifecycle reconciliation favors visible human triage. A clean
closed-unmerged PR blocks linked `pr_open`, `reviewing`, and
`changes_requested` tasks (`pr_closed_unmerged`). A terminal top-level implement
job that finishes advancement/delegation with no PR or live successor also
blocks the task, while delegation children and queued continuations remain under
their existing workflow owner.
