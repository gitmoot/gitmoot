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
hubs, `/workflows/<label>`) and `--remember` distills its insight into shared
memory. Use `orchestrate` when the plan fits a declarable tree run unattended
(parallel fan-out, bounded autonomous work, pipeline orchestrate stages); use
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
gitmoot workflow note release-42 "Canary passed." --author operator --remember
gitmoot workflow show release-42
gitmoot workflow close release-42 --reason "Release 42 shipped and verified."
```

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
with live file status, registry modes, configured proxy placement, and sorted
pipeline grants. Credential values and value-derived data are never projected.

The append-only journal stores text and authors verbatim. JSON show output keeps
them verbatim; terminal text output sanitizes escapes/control bytes and caps each
field to one line. `--remember` uses the
normal low-trust prefilter/dedup path, defaults to the shared pool, and accepts
`--agent NAME` for a registered agent's private pool. A single repo is inferred;
otherwise `--repo` is required. Rejection writes no note, and note plus
observation are atomic. V1 has no group lifecycle controls and allows reuse.

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
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review --runtime codex --repo owner/repo --template thermo-nuclear-code-quality-review --start-daemon
```

For a local dispatch, pair that reviewer with the registered implementer that
should receive a `changes_requested` fix pass:

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

Custom template content is snapshotted into local Gitmoot state. After editing the
source template file, run `gitmoot agent template diff <id>` and `gitmoot agent template update
<id>` before expecting new jobs to use the changed prompt.

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

If the user asks for a standard goal file, read the canonical goal template and
write the goal file. Do not create a goal file unless explicitly requested.

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
GITMOOT_ORG_ROLE=<approver-role> gitmoot org directive send \
  --to implementer-role --workflow feature-42 \
  "approved: implement plan 1234 as written; the plan is the scope fence"

# 4. Implement, quoting the approved plan-id.
gitmoot agent implement builder --repo owner/repo --workflow feature-42 \
  "Implement plan 1234 (workflow note in feature-42). Stay inside it; work
   outside the plan needs an amended plan and a fresh approval."

# 5. Completion ends the obligation; merge closes the workflow — in this
#    order: a `done` posted after `close` reopens the workflow, because
#    any note into a closed workflow does.
gitmoot org directive done <directive-id> --by implementer-role
gitmoot workflow close feature-42 --reason "Plan 1234 implemented and merged."
```

Outside org mode, step 3 is an explicit human approval message referencing the
plan-id; the rest is unchanged. A coordinator may waive the gate for trivial
mechanical fixes by writing "plan-waived" in the dispatch prompt — the waiver
is deliberate and visible, never implied. Under this convention every
implement dispatch prompt carries either a plan-id or the literal
"plan-waived"; a dispatch with neither is out of contract. Prefer this
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

## Current-Chat Template Capture

Use template capture when the user wants to turn a successful visible Codex or
Claude Code conversation into a reusable agent template.

```text
Use Gitmoot to capture this session as agent template release-planner. Draft only.
```

The current chat reads `references/TEMPLATE_CAPTURE.md`, extracts durable
workflow rules from visible conversation context and inspected files, and writes
or returns a draft. It must not route the request through `gitmoot agent ask`,
start a daemon, queue a job, or install/replace a template without explicit user
approval.

For a blank starting point, scaffold the required sections:

```sh
gitmoot agent template draft release-planner
```

After the user reviews the draft:

```sh
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add release-planner --file .gitmoot/templates/release-planner.md
gitmoot agent prompt release-planner
```

The capture pieces are distinct:

- `agent template draft`: scaffold a blank structure.
- "capture here": current chat fills that structure from visible context.
- `agent template validate`: structural check.
- `agent template add`: install a snapshot.
- `agent prompt`: reuse the installed prompt in the current chat.
- `agent start --template`: create a runnable background agent instance.

## Background Planner Agent

Use the planner template when the user wants a structured implementation plan or a
standard Gitmoot goal file to run as a tracked Gitmoot background agent job.

```sh
gitmoot agent template update planner
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

Ask from a PR comment:

```text
/gitmoot project-planner ask Write a task-by-task implementation plan for this feature, then create the goal file prompt.
```

