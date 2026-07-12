# Dashboard Views

The web dashboard has nine views, reachable from the left nav rail (or the mobile
bottom tab bar). Each is described below with its screenshot. See
[Dashboard Overview](./overview.md) for launching, routes, refresh cadences, and
security.

## Graph

Route: `/` (or `/?run=<id>`)

![Graph view: a per-run delegation DAG with the runs column and a node detail aside](/img/dashboard/graph.png)

The Graph view is a **single run's delegation DAG**. Nodes are the jobs in that
run's tree; edges are delegation parentage plus each child's declared
dependencies resolved to its sibling jobs. Node color tracks state, and the
selected run streams over SSE — running nodes pulse and settle live.

- **Runs column** (left): every orchestration run, active (running/queued) runs
  first. A search box matches over run title and prompt snippet, and facets
  filter by **status, kind, agent, and repo** (AND across categories, OR within
  one). Selecting a run loads its graph and updates the URL to `/?run=<id>`.
- **Node detail aside** (right): click a node to open its prompt, output, event
  timeline, agent/runtime/model, state, depth, and a link to its pull request
  when one exists.

## Galaxy

Route: `/galaxy`

![Galaxy view: a whole-history force graph with repo and agent hubs](/img/dashboard/galaxy.png)

Galaxy is the **whole-history force graph** — every job across every run in one
canvas, clustered around synthetic **repo** and **agent** hub nodes. Links are
delegation parentage, a per-parent sibling mesh (capped so one huge fan-out can't
dominate the density), and job-to-repo / job-to-agent spokes.

- Running jobs render as live **beacons** (floored to a minimum on-screen size so
  they stay visible when zoomed out) and pulse.
- **Flowing particles** travel the links touching a running job (bright and fast),
  with a subtle slow particle on ordinary delegation links, so active work is
  legible from the zoomed-out view.
- A **repository** filter scopes the visible jobs and their hubs to one repo; the
  filter list always stays complete regardless of the current scope.
- Clicking a node opens the same detail panel (pinned right on desktop, a
  full-screen sheet on mobile). A readout shows how many nodes and links are
  currently drawn.

## Jobs

Route: `/jobs` (or `/jobs?agent=<name>`)

![Jobs view: a filterable table of every job across all runs](/img/dashboard/jobs.png)

Jobs is a flat, filterable view of **every job across every run**, newest activity
first. It renders as a table on desktop (columns: Title, Agent, Repo, Kind, Depth,
Tokens, Started, Duration) and as cards on mobile.

- A search box matches over **title, agent, id, and repo**; facets filter by
  **State, Repo, and Agent** (the `?agent=` route pre-selects the agent facet).
- The table is capped at **400 rows** after filtering; when more match, a footer
  reads `showing 400 of N — refine filters`, and a running `showing X of Y`
  count sits in the header.
- Clicking a row (or card) opens a detail panel with the job's prompt, output,
  and event history.

## Pipelines

Route: `/pipelines` (or `/pipelines/<name>`)

Pipelines is the read-only view of the declared **shell-stage pipelines** — the
scheduled/manual DAGs defined with `gitmoot pipeline add`. It is **strictly
read-only**: every action it surfaces is a **copyable command**, never a mutation.
When no pipeline is declared yet, the view shows an empty state prompting
`gitmoot pipeline add <spec.yaml>`.

**List view** — a table, one row per pipeline (columns: **Name**, **Repo**,
**Enabled** chip, **Schedule** rendered as `every 24h +15m`, **Last run** state
chip, **Next due** countdown rendered from the schedule as e.g. `due in 6h 12m`
and recomputed each poll, and a **Recent** outcomes strip of the last ≤10 runs as
small state-colored squares, oldest→newest left to right). Clicking a row opens
that pipeline's detail (`/pipelines/<name>`).

**Detail view** — a back link, a pipeline header (name, repo, enabled, schedule,
next due), and a **run-history strip** of recent runs (each a chip: state color,
short run id, relative time). Selecting a run (newest by default) renders that
run's **stage DAG** and, when the run parked, a banner:

- a **blocked** run shows an amber banner with the halt stage, halt reason, the
  outstanding **needs**, and a copyable `gitmoot pipeline resume <run-id>`;
- a **failed** run shows a red banner with the halt reason and a copyable
  `gitmoot report bug --job <failing-stage-job-id>`.

The **stage DAG** lays stages out in dependency (depth) columns, left to right,
edges following the spec's `needs`. Each stage chip shows its id, state, attempt
(when it retried), elapsed time, and a one-line summary; pending and skipped
stages render dimmed. Clicking a stage opens a side panel: stages with a job show
the full job detail (prompt, output, events) fetched from `/api/job/<id>`; a
pending or skipped stage (no job yet) shows a minimal card with its id, state,
command, and dependencies. An in-flight run's stages flip live as the dashboard
polls (every 12s).

