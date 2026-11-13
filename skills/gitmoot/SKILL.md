---
name: gitmoot
description: Use Gitmoot for local-first AI agent coordination across repositories, reviews, GitHub PR comments, agent subscriptions, daemon checks, stuck jobs, agent-templates, custom prompt agents, the web dashboard, per-job runtime overrides, the config-driven runtime metadata registry, and Codex, Claude Code, Kimi Code, or omp runtime workflows.
license: Apache-2.0
compatibility: Requires the gitmoot CLI, git, GitHub CLI authentication, network access to GitHub, and omp (the job runtime; shell runs no-LLM tests).
metadata:
  gitmoot-version: "0.8.8"
  source: "gitmoot/gitmoot"
---

# Gitmoot Agent Skill

Gitmoot is a local-first coordinator for AI agents working across repositories,
reviews, PR comments, and runtime workflows. Use this skill when the
user wants PR-comment agent workflows, repo-scoped agent subscriptions,
background daemon checks, omp agent startup, the Claude Code, Codex or omp plugins, structured
implementation plans, agent template workflows, custom
prompt agents, or job status.

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

Agent templates are READ-ONLY installed data (#2204). Gitmoot can inspect and
read them — `gitmoot agent template list`, `gitmoot agent template show <id>`,
and `gitmoot agent prompt <agent-or-template>` to reuse an installed template in
the current chat — but it cannot author, update, or distribute one. There is no
draft, validate, add, export, publish, pull, remote, diff, revert, or update
verb. A template that must change is edited or re-seeded in the
`agent_templates` store row by hand. See CLI.md § Agent Templates.

For background work, keep Gitmoot's resource model explicit: repo checkout
locks protect local checkouts, runtime session locks serialize delivery for the
same resumable runtime session (omp runs a fresh session per job).
The daemon default is `--workers 1`; raise it only for independent runtime
sessions or managed agent types with `max_background` greater than one.
omp is the only job runtime (`shell` remains a subscribe-only runtime for no-LLM
tests). Agents still registered on the removed `codex`, `claude`, or `kimi`
runtimes are reported as unsupported by `gitmoot agent list` and `gitmoot
doctor`, and their jobs are refused; re-register them with `--runtime omp`.

When a job is **blocked** and the user asks to "resume a blocked job", "clear a
blocker", or "what does this job need" (#682), route to
`gitmoot job gates <id>` to list the open resource gates — one per `needs[]` entry
a blocked job recorded — then `gitmoot job gates clear <id> --need "<text>"` (or
`--all`) to satisfy them. Clearing the **last** open gate auto-re-runs the blocked
job via the same machinery as `gitmoot job retry` (resume on clear, no polling).
See CLI.md § Resumable gates.

For Gitmoot health or status questions, first use the injected SessionStart
snapshot when it is present and sufficient. If more detail is needed, run the
relevant read-only Gitmoot CLI checks and answer directly from the results.
Mention `gitmoot dashboard` (or `gitmoot dashboard --web` for a browser view of
a running job tree) only after that answer, as a live monitoring
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
or `gitmoot task list --repo owner/repo --json` for task state. Use
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
`gitmoot agent ask <agent> --repo owner/repo "..."` only for
analysis, planning, or questions. To get an independent review of a pull
request, use `gitmoot review request --pr <number>` and let Gitmoot choose the
reviewer, runtime and model, deduplicate on the exact head, and wake your role
when the verdict is saved (CLI.md § Review Router). When a script needs the job
id, add `--json`: `gitmoot review request --pr <number> --head <40-hex>
--json` prints `job_id`, `agent`, `awaited_fact_id` and the
same keys `agent review --json` printed. Use `gitmoot agent review
<reviewer> --repo owner/repo --pr <number> "..."` only for a deliberate manual
dispatch. With `--org-role` it still goes through the router and keeps the
reviewer you named. A review never routes a fix; `--no-fix-target` is accepted
and ignored.

E2B reviews prepare their exact source and review base inside E2B, without a host
checkout or bulk source staging. They require the configured mTLS credential
gateway for a temporary, repository-scoped Git-read lease; the host token is
never exported. When Janitor is enabled, remaining local control-state writes
use fresh-space monitoring rather than the cleanup-target latch. Local reviews
can opt in with `[janitor_capacity] review_admission = "fresh_space"`; see CLI.md
for its best-effort limits and cancellation behavior. This changes neither
ordinary non-review admission nor provider spending/concurrency limits.

**Gitmoot does not dispatch implementation (#2203).** `agent implement`,
`--action implement`, `task run`, and `repo auto-fix`
are all gone; a `/gitmoot <agent> implement`
comment is refused. That is the product statement, not a gap: gitmoot reviews,
seats implement. Do the file changes in THIS session, then record them so the
merge gate can attribute them:
`gitmoot job record --agent <seat> --repo owner/repo --type implement
--decision implemented [--pr <number> --head-sha <sha>] [--task <task-id>]
--summary "..."`. For a fix pass on an existing open PR, push the fix on its
branch, record it that way, and ask for the next review with `gitmoot review
request --pr <number>`.

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

An agent (via `--model` on start/subscribe/type set) and an
individual job (via `--model` on ask/review) can pin a runtime model, with the
per-job value overriding the agent default. When neither pins one, a job falls back to the
runtime's configured `[runtimes.<name>].default_model`, then the runtime CLI's own
default. Agents and jobs can likewise
set reasoning effort with `--effort` or the `effort` field. Job effort
overrides agent effort, then `[runtimes.omp].default_effort`. omp receives
effort as `--thinking <level>` when the value is one of
`off|minimal|low|medium|high|xhigh|max|auto`, and no flag at all otherwise.
The `omp` runtime is a multi-provider **routing
harness**, so a few of its properties are worth
saying out loud before routing work to it: it holds `review`/`implement`/`ask`,
every job runs a fresh session (it never resumes), and
read-only seats use Gitmoot's broker-backed sandbox. Runtime-family diversity
uses the upstream provider proved by a successful OMP delivery, not the `omp`
wrapper or requested model text. It is advisory: same-family comparisons emit
`merge_gate_family_advisory` and unresolved comparisons emit
`merge_gate_family_unresolved`; neither disqualifies a distinct reviewer with
substantive evidence (see CLI.md § Agent Setup). Use `gitmoot runtime list` to inspect
each built-in
runtime's resolved metadata: capabilities, default model/effort,
known models, and the token-usage source.
Operators can override a built-in runtime's
metadata without recompiling via a `[runtimes.<name>]` config section — `default_model`
retargets delivery and `default_effort` selects omp's
`--thinking` level, while
`models`/`capabilities` stay advisory (see CLI.md
§ Runtime Metadata Registry). Use
`gitmoot plugin doctor` when checking whether Codex, Claude Code, or omp
can discover Gitmoot through an installed plugin.
Use `gitmoot workflow list`, `gitmoot
workflow show`, `gitmoot workflow show-note`, `gitmoot workflow describe`,
`gitmoot workflow note`, and `gitmoot workflow close` to
inspect external-coordinator workflow groups, set their stable description, add
verbatim journal entries, set a manual status escape hatch, optionally stage
a note in persistent memory, and end a finished group. Workflow hygiene:
`describe` a workflow at first use and `close` it (with `--reason`) when its
work merges or completes — never-closed workflows accumulate and bury live
work in the dashboard's active buckets and every workflow-picking surface. Linked PR lifecycle transitions also add deduped
`daemon` journal notes and advance live status.
Use `gitmoot message send <role> "<text>"` for ordinary fleet conversation,
`message inbox` to read received mail, `message show <id>` for a full message,
and `message reply <id> "<text>"` to reply in its thread. No workflow or job is
required; attach an existing workflow with `--workflow <label>` when useful.
Any configured role may talk to another, without acquiring authority over it.
Sender defaults to the session role/current registered pane; `--role` is an
operator attribution override, not authentication. Success means queued, not
read. Ordinary messages have no acknowledgment, completion, TTL or nag duty.
Plain journal notes do not send notifications, even if their body says `to=...`.
Escalations and directives use the same inbox, thread IDs and optional workflow
association. `gitmoot message escalate "QUESTION"` normally addresses your
coordinator; `message resolve ID --answer "ANSWER"` resolves the tracked request
and notifies its requester. Reading or discussing it does not resolve it.
`gitmoot message directive send --to ROLE "ASSIGNMENT"` requires issuer authority.
Confirmed transport receipt needs no manual acknowledgment; `message directive
ack ID` remains available for explicit acceptance. `message directive done ID`
records completion by the target subtree and ends its nudges; the issuer instead
uses `message directive cancel ID` when the work is no longer required.
All actions use session identity or `--role ROLE`, not an identity inferred from
the target. Replies and prose cannot grant authority or certify completion.
Exact-head review notices carry system provenance and remain separate from
ordinary discussion of a review. A queued or uncertain notification is not proof
of submission, reading, acceptance or completion. Jobs join a group through
`--workflow <label>` on agent
ask/review or `job open`. Use
`[workflow] require_workflow = true` to enforce this discipline. `auto` mode
files fresh unlabeled dispatches under `adhoc/<agent>-<yyyy-mm-dd>` and records
`workflow_autolabeled`; `strict` instead requires `--workflow`. Per-repo
overrides live in `[repos."owner/repo"]`, and `repo add --agents-md` scaffolds
the discipline into a checkout's AGENTS.md.

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

## Plan-Gated Implementation

For non-trivial implementation work, default to a plan-gated flow: plan first,
get the plan approved explicitly, then implement against the approved plan. The
gate is a convention over existing primitives, so it works unchanged on any
seat (omp jobs, or Claude Code and Codex sessions using the plugins).

1. **Plan read-only.** Produce a decision-complete plan with `gitmoot agent
   ask` (or the `planner` template, or a session job opened with `--type ask`).
   No file changes.
2. **Record the plan.** Post it with `gitmoot workflow note <label> "..."` and
   keep the entry id the CLI prints — that id is the plan-id.
3. **Stop.** Approval is an explicit act, never inferred from silence. In org
   mode the approver sends `gitmoot message directive send --to <role> --workflow
   <label> "approved: implement plan <plan-id> …"`; outside org mode an
   explicit human approval message referencing the plan-id serves the same
   role.
4. **Implement in your own session, quoting the plan-id, then record it.** The
   approved plan is the scope fence: work outside it needs an amended plan and
   a fresh approval, not silent expansion. Gitmoot does not dispatch
   implementation (#2203), so the seat is the implementer and `gitmoot job
   record --type implement` is its receipt.
5. **Close the loop.** The implementer marks the approval directive `done`
   when the work lands, and the workflow is `close`d when the PR merges.

A coordinator may waive the gate for trivial mechanical fixes by writing
"plan-waived" in the recorded summary. Claude Code seats may additionally use
the harness's own plan mode, but only when the approver is present at that
session: an interactive plan-approval prompt blocks invisibly — the session
appears idle, no job runs, and nothing escalates — so an unattended seat must
use the durable convention above, which is the runtime-neutral form and always
applies. Under this convention every implementation records either
a plan-id or the literal "plan-waived" in its summary; one with neither is out
of contract. See WORKFLOWS.md § Plan-Gated Implement for the full command
sequence.

## Agent Job Contract

Every Gitmoot job should return a concise and truthful `gitmoot_result` JSON
object. Use `blocked` when work cannot continue without human input or external
state, and `failed` when an attempted action errored.

For the required result shape and decision meanings, read
[RESULT_CONTRACT.md](references/RESULT_CONTRACT.md).

## Safety Rules
A read-only review seat is handed an immutable, daemon-owned copy of a Go
installation selected from the daemon's `PATH` to satisfy the effective
`go.work` and checkout-root `go.mod`. Run `go` normally: `GOROOT`, `PATH`,
`GOTOOLCHAIN=local`, and `GOWORK` are already pinned to that selection. Do not
invoke a toolchain path directly or download Go into the seat cache; that
workaround is retired. `CGO_ENABLED=0` is still required and `-race` remains
unavailable. Exit 126 means no candidate could be safely staged or satisfy the
workspace. A real review job records `seat_toolchain_unavailable` and blocks;
host-specific staging detail is also written on daemon stderr as
`gitmoot: read-only seat toolchain:`. When no Go installation exists, the event
is the only recorded reason.
The seat's `TMPDIR` is a private `/tmp/gmr-<hex>` directory removed when the
job ends, and offline Go builds read a per-repository module cache the daemon
fills before the seat starts (`GOPROXY=off`). Python, Rust, Node and
Flutter/Dart checks are not available in a local seat; report them with
`evidence: static_only` and `checks_blocked`.


Preserve existing behavior unless the job explicitly changes it. Keep work
scoped to the target repo. Do not commit generated data, caches, logs, secrets,
session archives, cloned helper repos, or large outputs unless explicitly
requested.

For detailed safety and lock rules, read [SAFETY.md](references/SAFETY.md).

## When Unsure

Reread this `SKILL.md`, then inspect `/gitmoot help`, `gitmoot status`, and the
relevant job events before acting. If this skill disagrees with the installed
binary, trust the live `gitmoot --help` / subcommand help output and treat the
skill as stale documentation that should be refreshed.
