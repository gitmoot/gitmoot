# Gitmoot Workflows

## Orchestrate vs. workflow: who is the manager?

Two ways to run multi-job efforts, answering one question differently: who
decides the next step? `gitmoot orchestrate` makes GITMOOT the manager — a
coordinator agent JOB returns `delegations[]` and the engine executes the tree
with its guardrails (dependency ordering, retries, timeouts, failure policies,
synthesis, tree token budgets, kill scope). `--workflow <label>` makes an
EXTERNAL coordinator the manager — a Claude/Codex session, a script, or a human
drives independent jobs, judging results between steps; the label plus the
`workflow note` journal make that project visible (workflow list/show, Galaxy
hubs, `/workflows/<label>`) and the journal keeps its body verbatim.
Use `orchestrate` when the plan fits a declarable tree run unattended
(parallel fan-out, bounded autonomous work); use
an external coordinator with `--workflow` when judgment between steps is the
point (build-review-fix loops, test-and-decide sequences, git/PR ownership).
They compose: `orchestrate --workflow x` labels the whole tree, so a workflow
can contain orchestrations. `--workflow` is visibility-only by design — never
scheduling, locking, budgets, or lifecycle.

## External-coordinator workflows

Use `--workflow <label>` when an external coordinator needs one durable group
across Gitmoot jobs without lifecycle state. The flag works on agent
ask/run/review/implement, `orchestrate`, and `job open`; orchestration children
and continuations inherit it.

```sh
gitmoot orchestrate planner "Run release checks." --repo owner/repo --workflow release-42
gitmoot workflow describe release-42 "Validate and ship release 42."
gitmoot workflow note release-42 "Kickoff." --author operator --status "Release checks running"
gitmoot workflow note release-42 "Canary passed." --author operator
gitmoot workflow show release-42
gitmoot workflow close release-42 --reason "Release 42 shipped and verified."
```
Before any job exists, explicitly register durable intent with
`gitmoot workflow register <label> "<description>"`. Repeating registration
does not overwrite existing intent. The registered workflow appears in workflow
reads and accepts notes, messages and escalations without a dummy job.
`workflow note` reports a journal save, not message delivery; use
`message send ROLE "TEXT"` when a recipient needs a notification. Ordinary
messages do not need a workflow; `--workflow LABEL` optionally associates one.


`description` is the stable human "what/why" line. Gitmoot seeds it on the first
note from a referenced issue title available in local workflow jobs, otherwise
the first note sentence, otherwise the campaign portion of the label. Override
it with `workflow describe <label> "<text>"`. The legacy note flag `--summary`
remains an alias for description and mirrors the value into the retained
`summary` field for older clients; omitting it preserves both, while an explicit
empty value clears them until a later note seeds description again.

`status` is the live line. `workflow note --status "..."` is the manual escape
hatch. For workflow-linked PRs, the daemon writes visibly marked `[auto:pr:...]`
notes as author `daemon` and updates status at open, checks-green/ready-to-merge,
and merged or closed-without-merging transitions. Repeated polls do not duplicate
the same workflow/PR/transition note. Description and status are stored verbatim
with independent 300-byte limits.

When `workflow note` runs inside Herdr and `--pane`, `--session`, or `--workdir`
is omitted, it fills each omitted value from `herdr pane current --current`.
The pane label, full agent session UUID, and pane working directory become the
latest handoff; explicit flags always win. Use `--no-auto` to disable this for a
script. Lookup failures are ignored so the note still lands, and author is never
inferred. The dashboard only builds a resume command from a full UUID.

In `gitmoot dashboard --web`, labeled jobs cluster around workflow hubs in
Galaxy; a labeled run links to `/workflows/<label>`, which shows the complete
run forest, state totals, best-effort token totals, and shared journal.
The dashboard Config page also includes a read-only, names-only Keychain section
with live file status, registry modes, and configured proxy placement.
Credential values and value-derived data are never projected.

The append-only journal stores text and authors verbatim. JSON show output keeps
them verbatim; terminal text output sanitizes escapes/control bytes and caps each
field to one line. V1 has no group lifecycle controls and allows reuse.

## First Repo Setup

