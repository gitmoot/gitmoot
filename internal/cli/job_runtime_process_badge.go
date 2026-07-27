package cli

import (
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type runtimeProcessLivenessProbe func(pid int, identity string) (live bool, known bool)

var jobRuntimeProcessLiveness runtimeProcessLivenessProbe = workflow.RuntimeProcessLiveness

// deriveRuntimeProcessActive reports recorded runtime-process liveness only for
// running jobs. nil means unknown: there is no recorded PID/identity, the payload
// is unreadable, or the host cannot verify the process identity.
func deriveRuntimeProcessActive(job db.Job, probe runtimeProcessLivenessProbe) *bool {
	if job.State != string(workflow.JobRunning) || probe == nil {
		return nil
	}
	payload, err := jobListPayload(job)
	if err != nil || payload.RuntimePID <= 0 {
		return nil
	}
	live, known := probe(payload.RuntimePID, payload.RuntimePIDStartTime)
	if !known {
		return nil
	}
	return &live
}
