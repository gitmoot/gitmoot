package cli

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/execbackend"
)

// execBackendReconcileBaseInterval is the steady-state cadence for the periodic
// off-box reconciliation pass (#1539). It is deliberately far slower than the
// worker tick: a provider inventory read is a paid, rate-limited network call,
// and the pass exists to bound how long an orphan can hold state, not to detect
// one promptly. The provider-side hard TTL set at creation remains the primary
// fail-safe and is unaffected by this cadence.
const execBackendReconcileBaseInterval = 5 * time.Minute

// TWO POLARITIES IN ONE SUBSYSTEM, ON PURPOSE. Do not "fix" one to match the
// other; #1539 asks for both.
//
//   - AT BACKEND CONSTRUCTION (execbackend_lifecycle.go) a reap failure is
//     FAIL-CLOSED: it aborts construction and the dispatch that triggered it, so
//     a restarted daemon cannot provision before reconciling the previous
//     process's instances. Double-running a job against a live remote instance is
//     unrecoverable; refusing one dispatch is not.
//   - ON THE WORKER TICK (this file) a failure is LOG-AND-CONTINUE, matching
//     every other maintenance step in runDaemonWorkerTickTracked. A transient
//     provider read must not stop the daemon's other sweeps or its dispatch.
//     #1539: "back-off cadence (advance-on-success), log-and-continue (never
//     abort the tick)".
//
// The asymmetry is the point: one path guards against double-execution at the
// only moment it can happen, the other guards against a provider outage becoming
// a gitmoot outage.
type execBackendReconcileCadence struct {
	mu     sync.Mutex
	nextAt map[string]time.Time
	streak map[string]int
}

var execBackendReconcileState = &execBackendReconcileCadence{
	nextAt: map[string]time.Time{},
	streak: map[string]int{},
}

// due reports whether key's next attempt is owed at now. An unseen key is due
// immediately, so a freshly started daemon reconciles on its first tick rather
// than waiting out a full interval.
func (c *execBackendReconcileCadence) due(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	next, seen := c.nextAt[key]
	return !seen || !now.Before(next)
}

// succeeded resets the back-off: ADVANCE ON SUCCESS. A provider that recovers
// returns to the steady cadence immediately instead of serving out a penalty it
// no longer deserves.
func (c *execBackendReconcileCadence) succeeded(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streak[key] = 0
	c.nextAt[key] = now.Add(execBackendReconcileBaseInterval)
}

// failed lengthens the interval for key using the daemon's existing streak
// back-off, so a persistently failing provider is polled less rather than every
// tick. It returns the interval actually applied for logging.
func (c *execBackendReconcileCadence) failed(key string, now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streak[key]++
	interval := repoBackoffInterval(execBackendReconcileBaseInterval, c.streak[key])
	c.nextAt[key] = now.Add(interval)
	return interval
}

// reconcileExecBackendInventory runs one periodic bidirectional reconciliation
// pass when the configured backend exposes a provider inventory.
//
// It returns an error only so the caller can log it in the tick's existing
// idiom; the caller MUST NOT abort on it. A backend with no inventory (the local
// backend) is a no-op, not a failure: there is nothing off-box to reconcile.
func reconcileExecBackendInventory(ctx context.Context, worker jobWorker, stdout io.Writer, now time.Time) error {
	backend, cfg, err := daemonJobExecBackendFor(worker, "", false)
	if err != nil {
		return fmt.Errorf("resolve execution backend for reconciliation: %w", err)
	}
	// THE CADENCE KEY IS THE BACKEND, AND THAT IS DELIBERATE ON TWO AXES.
	//
	// Across REPOS: runDaemonWorkerTickTracked runs per repo, so several ticks
	// share one cadence entry - which is correct rather than starvation, because
	// reconciliation is provider-global by construction.
	// ListRecoverableExecBackendAttempts filters on `provider = ?` ALONE and the
	// execbackend_attempts table has NO repo column, so one pass already covers
	// every repo's attempts. Keying per repo would multiply identical provider
	// inventory reads by the repo count for no added coverage.
	//
	// Across PROVIDER TARGETS: the construction guard keys finer -
	// `backend|root` for local and `backend|baseURL|sha256(apiKey)` for remote -
	// because it also runs from non-daemon callers. Within one daemon those
	// dimensions are PROCESS CONSTANTS: RemoteExecConfig is loaded per home
	// (config.LoadRemoteExecConfig(paths)) and a daemon has one home, so its
	// local root and its E2B base URL and key cannot vary between ticks. If a
	// daemon ever serves multiple homes or accounts, this key must gain those
	// dimensions or one target will suppress another's pass.
	key := string(backend)
	if !execBackendReconcileState.due(key, now) {
		return nil
	}
	if worker.ExecutionBackendFactory == nil {
		return nil
	}
	built, err := worker.ExecutionBackendFactory(backend, cfg)
	if err != nil {
		interval := execBackendReconcileState.failed(key, now)
		return fmt.Errorf("build execution backend %q for reconciliation (next attempt in %s): %w", backend, interval, err)
	}
	reaper, ok := built.(execbackend.InventoryReaper)
	if !ok {
		// No provider inventory to read. Record success so a local-only daemon
		// does not re-resolve the backend on every single tick.
		execBackendReconcileState.succeeded(key, now)
		return nil
	}
	report, err := reaper.ReapInventory(ctx)
	if err != nil {
		interval := execBackendReconcileState.failed(key, now)
		return fmt.Errorf("reconcile execution backend %q inventory (next attempt in %s): %w", backend, interval, err)
	}
	execBackendReconcileState.succeeded(key, now)
	if len(report.Destroyed) > 0 {
		writeLine(stdout, "execution backend reconciliation: destroyed %d orphaned instance(s) on %s", len(report.Destroyed), backend)
	}
	return nil
}
