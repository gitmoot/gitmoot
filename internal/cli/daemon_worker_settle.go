package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2618. A daemon worker can return while the row it claimed is still running:
// its terminal write timed out on a contended store, or it ran on a context the
// review capacity monitor had already cancelled. Nothing else settles such a
// row for hours (a remote job records no runtime PID, so liveness falls back to
// its multi-hour legacy rule), and `daemon drain` waits on it. Three layers
// close that:
//
//  1. workerExitGuard: run() fails its own claimed run on the way out, after
//     every cleanup defer, if the row is still that run.
//  2. jobWorker.recordUnrecordedResult: a result the runtime produced is
//     written again instead of being lost with the failed write.
//  3. orphanedClaimSweep: a running row this process claimed that no live
//     worker owns is failed once it has stayed that way for a grace period. It
//     covers the guard's own write failing on the same contended store.

// workerExitSettleTimeout bounds each detached settle write. It matches the
// review capacity monitor's cancel write: long enough to outlast ordinary
// store contention, short enough that a stuck store cannot pin the worker.
const workerExitSettleTimeout = 2 * time.Minute

// orphanedClaimGrace is how long a claimed running row must have no live
// worker before the sweep fails it.
const orphanedClaimGrace = 5 * time.Minute

// orphanedClaimSweepInterval throttles the sweep's query: the multi-repo
// supervisor runs one tracked tick per repo.
const orphanedClaimSweepInterval = 15 * time.Second

// liveJobWorkers counts, per job ID, the run() frames active in this process.
// Every in-daemon queued->running claim happens inside one of them, so a
// running row claimed by this process with no entry here has no worker.
var liveJobWorkers = jobWorkerRegistry{counts: map[string]int{}}

type jobWorkerRegistry struct {
	mu     sync.Mutex
	counts map[string]int
}

func (r *jobWorkerRegistry) enter(jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[jobID]++
}

func (r *jobWorkerRegistry) leave(jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[jobID] <= 1 {
		delete(r.counts, jobID)
		return
	}
	r.counts[jobID]--
}

func (r *jobWorkerRegistry) count(jobID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[jobID]
}

// workerExitGuard is one run()'s claim on its job. It is armed only by a
// successful queued->running claim, with that claim's exact lifecycle
// generation: remote review admission claims itself, and Mailbox.Run reports
// its claim through the observer run() installs (workflow.WithClaimObserver).
type workerExitGuard struct {
	jobID      string
	armed      bool
	generation int64
}

// beginWorkerExitGuard registers a live worker for jobID. The caller must
// defer settleWorkerExit.
func beginWorkerExitGuard(jobID string) *workerExitGuard {
	liveJobWorkers.enter(jobID)
	return &workerExitGuard{jobID: jobID}
}

// claimed arms the guard for the run claimed at generation. Every claim starts
// a distinct generation (a re-queue bumps it), so this names one run.
func (g *workerExitGuard) claimed(jobID string, generation int64) {
	if jobID != g.jobID {
		return
	}
	g.armed = true
	g.generation = generation
}

// settleWorkerExit fails the run this worker claimed if it is still running
// under this process's claim. It runs after every other defer in run(), on a
// context detached from the (possibly cancelled) run context.
//
// Only the claimed generation is ever settled, and the write is anchored to
// it: a row at any other generation is a different run (an operator retry
// claimed by another worker, say) and is not this worker's to fail, however
// recently it was read.
func (w jobWorker) settleWorkerExit(ctx context.Context, g *workerExitGuard, returnErr error, panicked any) {
	defer liveJobWorkers.leave(g.jobID)
	if !g.armed {
		return
	}
	cause := "worker returned without settling the job"
	switch {
	case panicked != nil:
		cause = fmt.Sprintf("worker panicked: %v", panicked)
	case returnErr != nil:
		cause = "worker exited on error: " + returnErr.Error()
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerExitSettleTimeout)
	defer cancel()
	job, err := w.Store.GetJob(settleCtx, g.jobID)
	if err != nil {
		writeLine(w.Stdout, "job %s exit settlement read failed (the orphaned-claim sweep will retry): %v", g.jobID, err)
		return
	}
	if job.State != string(workflow.JobRunning) || job.LifecycleGeneration != g.generation {
		return
	}
	if _, err := failClaimedRunningJob(settleCtx, w.Store, w.Stdout, time.Now().UTC(), job, g.generation, db.CurrentJobRunner(), "worker exited without settling its claimed run: "+cause); err != nil {
		writeLine(w.Stdout, "job %s exit settlement failed (the orphaned-claim sweep will retry): %v", g.jobID, err)
	}
}

