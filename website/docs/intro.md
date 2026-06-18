# Gitmoot

Gitmoot coordinates local AI agents through the same surface teams already use
to audit software work: repositories and pull requests.

It runs on the user's machine, stores workflow state in local SQLite, polls
GitHub pull request comments, routes jobs to registered agent runtimes, and
writes attributed results back to the pull request discussion. There is no
hosted control plane in the current beta.

## What Gitmoot Is For

- Route PR comments to named local agents.
- Keep Codex, Claude Code, Kimi Code, shell, and future runtimes behind one
  agent model.
- Start or subscribe agents with explicit repo access and capabilities.
- Orchestrate work across multiple agents with structured delegations and
  automatic synthesis.
- Use agent templates for reusable planner, review, or custom prompt agents.
- Capture a successful current chat as a reviewed, reusable agent template
  draft.
- Import a cached agent prompt into the current chat with
  `gitmoot agent prompt <agent-or-template>`.
- Track jobs, branch locks, goals, tasks, reviews, and merges locally.
- Open `gitmoot dashboard`, an interactive TUI cockpit with pages for Attention,
  Trains, Agents, Runtime sessions, Jobs, and Locks (pending prompts live under
  Attention).
- Run SkillOpt training loops that collect human feedback, call the external
  optimizer, and keep promotion decisions explicit.

## How It Works

```mermaid
flowchart TD
  User[User or PR comment] --> CLI[gitmoot CLI]
  CLI --> Daemon[Local daemon]
  CLI --> Dashboard[Dashboard / TUI]
  Daemon --> DB[Local SQLite state]
  Daemon --> Runtime[Runtime adapter]
  Runtime --> Codex[Codex]
  Runtime --> Claude[Claude Code]
  Runtime --> Kimi[Kimi Code]
  Runtime --> Shell[Shell]
  Daemon --> GitHub[GitHub issues and PRs]
  CLI --> SkillOpt[SkillOpt train workflow]
  SkillOpt --> Optimizer[gitmoot-skillopt]
```

Codex, Claude Code, and Kimi Code are the runtimes Gitmoot can start or
subscribe; shell is subscribe-only.

Use the docs here for the human workflow. Agents should also read
[`SKILL.md`](https://gitmoot.io/SKILL.md) and
[`llms.txt`](https://gitmoot.io/llms.txt).

## Docs Map

```mermaid
flowchart LR
  Start[Getting Started] --> Workflows[Workflows]
  Workflows --> Reference[Reference]
  Reference --> Operations[Operations]
  Start --> Plugins[Codex and Claude plugins]
  Workflows --> SkillOpt[SkillOpt train]
  Reference --> CLI[CLI and dashboard]
```