The supported one-liner is `gitmoot setup`, which registers the repo and an
agent in one command (`--watch-issues` defaults on, so the daemon comes up
tagging-ready). `--repo`, `--agent`, `--runtime`, and `--session` are all
**required** (setup exits with an error if any is missing); `--session` takes a
runtime session reference, `last`, or a shell command:

```sh
gitmoot setup --repo owner/repo --agent reviewer --runtime codex --session last --start-daemon
```

Or the manual path:

1. Confirm the repo identity and GitHub auth.
2. Run `gitmoot doctor` to validate `gh auth` and runtime credentials before
   anything can stall on them.
3. Start the daemon.
4. Start or subscribe at least one agent.
5. Verify the agent is healthy before asking PR comments to route jobs.

```sh
gh repo view --json nameWithOwner
gh auth status
gitmoot doctor
gitmoot daemon start
gitmoot agent start reviewer --runtime codex --repo owner/repo --role reviewer --capability ask --capability review --start-daemon
gitmoot agent doctor reviewer
```

Local `agent review` / review-resolved `agent run` and native engine review
fan-out enforce an exact-head loop guard before a new review job is created. An
homogeneous succeeded decision history for `(repo, PR, head_sha)` is refused and
emits one `review_loop_detected` event on the matched succeeded job; the CLI
hard-errors while the engine blocks the task. A new commit or mixed decisions at
one head proceeds. The loop guard permits an empty engine event only before any
succeeded history exists; local CLI preparation still requires a concrete head.
The old verdict is evidence for escalation, never a cached response. An admitted
local review then receives a per-job detached read-only worktree at the requested
head. Allocation is fail-closed and requires a measurable 5 GiB free-space floor;
the stable Task row remains lifecycle identity rather than a shared checkout.
This exact decision-bearing key replaces no round counter: #1419's panel
explicitly rejected that instrument. Direct PR-comment ingress is unchanged in
this safe half and remains #1433 work.

## Review Agent From A PR Comment

1. Register a reviewer agent for the target repo.
2. Ensure the daemon watches the same repo.
3. Comment on the PR.
4. Inspect job status if no result appears.

```text
/gitmoot reviewer review
```

Gitmoot removes triple-backtick and tilde fenced code blocks plus closed inline
backtick spans before considering comment commands. It then verifies through
GitHub that the comment author currently has `write`, `maintain`, or `admin`
permission on the repository before the remaining command text reaches the
parser. Ordinary prose and code examples produce no Gitmoot reply; a malformed
command from an authorized author produces a visible routing error. On a pull
request, a line that is addressed by shape but names an unimplemented action —
as source code does routinely, since `@Published private(set) var state =
.uninitialized` matches the `@<agent> <action>` form — produces no reply at all
and is logged instead, while a recognized action with a bad argument still
replies. On an issue only `ask` is acted on, so any other action is dropped
before dispatch with no reply and no log entry. An unclosed
fenced code block treats the remainder of the comment as code, so any later
command is ignored without a reply.

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
```

## Built-In Thermo Review Agent

Use the thermo template for strict review-only work. It should not implement code
or request implementation capability.

```sh
gitmoot agent start thermo-review --runtime codex --repo owner/repo --template thermo-nuclear-code-quality-review --start-daemon
```

For a local dispatch, pair that reviewer with the registered implementer that
should own a `changes_requested` fix:

```sh
gitmoot agent review thermo-review --repo owner/repo --pr 12 --lead lead "Review this PR."
```

Gitmoot refuses the dispatch before creating a review job when the lead is
missing, cannot access the repo, lacks `implement`, or has a non-write policy.
Do not grant `implement` to the reviewer merely to satisfy this check; keep the
reviewer read-only and name the separate implementer with `--lead`.

PR comment:

```text
/gitmoot thermo-review review
```

## Custom Prompt Agent

Use custom prompt agent templates for project-specific reviewers or helpers.
Templates are read-only installed data (#2204): Gitmoot cannot author one, so the
`agent_templates` row is seeded or edited directly, and `agent template list`
confirms it is installed before an agent references it.

```sh
gitmoot agent template list --tag review
gitmoot agent start frontend-reviewer \
  --runtime codex \
  --repo owner/repo \
  --template frontend-reviewer \
  --role reviewer \
  --capability ask \
  --capability review
