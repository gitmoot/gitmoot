package cli

import (
	"context"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// dashboardWorktreeRefreshInterval is how often the web dashboard recomputes the
// delegation-worktree usage block of /api/health. The inspection walks the
// worktree roots (an sshfs mount on the live host) and every job's payload, about
// 50 s per run on the #2639 store, so it runs in the background and a health
// response carries the last computed value and its age.
const dashboardWorktreeRefreshInterval = 5 * time.Minute

// dashboardWorktreeStopTimeout bounds how long shutdown waits for an
// inspection that is still walking the filesystem after its context ended.
const dashboardWorktreeStopTimeout = 5 * time.Second

const (
	dashboardWorktreesPending = "pending"
	dashboardWorktreesReady   = "ready"
)

// dashboardWorktreeSnapshot is the last completed worktree inspection.
type dashboardWorktreeSnapshot struct {
	usage      delegationWorktreeUsage
	computedAt time.Time
	// err is the last run's failure; a failed run keeps the previous usage.
	err string
}

// dashboardWorktreeRefresher owns the dashboard's background worktree
// inspection. One goroutine runs every inspection, so runs never overlap; the
// first starts immediately and the rest follow at interval. Readers only ever
// take the lock to copy the last snapshot, so a request never waits for a run.
type dashboardWorktreeRefresher struct {
	interval time.Duration
	inspect  func(context.Context) (delegationWorktreeUsage, error)

	mu       sync.Mutex
	snapshot dashboardWorktreeSnapshot
	started  bool
	cancel   context.CancelFunc
	done     chan struct{}
}

func newDashboardWorktreeRefresher(interval time.Duration, inspect func(context.Context) (delegationWorktreeUsage, error)) *dashboardWorktreeRefresher {
	return &dashboardWorktreeRefresher{interval: interval, inspect: inspect}
}

// start launches the refresh loop once; later calls are no-ops.
func (r *dashboardWorktreeRefresher) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.started = true
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.loop(ctx, r.done)
}

func (r *dashboardWorktreeRefresher) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		r.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *dashboardWorktreeRefresher) refresh(ctx context.Context) {
	usage, err := r.inspect(ctx)
	if ctx.Err() != nil {
		return // shutting down: a cancelled walk is not a result
	}
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.snapshot.err = err.Error()
		return
	}
	r.snapshot = dashboardWorktreeSnapshot{usage: usage, computedAt: now}
}

// stop cancels the loop and waits up to timeout for an in-flight inspection to
// return. It is safe to call on a refresher that never started.
func (r *dashboardWorktreeRefresher) stop(timeout time.Duration) {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (r *dashboardWorktreeRefresher) current() dashboardWorktreeSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot
}

// dashboardHealthWorktrees is the worktree part of a /api/health response.
type dashboardHealthWorktrees struct {
	usage      delegationWorktreeUsage
	status     string
	computedAt int64
	err        string
}

// worktreeHealth returns the last background inspection without waiting for
// one. Until the first run completes the usage is an explicit empty value with
// status "pending" and computedAt 0.
func (d *webDataSource) worktreeHealth() dashboardHealthWorktrees {
	var snap dashboardWorktreeSnapshot
	if d.worktrees != nil {
		snap = d.worktrees.current()
	}
	if snap.computedAt.IsZero() {
		return dashboardHealthWorktrees{
			usage: delegationWorktreeUsage{
				Roots:   []string{},
				Summary: "worktree usage: first background inspection has not finished",
			},
			status: dashboardWorktreesPending,
			err:    snap.err,
		}
	}
	usage := snap.usage
	if usage.Roots == nil {
		usage.Roots = []string{}
	}
	return dashboardHealthWorktrees{
		usage:      usage,
		status:     dashboardWorktreesReady,
		computedAt: snap.computedAt.UnixMilli(),
		err:        snap.err,
	}
}

// startWorktreeRefresher starts the dashboard-owned background inspection;
// stopWorktreeRefresher ends it on shutdown.
func (d *webDataSource) startWorktreeRefresher() {
	if d.worktrees == nil {
		d.worktrees = newDashboardWorktreeRefresher(dashboardWorktreeRefreshInterval, d.inspectWorktreeUsage)
	}
	d.worktrees.start()
}

func (d *webDataSource) stopWorktreeRefresher() {
	if d.worktrees != nil {
		d.worktrees.stop(dashboardWorktreeStopTimeout)
	}
}

// inspectWorktreeUsage is the production inspection: the same
// inspectDelegationWorktreeUsage `gitmoot doctor` runs synchronously.
func (d *webDataSource) inspectWorktreeUsage(ctx context.Context) (delegationWorktreeUsage, error) {
	paths, err := initializedPaths(d.home)
	if err != nil {
		return delegationWorktreeUsage{}, err
	}
	ttl, err := config.LoadDelegationWorktreeTTL(paths)
	if err != nil {
		return delegationWorktreeUsage{}, err
	}
	var usage delegationWorktreeUsage
	err = withStore(d.home, func(store *db.Store) error {
		var inspectErr error
		usage, inspectErr = inspectDelegationWorktreeUsage(ctx, paths, store, time.Now().UTC(), ttl)
		return inspectErr
	})
	return usage, err
}
