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
	// blockKind and blockFromState carry the task_event the block owns, so the event
	// and the state transition commit together rather than as two writes.
	taskEvent      db.TaskEvent
	taskEventValid bool
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
