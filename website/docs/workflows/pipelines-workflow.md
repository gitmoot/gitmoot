# Run A Fixed Multi-Step Flow (Pipelines)

Pipelines let the gitmoot daemon run a **declared DAG of shell stages** — a fixed,
repeatable multi-step flow with explicit dependencies — on demand or on an interval
schedule. Each stage is an ordinary queued job run through the **shell runtime**: the
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
schedule:                   # optional; auto-runs every interval once enabled
  interval: 24h             #   positive Go duration (required with a schedule block)
  jitter: 15m               #   optional random [0, jitter] added to each next_due
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
name/id, a duplicate stage id, a missing `cmd`, an unknown/self/cyclic `needs`, an
invalid duration, a negative retry, or a `success_decisions` value outside
`approved`/`implemented`/`changes_requested` — so a structural mistake is a clear
error at registration, not a stuck run later. It stores the raw YAML **verbatim**
plus a content hash; each run snapshots the hash and executes its snapshot, so
editing the file later never mutates an in-flight run.

## Manage pipelines

```sh
gitmoot pipeline list [--json]
gitmoot pipeline show <name> [--json]        # registry view for a pipeline name
gitmoot pipeline show <run-id> [--json]      # run funnel for a "prun-…" run id
gitmoot pipeline run <name>                  # start a manual run; prints the run id
gitmoot pipeline resume <run-id> [--from <stage>]
gitmoot pipeline cancel <run-id>
gitmoot pipeline enable <name>
gitmoot pipeline disable <name>
gitmoot pipeline remove <name>
```

A manual `pipeline run` ignores the `enabled` flag — a disabled pipeline can still be
run by hand — but still requires a `repo` and refuses to start while the pipeline
already has an active run (one active run per pipeline). `pipeline add` also
auto-creates one hidden shell runner agent per pipeline (`pipeline-<name>-runner`)
that owns the stage jobs; it is filtered out of `gitmoot agent list` and disposed by
`pipeline remove`.

## The stage contract

A stage command runs as `sh -c '<cmd>' gitmoot '<prompt>'`. It signals its outcome by
printing a `gitmoot_result` JSON blob to **stdout**; the advancer folds the stage by
that result's **`decision`**, never by the job's exit state:

```sh
# succeeds:
printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"synced"}}'
# parks the run awaiting a human, listing what it needs:
printf '%s' '{"gitmoot_result":{"decision":"blocked","summary":"secret missing","needs":["R2 token"]}}'
```

- a decision in the stage's `success_decisions` (default `["approved","implemented"]`)
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

## Park and resume

The central story is **park-then-resume**. A run that hits a `blocked` stage parks
until an operator intervenes. `pipeline show <run-id>` renders the halt as a funnel
under a run header:

```
run: prun-nightly-sync-18bfa02e9afb86ed
pipeline: nightly-sync
trigger: manual
state: blocked
halt_stage: score
halt_reason: secret missing
needs: R2 token

source OK -> score BLOCKED (needs: R2 token) -> deploy SKIPPED
```

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

A pipeline is **not** an orchestra. Each stage is a leaf: it runs a shell command and
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
