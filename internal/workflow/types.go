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
	TaskBlocked          TaskState = "blocked"
	TaskDismissed        TaskState = "dismissed"
	// TaskAwaitingHuman is the resumable pause state a task enters when a
	// delegation fails under the escalate_human failure_policy (#340). Unlike
	// TaskBlocked (terminal), it is a durable human-in-the-loop pause: the tree
	// enqueues no continuation and consumes no compute until an operator resumes
	// it via `/gitmoot resume <jobID> retry|continue|abort`.
	TaskAwaitingHuman TaskState = "awaiting_human"
)

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