Ask directly from a local Codex or Claude Code chat by having the runtime call
the Gitmoot CLI when the user explicitly wants a registered background-capable
agent path:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write a task-by-task implementation plan for this feature, then create the goal file prompt."
gitmoot job watch <job-id>
```

If the Codex plugin exposes a Gitmoot command bridge in chat, the equivalent
form is `$gitmoot:gitmoot agent ask project-planner --repo owner/repo --background "..."`. The
important part is that background planner work goes through `gitmoot agent ask`;
fast "here" planning stays in the current chat and uses `gitmoot agent prompt`
only to read prompt content.

If the planner writes a goal file and the user wants Gitmoot to track it, import
it explicitly:

```sh
gitmoot goal import --file GOAL-feature.md --repo owner/repo
```

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

**Dead implement recovery (manual):** if an implementer's process dies after
editing the task worktree but before it commits/pushes/opens a PR, the edits sit
uncommitted. `gitmoot task run` and `gitmoot agent implement` refuse to restart
over a dirty worktree with no active job (so nothing is discarded) and point at
`gitmoot task recover <task-id> --owner <agent>`, which commits the full
worktree state (`git add -A`, incl. untracked non-ignored files), pushes the
branch, and opens or adopts the PR. `--repo` is optional (falls back to the
task's repo). `--owner` is required for this artifact-finalization path, but not
when a dismissed branchless task is simply restored to `planned`. Recovery
refuses while a live process is still inside the worktree.

**Task dismissal and stale reconciliation:** `dismissed` is a terminal task
state for implicit workflow transitions. An operator can run `gitmoot task
dismiss <id> [--reason ...]` only from `implementing` or `blocked`; Gitmoot
refuses while any matching job is live or a process remains in the task
worktree. The branch and worktree are preserved, while the branch lock is
released best-effort. Manual and daemon transitions are audited as
`task_dismissed_manual` and `task_dismissed_auto` in `gitmoot task events <id>`.

Each repo poll reads a bounded oldest-first stale window and processes up to 20
qualifying `implementing` tasks whose `updated_at` predates
`[workflow].stale_task_ttl` (default `168h`; `"0"` disables the leg).
`updated_at` is deliberately a conservative activity proxy, not proof of
abandonment. A candidate is skipped for a live job, a same-repo open-PR branch,
or an exact branch still present on `origin`; remote lookup uncertainty skips
mutation. A branchless candidate needs no remote lookup. Explicit `task
recover` restores preserved artifacts through `implementing` to `pr_open`, or
restores a branchless task to `planned`; job retry records its own recovery
event. The server-side task board omits dismissed rows immediately.

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
destroy human planning context that goal-file re-import cannot reconstruct.
When enabled, it reuses the same live-job, same-repo open-PR, remote-branch,
and remote-uncertainty skips and records `task_dismissed_planned_ttl`. Task
worktree allocation claims `planned -> implementing` with a write-time CAS, so
a concurrent TTL dismissal cannot be overwritten; explicit recovery is needed.

A clean closed-unmerged PR moves `pr_open`, `reviewing`, or
`changes_requested` to `blocked` with `pr_closed_unmerged`; ambiguous PR state
does not advance or unblock anything. After advancement and delegation handling,
a terminal top-level implement job with no PR and no live successor blocks an
otherwise-stuck `implementing` task. Implemented success without a PR records
`task_blocked_terminal_no_pr`; other terminal outcomes record
`task_blocked_job_failed`. Delegation children and tasks with queued retries,
fixes, continuations, or pending advancement remain under their existing owner.

**PR-bound fix pass:** use `gitmoot agent implement <agent> --repo owner/repo
--pr <number> "..."` or `gitmoot agent run <agent> --repo owner/repo --action
implement --pr <number> "..."` to send an existing open PR back through its
implementation task. `--action` chooses ask/review/implement; `--type` instead
chooses a managed agent type, so the two flags are independent. Before reuse,
Gitmoot proves the PR is open, same-repository, and bound to the existing task's
head branch. That validated door permits `pr_open` to re-enter implementation
without widening the predicate shared by `task recover`; review/merge states,
branch mismatches, dirty/live worktrees, active implement jobs, and foreign
branch locks still fail closed. The job keeps the PR number so finalization
adopts the existing PR.

Fresh implementation PRs opened by the engine are drafts by default. Dispatch
with `--ready` only when the PR should enter review and merge-gate processing
immediately; `--draft` records the default intent explicitly. While the forge
reports the PR as draft, Gitmoot leaves the task in its current lifecycle state
instead of parking it at `awaiting_human_merge`: a draft is an author hold, not
a pending human merge decision.

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

By default an `implement` job that opens a pull request fans the PR out to
Gitmoot's native reviewers — the configured required reviewers, or the ones
passed for the task — so each reviewer runs as its own review job before the
merge gate. When a coordinator already plans review itself (for example a
`review-panel` leg, or a custom continuation that reconvenes its own reviewers),
that native fan-out duplicates work. Pass `--skip-native-review-fanout` on
`gitmoot orchestrate`, `gitmoot agent run`, or `gitmoot agent implement` to hand
review orchestration to the coordinator:

```sh
gitmoot agent implement lead --repo owner/repo --task task-001 --skip-native-review-fanout "Implement this task."
gitmoot orchestrate decompose-and-verify "Implement the export feature described in the task." --repo owner/repo --skip-native-review-fanout
```

With the flag set, the implement→PR step still records the PR baseline, runs the
merge gate, and records the `implemented` decision — it simply enqueues **no**
native review jobs. The skip is honored on both PR-open paths: the engine's
implement-advance and the daemon's GitHub PR-watcher, so a PR opened either way
stays free of native review fan-out. The flag defaults off; leaving it off keeps
the full native review fan-out, byte-identical to prior behavior.

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
- **`decompose-and-verify`** — decomposes one implementation task into
  file-disjoint subtasks, fans them out to ephemeral implementation workers that
  build in parallel in their own branch worktrees, then runs a single `review`
  verify step that `deps` on every implementation leg before reporting back.
- **`verifier`** — the minimal **produce vs. independent check** recipe: one
  producer leg plus one independent verify leg. The verify leg is a read-only
  ephemeral `review` worker that `deps` on the producer, runs on a **different
  runtime/model**, and checks the producer's combined result against the original
  goal — re-running the build and tests itself rather than trusting the producer's
  self-report. It returns `changes_requested` with structured findings on any
  objective failure (else `approved`), with `failure_policy: escalate` routing a
  failed verdict back to the coordinator continuation for autonomous correction
  (or `escalate_human` for a human pause).

**Produce vs. independent check.** A `synthesis_rule` (`summary`/`vote`/`quorum`)
reconciles what the producers **self-report** — self-evaluation, which inherits
the producer's blind spots. A `verifier`/`decompose-and-verify` verify leg is a
*separate* worker on a different runtime/model that checks the combined result
against the goal — cross-evaluation, which the literature finds beats
self-evaluation (the generator-verifier gap; LLM-as-judge self-preference bias).
This generalizes ROMA's Verifier (`(goal, candidate_output) -> verdict +
feedback`, vendored at `repos/ROMA`); it uses only shipped primitives
(`ephemeral`, `failure_policy`, the merge gate) — no new engine code. See the
**produce vs. independent check** note in `RESULT_CONTRACT.md`.

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Implement the export feature described in the task." --repo owner/repo --recipe decompose-and-verify
gitmoot orchestrate project-planner "Implement the rate limiter described in the task and prove it works." --repo owner/repo --recipe verifier
```

