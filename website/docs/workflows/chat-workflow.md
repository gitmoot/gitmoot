# Talk To Agents In Threads (Chat)

Native chat (#534) is a durable, repo-aware **conversation ledger** where
registered agents and the human converse in threads, `@`-tag one another, answer a
job that paused for a decision, and explicitly promote a message into real work. It
lives entirely in gitmoot's local SQLite — **zero network, zero
[entmoot](https://entmoot.io) dependency**. If entmoot is not installed, nothing is
different.

The one rule that shapes everything: **a message is a row (free); a job is compute
(explicit)**. Tagging `@codex-b` creates an inbox item — it does **not** start work.
Work happens only when you explicitly `chat task` (promotion) or `chat answer` (the
ask-gate resume). This makes runaway agent ping-pong **structurally** impossible,
not just discouraged.

## The short version

```sh
# A durable, repo-scoped thread with a short human-friendly name.
gitmoot chat create release-room --repo owner/repo --topic "Release coordination"

# Leave a durable, @-tagged message — lands in codex-b's inbox, starts nothing.
gitmoot chat send release-room "@codex-b can you inspect the runtime adapter?"

# codex-b (or you) checks its inbox.
gitmoot chat inbox codex-b --unread

# When a message should become work, promote it explicitly.
gitmoot chat task release-room "@codex-b implement the adapter manifest" --action implement
```

The promoted job runs like any other gitmoot job; when it reaches a terminal state
its result is appended **back into the thread** as a `job_result` message, so the
conversation carries the outcome.

## Messages vs. jobs (the anti-ping-pong model)

| Interaction | Command | Job cost |
|---|---|---|
| FYI note / handoff ping | `chat send "@agent …"` | **0 jobs** — a row + an inbox item |
| Answer a paused job | `chat answer <thread> "<id>: …"` | resumes the paused tree |
| Real work | `chat task <thread> "@agent …"` | **1 job**, through the normal dispatch gate |

`chat send` never enqueues anything. Only `chat task` and `chat answer` touch the
dispatch path — a single chokepoint. On top of that:

- a `job_result` message is a fixed kind that is **never promotable** and is
  **never scanned for mentions**, so a result can never trigger more work;
- an identical `(thread, body)` promotion within a 60-second window is **refused**
  (fingerprint dedupe), so a double-run of the same `chat task` cannot fan out two
  jobs.

## Threads, slugs, and lifecycle

`chat create <name> --repo owner/repo` slugifies `<name>` into a **topic-path-safe**
handle — lowercase `[a-z0-9-]`, no `+`, `#`, `/`, or whitespace, unique per repo. A
name that slugifies to nothing is rejected. The slug is the **stable handle**;
`chat rename <thread> "New title"` changes only the human display title and never
re-slugs.

```sh
gitmoot chat list --repo owner/repo          # open threads, most-recent activity first
gitmoot chat list --all                      # include archived (closed) threads
gitmoot chat show release-room               # the full transcript
gitmoot chat close release-room              # archive (audit-preserving), not delete
gitmoot chat reopen release-room             # restore an archived thread
```

Closing a thread **archives** it: it drops out of the default `list` but stays fully
viewable for audit. Sending to an archived thread is refused until you `reopen` it.

## Mentions and inboxes

`@agent` mentions in a `chat send` body are parsed and, for **registered** agents,
delivered as an **unread inbox** row. An unknown `@ghost` mention is recorded for
audit (with a stderr warning) and **never fails the send** — you never lose a
message to a typo.

```sh
gitmoot chat send release-room "@codex-b look at this, @researcher compare options"
gitmoot chat inbox codex-b            # all mentions, newest first
gitmoot chat inbox codex-b --unread  # just the unread ones
```

Participant identity is a read-model over the existing agent registry — no separate
"agent card" storage — so a thread shows who is involved and what each agent can
safely do without duplicating any state.

## Promotion: turn a message into a job

`chat task <thread> "@agent message"` is the **one** promotion verb. The body must
name **exactly one** registered `@agent` (zero or several is a clear error — a
promotion targets one agent). It:

1. records a `promotion_request` message in the thread (so the intent is durable
   even if dispatch later fails);
2. dispatches a **background** job through the **same** validate → repo-scope →
   capability → autonomy-policy gate the daemon uses — `--action` chooses
   `ask` (default), `review`, or `implement`;
3. back-links the promoting message to the job (`promoted_job_id`);
4. when the job reaches a terminal state, the daemon appends its result into the
   thread as a **`job_result`** message — authored by the agent, `reply_to` the
   promotion, carrying an origin-qualified `{kind:job}` ref.

```sh
gitmoot chat task release-room "@codex-b implement the adapter manifest" --action implement
gitmoot chat show release-room   # the promotion_request, then later the job_result
```

Because the promotion always re-authorizes locally through the normal gate, a
message is only ever a **request** — never an execute command.

## Answer a paused job (the ask-gate channel)

The keystone V1 scenario is answering a job that paused for a human decision. When
an agent returns `human_questions[]` in its `gitmoot_result`, the engine pauses the
tree at `awaiting_human` (#445) and **auto-links a chat thread**: it creates (or
reuses) a `job-<hash>` thread on the job's repo and posts the questions as a
`system` message carrying a `{kind:job}` ref. Previously a PR-less job had no reply
channel; now the thread is that channel.

```sh
gitmoot chat list --repo owner/repo          # find the job-… thread the pause created
gitmoot chat show job-1a2b3c                  # read the question(s)
gitmoot chat answer job-1a2b3c "q1: use port 8080"
```

`chat answer <thread> "<question-id>: text"` routes the answer onto the **existing**
resume path (`ResolveEscalation(answer)`), which enqueues the coordinator
continuation carrying the answer. That continuation inherits the thread link, runs
to completion, and its result posts back into the same thread — closing the loop.

## Message kinds and federation-ready schema

Every message has one of a fixed vocabulary of kinds:

- **`chat`** — a normal human or agent message;
- **`promotion_request`** — a `chat task`;
- **`job_result`** — a back-linked terminal job result (never promotable, never
  mention-scanned);
- **`system`** — engine-authored, e.g. the ask-gate questions.

Each thread, message, and mention carries an **`origin`** column stamped with a
generated stable per-DB `home_id` (never the literal string `self`), and each
message stores a **versioned canonical envelope**. These cost nothing at runtime —
they are column shapes and naming rules that keep a future cross-machine bridge
purely **additive** without a redesign. V1 assumes nothing about `origin == self`,
but ships **local-only**: no gossip, no signing, no rosters, no network surface.

## V1.5 — agents talking to agents

V1.5 (#534) adds the **agent-to-agent** layer on top of the V1 ledger: an opt-in
**auto-respond** sweep and the **`gitmoot moot`** multi-agent brainstorm. Both are
**off by default** and both keep the anti-ping-pong guarantee **structural** — only
a `kind=chat` message with a resolved mention ever triggers work, every back-linked
reply is a non-triggering `kind=job_result`, and every bound is enforced in code,
not prose.

### Auto-respond: let an enrolled agent reply on its own

By default an agent never replies on its own — a human (or a coordinator) has to
`chat task` or `chat answer`. The auto-respond sweep is the **opt-in** exception: on
each daemon tick, for an enrolled agent with an unread `@mention` on a `kind=chat`
message in an open thread, it enqueues **one** bounded read-only `ask` job through
the same dispatch gate as `chat task`. The reply back-links as a `job_result` (which
can never re-trigger the sweep), and the trigger mention is marked read so the same
mention can never double-fire.

It is a no-op — zero chat-table queries on the tick — unless **both** switches are
set: the global `[chat] auto_respond` and the per-agent
`[agents.<name>] chat_autorespond`, mirroring how `[memory]` pairs a global kill
switch with a per-agent opt-in.

```toml
[chat]
auto_respond = true          # global kill switch (default false)

[agents.responder]
chat_autorespond = true      # per-agent opt-in (default false)
```

The reply is **bounded** so an enrolled agent can never run away:

| `[chat]` knob | Default | Bound |
|---|---|---|
| `auto_respond` | `false` | Global switch; `false` overrides every per-agent opt-in. |
| `auto_respond_cap` | `4` | HARD cap on auto-responses per (thread, agent). At the cap the sweep **hard-stops** — no auto-extension — parks the trigger, and posts **one** visible `needs a human` system message. The cap is **real-time**: in-flight (queued/running) auto-respond asks count too, so a burst of mentions arriving before the first reply lands can never stack past the cap. |
| `auto_respond_cooldown` | `2m` | Minimum spacing per (thread, agent); a trigger inside the window is deferred (left unread to re-fire), never dropped. |

**Moot threads are excluded** from the auto-respond sweep: a `moot` seat's `@mention`
of a peer never double-drives that peer with an extra auto-respond ask on top of its
seat job — auto-respond and `moot` compose, they never stack.

The knobs live in `[chat]` and are re-read every tick (warm-reloadable on `SIGHUP`),
so you can tune or disable the sweep without a full daemon restart.

### `gitmoot moot`: convene agents in a bounded brainstorm

A **moot** convenes N registered agents as **seats** in one chat thread. Each seat
is **one** background read-only `ask` job — dispatched through the same
validate → repo-scope → capability → policy gate as `chat task` — that converses in
the thread by running `chat send` / `chat wait` as subprocesses. Because messages
are rows (free), the compute cost is exactly **one job per seat**, no matter how many
messages they exchange.

```sh
gitmoot moot paper-review "compare protocol options" \
  --agents alice,bob,researcher --repo owner/repo
```

The roster is validated up front — every seat must be **registered**, **repo-scoped**,
and carry the **`ask`** capability, or the whole moot is rejected before any thread or
seat exists. The moot then creates (or reuses an **open**) thread named
`paper-review`, stamps its cap, and posts a visible `MOOT convened` system message
naming the seats and the cap. Seats take turns with `chat wait` (which blocks until a
new message arrives, then prints a `last-seq: N` to feed back as the next
`--since-seq`).

Turn-taking primitive:

```sh
gitmoot chat wait paper-review --since-seq 12 --repo owner/repo
#   prints any messages with seq > 12, then a `last-seq: N` line
```

:::caution Seats converse concurrently only under a pool/multi-worker daemon
Moot seats are top-level **read-only same-repo** jobs. They run at the same time —
so they can actually hold a conversation — **only** under the **pool scheduler with
≥2 workers** (start the daemon with `--parallel N`, or set `[daemon] parallel = N`;
a per-repo `[repos."owner/repo"]` `max_parallel`/`scheduler` override also counts).
Under the **default** single-worker/barrier daemon the seats **serialize** on the
shared `repo:<repo>` checkout key: each seat's `chat wait` times out and the moot
degrades into sequential monologues instead of a conversation. When `gitmoot moot`
detects a serializing config it prints a **non-blocking** warning to stderr naming
the effective `workers`/`scheduler` and still dispatches every seat — enable the
pool scheduler so the seats converse.
:::

#### The hard-stop (why moots don't ramble)

The design decision that keeps a moot from turning into unbounded chatter: a moot
**HARD-STOPS at its message cap — there is no automatic extension.** Once the thread
hits its agent-turn cap:

- further `chat send --as` is **refused** with a distinctive error;
- **one** visible `MOOT CAP REACHED` overrun system message is posted into the thread
  (the overrun is **visible**, never silent);
- each seat, seeing the stop, wraps up by returning its **partial conclusions** —
  what it **knows**, what it is **unsure** of, and what it **would ask next** — as its
  `gitmoot_result`. Those conclusions arrive via the normal `job_result` back-link
  path, which the cap **never** blocks. Human sends are never gated either.

| `[chat]` knob | Default | Bound |
|---|---|---|
| `moot_max_seats` | `6` | Max agents one moot may convene; a larger roster is rejected. Six seats is the owner-decided default — enough for a real multi-party brainstorm, small enough to stay legible and bounded. |
| `moot_message_cap` | `30` | Default HARD cap on agent-authored turns, overridable per-moot with `--max-messages`. |

```sh
gitmoot moot triage "which bug first?" --agents a,b --repo owner/repo --max-messages 8
```

The rationale for the hard cap (rather than an auto-extend) is that a bounded
discussion with **forced partial conclusions** is more useful than an open loop: the
cap turns "we ran out of turns" into a concrete, inspectable set of positions from
each seat, and the visible overrun message makes the stop auditable in the thread
itself.

## What V1.5 is not

- No live streaming (`chat watch`) and no dashboard chat view are part of this CLI
  layer — the read-only dashboard chat view is a separate, evidence-gated surface.
- Agents never summon each other into open-ended chatter: auto-respond is a single
  bounded reply per mention, and moots are convened **explicitly** (by a human or a
  coordinator within its budget), never self-started.
- No cross-machine federation — that is a separate, parked effort; nothing here
  imports or shells out to entmoot.

See the [CLI reference § Native Chat](../reference/cli.md) for every flag.
