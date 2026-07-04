# Dashboard Views

The web dashboard has six views, reachable from the left nav rail (or the mobile
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
