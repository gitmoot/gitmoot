package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
)

// daemonReloadableConfig holds the daemon's live, WARM-reloadable runtime settings
// (issue #577): the poll interval, the worker-pool size, and the scheduler mode.
// It exists so a running daemon can pick these up from a SIGHUP WITHOUT a
// `daemon restart` — a restart tears down in-flight supervision and re-inherits the
// launching shell's environment, dropping runtime auth (the #559 disaster).
//
// A single mutex guards all fields because they are shared between the SIGHUP reload
// goroutine (writer) and the supervisor/worker goroutines (readers, once per
// tick/cycle). The reload NEVER touches the process, its env, or in-flight jobs: the
// supervisor loops simply read the current snapshot each cycle and the worker pool is
// re-dispatched per tick, so a changed value takes effect on the next iteration while
// everything in flight completes untouched.
type daemonReloadableConfig struct {
	mu      sync.Mutex
	poll    time.Duration
	workers int
	usePool bool
}

func newDaemonReloadableConfig(poll time.Duration, workers int, usePool bool) *daemonReloadableConfig {
	return &daemonReloadableConfig{poll: poll, workers: workers, usePool: usePool}
}

// snapshot returns the current live settings atomically. Supervisor loops call it
// each cycle (poll) and each worker tick (workers/usePool) so a warm reload is
// observed without any per-value locking at the call sites.
func (c *daemonReloadableConfig) snapshot() (poll time.Duration, workers int, usePool bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.poll, c.workers, c.usePool
}

// pollInterval returns just the live poll interval.
func (c *daemonReloadableConfig) pollInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.poll
}

func schedulerName(usePool bool) string {
	if usePool {
		return "pool"
	}
	return "barrier"
}

// applyStart merges a start-time [daemon] config into the live settings, applying a
// key ONLY when the operator did not pass the matching CLI flag (flag = override).
// This is what "read the config layer at start, keep CLI flags as the initial value /
// override" means: an explicit flag always wins at launch; the file fills in the rest.
// It returns a human-readable summary of what the file contributed (empty when it
// contributed nothing), so startup can log it without pretending a no-op happened.
func (c *daemonReloadableConfig) applyStart(next config.DaemonRuntimeConfig, explicitPoll, explicitWorkers, explicitScheduler bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var applied []string
	if next.PollSet && !explicitPoll {
		c.poll = next.Poll
		applied = append(applied, "poll="+next.Poll.String())
	}
	if next.WorkersSet && !explicitWorkers {
		c.workers = next.Workers
		applied = append(applied, fmt.Sprintf("workers=%d", next.Workers))
	}
	if next.SchedulerSet && !explicitScheduler {
		c.usePool = next.Scheduler == "pool"
		applied = append(applied, "scheduler="+next.Scheduler)
	}
	return strings.Join(applied, ", ")
}

// apply merges a re-read [daemon] config into the live settings on SIGHUP. Only keys
// PRESENT in the file are applied; every other live value (including one set from a
// CLI flag at launch) is preserved, so an absent/empty [daemon] section reloads to a
// no-op. It returns a concise one-line summary of what was re-read and what actually
// changed (deliverable 3). The worker-pool size is applied LIVE because the pool is
// re-dispatched every tick (a safe resize): the summary calls that out so the operator
// knows a worker-count change took effect without a restart and without disturbing
// in-flight jobs.
func (c *daemonReloadableConfig) apply(next config.DaemonRuntimeConfig) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var reread, changed []string
	if next.PollSet {
		reread = append(reread, "poll="+next.Poll.String())
		if next.Poll != c.poll {
			changed = append(changed, fmt.Sprintf("poll %s->%s", c.poll, next.Poll))
			c.poll = next.Poll
		}
	}
	if next.WorkersSet {
		reread = append(reread, fmt.Sprintf("workers=%d", next.Workers))
		if next.Workers != c.workers {
			changed = append(changed, fmt.Sprintf("workers %d->%d (live pool resize; in-flight jobs unaffected)", c.workers, next.Workers))
			c.workers = next.Workers
		}
	}
	if next.SchedulerSet {
		usePool := next.Scheduler == "pool"
		reread = append(reread, "scheduler="+next.Scheduler)
		if usePool != c.usePool {
			changed = append(changed, fmt.Sprintf("scheduler %s->%s", schedulerName(c.usePool), schedulerName(usePool)))
			c.usePool = usePool
		}
	}
	if len(reread) == 0 {
		return "no [daemon] settings found; nothing to reload"
	}
	summary := "re-read " + strings.Join(reread, ", ")
	if len(changed) == 0 {
		return summary + "; no changes"
	}
	return summary + "; changed " + strings.Join(changed, ", ")
}

// reloadDaemonConfig re-reads the [daemon] config section and applies it to the live
// supervisor settings, logging a concise summary. A config read/parse error keeps the
// CURRENT live settings (a bad edit never disrupts a healthy daemon) and is logged.
// It never touches the process, its environment, or in-flight jobs.
func reloadDaemonConfig(paths config.Paths, live *daemonReloadableConfig, stdout io.Writer) {
	next, err := config.LoadDaemonRuntimeConfig(paths)
	if err != nil {
		writeLine(stdout, "daemon reload (SIGHUP): config error, keeping current settings: %v", err)
		return
	}
	writeLine(stdout, "daemon reload (SIGHUP): %s", live.apply(next))
}

// installDaemonReloadHandler wires SIGHUP to a warm config reload for the lifetime of
// ctx. The handler runs in its own goroutine and stops when ctx is cancelled (daemon
// shutdown), so it never outlives the run loop. SIGHUP is deliberately separate from
// the SIGINT/SIGTERM shutdown context: it must NOT cancel the daemon.
func installDaemonReloadHandler(ctx context.Context, paths config.Paths, live *daemonReloadableConfig, stdout io.Writer) {
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go func() {
		defer signal.Stop(hupCh)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hupCh:
				reloadDaemonConfig(paths, live, stdout)
			}
		}
	}()
}
