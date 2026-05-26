# Planner And Goal Workflow

Gitmoot includes the `gitmoot-plan-and-goal` preset for structured plans and
standard goal files.

```sh
gitmoot preset update gitmoot-plan-and-goal
gitmoot agent start planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --preset gitmoot-plan-and-goal \
  --start-daemon
```

For fast planning in the current Codex or Claude chat, ask the runtime:

```text
Use the Gitmoot planner here. Write the implementation plan.
```

Ask the registered background planner when you want a queued Gitmoot job:

```sh
gitmoot agent ask planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

Goal files should use task headings shaped like:

```md
### Task 1: Task Title
```

Then import and run tasks through Gitmoot:

```sh
gitmoot goal import --file GOAL.md --repo owner/repo
gitmoot task run task-001 --repo owner/repo --owner planner --base main
```