The detail view also fetches `/api/pipelines/<name>` for the pipeline's full
history and declared shape. It shows a **run-history table** — one row per run
(newest-first, capped at the last 100), with started time (absolute and
relative), trigger, state chip, duration, and halt stage — where selecting a row
drives the strip, matrix, and DAG together. A **stage×run matrix** places the
stages down the rows (declared order first, any history-only ids appended) and
the most recent runs across the columns oldest→newest, each cell a
state-colored dot, so a stage that keeps failing reads as a streak across a row;
clicking a cell selects that run. When the pipeline has a schedule, an
**upcoming card** projects the next few fire times from `next due + k × interval`
(with the jitter suffixed when set); a disabled schedule shows *paused* and a
manual-only pipeline shows the copyable `gitmoot pipeline run <name>`. A pipeline
that has **never run** renders its **declared DAG** (every stage pending, from
the current spec) as a preview instead of a run. This detail feed is strictly
read-only and deterministic; the list rows additionally summarize each pipeline's
recent success rate and average duration.

## Learning

Route: `/learning` (opens `/learning/skills`; second tab `/learning/knowledge`)

Learning is one nav item with two tabs telling one story: what the agents are
learning. **Skills** is the SkillOpt evolution overview — one row per agent
template with its version history as an inline score-trend sparkline, the
current version, any active canary, and pending candidates (each expandable to
a copy-paste `gitmoot skillopt candidate promote` command). Clicking a row
jumps to that agent's detail panel. The memory fact galaxy that used to live
here as a second tab is now the top-level **Brain** view (below) —
one lane per repo scope, each lane holding its facts as a wiki-link
constellation, cluster and sub-cluster hub columns, and enrolled agents as
wells on the right; clicking a fact, cluster, or agent opens its detail panel.
Deep cluster trees stay scannable through progressive disclosure: a `+n` badge
on a hub reveals hidden intermediate levels, and Escape (or clicking empty sky)
collapses back. Three header toggles control density: **fact links** (wiki
links between facts), **cross-repo links** (links whose endpoints live in
different repo lanes render as dashed stubs at the lane border; hovering or
selecting a fact draws its full dashed curve to the partner), and **history**
(off by default — superseded ghost facts and their red supersede edges are
hidden; toggle on to see how facts were replaced over time, e.g. by the
automatic brick splitter). Unclustered facts sit in their own repo's lane.
Both tabs are read-only and poll every 12 seconds.

## Workflows

Route: `/workflows` (index) and `/workflows/{label}` (mission log)

Workflows are labeled campaigns of related work driven by a coordinator: jobs
dispatched with `--workflow <label>` plus the journal written with
`gitmoot workflow note`. Labels may carry one namespace slash
(`fable/dashboard-redesign`) — by convention the coordinator's herdr pane name
namespaces the campaign; unattended sources use reserved namespaces.

The **index** groups every known workflow by derived lifecycle: **stalled**
pinned on top ("needs a look"), then **active**, then recently **settled**.
A workflow is stalled only when a failure has gone **unacknowledged** — nothing
is running, the goal was not reached, and no journal note has been written
since the last failure (a failure alone is not an alarm; the silence after it
is). Stalled entries age into settled history after a day of silence.

The **detail page** reads as a mission log: journal notes and run trees
interleaved in reverse-chronological order — the coordinator's intent next to
the runs it produced. Run blocks expand inline to their child jobs; a
"view as graph" link opens the classic per-run node graph as a drill-down.
The header carries the coordinator identity (author, herdr pane, session id),
and a stalled workflow shows a go-here card with the pane, the session id, and
a copyable resume command — the dashboard tells you where to intervene; it
never intervenes itself.

## Brain

Route: `/brain` (the old `/learning/knowledge` route redirects)

The memory fact galaxy, promoted from Learning's second tab to a first-class
view: one lane per repo scope, facts as wiki-link constellations, cluster and
sub-cluster hub columns, enrolled agents as wells, progressive disclosure for
deep cluster trees, and the fact-links / cross-repo / history header toggles.
See the Learning section above for how facts are produced; Brain is where you
read them.

## Agents

Route: `/agents` (or `/agents/<name>`)

![Agents view: a grid of agent cards with per-agent stats](/img/dashboard/agents.png)

Agents is a grid of cards — one per registered agent, plus a single rollup card
for the fleet of ephemeral workers. Each card shows the agent's runtime, role,
model, repo scope, and job tallies (total, running, succeeded, failed) with its
last-active time.

Clicking a card opens the agent detail (`/agents/<name>`):

![Agent detail: the template card and full prompt viewer with a copy button](/img/dashboard/agent-detail.png)

- **Template card** — the agent's template identity (name, description, source
  repo/ref/path, resolved commit) and the **full prompt viewer**: the exact
  resolved prompt body an agent runs, with a **copy** button.
