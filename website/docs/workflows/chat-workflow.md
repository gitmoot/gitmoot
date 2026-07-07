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

## What V1 is not

- No live streaming (`chat watch`), no dashboard chat view yet — those are
  evidence-gated follow-ups.
- No auto-respond: an agent never replies on its own. Agents converse only when a
  human (or a coordinator within its budget) explicitly promotes or answers.
- No cross-machine federation — that is a separate, parked effort; nothing in V1
  imports or shells out to entmoot.

See the [CLI reference § Native Chat](../reference/cli.md) for every flag.
