package cli

import (
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type worktreeLivenessProbe func(path string) (live bool, known bool)

var jobWorktreeLiveness worktreeLivenessProbe = workflow.WorktreeLiveness

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
	path := strings.TrimSpace(payload.WorktreePath)
	if path == "" || probe == nil {
		return false
	}
	live, known := probe(path)
	return known && live
}
