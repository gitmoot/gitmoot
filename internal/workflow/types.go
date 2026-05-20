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

type Task struct {
	ID     string
	Title  string
	State  TaskState
	Branch string
}