```

Template content is snapshotted into each queued job. A job created before a row
was edited keeps the prompt it was created with; only later jobs see the change.

## Current-Chat Planner

Use the same `planner` template in the current Codex or Claude chat when the
user wants a fast implementation plan and the current session already has the
repo context. Load the prompt with `gitmoot agent prompt planner` when it is
cached, or read the packaged `agent-templates/planner.md` instructions from the
Gitmoot skill package. Inspect only the relevant files, use web search only for
current external contracts or best-practice claims, and return the plan directly
in chat.

```text
Use the Gitmoot planner here. Write a task-by-task implementation plan for this feature.
```

## Plan-Gated Implement

Gate implementation on an approved plan when the change is non-trivial. The
gate composes existing primitives — an ask job, a workflow note, an org
directive — so it works unchanged on Codex, Claude, and Kimi seats, and the
approval survives restarts because every step is durable state rather than
conversation.

```sh
# 1. Plan, read-only. No file changes, no implement dispatches.
gitmoot agent ask planner-agent --repo owner/repo --workflow feature-42 \
  "Write a task-by-task implementation plan for <feature>."

# 2. Record the plan. The printed entry id is the plan-id.
gitmoot workflow note feature-42 "PLAN: <the complete plan text>"
# The note must contain the complete plan — it is the text the approval
# binds to and the fence the implementer is held inside.
# -> noted workflow feature-42 as entry 1234

# 3. Approve explicitly (org mode). Approval never comes from silence:
#    the directive is tracked, TTL-nudged, and visible until completed.
GITMOOT_ORG_ROLE=<approver-role> gitmoot message directive send \
  --to implementer-role --workflow feature-42 \
  "approved: implement plan 1234 as written; the plan is the scope fence"

# 4. Implement in the implementer seat's OWN session, quoting the approved
#    plan-id, then record the work. Gitmoot does not dispatch implementation
#    (#2203), so the seat is the implementer and the job row is its receipt.
gitmoot job record --agent builder --repo owner/repo --workflow feature-42 \
  --type implement --decision implemented \
  --title "Plan 1234" \
  --summary "Implemented plan 1234 (workflow note in feature-42), inside its
   fence; work outside the plan needs an amended plan and a fresh approval."

# 5. Completion ends the directive; closing the workflow is a separate action.
#    Message and receipt traffic does not reopen closed workflows.
gitmoot message directive done <directive-id> --role implementer-role
gitmoot workflow close feature-42 --reason "Plan 1234 implemented and merged."
```

Outside org mode, step 3 is an explicit human approval message referencing the
plan-id; the rest is unchanged. A coordinator may waive the gate for trivial
mechanical fixes by writing "plan-waived" in the recorded summary — the waiver
is deliberate and visible, never implied. Under this convention every
implementation carries either a plan-id or the literal "plan-waived" in its
recorded summary; one with neither is out of contract. Prefer this
durable handshake over an
interactive plan-approval prompt for any unattended seat: a session blocked on
an in-pane approval modal reports idle, runs no job, and escalates nothing, so
every fleet channel reads it as healthy while it waits. The `planner` template knows this
handshake: in coordinator mode it posts its plan as a workflow note, reports
the plan-id, and stops until an approval referencing that id arrives.

## Current-Chat Custom Agent Prompt

Use a registered agent or custom template in the current chat when the user says
something like:

```text
Use frontend-reviewer here. Review this diff.
```

Resolve and load the prompt with:

```sh
gitmoot agent prompt frontend-reviewer
```

Treat the returned content as instructions for the current chat. This is prompt
import, not true system-prompt injection, and it does not create a Gitmoot job,
start a daemon, resume a runtime session, or post a PR comment. If the user
wants tracked background execution, use `gitmoot agent ask <agent> --background`
instead.

## Background Planner Agent

Use the planner template when the user wants a structured implementation plan to
run as a tracked Gitmoot background agent job.

```sh
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

Ask from a PR comment:

```text
/gitmoot project-planner ask Write a task-by-task implementation plan for this feature.
```