- **Version history** — every template version, newest first, with the version
  Gitmoot currently resolves to marked. Each version carries its own full prompt
  and a collapsed-by-default **diff** toggle that shows a **unified diff against
  its predecessor version** (long unchanged runs collapse to their first and last
  few lines):

![Agent detail: a unified diff between two template versions](/img/dashboard/agent-diff.png)

## Charts

Route: `/charts`

![Charts view: activity time series and top agents/repos](/img/dashboard/charts.png)

Charts summarizes activity over a selectable window — **7d, 30d, 90d, or all**
history (default 30d):

- **Jobs per day**, stacked by state (succeeded, failed, cancelled, blocked,
  queued, running), zero-filled continuously across the range.
- **Tokens per day** (input and output).
- **Top agents** and **Top repos** by job count (each capped at the busiest 12).

The series and totals reflect the selected range and refresh on the standard 12s
cadence.

## Health

Route: `/health`

![Health view: daemon status, state totals, stuck jobs, locks, and recent failures](/img/dashboard/health.png)

Health is the operability view for unattended runs:

- **Daemon** — running/stopped, PID, uptime, the running binary's version, and an
  update chip (`up to date`, or `update available: <version>` linking to the
  release).
- **State totals** — a chip per state (queued, running, blocked, succeeded,
  failed, cancelled).
- **Stuck jobs** — blocked jobs (surfaced at any age) plus queued jobs older than
  10 minutes, each with a **derived why-stuck reason** (from the job's latest
  reason event and any resource lock it is waiting on) and how long it has been
  wedged.
- **Locks** — branch/checkout locks (`repo@branch`, owner, acquired time) and
  non-branch resource locks such as runtime sessions (key, owner, acquired,
  expires).
- **Recent failures** — the ten newest failed jobs.

A blocked job stays blocked until you clear it or a sweep does. Use
`gitmoot job cancel <job-id>` to abandon one, or `gitmoot job cancel --state
blocked` to clear a backlog; set `[orchestrate].blocked_ttl` to sweep old blocked
jobs automatically, and run `gitmoot doctor` for the environment preflight that
warns about a blocked-job pileup. All of these are documented under
[Jobs And Locks](../reference/cli.md#jobs-and-locks).

## Needs a human

Route: `/attention`

Needs a human is the fleet-wide roll-up of everything parked on an **explicit human
decision** — the one place to see what is waiting on you, grouped by the action
required. Three buckets:

- **Blocked job gates** — jobs blocked on a human-satisfiable gate (a `--need`
  recorded against the job). Each row carries the job's title, agent, repo and PR,
  the exact need, and how long it has been waiting. Clear one with
  `gitmoot job gates clear <job-id> --need "<need>"`. Expand a row to see that job's
  **failed result checks** — the deterministic checks its result failed, each with
  the question asked and the evaluator's explanation, plus the home-wide
  `[workflow] result_checks` policy in force (`warn` or `block`).
- **Pending synth approvals** — synthesized SkillOpt review items awaiting the
  human approval gate (the challenger/weak-vs-strong items, each with its question,
  the weak→strong agents, the judge, and the score gap).
- **Candidates awaiting promotion** — agent-template candidate versions in the
  `pending` state, waiting to be promoted (version number and its review score).

Human approval stays **explicit** for these ambiguous or high-stakes evaluator
outcomes: the view surfaces them and links to the action, but nothing is auto-approved.

The same failed-check and binary-verdict payloads back the planned Slack/media
bridge ([#519](https://github.com/jerryfane/gitmoot/issues/519)), which needs
compact human-action status to post into a thread. Two read-only endpoints expose
them for bridge consumers: `GET /api/job/{id}/checks` (a job's failed result checks
+ policy mode) and `GET /api/run/{id}/verdicts` (a SkillOpt run's per-question
binary verdicts with pass/fail counts).

## Config

Route: `/config`

The read-only effective-configuration viewer. Every known knob is listed by
config section with its current value, its default, and a `default` /
`overridden` badge (overridden values stand out); boolean feature flags render
as ON/OFF chips, and the off-by-default feature flags (memory distillation,
`groom_split_llm`, SkillOpt auto-promote, chat auto-respond, …) are gathered
into a highlighted section at the top so you can see at a glance what is
enabled on this install. Below the knobs, a per-agent table shows each
registered or configured agent's runtime, model, capabilities, autonomy
policy, memory enrollment, chat auto-respond flag, and background-job cap. A
filter box narrows by key, section, or value, and every knob carries a
click-to-copy `key = value` snippet for pasting into `config.toml`, whose path
and last-modified time are shown in the footer.

Values come from a strict server-side allowlist: only known, registered
settings are ever serialized, so secrets and tokens can never appear. Keys
present in `config.toml` that the allowlist does not recognize are listed by
name only, without values. The page never edits anything — configuration
changes happen in `config.toml` on the box.
