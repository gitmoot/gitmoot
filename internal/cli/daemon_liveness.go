package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type runtimePIDState int

const (
	runtimePIDUnknown runtimePIDState = iota
	runtimePIDLive
	runtimePIDDead
	runtimePIDIdentityMismatch
)

// daemonDeadRuntimeReapAfter gives the normal worker settlement path its
// bounded terminal-write window before recovery intervenes. A process proven
// dead cannot resume work, so it must not inherit the 30m/45m silence policy
// used for legacy rows with no process identity.
const daemonDeadRuntimeReapAfter = 30 * time.Second

type livenessLogSample struct {
	size             int64
	modified         time.Time
	observed         time.Time
	identityReported bool
}

// daemonLivenessSweep retains one prior transcript observation per candidate.
// A single stat can never turn temporary silence into a terminal verdict.
type daemonLivenessSweep struct {
	mu        sync.Mutex
	samples   map[string]livenessLogSample
	stat      func(string) (os.FileInfo, error)
	probe     func(int, string) runtimePIDState
	identity  func(int) string
	signal    func(int, syscall.Signal) error
	cancelJob func(string) bool
}

func newDaemonLivenessSweep() *daemonLivenessSweep {
	return &daemonLivenessSweep{
		samples:  make(map[string]livenessLogSample),
		stat:     os.Stat,
		probe:    probeRuntimePID,
		identity: workflow.RuntimeProcessIdentity,
		signal:   syscall.Kill,
	}
}

// probeRuntimePID combines kill(0) with the recorded /proc starttime. A live
// PID with a different starttime is a recycled PID, not proof the old runtime
// died safely; that ambiguity vetoes reaping and gets its own event.
func probeRuntimePID(pid int, recordedIdentity string) runtimePIDState {
	if pid <= 0 || strings.TrimSpace(recordedIdentity) == "" {
		return runtimePIDUnknown
	}
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return runtimePIDDead
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return runtimePIDUnknown
	}
	current := workflow.RuntimeProcessIdentity(pid)
	if current == "" {
		return runtimePIDUnknown
	}
	if current != strings.TrimSpace(recordedIdentity) {
		return runtimePIDIdentityMismatch
	}
	// A zombie still answers kill(0), but it cannot produce work. Its /proc
	// identity remains available until reaped, which lets the liveness verdict
	// safely kill any surviving members of its recorded process group.
	if runtimeProcessState(pid) == "Z" {
		return runtimePIDDead
	}
	return runtimePIDLive
}

func runtimeProcessState(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 {
		return ""
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func configuredDaemonQuietKillAfter(home string, stdout io.Writer) time.Duration {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return config.DefaultDaemonQuietKillAfter
	}
	cfg, err := config.LoadDaemonRuntimeConfig(paths)
	if err != nil {
		writeLine(stdout, "quiet_kill_after config invalid; using default %s: %v", config.DefaultDaemonQuietKillAfter, err)
		return config.DefaultDaemonQuietKillAfter
	}
	return cfg.QuietKillPolicy()
}

// sweepRunningJobLiveness is the recurring same-boot crash detector. Startup
// and foreign-boot recovery stay separate. Rows with a recorded dead PID use a
// short, two-sample confirmation window; legacy rows still require the original
// stale-age, byte-progress, and lease evidence.
func (s *daemonLivenessSweep) sweepRunningJobLiveness(ctx context.Context, store *db.Store, stdout io.Writer, paths config.Paths, now time.Time, staleAfter, quietAfter time.Duration, repoFilter, rootFilter string) error {
	if s == nil {
		return nil
	}
	candidateAge := staleAfter
	if candidateAge <= 0 || candidateAge > daemonDeadRuntimeReapAfter {
		candidateAge = daemonDeadRuntimeReapAfter
	}
	jobs, err := store.ListRunningJobsUpdatedBefore(ctx, now.Add(-candidateAge))
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if err := s.evaluate(ctx, store, stdout, paths, now, staleAfter, quietAfter, job); err != nil {
			return err
		}
	}
	pruneAfter := 4 * quietAfter
	if minimum := 4 * daemonDeadRuntimeReapAfter; pruneAfter < minimum {
		pruneAfter = minimum
	}
	s.prune(now, pruneAfter)
	return nil
}

