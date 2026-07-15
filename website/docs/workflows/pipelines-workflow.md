# Run A Fixed Multi-Step Flow (Pipelines)

Pipelines let the gitmoot daemon run a **declared DAG of shell and agent stages** — a fixed,
repeatable multi-step flow with explicit dependencies — on demand, on an interval,
or after another pipeline succeeds. Each stage is an ordinary queued job (shell commands use the shell runtime;
agent stages use their registered runtime): the
existing worker tick claims and runs it, and a scan-based **advancer** folds each
stage's `gitmoot_result` decision and enqueues the stages whose dependencies have all
succeeded. Pipelines reuse the same job queue, result contract, and scheduling idiom
as [heartbeats](./heartbeat-schedules-workflow.md) — there is no separate runner.

Pipelines are **off by default**: with no pipelines defined, the daemon's pipeline
scan returns before touching any state.

Reach for a pipeline when the steps and their wiring are **known up front**. When you
instead want dynamic, model-driven decomposition, reach for an
[orchestra](./coordinator-recipes-workflow.md) — a pipeline stage is a leaf and can
never fan out into a delegation tree (see [Stages are leaves](#stages-are-leaves)).

## The short version

Declare the DAG as YAML:

```yaml
# nightly-sync.yaml
name: nightly-sync          # required, name-safe token (letters, digits, - _)
repo: owner/repo            # optional to register; REQUIRED to run
group: Release Automation   # optional display section on /pipelines and `pipeline list`;
                            #   free-form, decoupled from repo (one group may span repos,
                            #   one repo may split across groups); unset falls back to repo.
                            #   Built-in memory pipelines ship under "Gitmoot System".
description: Syncs nightly data for deployment. # optional detail-page purpose (multiline, max 500 chars)
schedule:                   # optional; auto-runs every interval once enabled
  interval: 24h             #   positive Go duration (required with a schedule block)
  jitter: 15m               #   optional random [0, jitter] added to each next_due
trigger:                    # optional event source (requires repo:)
  kind: email
  connection: gmail-imap    #   optional; default gmail-imap
  mailbox: INBOX            #   optional; default INBOX
  map:                      #   optional outputs from closed email selectors
    subject: subject
    sender: from_address
stages:                     # the DAG, keyed by unique id and wired by needs
  - id: source
    cmd: "curl -sf https://example.com/data > data.json"
  - id: score
    cmd: "python score.py data.json"
    needs: [source]         # runs only after source SUCCEEDS
  - id: deploy
    cmd: "rclone copy out/ r2:bucket"
    needs: [score]
    timeout: 30m            # optional per-stage job timeout
    retry: 2                # optional; re-attempt a FAILED stage up to N times
```

Register it and run it:

```sh
# Validate + store. --enable turns on the interval schedule; omit it to add disabled.
gitmoot pipeline add nightly-sync.yaml --enable

# Or trigger a manual run right now (script-stable: prints just the run id).
RUN=$(gitmoot pipeline run nightly-sync)

# Watch the run as a text funnel.
gitmoot pipeline show "$RUN"
```

`pipeline add` validates the whole spec at add time — unknown keys, a non-name-safe
name/id, a duplicate stage id, a stage that is not exactly one of `cmd`, `agent`, or
`gate` (and, per kind: an agent stage's missing `prompt` or invalid `action`,
`implement` without `write: true`, a mutating stage on a scheduled pipeline without
`allow_scheduled_writes`, a mutating stage on a triggered pipeline without
`allow_triggered_writes`, a gate/review's bad `source`, or `source` on another kind), an unknown/self/cyclic
`needs`, a self/cyclic pipeline trigger, a schedule+pipeline-trigger hybrid, an invalid duration, a negative retry, or a
`success_decisions` value outside `approved`/`implemented`/`changes_requested`/`skipped` - so a
structural mistake is a clear error at registration, not a stuck run later. It stores the raw YAML **verbatim**
plus a content hash; each run snapshots the hash and executes its snapshot, so
editing the file later never mutates an in-flight run.

### Chain pipelines on success

Use a pipeline trigger when an upstream must finish successfully before the next
flow starts:

```yaml
name: memory-ingest-sweep
repo: owner/repo
trigger:
  kind: pipeline
  pipeline: memory-groom-propose
stages:
  - id: sweep
    cmd: gitmoot memory ingest sweep --json
```

This replaces a `24h` groom plus `24h30m` ingest clock stagger with real ordering.
Each newly-succeeded groom run fires ingest once; failed and cancelled runs do
nothing. A durable cursor makes re-ticks and daemon restarts idempotent. Add and
enable re-arm at the latest upstream run, preventing historical or disabled-period
backfill. If ingest already has an active run, its cursor stays put and the same
upstream success fires after ingest settles.

Pipeline-trigger cycles (including self-reference) are rejected at `pipeline
add`. A missing upstream warns but is allowed: the downstream stays dormant and
renders as `after: <upstream> (upstream missing)` until the upstream exists again.
Pipeline triggers do not use Activepieces, and `pipeline bind-trigger` is a
friendly no-op for them. Schedule-plus-pipeline hybrids are deliberately rejected
in this MVP.

## Share a pipeline bundle

Use a private GitHub repository as a reviewable pipeline catalog, then pull it
into another Gitmoot home against that machine's repository:

```sh
# Source home. --create makes acme/pipeline-catalog PRIVATE.
gitmoot pipeline remote set acme/pipeline-catalog
gitmoot pipeline publish nightly-sync --create

# Target home.
gitmoot pipeline remote set acme/pipeline-catalog
gitmoot pipeline pull --list
gitmoot pipeline pull nightly-sync \
  --repo acme/nightly-target \
  --name nightly-sync-copy \
  --agent-map scorer=local-scorer
```

The remote layout is intentionally inspectable:

```text
pipelines/nightly-sync/
├── bundle.yaml           # version, requirements, warnings, agents, spec hash
├── spec.yaml             # original bytes with repo parameterized
└── templates/
    └── scorer.md         # canonical template snapshot, including full prompt
```

An unchanged republish performs no writes. When bytes change, only the changed
files are committed, and template files that vanished from the exported bundle
are deleted from that managed directory. Template prompts and metadata travel
verbatim, so `--create` creates a private repository and you should only target a
public repository when those prompts are intentionally public. Set a one-off
repo with `--remote owner/repo`; `[pipeline_remote]` stores the default `repo`,
`ref` (default `main`), and `path` (default `pipelines`).

`pipeline pull --list` prints each manifest's name, description, and requirements
on one line. Pull downloads the selected directory at HEAD and then uses the
same import path and gates described below.

`spec.yaml` preserves comments, ordering, and block formatting; only its `repo`
scalar becomes `__GITMOOT_REPO__`. Template prompts travel verbatim, so review
them before publishing a bundle. Local trigger bindings, tokens, Activepieces
credentials, and environment values are never exported. Connection names remain
in `bundle.yaml` as requirements so the recipient can create the credentials
locally.

Import prints a requirements report on every attempt: runtimes, named connections
when checkable, upstream pipelines, write-authority flags, and host-specific
absolute paths found in command stages. An unmapped agent whose runtime is
missing is a hard failure. `--agent-map exported=local` selects an existing local
agent; without a mapping Gitmoot installs the embedded template and registers the
agent. Different-content template/agent and pipeline-name collisions require
`--force`; use `--name` when both copies should coexist.

Imports land **disabled by default**. Review the report, especially
`allow_scheduled_writes`, `allow_triggered_writes`, and `allow_auto_merge`, then
enable explicitly (or pass `--enable` to import) to re-consent that authority.
Missing upstreams are allowed and keep the pipeline dormant. The imported
`spec_hash` is computed from the final stored bytes after repo/name/agent mapping,
so it intentionally differs from the source hash when those parameters change.

For an offline transfer, export and import the same directory format directly:

```sh
gitmoot pipeline export nightly-sync --output ./nightly-sync.bundle
gitmoot pipeline import ./nightly-sync.bundle --repo acme/nightly-target
```

## Manage pipelines

```sh
gitmoot pipeline list [--json]
gitmoot pipeline show <name> [--json]        # registry view for a pipeline name
gitmoot pipeline show <run-id> [--json]      # run funnel for a "prun-…" run id
gitmoot pipeline bind-trigger <name>         # create/re-sync the owned AP flow
gitmoot pipeline install-defaults            # install built-in memory pipelines
gitmoot pipeline export <name> --output <dir>
gitmoot pipeline import <dir> --repo owner/repo [--agent-map exported=local]
gitmoot pipeline remote set <owner/repo> [--ref <ref>] [--path <subdir>]
gitmoot pipeline remote show
gitmoot pipeline publish <name> [--remote owner/repo] [--create]
gitmoot pipeline pull --list [--remote owner/repo]
gitmoot pipeline pull <name> [--remote owner/repo] --repo owner/repo [--agent-map exported=local]
gitmoot pipeline run <name>                  # start a manual run; prints the run id
gitmoot pipeline resume <run-id> [--from <stage>]
gitmoot pipeline cancel <run-id>
gitmoot pipeline enable <name>
gitmoot pipeline disable <name>
gitmoot pipeline remove <name>
```

### Reading pipeline status

`pipeline show <name>` labels how the pipeline starts: `email-triggered` plus its
binding state, `after: <upstream>`, `scheduled <interval>`, or `manual`. Its stage block leads with the
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
making inspection fail. `pipeline list` keeps its existing six-column shape but
uses `email` or `after: <upstream>` in the interval column for trigger pipelines
(`email+6h` when an email schedule is also present, and the mode reads
`email-triggered (unbound)` before the first bind). `--json` remains
additive: pipeline objects include `mode`, while stage objects include `kind` and
the available `agent_runtime`, `prompt_preview`, and `cmd_preview` fields without
removing the full `prompt` or `cmd`.

A manual `pipeline run` ignores the `enabled` flag — a disabled pipeline can still be
run by hand — but still requires a `repo` and refuses to start while the pipeline
already has an active run (one active run per pipeline). `pipeline add` also
auto-creates one hidden shell runner agent per pipeline (`pipeline-<name>-runner`)
that owns the stage jobs; it is filtered out of `gitmoot agent list` and disposed by
`pipeline remove`.

An enabled pipeline with `trigger.kind: email` auto-binds an owned Activepieces
flow. If Activepieces is unavailable, local registration succeeds with a
pending binding and prints the `pipeline bind-trigger` repair command. The
connection id must be name-safe because it is embedded in an Activepieces
expression. `pipeline disable` updates the local registry first, so bridge-triggered runs fail closed even if
Activepieces cannot be reached to disable its listener. Rebinding recreates the
owned flow if it was deleted in Activepieces.

### Trigger payloads

`trigger.map` compiles closed email selectors; raw Activepieces expressions are
never accepted in a pipeline spec.

| Selector | Generated expression | Meaning |
| --- | --- | --- |
| `subject` | `{{trigger['subject']}}` | Message subject. |
| `from_address` | `{{trigger['from']['value'][0]['address']}}` | First parsed sender address. |
| `text` | `{{trigger['text']}}` | Plain-text content. |
| `message_id` | `{{trigger['messageId']}}` | Message-ID (data only; no dedupe yet). |
| `date` | `{{trigger['date']}}` | Message date. |

Map output names must be 1–64 bytes and match `^[a-z][a-z0-9_]*$`; an explicit
empty map is rejected. Mapped flows require `@gitmoot/piece-gitmoot` 0.1.4 or
newer, and binding fails closed if that installed version cannot be resolved.

The bridge accepts optional `{"payload":{"key":"value"}}` input for any enabled,
repo-bound pipeline, whether or not its spec declares a trigger. It rejects a raw
body over 64 KiB, more than 32 entries, a value over 32 KiB, U+0000, or more than
48 KiB of decoded keys and values. Keys use the same trigger-map rule. Overlapping
runs still receive `409`; idempotency and queueing are deferred.

Every agent stage receives the input before upstream context in a dynamically
fenced block labeled `UNTRUSTED external data`; each rendered value is capped at
1500 bytes and the whole block at 6000 bytes with explicit truncation markers.
Every shell stage receives exact `GITMOOT_TRIGGER_<UPPERCASE_KEY>` exec environment
entries. Values are never interpolated into shell source, so newlines and UTF-8
remain data. The full canonical payload lives in the SQLite run row, is printed
as `payload_json` by `pipeline show <run-id>`, and shell env and
agent prompt projections also live in normal job data.

### Shell-stage upstream context

Every pipeline shell stage receives exact `GITMOOT_PIPELINE_NAME`,
`GITMOOT_PIPELINE_RUN_ID`, and `GITMOOT_PIPELINE_STAGE_ID` environment entries.
A stage with `needs` also receives `GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE`, which
names a readable `0600` JSON tempfile. It exists only during delivery and is
removed after success, failure, timeout, or cancellation. Gitmoot persists the
content rather than the path, so retries and restarts recreate identical bytes at
a fresh path. Root stages do not receive the context-file variable, and the
existing `sh -c` argv contract is unchanged.

```json
{"schema_version":1,"complete":true,"stages":{"extract":{"id":"extract","state":"succeeded","summary":"three records","summary_truncated":false}}}
```

Map-key ordering makes the JSON deterministic. Each summary's marshaled JSON
string is capped at 16 KiB with rune-safe truncation, and the final marshaled file
at 64 KiB. `summary_truncated:true` marks a
per-stage truncation; `complete:false` means a summary was truncated or an expected
stage was omitted, allowing consumers to fail closed:

```sh
jq -e '.schema_version == 1 and .complete == true' \
  "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE" >/dev/null
summary=$(jq -r '.stages.extract.summary' \
  "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE")
```

Summaries are untrusted data flowing into your trusted script. Parse them as data,
never evaluate them as shell source, and do not put credentials in summaries.

A triggered pipeline containing an `implement` or `produce` stage must set
`allow_triggered_writes: true`, in addition to each stage's `write: true`. This is
independent of `allow_scheduled_writes` when an email pipeline has both trigger and schedule.

`pipeline install-defaults` installs Gitmoot's built-in memory pipelines:
`memory-ingest-sweep` and `memory-groom-propose`. The daemon also runs that
installer at startup. The installer is idempotent; if either name already exists,
Gitmoot skips it and preserves the user's stored YAML, hash, enabled flag, and
schedule. With empty config, the definitions are installed manual-only. Add
`[[memory.ingest]]` sources and optional `[memory.pipelines]` intervals to make
them useful:

```toml
[[memory.ingest]]
path = "/path/to/markdown-notes"
agent = "lead"
repo = "owner/repo"
tier = "repo"

[memory.pipelines]
repo = "owner/repo"
ingest_sweep = "nightly"
groom_propose = "nightly"
```

`nightly` is accepted as `24h`; any positive Go duration such as `"12h"` also
works. If schedules are unset, run them on demand:

```sh
gitmoot pipeline run memory-ingest-sweep
gitmoot pipeline run memory-groom-propose
```

The installed `memory-ingest-sweep` spec has a fixed two-stage shape: `sweep`
calls `gitmoot memory ingest sweep --json`, then `summarize` reports the totals.
The source list is loaded from `[[memory.ingest]]` at run time, so config edits
apply on the next scheduled or manual run without reinstalling defaults. A bad
source is reported in the sweep JSON and does not stop other sources. The stage
fails visibly only when the config is invalid or every configured source fails.
With no sources, the run succeeds with a skipped summary.

## The stage contract

A stage command runs as `sh -c '<cmd>' gitmoot '<prompt>'`. It signals its outcome by
printing a `gitmoot_result` JSON blob to **stdout**; the advancer folds the stage by
that result's **`decision`**, never by the job's exit state:

```sh
# succeeds:
printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"synced"}}'
printf '%s' '{"gitmoot_result":{"decision":"skipped","summary":"no new replies today"}}'
# parks the run awaiting a human, listing what it needs:
printf '%s' '{"gitmoot_result":{"decision":"blocked","summary":"secret missing","needs":["R2 token"]}}'
```

- a decision in the stage's `success_decisions` (default `["approved","implemented","skipped"]`)
  → the stage **succeeded**; stages whose `needs` have all now succeeded are enqueued;
- `blocked` → the stage **blocks**; its `needs` are persisted at the stage and run
  level and the run **parks blocked** (downstream stages never enqueue, zero compute
  while parked);
- `failed`, any decision outside the stage's `success_decisions` (`changes_requested`
  by default), a cancelled job, or no `gitmoot_result` at all → the stage **fails**
  (re-attempted if it has retry budget), else the run **parks failed**.

`changes_requested` is a stage **failure by default** even though the underlying job
succeeded — a stage folds on the decision, not the job state. List it in
`success_decisions` (per-stage or top-level) to treat it as success instead.

`skipped` is a stage **success by default** for a task that had no work. Gitmoot
prefixes the persisted summary with `[skipped: no work]`, which downstream agent
stages receive as upstream context. An explicit `success_decisions` list is strict:
if it omits `skipped`, the stage fails because the author required real work. A
skipped result uses the existing succeeded stage state; the `SKIPPED` funnel state
still means a downstream stage never ran after the pipeline halted.

## Agent stages

A stage can run a **named managed gitmoot agent** instead of a shell command — a stage
is **exactly one** of `cmd`, `agent`, or `gate`. There are five agent-stage kinds:

| Kind | Declared by | What it does |
| ---- | ----------- | ------------ |
| **ask / review** (#757/#813) | `action: ask\|review` (review may add `source:`) | Read-only leaf; optionally reviews one upstream implement PR at its exact head. |
| **implement** (#768) | `action: implement` + `write: true` | Mutates the repo; `implemented` folds on PR-opened, while other configured successes settle without promising a PR. The implement job never merges. |
| **produce** (#814/#825) | `action: produce` + `write: true` + `writes:` | Sandboxed data writer: Codex, plus Claude/Kimi on Landlock-capable Linux; never creates repo/branch/task/PR state. |
| **orchestrate** (#758) | `orchestrate: true` | Sub-tree coordinator — fans out owned children, waits, folds the synthesis. |
| **gate** (#768) | `gate: pr_merged` + `source:` (no `agent`) | Jobless waiter — human merge by default; reviewed auto-merge is an explicit double-key opt-in. |

```yaml
stages:
  - id: extract
    cmd: "python fetch_replies.py > replies.json"
  - id: triage
    agent: reply-triager        # managed agent — create it before the pipeline runs
    action: ask                 # ask (default) | review | implement (+ write: true)
    prompt: "Triage the fetched replies and flag anything that needs a human."
    needs: [extract]
  - id: fix
    agent: fixer                # a MUTATING implement stage: opens a real PR
    action: implement
    write: true                 # required double-key; scheduled pipelines also need allow_scheduled_writes
    prompt: "Apply the approved change."
    needs: [triage]
  - id: wait
    gate: pr_merged             # jobless gate: waits for the fix stage's PR to merge
    source: fix
    needs: [fix]
```

For the first-class `implement -> review the PR -> wait for human merge` flow, bind
the review to the implement stage:

```yaml
  - id: review
    agent: reviewer
    action: review
    prompt: "Review the implementation PR."
    source: fix              # must also be in needs; must name an implement stage
    needs: [fix]
    success_decisions: [approved]
  - id: wait
    gate: pr_merged
    source: fix
    needs: [fix, review]
```

At enqueue, Gitmoot copies the source implement job's structured PR number, head
SHA, branch, task, and lead-agent stamp onto the review job. The read-only worktree
is detached at that exact head and checkout validation confirms it; this binding is
never inferred from summary text. Pipeline reviews are **report-only**: the verdict
still appears as a PR comment and folds through `success_decisions`, but
`changes_requested` does not dispatch a native fix and `approved` does not run the
native merge gate. Human merge remains required unless a separately double-keyed
`merge: auto` gate is present. Declaring the binding sets
`SkipNativeReviewFanout` on the implement request so Gitmoot does not also enqueue
native reviewers. If the source produced no PR (a no-op or any successful
non-`implemented` decision), the review
folds blocked immediately with `source stage produced no PR; nothing to review`.

### Opt-in auto-merge gates

Human merge is the unchanged default. A pipeline may instead let its jobless gate
perform the squash merge by declaring both keys and a source-bound robot review:

```yaml
allow_auto_merge: true
stages:
  - id: review
    agent: reviewer
    action: review
    prompt: "Review the implementation PR."
    source: fix
    needs: [fix]
    success_decisions: [approved]
  - id: merge
    gate: pr_merged
    merge: auto
    source: fix
    needs: [fix, review]
```

`merge: auto` is valid only on a gate and registration requires top-level
`allow_auto_merge: true` plus at least one review bound to the same implement
source. Every bound review in the spec must fold succeeded with decision
`approved`. The pipeline advancer—not the report-only review job—then verifies the
live PR head still equals the reviewed structured `HeadSHA`, waits for GitHub
mergeability and at least one external CI status/check, atomically claims the
write, and makes one squash attempt. Pending checks keep waiting within the gate
timeout. Check-run `skipped` and `neutral` count as passing, matching the native
merge gate; failures block. Zero external statuses/checks always block pipeline
auto-merge—even when `[merge_gate] require_external_ci` is false—so unattended
merge never synthesizes a no-CI success. Head drift, unmergeability/conflict, or a
merge API failure also folds the gate blocked; merge errors are not retried. A
scheduled auto-merge flow requires both `allow_auto_merge: true` and the existing
`allow_scheduled_writes: true`. The source job records an atomic
`pipeline_auto_merge_claim` before the write and `pipeline_auto_merge_confirmed`
after GitHub confirms it; racing scans that lose the claim do not call merge.

An agent stage runs the named agent on **its own registered runtime** (claude /
codex — no per-job shell override):

- **kinds** — `ask`/`review` are read-only **leaves** (`delegations[]` and
  `human_questions[]` are stripped, so no fan-out and no human-pause). `implement`
  mutates on a deterministic `gitmoot/pipe-<run>-<stage>` branch (retry reuses it),
  requires `write: true`, and never auto-merges. Only its `implemented` decision
  promises a PR and waits for the PR-opened stamp; other configured success decisions
  settle immediately. `orchestrate` is a
  coordinator (the one non-leaf): it fans out owned children, waits for the sub-tree
  via the continuation chain, then folds the tail. `gate` runs no job and folds when
  `pr_merged` holds on its `source` PR (parks `blocked` on close-unmerged, timeout,
  or any terminal succeeded source that opened no PR).
- **agent existence is warned, not blocked** — `pipeline add` warns for any referenced
  agent that does not exist yet but still adds the pipeline (so a spec can be bundled
  ahead of provisioning its agents); create the agent (`gitmoot agent …`) before the
  stage runs, or it fails loudly at run time.
- **upstream context is injected** — the stage prompt is prepended with a bounded,
  clearly-delimited **"Upstream stage results"** block carrying the result summary of
  each stage in its `needs`, so a downstream agent stage acts on upstream output as
  real dataflow (an over-long upstream summary is truncated with a `[truncated]`
  marker). Trigger payload context is injected separately for every agent stage,
  including roots; a root with no trigger payload still gets the bare prompt.
- **isolated read-only worktree** — a repo-bound ask/review agent stage runs in its
  own detached, committed-tip read-only worktree (#739), so same-repo agent stages
  parallelize and never touch the live checkout; the worktree is disposed when the
  stage job settles. A source-bound review is pinned to its payload `HeadSHA` and
  fails closed if that detached checkout cannot be allocated.

Agent stages fold by `gitmoot_result` `decision` and park/advance exactly like shell
stages — an `approved`/`implemented` review advances dependents, a `blocked` result
parks the run with its `needs`, `changes_requested` is a failure by default.

## Produce data without changing the repo

Use `produce` when the agent should write a dataset, report bundle, object-store
payload, or other operator-owned data rather than code:

```yaml
  - id: export
    agent: dataset-writer
    action: produce
    write: true
    writes: [/srv/datasets/nightly]
    network: true
    check: test -s /srv/datasets/nightly/index.json
    check_retries: 2
    prompt: Reconcile and atomically replace tonight's export.
```

Codex keeps its existing native sandbox. Claude and modern Kimi are also supported
on Linux when `gitmoot sandbox probe` reports supported: Gitmoot re-execs the runtime
under a strict Landlock ruleset before it starts. Non-Linux and unsupported kernels
keep the explicit Codex-only refusal; Kimi CLI and shell remain unsupported. Produce
also refuses read-only/auto agents. Each `writes:` entry must be absolute and cleaned.
At `pipeline add`, Gitmoot resolves symlinks and rejects `/`, its home, every managed
checkout, and paths containing any of those protected roots. The worker repeats the
same check immediately before delivery, so retargeting a previously-safe symlink
fails before the runtime starts.

The paths become additive runtime `--add-dir` arguments. For Claude/Kimi, Landlock
is the hard boundary: writes are limited to the declared existing directories, the
disposable workdir, `/tmp`/`$TMPDIR`, runtime-owned state, and standard CLI device
files; reads and execution remain broadly available for auth/config. Runtime state is
writable by design: Claude receives `$HOME/.claude` and the empirically used
`$XDG_CACHE_HOME/claude-cli-nodejs` cache, with
`CLAUDE_CONFIG_DIR=$HOME/.claude`; Kimi receives `$HOME/.kimi-code`. Apart from
those runtime-owned locations and device nodes, declared data paths, the disposable
workdir, and temp roots are the only writable filesystem locations. Codex remains unchanged: its
workspace-write defaults stay writable, and danger-full-access receives no
add-dir/network arguments. Produce runs from a disposable detached worktree and
carries no branch, task, or PR fields. Allocation fails closed instead of falling
back to the managed checkout.

Landlock does **not** govern network access in this feature. Network policy remains
the selected runtime CLI's responsibility.

After a valid result, `check` runs as trusted operator configuration in the stage
cwd with the daemon environment. On failure, redacted output capped at 8 KiB is
sent back to the same session for up to `check_retries` correction turns. Their
tokens are counted. Exhaustion fails the stage; ordinary stage retry can then
start a new attempt. Because earlier attempts may have written partial data,
produce work **must be idempotent** and reconcile or atomically overwrite. Gitmoot
never deletes declared data directories; it only cleans its disposable cwd.

Batch decisions keep the standard contract: `implemented` = complete,
`changes_requested` = partial (opt in via `success_decisions`), `blocked` = human
input required, and `skipped` = no work.

## Park and resume

The central story is **park-then-resume**. A run that hits a `blocked` stage parks
until an operator intervenes. `pipeline show <run-id>` renders the halt as a funnel
under a run header:

```
run: prun-nightly-sync-18bfa02e9afb86ed
pipeline: nightly-sync
trigger: manual
state: blocked
tokens: 12345 (best-effort)
halt_stage: score
halt_reason: secret missing
needs: R2 token

source OK -> score BLOCKED (needs: R2 token) -> deploy SKIPPED
```

Active runs also show queued/running stage details. The duration is labeled
`enqueued` because the stage timestamp is recorded at enqueue, not worker claim.
After a pipeline job has been delivering for 60 seconds, its worker updates one
latest-only `progress` event about every 30 seconds; `pipeline show` renders the
stored event's age and last sanitized activity line. The age keeps growing when
updates stop, so stale progress never masquerades as liveness. An orchestrate
stage can have no fresh per-stage event while its child sub-tree is active; this is
reported as `(sub-tree running; no per-stage progress)`, not a failure. JSON stage
objects add `started_at`, `finished_at`, and optional structured `progress` data.
The run token total is best-effort. JSON also carries run `tokens` and per-stage
`input_tokens`/`output_tokens` when captured; resumed-session edge cases and
runtimes without usage events contribute zero.

The operator provisions what the stage needs out of band (here, an R2 token), then
resumes — which re-runs the halted stage and everything downstream of it, while the
already-landed upstream stages are left untouched:

```sh
gitmoot pipeline resume "$RUN"                 # re-runs score + deploy; source is NOT re-run
gitmoot pipeline resume "$RUN" --from source   # re-run from an explicit stage instead
```

Resume works only on a **parked** (blocked or failed) run, resets the resume point and
its transitive dependents (bumping each one's attempt), never re-runs a succeeded
stage, and is refused if the pipeline's spec changed since the run was created.
Approval gates that resume a blocked run automatically are a follow-up — v1 ships the
manual `resume` verb.

A run that **fails** (a stage exhausted its retry budget) parks failed, and
`pipeline show` prints the exact command to file a bug for the halted stage's job —
gitmoot never files it for you:

```
stage failed; report it with:
  gitmoot report bug --job <stage-job-id>
```

`pipeline cancel <run-id>` abandons a running or parked run, cancelling its in-flight
stage jobs through the shared `job cancel` path and marking the run and its
non-terminal stages `cancelled`; an already-settled stage keeps its recorded outcome.

## Scheduling

An enabled pipeline with a `schedule.interval` auto-runs on that interval, using the
same durable-`next_due` idiom as heartbeats. The daemon's pipeline scan runs in both
the registered-repo and single-repo daemon loops:

- **Interval + jitter only** — there is no cron parser in v1 (a durable `next_due`
  makes a cron front-end a later drop-in).
- **One active run per pipeline** — a scheduled tick that finds a run in flight is
  skipped without advancing `next_due`, so the next run fires as soon as the current
  one settles. A parked run does **not** count as active.
- **Missed ticks coalesce** — a long-idle scheduler fires exactly one run and
  schedules the next one interval out, never a backlog replay.
- **Restart-safe** — the advancer recovers purely from the persisted run/stage rows,
  so a daemon restart mid-run picks the run back up and completes it.
- **Repo required to run** — a scheduled pipeline with no `repo` is skipped and its
  `next_due` advanced, so a misconfigured schedule does not hot-loop and self-recovers
  once a repo is set.

## Stages are leaves

A pipeline is **not** an orchestra. Each stage is a leaf: it runs a shell command (or
a read-only [agent](#agent-stages)) and
returns a decision, full stop. A stage result that carries `delegations[]` does **not**
spawn children — the advancer ignores them and the engine strips them for a pipeline
stage job, so a pipeline can never fan out into a delegation tree. Use an orchestra
when you want dynamic decomposition; use a pipeline when the steps and their
dependencies are known up front.

## Safety

`gitmoot pipeline add` is an **operator-trust action**: a stage's `cmd` runs verbatim
via `sh -c` with the daemon's permissions, with no sandbox or policy gate. Only
register specs you would run yourself, and treat a spec from an untrusted source as
arbitrary code execution. The spec is stored verbatim, so do not embed secrets in a
stage command — provision them out of band and have the stage read them from the
environment, letting it return `blocked` until they are present.

See the in-repo reference at `docs/pipelines.md` for the full field reference.
