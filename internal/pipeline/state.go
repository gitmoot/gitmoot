package pipeline

// The run/stage state vocabulary the #681 advancer drives. They are stored
// verbatim in pipeline_runs.state and pipeline_run_stages.state (raw strings; the
// DB does not enum them) and interpreted by the scan-based advancer + the text
// funnel. Kept in this leaf package so the advancer, the CLI funnel, and tests
// share one source of truth without importing the engine.

// Run states. A run is RunRunning until nothing is in flight, then settles to
// exactly one terminal state: RunSucceeded (every stage succeeded or skipped),
// RunBlocked (a stage returned decision "blocked" — parked, needs persisted),
// RunFailed (a stage failed past its retry budget — parked, halt_reason set), or
// RunCancelled (operator cancel; the cancel/resume CLI lands in a later step).
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunBlocked   = "blocked"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// Stage states. A stage starts StagePending, becomes StageQueued when the
// advancer enqueues its job, StageRunning while that job runs, then settles by
// the job's gitmoot_result DECISION (never jobs.state — changes_requested is a
// SUCCEEDED job that must NOT advance): StageSucceeded (decision in the stage's
// success_decisions), StageBlocked (decision "blocked"), or StageFailed (decision
// "failed"/changes_requested/other, a job error, or a retry budget exhausted). A
// pending stage whose deps can never all succeed becomes StageSkipped.
// StageCancelled mirrors a cancelled stage job (later step).
const (
	StagePending   = "pending"
	StageQueued    = "queued"
	StageRunning   = "running"
	StageSucceeded = "succeeded"
	StageBlocked   = "blocked"
	StageFailed    = "failed"
	StageSkipped   = "skipped"
	StageCancelled = "cancelled"
)
