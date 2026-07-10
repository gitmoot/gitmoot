# Pipelines

Pipelines (#681) let the gitmoot daemon run a **declared DAG of shell stages** —
a small, durable multi-step flow — on demand or on an interval schedule. Each
stage is an ordinary queued job run through the **shell runtime**: the normal
worker tick claims and runs it, and a scan-based **advancer** folds each stage's
`gitmoot_result` decision and enqueues the stages whose dependencies have all
succeeded. Pipelines reuse the same job queue, result contract, and scheduling
idiom as heartbeats — there is no separate runner.

Pipelines are **off by default**: with no pipelines defined, the daemon's
pipeline scan returns an empty list before touching any state, and behavior is
unchanged.

A pipeline is not an orchestra. Each stage is a **leaf**: it runs a shell command
and returns a decision. A stage that emits `delegations[]` does **not** spawn
children — the advancer ignores them and the engine strips them for a pipeline
stage job, so a pipeline can never fan out into a delegation tree. Reach for an
orchestra (`gitmoot orchestrate` / a coordinator that returns `delegations[]`)
when you want dynamic decomposition; reach for a pipeline when you have a fixed,
repeatable sequence of shell steps with explicit dependencies.

## The spec

A pipeline is declared as a YAML file and registered with `gitmoot pipeline add`.
The raw bytes are stored **verbatim** in the local store alongside a content hash;
each run snapshots the hash it was created from, so a run always executes the spec
it was created against — editing the file later (even whitespace) changes the hash
and does not affect an in-flight run.

```yaml
name: nightly-sync          # required, name-safe token (letters, digits, - _)
repo: owner/repo            # optional to register; REQUIRED to actually run
schedule:                   # optional; interval schedule (no cron in v1)
  interval: 24h             #   required when a schedule block is present (positive Go duration)
  jitter: 15m               #   optional random [0, jitter] added to each next_due (>= 0)
success_decisions:          # optional top-level default (see below)
  - approved
  - implemented
  - skipped
stages:                     # the DAG, keyed by unique id and wired by needs
  - id: source
    cmd: "curl -sf https://example.com/data > data.json"
  - id: score
    cmd: "python score.py data.json"
    needs: [source]         # runs only after every listed stage SUCCEEDS
  - id: deploy
    cmd: "rclone copy out/ r2:bucket"
    needs: [score]
    timeout: 30m            # optional per-stage job timeout (positive Go duration)
    retry: 2                # optional; re-attempt a FAILED stage up to N times (>= 0)
    success_decisions:      # optional per-stage override of the pipeline default
      - approved
```

### Fields

| Field                       | Scope        | Required | Notes |
| --------------------------- | ------------ | -------- | ----- |
| `name`                      | pipeline     | yes      | Stable identifier and DB primary key; a name-safe token (letters, digits, `-`, `_`). |
| `repo`                      | pipeline     | no\*     | `owner/name` the stages run against. Optional to **register**, but **required to run** — stage jobs need a managed repo for the worker to claim them. |
| `schedule.interval`         | pipeline     | cond.    | Required when a `schedule:` block is present. A positive Go duration (`24h`, `1h30m`). |
| `schedule.jitter`           | pipeline     | no       | Random `[0, jitter]` added to each `next_due` to de-thunder (`>= 0`). |
| `success_decisions`         | pipeline     | no       | Decisions that mark a stage succeeded. Default `["approved","implemented","skipped"]`. Any value must be one of `approved`, `implemented`, `changes_requested`, `skipped` - `blocked`/`failed` are park states and are rejected. An explicit list is strict: omitting `skipped` requires real work and makes a skipped result fail. |
| `allow_scheduled_writes`    | pipeline     | no       | Safety flag (#768). A **mutating** `implement` stage on a **scheduled** pipeline (a `schedule:` block) is rejected at add time unless this is `true` — so an unattended nightly pipeline can never write code / open a PR by accident. Manual-run pipelines don't need it. Default `false`. |
| `stages[].id`               | stage        | yes      | Unique, name-safe stage id. Appears verbatim in the stage job's fingerprint and deterministic id. |
| `stages[].cmd`              | stage        | cond.    | Shell command run verbatim via `sh -c` (see the stage contract below). A stage is **exactly one** of `cmd`, `agent`, or `gate`. |
| `stages[].agent`            | stage        | cond.    | Name of a managed gitmoot agent to run this stage (instead of a shell `cmd`). Must be a name-safe token; `pipeline add` warns (does not block) if the agent does not exist yet, but it must exist before the stage runs. Mutually exclusive with `cmd`/`gate`. The stage kind depends on `action`/`write`/`orchestrate` — see [Agent stages](#agent-stages). |
| `stages[].prompt`           | stage        | cond.    | Instruction handed to an agent stage's agent. **Required** for an agent stage; rejected for a shell/gate stage. Prepended with the upstream `needs` stages' result summaries at enqueue. |
| `stages[].action`           | stage        | no       | Verb for an agent stage: `ask` (default), `review`, or `implement` (#768). `ask`/`review` are read-only leaves; `implement` mutates the repo + opens a PR (requires `write: true`). Rejected for a shell/gate stage. |
| `stages[].write`            | stage        | cond.    | Acknowledges that an `implement` stage **mutates** the repo (#768). Required with `action: implement`, and valid **only** there — a double-key so a typo/injection can't turn a read-only pipeline into a writing one. |
| `stages[].orchestrate`      | stage        | no       | `true` makes an agent stage a **sub-tree coordinator** (#758): its `delegations[]` fan out as owned children and the stage waits for the whole tree, then folds the synthesis. Agent stage only; `action` must be `ask`. |
| `stages[].gate`             | stage        | cond.    | Makes this a **jobless gate stage** (#768): it runs no worker and folds when an external predicate holds. Only `pr_merged` today. Exclusive with `cmd`/`agent`. Requires `source`. |
| `stages[].source`           | stage        | cond.    | For a `gate` stage: the id of the upstream `implement` stage whose PR to watch. Must be one of the gate's own `needs`. |
| `stages[].needs`            | stage        | no       | Ids of sibling stages that must **succeed** before this stage is enqueued. Must reference known stages, never the stage itself, and form no cycle. |
| `stages[].timeout`          | stage        | no       | Per-stage job timeout (positive Go duration). |
| `stages[].retry`            | stage        | no       | How many times a **failed** stage may be re-attempted (`>= 0`, default `0`). |
| `stages[].success_decisions`| stage        | no       | Per-stage override of the pipeline default. |

`gitmoot pipeline add` validates the whole spec **at add time** — unknown keys, a
non-name-safe name/id, a duplicate stage id, a stage that is not **exactly one** of
`cmd`, `agent`, or `gate` (and, per kind: an agent stage's missing `prompt`, an
invalid `action`, `implement` without `write: true` or `write: true` off an
implement stage, a mutating stage on a scheduled pipeline without
`allow_scheduled_writes`, a gate's invalid predicate or a `source` that is not an
upstream implement stage in `needs`), an unknown/self/cyclic `needs`, an invalid
timeout/interval/jitter, a negative retry, or a `success_decisions` value outside the
allowed set — so a structural mistake surfaces as a clear error at registration
rather than a stuck run later.

### Agent stages

A stage may run a **named managed gitmoot agent** instead of a shell command. There
are four agent-stage kinds, all sharing the mechanics in this section:

| Kind | Declared by | What it does |
| ---- | ----------- | ------------ |
| **ask** / **review** (#757) | `agent` + `action: ask\|review` | Read-only leaf: looks + decides, never mutates. |
| **implement** (#768) | `agent` + `action: implement` + `write: true` | Mutates the repo + opens a PR (fold-on-PR-opened). See [Implement stages](#implement-stages). |
| **orchestrate** (#758) | `agent` + `orchestrate: true` | Sub-tree coordinator: fans out owned children, waits for the tree, folds the synthesis. See [Orchestrate stages](#orchestrate-stages). |
| **gate** (#768) | `gate:` (no `agent`) | Jobless waiter: folds when an external predicate holds (e.g. a PR merges). See [Gate stages](#gate-stages). |

The basic read-only form (#757) sets `agent` + `prompt` (and optionally `action`) in
place of `cmd`:

```yaml
stages:
  - id: extract
    cmd: "python fetch_replies.py > replies.json"
  - id: triage
    agent: reply-triager      # managed agent — create it before the pipeline runs
    action: ask               # ask (default) | review — read-only ONLY
    prompt: "Triage the fetched replies; block if anything needs a human."
    needs: [extract]
```

- **Runs on the agent's own runtime.** Unlike a shell stage (which runs through the
  hidden per-pipeline shell runner via a per-job runtime override), an agent stage
  binds the stage job to the named agent and runs it on **its own** registered runtime
  (claude / codex) and session — no runtime override.
- **Leaf by default.** An `ask`/`review`/`implement` stage is a **leaf**: its
  `delegations[]` and `human_questions[]` are stripped (Sender is the pipeline sender),
  so it can neither fan out nor pause a human. (An `orchestrate` stage is the one
  exception — it is a coordinator, not a leaf; see [Orchestrate stages](#orchestrate-stages).)
- **Agent existence is warned at add time.** `pipeline add` checks every referenced
  agent and prints a warning for any that does not exist yet, but does **not** block —
  a spec may legitimately be added before its agents are provisioned (bundled or
  shareable pipelines, scripted setup). A genuinely-missing agent still fails loudly
  when the stage runs, so create it before the pipeline runs (`gitmoot agent …`).
- **Upstream needs-context injection.** At enqueue the stage prompt is **prepended**
  with a bounded `Upstream stage results` block — one labeled entry per `needs` stage
  carrying that stage's fold state and result summary — so a downstream agent stage
  acts on upstream output as real dataflow. Each summary is **fenced** (a backtick
  fence sized longer than any run inside it) so an upstream summary can never spoof the
  block structure or inject instructions into the downstream agent. The block is
  size-bounded and each summary is truncated with a `[truncated]` marker when oversized;
  a root agent stage (no `needs`) receives the bare prompt.
- **Read-only worktree isolation.** A repo-bound ask/review agent stage is born with
  its own detached, committed-tip **read-only worktree** (#739), so it keys
  `worktree:<path>` rather than the shared `repo:<repo>` live checkout — same-repo
  agent stages then run **concurrently** and never mutate the live checkout. The
  worktree is disposed when the stage job settles (and reclaimed on daemon restart).
  Allocation is fail-open: if it cannot be created the stage still runs, serialized on
  the shared checkout, with a loud skip event. A pure-reasoning agent stage with no
  `repo` needs no worktree.
- **Stateless per run.** Because each agent stage runs in a freshly-created worktree
  directory, a runtime that scopes sessions by working directory (e.g. Claude Code)
  starts a **fresh session each run** — a pipeline stage carries no session context
  across runs. This is intended: a pipeline run is deterministic and independent, so
  supply everything a stage needs via its `prompt` and the injected upstream context,
  not via accumulated agent memory.

Agent stages fold by `decision` and advance/park exactly like shell stages.

### Implement stages

An `implement` agent stage (#768) **mutates the repo and opens a pull request**, then
the pipeline advances the moment the PR is opened (**fold-on-PR-opened**):

```yaml
stages:
  - id: fix
    agent: fixer
    action: implement
    write: true                       # required acknowledgement (see Safety)
    prompt: "Apply the change described in the ticket."
  - id: verify
    agent: reviewer
    action: review
    needs: [fix]
```

- **Writable worktree + deterministic branch reuse.** Unlike a read-only stage, an
  implement stage runs in a **writable** task-worktree on a deterministic,
  attempt-independent branch `gitmoot/pipe-<run>-<stage>` (reusing the implement
  dispatch's fail-closed guards). A **retry** lands in the *same* branch/PR — never a
  duplicate — and fails closed if that worktree is dirty or has a live process.
- **Folds on PR-opened.** A `success` decision folds only once the job carries an
  opened PR. The first-class `skipped` decision bypasses that wait because there is
  no work to turn into a PR. The opened PR number is appended to the stage summary so a
  downstream stage sees it via upstream-context injection.
- **Never auto-merges.** The pipeline **opens** the PR; a human or your CI does the
  merge. See [Gate stages](#gate-stages) to make a later stage wait for that merge.
- Still a **leaf** — it may mutate but never fan out.

### Gate stages

A `gate` stage (#768) runs **no worker job** — it is a patient waiter that folds when an
external predicate holds. It is the composable way to express *wait-for-merge* without
making the implement stage itself block:

```yaml
stages:
  - id: fix
    agent: fixer
    action: implement
    write: true
    prompt: "Apply the change."
  - id: wait
    gate: pr_merged                   # only predicate today
    source: fix                       # the upstream implement stage whose PR to watch
    needs: [fix]
  - id: deploy
    cmd: "./deploy.sh"
    needs: [wait]
```

- **`pr_merged`** watches the PR opened by the `source` implement stage and folds
  **succeeded** once it merges. It reads the store's PR state (kept current by the
  daemon's PR poller), so it needs no live GitHub call, is **non-blocking** and
  **fails open** (keeps waiting while unmerged), and is bounded by the stage `timeout`.
- A PR that is **closed without merging**, or a **timeout**, parks the run `blocked`
  (a gate is a wait, not a failure — so a retry budget can't re-arm the timer).
- A source stage that succeeded via `skipped` also parks the gate `blocked`
  immediately: no PR will exist for that run.

### Orchestrate stages

An `orchestrate` agent stage (#758) is a **bounded sub-tree coordinator**: instead of
doing the work itself, its agent returns `delegations[]` that fan out as **children it
owns**, and the stage waits for the whole tree, then folds the synthesis:

```yaml
stages:
  - id: investigate
    agent: coordinator
    orchestrate: true
    prompt: "Decompose the incident and delegate the sub-investigations."
  - id: report
    cmd: "./publish.sh"
    needs: [investigate]
```

- **The stage job is the sub-tree root.** Children inherit the full delegation bounds
  ladder (depth cap, job-budget admission, wall-clock / token / cost budgets, loop
  detection, graceful finalize, root kill). The stage `timeout` bounds the whole tree.
- **Not a leaf.** The delegations strip is relaxed **only** for a validated orchestrate
  stage — a stage that merely sets `orchestrate: true` but validates as something else
  cannot fan out.
- **Waits, then folds the synthesis.** The pipeline follows the coordinator's
  continuation chain to the terminal tail and folds *that* result — a pure, restartable
  DB walk (a daemon restart re-derives the wait, never restarting the sub-tree).
- **`retry: 0` recommended** — a retry mints a fresh tree only after the old one is
  terminal, never resuming a half-finished tree.

### The stage contract

A stage command runs under the shell runtime as `sh -c '<cmd>' gitmoot '<prompt>'`
(so `$0` is `gitmoot` and `$1` is the stage's job prompt). The stage signals its
outcome by printing a `gitmoot_result` JSON blob to **stdout**; the advancer folds
the stage by that result's **`decision`**, never by the job's exit state:

```sh
# A stage that succeeds:
printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"synced"}}'

# A stage whose task had no work:
printf '%s' '{"gitmoot_result":{"decision":"skipped","summary":"no new replies today"}}'

# A stage that parks the run awaiting a human, listing what it needs:
printf '%s' '{"gitmoot_result":{"decision":"blocked","summary":"secret missing","needs":["R2 token"]}}'
```

The decision drives advancement:

- **`decision` in the stage's `success_decisions`** → the stage **succeeded**;
  stages whose `needs` have all now succeeded are enqueued.
- **`decision: blocked`** → the stage is **blocked**; its `needs` are persisted at
  the stage and run level and the run **parks blocked**. Downstream stages are
  never enqueued.
- **`decision: failed`, any decision outside the stage's `success_decisions`
  (`changes_requested` by default), a cancelled job, or no `gitmoot_result` at all**
  → the stage **failed**. If the stage has retry budget left it is re-attempted;
  otherwise the run **parks failed**.

`changes_requested` is a stage **failure by default** — even though the underlying
job succeeded — because a stage folds on the decision, not the job state, and a
review that asked for changes is not "this step landed." List it in a stage's (or
the pipeline's) `success_decisions` to treat it as success instead.

`skipped` is a stage **success by default**. Gitmoot prefixes its persisted summary
with `[skipped: no work]`, and downstream agent stages receive that honest note in
their upstream context. An explicit `success_decisions` list is strict: if it omits
`skipped`, the stage fails because the author required real work. A skipped result
uses the existing succeeded stage state; the `SKIPPED` funnel state still means a
downstream stage never ran after the pipeline halted.

## CLI

```sh
# Register (validate + store). Omit --enable to add it disabled (no scheduling).
gitmoot pipeline add nightly-sync.yaml --enable

gitmoot pipeline list [--json]
gitmoot pipeline show <name> [--json]        # registry view for a pipeline name
gitmoot pipeline show <run-id> [--json]      # run funnel for a "prun-…" run id

gitmoot pipeline run <name>                  # start a manual run; prints the run id
gitmoot pipeline resume <run-id> [--from <stage>]
gitmoot pipeline cancel <run-id>

gitmoot pipeline enable <name>
gitmoot pipeline disable <name>
gitmoot pipeline remove <name>
```

`pipeline run` prints just the run id (script-stable), so `RUN=$(gitmoot pipeline
run nightly-sync)` works. A manual run ignores the `enabled` flag — a disabled
pipeline can still be run by hand — but still requires a `repo` and refuses to
start while the pipeline already has an active run (one active run per pipeline).

`pipeline show <run-id>` renders the run as a **text funnel**:

```
run: prun-nightly-sync-18bfa02e9afb86ed
pipeline: nightly-sync
trigger: manual
state: blocked
started: 2026-07-06T06:41:39Z
finished: 2026-07-06T06:42:10Z
halt_stage: score
halt_reason: secret missing
needs: R2 token

source OK -> score BLOCKED (needs: R2 token) -> deploy SKIPPED
```

Funnel labels are `OK` for a succeeded stage, `BLOCKED (needs: …)` for a parked
stage, and the uppercased state otherwise (`PENDING`, `QUEUED`, `RUNNING`,
`FAILED`, `SKIPPED`, `CANCELLED`). When a run **failed**, the view also prints the
exact command to file a bug for the halted stage's job — gitmoot never files it for
you:

```
stage failed; report it with:
  gitmoot report bug --job <stage-job-id>
```

`--json` on `list` and `show` emits a stable machine shape (pipelines as an array;
a run as `{id, pipeline, trigger, state, halt_stage, halt_reason, needs, spec_hash,
started_at, finished_at, funnel, stages[]}`).

### Resume

`pipeline resume <run-id>` re-runs a **parked** (blocked or failed) run from its
halted stage; `--from <stage>` overrides the resume point. Resume resets the halted
stage (or `--from` stage) and every stage that transitively **depends** on it back
to pending — bumping each reset stage's attempt so the next scan enqueues a fresh
stage job — clears the run's park fields, and returns the run to `running`. It
**never re-runs a stage that already succeeded**, even one downstream of the resume
point, and it refuses a run that is not parked. Because a run executes its spec
snapshot, resume is refused if the pipeline's spec changed since the run was
created.

The intended story: a stage blocks on something the operator must provide (a
missing secret, an unapproved change), the operator provisions it out of band, then
`pipeline resume` re-runs the halted stage and everything downstream — while the
already-landed upstream stages are left untouched. Approval gates that call resume
automatically are a follow-up (#682); v1 is the manual verb.

### Cancel and remove

`pipeline cancel <run-id>` abandons a running or parked run: it cancels each
in-flight stage job through the shared `job cancel` path (which also best-effort
releases the job's locks) and marks the run and its non-terminal stages
`cancelled`. An already-settled stage keeps its recorded outcome, so a cancelled
run still shows why it halted. It refuses an already-terminal (succeeded/cancelled)
run.

`pipeline remove <name>` deletes the pipeline and disposes its hidden shell runner
agent (below).

## Scheduling

A pipeline with a `schedule.interval` and `enable`d auto-runs on that interval,
using the same durable-`next_due` idiom as heartbeats. The daemon's pipeline scan
runs in **both** the registered-repo and single-repo daemon loops and does two
passes per tick:

1. **Schedule pass** — for each enabled pipeline whose interval is due and that has
   no active run, create a fresh scheduled run and advance `next_due` **anchored to
   now**.
2. **Advance pass** — advance every in-flight run once (fold settled stage jobs,
   enqueue newly-ready stages, park or finish).

A parked or terminal run is never advanced, so a blocked/failed run consumes **zero
compute** while it waits. Behavior worth knowing:

- **Interval + jitter only** — there is no cron parser in v1; because `next_due` is
  durable, a cron front-end is a documented drop-in for a later release.
- **One active run per pipeline** — a scheduled tick that finds a run in flight is
  skipped without advancing `next_due`, so the next run fires as soon as the current
  one settles. A parked run does **not** count as active.
- **Missed ticks coalesce** — a long-idle scheduler that missed many intervals fires
  exactly **one** run and schedules the next one interval out; it never replays a
  backlog.
- **Restart-safe** — the advancer recovers purely from the persisted run/stage rows,
  so a daemon restart mid-run picks the run back up and completes it.
- **Repo required to run** — a scheduled pipeline with no `repo` is skipped and its
  `next_due` advanced (so a misconfigured schedule does not hot-loop and
  self-recovers once a repo is set), mirroring the heartbeat repo-unmanaged idiom.

## The hidden runner agent

`pipeline add` auto-creates one hidden shell agent per pipeline, named
`pipeline-<name>-runner`, that owns the pipeline's stage jobs (the worker loop
resolves a job's agent record). The stage command travels **per job** (in the stage
job's runtime-override ref), not on the agent's runtime ref, so one runner serves
every stage. These runner agents are an implementation detail and are **filtered out
of `gitmoot agent list`**; `pipeline remove` disposes them.

## Observability

- `gitmoot pipeline list` shows each pipeline's enabled state, interval, repo, and
  last run status.
- `gitmoot pipeline show <name>` shows the registry view (spec hash, schedule,
  last/next run bookkeeping, and the stage DAG).
- `gitmoot pipeline show <run-id>` shows the run funnel.
- Stage jobs are ordinary jobs (sender `pipeline`), so they also appear in the usual
  job/status surfaces.

### Web dashboard

The web dashboard (`gitmoot dashboard --web`) has a dedicated, read-only
**Pipelines** view: a list of every declared pipeline with its schedule state,
next-due countdown, and recent-run outcomes, and a per-run detail that renders the
stage DAG in spec (topological) order with the same halt/needs information the
`pipeline show <run-id>` funnel prints. It surfaces the resume / bug-report
commands for a parked run as copyable text but never mutates anything. See
[Dashboard Views → Pipelines](https://gitmoot.io/docs/dashboard/views#pipelines).

## Safety

`pipeline add` is an **operator-trust action**: a stage's `cmd` runs verbatim via
`sh -c` with the daemon's permissions. Only register specs you would run yourself —
the same trust you extend to a heartbeat prompt or a CI step. The spec is stored
verbatim (raw bytes), so treat a spec containing private hostnames, paths, or repo
names the same as any other private-repo data. See `SAFETY.md → Pipeline stages run
with daemon permissions`.

**Mutating (`implement`) stages** carry an extra safety model (#768): `action:
implement` requires an explicit `write: true` double-key (and `write: true` is valid
only on an implement stage), so a typo or prompt injection cannot flip a read-only
pipeline into a writing one; a mutating stage on a **scheduled** pipeline is rejected
unless the pipeline sets `allow_scheduled_writes: true`; the bound agent's own
capability/policy still applies; and the pipeline **never merges its own PR** — it
opens the PR and leaves the merge to a human or CI.

## Not yet supported (deferred)

These are intentionally out of scope for v1 and tracked as follow-ups:

- A cron schedule front-end (interval + jitter only today).
- Approval gates / secret stores / an approval UI for a blocked stage (#682) — v1
  ships the manual `pipeline resume` seam.
- Auto-filing a bug for a failed stage (`show` prints the command; you run it).
- Per-stage env/workdir. (Agent stages and upstream stage output flowing into a
  downstream stage's prompt **are** supported — see [Agent stages](#agent-stages).)
- Gate predicates beyond `pr_merged`, and a `gate` folding on PR-**merged** built into
  the implement stage itself (use a separate `gate` stage).
- Matrix / dynamic stages, more than one concurrent run per pipeline, pipelines
  defined from the dashboard, a web funnel view, and a foreground `--watch`.
</content>
