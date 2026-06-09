<p align="center">
  <img src="docs/assets/gitmoot-hero.png" alt="Gitmoot coordinates local agent runtimes through GitHub pull request workflows" width="900">
</p>

# Gitmoot

Local-first multi-agent coordination for GitHub pull request workflows.

[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-black.svg)](./LICENSE)
[![GitHub release](https://img.shields.io/github/v/release/jerryfane/gitmoot?include_prereleases&color=black)](https://github.com/jerryfane/gitmoot/releases)
[![Go module](https://img.shields.io/badge/go-module-black.svg)](./go.mod)

Gitmoot lets humans and AI agents collaborate through the place software teams
already audit work: GitHub pull requests. It runs on the user's machine, keeps
workflow state in local SQLite, routes PR comments to registered agent
runtimes, and writes the agent's work back into the repo and PR discussion.

V1 is intentionally local-only. There is no hosted dashboard, webhook receiver,
cloud runner, or remote control plane.

## Why Gitmoot

AI agents can already edit code, review diffs, and run local tools. The hard
part is coordinating that work across sessions without losing the human audit
trail. Gitmoot makes the repository and its pull requests the shared surface:

- PR comments become agent tasks, review requests, retries, and merge signals.
- Local SQLite records agents, repos, jobs, goals, tasks, PRs, and branch locks.
- Runtime adapters keep Codex, Claude Code, and future runtimes behind the same
  Gitmoot agent model.
- Agent Templates and job snapshots make agent instructions explicit and reproducible.
- Humans can follow progress from GitHub while agents keep working locally.

## How It Works

```text
GitHub PR comments/state
  -> local gitmoot daemon
  -> local SQLite state machine and job mailbox
  -> registered runtime adapter
  -> Codex, Claude Code, shell, or another agent runtime
  -> GitHub PR comments, statuses, branches, PRs, and merges
```

The core primitive is a runtime-neutral Gitmoot agent, not a Codex-specific
session. Codex and Claude Code are adapters behind the same internal runtime
contract.

## Install

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot version
gh auth status
```

Install runtime plugin guidance when you want Codex or Claude Code to discover
Gitmoot's Agent Skill from their plugin systems:

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

The plugins are discovery and guidance surfaces. The `gitmoot` CLI and local
daemon remain the execution path.

## Quick Start

From a project checkout:

```sh
gitmoot init
gitmoot repo add owner/repo --path . --poll 30s
gitmoot doctor --repo .
```

Start a Gitmoot-managed Codex agent and daemon:

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

That uses the same `planner` template as the background agent, but imports the
prompt into the current chat instead of creating a Gitmoot job.

Ask the registered background planner when you want a queued analysis or
planning job:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

For coordinator delegation where the request may require review or file edits,
use `agent run` and let Gitmoot select the safe workflow path:

```sh
gitmoot agent run lead --repo owner/repo --task task-001 --background "Implement this task."
gitmoot agent run reviewer --repo owner/repo --pr 12 --background "Review this PR."
```

Background jobs are safe by default. The daemon starts with one worker, repo
checkout locks protect local checkouts, branch locks protect implementation
ownership, and busy Codex/Claude runtime sessions can fork bounded temporary
workers when `[parallel_sessions]` allows it. Temp workers still require
checkout/worktree safety and write-capable agents for implementation jobs.

Route work through PR comments:

```text
/gitmoot project-planner ask Write a task-by-task implementation plan for this PR.
/gitmoot thermo-review review
/gitmoot retry <job-id>
```

For the full walkthrough, see [docs/local-workflow.md](docs/local-workflow.md).

## Core Concepts

- **Repo**: a GitHub repository plus local checkout path that Gitmoot is allowed
  to monitor and mutate.
- **Daemon**: the local background process that polls GitHub PRs and routes
  queued jobs.
- **Agent**: a named Gitmoot identity with repo access, role, capabilities, and
  a runtime adapter.
- **Runtime adapter**: the bridge from Gitmoot jobs to Codex, Claude Code,
  shell commands, or future runtimes.
- **Template**: cached prompt content attached to an agent and snapshotted into
  each job.
- **Job**: a routed unit of work created from a PR comment, local ask, task run,
  retry, or merge action.
- **Goal and task**: Markdown implementation plans imported into local Gitmoot
  state with `gitmoot goal import`.
- **Branch lock**: local coordination state that prevents multiple agents from
  racing on the same branch.

## Common Workflows

### Planner Agent

Gitmoot includes `planner` for structured implementation planning
and standard goal-file writing.

```sh
gitmoot agent template update planner
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

### Review Agent

Gitmoot includes `thermo-nuclear-code-quality-review` for strict review-only
work.

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --start-daemon
```

Ask it from a PR comment:

```text
/gitmoot thermo-review review
```

### Custom Prompt Agents

Custom agent templates let you keep a local template file and bind its
snapshotted instructions to any Gitmoot agent.

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

After editing the template file, refresh the cached snapshot:

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

Reuse a custom agent prompt in the current Codex or Claude chat without
starting a background job:

```text
Use frontend-reviewer here. Review this diff.
```

The runtime should load the prompt with:

```sh
gitmoot agent prompt frontend-reviewer
```

### Template Capture

Template capture turns a useful current Codex or Claude chat workflow into a
reusable agent template. Capture happens in the current chat from visible
conversation context and inspected files; Gitmoot does not read hidden model
state or runtime memory.

Draft a blank structure when starting from scratch:

```sh
gitmoot agent template draft release-planner
```

Or ask the current chat to fill the structure from the visible session:

```text
Use Gitmoot to capture this session as agent template release-planner. Draft only.
```

Review the draft before installing it:

```sh
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add release-planner --file .gitmoot/templates/release-planner.md
gitmoot agent prompt release-planner
```

Installed custom templates are snapshots. After editing the source file, run
`gitmoot agent template diff <id>` and `gitmoot agent template update <id>`.

### Jobs, Locks, And Recovery

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot daemon logs
gitmoot lock list --repo owner/repo
gitmoot lock release owner/repo <branch> --owner <agent>
```

### SkillOpt Exchange

```sh
gitmoot skillopt train init --name <name> --template <id> --review-repo owner/repo --artifact-kind kind --preview kind (--request text|--request-file path)
gitmoot skillopt train init templates --json
gitmoot interactive list --state pending --json
gitmoot interactive show <prompt-id> --json
gitmoot interactive answer <prompt-id> <value> --source agent
gitmoot skillopt train start --config .gitmoot/skillopt/<name>/config.toml --yes
gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id>
gitmoot skillopt review item add --run <run-id> --item <item-id> --baseline baseline.md --candidate candidate.md [--title text]
gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id> --mode explore --options 4
gitmoot skillopt review item add --run <run-id> --item <item-id> --option a=option-a.md --option b=option-b.md [...]
gitmoot skillopt review status --run <run-id>
gitmoot skillopt export --run <run-id> --output training.json
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

Use `skillopt train init` to create a local scaffold under
`.gitmoot/skillopt/<name>/` without starting model work. The scaffold pins the
template/version, writes `config.toml`, `task.md`, and starter
`review-items.yml`, and prints the exact `train start --config` command. Agents
can list template choices with `train init templates --json`; when an
interactive setup needs missing values, they can answer the stored prompts with
`gitmoot interactive list/show/answer`.

For lower-level debugging, create a review run, add saved baseline/candidate outputs as review items,
export a Markdown or GitHub feedback packet, import the completed feedback, then
export `training.json` for a future external `gitmoot-skillopt` optimizer.
Use ranked exploration when the template needs broad search: start with
`--mode explore --options 4` to `6`, rank every option, record useful/rejected
traits, then narrow into `refine`, `distill`, and finally A/B `validate`.
`skillopt review status` reports ranking stability and the recommended next
mode, but the recommendation is advisory only.
Dry-run optimization validates the contract without model calls:

```sh
gitmoot-skillopt optimize \
  --training-package training.json \
  --artifact-root ~/.gitmoot/evals/blobs \
  --out-root .gitmoot/skillopt/run-2026-05-31 \
  --candidate-output candidate.json \
  --dry-run
```

Before real model-backed optimization, run `gitmoot-skillopt optimize --help`
and verify required model/backend environment variables for the installed
optimizer version. Imported candidates stay pending until a human review
workflow promotes them.
When a candidate package includes new artifact manifest entries, pass
`--artifact-dir` to the directory containing those files; Gitmoot verifies
relative paths and SHA256 hashes before storing blobs.
The external optimizer walkthrough lives in
[`jerryfane/gitmoot-skillopt`](https://github.com/jerryfane/gitmoot-skillopt/blob/main/docs/guide/gitmoot-mvp-workflow.md).
Use `skillopt candidate show` to inspect the candidate metadata, eval report,
feedback summary, and content diff before `promote` or `reject` records the
decision.

Use the Markdown feedback collector for a local blind A/B packet: open
`index.md`, review `items/*.md`, set `reviewer`, edit `feedback.yml` with `a`,
`b`, `tie`, `neither`, or `skip`, and import it. Keep `.assignments.json`
untouched so Gitmoot can validate and de-blind the mapping.

Use the GitHub feedback collector when review should happen in an issue or an
existing PR thread. Reviewers can reply with either the copy-paste YAML block or
run-scoped short lines such as `run_id: run-1` followed by
`item-001: b - More concrete.`. Gitmoot imports matching comments into
canonical feedback events and ignores unrelated comments. Repo selection uses
`--repo`, then the eval run target repo, then the template source repo, then
optional `[feedback].repo = "owner/reviews"` in Gitmoot config.

Detailed ranked exploration examples, including a landing-page visual task and
a non-visual writing task, live in
[docs/skillopt-exchange-contract.md](docs/skillopt-exchange-contract.md).

Detailed command coverage lives in
[skills/gitmoot/references/CLI.md](skills/gitmoot/references/CLI.md).

## Plugins

Gitmoot can package its Agent Skill for Codex and Claude Code so the runtime
can discover Gitmoot guidance from its plugin system.

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

Plugins do not start a hosted service, replace the daemon, subscribe agents, or
mutate repository state by themselves. See [docs/plugins.md](docs/plugins.md)
for install details and troubleshooting.

## Documentation

- [Hosted docs](https://gitmoot.io/docs/intro)
- [LLM index](https://gitmoot.io/llms.txt)
- [Agent Skills package](skills/gitmoot/SKILL.md)
- [CLI reference](skills/gitmoot/references/CLI.md)
- [Codex and Claude plugins](docs/plugins.md)
- [Local workflow walkthrough](docs/local-workflow.md)
- [Beta smoke tests](docs/beta-smoke-tests.md)
- [Runtime adapter authoring](docs/adapters.md)
- [Troubleshooting](docs/troubleshooting.md)

## Status And V1 Limits

- Local-only: no hosted dashboard, GitHub App bot identity, cloud runner, or
  remote control plane.
- Polling watches GitHub PRs; there is no webhook receiver in V1.
- GitHub comments are authored by the authenticated `gh` user. Agent identity
  appears in the comment body.
- Local SQLite remains the workflow source of truth.

## Contributing

Gitmoot is early. Keep changes scoped, preserve local-first behavior, and add
focused tests for runtime, CLI, daemon, plugin, or workflow changes. Before
opening a PR, run the relevant checks for the files you touched.

## License

Gitmoot is licensed under the [Apache License 2.0](./LICENSE). See
[NOTICE](./NOTICE) for copyright and attribution details.

## Development

```sh
go test ./...
go vet ./...
```
