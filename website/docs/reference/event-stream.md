# Outbound Event Stream (`[events]`)

Gitmoot can push a small, versioned, redacted JSON event to one HTTP endpoint
whenever a job reaches a terminal state or pauses awaiting a human. This is an
**off-by-default, best-effort** outbound seam (#446): with no `[events]` config
nothing is constructed and behavior is byte-identical to a build without it.

It complements — it does not replace — the existing observability surfaces
(`gitmoot job watch`, the local dashboard, PR comments). The typed lifecycle
events recorded in SQLite (`job_events`) are unchanged; this is an additional
fan-out, not a migration.

## What it emits

The pilot emits a tight allowlist of event types over the webhook transport:

| `event_type`           | When                                                              |
| ---------------------- | ---------------------------------------------------------------- |
| `job.finished`         | a job reaches the `succeeded` terminal state                     |
| `job.failed`           | a job reaches the `failed` terminal state                        |
| `job.blocked`          | a job reaches the `blocked` terminal state                       |
| `job.needs_attention`  | a tree pauses awaiting a human (the `escalate_human` pause today) |

Each terminal transition emits **exactly once**. The engine owns the
succeeded/failed/blocked emit on its `Mailbox` terminal path; the daemon owns the
pre-flight (`queued -> failed|blocked`) and permission-blocked cases that never
pass through that path. `job.needs_attention` is emitted once per fresh
escalation round (a re-advance does not re-emit).

The following `event_type` values are **reserved** for the graduate step. They
are enumerated in the contract so a consumer can `switch` over them
forward-compatibly without a schema bump when they start arriving, but the pilot
does **not** emit them: `job.started`, `delegation.escalation`,
`delegation.finalized`, `orchestration.finished`.

## The event contract (`schema_version = 1`)

Every event is a single JSON object:

```json
{
  "schema_version": 1,
  "event_type": "job.finished",
  "job_id": "implement-task-7",
  "root_id": "coordinator-job-id",
  "repo": "owner/repo",
  "status": "succeeded",
  "ts": "2026-06-16T12:00:00Z",
  "detail": "Opened PR #42"
}
```

| Field            | Type   | Notes                                                                       |
| ---------------- | ------ | --------------------------------------------------------------------------- |
| `schema_version` | int    | Contract version. Currently `1`. A breaking field change bumps it.          |
| `event_type`     | string | The enum above.                                                             |
| `job_id`         | string | Opaque job id this event is about.                                          |
| `root_id`        | string | The coordination tree's root id, so a consumer can aggregate a run client-side. |
| `repo`           | string | `owner/repo` only — never an absolute checkout path.                        |
| `status`         | string | Terminal/lifecycle state (`succeeded`/`failed`/`blocked`/`awaiting_human`). |
| `ts`             | string | RFC3339 emit time.                                                          |
| `detail`         | string | Short redacted human-facing string (failure summary, the escalation question). |

There is **no** synthetic `orchestration.finished` in the pilot — every event
carries `root_id`, so a consumer groups a run client-side. Server-side
tree-convergence aggregation is a documented graduate item.

## Redaction

Every outbound string field is redacted at event construction through the same
redactor Gitmoot uses for off-box PR/issue comments and bug reports
(`workflow.RedactCommentText`): GitHub tokens, OpenAI keys, AWS secrets, and
`api_key`/`token`/`secret`/`password` assignments are replaced with
`[REDACTED]`. `repo` is reduced to `owner/repo` only. No absolute paths, raw
runtime output, or secrets leave the box.

## Best-effort delivery

Delivery is **fire-and-forget**. A slow, hung, erroring, or full consumer never
blocks or fails a job — this mirrors the `escalate_human` notifier contract:

- Each event is handed to a small buffered channel drained by **one** background
  goroutine, so concurrent emits from many workers serialize cleanly.
- Each POST is bounded by `[events].timeout` (default `2s`). A hung consumer
  times out and the event is dropped.
- On a full buffer, a transport error, or a non-2xx response the event is
  **dropped** and a single best-effort `event_sink_drop` job event is recorded
  locally (so a drop is observable without coupling delivery to the job).

There is **no** outbox / retry in the pilot: at-least-once delivery and an
outbox table are the explicit graduate step.

## Configuration

Add an `[events]` section to the Gitmoot config file:

```toml
[events]
# The single endpoint each event is POSTed to as application/json.
# Empty (the default) means the event stream is OFF: no sink, no goroutine,
# no emits, byte-identical behavior.
webhook_url = "https://example.com/gitmoot-events"

# Per-POST timeout (Go duration). Default 2s. Bounds a hung consumer.
timeout = "2s"

# RESERVED for the graduate Unix-socket transport. Parsed and validated but
# UNUSED by the pilot (webhook-only).
# socket_path = "/run/gitmoot/events.sock"
```

| Key           | Default | Meaning                                                          |
| ------------- | ------- | ---------------------------------------------------------------- |
| `webhook_url` | `""`    | HTTP(S) endpoint. Empty = OFF.                                   |
| `timeout`     | `2s`    | Per-POST timeout (Go duration). Must be positive.               |
| `socket_path` | `""`    | Reserved for the graduate Unix-socket transport (unused today). |

`webhook_url` must be an `http://` or `https://` URL. An invalid `timeout`
duration or a non-http(s) URL is rejected at config load.

## Graduate (go/no-go)

The pilot validates the contract and the best-effort guarantee with the least
scaffolding. The following are out of scope and gated behind an explicit go/no-go
on the tracking issue:

- The Unix-socket transport (`socket_path`), behind the same `Sink` seam.
- More event types (`job.started`, `delegation.*`, `orchestration.finished`).
- An outbox table for at-least-once / durable delivery.
- A server-side synthetic `orchestration.finished` (reliable tree convergence).
