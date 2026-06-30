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
  --repo jerryfane/gitmoot --interval 24h --jitter 15m \
  --prompt "Review open issues, PRs, and recent jobs. Return a concise status report." \
  --enabled

# A review heartbeat requires the agent to hold the review capability.
gitmoot agent heartbeat add reviewer stale-prs \
  --repo jerryfane/gitmoot --interval 12h --action review \
  --prompt "Review stale open PRs and summarize blockers."
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
| `action`         | no       | `ask`   | `ask` (read-only analysis) or `review` (read-only PR/code review). `review` needs the `review` capability. `implement` is not supported. |
| `prompt`         | yes      | —       | Instructions passed to the agent.                            |
| `max_concurrent` | no       | `1`     | Overlap cap; a new run is skipped while this many are active.|

## Observability

`gitmoot daemon status` lists every configured heartbeat with its enabled state,
action, interval, and repo, plus its `last_run`, `next_due`, and `last_status`
once it has fired. With no heartbeats configured the section is omitted.

## Behavior and safety

- The daemon checks schedules during its existing poll loop (both the
  registered-repo and single-repo daemons).
- **No duplicate jobs:** a new run is skipped while a prior heartbeat job is still
  active (`max_concurrent`), and the persisted `next_due` means a daemon restart
  does not re-fire an active heartbeat.
- **Capacity-aware:** skipped this tick when the agent is at its `max_background`.
- **Missed ticks coalesce:** after an outage the schedule replays only once.
- **Read-only by design:** both actions (`ask`, `review`) are read-only; heartbeats
  never auto-implement code.
- **Managed repos only:** a heartbeat pointing at an unmanaged/disabled repo is
  skipped (`last_status = repo_unmanaged`) and self-recovers once the repo becomes
  managed. A `review` heartbeat for an agent lacking the review capability is
  skipped (`last_status = capability_missing`) until the capability is granted.

See the in-repo reference at `docs/heartbeats.md` for the full field reference.
