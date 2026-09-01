package workflow

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/db"
)

// resolutionEffectSink collects a resolution's durable database writes so they can be
// committed together. It is deliberately dumb: the verb logic that produces effects is
// UNCHANGED, it just lands here instead of in the store (#1673).
type resolutionEffectSink struct {
	jobs   []db.PreparedJob
	events []db.JobEvent
	// task is the intended task-state transition; taskSet distinguishes "no task move"
	// from "move to the zero state".
	task          db.Task
	taskForbidden []string
	taskSet       bool
	// blocked records a refused allocation. It becomes the transaction's ALTERNATIVE
	// outcome: the block commits under the fence and the receipt is withheld, so the
	// claim stays preserved and a crash-replay cannot double-block.
	blocked *BlockedError
	// preEffect records resources allocated OUTSIDE the transaction, under the fence.
	preEffectRepo      string
	preEffectBranch    string
	preEffectWorktree  string
	preEffectLockOwner string
}

// capturing reports whether this engine copy is collecting effects rather than writing.
func (e Engine) capturing() bool { return e.resolutionSink != nil }

// recordEffectEvent routes a resolution's job event into the sink, or writes it
// directly on an ordinary path.
func (e Engine) recordEffectEvent(ctx context.Context, event db.JobEvent) error {
	if e.capturing() {
		e.resolutionSink.events = append(e.resolutionSink.events, event)
		return nil
	}
	return e.Store.AddJobEvent(ctx, event)
}

// recordEffectPreAllocation notes the resources a pre-effect took, so a supersede or a
// Class I release can hand them back.
func (e Engine) recordEffectPreAllocation(repo string, branch string, worktree string, lockOwner string) {
	if !e.capturing() {
		return
	}
	e.resolutionSink.preEffectRepo = repo
	e.resolutionSink.preEffectBranch = branch
	e.resolutionSink.preEffectWorktree = worktree
	e.resolutionSink.preEffectLockOwner = lockOwner
}
