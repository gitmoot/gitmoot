# Pipelines

Pipelines (#681) let the gitmoot daemon run a **declared DAG of shell and agent
stages** — a small, durable multi-step flow — on demand or on an interval schedule.
Each stage is an ordinary queued job: shell commands use the shell runtime, while
agent stages use their registered runtime. The normal worker tick claims and runs
it, and a scan-based **advancer** folds each stage's
`gitmoot_result` decision and enqueues the stages whose dependencies have all
succeeded. Pipelines reuse the same job queue, result contract, and scheduling
idiom as heartbeats — there is no separate runner.

Pipelines are **off by default**: with no pipelines defined, the daemon's
pipeline scan returns an empty list before touching any state, and behavior is
unchanged.

## Code map

The pipeline engine lives in `internal/pipeline`. Start with `run.go` for run
creation, scheduling, stage enqueue requests, advancement, settlement, worktree
context, and job-event timestamps. Service admission, proof finalization, and
artifact collection live beside it in `service*.go`; environment delivery and
produce-path safety live in `env_runtime.go` and `core.go`.

The engine uses three lower-level seams:

- `internal/workflow` owns mailbox enqueueing, job and runtime-session locks,
  result parsing, and read-only worktree cleanup.
- `internal/db` owns persisted pipeline, run, stage, service-run, job, and event
  state. Engine scans advance only from these stored snapshots.
- `internal/runtime` supplies runtime identities and delivery adapters; pipeline
  environment wrapping adds only the stage-scoped values already resolved by the
  engine.

`internal/cli` remains the command and presentation boundary. It parses
`gitmoot pipeline ...`, renders CLI/dashboard views, owns GitHub bundle transport,
and adapts the existing local implement-dispatch worktree allocator into the
engine's stage enqueuer. The engine package never imports `internal/cli`.

A pipeline is not an orchestra. Each stage is a **leaf**: it runs a shell command
and returns a decision. A stage that emits `delegations[]` does **not** spawn
children — the advancer ignores them and the engine strips them for a pipeline
stage job, so a pipeline can never fan out into a delegation tree. Reach for an
orchestra (`gitmoot orchestrate` / a coordinator that returns `delegations[]`)
when you want dynamic decomposition; reach for a pipeline when you have a fixed,
repeatable sequence of shell steps with explicit dependencies.

## The spec

A pipeline is declared as a YAML file and registered with `gitmoot pipeline add`.
The raw bytes are stored **verbatim** in the local store alongside a content hash;
each run snapshots the hash it was created from, so a run always executes the spec
it was created against — editing the file later (even whitespace) changes the hash
and does not affect an in-flight run.

```yaml
name: nightly-sync          # required, name-safe token (letters, digits, - _)
repo: owner/repo            # optional to register; REQUIRED to actually run
description: Syncs nightly data for deployment. # optional purpose shown in inspection views
env_file: /root/.config/nightly-sync/env # optional 0600 secret file
env:                       # optional inline NON-secret defaults
  OUTPUT_DIR: /srv/nightly-sync
schedule:                   # optional; interval schedule (no cron in v1)
  interval: 24h             #   required when a schedule block is present (positive Go duration)
  jitter: 15m               #   optional random [0, jitter] added to each next_due (>= 0)
trigger:                    # optional; generated Activepieces event source (requires repo:)
  kind: email               #   only email in this release
  connection: gmail-imap    #   optional; default gmail-imap
  mailbox: INBOX            #   optional; default INBOX
  map:                      #   optional run payload outputs from closed email selectors
    subject: subject
    sender: from_address
success_decisions:          # optional top-level default (see below)
  - approved
  - implemented
  - skipped
stages:                     # the DAG, keyed by unique id and wired by needs
  - id: source
    cmd: "curl -sf https://example.com/data > data.json"
    env_keys: [SOURCE_API_TOKEN]
  - id: score
    cmd: "python score.py data.json"
    isolate: true           # optional shell-only detached read-only worktree
    needs: [source]         # runs only after every listed stage SUCCEEDS
  - id: deploy
    cmd: "rclone copy out/ r2:bucket"
    needs: [score]
    timeout: 30m            # optional per-stage job timeout (positive Go duration)
    retry: 2                # optional; re-attempt a FAILED stage up to N times (>= 0)
    success_decisions:      # optional per-stage override of the pipeline default
      - approved
```

### Fields

| Field                       | Scope        | Required | Notes |
| --------------------------- | ------------ | -------- | ----- |
| `name`                      | pipeline     | yes      | Stable identifier and DB primary key; a name-safe token (letters, digits, `-`, `_`). |
| `repo`                      | pipeline     | no\*     | `owner/name` the stages run against. Optional to **register**, but **required to run** — stage jobs need a managed repo for the worker to claim them. |
| `description`               | pipeline     | no       | Optional purpose (up to 500 characters) shown by `pipeline show`, dashboard detail, and `pipeline list` (truncated there). |
| `env_file`                  | pipeline     | no       | Absolute operator-owned secret file. It must exist, be a regular file owned by the current uid with mode exactly `0600`, and live outside the Gitmoot home and every managed checkout. |
| `env`                       | pipeline     | no       | Inline **non-secret** `KEY: value` defaults. Pipeline-owned and granted shared values take precedence. Values are delivered only when a shell stage selects the key. |
| `schedule.interval`         | pipeline     | cond.    | Required when a `schedule:` block is present. A positive Go duration (`24h`, `1h30m`). |
| `schedule.jitter`           | pipeline     | no       | Random `[0, jitter]` added to each `next_due` to de-thunder (`>= 0`). |
| `trigger.kind`              | pipeline     | cond.    | Required with `trigger:`. Only `email` is supported; it generates an owned Activepieces IMAP flow. |
| `trigger.connection`        | pipeline     | no       | Activepieces connection external id; default `gmail-imap`. Must match `[A-Za-z0-9][A-Za-z0-9_-]*`. |
| `trigger.mailbox`           | pipeline     | no       | IMAP mailbox; default `INBOX`. |
| `trigger.map`               | pipeline     | no       | Output name to email selector. Output names must match `^[a-z][a-z0-9_]*$` and be at most 64 bytes; an explicit empty map is rejected. See [Trigger payloads](#trigger-payloads). |
| `success_decisions`         | pipeline     | no       | Decisions that mark a stage succeeded. Default `["approved","implemented","skipped"]`. Any value must be one of `approved`, `implemented`, `changes_requested`, `skipped` - `blocked`/`failed` are park states and are rejected. An explicit list is strict: omitting `skipped` requires real work and makes a skipped result fail. |
| `allow_scheduled_writes`    | pipeline     | no       | Safety flag. A **mutating** `implement` or `produce` stage on a **scheduled** pipeline is rejected unless this is `true`. Manual runs do not need it. Default `false`. |
| `allow_triggered_writes`    | pipeline     | no       | Safety flag. A **mutating** `implement` or `produce` stage on a pipeline with `trigger:` is rejected unless this is `true`. Default `false`. |
| `allow_auto_merge`          | pipeline     | no       | Pipeline-level half of the auto-merge double key. Required with a gate's `merge: auto`; default `false`. It does not replace `allow_scheduled_writes` on scheduled pipelines. |
| `stages[].id`               | stage        | yes      | Unique, name-safe stage id. Appears verbatim in the stage job's fingerprint and deterministic id. |
| `stages[].cmd`              | stage        | cond.    | Shell command run verbatim via `sh -c` (see the stage contract below). A stage is **exactly one** of `cmd`, `agent`, or `gate`. |
| `stages[].isolate`          | stage        | no       | Shell-only opt-in to a detached read-only committed-tip worktree, removing checkout-lock serialization so same-repo siblings can run concurrently — including siblings that share the identical `cmd`, which also take a job-scoped shell runtime-session key (#1034). Default `false` preserves the shared checkout; rejected on non-shell stages. See [Shell-stage isolation](#shell-stage-isolation). |
| `stages[].agent`            | stage        | cond.    | Name of a managed gitmoot agent to run this stage (instead of a shell `cmd`). Must be a name-safe token; `pipeline add` warns (does not block) if the agent does not exist yet, but it must exist before the stage runs. Mutually exclusive with `cmd`/`gate`. The stage kind depends on `action`/`write`/`orchestrate` — see [Agent stages](#agent-stages). |
| `stages[].prompt`           | stage        | cond.    | Instruction handed to an agent stage's agent. **Required** for an agent stage; rejected for a shell/gate stage. Prepended with the upstream `needs` stages' result summaries at enqueue. |
| `stages[].action`           | stage        | no       | `ask` (default), `review`, `implement`, or `produce`. Produce writes operator-owned data but never the repo or a PR. |
| `stages[].write`            | stage        | cond.    | Required acknowledgement for both mutating actions: `implement` and `produce`; rejected elsewhere. |
| `stages[].writes`           | stage        | cond.    | Required non-empty list for `produce`. Paths must be absolute and cleaned. At add time symlinks are resolved and overlap with `/`, the Gitmoot home, or any managed checkout is rejected in either direction. Rejected elsewhere. |
| `stages[].reads`            | stage        | no       | Produce-only read-only input directories. Paths must be absolute and cleaned; `/`, Gitmoot state, the configured keychain, the pipeline `env_file`, and read roots that contain a declared write root are rejected after symlink resolution. |
| `stages[].network`          | stage        | no       | Produce-only opt-in to Codex workspace-write network access. Default `false`. |
| `stages[].check`            | stage        | no       | Produce-only trusted operator command run with `sh -c` after a valid result, in the stage cwd with the daemon environment. |
| `stages[].check_retries`    | stage        | no       | Produce-only count of same-session correction turns after a non-zero `check` (`>= 0`, default `0`). |
| `stages[].orchestrate`      | stage        | no       | `true` makes an agent stage a **sub-tree coordinator** (#758): its `delegations[]` fan out as owned children and the stage waits for the whole tree, then folds the synthesis. Agent stage only; `action` must be `ask`. |
| `stages[].gate`             | stage        | cond.    | Makes this a **jobless gate stage** (#768): it runs no worker and folds when an external predicate holds. Only `pr_merged` today. Exclusive with `cmd`/`agent`. Requires `source`. |
| `stages[].merge`            | stage        | no       | Gate-only merge authority. Omit for the unchanged human-merge default; `auto` lets the pipeline advancer squash-merge after all safety conditions hold. |
| `stages[].source`           | stage        | cond.    | The upstream `implement` stage whose PR to bind. Required on a `gate`; optional on `action: review`; rejected on shell/ask/implement/orchestrate stages. It must be one of the stage's own `needs`. |
| `stages[].needs`            | stage        | no       | Ids of sibling stages that must **succeed** before this stage is enqueued. Must reference known stages, never the stage itself, and form no cycle. |
| `stages[].timeout`          | stage        | no       | Per-stage job timeout (positive Go duration). |
| `stages[].retry`            | stage        | no       | How many times a **failed** stage may be re-attempted (`>= 0`, default `0`). |
| `stages[].env_keys`         | stage        | no       | Exact names or globs (for example `REDDIT_*`). Shell stages resolve pipeline-owned, pipeline-granted, and inline sources. Agent stages resolve only configured proxied keys granted to their named seat. Gates reject this field; no entry means no key access. |
| `stages[].success_decisions`| stage        | no       | Per-stage override of the pipeline default. |

`gitmoot pipeline add` validates the whole spec **at add time** — unknown keys, a
non-name-safe name/id, a duplicate stage id, a stage that is not **exactly one** of
`cmd`, `agent`, or `gate` (and, per kind: an agent stage's missing `prompt`, an
invalid `action`, `implement` without `write: true` or `write: true` off an
implement stage, `isolate: true` on a non-shell stage, a mutating stage on a
scheduled pipeline without
`allow_scheduled_writes`, a mutating stage on a triggered pipeline without
`allow_triggered_writes`, a gate/review `source` that is not an upstream implement
stage in `needs`, or `source` on another stage kind), an unknown/self/cyclic `needs`, an invalid
timeout/interval/jitter, a negative retry, or a `success_decisions` value outside the
allowed set — so a structural mistake surfaces as a clear error at registration
rather than a stuck run later.

### Stage-scoped environment

Pipeline secrets are deny-by-default. A stage receives only the concrete names
selected by its own `env_keys`; a sibling with different or empty `env_keys`
does not receive them. Shell grants belong to the pipeline. Agent stages require
two independent locks: a configured proxied key granted to the registered agent
seat, plus that stage's explicit selector. Injected agent grants are refused,
and gate stages cannot declare `env_keys`. Shared keys must be registered and
granted separately:

```sh
gitmoot key path # edit this 0600 file; the CLI never accepts values
gitmoot key add REDDIT_CLIENT_SECRET --mode injected
gitmoot key grant REDDIT_CLIENT_SECRET --pipeline trend-scout
gitmoot key add PARTNER_API_TOKEN --mode proxied
gitmoot key configure PARTNER_API_TOKEN --upstream https://api.partner.example/v1 --auth bearer
gitmoot key grant PARTNER_API_TOKEN --pipeline trend-scout
gitmoot key grant PARTNER_API_TOKEN --agent researcher
```

```yaml
env_file: /root/.config/trend-scout/env
env:
  TREND_SCOUT_DATA: /srv/trend-scout # non-secret fallback
stages:
  - id: harvest
    cmd: scripts/harvest.sh
    env_keys: [REDDIT_CLIENT_ID, REDDIT_CLIENT_SECRET, STACKEXCHANGE_*]
  - id: deliver
    cmd: scripts/deliver.sh
    env_keys: [TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID]
```

`pipeline add` parses the pipeline-owned file and refuses insecure mode, wrong
ownership, protected locations, malformed selectors, and any inline, file, or
selector collision with reserved `GITMOOT_*` names. An unresolved selector is a
names-only warning only while the pipeline remains disabled, which allows the
pipeline to exist before `key grant`; add-with-enable, enable, manual run, and
scheduled/triggered preflight fail closed until it resolves. Values are loaded
again immediately before each stage process starts, so an atomic rewrite of a
`0600` source rotates credentials without a daemon restart. Resolution is own
`env_file`, then granted shared key, then inline default; Gitmoot's reserved
`GITMOOT_*` entries always win. Registered but ungranted names are not glob or
exact-match candidates. Agent stages resolve only from their seat's configured
proxied grants: pipeline `env_file`, inline defaults, pipeline grants, and
injected keys are never candidates. Ordinary agent jobs receive nothing, and
delegated children do not inherit a pipeline stage's key access.

The persisted stage job payload is the audit record: `PipelineKeyAccess` carries
only `{stage,name,source,mode}` rows (plus legacy env-file/name fields for
already-enqueued jobs), never secret values. `pipeline show --json` exposes the
same rows. Delivery rechecks every shared grant and its registry mode; revoking
after enqueue fails the stage rather than switching to another source. Inline
`env` is stored in the pipeline spec and is therefore for non-secrets only. An
injected key is visible to its shell process; this is scoping and audit, not a
vault.

A configured `proxied` shared key gives the selected shell or agent stage a
per-job placeholder in `<KEY>` and a loopback endpoint in
`GITMOOT_PROXY_<KEY>_URL`. The endpoint is
pinned to the configured HTTPS origin and normalized base path. Gitmoot places
the real credential as either bearer auth or an approved custom header, rereads
the keychain and rechecks the grant on every request, and revokes the lease when
delivery ends. Rotation therefore applies to the next request; revocation
returns `401` without contacting the upstream. The real value never enters an
agent process, though that authorized agent can exercise it against the pinned
upstream. Gate stages remain ineligible, and the Claude model gateway remains
independent.

Proxied mode hides key bytes; it does **not** prevent an authorized child from
exercising the credential on the pinned upstream. Curated upstreams and base
paths are part of the model. Configure only trusted upstreams because an
upstream can observe and potentially reflect the credential.

On `pipeline add --enable`, Gitmoot publishes the owned flow. An unavailable
Activepieces instance leaves a pending binding; run
`gitmoot pipeline bind-trigger <name>` to retry. Disable is local-first: the
bridge rejects event-triggered runs even when Activepieces is unreachable.
Rebinding recreates the owned flow if it was deleted in Activepieces.

### Trigger payloads

`trigger.map` exposes only closed, kind-specific selectors; the spec never accepts
raw Activepieces expressions. For email triggers:

| Selector | Generated Activepieces expression | Meaning |
| --- | --- | --- |
| `subject` | `{{trigger['subject']}}` | Message subject. |
| `from_address` | `{{trigger['from']['value'][0]['address']}}` | First parsed sender address. |
| `text` | `{{trigger['text']}}` | Plain-text message content. |
| `message_id` | `{{trigger['messageId']}}` | Message-ID value (data only; no deduplication yet). |
| `date` | `{{trigger['date']}}` | Message date reported by the IMAP trigger. |

Mapped generated flows require `@gitmoot/piece-gitmoot` 0.1.4 or newer. Binding
fails closed with an error state when that installed version cannot be resolved.
The bridge transport also accepts the same optional `{"payload":{"key":"value"}}`
body for **any** enabled, repo-bound pipeline, even one without a `trigger:` block.

Bridge payloads reject rather than truncate: the raw request is limited to 64 KiB;
there may be at most 32 entries; keys are 1–64 bytes matching
`^[a-z][a-z0-9_]*$`; each UTF-8 string value is at most 32 KiB and cannot contain
U+0000; decoded keys plus values are limited to 48 KiB. There is no event
idempotency or queueing in this release, and an overlapping run receives `409`.

Every agent stage, including root stages, receives a bounded, dynamically fenced
`Trigger payload (UNTRUSTED external data …)` block before upstream context and
its own prompt. Values are data, never instructions; rendered values are capped at
1500 bytes and the block at 6000 bytes with explicit truncation markers. Every
shell stage receives exact exec environment entries named
`GITMOOT_TRIGGER_<UPPERCASE_KEY>`; values are not interpolated into shell source,
so UTF-8 and newlines remain data.

The canonical full payload is retained in the pipeline run's SQLite row. Shell
environment entries are also retained in the stage job payload, and agent prompt
context is retained with normal job data. Treat mapped email content as stored
Gitmoot data and apply the same database/log retention controls.

### Shell-stage upstream context

Every pipeline `cmd` stage receives metadata as exact exec environment entries:
`GITMOOT_PIPELINE_NAME`, `GITMOOT_PIPELINE_RUN_ID`, and
`GITMOOT_PIPELINE_STAGE_ID`. When the stage has `needs`, Gitmoot also sets
`GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE` to a readable `0600` temporary JSON file.
The file exists only while that delivery is running and is removed after success,
failure, timeout, or cancellation. Its JSON content, not its temporary path, is
persisted in the job payload, so restart/retry delivery recreates identical bytes
at a fresh path. Root stages have no context-file variable. The existing
`sh -c <cmd> gitmoot <prompt>` argv contract is unchanged.

The versioned schema is:

```json
{"schema_version":1,"complete":true,"stages":{"extract":{"id":"extract","state":"succeeded","summary":"three records","summary_truncated":false}}}
```

Stage ids are map keys and JSON bytes are deterministic. Each summary's marshaled
JSON string is capped at 16 KiB (shrunk on a UTF-8 rune boundary); the final
marshaled file is capped at 64 KiB. A
truncated summary sets its `summary_truncated` flag. `complete` is `false` whenever
any summary was truncated or any expected upstream stage was omitted by the total
cap, so consumers can fail closed instead of silently processing partial data.

For example, a trusted script can require a complete v1 document before reading a
summary:

```sh
jq -e '.schema_version == 1 and .complete == true' \
  "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE" >/dev/null
summary=$(jq -r '.stages.extract.summary' \
  "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE")
```

Upstream summaries are **untrusted data flowing into your trusted script**. Parse
them as data; never evaluate them as shell source, and do not put credentials in
stage summaries.

### Shell-stage isolation

Non-service shell stages use the managed shared checkout by default, preserving
access to uncommitted and gitignored files and commands that intentionally write
there. Set `isolate: true` on a `cmd` stage to run it in a disposable detached
read-only worktree at the checkout's committed tip. Isolated fork stages key on
their own worktree paths, removing checkout-lock serialization, and each also
takes a **job-scoped** shell runtime-session key (`runtime:shell:job:<hash(job)>`)
instead of the command-hash key, so same-repo siblings run concurrently even when
they share the **identical** `cmd` (#1034). Agent stages already have their own
isolation rules, so `isolate` is rejected on them.

This opt-in path is fail-open: if Gitmoot cannot allocate the worktree, it emits a
`readonly_worktree_skipped` job event and runs the command in the shared checkout.
On successful allocation, the shell environment includes the best-effort path
`GITMOOT_CHECKOUT=<live managed checkout>` so the command can read gitignored
`repos/**` or uncommitted data absent from the committed-tip worktree. Prefer the
stage cwd for committed files. Treat `GITMOOT_CHECKOUT` as read-only: a concurrent
default shell stage that mutates the live checkout can produce inconsistent reads
(including a torn tree or `index.lock` contention), so do not run an isolated reader
beside a checkout-mutating stage. The existing `GITMOOT_INPUT_*`,
`GITMOOT_TRIGGER_*`, metadata, selected `env_keys`, and absolute upstream-context
file remain available independent of the stage cwd.
Service-triggered shell stages remain always isolated and fail closed; their policy
does not depend on this field.

### Agent stages

A stage may run a **named managed gitmoot agent** instead of a shell command. There
are four agent-stage kinds, all sharing the mechanics in this section:

| Kind | Declared by | What it does |
| ---- | ----------- | ------------ |
| **ask** / **review** (#757) | `agent` + `action: ask\|review` | Read-only leaf: looks + decides, never mutates. |
| **implement** (#768) | `agent` + `action: implement` + `write: true` | Mutates the repo + opens a PR (fold-on-PR-opened). See [Implement stages](#implement-stages). |
| **produce** (#814/#825) | `agent` + `action: produce` + `write: true` + `writes:` (+ optional `reads:`) | Sandboxed data writer: Codex everywhere it is supported, plus Claude/Kimi on Landlock-capable Linux; never creates a branch, task, commit, or PR. |
| **orchestrate** (#758) | `agent` + `orchestrate: true` | Sub-tree coordinator: fans out owned children, waits for the tree, folds the synthesis. See [Orchestrate stages](#orchestrate-stages). |
| **gate** (#768) | `gate:` (no `agent`) | Jobless waiter: folds when an external predicate holds (e.g. a PR merges). See [Gate stages](#gate-stages). |

The basic read-only form (#757) sets `agent` + `prompt` (and optionally `action`) in
place of `cmd`:

```yaml
stages:
  - id: extract
    cmd: "python fetch_replies.py > replies.json"
  - id: triage
    agent: reply-triager      # managed agent — create it before the pipeline runs
    action: ask               # ask (default) | review — read-only ONLY
    prompt: "Triage the fetched replies; block if anything needs a human."
    needs: [extract]
```

- **Runs on the agent's own runtime.** Unlike a shell stage (which runs through the
  hidden per-pipeline shell runner via a per-job runtime override), an agent stage
  binds the stage job to the named agent and runs it on **its own** registered runtime
  (claude / codex) and session — no runtime override.
- **Leaf by default.** An `ask`/`review`/`implement`/`produce` stage is a **leaf**: its
  `delegations[]` and `human_questions[]` are stripped (Sender is the pipeline sender),
  so it can neither fan out nor pause a human. (An `orchestrate` stage is the one
  exception — it is a coordinator, not a leaf; see [Orchestrate stages](#orchestrate-stages).)
- **Agent existence is warned at add time.** `pipeline add` checks every referenced
  agent and prints a warning for any that does not exist yet, but does **not** block —
  a spec may legitimately be added before its agents are provisioned (bundled or
  shareable pipelines, scripted setup). A genuinely-missing agent still fails loudly
  when the stage runs, so create it before the pipeline runs (`gitmoot agent …`).
- **Upstream needs-context injection.** At enqueue the stage prompt is **prepended**
  with a bounded `Upstream stage results` block — one labeled entry per `needs` stage
  carrying that stage's fold state and result summary — so a downstream agent stage
  acts on upstream output as real dataflow. Each summary is **fenced** (a backtick
  fence sized longer than any run inside it) so an upstream summary can never spoof the
  block structure or inject instructions into the downstream agent. The block is
  size-bounded and each summary is truncated with a `[truncated]` marker when oversized;
  a root agent stage (no `needs`) receives the bare prompt.
- **Read-only worktree isolation.** A repo-bound ask/review agent stage is born with
  its own detached, committed-tip **read-only worktree** (#739), so it keys
  `worktree:<path>` rather than the shared `repo:<repo>` live checkout — same-repo
  agent stages then run **concurrently** and never mutate the live checkout. The
  worktree is disposed when the stage job settles (and reclaimed on daemon restart).
  Allocation is fail-open for ordinary ask/review stages: if it cannot be created the
  stage still runs, serialized on the shared checkout, with a loud skip event. A
  source-bound PR review is the safety exception: its detached worktree is pinned to
  the source payload's `HeadSHA`, validated clean at that exact SHA, and allocation
  fails closed rather than reviewing the shared default branch. A pure-reasoning
  agent stage with no `repo` needs no worktree.
- **Stateless per run.** Because each agent stage runs in a freshly-created worktree
  directory, a runtime that scopes sessions by working directory (e.g. Claude Code)
  starts a **fresh session each run** — a pipeline stage carries no session context
  across runs. This is intended: a pipeline run is deterministic and independent, so
  supply everything a stage needs via its `prompt` and the injected upstream context,
  not via accumulated agent memory.

Agent stages fold by `decision` and advance/park exactly like shell stages.

### Produce stages

`action: produce` is the data-writing agent stage. Codex keeps its existing native
sandbox. On Linux hosts where `gitmoot sandbox probe` reports supported, Claude and
modern Kimi are also accepted: Gitmoot re-execs the runtime under a strict Landlock
ruleset before it starts. On non-Linux or unsupported kernels, Claude/Kimi retain the
explicit Codex-only refusal; Kimi CLI and shell are always refused. The bound agent
must advertise the `produce` capability and use a `workspace-write` or
`danger-full-access` policy; read-only/auto agents are refused.

```yaml
  - id: publish-dataset
    agent: dataset-writer
    action: produce
    write: true
    reads:
      - /srv/datasets/source
    writes:
      - /srv/datasets/nightly
    network: true
    check: test -s /srv/datasets/nightly/index.json
    check_retries: 2
    prompt: Reconcile and atomically replace tonight's dataset export.
```

`writes:` is the required output allowlist. Optional `reads:` entries expose absolute
input directories without granting write access. Read paths must be cleaned; Gitmoot
resolves symlinks and rejects `/`, its own home, the configured keychain path, the
pipeline's `env_file`, and a read root that equals or contains a declared write root.
The worker repeats those checks immediately before delivery. Declared directories
must exist when delivery begins.

For Claude/Kimi, both lists become runtime `--add-dir` visibility hints, while the
kernel-enforced Landlock rules distinguish them: `reads:` receives `RODirs` only and
`writes:` receives `RWDirs`. When `reads:` is present, unrelated host data is no
longer broadly readable; the runtime retains only fixed system/executable roots,
its state, the disposable workdir, temp space, writes, and the declared inputs.
Without `reads:`, the pre-existing broadly-readable/write-confined policy is
preserved. Runtime state is writable
by design: Claude receives `$HOME/.claude` plus its empirically used
`$XDG_CACHE_HOME/claude-cli-nodejs` cache (and Gitmoot sets
`CLAUDE_CONFIG_DIR=$HOME/.claude`); Kimi receives `$HOME/.kimi-code`. Apart from
those runtime-owned locations and device nodes, the declared write paths, disposable
workdir, and temp roots are the only writable filesystem locations. Codex stays
exactly as before: its native sandbox supplies read access, and Gitmoot never turns
`reads:` into Codex's writable `--add-dir`; its workspace-write defaults remain
writable, and danger-full-access receives no add-dir/network arguments. A produce
stage runs from a disposable detached worktree so accidental repo writes land in
throwaway state; its request never carries branch/task/PR fields and Gitmoot never
pushes that cwd. Unlike read-only ask/review isolation, produce worktree allocation
fails closed and records a failed stage job rather than falling back to the managed
checkout.

For a Claude produce stage that declares `reads:`, Gitmoot also inspects the
operator's user-level Claude settings (`$CLAUDE_CONFIG_DIR/settings.json`, or
`$HOME/.claude/settings.json`, plus `$HOME/.claude.json`). Absolute script paths
referenced by command hooks are admitted by parent directory as read-only runtime
resources; `.claude.json` is admitted as an individual read-only file. Gitmoot
never auto-admits a resource that would expose its home, the configured keychain,
or the pipeline `env_file`. A protected, missing, or unreadable hook fails delivery
before Claude starts and names the path; a relative or malformed hook command adds
a `produce_runtime_resource_warning` job event. Stages without `reads:` retain the
historical unrestricted-read behavior. Kimi exposes no analogous command-hook
surface in its supported config, and Codex continues to use its native sandbox.

Landlock governs filesystem access only in this feature. It does **not** implement
the stage's network policy; network behavior remains whatever the selected runtime
and its CLI policy enforce. There is no advisory fallback for Claude/Kimi: if
Landlock is unavailable, produce dispatch is refused. Codex continues to use its
native sandbox.

The worker repeats the same symlink resolution and protected-root containment check
immediately before delivery. If a path was retargeted after `pipeline add`, the job
fails before any runtime command starts.

After every valid result, `check` runs deterministically. A failure sends the
redacted, 8 KiB-capped output back to the same runtime session and asks for a fresh
result, up to `check_retries`; every correction turn contributes tokens. Exhaustion
fails the stage, after which ordinary stage `retry` may re-run it. Stage retries warn
that partial data may already exist. Produce commands therefore **must be idempotent**:
reconcile or atomically overwrite instead of appending duplicates. Declared data
directories are operator-owned and Gitmoot never deletes or cleans them; only its
disposable cwd follows normal cleanup.

Batch decision convention: `implemented` means the batch is complete;
`changes_requested` means partial output (opt in through `success_decisions` if that
should advance); `blocked` means human input is required and parks with `needs`;
`skipped` means there was no work.

### Implement stages

An `implement` agent stage (#768) **mutates the repo and opens a pull request**, then
the pipeline advances the moment the PR is opened (**fold-on-PR-opened**):

```yaml
stages:
  - id: fix
    agent: fixer
    action: implement
    write: true                       # required acknowledgement (see Safety)
    prompt: "Apply the change described in the ticket."
  - id: verify
    agent: reviewer
    action: review
    source: fix                       # bind to fix's PR + exact head SHA
    needs: [fix]
    success_decisions: [approved]     # changes_requested parks before the merge gate
  - id: wait
    gate: pr_merged
    source: fix
    needs: [fix, verify]
```

- **Writable worktree + deterministic branch reuse.** Unlike a read-only stage, an
  implement stage runs in a **writable** task-worktree on a deterministic,
  attempt-independent branch `gitmoot/pipe-<run>-<stage>` (reusing the implement
  dispatch's fail-closed guards). A **retry** lands in the *same* branch/PR — never a
  duplicate — and fails closed if that worktree is dirty or has a live process.
- **`implemented` folds on PR-opened.** Only `implemented` promises a PR, so that
  decision waits until the job carries the opened PR stamp (or the engine confirms a
  terminal no-PR no-op). Other configured success decisions, including `approved` and
  `skipped`, settle immediately without a PR. The stage summary records the opened PR
  number or the terminal no-PR outcome for downstream context.
- **Does not merge by itself.** The implement stage only opens the PR. The default
  gate still waits for a human merge; a separately double-keyed `merge: auto` gate
  may merge after review and CI (see [Gate stages](#gate-stages)).
- **Declared review suppresses native fan-out.** When an implement stage has a
  downstream `action: review` stage with `source` pointing to it, its job carries
  `SkipNativeReviewFanout`. Gitmoot does not also enqueue the ordinary native
  reviewers; without that declaration, native review behavior is unchanged.
- Still a **leaf** — it may mutate but never fan out.

### Source-bound review stages

Set `source: <implement-stage>` on an `action: review` stage to review the PR that
the implement stage just opened. `source` must also appear in the review stage's
`needs`, and must name an `action: implement` stage.

- At enqueue, Gitmoot reads the succeeded source stage row's `JobID`, parses that
  job's structured payload stamp, and copies `PullRequest`, `HeadSHA`, `Branch`,
  `TaskID`, and `LeadAgent` onto the review job. It never scrapes the stage summary.
- The review runs in a detached read-only worktree at the stamped `HeadSHA` and the
  existing review checkout preflight verifies that exact clean head.
- The review is **report-only** for native PR lifecycle purposes. Its verdict is
  still posted as a PR comment and folded by the pipeline's `success_decisions`, but
  `changes_requested` does not dispatch a native fix job and `approved` does not run
  the native merge gate. Human merge remains the default.
- If the succeeded implement stage produced no PR (a permanent no-op or any successful
  non-`implemented` decision), the review stage immediately folds `blocked` with
  `source stage produced no PR; nothing to review`; no unbound review job is sent.

### Gate stages

A `gate` stage (#768) runs **no worker job** — it is a patient waiter that folds when an
external predicate holds. It is the composable way to express *wait-for-merge* without
making the implement stage itself block:

```yaml
stages:
  - id: fix
    agent: fixer
    action: implement
    write: true
    prompt: "Apply the change."
  - id: wait
    gate: pr_merged                   # only predicate today
    source: fix                       # the upstream implement stage whose PR to watch
    needs: [fix]
  - id: deploy
    cmd: "./deploy.sh"
    needs: [wait]
```

- **`pr_merged`** watches the PR opened by the `source` implement stage and folds
  **succeeded** once it merges. It reads the store's PR state (kept current by the
  daemon's PR poller), so it needs no live GitHub call, is **non-blocking** and
  **fails open** (keeps waiting while unmerged), and is bounded by the stage `timeout`.
- A PR that is **closed without merging**, or a **timeout**, parks the run `blocked`
  (a gate is a wait, not a failure — so a retry budget can't re-arm the timer).
- A source stage that succeeds without opening a PR also parks the gate `blocked`
  immediately with `source stage succeeded without opening a PR; nothing to wait for`.
  Only `implemented` promises the PR that this gate can watch.

#### Opt-in auto-merge

The default is and remains **human merge**. To let the pipeline advancer perform
the squash merge, spell both keys and declare at least one source-bound review:

```yaml
allow_auto_merge: true
stages:
  - id: fix
    agent: fixer
    action: implement
    write: true
    prompt: "Apply the change."
  - id: review
    agent: reviewer
    action: review
    source: fix
    needs: [fix]
    prompt: "Review the implementation PR."
    success_decisions: [approved]
  - id: merge
    gate: pr_merged
    merge: auto
    source: fix
    needs: [fix, review]
```

- `merge: auto` is valid only on a gate and requires top-level
  `allow_auto_merge: true`. A scheduled pipeline still needs
  `allow_scheduled_writes: true` for its implement stage, so scheduled auto-merge
  requires **both** top-level keys.
- Registration rejects an auto-merge gate unless at least one `action: review`
  stage binds to the same implement `source`. Before merging, the advancer requires
  **every** such review stage to have folded `succeeded` with decision `approved`.
  `changes_requested` parks the run and never merges.
- The source PR and reviewed head come from structured job payloads, never summary
  prose. The live PR head must still equal that reviewed `HeadSHA`; drift parks the
  gate `blocked` and names the mismatch.
- GitHub must report the PR mergeable and at least one external CI status/check.
  Pending checks keep the jobless gate waiting within its stage timeout. Check-run
  `skipped`/`neutral` outcomes count as passing, matching the native merge gate;
  failed checks park it `blocked`. Zero external statuses/checks always park
  pipeline auto-merge `blocked`, even when `[merge_gate] require_external_ci` is
  false—the unattended path never synthesizes no-CI success.
- Unmergeable/conflicting PRs, head drift, and merge API errors park it `blocked`;
  a merge API failure is tried once and never retry-spammed.
- The source job timeline atomically records `pipeline_auto_merge_claim` before
  the write and `pipeline_auto_merge_confirmed` afterward, carrying pipeline/run,
  stage, PR, and head SHA. Racing scans that lose the claim never call merge.

### Orchestrate stages

An `orchestrate` agent stage (#758) is a **bounded sub-tree coordinator**: instead of
doing the work itself, its agent returns `delegations[]` that fan out as **children it
owns**, and the stage waits for the whole tree, then folds the synthesis:

```yaml
stages:
  - id: investigate
    agent: coordinator
    orchestrate: true
    prompt: "Decompose the incident and delegate the sub-investigations."
  - id: report
    cmd: "./publish.sh"
    needs: [investigate]
```

- **The stage job is the sub-tree root.** Children inherit the full delegation bounds
  ladder (depth cap, job-budget admission, wall-clock / token / cost budgets, loop
  detection, graceful finalize, root kill). The stage `timeout` bounds the whole tree.
- **Not a leaf.** The delegations strip is relaxed **only** for a validated orchestrate
  stage — a stage that merely sets `orchestrate: true` but validates as something else
  cannot fan out.
- **Waits, then folds the synthesis.** The pipeline follows the coordinator's
  continuation chain to the terminal tail and folds *that* result — a pure, restartable
  DB walk (a daemon restart re-derives the wait, never restarting the sub-tree).
- **`retry: 0` recommended** — a retry mints a fresh tree only after the old one is
  terminal, never resuming a half-finished tree.

### The stage contract

A stage command runs under the shell runtime as `sh -c '<cmd>' gitmoot '<prompt>'`
(so `$0` is `gitmoot` and `$1` is the stage's job prompt). The stage signals its
outcome by printing a `gitmoot_result` JSON blob to **stdout**; the advancer folds
the stage by that result's **`decision`**, never by the job's exit state:

```sh
# A stage that succeeds:
printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"synced"}}'

# A stage whose task had no work:
printf '%s' '{"gitmoot_result":{"decision":"skipped","summary":"no new replies today"}}'

# A stage that parks the run awaiting a human, listing what it needs:
printf '%s' '{"gitmoot_result":{"decision":"blocked","summary":"secret missing","needs":["R2 token"]}}'
```

The decision drives advancement:

- **`decision` in the stage's `success_decisions`** → the stage **succeeded**;
  stages whose `needs` have all now succeeded are enqueued.
- **`decision: blocked`** → the stage is **blocked**; its `needs` are persisted at
  the stage and run level and the run **parks blocked**. Downstream stages are
  never enqueued.
- **`decision: failed`, any decision outside the stage's `success_decisions`
  (`changes_requested` by default), a cancelled job, or no `gitmoot_result` at all**
  → the stage **failed**. If the stage has retry budget left it is re-attempted;
  otherwise the run **parks failed**.

`changes_requested` is a stage **failure by default** — even though the underlying
job succeeded — because a stage folds on the decision, not the job state, and a
review that asked for changes is not "this step landed." List it in a stage's (or
the pipeline's) `success_decisions` to treat it as success instead.

`skipped` is a stage **success by default**. Gitmoot prefixes its persisted summary
with `[skipped: no work]`, and downstream agent stages receive that honest note in
their upstream context. An explicit `success_decisions` list is strict: if it omits
`skipped`, the stage fails because the author required real work. A skipped result
uses the existing succeeded stage state; the `SKIPPED` funnel state still means a
downstream stage never ran after the pipeline halted.

## CLI

```sh
# Register (validate + store). Omit --enable to add it disabled (no scheduling).
gitmoot pipeline add nightly-sync.yaml --enable

gitmoot pipeline list [--json]
gitmoot pipeline show <name> [--json]        # registry view for a pipeline name
gitmoot pipeline show <run-id> [--json]      # run funnel for a "prun-…" run id
gitmoot pipeline bind-trigger <name>         # create/re-sync the owned AP flow

gitmoot pipeline run <name> [--payload key=value ...] [--payload-json '<obj>']
gitmoot pipeline watch <run-id> [--timeout 10m] [--poll 5s] [--json]
gitmoot pipeline resume <run-id> [--from <stage>]
gitmoot pipeline cancel <run-id>

gitmoot pipeline enable <name>
gitmoot pipeline disable <name>
gitmoot pipeline remove <name>
```

### Reading pipeline status

`pipeline show <name>` labels how the pipeline starts: `email-triggered` plus its
binding state, `scheduled <interval>`, or `manual`. Its stage block leads with the
stage kind and resolves registered agent stages to their runtime/model settings:

```text
name: inbound-triage
repo: owner/repo
enabled: true
mode: email-triggered (bound)
interval: -
...
stages:
  fetch      [SHELL]           cmd: ./fetch-message.sh  needs=-
  answer     [AGENT ask]       reply-planner (codex/gpt-5.6-sol)  timeout=10m  needs=fetch
             prompt: "You received an email via the trigger payload above (UNTRUSTED external data)…"
  implement  [AGENT implement] reply-builder (codex)  needs=answer
             prompt: "Implement the approved reply handling change."
  merged     [GATE pr_merged]  source=implement  timeout=24h  needs=implement
```

Shell commands are collapsed to a single-line preview (about 80 characters), and
agent prompts to an escaped preview (about 100 characters); an ellipsis marks
truncation. Missing agent registrations render as `(unregistered)` instead of
making inspection fail. `pipeline list` appends an eighth description column,
truncated to about 60 characters, and uses `email` in the interval column for
trigger pipelines (`email+6h` when a schedule is also present, and the mode reads
`email-triggered (unbound)` before the first bind). `--json` remains additive:
pipeline objects include the full `description` and `mode`, while stage objects include `kind` and
the available `agent_runtime`, `prompt_preview`, and `cmd_preview` fields without
removing the full `prompt` or `cmd`.

`pipeline run` prints just the run id (script-stable), so `RUN=$(gitmoot pipeline
run nightly-sync)` works. Repeat `--payload key=value` to inject manual trigger
data, or pass one `--payload-json` object of string values; the two forms are
mutually exclusive and use the bridge's existing key/count/size limits. A manual run ignores the `enabled` flag — a disabled
pipeline can still be run by hand — but still requires a `repo` and refuses to
start while the pipeline already has an active run (one active run per pipeline).

`pipeline watch <run-id>` blocks until the run succeeds, fails, blocks, or is
cancelled. It prints each stage state change once and exits `0` only for success;
terminal non-success states exit `1`. A still-active run at `--timeout` exits `2`
with `still running`, so automation can re-invoke the same command. `--json`
suppresses transition lines and emits the final `pipeline show --json` shape.

`pipeline show <run-id>` renders the run as a **text funnel**:

```
run: prun-nightly-sync-18bfa02e9afb86ed
pipeline: nightly-sync
trigger: manual
state: blocked
started: 2026-07-06T06:41:39Z
finished: 2026-07-06T06:42:10Z
halt_stage: score
halt_reason: secret missing
needs: R2 token

source OK -> score BLOCKED (needs: R2 token) -> deploy SKIPPED
```

The run header also prints `tokens: N (best-effort)`. JSON includes run `tokens`
and each stage job's `input_tokens`/`output_tokens` when captured. Codex resumed
session edge cases and runtimes without usage reporting contribute `0`, so this is
honest accounting rather than a billing guarantee.

Queued and running stages also get detail lines such as:

```
  source: RUNNING; enqueued 2m14s ago
    last activity 8s ago: downloading object 41
```

`enqueued` is deliberate: the stage `started_at` is written when the job is
enqueued, not when a worker claims it. After 60 seconds of delivery, pipeline
workers update one latest-only `progress` job event about every 30 seconds. Its
age is computed from the stored event timestamp, so a stopped daemon never looks
fresh. Activity is stripped of terminal controls, redacted, and length-capped
before storage. Orchestrate stages may have no current per-stage event while their
sub-tree runs; the view says `(sub-tree running; no per-stage progress)` and does
not treat that absence as a failure. `--json` stage objects add `started_at`,
`finished_at`, and optional `{elapsed, activity, updated_at}` progress data.

Funnel labels are `OK` for a succeeded stage, `BLOCKED (needs: …)` for a parked
stage, and the uppercased state otherwise (`PENDING`, `QUEUED`, `RUNNING`,
`FAILED`, `SKIPPED`, `CANCELLED`). When a run **failed**, the view also prints the
exact command to file a bug for the halted stage's job — gitmoot never files it for
you:

```
stage failed; report it with:
  gitmoot report bug --job <stage-job-id>
```

`--json` on `list` and `show` emits a stable machine shape (pipelines as an array;
a run as `{id, pipeline, trigger, state, halt_stage, halt_reason, needs, spec_hash,
started_at, finished_at, funnel, stages[]}`).

### Resume

`pipeline resume <run-id>` re-runs a **parked** (blocked or failed) run from its
halted stage; `--from <stage>` overrides the resume point. Resume resets the halted
stage (or `--from` stage) and every stage that transitively **depends** on it back
to pending — bumping each reset stage's attempt so the next scan enqueues a fresh
stage job — clears the run's park fields, and returns the run to `running`. It
**never re-runs a stage that already succeeded**, even one downstream of the resume
point, and it refuses a run that is not parked. Because a run executes its spec
snapshot, resume is refused if the pipeline's spec changed since the run was
created.

The intended story: a stage blocks on something the operator must provide (a
missing secret, an unapproved change), the operator provisions it out of band, then
`pipeline resume` re-runs the halted stage and everything downstream — while the
already-landed upstream stages are left untouched. Approval gates that call resume
automatically are a follow-up (#682); v1 is the manual verb.

### Cancel and remove

`pipeline cancel <run-id>` abandons a running or parked run: it cancels each
in-flight stage job through the shared `job cancel` path (which also best-effort
releases the job's locks) and marks the run and its non-terminal stages
`cancelled`. An already-settled stage keeps its recorded outcome, so a cancelled
run still shows why it halted. It refuses an already-terminal (succeeded/cancelled)
run.

`pipeline remove <name>` deletes the pipeline and disposes its hidden shell runner
agent (below).

## Scheduling

A pipeline with a `schedule.interval` and `enable`d auto-runs on that interval,
using the same durable-`next_due` idiom as heartbeats. The daemon's pipeline scan
runs in **both** the registered-repo and single-repo daemon loops and does two
passes per tick:

1. **Schedule pass** — for each enabled pipeline whose interval is due and that has
   no active run, create a fresh scheduled run and advance `next_due` **anchored to
   now**.
2. **Advance pass** — advance every in-flight run once (fold settled stage jobs,
   enqueue newly-ready stages, park or finish).

A parked or terminal run is never advanced, so a blocked/failed run consumes **zero
compute** while it waits. Behavior worth knowing:

- **Interval + jitter only** — there is no cron parser in v1; because `next_due` is
  durable, a cron front-end is a documented drop-in for a later release.
- **One active run per pipeline** — a scheduled tick that finds a run in flight is
  skipped without advancing `next_due`, so the next run fires as soon as the current
  one settles. A parked run does **not** count as active.
- **Missed ticks coalesce** — a long-idle scheduler that missed many intervals fires
  exactly **one** run and schedules the next one interval out; it never replays a
  backlog.
- **Restart-safe** — the advancer recovers purely from the persisted run/stage rows,
  so a daemon restart mid-run picks the run back up and completes it.
- **Repo required to run** — a scheduled pipeline with no `repo` is skipped and its
  `next_due` advanced (so a misconfigured schedule does not hot-loop and
  self-recovers once a repo is set), mirroring the heartbeat repo-unmanaged idiom.

## The hidden runner agent

`pipeline add` auto-creates one hidden shell agent per pipeline, named
`pipeline-<name>-runner`, that owns the pipeline's stage jobs (the worker loop
resolves a job's agent record). The stage command travels **per job** (in the stage
job's runtime-override ref), not on the agent's runtime ref, so one runner serves
every stage. These runner agents are an implementation detail and are **filtered out
of `gitmoot agent list`**; `pipeline remove` disposes them.

## Observability

- `gitmoot pipeline list` shows each pipeline's enabled state, interval, repo, trigger binding, and
  last run status.
- `gitmoot pipeline show <name>` shows the registry view (spec hash, schedule,
  last/next run bookkeeping, and the stage DAG).
- `gitmoot pipeline show <run-id>` shows the run funnel.
- `gitmoot pipeline watch <run-id>` waits for terminal state without an agent-side poll loop.
- Stage jobs are ordinary jobs (sender `pipeline`), so they also appear in the usual
  job/status surfaces.

### Web dashboard

The web dashboard (`gitmoot dashboard --web`) has a dedicated, read-only
**Pipelines** view: a list of every declared pipeline with its schedule state,
next-due countdown, and recent-run outcomes, and a per-run detail that renders the
stage DAG in spec (topological) order with the same halt/needs information the
`pipeline show <run-id>` funnel prints. It surfaces the resume / bug-report
commands for a parked run as copyable text but never mutates anything. See
[Dashboard Views → Pipelines](https://gitmoot.io/docs/dashboard/views#pipelines).

## Safety

`pipeline add` is an **operator-trust action**: a stage's `cmd` runs verbatim via
`sh -c` with the daemon's permissions. Only register specs you would run yourself —
the same trust you extend to a heartbeat prompt or a CI step. The spec is stored
verbatim (raw bytes), so treat a spec containing private hostnames, paths, or repo
names the same as any other private-repo data. See `SAFETY.md → Pipeline stages run
with daemon permissions`.

**Mutating (`implement`) stages** carry an extra safety model (#768): `action:
implement` requires an explicit `write: true` double-key (and `write: true` is valid
only on an implement stage), so a typo or prompt injection cannot flip a read-only
pipeline into a writing one; a mutating stage on a **scheduled** pipeline is rejected
unless the pipeline sets `allow_scheduled_writes: true`; a mutating stage on a
**triggered** pipeline is rejected unless it sets `allow_triggered_writes: true`;
the bound agent's own
capability/policy still applies; and the implement job never merges its own PR. The
default gate leaves merge to a human or CI; only the separately double-keyed,
review-required `merge: auto` gate grants merge authority to the advancer.

## Not yet supported (deferred)

These are intentionally out of scope for v1 and tracked as follow-ups:

- A cron schedule front-end (interval + jitter only today).
- Approval gates / secret stores / an approval UI for a blocked stage (#682) — v1
  ships the manual `pipeline resume` seam.
- Auto-filing a bug for a failed stage (`show` prints the command; you run it).
- Arbitrary per-stage env/workdir. (`GITMOOT_TRIGGER_*` inputs, pipeline metadata,
  the shell upstream-context file, agent trigger context, and upstream stage output
  flowing into a downstream agent prompt **are** supported.)
- Gate predicates beyond `pr_merged`, and a `gate` folding on PR-**merged** built into
  the implement stage itself (use a separate `gate` stage).
- Matrix / dynamic stages, more than one concurrent run per pipeline, pipelines
  defined from the dashboard, a web funnel view, and a foreground `--watch`.
</content>
