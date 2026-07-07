---
name: gitmoot
description: Use Gitmoot for local-first AI agent coordination across repositories, goals, reviews, GitHub PR comments, agent subscriptions, daemon checks, stuck jobs, branch locks, agent-templates, template capture and publish/pull, custom prompt agents, orchestration, heartbeats, pipelines, routing telemetry, event webhooks, the web dashboard, per-job runtime overrides, the config-driven runtime metadata registry, and Codex, Claude Code, or Kimi Code runtime workflows.
license: Apache-2.0
compatibility: Requires the gitmoot CLI, git, GitHub CLI authentication, network access to GitHub, and a supported runtime such as Codex, Claude Code, or Kimi Code.
metadata:
  gitmoot-version: "0.8.1"
  source: "jerryfane/gitmoot"
---

# Gitmoot Agent Skill

Gitmoot is a local-first coordinator for AI agents working across repositories,
goals, reviews, PR comments, and runtime workflows. Use this skill when the
user wants PR-comment agent workflows, repo-scoped agent subscriptions,
background daemon checks, Codex, Claude Code, or Kimi Code agent startup, structured
implementation plans, standard goal files, agent template workflows, custom
prompt agents, template capture, native agent chat threads, job status, or branch
lock inspection. When the user wants agents and humans to converse in a durable
repo thread, tag an agent, answer a paused job's question, or turn a chat message
into a real job, use `gitmoot chat`; to convene several agents in one bounded,
hard-capped brainstorm, use `gitmoot moot` (see CLI.md § Native Chat and
WORKFLOWS.md § Chat).

For current-chat prompt import, "use <agent> here" or "use Gitmoot agent
<agent> here" means import the agent's prompt into this current chat and apply
it. This is prompt import, not true system-prompt injection. The natural phrase
"use the Gitmoot planner here" maps to the same `planner` template used by
managed planner agents. If the planner template is not cached, read and apply
the packaged `agent-templates/planner.md` instructions directly. Do not route a
"here" request through a background `gitmoot agent ask` unless the user
explicitly asks for background execution, PR-comment routing, or job tracking.

