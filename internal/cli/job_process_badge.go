package cli

import (
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type worktreeLivenessProbe func(path string) (live bool, known bool)

var jobWorktreeLiveness worktreeLivenessProbe = workflow.WorktreeLiveness

// jobWorktreeLivenessSnapshot takes one /proc snapshot for a whole listing;
// run list calls it at most once, and only when some row needs the probe.
var jobWorktreeLivenessSnapshot = func() worktreeLivenessProbe { return workflow.SnapshotWorktreeLiveness() }

// lazyWorktreeLiveness defers the snapshot until the first probe, so a listing
// with no settled worktree rows never reads /proc.
func lazyWorktreeLiveness(snapshot func() worktreeLivenessProbe) worktreeLivenessProbe {
	var probe worktreeLivenessProbe
	return func(path string) (bool, bool) {
		if probe == nil {
			probe = snapshot()
		}
		return probe(path)
	}
}

// deriveWorktreeProcessActive reports the passive process badge for a job.
// Non-terminal jobs short-circuit before payload decoding or the /proc probe:
// live worktree activity is expected while a job is still progressing.
func deriveWorktreeProcessActive(job db.Job, probe worktreeLivenessProbe) bool {
	if !workflow.IsSettledJobState(job.State) {
		return false
	}
	payload, err := jobListPayload(job)
	if err != nil {
		return false
	}
	return payloadWorktreeProcessActive(job.State, payload, probe)
}

// payloadWorktreeProcessActive is deriveWorktreeProcessActive for a caller that
// already decoded the payload (run list), so a row is decoded once, not twice.
func payloadWorktreeProcessActive(state string, payload workflow.JobPayload, probe worktreeLivenessProbe) bool {
	if !workflow.IsSettledJobState(state) {
		return false
	}
	path := strings.TrimSpace(payload.WorktreePath)
	if path == "" || probe == nil {
		return false
	}
	live, known := probe(path)
	return known && live
}
