# Schedule Recurring Agent Work (Heartbeats)

Heartbeats let the gitmoot daemon schedule **recurring agent work** itself —
cron-like background jobs — without an external cron. A due heartbeat enqueues an
ordinary job that the existing worker tick runs (no separate runner), so heartbeat
jobs show up in the usual job/status surfaces.

Heartbeats are **off by default**: with no heartbeat sections configured, the
daemon's scan returns immediately and behavior is unchanged.

## The short version

```sh
# Add a daily read-only status heartbeat for a named agent (disabled until --enabled).
gitmoot agent heartbeat add repo-maintainer daily-status \
  --repo gitmoot/gitmoot --interval 24h --jitter 15m \
  --prompt "Review open issues, PRs, and recent jobs. Return a concise status report." \
  --enabled

# A review heartbeat requires the agent to hold the review capability.
gitmoot agent heartbeat add reviewer stale-prs \
  --repo gitmoot/gitmoot --interval 12h --action review \
  --prompt "Review stale open PRs and summarize blockers."

# Add --runtime to pin a specific runtime for this schedule.
gitmoot agent heartbeat add reviewer nightly-stale \
  --repo gitmoot/gitmoot --interval 24h --action review --runtime codex \
  --prompt "Review the oldest open PR and summarize what blocks it."
```

The CLI edits the `[agents.<agent>.heartbeats.<name>]` config section through the
lossless config writer, so it never clobbers your agent-type blocks or sibling
heartbeats — do not hand-edit the TOML.

## Manage heartbeats

```sh
gitmoot agent heartbeat list [--agent <agent>]
gitmoot agent heartbeat show <agent> <name>
gitmoot agent heartbeat enable <agent> <name>
gitmoot agent heartbeat disable <agent> <name>
gitmoot agent heartbeat remove <agent> <name>
```

`enable`/`disable` flip just the `enabled` flag in place, preserving the rest of
the block and its comments.

## Configuration fields

| Field            | Required | Default | Notes                                                        |
| ---------------- | -------- | ------- | ------------------------------------------------------------ |
| `enabled`        | no       | `false` | A disabled heartbeat never runs.                             |
| `repo`           | yes      | —       | `owner/name` the job runs against. Must be a registered, enabled daemon repo with a checkout. |
| `interval`       | yes      | —       | Go duration (e.g. `24h`, `1h30m`). Validated at load.        |
| `jitter`         | no       | `0s`    | Random `[0, jitter]` added to each `next_due` to de-thunder. |
| `action`         | no       | `ask`   | `ask` (read-only analysis) or `review` (read-only PR/code review; needs the `review` capability). `implement` is **refused** — see below. |
| `runtime`        | no       | —       | Optional per-heartbeat runtime override (`codex`/`claude`/`kimi`/`omp` today — the set is derived from the adapter registry minus `shell`, so a newly registered runtime joins it); runs the scheduled job on that runtime (fresh session) instead of the agent default. Empty ⇒ agent default. |
| `prompt`         | yes      | —       | Instructions passed to the agent.                            |
| `max_concurrent` | no       | `1`     | Overlap cap; a new run is skipped while this many are active.|

### The `implement` action is refused

`implement` is refused at config validation (#2203). Gitmoot no longer
dispatches implementation, and the heartbeat's writable-worktree allocator and
its enqueue went with that removal — precisely what would have to come back for
an `implement` schedule to mean anything. Without them a due `implement`
heartbeat could only have produced a branchless job with no checkout to
resolve, which is worse than refusing it.

Schedule read-only work and let the seat record its own implementation with
`gitmoot job record --type implement`. That is what the traffic already was:
568 of the last 570 implement rows are seats recording their own sessions, and
no heartbeat has ever been configured with the `implement` action.

## Observability

`gitmoot daemon status` lists every configured heartbeat with its enabled state,
action, interval, repo, and `runtime` override (only when set), plus its
`last_run`, `next_due`, and `last_status` once it has fired. With no heartbeats
configured the section is omitted.

## Behavior and safety

- The daemon checks schedules during its existing poll loop (both the
  registered-repo and single-repo daemons).
- **No duplicate jobs:** a new run is skipped while a prior heartbeat job is still
  active (`max_concurrent`), and the persisted `next_due` means a daemon restart
  does not re-fire an active heartbeat.
- **Capacity-aware:** skipped this tick when the agent is at its `max_background`.
- **Missed ticks coalesce:** after an outage the schedule replays only once.
- **Read-only only:** `ask` and `review` are the available actions, and both are
  read-only. The write action `implement` is refused at config validation
  (#2203), so a heartbeat cannot schedule a recurring unattended code change.
- **Managed repos only:** a heartbeat pointing at an unmanaged/disabled repo is
  skipped (`last_status = repo_unmanaged`) and self-recovers once the repo becomes
  managed. A `review` heartbeat for an agent lacking the review capability is
  skipped (`last_status = capability_missing`), self-recovering once the
  capability is granted.

See the in-repo reference at `docs/heartbeats.md` for the full field reference.