By DEFAULT, the "here" flow tracks the work: run
`gitmoot agent prompt <agent> --record [--repo owner/repo]`, which opens a
session job on import and returns the prompt with a header line naming its job
id. Apply the prompt, do the work, then — this is REQUIRED — clock out with
`gitmoot job close <id> --decision <approved|changes_requested|implemented|blocked|failed> --summary "..."`
so the work shows in `job list`, the dashboard, and the event stream just like
an engine-run job (no runtime is spawned; gitmoot is only the record-keeper).
`--record` works for both a **registered agent** (repo defaults to its repo scope)
and a bare **template** (e.g. the packaged `planner` above, when no `planner` agent
exists): a template has no repo scope, so pass `--repo owner/repo` and the session
job records the **template id** as its agent identity (#673). Omitting `--repo` for
a bare template is a clear error.
`--record` also defaults the job `--type` to `implement` — pass `--type ask` for
advisory "here" work (planning, research) so it is not mislabeled.
For a plain read-only peek — "just show me the prompt" — use
`gitmoot agent prompt <agent>` WITHOUT `--record`, which opens no job. You can
also clock in/out manually with `gitmoot job open` / `gitmoot job close`, or log
already-finished work in one shot with `gitmoot job record` (see
`references/CLI.md` → Session jobs).

For template capture, phrases like "capture this session as a Gitmoot agent
template", "turn this workflow into a Gitmoot template", or "draft a reusable
agent template from this chat" mean read [TEMPLATE_CAPTURE.md](references/TEMPLATE_CAPTURE.md)
and distill the visible current-chat context into a draft template. Gitmoot
cannot read hidden model memory or runtime internals. Do not install, overwrite,
or update a permanent template unless the user explicitly approves that step.
Use `gitmoot agent template draft <id>` for a blank scaffold,
`gitmoot agent template validate <file>` for a structural check,
`gitmoot agent template add <id> --file <file>` to install a snapshot, and
`gitmoot agent prompt <id>` to reuse the installed template in the current chat.
"Publish", "back up", or "pull" agent templates means the GitHub-backed
`gitmoot agent template export/publish/pull/remote set` commands — see CLI.md
§ Agent Templates.

For agent persistent memory, phrases like "give this agent persistent memory",
"why does my agent keep forgetting things about this repo", or "what has this
agent learned" map to Gitmoot's off-by-default agent memory feature (#626): an
enrolled agent gets a repo-filtered pool of durable facts injected into its job
prompt as a read-only "Prior learnings" reference block (never instructions).
Enrollment is per agent via `[agents.<name>].memory = true` plus an optional
`[memory]` section; inspect the store read-only with `gitmoot memory list`. See
CLI.md § Agent Memory and the "Agent Persistent Memory" concepts page for depth.

For routing telemetry, phrases like "which runtime/model works best here",
"show observed routing performance", or "should I route Go tasks to Codex" map to
Gitmoot's execution-grounded routing telemetry (#530): every job records an
additive `routing_telemetry` row at terminal, and `gitmoot router summary` reports
local observed success/approval rates by `(action, runtime, model, template)`. It
is **advisory only** — nothing auto-overrides routing — and labeled "local observed
performance, not a benchmark". An optional `[router] context_enabled = true` feeds a
bounded table into coordinator prompts. See CLI.md § Routing Telemetry.

For background work, keep Gitmoot's resource model explicit: repo checkout
locks protect local checkouts, runtime session locks serialize delivery for the
same Codex, Claude, or Kimi session, and branch locks protect implementation
ownership.
The daemon default is `--workers 1`; raise it only for independent runtime
sessions or managed agent types with `max_background` greater than one.
To restart the daemon without losing its Claude token, use
`gitmoot daemon restart` (not stop + start); see CLI.md § Repo And Daemon
Status for the runtime-auth persistence model.

When a job is **blocked** and the user asks to "resume a blocked job", "clear a
blocker", or "what does this job need" (#682), route to
`gitmoot job gates <id>` to list the open resource gates — one per `needs[]` entry
a blocked job recorded — then `gitmoot job gates clear <id> --need "<text>"` (or
`--all`) to satisfy them. Clearing the **last** open gate auto-re-runs the blocked
job via the same machinery as `gitmoot job retry` (resume on clear, no polling). A
job whose tree is **paused awaiting a human** (`escalate_human` / ask-gate) is
never auto-resumed by clearing gates — that still needs a `gitmoot resume`
decision. See CLI.md § Resumable gates.

For Gitmoot health or status questions, first use the injected SessionStart
snapshot when it is present and sufficient. If more detail is needed, run the
relevant read-only Gitmoot CLI checks and answer directly from the results.
Mention `gitmoot dashboard` (or `gitmoot dashboard --web` for a browser view of
a running orchestration) only after that answer, as a live monitoring
follow-up. Do not start daemons, create agents, change subscriptions, update
templates, or release locks unless the user asks for that action.

## Before Acting

1. Check whether `gitmoot` is installed with `gitmoot version`.
2. Confirm GitHub CLI access with `gh auth status` only before using PR
   workflows or remote GitHub actions.
3. Detect or ask for the target repo before starting daemons, subscribing agents,
   or routing jobs.
4. Do not start daemons, create agents, update agent templates, or change
   subscriptions, or release locks unless the user asks or the current task
   clearly requires it.
5. Prefer the SessionStart snapshot and read-only status commands, then answer
   directly before mutating Gitmoot state or pointing the user to live
   monitoring.

## Common Commands

Use the SessionStart "Current snapshot" for quick repo-local daemon/task/job/lock
answers when available. Use `gitmoot status --repo owner/repo` for concise repo
status, `gitmoot daemon status` for daemon state, `gitmoot agent list` and
`gitmoot agent show <agent>` for registered agents. Use `gitmoot task list --repo owner/repo`
or `gitmoot task list --repo owner/repo --json` for imported task state. Use
`gitmoot job list --repo owner/repo` for jobs, and use
`gitmoot dashboard --json` only when a structured full dashboard snapshot is
needed. Do not use nonexistent commands such as `gitmoot status --json` or
`gitmoot task show`. Use `gitmoot agent prompt <agent-or-template>` to import an
agent prompt into the current chat. Use
`gitmoot agent run <agent> --repo owner/repo "..."` for coordinator delegation
so Gitmoot can route to ask, review, or implement and own worktrees, branch
locks, commits, pushes, PRs, and workflow advancement. Use
`gitmoot agent ask <agent> --repo owner/repo "..."` only for
analysis, planning, or questions. Use `gitmoot agent review <agent> --repo
owner/repo --pr <number> "..."` for PR review decisions and `gitmoot agent
implement <agent> --repo owner/repo --task <task-id> "..."` for file changes.
Add `--background` only when the user wants a queued background job.

Orchestrate (Orchestra): when the user says "orchestrate …" or "spin up an
orchestra of agents", run a background coordinator that returns a `delegations[]`
score so the players (child agents) run and a finale (continuation) reconvenes
and synthesizes. `gitmoot orchestrate <agent> "..." [--repo R]` is sugar for
`gitmoot agent run <agent> --background "..."`. See
[RESULT_CONTRACT.md](references/RESULT_CONTRACT.md) for the delegation fields and
termination bounds. A coordinator can also spawn throwaway, auto-disposed
ephemeral workers on demand via a delegation's `ephemeral` spec (no
pre-registration; mutually exclusive with `agent`). A `synthesis_rule`
(`summary`/`vote`/`quorum`) reconciles the producers' **self-report**; to check
the combined result against the goal **independently**, add a read-only verify
leg on a **different** runtime/model that `deps` on the producer(s) — produce vs.
independent check, the same separation as ROMA's Verifier (cross-evaluation beats
self-evaluation; see the `verifier` and `decompose-and-verify` recipes and the
"produce vs. independent check" note in
[RESULT_CONTRACT.md](references/RESULT_CONTRACT.md)). An agent (via `--model` on start/subscribe/type set) and an
individual job or delegation (via `--model` on run/ask/review/implement or the
delegation `model` field) can pin a runtime model, with the per-job/delegation
value overriding the agent default. When neither pins one, a job falls back to the
runtime's configured `[runtimes.<name>].default_model` (the one behavioral registry
field), then the runtime CLI's own default. Use `gitmoot runtime list` to inspect
each built-in runtime's resolved metadata (capabilities, default/known models, and
where token usage is read from); operators can override a built-in runtime's
metadata without recompiling via a `[runtimes.<name>]` config section — `default_model`
retargets delivery, `models`/`capabilities` stay advisory (see CLI.md
§ Runtime Metadata Registry). Use
`gitmoot plugin doctor` when checking whether Codex, Claude Code, or Kimi Code
can discover Gitmoot through an installed runtime plugin. Use
`gitmoot plugin codex-launch --repo <path>` to print a Codex launch command that
adds the resolved `.gitmoot` home to the sandbox on Linux, macOS, and Windows.
Use `gitmoot goal template` when
writing a standard task-by-task goal file. Use
`gitmoot report bug --job <job-id> --preview` to inspect a redacted GitHub issue
draft for failed, blocked, or cancelled jobs; use
`gitmoot report bug --job <job-id> --create --yes` only when the user
explicitly asks you to file it or the active workflow policy permits automatic
bug filing.