The panelists in `review-panel` and every producer and verify leg in
`decompose-and-verify` and `verifier` are **ephemeral** workers: Gitmoot creates
each from the delegation's `ephemeral` spec, runs it, and disposes of it once the
child job finishes. Ephemeral workers are leaf-only — they return findings, never their own
delegations — so a recipe's fan-out is exactly one level deep. In all three recipes the
delegations never set `agent`, because `agent` and `ephemeral` are mutually
exclusive. Once every leg is terminal, Gitmoot enqueues one continuation back to
the coordinator to merge the results (the panel verdict, or the verify gate plus
the merged changes). Inspect the run under `gitmoot job list --repo owner/repo`
and the `delegation_enqueued` events in `gitmoot events --repo owner/repo`. See
`RESULT_CONTRACT.md` for the `ephemeral` field reference and the termination
bounds these recipes run inside.

## Pipelines

When the work is a **fixed, repeatable sequence of shell steps** with explicit
dependencies — not a decomposition an LLM should reason about — declare a pipeline
(#681) instead of orchestrating. A pipeline is a declared DAG of shell or
managed-agent stages; each stage is an ordinary queued job (shell commands use
the shell runtime; agent stages use their registered runtime), and a scan-based
advancer folds each stage's `gitmoot_result` decision and enqueues the stages whose
`needs` have all succeeded. Pipelines are off by default and reuse the same job
queue and (heartbeat-style) scheduling as everything else.

Write the DAG as YAML, register it, and run it:

```yaml
# nightly-sync.yaml
name: nightly-sync
repo: owner/repo            # required to run (stages need a managed repo)
group: Release Automation   # optional display section (falls back to repo when unset;
                            #   built-in memory pipelines ship under "Gitmoot System")
description: Syncs nightly data for deployment. # optional detail-page purpose (multiline, max 500 chars)
env_file: /root/.config/nightly-sync/env # optional 0600 secret file
env:                         # optional inline NON-secret defaults
  OUTPUT_DIR: /srv/nightly-sync
schedule:                   # optional; auto-runs every interval once enabled
  interval: 24h
  jitter: 15m
stages:
  - id: source
    cmd: "curl -sf https://example.com/data > data.json"
    env_keys: [SOURCE_API_TOKEN]
  - id: score
    cmd: "python score.py data.json"
    isolate: true          # optional shell-only detached read-only worktree
    needs: [source]         # runs only after source SUCCEEDS
  - id: deploy
    cmd: "rclone copy out/ r2:bucket"
    needs: [score]
    retry: 2
```

```sh
gitmoot pipeline add nightly-sync.yaml --enable   # validate + store; --enable turns on the schedule
RUN=$(gitmoot pipeline run nightly-sync)          # or trigger a manual run now
gitmoot pipeline watch "$RUN"                      # one blocking call; no agent poll loop
```

### Pipelines as a service

An owner can opt a shell-only, template-free pipeline into the service surface
with a small versioned flat schema:

```sh
gitmoot pipeline expose --schema schema.json nightly-sync
gitmoot pipeline serve # loopback-only by default
```

The bearer token is shown once and stored only as a SHA-256 digest. Requests are
validated before admission; typed values reach stages only through reserved
`GITMOOT_INPUT_*` environment variables, never prompts. Atomic admission applies
the persisted rate bucket, a global active-run cap, and a same-pipeline overlap
guard. Accepted shell jobs run in detached worktrees. A successful authenticated
status read finalizes a frozen bundle containing `spec.yaml`, `bundle.yaml`,
`proof.json`, and `verification.json`; `/receipts/<run-id>` is the sanitized
public receipt. `gitmoot proof --verify <run-id>` repeats the offline store-only
run/stage/job/result-hash consistency check and does not rerun work or contact CI.

`env_keys` is a deny-by-default allowlist of exact names or globs. Shell stages
resolve the pipeline's `env_file`, pipeline-granted shared keys, and inline
non-secret `env`. Agent stages resolve only configured proxied keys granted to
their registered seat; the grant and explicit stage selector are both required,
and the real value never enters the agent process. Gates reject the field. No
list means no key access. `pipeline add` requires the file
to be absolute, operator-owned `0600`, and outside Gitmoot state/checkouts; it
also refuses missing keys and reserved `GITMOOT_*` names. Values are read fresh
at stage delivery for restart-free rotation. The job audit stores the path and
expanded names only, not file values.

Ordinary agent jobs receive no pipeline-stage keys, and a coordinator's
delegation children inherit nothing. A proxied agent can exercise the credential
against its pinned upstream even though it never receives the underlying bytes.

The pipeline detail **Keys** tab exposes this authorization as names only: every
stage appears in spec order with each resolved key's `own`, `shared`, or
`default` source and delivery mode. It live-checks the declared `env_file` and
reports `none`, `ok`, `missing`, `bad_mode`, `bad_owner`, `bad_location`, or
`invalid`; selectors that cannot resolve after file drift are listed separately.
The tab never reads values into its response, and delivery-time validation
remains authoritative.

### Share a pipeline with another Gitmoot home

Use a private GitHub repository as a reviewable pipeline catalog. The source and
target homes can keep the same default remote while using different local target
repositories and runtime sessions:

```sh
# Source home: creates the catalog privately, then writes pipelines/nightly-sync/.
gitmoot pipeline remote set acme/pipeline-catalog
gitmoot pipeline publish nightly-sync --create

# Target home: inspect the catalog and import through the same bundle gates.
gitmoot pipeline remote set acme/pipeline-catalog
gitmoot pipeline pull --list
gitmoot pipeline pull nightly-sync \
  --repo acme/nightly-target \
  --agent-map scorer=local-scorer
```

The remote layout keeps `bundle.yaml`, `spec.yaml`, and every
`templates/<id>.md` snapshot separate, so GitHub diffs stay reviewable. An
unchanged republish performs no writes; a changed republish touches only changed
files and removes snapshots that vanished from the exported bundle. `--create`
is private by default because prompt bodies and metadata are published verbatim.
Use `--remote owner/repo` to override `[pipeline_remote]` for one command; the
configured `ref` defaults to `main` and `path` to `pipelines`.

`pipeline pull --list` shows the manifest name, description, and requirements
summary before installation. Pull fetches the directory at HEAD and invokes the
existing import flow unchanged. `spec.yaml` is the stored pipeline text with only
`repo` replaced by the declared
`__GITMOOT_REPO__` parameter; comments, ordering, block scalars, and other bytes
survive. Referenced custom templates are canonical snapshots produced by the same
export path as `agent template export`. Template prompts are verbatim, so inspect
them before publishing. Local environment values and runtime state are never
copied.

Import always prints its requirements report. Use `--agent-map exported=local`
for a machine-local agent/session, or omit it to install the embedded template and
register the declared runtime. A missing runtime for an unmapped agent fails
before anything is imported. Name/content collisions fail unless `--force`, and
`--name` gives the pipeline a new local name.

The imported pipeline is disabled by default. This is also the re-consent
boundary for any bundled write authority (`allow_scheduled_writes`,
`allow_triggered_writes`, or `allow_auto_merge`): review the report and absolute
path warnings, then enable explicitly:

```sh
gitmoot pipeline enable nightly-sync
# Or add --enable to the import after review.
```

Missing upstream-pipeline requirements are warnings, not import failures; the
pipeline stays dormant until its upstream exists. The target repo/name/agent
mapping changes the stored bytes, so the imported `spec_hash` is intentionally
computed from those final bytes rather than copied from the source.

For an offline or non-GitHub transfer, the underlying directory commands remain
available:

```sh
gitmoot pipeline export nightly-sync --output ./nightly-sync.bundle
gitmoot pipeline import ./nightly-sync.bundle --repo acme/nightly-target
```

### Chain a pipeline after another succeeds

Use `kind: pipeline` when ordering matters more than a clock stagger. This
example runs ingest exactly once after each newly-succeeded groom run:

```yaml
name: memory-ingest-sweep
repo: owner/repo
trigger:
  kind: pipeline
  pipeline: memory-groom-propose
stages:
  - id: sweep
    cmd: gitmoot memory ingest sweep --json
```

This replaces the old `24h` / `24h30m` imitation of ordering: failed or cancelled
groom runs do not start ingest, while each successful run fires it once. The
cursor is durable across daemon restarts. Adding or enabling ingest arms at the
latest groom run, so no historical or disabled-period runs backfill. If ingest
is already active, its cursor does not move and it fires after settlement.
Pipeline-trigger cycles are rejected at add time. A missing or later-removed
upstream is allowed but leaves the downstream dormant and visibly marked
`(upstream missing)`. Pipeline triggers use local database state.

A stage prints a `gitmoot_result` blob to stdout to signal its decision; the
advancer folds by that **decision**, never the job's exit state:

```sh
printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"synced"}}'
printf '%s' '{"gitmoot_result":{"decision":"skipped","summary":"no new replies today"}}'
printf '%s' '{"gitmoot_result":{"decision":"blocked","summary":"secret missing","needs":["R2 token"]}}'
```

`skipped` is the default-on success decision for a stage whose task had no work.
The persisted summary is prefixed with `[skipped: no work]`, so downstream agent
stages receive the honest outcome. An explicit `success_decisions` list that
omits `skipped` is strict and folds it failed. Only `implemented` promises a PR from
an implement stage; other configured success decisions settle immediately. If an
implement source succeeds without a PR, a downstream `pr_merged` gate parks blocked
instead of waiting forever. The result still uses the existing succeeded stage state; the `SKIPPED`
funnel state remains reserved for downstream stages that never ran.

### Agent stages (#757 / #768 / #758)

A stage may run a **named managed agent** instead of a shell command; a stage is
exactly one of `cmd`, `agent`, or `gate`. An agent stage runs on its **own** registered
runtime (claude / codex). Four kinds:

- **ask / review** (#757) — read-only **leaf** (`action: ask|review`); `delegations[]`
  and `human_questions[]` stripped. A review may add `source: <implement stage>`
  (#813) to bind to that stage's PR and exact head SHA.
- **implement** (#768): `action: implement` + `write: true`. MUTATES the repo on a
  deterministic `gitmoot/pipe-<run>-<stage>` branch (retry reuses it, never duplicates).
  The `implemented` decision folds **on PR-opened**; other configured success decisions
  settle immediately without promising a PR. The implement job never merges. Scheduled
  pipelines also need pipeline-level `allow_scheduled_writes: true`.
- **produce** (#814) — `action: produce` + `write: true` + absolute cleaned
  `writes:` and optional absolute cleaned read-only `reads:` inputs. Codex uses
  its native sandbox; Gitmoot never turns a read path into Codex's writable
  `--add-dir`. Claude/modern Kimi are supported when
  `gitmoot sandbox probe` confirms strict Landlock enforcement. Unsupported hosts
  retain the Codex-only refusal; there is no advisory fallback. Never
  branch/task/PR state. Optional
  `network: true`, `check`, and bounded same-session `check_retries`. Declared paths
  are additive grants (workdir, `/tmp`, and `$TMPDIR` remain writable). Runtime-owned
  state is writable by design: `$HOME/.claude` plus
  `$XDG_CACHE_HOME/claude-cli-nodejs` for Claude and `$HOME/.kimi-code` for Kimi;
  apart from that state/cache and device nodes, only `writes:`, workdir, and temp
  roots are writable. When `reads:` is declared, Landlock gives those paths
  read-only access and denies unrelated host data. For Claude, delivery also
  discovers absolute command-hook scripts from the operator's user settings,
  grants their parent directories read-only, and grants `~/.claude.json` as one
  read-only file. Gitmoot home, keychain, pipeline `env_file`, and read roots
  containing a write root are rejected after symlink resolution at add and delivery
  time; those exclusions also override hook discovery. Missing/protected hooks fail
  before launch, while relative or malformed hook commands emit a
  `produce_runtime_resource_warning` event. No discovery runs without `reads:`.
  Landlock governs filesystem access rather than network access,
  retries must be
  idempotent, and Gitmoot never cleans operator-owned data directories.
- **orchestrate** (#758) — `orchestrate: true`. Sub-tree **coordinator** (the one
  non-leaf): fans out owned children (full delegation bounds ladder), waits via the
  continuation chain, folds the tail. `retry: 0`.
- **gate** (#768) — `gate: pr_merged` + `source: <upstream implement stage>`, no
  `agent`. Jobless waiter: folds succeeded when the source PR merges; parks `blocked`
  on close-unmerged or timeout. Human merge is the default. Add gate-level
  `merge: auto` plus top-level `allow_auto_merge: true` for reviewed auto-merge.

```yaml
stages:
  - id: extract
    cmd: "python extract.py > out.json"
  - id: triage
    agent: reply-triager        # create it before the pipeline runs: gitmoot agent create …
    action: ask                 # ask (default) | review | implement (+ write: true)
    prompt: "Triage the extracted replies and flag anything urgent."
    needs: [extract]
  - id: fix
    agent: fixer                # MUTATING implement stage → opens a real PR
    action: implement
    write: true
    prompt: "Apply the approved change."
    needs: [triage]
  - id: wait
    gate: pr_merged             # jobless gate: waits for fix's PR to merge
    source: fix
    needs: [fix]
```

To make review first-class between implementation and the human merge, insert a
source-bound review before the gate:

```yaml
  - id: review
    agent: reviewer
    action: review
    prompt: "Review the implementation PR."
    source: fix
    needs: [fix]
    success_decisions: [approved]
  - id: wait
    gate: pr_merged
    source: fix
    needs: [fix, review]
```

The review job copies the structured PR/head/branch/task/lead stamp from the
succeeded implement job and runs in a detached worktree pinned to that head. It is
report-only: the verdict is posted to the PR and folded by the pipeline, but it does
not dispatch a native fix job or run the native merge gate. The declared binding
also sets `SkipNativeReviewFanout` on `fix`, preventing duplicate reviewer fan-out;
pipelines without the declaration keep native behavior. Any terminal succeeded no-PR
source (a no-op or a non-`implemented` success decision) blocks the review immediately with `source stage produced no
PR; nothing to review` instead of dispatching an unbound job or waiting.

For opt-in auto-merge, add `merge: auto` to the `pr_merged` gate and
`allow_auto_merge: true` at pipeline level. Registration refuses this mode without
at least one review bound to the same implement source. The advancer requires every
such review to fold succeeded with decision `approved`, verifies the live PR head
still equals the reviewed structured `HeadSHA`, then requires GitHub mergeability
and passing checks before one squash attempt. Pending checks wait within the gate
timeout; head drift, unmergeability/conflict, and merge API errors fold blocked, and
merge errors are not retried. The review job remains report-only. Scheduled flows
also require `allow_scheduled_writes: true`; both top-level safety keys are required.
Without `merge: auto`, human merge remains unchanged.
Pending checks wait; skipped/neutral check-runs pass; failures block; and zero
external statuses/checks always block regardless of `require_external_ci`. The
source job event timeline atomically records `pipeline_auto_merge_claim` before
the write and `pipeline_auto_merge_confirmed` after GitHub confirms it, plus
`pipeline_auto_merge_held` when a workload-mode reconciliation hold parks the
gate: that hold releases the claim and retries, and is bounded by the gate
`timeout` or by 24h from the start of the hold episode, keyed on the head and
the decision rather than the cause text. A scan that loses the claim ages its wait from the claim row's own `created_at`
and records `pipeline_auto_merge_claim_orphaned` past 15m - or immediately, with
`cause=claim_timestamp_unreadable`, when that value will not parse - and waits
rather than parking. A stage `timeout` parks the aged case terminally instead of
recovering it, and cannot park the unreadable case at all.

`pipeline add` warns (does not block) when an agent stage names an agent that does
not exist yet; create it before the stage runs. The
agent's stage prompt is **prepended with the results (summaries) of its `needs`
stages** — a clearly-delimited, bounded "Upstream stage results" block — so a
downstream agent stage acts on upstream output as real dataflow. A repo-bound
ask/review agent stage runs in its own detached read-only worktree (#739), so
same-repo agent stages parallelize and never touch the live checkout.

A `cmd` stage can opt into the same concurrency boundary with `isolate: true`.
Its disposable worktree is the committed tip, while the default `false` continues
to run in the shared checkout for commands that need dirty/gitignored data or write
there. Allocation is fail-open (`readonly_worktree_skipped` records fallback), and
successful isolation adds `GITMOOT_CHECKOUT=<live-checkout>` to the shell env. This
removes checkout-lock serialization for siblings, and each isolated stage also
takes a job-scoped shell runtime-session key (`runtime:shell:job:<hash(job)>`)
instead of the command-hash key, so siblings run concurrently even when they share
the **identical** command (#1034). Service shell stages remain unconditionally
isolated and fail closed. The field is rejected on agent and gate stages.

A dependent `cmd` stage receives the same settled upstream results through a data
channel, not shell interpolation. All pipeline shell stages get
`GITMOOT_PIPELINE_NAME`, `GITMOOT_PIPELINE_RUN_ID`, and
`GITMOOT_PIPELINE_STAGE_ID`; stages with `needs` also get
`GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE`. That variable names a fresh readable
`0600` JSON tempfile for the duration of the delivery. Gitmoot persists the JSON
content (not the path), recreates identical bytes on retry/restart, and removes the
file after every exit path. Root stages have no context-file variable.
An isolated non-service shell stage additionally receives `GITMOOT_CHECKOUT`, a
best-effort path to the live checkout for gitignored or uncommitted reads. Prefer
the stage cwd for committed files, treat the live path as read-only, and never run
that reader beside a default stage mutating the checkout because reads can be torn
or contend on `index.lock`. `GITMOOT_INPUT_*`, `GITMOOT_TRIGGER_*`, selected keys,
and the absolute upstream context path remain cwd-independent.

The v1 shape is
`{"schema_version":1,"complete":true,"stages":{"extract":{"id":"extract","state":"succeeded","summary":"...","summary_truncated":false}}}`.
Each summary's marshaled JSON string is capped at 16 KiB with rune-safe
truncation, and the final marshaled document at 64 KiB. `complete:false` means a
summary was truncated or an expected stage was
omitted, so scripts can fail closed:

```sh
jq -e '.schema_version == 1 and .complete == true' \
  "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE" >/dev/null
jq -r '.stages.extract.summary' "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE"
```

Summaries are untrusted data flowing to your trusted script: parse them as data,
never evaluate them as shell, and do not put credentials in summaries.

Produce batches use the existing result decisions: `implemented` = complete,
`changes_requested` = partial (only advances when opted into `success_decisions`),
`blocked` = needs a human, and `skipped` = no work. `pipeline show` reports a
best-effort run token total and per-stage input/output usage in JSON.

Manual runs can carry the same trigger payload as bridge-started runs:

```sh
RUN=$(gitmoot pipeline run nightly-sync --payload batch=nightly --payload region=eu)
gitmoot pipeline watch "$RUN" --timeout 10m
```

Use `--payload-json '{"batch":"nightly"}'` instead of repeatable `--payload`
when the input already exists as JSON. The forms are mutually exclusive and use
the bridge's shared payload limits. If watch exits `2` with `still running`,
re-invoke it with another timeout window; do not teach agents to poll `pipeline
show` in a loop.

### Park and resume

The core story is **park-then-resume**. When a stage returns `blocked`, the run
parks: its `needs` are persisted and downstream stages are never enqueued, so the
run consumes zero compute while it waits. `pipeline show <run-id>` makes the halt
obvious as a funnel:

```
source OK -> score BLOCKED (needs: R2 token) -> deploy SKIPPED
```

For an active run, `pipeline show` also lists each queued/running stage with the
time since it was enqueued. After a pipeline job has run for a minute, its worker
updates one latest-only `progress` event about every 30 seconds; the view prints
the event age and its last sanitized output line. That age visibly grows when
updates stop. An orchestrate stage can temporarily point at a settled coordinator
while its children run, so absent or stale per-stage progress is informational and
renders as `(sub-tree running; no per-stage progress)`, never as failure.

The operator provisions what the stage needs out of band (here, an R2 token), then
resumes — which re-runs the halted stage and everything downstream of it, while the
already-landed upstream stages are left untouched:

```sh
gitmoot pipeline resume "$RUN"          # re-runs score + deploy; source is NOT re-run
gitmoot pipeline resume "$RUN" --from source   # re-run from an explicit stage instead
```

A stage that returns `failed` (or any non-success decision, a cancelled job, or no
`gitmoot_result`) parks the run **failed** after exhausting its `retry` budget;
`pipeline show` then prints the exact `gitmoot report bug --job <stage-job>` command
for the halted stage (it never files the bug for you). Approval gates that resume a
blocked run automatically are a follow-up (#682) — v1 is the manual `resume` verb.

### Stages are leaves

A pipeline is **not** an orchestra. Each stage is a leaf: it runs a shell command
and returns a decision, full stop. A stage result that carries `delegations[]` does
**not** spawn children — the advancer ignores them and the engine strips them for a
pipeline stage job, so a pipeline can never fan out into a delegation tree. Reach
for an orchestra (a coordinator returning `delegations[]`) when you want dynamic,
model-driven decomposition; reach for a pipeline when the steps and their wiring are
known up front. See `../../../docs/pipelines.md` for the full reference.
