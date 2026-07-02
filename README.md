<p align="center">
  <img src="docs/assets/gitmoot-hero.png" alt="Gitmoot coordinates local agent runtimes through GitHub pull request workflows" width="900">
</p>

# Gitmoot

Local-first multi-agent coordination for GitHub pull request workflows.

[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-black.svg)](./LICENSE)
[![GitHub release](https://img.shields.io/github/v/release/jerryfane/gitmoot?include_prereleases&color=black)](https://github.com/jerryfane/gitmoot/releases)
[![Go module](https://img.shields.io/badge/go-module-black.svg)](./go.mod)
[![CI](https://github.com/jerryfane/gitmoot/actions/workflows/ci.yml/badge.svg)](https://github.com/jerryfane/gitmoot/actions/workflows/ci.yml)

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
- Runtime adapters keep Codex, Claude Code, Kimi Code (plus the legacy
  `kimi-cli`, a deterministic `shell` adapter), and future runtimes behind the
  same Gitmoot agent model.
- **Orchestra** is Gitmoot's name for structured multi-agent delegation: a
  conductor (coordinator) returns a validated `delegations[]` DAG that Gitmoot
  dispatches as child jobs (the players), then enqueues one continuation job
  (the finale) to synthesize their results (replacing the old `next_agents`
  mechanism). Use `gitmoot orchestrate <agent> "..."` to start one.
- Delegation trees are bounded by a depth cap, a per-root job budget, a per-root
  wall-clock budget, a per-coordinator width cap, and loop detection. When a
  bound trips, the engine enqueues one graceful finalize continuation so the
  coordinator synthesizes a best-effort result instead of recursing forever or
  dropping work silently.
- Agent Templates and job snapshots make agent instructions explicit and
  reproducible — and can be backed up to / pulled from a GitHub repo.
- Heartbeat schedules run recurring agent work without an external cron, and an
  opt-in `[events]` webhook streams job/candidate lifecycle events to your own
  systems.
- Humans can follow progress from GitHub while agents keep working locally —
  in the PR thread, the terminal TUI, or the read-only web dashboard.

## How It Works

```text
GitHub PR comments/state
  -> local gitmoot daemon
  -> local SQLite state machine and job mailbox
  -> registered runtime adapter
  -> Codex, Claude Code, Kimi Code, shell, or another agent runtime
  -> GitHub PR comments, statuses, branches, PRs, and merges
```

The core primitive is a runtime-neutral Gitmoot agent, not a Codex-specific
session. Codex, Claude Code, and Kimi Code are adapters behind the same internal
runtime contract.

## Install

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot version
gh auth status
```

Later, self-update from GitHub Releases:

```sh
gitmoot update --check
gitmoot update --restart-daemon
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

For sandboxed Codex sessions, print a launch command that grants access to the
resolved Gitmoot home:

```sh
gitmoot plugin codex-launch --repo .
```

## Quick Start

One command registers the repo, subscribes an agent, and launches a
tagging-ready daemon (`--watch-issues` defaults on):

```sh
gitmoot setup --repo owner/repo --path . --agent helper --runtime claude --session last --start-daemon
```

Or step through the manual path from a project checkout:

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

`--runtime` accepts `codex`, `claude`, `kimi`, or `kimi-cli` (the opt-in legacy
Kimi CLI adapter). For Kimi Code, run `kimi login` and restart the Gitmoot
daemon so it inherits the session, then start the agent with `--runtime kimi`.

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

Run one job through a different runtime (or model) without re-registering the
agent — the agent's default runtime and session are untouched:

```sh
gitmoot agent ask project-planner --repo owner/repo --runtime claude "Compare the approaches."
gitmoot agent run lead --repo owner/repo --model gpt-5-codex "Implement this task."
```

For coordinator delegation (the Orchestra pattern) where the request may require
review or file edits, use `agent run` and let Gitmoot select the safe workflow
path:

```sh
gitmoot agent run lead --repo owner/repo --task task-001 --background "Implement this task."
gitmoot agent run reviewer --repo owner/repo --pr 12 --background "Review this PR."
```

`gitmoot orchestrate <agent> "..." [--repo R]` is sugar for
`gitmoot agent run <agent> --background "..."` — it starts an orchestra of agents
from a conductor that returns a `delegations[]` score.

Background jobs are safe by default. The daemon starts with one worker, repo
checkout locks protect local checkouts, branch locks protect implementation
ownership, and busy Codex/Claude runtime sessions can fork bounded temporary
workers when `[parallel_sessions]` allows it. Temp workers still require
checkout/worktree safety and write-capable agents for implementation jobs.
`gitmoot daemon start --parallel N` runs a repo's queued jobs N-wide
(auto-selecting the pool scheduler); see
[docs/parallel-jobs.md](docs/parallel-jobs.md).

`daemon start --repo owner/repo` scopes the daemon to a **single** repo: it
polls only that repo's PRs and claims only that repo's queued jobs. Omit
`--repo` and one daemon supervises every enabled repo; cap an individual repo's
parallelism on that shared daemon with the `[repos."owner/repo"] max_parallel`
config key. Both `daemon run` and `daemon start` accept
`--session <root-job-id>` (alias `--root`) to pin the worker to a single
orchestration run — it then runs only jobs whose `root_job_id` matches that
value plus the root coordinator job itself:

```sh
gitmoot daemon start --session <root-job-id>
```

Route work through PR comments:

```text
/gitmoot project-planner ask Write a task-by-task implementation plan for this PR.
/gitmoot thermo-review review
/gitmoot retry <job-id>
/gitmoot resume <job-id> retry|continue|abort|answer [instructions]
@project-planner ask What is blocking this PR?
```

A bare `@<agent> <action>` mention works as the same command; with the daemon's
`--watch-issues` flag (on by default in `gitmoot setup`) mentions on **issues**
are routed too. `/gitmoot resume` continues a delegation tree paused for a
human (`escalate_human` failures or ask-gate questions) — the daemon @-tags you
with the exact resume command when a tree pauses.

For the full walkthrough, see [docs/local-workflow.md](docs/local-workflow.md).

### Operate the daemon

```sh
gitmoot daemon status                 # scheduler mode, workers, runtime auth
gitmoot daemon start --parallel 5     # run queued jobs 5-wide (pool scheduler)
kill -HUP <daemon-pid>                # live-reload [daemon] poll/workers/scheduler — no restart
gitmoot daemon restart                # restart WITHOUT losing the persisted Claude token
gitmoot daemon stop [--forget-runtime-auth]
```

Reconfigure live with SIGHUP (#577) instead of restarting: the running daemon
re-reads the `[daemon]` config keys with no teardown, dropped jobs, or env
re-inheritance. Cap a single repo from config with `[repos."owner/repo"]
max_parallel = N` (#576) — applied live, no relaunch. Runtime auth survives
restarts (#578): the daemon persists its Claude token to a 0600
`daemon-runtime.env` file, `daemon restart` recovers it even from a token-less
shell (plain `stop`+`start` does not), and `--forget-runtime-auth` deletes it.
An opt-in `[events]` webhook streams `job.finished/failed/blocked/
needs_attention/deferred` and `candidate.*` events to one HTTP endpoint
([docs/events.md](docs/events.md)).

## Core Concepts

- **Repo**: a GitHub repository plus local checkout path that Gitmoot is allowed
  to monitor and mutate.
- **Daemon**: the local background process that polls GitHub PRs and routes
  queued jobs. Passing `--repo owner/repo` scopes it to that single repo
  (polling + job claims); omit `--repo` and it supervises every enabled repo
  (cap one repo via `[repos."owner/repo"] max_parallel`).
  `--session <root-job-id>` scopes it to one orchestration run.
- **Agent**: a named Gitmoot identity with repo access, role, capabilities, and
  a runtime adapter.
- **Runtime adapter**: the bridge from Gitmoot jobs to Codex, Claude Code,
  Kimi Code, shell commands, or future runtimes.
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

### Coordinator Recipes

Coordinator recipes are built-in templates that turn the Orchestra pattern into
one command. Each runs a coordinator that fans work out to ephemeral workers (no
pre-registration) and reconvenes them in a single continuation. `review-panel`
convenes a panel of diverse-lens reviewers over a PR and synthesizes one verdict;
`decompose-and-verify` splits an implementation task into parallel, file-disjoint
legs and runs a verify step that depends on all of them; `verifier` is the
minimal produce-vs-independent-check pair (one producer leg plus one read-only
verify leg on a different runtime). Route any coordinator agent through a
recipe with `--recipe`:

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Implement the export feature described in the task." --repo owner/repo --recipe decompose-and-verify
gitmoot orchestrate project-planner "Implement the rate limiter and prove it works." --repo owner/repo --recipe verifier
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

Back up and share templates via a GitHub repo (#476):

```sh
gitmoot agent template remote set owner/my-templates   # default remote ([template_remote]; ref=main, path=templates/)
gitmoot agent template publish --all                   # commit templates to the remote (--create makes the repo)
gitmoot agent template pull frontend-reviewer          # install/refresh from the remote
gitmoot agent template add frontend-reviewer --from-repo owner/my-templates
```

Templates are stored and published **verbatim** (prompt body + metadata) —
point the remote at a private repo unless the prompts are meant to be public.

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

### Dashboard Cockpit

On a terminal, `gitmoot dashboard` opens an interactive TUI that can act on
everything it shows: answer prompts and retry blocked/failed jobs from the
Attention page (plus `s` to restart a stopped daemon), open/stop/delete train
sessions (with optional cleanup of GitHub repos gitmoot created for them),
create/delete agents, revert a template to a previous version, start a training
run for an agent with a codex/claude backend pick (`o`), and cancel queued or
running jobs. For failed, blocked, or cancelled jobs, `B` opens a redacted bug
report preview and `g` creates or reuses a GitHub issue. Press `?` on any page
for its keys. Piped/`--plain`/`--json` output is unchanged and script-stable.

```sh
gitmoot dashboard           # interactive cockpit
gitmoot dashboard --json    # one-shot snapshot for scripts
gitmoot dashboard --web [--addr 127.0.0.1:8080]   # read-only web dashboard
```

`gitmoot dashboard --web` serves a read-only browser view — a live
orchestration/delegation graph with run summaries and prompt/output
inspection — until interrupted.

Agents should answer directly from the SessionStart snapshot or read-only CLI
checks first. The dashboard remains the live cockpit for humans.

### Heartbeat Schedules

Heartbeats let the daemon run recurring agent work itself — cron-like
background jobs without an external cron (off by default):

```sh
gitmoot agent heartbeat add repo-maintainer daily-status \
  --repo owner/repo --interval 24h --prompt "Daily status report." --enabled
gitmoot agent heartbeat list
```

See [docs/heartbeats.md](docs/heartbeats.md).

### Jobs, Locks, And Recovery

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot job retry <job-id>
gitmoot job cancel <job-id>
gitmoot job kill <root-job-id>
gitmoot report bug --job <job-id> [--preview]
gitmoot report bug --job <job-id> --create --yes
gitmoot daemon logs
gitmoot lock list --repo owner/repo
gitmoot lock release owner/repo <branch> --owner <agent>
```

`gitmoot job kill <root-job-id>` is the operator kill switch for a whole
delegation tree: in-flight jobs finish, the coordinator's next continuation is
routed through the graceful finalize path, and the daemon stops starting queued
children of that root.

`gitmoot report bug` builds a redacted issue draft with job context, selected
error, recent events, redaction notes, labels, and a duplicate-detection
fingerprint. Agents should preview first and create only when the user explicitly
asks or the active workflow policy allows filing the report, then return the
created or existing issue URL.

### SkillOpt Exchange

```sh
gitmoot skillopt train init --name <name> --template <id> --review-repo owner/repo --artifact-kind kind --preview kind (--request text|--request-file path)
gitmoot skillopt train init templates --json
gitmoot interactive list --state pending --json
gitmoot interactive show <prompt-id> --json
gitmoot interactive answer <prompt-id> <value> --source agent
gitmoot skillopt train start --config .gitmoot/skillopt/<name>/config.toml --workspace-repo <owner/repo> --yes
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
`gitmoot interactive list/show/answer`. On a real terminal, missing fields are
collected through an interactive form (arrow-key template/preview pickers, inline
validation); each field is still published as a prompt record, so another
terminal can answer it with `gitmoot interactive answer` and the form advances
automatically. Set `GITMOOT_NO_TUI=1` (or pipe stdin) to use the line-based
wizard instead.

On a real terminal, `gitmoot skillopt train run [--config path | --session <id>]`
opens an interactive view of one train session: a phase bar
(generate → review → optimize → promote) and a single keypress per step — `enter`
generates options / publishes the review / syncs feedback / runs the optimizer
(the long generate and optimizer steps run in a detached background process, so
quitting the TUI with `q` leaves the run going), `p`/`x` promote or reject a
candidate, `n` starts the next iteration. At a review-blocked phase it shows the
GitHub issue link so you can instead comment on the issue and let the
review-watcher continue. Piped/`--plain`/`GITMOOT_NO_TUI` print a one-shot status
snapshot. Pass `--create-repos` to `train start` (or accept the in-form prompt
during `train init`) to create a missing target/workspace/review repo on GitHub.

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
gitmoot plugin codex-launch --repo .
```

Plugins do not start a hosted service, replace the daemon, subscribe agents, or
mutate repository state by themselves. See [docs/plugins.md](docs/plugins.md)
for install details and troubleshooting.

## Documentation

- [Hosted docs](https://gitmoot.io/docs/intro)
- [LLM index](https://gitmoot.io/llms.txt) — machine-readable docs index; agents start here (full context: [llms-full.txt](https://gitmoot.io/llms-full.txt))
- [Agent Skills package](skills/gitmoot/SKILL.md)
- [CLI reference](skills/gitmoot/references/CLI.md)
- [Codex and Claude plugins](docs/plugins.md)
- [Local workflow walkthrough](docs/local-workflow.md)
- [Run jobs in parallel](docs/parallel-jobs.md)
- [Heartbeat schedules](docs/heartbeats.md)
- [Outbound event stream](docs/events.md)
- [Cockpit and the [orchestrate] config](docs/cockpit-orchestrate.md)
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

GitHub Actions enforces the same gate on every push to `main` and every pull
request: build, vet, and test, plus the race detector on `internal/workflow`.
