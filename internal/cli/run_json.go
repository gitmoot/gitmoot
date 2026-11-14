package cli

import "github.com/gitmoot/gitmoot/internal/db"

// CLI --json shapes for a run and its events (#2601). db.Job and db.JobEvent
// keep their stored/dashboard field names; these wrappers rename the
// user-facing keys by embedding the db struct and shadowing the old key with
// an always-empty omitempty field of the same Go (untagged) or JSON name.
// Every other key, payload and result content stays exactly as the db struct
// serializes it.

type runJSON struct {
	db.Job
	Reviewer    string
	ParentRunID string
	Campaign    string `json:"campaign,omitempty"`

	Agent       string `json:",omitempty"`
	ParentJobID string `json:",omitempty"`
	WorkflowID  string `json:"workflow_id,omitempty"`
}

func newRunJSON(job db.Job) runJSON {
	return runJSON{Job: job, Reviewer: job.Agent, ParentRunID: job.ParentJobID, Campaign: job.WorkflowID}
}

type runEventJSON struct {
	db.JobEvent
	RunID string

	JobID string `json:",omitempty"`
}

func newRunEventsJSON(events []db.JobEvent) []runEventJSON {
	out := make([]runEventJSON, len(events))
	for i, event := range events {
		out[i] = runEventJSON{JobEvent: event, RunID: event.JobID}
	}
	return out
}
