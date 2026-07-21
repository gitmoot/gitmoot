# CODEMAP — where the pipeline engine lives

Someone working on pipelines should land in the ~15k lines that matter, not
navigate the ~120k lines around them. This map names the engine seam.

## The engine: `internal/pipeline`

The pipeline engine — the scan loop / advancer, stage dispatch and worktree
isolation, trigger evaluation, and the expose/serve service layer — lives in
`internal/pipeline` (~11.6k non-test lines + spec/schema/state that were already
here). Start in:

- `pipeline_run.go` — the advancer (`AdvancePipelineRun`), the stage enqueuer
  (`NewPipelineStageEnqueuer`) and the three worktree-isolation allocation paths
  (service fail-closed, agent fail-open, non-service-shell opt-in fail-open),
  stage settle/fold, run creation (`CreatePipelineRun`), and the job-event-derived
  stage timestamps.
- `pipeline_trigger.go` — trigger evaluation.
- `pipeline_service*.go` — service admission / finalization / artifact collection.
- `pipeline_expose.go`, `pipeline_auto_merge.go`, `pipeline_resume.go` — the rest
  of the engine.
- `spec.go`, `validate.go`, `state.go`, `env.go`, `service_schema.go` — the types,
  validation, and state the engine operates on.

The command surface (`gitmoot pipeline <sub>` flag parsing, usage, presentation /
JSON / funnel rendering), the GitHub publish/pull transport, and the dashboard
receipt handlers stay in `internal/cli` — they are the caller, not the engine.

## The seam: what the engine needs

Stages are jobs, so the engine leans on the coordinator, not a parallel runtime:

- **`internal/workflow`** — the mailbox (enqueue/deliver), resource locks
  (checkout / runtime-session / branch), and read-only worktree allocation. Stage
  parallelism, resume, and receipts fall out of this coupling (see #1028's
  non-goal note on why the engine and coordinator are not split into separate
  repos).
- **`internal/db`** — the SQLite store: pipeline runs, run-stages, jobs, and the
  `job_events` stream (the truth for stage timing, #1016).
- **`internal/runtime`** — the shell/codex/claude/kimi adapters a stage job runs on.

## The cli ⇄ engine boundary (internal API, not a public contract)

`internal/pipeline` never imports `internal/cli` (no back-edge). cli reaches the
engine through an **internal** exported surface — mechanical import hygiene from
the #1028 move, not a designed API. It groups as:

- **~17 genuine glue** — the daemon (scan tick, enqueuer, progress, install-defaults,
  env-resolution, artifact cleanup hook), the bridge (create/advance run, run view),
  the dashboard (`PipelineProgressEventPayload` and display helpers), and skillopt
  (`PipelineContextFence`) calling engine entrypoints.
- **~12 command-entrypoint calls** — the `runPipeline*` command wiring that stays in
  cli but invokes the moved engine (`CreatePipelineRun`, `AdvancePipelineRun`,
  `NewPipelineStageEnqueuer`, `InstallDefaultMemoryPipelines`, `ResolvePipelineEnvironment`,
  `ValidatePipelineTriggerCycle`, …). These are candidates to narrow behind a
  thinner engine facade in the #1027 DAG-expressiveness era.
- The remainder are transitive types in exported signatures and methods on exported
  types (naturally exported, not trimmable).

Three `internal/cli/pipeline_*_compat.go` files hold delegation-only shims —
type aliases and one-line forwarders that keep old cli-package identifiers pointing
at the moved engine so call sites did not churn. They carry zero logic.
