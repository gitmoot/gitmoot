# Agent heartbeat schedules

Heartbeats let the gitmoot daemon schedule **recurring agent work** itself —
cron-like background jobs — without relying on an external cron. They reuse the
normal job queue and background-agent path (no separate runner): a due heartbeat
enqueues an ordinary job that the existing worker tick runs.

Heartbeats are **off by default**. With no heartbeat sections in your config, the
daemon's scan returns immediately and nothing changes.

## Configuration

A heartbeat is one `[agents.<agent>.heartbeats.<name>]` section per schedule,
scoped to a named agent. Manage these with the CLI (below) rather than hand-editing
TOML — the CLI edits the section through the lossless config writer, so it never
clobbers your agent-type blocks or sibling heartbeats. The resulting section looks
like:

```toml
[agents.repo-maintainer.heartbeats.daily-status]
enabled = true
repo = "jerryfane/gitmoot"
interval = "24h"
jitter = "15m"
action = "ask"
prompt = "Review open issues, PRs, and recent jobs. Return a concise status report with blockers and suggested next actions."
max_concurrent = 1
```

| Field            | Required | Default | Notes                                                        |
| ---------------- | -------- | ------- | ------------------------------------------------------------ |
| `enabled`        | no       | `false` | A disabled heartbeat never runs.                             |
| `repo`           | yes      | —       | `owner/name` the job runs against. Must be a registered, enabled daemon repo with a checkout (see note below). |
| `interval`       | yes      | —       | Go duration (e.g. `24h`, `1h30m`). Validated at load.        |
| `jitter`         | no       | `0s`    | Random `[0, jitter]` added to each `next_due` to de-thunder. |
| `action`         | no       | `ask`   | `ask` (read-only analysis) or `review` (read-only PR/code review). A `review` heartbeat requires the agent to hold the `review` capability. `implement` is deliberately not supported. |
| `prompt`         | yes      | —       | Instructions passed to the agent.                            |
| `max_concurrent` | no       | `1`     | Overlap cap; a new run is skipped while this many are active.|

Invalid intervals/jitter or an unsupported action produce a clear validation
error at config load.

## CLI

Create and manage heartbeats programmatically (no hand-edited TOML):

```sh
# Create (or update) a heartbeat. Omit --enabled to add it disabled.
gitmoot agent heartbeat add repo-maintainer daily-status \
  --repo jerryfane/gitmoot --interval 24h --jitter 15m \
  --prompt "Review open issues, PRs, and recent jobs." --enabled

# A review heartbeat requires the agent to hold the review capability.
gitmoot agent heartbeat add reviewer stale-prs \
  --repo jerryfane/gitmoot --interval 12h --action review \
  --prompt "Review stale open PRs and summarize blockers."

gitmoot agent heartbeat list [--agent repo-maintainer]
gitmoot agent heartbeat show repo-maintainer daily-status
gitmoot agent heartbeat enable repo-maintainer daily-status
gitmoot agent heartbeat disable repo-maintainer daily-status
gitmoot agent heartbeat remove repo-maintainer daily-status
```

`add` validates the action, repo, interval, jitter, and prompt before writing, and
(for `action = review`) refuses to write a heartbeat for an agent lacking the
review capability. `enable`/`disable` flip just the `enabled` flag in place,
preserving the rest of the block and its comments.

## Observability

`gitmoot daemon status` surfaces every configured heartbeat with its enabled
state, action, interval, repo, and — once it has fired — its `last_run`,
`next_due`, and `last_status` (from the local `heartbeat_state`). With no
heartbeats configured the section is omitted entirely (status is unchanged).

## Behavior

- The daemon checks heartbeat schedules during its existing poll loop (both the
  registered-repo and single-repo daemons).
- When a heartbeat is due, gitmoot enqueues one normal background job. Heartbeat
  jobs are visible in the usual job/status surfaces (sender `heartbeat`,
  fingerprint `heartbeat:<agent>/<name>`).
- gitmoot records heartbeat state locally: last run time, next due time, last job
  id, and last status.
- **No duplicate jobs:** a new run is skipped while a prior heartbeat job is still
  active (the `max_concurrent` cap), and the persisted `next_due` means a daemon
  restart does not re-fire an active heartbeat.
- **Capacity-aware:** a heartbeat is skipped for this tick when the agent is
  already at its `max_background`; it retries once capacity frees up.
- **Missed ticks coalesce:** after a long outage the schedule replays only once
  (`next_due` is re-anchored to now), not a backlog of every missed interval.
- **Repo must be managed:** `repo` has to be a repo the daemon actually manages —
  registered (added to the daemon), enabled, and with a local checkout. A
  heartbeat pointing at an unmanaged/disabled repo is skipped each tick with
  `last_status = repo_unmanaged` (it does not enqueue a job no worker would claim),
  and starts running on its own once the repo becomes managed.

## Safety notes

- Both supported actions are **read-only**: `ask` (the conservative default) and
  `review`. Heartbeats do **not** auto-implement code — recurring unattended
  code-change PRs are intentionally out of scope.
- A `review` heartbeat only enqueues for an agent that holds the `review`
  capability; the check runs both when the heartbeat is written (CLI) and when it
  is due (daemon scan). A review heartbeat for an agent without the capability is
  skipped with `last_status = capability_missing` and self-recovers if the
  capability is later granted.
- `repo` must be a repo the daemon **manages** (registered, enabled, with a
  checkout). This keeps heartbeats from enqueuing jobs no worker would run.
- Heartbeats respect existing agent/runtime capacity limits (`max_background`),
  runtime locks, and repo policy — they enqueue through the same path as any other
  job.
- Keep `interval` sane: every due tick consumes a background slot and runtime
  budget.

## Not yet supported (deferred)

- `action = "implement"` heartbeats (recurring unattended code-change PRs).
- Agent-**type** scoping (only named agents today).
- Per-job runtime override (see #531).
