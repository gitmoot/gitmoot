package cli

import (
	"context"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// stuckJobsStatus reads job payloads in the CLI layer and passes only inert
// liveness data into doctor. Store/probe failures omit the entire check rather
// than manufacturing a dead-process verdict.
func stuckJobsStatus(paths config.Paths) *doctor.StuckJobsStatus {
	if strings.TrimSpace(paths.Database) == "" {
		return nil
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return nil
	}
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		return nil
	}
	status := &doctor.StuckJobsStatus{}
	for _, job := range jobs {
		if job.State != string(workflow.JobRunning) {
			continue
		}
		payload, err := jobListPayload(job)
		if err != nil || payload.RuntimePID <= 0 {
			// Preserve unknown-as-neutral explicitly for legacy jobs.
			status.Jobs = append(status.Jobs, doctor.StuckJobProcess{JobID: job.ID})
			continue
		}
		live, known := workflow.RuntimeProcessLiveness(payload.RuntimePID, payload.RuntimePIDStartTime)
		status.Jobs = append(status.Jobs, doctor.StuckJobProcess{
			JobID: job.ID,
			PID:   payload.RuntimePID,
			Live:  live,
			Known: known,
		})
	}
	return status
}
