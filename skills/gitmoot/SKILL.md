---
name: gitmoot
description: Use Gitmoot for local-first AI agent coordination across repositories, goals, reviews, GitHub PR comments, agent subscriptions, daemon checks, stuck jobs, branch locks, agent-templates, template capture and publish/pull, custom prompt agents, orchestration, heartbeats, pipelines, pipeline chaining through the localhost bridge, routing telemetry, event webhooks, the web dashboard, per-job runtime overrides, the config-driven runtime metadata registry, and Codex, Claude Code, Kimi Code, or omp runtime workflows.
license: Apache-2.0
compatibility: Requires the gitmoot CLI, git, GitHub CLI authentication, network access to GitHub, and a supported runtime such as Codex, Claude Code, Kimi Code, or omp.
metadata:
  gitmoot-version: "0.8.8"
  source: "gitmoot/gitmoot"
---

# Gitmoot Agent Skill

Gitmoot is a local-first coordinator for AI agents working across repositories,
goals, reviews, PR comments, and runtime workflows. Use this skill when the
user wants PR-comment agent workflows, repo-scoped agent subscriptions,
background daemon checks, Codex, Claude Code, Kimi Code, or omp agent startup, structured
implementation plans, standard goal files, agent template workflows, custom
prompt agents, template capture, job status, or branch lock inspection. When a job
pauses at `awaiting_human`, answer it locally with `gitmoot job answer <job-id>
"<question-id>: text"` (see CLI.md § Jobs).

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
`gitmoot job close <id> --decision <approved|changes_requested|implemented|blocked|failed|skipped> --summary "..."`
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

For an in-session PR review, clock in with `--type review --pr <n> --head-sha
<sha> --workflow <label>` and journal progress with `gitmoot workflow note
<label> "..."`. `job list` and `job show` expose the resulting liveness hint as
`review_status`, alongside `review_status_grade: reported` and
`review_status_authority: non_authoritative`. This is display-only,
caller-reported activity: it does not satisfy, block, or otherwise feed the
merge gate. Close the session job after posting the verdict. A
`changes_requested` review close must pass `--severity P0|P1|P2|P3`.

For a running engine-dispatched review with an isolated worktree, `job list`
uses the verified daemon's descendant process tree instead: a runtime descendant
owning that worktree is `in_progress`, while its conclusive sampled absence is
`stalled`. That system observation is graded `observed` but remains explicitly
`non_authoritative`; it triggers no cancellation or recovery policy.

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
The block can include `[linked]` facts reached from persisted memory links, and
non-empty blocks end with a footer pointing the agent to
`gitmoot memory recall "<query>" --agent <agent-name>` for on-demand recall.
Live prompt injection and direct recall hits maintain best-effort per-fact usage
counters; preview/eval reads and linked-only recall expansion do not count.
Enrollment is per agent via `[agents.<name>].memory = true` plus an optional
`[memory]` section; inspect the store read-only with `gitmoot memory list`. For
owner-curated memory, `gitmoot memory ingest` stages Markdown as pending
observations, `gitmoot memory observations` lists them, `gitmoot memory confirm`
promotes selected observations, and `gitmoot memory groom` proposes or applies
deterministic retirements. See CLI.md § Agent Memory and the "Agent Persistent
Memory" concepts page for depth.

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
Claude runtime auth lives in `runtime-auth.env`; rotate it with `gitmoot auth
set claude` and clear it with `gitmoot auth unset claude`. Adapter builds read
the file per delivery, so no daemon restart is needed. See CLI.md for the
single-source model.

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
6. If the user names a Gitmoot concept or command that is version-sensitive or
   missing from this skill, verify the live surface with `gitmoot --help` and
   the relevant `gitmoot <command> --help` before answering or acting.

## Common Commands

