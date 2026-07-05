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
| `action`         | no       | `ask`   | `ask` (read-only analysis), `review` (read-only PR/code review), or `implement` (write). A `review` heartbeat requires the agent to hold the `review` capability. `implement` is **policy-gated** — see below. |
| `runtime`        | no       | —       | Optional per-heartbeat runtime override (`codex`/`claude`/`kimi`). When set, the scheduled job runs on that runtime instead of the agent's registered default, on a fresh session (reuses the per-job override from #531). Empty ⇒ agent default. |
| `prompt`         | yes      | —       | Instructions passed to the agent.                            |
| `max_concurrent` | no       | `1`     | Overlap cap; a new run is skipped while this many are active.|

Invalid intervals/jitter, an unsupported action, or an unsupported runtime
override produce a clear validation error at config load.

### Policy-gated `implement` action

An `implement` heartbeat enqueues a **write** job (it can change code and open
PRs), so it is deliberately gated and off by default. It only runs when the
target agent both:

- holds the `implement` capability, **and**
- carries a write-granting autonomy policy — `--policy workspace-write` or
  `--policy danger-full-access`.

This mirrors `agent start` / `agent implement` exactly: under the default `auto`
policy (or `read-only`) a headless implement job would run and produce no files,
so gitmoot **fails closed**. The gate is enforced twice: `agent heartbeat add`
refuses to write an `implement` heartbeat for an agent without a write policy or
the implement capability, and the daemon scan skips a due `implement` heartbeat
(advancing `next_due` with `last_status = policy_readonly`) if the agent no longer
qualifies — it self-recovers once a write policy is granted. Implement heartbeats
respect branch locks and the merge gate like any other implement job.

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

# An implement heartbeat requires the agent to hold the implement capability AND
# a write-granting policy (workspace-write / danger-full-access). Add --runtime to
# pin a specific runtime for this schedule.
gitmoot agent heartbeat add builder nightly-tidy \
  --repo jerryfane/gitmoot --interval 24h --action implement --runtime codex \
  --prompt "Fix the top lint/type error and open a small PR."

gitmoot agent heartbeat list [--agent repo-maintainer]
gitmoot agent heartbeat show repo-maintainer daily-status
gitmoot agent heartbeat enable repo-maintainer daily-status
gitmoot agent heartbeat disable repo-maintainer daily-status
gitmoot agent heartbeat remove repo-maintainer daily-status
```

`add` validates the action, runtime, repo, interval, jitter, and prompt before
writing; refuses (for `action = review`) a heartbeat for an agent lacking the
review capability; and refuses (for `action = implement`) a heartbeat for an agent
lacking the implement capability or a write-granting policy. `enable`/`disable`
flip just the `enabled` flag in place, preserving the rest of the block and its
comments.

## Observability

`gitmoot daemon status` surfaces every configured heartbeat with its enabled
state, action, interval, repo, its `runtime` override (only when one is set), and
— once it has fired — its `last_run`, `next_due`, and `last_status` (from the
local `heartbeat_state`). With no heartbeats configured the section is omitted
entirely (status is unchanged).

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

- The default action `ask` and `review` are **read-only**. `implement` is a
  **write** action and is off by default; it is policy-gated (see above) so a
  recurring unattended code-change PR can only run for an agent an operator has
  deliberately given a write-granting policy.
- A `review` heartbeat only enqueues for an agent that holds the `review`
  capability; the check runs both when the heartbeat is written (CLI) and when it
  is due (daemon scan). A review heartbeat for an agent without the capability is
  skipped with `last_status = capability_missing` and self-recovers if the
  capability is later granted.
- An `implement` heartbeat only enqueues for an agent that holds the `implement`
  capability AND a write-granting policy; the same two-place check (CLI write +
  daemon scan) applies. A due implement heartbeat that no longer qualifies is
  skipped with `last_status = policy_readonly` and self-recovers once a write
  policy is granted.
- A per-heartbeat `runtime` override runs the scheduled job on a **fresh** session
  of the named runtime; it never resumes or writes the agent's default-runtime
  session, so it cannot collide with the agent's own runtime lock.
- `repo` must be a repo the daemon **manages** (registered, enabled, with a
  checkout). This keeps heartbeats from enqueuing jobs no worker would run.
- Heartbeats respect existing agent/runtime capacity limits (`max_background`),
  runtime locks, and repo policy — they enqueue through the same path as any other
  job.
- Keep `interval` sane: every due tick consumes a background slot and runtime
  budget.

## Not yet supported (deferred)

- Agent-**type** scoping (only named agents today).
