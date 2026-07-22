# Outbound Event Stream (`[events]`)

Gitmoot can push a small, versioned, redacted JSON event to one HTTP endpoint
whenever a job reaches a terminal state or pauses awaiting a human. This is an
**off-by-default, best-effort** outbound seam (#446): with no `[events]` config
and no organization event rules, nothing is constructed and behavior is
byte-identical to a build without it.

It complements — it does not replace — the existing observability surfaces
(`gitmoot job watch`, the local dashboard, PR comments). The typed lifecycle
events recorded in SQLite (`job_events`) are unchanged; this is an additional
fan-out, not a migration.

## What it emits

The pilot emits a tight allowlist of event types over the webhook transport:

| `event_type`                    | When                                                                              |
| ------------------------------- | --------------------------------------------------------------------------------- |
| `job.finished`                  | a job reaches the `succeeded` terminal state                                       |
| `job.failed`                    | a job reaches the `failed` terminal state                                          |
| `job.blocked`                   | a job reaches the `blocked` terminal state                                         |
| `job.needs_attention`           | a tree pauses awaiting a human (the `escalate_human` pause today)                  |
| `job.deferred`                  | the daemon re-queued a run whose delivery failed on a retryable operational blocker (runtime auth, rate limit/quota, network/GitHub outage, checkout contention) — it will be re-dispatched automatically. Since #532 slice E this is a **first-class** transition emitted INSTEAD of `job.failed` (no preceding `job.failed` for that run) |
| `candidate.awaiting_promotion`  | a SkillOpt template candidate becomes `pending` after import (always, off the auto-promote policy) |
| `candidate.auto_promoted`       | the off-by-default `[skillopt].auto_promote` policy auto-promoted a candidate to `current` (also the canary GRADUATE event) |
| `candidate.canary_started`      | the off-by-default `[skillopt].auto_promote_canary` policy promoted a candidate to the `canary` state behind the live champion |
| `candidate.rolled_back`         | the canary regression window auto-rolled-back a canary on a material regression (champion stays current, canary rejected) |

Each terminal transition emits **exactly once**. The engine owns the
succeeded/failed/blocked emit on its `Mailbox` terminal path; the daemon owns the
pre-flight (`queued -> failed|blocked`) and permission-blocked cases that never
pass through that path. `job.needs_attention` is emitted once per fresh
escalation round (a re-advance does not re-emit).

**`job.deferred` is a first-class transition — a deferred run does NOT emit
`job.failed` first.** When a run's delivery fails on a classified operational
blocker (runtime auth rejected, provider rate limit/quota, network/GitHub outage,
or a self-healing checkout contention), the classification now happens
**pre-terminally** in the mailbox seam (#532 slice E): the run is re-queued
directly and the daemon emits **only** `job.deferred` for that `job_id` (detail
carries the blocker class, the `attempt N/M` retry budget, and the earliest
`retry at` timestamp), then silently re-dispatches after the hold. Product
failures (the agent answered with a `gitmoot_result`, including
`decision=failed`) still emit `job.failed` and are never auto-retried.

Migration note: before slice E the daemon emitted `job.failed` **then**
`job.deferred` for the same run (the accepted first-slice flap); consumers had to
suppress terminal handling for a `job.failed` followed by a `job.deferred`. That
flap is gone. A `job.failed` with no following `job.deferred` was already the only
"final" signal, so the safe consumer rule is unchanged and forward-compatible:
**treat `job.failed` as final only when it is not immediately followed by a
`job.deferred` for the same `job_id`.**

The two `candidate.*` events come from the `skillopt import` / `train continue`
CLI path, NOT the daemon (#471). When a candidate becomes `pending`,
`candidate.awaiting_promotion` is emitted **once** carrying the version id
(`job_id`), the template id (`root_id`), and a redacted score/samples/CI reason —
independent of the auto-promote policy, so a human is notified even in the manual
default. If `[skillopt].auto_promote` is on **and** every configured guardrail
holds, the candidate is promoted via the existing promote path and
`candidate.auto_promoted` fires so the change can be reviewed or rolled back. The
adjacent dashboard **Attention** page also lists every pending candidate
(read-only), so candidates are visible locally even with `[events]` off.

When `[skillopt].auto_promote_canary` is on **and** `auto_promote_canary_sample`
is a fraction in `(0,1]`, a guardrails-pass candidate is promoted to a **canary**
instead of straight to `current`: it routes that sampled fraction of new job
resolutions while the prior champion stays the live current version, and
`candidate.canary_started` fires (carrying the canary version id, template id, and
the sample fraction). The daemon then watches a bounded regression window over the
canary's harvested verifiable outcomes (reusing the #465 Mode A signal, no new
evaluator) and compares it to the prior champion: on parity-or-better it
**graduates** the canary to `current` and emits `candidate.auto_promoted`; on a
**material regression** it auto-rolls-back — the champion stays current, the canary
is rejected, and `candidate.rolled_back` fires. It is **fail-safe**: too few canary
outcomes, no champion baseline, or feedback it could not read all **hold** (keep
sampling), never rolling back on unread evidence and never graduating without
confirming non-regression. With the knob off (the default) promotion is the
unchanged direct path and no canary event is ever emitted.

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
  "event_type": "job.needs_attention",
  "job_id": "implement-task-7",
  "root_id": "coordinator-job-id",
  "repo": "owner/repo",
  "status": "awaiting_human",
  "ts": "2026-06-16T12:00:00Z",
  "detail": "Choose the rollout window",
  "cause": "escalation"
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
| `cause`          | string | Optional internal discriminator (`escalation`, `ask_gate`, `merge_guard`, or `permission_guard`). |

`cause` is an additive optional field, so `schema_version` remains `1` and
existing events serialize unchanged. It is a trusted enum assigned at emit
sites, not free text, and therefore is not passed through path scrubbing or
redaction.

There is **no** synthetic `orchestration.finished` in the pilot — every event
carries `root_id`, so a consumer groups a run client-side. Server-side
tree-convergence aggregation is a documented graduate item.

## Redaction

Every outbound free-text string field is redacted at event construction through the same
redactor Gitmoot uses for off-box PR/issue comments and bug reports
(`workflow.RedactCommentText`): GitHub tokens, OpenAI keys, AWS secrets, and
`api_key`/`token`/`secret`/`password` assignments are replaced with
`[REDACTED]`. The `detail` field is then additionally scrubbed of absolute
filesystem paths — host home layout, checkout and worktree paths embedded in
pre-flight failure detail (e.g. `git worktree add /root/.gitmoot/...`) collapse
to `<path>` — so host layout and usernames never leak. `repo` is reduced to
`owner/repo` only. No absolute paths, raw runtime output, or secrets leave the
box.

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

## Organization event-rule wakes

The daemon can also use the same event seam to wake a Herdr pane bound to an
organization role. This is independently off by default: zero `event_rules`
rows means no evaluator and no Herdr call, whether or not the webhook is on.

```toml
[org.roles."maintainer"]
parent = "owner"
scope = ["owner/*"]
pane = "w1:p2"
```

```sh
gitmoot org events rule add --on guard --match owner/repo --wake maintainer
gitmoot org events rule list
gitmoot org events rule rm <rule-id>
```

Kinds are `escalation`, `attention`, `guard`, `job-terminal`, and `blocked`.
The v1 `--match` filter is a case-insensitive substring tested against the event
repo and job id; empty matches all. A plain `job.blocked` event matches both
`job-terminal` and `blocked`, while guard-caused blocks match `guard` first.
The wake role's config sets `pane = "<pane-id-or-label>"`: a value containing `:`
is a `wX:pY` pane id used as-is, any other value is a pane label resolved to the
current id at wake time (so a recycled pane is still reached). Wake delivery runs
`herdr agent prompt <pane> <prompt> --wait --timeout 8000` and treats
`result.type = "agent_prompted"` — and a post-delivery `error.code = "timeout"` —
as delivered, `error.code = "agent_prompt_stalled"` as not delivered. Missing
bindings, unavailable Herdr, stalls, and transport errors are swallowed after
lightweight logging; they never block or fail a job.

## Configuration

Add an `[events]` section to the Gitmoot config file:

```toml
[events]
# The single endpoint each event is POSTed to as application/json.
# Empty (the default) means the webhook transport is OFF. Organization event
# rules, when explicitly added, can still consume the same local event seam.
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
