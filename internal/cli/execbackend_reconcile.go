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
// pass for the selected provider. A local-first daemon must still reconcile
// outstanding remote attempts after a restart, without waiting for another
// remote dispatch to construct the provider backend.
func reconcileExecBackendInventory(ctx context.Context, worker jobWorker, stdout io.Writer, now time.Time) error {
	backend, cfg, err := daemonJobExecBackendFor(worker, "", false)
	if err != nil {
		return fmt.Errorf("resolve execution backend for reconciliation: %w", err)
	}
	remoteKey := string(execbackend.Remote) + "|" + cfg.Provider
	if backend == execbackend.Local {
		if worker.Store == nil || !execBackendReconcileState.due(remoteKey, now) {
			return nil
		}
		attempts, err := worker.Store.ListRecoverableExecBackendAttempts(ctx, cfg.Provider)
		if err != nil {
			interval := execBackendReconcileState.failed(remoteKey, now)
			return fmt.Errorf("list remote execution attempts for reconciliation (next attempt in %s): %w", interval, err)
		}
		if len(attempts) == 0 {
			execBackendReconcileState.succeeded(remoteKey, now)
			return nil
		}
		backend, cfg, err = daemonJobExecBackendFor(worker, string(execbackend.Remote), true)
		if err != nil {
			interval := execBackendReconcileState.failed(remoteKey, now)
			return fmt.Errorf("resolve remote execution backend for reconciliation (next attempt in %s): %w", interval, err)
		}
	}
	// The cadence is scoped to backend and provider, not repo.
	// Across REPOS: runDaemonWorkerTickTracked runs per repo, so several ticks
	// share one cadence entry - which is correct rather than starvation, because
	// reconciliation is provider-global by construction.
	// ListRecoverableExecBackendAttempts filters on `provider = ?` ALONE and the
	// execbackend_attempts table has NO repo column, so one pass already covers
	// every repo's attempts. Keying per repo would multiply identical provider
	// inventory reads by the repo count for no added coverage.
	//
	// The construction guard keys the exact target (URL and API-key digest)
	// because non-daemon callers can build several targets. This daemon serves
	// one config home, so its provider target cannot vary across ticks. If one
	// daemon ever serves multiple homes/accounts, include that target identity
	// in the cadence key as well.
	key := string(backend) + "|" + cfg.Provider
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