Use the SessionStart "Current snapshot" for quick repo-local daemon/task/job/lock
answers when available. Use `gitmoot status --repo owner/repo` for concise repo
status, `gitmoot daemon status` for daemon state, `gitmoot agent list` and
`gitmoot agent show <agent>` for registered agents. Use `gitmoot task list --repo owner/repo`
or `gitmoot task list --repo owner/repo --json` for imported task state. Use
`gitmoot job list --repo owner/repo` for jobs, and use
`gitmoot dashboard --json` only when a structured full dashboard snapshot is
needed. The read-only web dashboard's Overview and Org pages distinguish live
Herdr session activity from engine jobs and label a missing session differently
from an unavailable Herdr source. Do not use nonexistent commands such as
`gitmoot status --json` or
`gitmoot task show`. Use `gitmoot org events rule add|list|set-scope|rm` to manage opt-in
organization event wakes; add validates the event kind, target org role, and
`addressed` (default) or `observer` scope. Use
`gitmoot agent prompt <agent-or-template>` to import an
agent prompt into the current chat. Use
`gitmoot agent run <agent> --repo owner/repo "..."` for coordinator delegation
so Gitmoot can route to ask, review, or implement and own worktrees, branch
locks, commits, pushes, PRs, and workflow advancement. Add `--action
ask|review|implement` when that job action must be explicit; `--type` is
independent and selects a managed agent type, not an action. Use
`gitmoot agent ask <agent> --repo owner/repo "..."` only for
analysis, planning, or questions. Use `gitmoot agent review <reviewer> --repo
owner/repo --pr <number> --lead <implementer> "..."` for PR review decisions;
the lead must be a registered, repo-allowed agent with `implement` capability
and a write-granting policy so requested changes can route to it. Use `gitmoot agent
implement <agent> --repo owner/repo --task <task-id> "..."` for file changes.
For a fix pass on an existing open PR, use `agent implement --pr <number>` (or
`agent run --action implement --pr <number>`); Gitmoot validates that the PR is
open, belongs to the same repository, and matches the existing task branch
before reusing its task worktree and PR.
Before local review dispatch or native engine review fan-out, Gitmoot refuses a
homogeneous succeeded decision repeated at the exact same repo/PR/head, emits
`review_loop_detected` on the matched succeeded job, and hard-errors (CLI) or
blocks (engine). New heads and mixed same-head decisions proceed; the loop guard
allows an empty engine event only before succeeded history exists, while local
CLI preparation still requires a concrete head. Treat the old verdict only as
escalation evidence—never return it as a cached review result. This exact key is
used instead of a round counter because #1419's panel rejected round-based
instruments. Direct PR-comment ingress remains separate #1433 work.
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
runtime's configured `[runtimes.<name>].default_model`, then the runtime CLI's own
default. Codex agents, jobs, delegations, and ephemeral worker specs can likewise
set reasoning effort with `--effort` or the `effort` field. Job/delegation effort
overrides agent/worker effort, then `[runtimes.codex].default_effort`, and Gitmoot
forwards the free-form value as `-c model_reasoning_effort=<value>`. omp receives
effort as `--thinking <level>` when the value is one of
`off|minimal|low|medium|high|xhigh|max|auto`, and no flag at all otherwise.
Claude and Kimi ignore effort. The `omp` runtime is a multi-provider **routing
harness**, so a few of its properties differ from the vendor CLIs and are worth
saying out loud before routing work to it: it holds `review`/`implement`/`ask`
but not `produce`, every job runs a fresh session (it never resumes), read-only
is enforced Gitmoot-side rather than by the runtime's approval flag, and its
implement jobs get a LOUD cross-family-review refusal instead of a silent skip
because an opaque router has no model family (see CLI.md § Agent Setup). Use
`gitmoot runtime list` to inspect each built-in runtime's resolved metadata:
capabilities, default model/effort, known models, and the token-usage source.
Operators can override a built-in runtime's
metadata without recompiling via a `[runtimes.<name>]` config section — `default_model`
retargets delivery and `default_effort` selects Codex effort and omp's
`--thinking` level, while
`models`/`capabilities` stay advisory (see CLI.md
§ Runtime Metadata Registry). Use
`gitmoot plugin doctor` when checking whether Codex, Claude Code, or Kimi Code
can discover Gitmoot through an installed runtime plugin. Use
`gitmoot plugin codex-launch --repo <path>` to print a Codex launch command that
adds the resolved `.gitmoot` home to the sandbox on Linux, macOS, and Windows.
Use `gitmoot goal template` when
writing a standard task-by-task goal file. Use `gitmoot workflow list`, `gitmoot
workflow show`, `gitmoot workflow show-note`, `gitmoot workflow describe`,
`gitmoot workflow note`, and `gitmoot workflow close` to
inspect external-coordinator workflow groups, set their stable description, add
verbatim journal entries, set a manual status escape hatch, optionally stage
a note in persistent memory, and end a finished group. Workflow hygiene:
`describe` a workflow at first use and `close` it (with `--reason`) when its
work merges or completes — never-closed workflows accumulate and bury live
work in the dashboard's active buckets and every workflow-picking surface. Linked PR lifecycle transitions also add deduped
`daemon` journal notes and advance live status.
In org mode, `gitmoot org message send --to <role> --workflow <label>
"<message>"` gives distinct same-parent siblings a durable sender-attributed
heads-up. It creates no acknowledgment, completion, TTL, or nag obligation.
In org mode, obligations and questions have a full lifecycle and closing it is
part of the work: `gitmoot org directive send --to <role> --workflow <label>`
mints a tracked, TTL-nudged obligation; `org directive ack <id>` records
RECEIPT only; `org directive done <id>` records COMPLETION and ends the
obligation with its nudges (target subtree only — the sender cancels with
`org directive cancel` instead). Finished work left un-`done` stays an outstanding
obligation on every owed-work surface (and, where completion TTLs are
enabled, keeps nudging — and finally escalating — over work that was
already delivered). Symmetrically, answer an
escalation on its merits and then close it with
`gitmoot org escalate resolve <note-id> [--by <role>]` — an
answered-but-unresolved escalation stays on every owed-work surface and camouflages real
blocks. Jobs join a group through
`--workflow <label>` on agent
ask/run/review/implement, orchestrate, or `job open`; orchestration descendants
inherit the label automatically. Use
`[workflow] require_workflow = true` to enforce this discipline. `auto` mode
files fresh unlabeled dispatches under `adhoc/<agent>-<yyyy-mm-dd>` and records
`workflow_autolabeled`; `strict` instead requires `--workflow`. Per-repo
overrides live in `[repos."owner/repo"]`, and `repo add --agents-md` scaffolds
the discipline into a checkout's AGENTS.md. Use
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