func (s *daemonLivenessSweep) evaluate(ctx context.Context, store *db.Store, stdout io.Writer, paths config.Paths, now time.Time, staleAfter, quietAfter time.Duration, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return nil // malformed evidence degrades to unknown, never to reaped
	}
	info, err := s.stat(transcript.JobLogPath(paths.Logs, job.ID))
	if err != nil || info.IsDir() {
		s.forget(job.ID)
		return nil
	}

	legacy := payload.RuntimePID <= 0
	requiredAge := staleAfter
	requiredQuiet := quietAfter
	stableFor := daemonWorkerLoopInterval
	if legacy {
		requiredQuiet = 2 * quietAfter
	} else {
		switch s.probe(payload.RuntimePID, payload.RuntimePIDStartTime) {
		case runtimePIDLive:
			s.forget(job.ID)
			return nil // absolute veto: a quiet, thinking runtime remains live
		case runtimePIDIdentityMismatch:
			previous, _ := s.previousStable(job.ID, info, now, daemonWorkerLoopInterval)
			if !previous.identityReported {
				_ = store.AddJobEvent(ctx, db.JobEvent{
					JobID: job.ID,
					Kind:  "job_liveness_identity_mismatch",
					Message: fmt.Sprintf("identity mismatch: pgid recycle suspected, orphans leaked; runtime pid %d no longer has recorded starttime %s; no process-group signal sent; job left running",
						payload.RuntimePID, payload.RuntimePIDStartTime),
				})
				previous.identityReported = true
				s.remember(job.ID, previous)
			}
			return nil
		case runtimePIDUnknown:
			s.forget(job.ID)
			return nil
		case runtimePIDDead:
			requiredAge = daemonDeadRuntimeReapAfter
			requiredQuiet = daemonDeadRuntimeReapAfter
			stableFor = daemonDeadRuntimeReapAfter
		}
	}

	updatedAt := parseJobTimeMillis(job.UpdatedAt)
	if updatedAt == 0 {
		updatedAt = parseJobTimeMillis(job.CreatedAt)
	}
	if updatedAt == 0 || now.Sub(time.UnixMilli(updatedAt)) < requiredAge {
		s.forget(job.ID)
		return nil
	}

	_, stable := s.previousStable(job.ID, info, now, stableFor)
	if now.Sub(info.ModTime()) < requiredQuiet || !stable {
		return nil
	}

	if legacy {
		leaseHeld, leaseErr := runtimeOwnerLeaseHeld(ctx, store, job.ID, now)
		if leaseErr != nil {
			return leaseErr
		}
		if leaseHeld {
			return nil
		}
	}

	runtimePID := payload.RuntimePID
	runtimeStart := payload.RuntimePIDStartTime
	runtimePGID := payload.RuntimePGID
	processLeg := "legacy row has no runtime PID and holds no runtime lease"
	if !legacy {
		processLeg = fmt.Sprintf("runtime pid %d is dead (recorded starttime %s) across two samples for %s", runtimePID, runtimeStart, stableFor)
	}
	message := fmt.Sprintf("liveness predicate satisfied: updated_at frozen past %s; job log byte-frozen for %s across two samples; %s", requiredAge, requiredQuiet, processLeg)
	transitioned, err := failRecoveredRunningJob(ctx, store, stdout, now, job, message)
	if err != nil {
		return err
	}
	if transitioned {
		if s.cancelJob != nil {
			s.cancelJob(job.ID)
		}
		if !legacy {
			s.killRecordedRuntimeGroup(ctx, store, job.ID, runtimePID, runtimePGID, runtimeStart)
		}
	}
	s.forget(job.ID)
	return nil
}

// killRecordedRuntimeGroup runs only after this sweep wins the running->failed
// transition. RuntimePIDStartTime belongs to the immediate child, which is also
// the group leader because GroupRunner uses Setpgid. Requiring both IDs to match
// and re-reading the leader identity immediately before kill(-pgid) prevents a
// recycled PGID from targeting an unrelated process tree.
func (s *daemonLivenessSweep) killRecordedRuntimeGroup(ctx context.Context, store *db.Store, jobID string, runtimePID, runtimePGID int, recordedIdentity string) {
	recordedIdentity = strings.TrimSpace(recordedIdentity)
	currentIdentity := ""
	if s.identity != nil && runtimePGID > 0 {
		currentIdentity = strings.TrimSpace(s.identity(runtimePGID))
	}
	if runtimePGID <= 0 || runtimePID != runtimePGID || recordedIdentity == "" || currentIdentity != recordedIdentity {
		_ = store.AddJobEvent(ctx, db.JobEvent{
			JobID: jobID,
			Kind:  "job_liveness_pgid_recycle_suspected",
			Message: fmt.Sprintf("pgid recycle suspected, orphans leaked: pid=%d pgid=%d recorded_starttime=%q current_leader_starttime=%q; no process-group signal sent",
				runtimePID, runtimePGID, recordedIdentity, currentIdentity),
		})
		return
	}
	if s.signal == nil {
		return
	}
	if err := s.signal(-runtimePGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = store.AddJobEvent(ctx, db.JobEvent{
			JobID: jobID,
			Kind:  "job_liveness_group_kill_failed",
			Message: fmt.Sprintf("process-group kill failed for pgid %d after verified leader identity: %v; orphans may remain",
				runtimePGID, err),
		})
	}
}

func (s *daemonLivenessSweep) previousStable(jobID string, info os.FileInfo, now time.Time, stableFor time.Duration) (livenessLogSample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, ok := s.samples[jobID]
	current := livenessLogSample{size: info.Size(), modified: info.ModTime(), observed: now, identityReported: previous.identityReported}
	unchanged := ok && previous.size == current.size && previous.modified.Equal(current.modified)
	if unchanged {
		current.observed = previous.observed
	}
	s.samples[jobID] = current
	return current, unchanged && now.Sub(previous.observed) >= stableFor
}

func (s *daemonLivenessSweep) remember(jobID string, sample livenessLogSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples[jobID] = sample
}

func (s *daemonLivenessSweep) forget(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.samples, jobID)
}

func (s *daemonLivenessSweep) prune(now time.Time, maxAge time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sample := range s.samples {
		if now.Sub(sample.observed) > maxAge {
			delete(s.samples, id)
		}
	}
}
