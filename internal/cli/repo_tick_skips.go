package cli

import "sync"

// repoTickSkipForceAfter bounds how many CONSECUTIVE sweeps a repo may lose the
// race for its own checkout lock before the worker tick waits for it instead of
// skipping (#2135).
//
// WHY THREE. The symmetric-acquisition fix makes a contended repo skip rather
// than block, which removes the cross-repo stall - but on its own it introduces a
// worse failure than the one it fixes: a repo whose poll is slow and whose tick
// always loses the race would be silently late FOREVER while the fleet looked
// healthy. Today's bug is at least visible, because every repo is late together.
//
// Three is chosen against the two cadences rather than picked: the worker loop
// ticks on the order of a second, the poller's own per-repo work is bounded at
// daemonPollTimeout (2m, daemon_scheduler.go), and a poll is only in progress for
// a fraction of any repo's poll interval. Losing three consecutive sweeps
// therefore means real contention rather than an unlucky sample, and forcing on
// the fourth costs one repo one bounded wait instead of an unbounded absence.
// Lower would force during ordinary contention and re-introduce the stall this
// fix removes; higher widens the window in which a starving repo is invisible.
const repoTickSkipForceAfter = 3

// repoTickSkips counts CONSECUTIVE worker-tick skips per repo. It is deliberately
// observable rather than internal: the question this fix must be able to answer
// in one query is "which repo is losing the race", and #2117's lesson is that an
// instrument whose absence is silent localises nothing. The count is logged on
// every skip with its threshold, so a starving repo names itself in the daemon
// log rather than requiring an evening of correlation.
var repoTickSkips = struct {
	mu     sync.Mutex
	counts map[string]int
}{counts: map[string]int{}}

// recordRepoTickSkip increments repo's consecutive-skip count and returns the new
// value, for logging.
func recordRepoTickSkip(repo string) int {
	repoTickSkips.mu.Lock()
	defer repoTickSkips.mu.Unlock()
	repoTickSkips.counts[repo]++
	return repoTickSkips.counts[repo]
}

// clearRepoTickSkip resets repo's streak. Called when the tick RUNS for that repo,
// whether it acquired the lock immediately or by forcing, so the counter measures
// consecutive misses rather than lifetime misses.
func clearRepoTickSkip(repo string) {
	repoTickSkips.mu.Lock()
	defer repoTickSkips.mu.Unlock()
	delete(repoTickSkips.counts, repo)
}

// skippedRepoTickNeedsLock reports whether repo has already lost
// repoTickSkipForceAfter consecutive sweeps and must therefore WAIT for its lock
// on this one. It is read-only: the streak is cleared by clearRepoTickSkip once
// the tick actually runs, so a forced acquisition that then runs resets the
// counter exactly like an uncontended one.
func skippedRepoTickNeedsLock(repo string) bool {
	repoTickSkips.mu.Lock()
	defer repoTickSkips.mu.Unlock()
	return repoTickSkips.counts[repo] >= repoTickSkipForceAfter
}
