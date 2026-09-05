package cli

import (
	"context"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"sync"
	"testing"
	"time"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Types are moved WITH their methods.

// wedgeBlockingAdapter is a delivery adapter whose Deliver BLOCKS for the job
// named in blockJob until release is closed (or the job's ctx is cancelled),
// standing in for a hung/very-long runtime subprocess. Every other job returns a
// successful ask result immediately. It records delivery order and lets the test
// observe (a) that the blocking job is genuinely in flight and (b) whether a
// second job was delivered WHILE the first one was still blocked.
type wedgeBlockingAdapter struct {
	mu        sync.Mutex
	blockJob  string
	release   chan struct{}
	delivered []string
	blocked   bool
	output    string
	// ignoreCtx makes the blocking job deaf to context cancellation, so it stays
	// in flight while the scheduler pass DRAINS. Default false keeps every
	// existing test byte-identical; only a test that needs to observe the pass's
	// post-cancellation behaviour sets it, because a ctx-obeying job returns
	// immediately and leaves no drain window to observe.
	ignoreCtx bool
}

func newWedgeBlockingAdapter(blockJob string) *wedgeBlockingAdapter {
	return &wedgeBlockingAdapter{
		blockJob: blockJob,
		release:  make(chan struct{}),
		output:   poolSchedulerAskResult,
	}
}

func (a *wedgeBlockingAdapter) Name() string { return "wedge-fake" }

func (a *wedgeBlockingAdapter) Start(context.Context, runtime.StartRequest) (runtime.StartResult, error) {
	return runtime.StartResult{RuntimeRef: "550e8400-e29b-41d4-a716-446655440000"}, nil
}

func (a *wedgeBlockingAdapter) Validate(context.Context, runtime.Agent) error { return nil }

func (a *wedgeBlockingAdapter) Deliver(ctx context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	a.mu.Lock()
	a.delivered = append(a.delivered, job.ID)
	blocking := job.ID == a.blockJob
	if blocking {
		a.blocked = true
	}
	a.mu.Unlock()
	if blocking {
		if a.ignoreCtx {
			<-a.release
		} else {
			select {
			case <-a.release:
			case <-ctx.Done():
				a.mu.Lock()
				a.blocked = false
				a.mu.Unlock()
				return runtime.Result{}, ctx.Err()
			}
		}
		a.mu.Lock()
		a.blocked = false
		a.mu.Unlock()
	}
	return runtime.Result{Raw: a.output}, nil
}

func (a *wedgeBlockingAdapter) Health(context.Context, runtime.Agent) error { return nil }

func (a *wedgeBlockingAdapter) Capabilities(context.Context) ([]string, error) { return nil, nil }

func (a *wedgeBlockingAdapter) stillBlocked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.blocked
}

func (a *wedgeBlockingAdapter) deliveredJobs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.delivered))
	copy(out, a.delivered)
	return out
}

// waitForJobState polls the store until jobID reaches wantState or the deadline
// elapses, returning the last observed state.
func waitForJobState(t *testing.T, store *db.Store, jobID string, wantState string, deadline time.Duration) string {
	t.Helper()
	stop := time.Now().Add(deadline)
	last := ""
	for time.Now().Before(stop) {
		job, err := store.GetJob(context.Background(), jobID)
		if err == nil {
			last = job.State
			if last == wantState {
				return last
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return last
}

// waitForCondition polls fn until it returns true or the deadline elapses.
func waitForCondition(t *testing.T, deadline time.Duration, fn func() bool) bool {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}
