# Gitmoot

Gitmoot coordinates local AI agents through the same surface teams already use
to audit software work: repositories and pull requests.

It runs on the user's machine, stores workflow state in local SQLite, polls
GitHub pull request comments, routes jobs to registered agent runtimes, and
writes attributed results back to the pull request discussion. There is no
hosted control plane in the current beta.

## What Gitmoot Is For

- Route PR comments to named local agents.
- Keep Codex, Claude Code, shell, and future runtimes behind one agent model.
- Start or subscribe agents with explicit repo access and capabilities.
- Use agent templates for reusable planner, review, or custom prompt agents.
- Track jobs, branch locks, goals, tasks, reviews, and merges locally.

## How It Works

```text
GitHub PR comments
  -> gitmoot daemon
  -> local SQLite state and job mailbox
  -> runtime adapter
  -> Codex, Claude Code, shell, or another runtime
  -> attributed PR reply and local job status
```

Use the docs here for the human workflow. Agents should also read
[`SKILL.md`](https://gitmoot.io/SKILL.md) and
[`llms.txt`](https://gitmoot.io/llms.txt).
