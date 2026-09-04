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

// TaskEventMergedRegressionRefused is the durable trace persistTaskStateOwned leaves
// when it refuses to overwrite a `merged` task with a state that asserts the
// work is not done. It is an informational event: the task does not move, so
// FromState/ToState stay empty per the db.TaskEvent contract, and the refused
// destination is named in Reason.
const TaskEventMergedRegressionRefused = "task_merged_regression_refused"

// IsMergedWorkRegressionTarget reports whether writing this task state over a
// task that is already `merged` would undo the record that the work LANDED.
//
// It exists because a dead DELEGATION CHILD reaches the parent's failure_policy
// long after the parent's pull request merged (#1673: the daemon sweep that
// terminates queued legs whose pull request is no longer open now hands each one
// to the child finalizer instead of cancelling it, so the parent's policy runs).
// setTaskState's only other guard is IsDisposedTaskState, which does not list
// `merged`. Without this predicate the choice is to let a leg that never ran
// rewrite the landed-work record or to strand the coordinator, and both are wrong.
//
// The rule is a TARGET-state test, not a from/to pair, because the from side is
// enforced by persistTaskStateOwned's conditional UPDATE. The "is it still merged?"
// question is answered by the statement that writes rather than by a pre-read
// another daemon can invalidate.
//
// Every TaskState, with its verdict for a task that is already `merged`:
//
//	planned              REFUSED. The two setTaskState sites that write it
//	                     (resumeEscalation, and the escalation TTL sweep) exist
//	                     only to CLEAR an awaiting_human pause, so they sit
//	                     directly downstream of the escalate_human refusal below —
//	                     the escalation round is recorded even when the pause is
//	                     refused, so resume/TTL still fires. Permitting `planned`
//	                     would hand the same dead child the same regression one
//	                     step later. Nothing legitimately moves landed work back
//	                     to a pre-work state.
//	implementing         PERMITTED. It is not a terminal failure-policy result.
//	                     The CLI resume-work command separately rejects `merged`;
//	                     this state-machine guard does not claim that CLI path.
//	pr_open              PERMITTED. A real pull request exists on the branch —
//	                     the fresh cycle resume-work started.
//	reviewing            PERMITTED. A real review is running on that real PR.
//	changes_requested    PERMITTED. A reviewer's verdict on a real PR is evidence,
//	                     not a leg that never ran; refusing it would silently drop
//	                     review feedback and strand the auto-fix cycle, and that
//	                     cycle itself re-reaches merged.
//	ready_to_merge       PERMITTED. Merge-gate verdict on an open PR.
//	awaiting_human_merge PERMITTED. Parks an OPEN, otherwise-ready PR for a human
//	                     merge; neither a quality failure nor a delegation pause.
//	merged               PERMITTED. Idempotent.
//	blocked              REFUSED. The default block_parent failure policy's
//	                     terminal state, and the original #1673 regression.
//	awaiting_human       REFUSED. The escalate_human failure policy's pause
//	                     (pauseAwaitingHuman, and the ask-round pause). Reachable
//	                     only BECAUSE the sweep now routes the queued child to the
//	                     finalizer, so it is this change's own new edge.
//	superseded           PERMITTED here, governed elsewhere: setTaskState has no
//	stranded             call site that writes any of the three, and
//	dismissed            IsDisposedTaskState guards moves OUT of them. They are
//	                     explicit operator/audit dispositions, not a dead leg's
//	                     opinion of whether the work landed.
func IsMergedWorkRegressionTarget(to string) bool {
	switch TaskState(to) {
	case TaskPlanned, TaskBlocked, TaskAwaitingHuman:
		return true
	default:
		return false
	}
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
// time or tear down live resources must use this predicate, never the settled
// one, or they would prematurely end a job that can still come back to life.
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
