# Quick Start

Run these commands from a project checkout.

```sh
git status --short
git remote -v
gh auth status
gitmoot init
gitmoot repo add owner/repo --path . --poll 30s
gitmoot doctor --repo .
```

Start a Gitmoot-managed planner agent and the background daemon:

```sh
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

For fast planning in the current Codex or Claude chat, ask the runtime:

```text
Use the Gitmoot planner here. Write the implementation plan.
```

Ask the registered background planner when you want a queued Gitmoot job:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

Or route work through PR comments:

```text
/gitmoot ask planner Write a task-by-task plan for this PR.
/gitmoot thermo-review review
/gitmoot retry <job-id>
```

Inspect state:

```sh
gitmoot status --repo owner/repo
gitmoot job list --repo owner/repo
gitmoot events --repo owner/repo
```