For a fixed, repeatable multi-step shell flow (not a model-driven decomposition),
prefer a **pipeline** (#681) over an orchestra: `gitmoot pipeline add <spec.yaml>`
registers a declared DAG of shell stages that the daemon runs on demand
(`pipeline run`) or on an interval schedule. Each stage is an ordinary shell-runtime
job whose `gitmoot_result` decision drives advancement; a `blocked` stage parks the
run (resume with `pipeline resume <run-id>`), and a stage is a leaf (its
`delegations[]` never spawn children). Pipelines are off by default. See CLI.md
§ Pipelines, WORKFLOWS.md § Pipelines, and `docs/pipelines.md`.

For lightweight, durable agent communication that is **not** immediately work, use
**native chat** (#534): `gitmoot chat create <name> --repo owner/repo`, then
`gitmoot chat send <thread> "@agent …"` to leave a durable, `@`-tagged message in
the agent's inbox. A message is a row (free); a job is compute (explicit) — a plain
`send` never starts work. Promote a message into a real job only with
`gitmoot chat task <thread> "@agent …" [--action ask|review|implement]` (the job's
result is posted back into the thread), and answer a job paused at `awaiting_human`
with `gitmoot chat answer <thread> "<question-id>: …"`. Chat is local-only (no
network). For the agent-to-agent V1.5 layer, an enrolled agent can auto-answer an
`@mention` via the off-by-default `[chat] auto_respond` sweep (one bounded read-only
`ask` per mention, hard-capped), and `gitmoot moot <name> "topic" --agents a,b,c
--repo owner/repo` convenes agents as seats in one brainstorm — one job per seat,
hard-stopping at a message cap (`moot_max_seats` default 6, `moot_message_cap`
default 30). See CLI.md § Native Chat (V1.5) and WORKFLOWS.md § Chat.

The plugin is only the runtime discovery surface for this skill. Local agent
invocation still goes through the `gitmoot` CLI and the same registered agent,
repo access, runtime adapter, and job history model used by PR-comment jobs.

For Gitmoot bug reports, preview by default and treat the preview as the
confirmation surface. Generated reports include redacted job context, recent
events, labels, and a fingerprint marker for duplicate detection. After
creation, tell the user the printed issue URL; if Gitmoot reused an existing
issue, report that URL as existing instead of new.

For SkillOpt template learning, prefer the high-level
`gitmoot skillopt train start/status/continue/stop` workflow when the user wants
Gitmoot to enforce the full feedback, optimizer, candidate-review, and
promotion loop. Use low-level `gitmoot skillopt review`, `feedback`, `export`,
`import`, and `candidate` commands for advanced/debug work or when recovering a
specific step. In train mode, collect enough ranked feedback and trait notes
before optimizer handoff, check `gitmoot-skillopt --version` and
`gitmoot-skillopt optimize --help` when optimizer-backed continue is needed,
keep promotion decisions explicit, and start follow-up iterations only through
`train continue --start-next`. Generation is durable: each review item's
artifacts and options commit in one transaction the moment that item finishes,
so an interrupted phase resumes idempotently when you re-run `train continue` —
already-complete items are skipped and never duplicated, while an item with some
but not all options persisted returns a hard error to inspect or clear before
continuing. For optimizer-phase recovery, use
`gitmoot skillopt train recover --session <id> [--out-root path] [--json]`,
which re-imports or repairs the optimizer candidate package and classifies the
iteration; it does not release the generation lock or rebuild generation
options. To improve the *judge's rubric* (not the model) from accumulated human
feedback, run `gitmoot skillopt rubric induce --template <id>` — an offline,
deterministic tool that induces a criterion-separated rubric from captured
trait feedback, meta-evaluates it for coverage/redundancy, and writes a frozen
JSON for human review; it never auto-injects. To generate Autodata-style
synthetic review items, use the explicit, off-by-default `gitmoot skillopt synth
--template <id> --repo owner/repo --weak <agent> --strong <agent> [--judge
<agent>]`: it keeps only items a strong agent beats a weak agent on and a judge
deems well-formed, stores them `pending_human_approval`, and requires
`gitmoot skillopt synth approve <item-id>` before an item may be used — nothing
runs it automatically.

For complete command examples, read [CLI.md](references/CLI.md).
For end-to-end workflows, read [WORKFLOWS.md](references/WORKFLOWS.md).
For current-chat template capture, read
[TEMPLATE_CAPTURE.md](references/TEMPLATE_CAPTURE.md).
For the canonical goal prompt template, read
[GOAL_TEMPLATE.md](references/GOAL_TEMPLATE.md) only when the user asks for a
goal file.

## Agent Job Contract

Every Gitmoot job should return a concise and truthful `gitmoot_result` JSON
object. Use `blocked` when work cannot continue without human input or external
state, and `failed` when an attempted action errored.

For the required result shape and decision meanings, read
[RESULT_CONTRACT.md](references/RESULT_CONTRACT.md).

## Safety Rules

Preserve existing behavior unless the job explicitly changes it. Keep work
scoped to the target repo. Do not commit generated data, caches, logs, secrets,
session archives, cloned helper repos, or large outputs unless explicitly
requested. Respect Gitmoot branch locks for implementation jobs.

For detailed safety and lock rules, read [SAFETY.md](references/SAFETY.md).

## When Unsure

Reread this `SKILL.md`, then inspect `/gitmoot help`, `gitmoot status`, and the
relevant job events before acting.