Ask directly from a local Codex or Claude Code chat by having the runtime call
the Gitmoot CLI when the user explicitly wants a registered background-capable
agent path:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write a task-by-task implementation plan for this feature."
gitmoot job watch <job-id>
```

If the Codex plugin exposes a Gitmoot command bridge in chat, the equivalent
form is `$gitmoot:gitmoot agent ask project-planner --repo owner/repo --background "..."`. The
important part is that background planner work goes through `gitmoot agent ask`;
fast "here" planning stays in the current chat and uses `gitmoot agent prompt`
only to read prompt content.

## Answering A Paused Job

When an agent returns `human_questions[]`, the engine pauses the tree at
`awaiting_human` (#445). The local (non-PR) way to answer it is:

```sh
gitmoot job answer <job-id> "<question-id>: answer text"
```

It routes the answer onto the existing resume path
(`ResolveEscalation(answer)`), enqueuing the coordinator continuation that
carries the answer — the same engine path the daemon's PR-comment `answer` verb
uses. Answering a job with no pending question is a clear refusal, not a false
success. See `CLI.md § Jobs` for every flag.

## Execution Model

Use `here` when the current chat should answer directly from the Gitmoot skill.
Use `background` when Gitmoot should create a tracked job, store events, and run
through a runtime adapter.

Background jobs are scheduled against three distinct resources:

- repo checkout mutexes for daemon ticks that use the same local checkout;
- runtime session locks keyed as `runtime:<runtime>:<runtime_ref>` for Codex,
  Claude, and Kimi delivery;
- branch locks for implementation ownership and merge safety.

Delivery is self-healing — a job whose events show one of these errors and then
a success is working as designed, not flaky:

- **Dead pinned session (#443):** when a Claude `--resume` target no longer
  exists, delivery retries on a fresh session and re-pins the agent to it.
- **Transient auth errors (#487/#509):** a transient 401 ("socket connection
  closed unexpectedly") under concurrency is retried with backoff; the old
  session is not abandoned.
- **Malformed output (#495):** an agent reply missing the `gitmoot_result`
  envelope records a `malformed_output` event and is re-asked with a repair
  prompt a bounded number of times before failing terminally.

**Dead implement recovery is gone, with its cause:** Gitmoot no longer
dispatches implementation (#2203), so there is no Gitmoot-owned implement job
that can die between editing a task worktree and opening the PR, and therefore
no `task run`/`task recover` pair to restart or finalize one. A seat that dies
mid-work left its own worktree dirty: finish or discard it with ordinary git in
that worktree, then record the outcome with `gitmoot job record --type
implement`.

**Task dismissal and stale reconciliation:** `dismissed` is a terminal task
state reached only through implicit workflow transitions; no command dismisses a
task. The daemon preserves the branch and worktree, releases the branch lock
best-effort, and audits each transition as `task_dismissed_auto` (or
`task_dismissed_planned_ttl`) in `gitmoot task events <id>`.

Each repo poll reads a bounded oldest-first stale window and processes up to 20
qualifying `implementing` tasks whose `updated_at` predates
`[workflow].stale_task_ttl` (default `168h`; `"0"` disables the leg).
`updated_at` is deliberately a conservative activity proxy, not proof of
abandonment. A candidate is skipped for a live job, a same-repo open-PR branch,
or an exact branch still present on `origin`; remote lookup uncertainty skips
mutation. A branchless candidate needs no remote lookup. Retrying one of a
dismissed task's jobs restores it and records its own recovery event. The
server-side task board omits dismissed rows immediately.

`blocked` and `awaiting_human_merge` use a separate evidence-based disposal
ladder at that TTL: own PR merged -> `merged`; a later merged task/PR on the same
referenced issue -> `superseded`; referenced issue/PR closed -> `superseded`;
otherwise -> terminal `stranded`. Git ancestry is never evidence because a
superseding branch need not be an ancestor of main. An open
`awaiting_human_merge` PR stays protected and reaches `stranded` for human
inspection. Every outcome records its tier and reason on the task; the stranded
fallback writes at most one durable escalation, and an unavailable route is
recorded without keeping the task alive. Query with `gitmoot task list --state
stranded --json`.

The periodic blocked-task alert is bounded independently: three
interval-separated nudges, one terminal escalation, then no further alerts for
that episode. Its persisted exhausted stamp does not dispose the task; disposal
still requires the evidence ladder or its TTL fallback.

Delegation worktrees use a separate default-on retention policy:
`[workflow].delegation_worktree_ttl = "72h"` (`"0"` disables it). Only final
owners (`succeeded`, `failed`, `cancelled`) older than the TTL are force-removed.
Blocked, queued, and running owners remain pinned; `gitmoot doctor` reports the
reclaimable/pinned/unproven counts and logical size.
Candidate-local lookup, runner, and removal failures skip only that worktree;
later candidates continue. The daemon logs three failures per path before
suppressing repeats. A restart-safe cleanup obligation delays retries by one
minute and becomes terminal `quarantined` after the third failure; doctor and
`/api/health` report that count. Use `gitmoot job cleanup list --state
quarantined` to inspect identities and paths, then `gitmoot job cleanup reopen
<resource-id>` after repair. Candidate-query and store-wide lookup failures
remain fatal.

Never-started `planned` tasks use a separate opt-in policy:
`[workflow].planned_ttl = "720h"`. It is disabled by default; unset, empty,
zero, and invalid values all resolve to off because automatic dismissal can
destroy human planning context that nothing else reconstructs.
When enabled, it reuses the same live-job, same-repo open-PR, remote-branch,
and remote-uncertainty skips and records `task_dismissed_planned_ttl`. Task
worktree allocation claims `planned -> implementing` with a write-time CAS, so
a concurrent TTL dismissal cannot be overwritten; a job retry restores it.

A clean closed-unmerged PR moves `pr_open`, `reviewing`, or
`changes_requested` to `blocked` with `pr_closed_unmerged`; ambiguous PR state
does not advance or unblock anything. After advancement and delegation handling,
a terminal top-level implement job with no PR and no live successor blocks an
otherwise-stuck `implementing` task. Implemented success without a PR records
`task_blocked_terminal_no_pr`; other terminal outcomes record
`task_blocked_job_failed`. Delegation children and tasks with queued retries,
fixes, continuations, or pending advancement remain under their existing owner.

**PR-bound fix pass:** there is no dispatch for one. A `changes_requested`
verdict reports to the requester and to the `--lead` seat and stops there; the
lead reopens its own checkout on the PR branch, pushes the fix, and records it
with `gitmoot job record --type implement --pr <number> --head-sha <sha>`.
Those rows are what the merge gate's implementer attribution and
reviewer-independence check read, so recording is not bookkeeping — it is how
the gate learns who implemented and that the reviewer was someone else. Request
the re-review at the new head with `gitmoot review request --pr <number>`.

While the forge reports a PR as draft, Gitmoot leaves the task in its current
lifecycle state instead of parking it at `awaiting_human_merge`: a draft is an
author hold, not a pending human merge decision. The `--draft`/`--ready`
dispatch flags went with `agent implement`, so mark the PR ready on the forge
when it should enter review and merge-gate processing.

The daemon default is `--workers 1`. Users can raise it when jobs target
different runtime sessions, managed agent types with `max_background` greater
than one, or forkable temporary workers. By default `[parallel_sessions]` uses
`same_session = "fork_temp_session"`, `merge_back = "summary"`,
`max_temp_sessions_per_agent = 4`, and
`eligible_actions = ["ask", "review", "implement"]`. Same-checkout work remains
serialized; same-runtime Codex/Claude jobs can fork only when the action is
eligible and implementation jobs have a safe task worktree.

### Running one agent's jobs in parallel

One registered agent serves one **foreground** ask at a time: `gitmoot agent ask
<name>` pins a single resumable runtime session, serialized by the
`runtime:<runtime>:<runtime_ref>` lock. Foreground asks do not auto-fork — to run
the *same* agent on several questions at once, dispatch them as **background**
jobs, where two mechanisms spin extra sessions for you:

1. **Temp-session forking (default, zero-config).** `[parallel_sessions]` ships
   with `same_session = "fork_temp_session"` and `max_temp_sessions_per_agent =
   4`: when a registered agent's session is busy and another **eligible**
   background job (`ask`/`review`/`implement`) is queued for it, the daemon forks
   a throwaway temp worker from that agent so the jobs run in parallel. Same
   runtime only (Codex/Claude/Kimi); same-checkout work stays serialized; an
   `implement` fork needs a safe task worktree. Nothing to configure.
2. **Managed agent types (`max_background`).** `gitmoot agent type set <type>
   --max-background N` defines a *pool* of named, reusable managed instances.
   Dispatch to the type with `gitmoot agent run <type> --type <type>
   --background …` and the daemon reuses an idle instance or spins a new one, up
   to `N`.

Both only deliver real parallelism when the daemon has job slots: raise
`--workers` above the default `1` (e.g. `--workers 6`) so `max_background`
instances / temp sessions actually run concurrently.

**Precedence — a single instance shadows a same-named type.** Dispatch resolves a
registered agent by name **first**, so if you `gitmoot agent start researcher`
*and* `gitmoot agent type set researcher`, plain `researcher` always uses the
single instance. Force the managed type with `--type researcher` (or don't
register a single instance of that name). Since **v0.5.1** a **foreground**
`gitmoot agent ask <type>` (the `ask` action) dispatches to the managed type
synchronously — it spins or reuses a managed instance up to `max_background`.
`review`/`implement` to a type still use `--background`.

## Multi-Repo Work

Agents are global identities with explicit per-repo access. When working across
multiple repos, always pass `--repo owner/repo` to status, daemon, job, and event
commands so jobs are routed in the correct repository context.

```sh
gitmoot agent allow reviewer --repo owner/project-a
gitmoot agent allow reviewer --repo owner/project-b
gitmoot status --repo owner/project-a
gitmoot status --repo owner/project-b
```

## Multi-Model Delegation (Orchestra)

Orchestra is gitmoot's name for structured multi-agent delegation: a conductor
(coordinator) returns a `delegations[]` score, the players (child agents) run in
parallel or in dependency order, and a finale (continuation) reconvenes and
synthesizes the results. This is how you orchestrate an orchestra of agents
across different runtimes.

A coordinator agent can fan work out to other agents running on different
runtimes by returning a `delegations` array in its `gitmoot_result`. Gitmoot
enqueues one child job per delegation, records a `delegation_enqueued` event on
the coordinator job, and runs the children in the daemon. Start a coordinator
and two workers on different runtimes so each delegation lands on the best model
for the job:

```sh
gitmoot agent start coordinator --runtime codex --repo owner/repo --role planner --capability ask --start-daemon
gitmoot agent start ui-worker --runtime claude --repo owner/repo --role reviewer --capability ask --capability review
gitmoot agent start api-worker --runtime kimi --repo owner/repo --role reviewer --capability ask --capability review
```

Queue the coordinator as background work so the daemon runs it and dispatches
its delegations (a synchronous `gitmoot agent ask` without `--background` only
returns the coordinator's own answer and does not fan out):

```sh
gitmoot agent ask coordinator --repo owner/repo --background "Coordinate the redesign across the API and UI teams."
```

The coordinator returns two delegations to the workers on different runtimes:

```json
{
  "gitmoot_result": {
    "decision": "approved",
    "summary": "Delegating UI review and API review to the workers.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": [],
    "delegations": [
      {
        "id": "ui",
        "agent": "ui-worker",
        "action": "ask",
        "prompt": "Propose the component changes for the new dashboard layout."
      },
      {
        "id": "api",
        "agent": "api-worker",
        "action": "review",
        "prompt": "Review the API contract for the dashboard endpoints."
      }
    ]
  }
}
```

Gitmoot enqueues each delegation as a flat parallel child job
(`<parent-job-id>/delegation/<id>`) with a `delegation_enqueued` event on the
parent. Delegations that declare `deps` run only after the named siblings
succeed, and once every top-level delegation is terminal Gitmoot enqueues one
coordinator continuation job so the coordinator can synthesize the results.
Inspect the fan-out with `gitmoot job list --repo owner/repo` (one row per child
job) and `gitmoot events --repo owner/repo` (the `delegation_enqueued` events).
Each child job carries job-tree linkage fields — `parent_job_id`,
`delegation_id`, `root_job_id`, `delegation_depth`, and `task_id` — so a child
can be traced back to its parent, its originating delegation, and the root of the
tree. See `RESULT_CONTRACT.md` for the full delegation field reference.

**Read-only jobs use action-specific refs.** Every top-level local `review` gets
an owned detached worktree at its requested PR head, even when it carries a
stable Task ID or runs foreground. That exact-head allocation is correctness and
fails the dispatch closed. A background taskless `ask` keeps the existing
committed-tip, fail-open isolation; task-bearing and foreground asks keep their
existing checkout behavior. Delegation/pool read-only isolation still applies
when same-repo readers contend.

These detached worktrees do **not** contain gitignored
paths (e.g. vendored clones under `repos/**`) or any uncommitted working-tree
changes, so an analysis/research leg cannot see the operator's live working tree
there. Committed-tip ask/delegation isolation carries a note with the canonical
base-checkout absolute path so a worker whose sandbox can read it (e.g. codex)
reaches the real tree instead of reporting a working-tree feature as absent.
Review deliberately keeps its exact-head binding and does not add that
committed-tip context note. For whole-working-tree ask analysis, use a foreground
or task-bearing ask, or pass an **absolute** path to the file/dir under analysis.

## Coordinator-Owned Review

When a PR appears on a branch Gitmoot holds a lock for, the daemon's PR-watcher
fans it out to Gitmoot's native reviewers — the configured required reviewers,
or the ones passed for the task — so each reviewer runs as its own review job
before the merge gate. When a coordinator already plans review itself (for
example a `review-panel` leg, or a custom continuation that reconvenes its own
reviewers), that native fan-out duplicates work. Pass
`--skip-native-review-fanout` on `gitmoot orchestrate` or `gitmoot agent run`
to hand review orchestration to the coordinator:

```sh
gitmoot orchestrate verifier "Produce the export-feature migration plan and prove it is complete." --repo owner/repo --skip-native-review-fanout
gitmoot agent run lead --repo owner/repo --pr 12 --skip-native-review-fanout "Review this PR."
```

The flag is persisted on the job payload and on the branch lock, and the
PR-watcher reads the lock, so a PR it observes on that branch stays free of
native review fan-out. The engine's implement-advance arm reads the same flag,
but no implement job can be dispatched since #2203, so the PR-watcher is the
path that still exercises it. The flag defaults off; leaving it off keeps the
full native review fan-out.

## Coordinator Recipes

Coordinator recipes are built-in agent templates that turn the Orchestra pattern
into one-command workflows. Each recipe is a coordinator prompt that emits a
`delegations[]` of **ephemeral** workers (no pre-registration), runs them in the
daemon, then reconvenes their results in a single continuation. Start one with
`gitmoot orchestrate <agent> "..." --repo owner/repo --recipe <recipe-id>`
(#477): the `--recipe` flag routes any existing coordinator agent through the
named recipe prompt without changing the agent's identity. The bare
`gitmoot orchestrate <recipe-id> "..."` form requires an agent **registered
under the recipe name** first; on a fresh install it fails with "agent not
found", so prefer `--recipe`.

Three recipes ship built in:

- **`review-panel`** — fans a PR or change out to a panel of ephemeral reviewers,
  each with a different lens (correctness and security; performance and
  maintainability; tests and edge cases), then synthesizes their findings into
  one verdict. The panelists are dep-free, so they review in parallel, each with
  a self-contained lens prompt across mixed runtimes so the panel does not share
  one model's blind spots (point a panelist at an installed review template such
  as `thermo-nuclear-code-quality-review` only if you want).
- **`decompose-and-verify`** was RETIRED because Gitmoot no longer dispatches
  implementation (#2203). It existed to fan one implementation task out to
  parallel implementation legs, and `ask`/`review` are now the only accepted
  delegation actions, so those legs have no action to run under. The teaching it
  carried is still the rule for any fan-out you write yourself: decompose into
  **file-disjoint** subtasks so parallel legs cannot conflict, and end with a
  separate verify worker rather than trusting a producer's self-report.
  Installing it refuses with `agent template is retired; use verifier`, so a
  stale script gets a named successor rather than "unknown template".
- **`verifier`** — the minimal **produce vs. independent check** recipe: one
  producer leg plus one independent verify leg. The verify leg is a read-only
  ephemeral `review` worker that `deps` on the producer, runs on a **different
  runtime/model**, and checks the producer's result against the original
  goal — reading the real code and running its own checks rather than trusting the producer's
  self-report. It returns `changes_requested` with structured findings on any
  objective failure (else `approved`), with `failure_policy: escalate` routing a
  failed verdict back to the coordinator continuation for autonomous correction
  (or `escalate_human` for a human pause).

**Produce vs. independent check.** A `synthesis_rule` (`summary`/`vote`/`quorum`)
reconciles what the producers **self-report** — self-evaluation, which inherits
the producer's blind spots. A `verifier` verify leg is a
*separate* worker on a different runtime/model that checks the producer's result
against the goal — cross-evaluation, which the literature finds beats
self-evaluation (the generator-verifier gap; LLM-as-judge self-preference bias).
This generalizes ROMA's Verifier (`(goal, candidate_output) -> verdict +
feedback`, vendored at `repos/ROMA`); it uses only shipped primitives
(`ephemeral`, `failure_policy`, the merge gate) — no new engine code. See the
**produce vs. independent check** note in `RESULT_CONTRACT.md`.

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Produce the export-feature migration plan and prove it is complete." --repo owner/repo --recipe verifier
```

The panelists in `review-panel` and both legs of `verifier` are **ephemeral**
workers: Gitmoot creates
each from the delegation's `ephemeral` spec, runs it, and disposes of it once the
child job finishes. Ephemeral workers are leaf-only — they return findings, never their own
delegations — so a recipe's fan-out is exactly one level deep. In both recipes the
delegations never set `agent`, because `agent` and `ephemeral` are mutually
exclusive. Once every leg is terminal, Gitmoot enqueues one continuation back to
the coordinator to merge the results (the panel verdict, or the verify gate plus
the producer's artifact). Inspect the run under `gitmoot job list --repo owner/repo`
and the `delegation_enqueued` events in `gitmoot events --repo owner/repo`. See
`RESULT_CONTRACT.md` for the `ephemeral` field reference and the termination
bounds these recipes run inside.

## Satisfying the workload-mode gate

A pull request whose diff changes the `**Current mode:` marker in `AGENTS.md` is
HELD until an exact-head reconciliation row exists. The gate is keyed on the
repository OWNER, so every `gitmoot/*` repository is held, human-requested merges
included.

Two note streams decide it, both matched by an exact body PREFIX and compared
case-insensitively on repository:

- DECISION: `[operating-mode repo=<owner/repo> mode=<THROUGHPUT|STEADY|DRAIN>]`
- RECONCILIATION: `[workload-mode-reconciliation repo=<owner/repo> pr=<number> head=<40-character reviewed head> mode=<mode> decision_note=<note id|none>]`

Every field is `key=value`, whitespace separated, inside the leading bracket. A
field with an empty key or value makes the whole body unparseable, so the
`repo=` field inside the body is what identifies the repository:

```
gitmoot workflow note <label> "[operating-mode repo=owner/repo mode=STEADY]"
```

`<label>` must be a workflow that already has jobs - `workflow note` refuses an
unknown label to guard against a typo - so file the row under the lane the PR is
already being coordinated in rather than inventing a label.

The note's repo COLUMN is a separate, optional thing. `gitmoot workflow note
--repo <owner/repo>` sets it and nothing else - it no longer opts the note into
anything, because the memory surface it used to accompany is gone (#2202). A
note whose column is empty still counts when its body names this repository,
which is the ordinary case, so setting the column is optional; getting `repo=`
right in the BODY is not. Prefer setting it anyway for an operating-mode or
reconciliation note: the gate reads those through two bounded windows, one
repo-scoped and one for the repo-less rows, and a scoped note cannot be crowded
out of the window by other repositories' notes.

PRECEDENCE. The NEWEST decision wins, and a reconciliation row must be newer than
it. `decision_note=none` means the PR itself is the decision, and the row's own
`created_at` is `decided_at`; otherwise `decision_note` names the newest
operating-mode note the exact head implements. A row that cites a note the PR
contradicts cannot ratify it.

WHEN A DECISION CANNOT BE READ. An unreadable newest decision - a malformed field
list, a `mode` that is not a workload mode, or a note recorded for this
repository whose body omits or contradicts `repo=` - SUPERSEDES like any other
decision rather than reading as "no decision". It is a recency boundary, not a
veto: a fresh exact-head row that agrees with the PR's own marker still merges,
so correcting the note is optional. The gate refuses outright only when nothing
readable remains - the note is unreadable AND the PR's marker patch is missing or
ambiguous - and the hold then names both exits: append a readable operating-mode
note, and file an exact-head row citing it.

CLEARING A HOLD. The hold is a pending merge-gate decision, not a failure: the
task stays `ready_to_merge`, the gate records the cause as its pending reason
(also published on the PR's pending merge-gate commit status), and the daemon
re-evaluates it on the next poll tick, so the PR merges once a valid row lands.
