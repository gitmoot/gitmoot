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
gitmoot agent start planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --preset gitmoot-plan-and-goal \
  --start-daemon
```

Ask the agent directly:

```sh
gitmoot agent ask planner --repo owner/repo "Write the implementation plan and goal file."
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