When a job pauses at `awaiting_human`, the local (non-PR) answer path is
`gitmoot job answer <job-id> "<question-id>: text"`. It resumes through the same
escalation-resume engine the daemon's PR-comment `answer` verb uses.

The plugin is only the runtime discovery surface for this skill. Local agent
invocation still goes through the `gitmoot` CLI and the same registered agent,
repo access, runtime adapter, and job history model used by PR-comment jobs.

For Gitmoot bug reports, preview by default and treat the preview as the
confirmation surface. Generated reports include redacted job context, recent
events, labels, and a fingerprint marker for duplicate detection. After
creation, tell the user the printed issue URL; if Gitmoot reused an existing
issue, report that URL as existing instead of new.

For complete command examples, read [CLI.md](references/CLI.md).
For end-to-end workflows, read [WORKFLOWS.md](references/WORKFLOWS.md).
For current-chat template capture, read
[TEMPLATE_CAPTURE.md](references/TEMPLATE_CAPTURE.md).
For the canonical goal prompt template, read
[GOAL_TEMPLATE.md](references/GOAL_TEMPLATE.md) only when the user asks for a
goal file.

## Plan-Gated Implementation

For non-trivial implementation work, default to a plan-gated flow: plan first,
get the plan approved explicitly, then implement against the approved plan. The
gate is a convention over existing primitives, so it works unchanged on Codex,
Claude, Kimi, and omp seats.

1. **Plan read-only.** Produce a decision-complete plan with `gitmoot agent
   ask` (or the `planner` template, or a session job opened with `--type ask`).
   No file changes, no implement dispatches.
2. **Record the plan.** Post it with `gitmoot workflow note <label> "..."` and
   keep the entry id the CLI prints — that id is the plan-id.
3. **Stop.** Approval is an explicit act, never inferred from silence. In org
   mode the approver sends `gitmoot org directive send --to <role> --workflow
   <label> "approved: implement plan <plan-id> …"`; outside org mode an
   explicit human approval message referencing the plan-id serves the same
   role.
4. **Implement quoting the plan-id.** The approved plan is the scope fence:
   work outside it needs an amended plan and a fresh approval, not silent
   expansion.
5. **Close the loop.** The implementer marks the approval directive `done`
   when the work lands, and the workflow is `close`d when the PR merges.

A coordinator may waive the gate for trivial mechanical fixes by writing
"plan-waived" in the dispatch prompt. Claude Code seats may additionally use
the harness's own plan mode, but only when the approver is present at that
session: an interactive plan-approval prompt blocks invisibly — the session
appears idle, no job runs, and nothing escalates — so an unattended seat must
use the durable convention above, which is the runtime-neutral form and always
applies. Under this convention every implement dispatch prompt carries either
a plan-id or the literal "plan-waived"; a dispatch with neither is out of
contract. See WORKFLOWS.md § Plan-Gated Implement for the full command
sequence.

## Agent Job Contract

Every Gitmoot job should return a concise and truthful `gitmoot_result` JSON
object. Use `blocked` when work cannot continue without human input or external
state, and `failed` when an attempted action errored.

For the required result shape and decision meanings, read
[RESULT_CONTRACT.md](references/RESULT_CONTRACT.md).

## Safety Rules
A read-only review seat is handed an immutable, daemon-owned COPY of the
operator-pinned Go toolchain: run `go` normally, with `GOROOT`, `PATH` and
`GOTOOLCHAIN=local` already set. Do NOT invoke a toolchain path directly and do
not download Go into the seat cache; that workaround is retired. `CGO_ENABLED=0`
is still required and `-race` is still unavailable in a seat. If `go` returns exit
126 the copy was not staged, and the reason is on the daemon's stderr
(`gitmoot: read-only seat toolchain:`) rather than in the job's events.


Preserve existing behavior unless the job explicitly changes it. Keep work
scoped to the target repo. Do not commit generated data, caches, logs, secrets,
session archives, cloned helper repos, or large outputs unless explicitly
requested. Respect Gitmoot branch locks for implementation jobs.

For detailed safety and lock rules, read [SAFETY.md](references/SAFETY.md).

## When Unsure

Reread this `SKILL.md`, then inspect `/gitmoot help`, `gitmoot status`, and the
relevant job events before acting. If this skill disagrees with the installed
binary, trust the live `gitmoot --help` / subcommand help output and treat the
skill as stale documentation that should be refreshed.