// recordUnrecordedResult repeats a result write that failed inside the engine,
// on a context detached from the run. Once recorded, the run settles like any
// result whose advancement failed: an AdvanceError, which handleRunJobError
// turns into the advance-retry marker. Any other err is returned unchanged.
func (w jobWorker) recordUnrecordedResult(ctx context.Context, engine workflow.Engine, err error) error {
	var unrecorded *workflow.ResultNotRecordedError
	if !errors.As(err, &unrecorded) {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerExitSettleTimeout)
	defer cancel()
	if recordErr := engine.RecordUnrecordedResult(writeCtx, unrecorded); recordErr != nil {
		writeLine(w.Stdout, "job %s result still not recorded on retry: %v", unrecorded.JobID, recordErr)
		return err
	}
	writeLine(w.Stdout, "job %s %s result recorded on retry after its terminal write failed: %v", unrecorded.JobID, unrecorded.State, unrecorded.Err)
	return workflow.AdvanceError{Err: fmt.Errorf("result recorded on retry after its terminal write failed (%v); advancement did not run", unrecorded.Err)}
}

// orphanedClaimSweep fails running rows this process claimed that no live
// worker owns. A row must be seen orphaned at the same generation for
// orphanedClaimGrace before it is failed, and the write is anchored to that
// generation and this process's claim.
type orphanedClaimSweep struct {
	mu      sync.Mutex
	lastRun time.Time
	seen    map[string]orphanedClaim
}

type orphanedClaim struct {
	generation int64
	since      time.Time
}

func (s *orphanedClaimSweep) sweep(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time) {
	if s == nil {
		return
	}
	runner := db.CurrentJobRunner()
	// Off Linux the claim carries no boot ID, so a pid alone cannot tell this
	// process from an earlier one; leave those rows to age/lease recovery.
	if strings.TrimSpace(runner.BootID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !maintenancePassDue(s.lastRun, now, orphanedClaimSweepInterval) {
		return
	}
	s.lastRun = now
	jobs, err := store.ListRunningJobsClaimedBy(ctx, runner)
	if err != nil {
		writeLine(stdout, "orphaned-claim sweep failed: %v", err)
		return
	}
	orphaned := make(map[string]orphanedClaim, len(jobs))
	for _, job := range jobs {
		if liveJobWorkers.count(job.ID) > 0 {
			continue
		}
		claim, known := s.seen[job.ID]
		if !known || claim.generation != job.LifecycleGeneration {
			claim = orphanedClaim{generation: job.LifecycleGeneration, since: now}
		}
		if now.Sub(claim.since) < orphanedClaimGrace {
			orphaned[job.ID] = claim
			continue
		}
		// Anchored to the generation observed orphaned for the whole grace
		// period: a run claimed since then is a different run and never matches.
		if _, err := failClaimedRunningJob(ctx, store, stdout, now, job, claim.generation, runner,
			fmt.Sprintf("no live worker in this daemon has owned the running claim for %s", now.Sub(claim.since).Round(time.Second))); err != nil {
			writeLine(stdout, "orphaned-claim sweep: job %s settle failed: %v", job.ID, err)
			orphaned[job.ID] = claim
		}
	}
	s.seen = orphaned
}
