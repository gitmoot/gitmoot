package workflow

type TaskState string

const (
	TaskPlanned          TaskState = "planned"
	TaskImplementing     TaskState = "implementing"
	TaskPullRequestOpen  TaskState = "pr_open"
	TaskReviewing        TaskState = "reviewing"
	TaskChangesRequested TaskState = "changes_requested"
	TaskReadyToMerge     TaskState = "ready_to_merge"
	TaskMerged           TaskState = "merged"
	TaskSuperseded       TaskState = "superseded"
	TaskStranded         TaskState = "stranded"
	TaskBlocked          TaskState = "blocked"
	// TaskAwaitingHumanMerge parks an otherwise-ready pull request for a human
	// merge. It is intentionally distinct from TaskBlocked and TaskAwaitingHuman:
	// it is neither a quality failure nor a delegation pause, and it must not be
	// auto-reaped while the pull request remains open.
	TaskAwaitingHumanMerge TaskState = "awaiting_human_merge"
	TaskDismissed          TaskState = "dismissed"
	// TaskAwaitingHuman is the resumable pause state a task enters when a
	// delegation fails under the escalate_human failure_policy (#340). Unlike
	// TaskBlocked (terminal), it is a durable human-in-the-loop pause: the tree
	// enqueues no continuation and consumes no compute until an operator resumes
	// it via `/gitmoot resume <jobID> retry|continue|abort`.
	TaskAwaitingHuman TaskState = "awaiting_human"
)

// IsDisposedTaskState reports terminal disposal outcomes that ordinary workflow
// advancement must never resurrect. Dismissed retains its explicit recovery
// path; superseded and stranded are evidence/audit outcomes requiring a new task.
func IsDisposedTaskState(state string) bool {
	switch TaskState(state) {
	case TaskDismissed, TaskSuperseded, TaskStranded:
		return true
	default:
		return false
	}
}

// TaskEventMergedBlockRefused is the durable trace setTaskState leaves when it
// refuses to rewrite a `merged` task into `blocked`. It is an informational
// event: the task does not move, so FromState/ToState stay empty per the
// db.TaskEvent contract, and the refused destination is named in Reason.
const TaskEventMergedBlockRefused = "task_merged_block_refused"

// IsMergedToBlockedRegression reports the one task-state move that is never
// right: rewriting a task whose work already LANDED (`merged`) into `blocked`,
// the terminal quality-failure state.
//
// It exists because a dead DELEGATION CHILD reaches the parent's failure_policy
// long after the parent's pull request merged (#1673: the daemon sweep that
// terminates queued legs whose pull request is no longer open). The default
// block_parent policy ends in setTaskState(TaskBlocked), whose only other guard
// is IsDisposedTaskState — which does not list `merged`. Without this predicate
// the choice is to rewrite `merged` to `blocked` or to strand the coordinator,
// and both are wrong: a review leg that never ran must not undo the record that
// the change shipped.
//
// Deliberately scoped to this one edge rather than freezing `merged` outright,
// because legitimate paths DO leave `merged`: `gitmoot task resume-work` moves a
// merged task back to `implementing` (internal/cli/workflow.go:752), and the
// pull-request lifecycle re-enters pr_open/reviewing on a fresh cycle. Only the
// blocked direction is a regression, so only it is refused.
func IsMergedToBlockedRegression(from, to string) bool {
	return TaskState(from) == TaskMerged && TaskState(to) == TaskBlocked
}

type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobBlocked   JobState = "blocked"
	JobFailed    JobState = "failed"
	JobSucceeded JobState = "succeeded"
	JobCancelled JobState = "cancelled"
)

// IsSettledJobState and IsFinalJobState are the two canonical "is this job state
// over?" predicates (#632). They exist because the codebase previously carried
// four ad-hoc, duplicated predicates that split 2–2 on `blocked` — including two
// package-level functions that shared the name isTerminalJobState with OPPOSITE
// handling of `blocked` (a refactor hazard: moving code between packages silently
// flipped semantics). The two helpers make the `blocked` disagreement a single,
// documented decision rather than four scattered accidents.
//
// The deliberate split is: `blocked` is SETTLED but not FINAL.

// IsSettledJobState reports whether a job state is "settled" under BARRIER
// semantics: there is no point waiting on the job any longer. The settled set is
// succeeded, failed, blocked, and cancelled.
//
// `blocked` IS settled: a delegation/continuation barrier must not stall waiting
// on a blocked child, and a `job watch` should stop tailing it — nothing more
// will happen without external intervention. Use this predicate to answer "will
// anything more happen on its own?" (delegation barriers, watch loops).
//
// It is intentionally DISTINCT from IsFinalJobState, which excludes `blocked`
// because a blocked job can be resumed via RetryJob. See #632.
func IsSettledJobState(state string) bool {
	switch JobState(state) {
	case JobSucceeded, JobFailed, JobBlocked, JobCancelled:
		return true
	default:
		return false
	}
}

// IsFinalJobState reports whether a job state is "final" under RESUMABILITY
// semantics: the job has reached an end state from which it will not resume. The
// final set is succeeded, failed, and cancelled.
//
// `blocked` is deliberately EXCLUDED (#632): a blocked job (awaiting a
// permission/approval or an interactive answer) can be resumed via RetryJob, so
// it is settled (see IsSettledJobState) but NOT final. Callers that stamp an end
// time (dashboard EndedAt) or tear down live resources (cockpit root
// finalization) must use this predicate, never the settled one, or they would
// prematurely end a job that can still come back to life.
func IsFinalJobState(state string) bool {
	switch JobState(state) {
	case JobSucceeded, JobFailed, JobCancelled:
		return true
	default:
		return false
	}
}

type Task struct {
	ID     string
	Title  string
	State  TaskState
	Branch string
}
