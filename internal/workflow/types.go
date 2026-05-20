package workflow

type TaskState string

const (
	TaskPlanned TaskState = "planned"
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
