package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	diskGuardRefusalEventKind  = "dispatch_refused_disk_guard"
	diskGuardLogRepeat         = 15 * time.Minute
	diskGuardRefusalLogKind    = "refusal"
	diskGuardEventErrorLogKind = "event_error"
)

type diskFilesystemUsage struct {
	TotalBytes uint64
	FreeBytes  uint64
}

type diskGuardEvaluation struct {
	Path        string
	Policy      config.DiskGuardPolicy
	Usage       diskFilesystemUsage
	FreePercent float64
	Err         error
}

func (e diskGuardEvaluation) allowsDispatch() bool {
	if !e.Policy.Enabled {
		return true
	}
	if e.Err != nil {
		return false
	}
	return e.Usage.FreeBytes >= e.Policy.MinFreeBytes &&
		e.FreePercent >= e.Policy.MinFreePercent
}

func (e diskGuardEvaluation) detail() string {
	if !e.Policy.Enabled {
		return fmt.Sprintf("path=%s disabled", e.Path)
	}
	floors := fmt.Sprintf("min_free_bytes=%d min_free_percent=%.2f", e.Policy.MinFreeBytes, e.Policy.MinFreePercent)
	if e.Err != nil {
		return fmt.Sprintf("path=%s free_bytes=unknown free_percent=unknown %s error=%v", e.Path, floors, e.Err)
	}
	return fmt.Sprintf(
		"path=%s free_bytes=%d free_percent=%.2f total_bytes=%d %s",
		e.Path,
		e.Usage.FreeBytes,
		e.FreePercent,
		e.Usage.TotalBytes,
		floors,
	)
}

func (e diskGuardEvaluation) refusalReason() string {
	if e.Err != nil {
		return "measurement_unavailable"
	}
	return "below_configured_floor"
}

func (e diskGuardEvaluation) refusalFingerprint() string {
	return fmt.Sprintf(
		"path=%s reason=%s min_free_bytes=%d min_free_percent=%.2f",
		e.Path,
		e.refusalReason(),
		e.Policy.MinFreeBytes,
		e.Policy.MinFreePercent,
	)
}

var measureDiskGuardFilesystem = statfsDiskGuardFilesystem

func statfsDiskGuardFilesystem(path string) (diskFilesystemUsage, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return diskFilesystemUsage{}, fmt.Errorf("filesystem path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return diskFilesystemUsage{}, fmt.Errorf("resolve filesystem path %q: %w", path, err)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(absolute, &stat); err != nil {
		return diskFilesystemUsage{}, fmt.Errorf("statfs %s: %w", absolute, err)
	}
	if stat.Bsize <= 0 {
		return diskFilesystemUsage{}, fmt.Errorf("statfs %s returned invalid block size %d", absolute, stat.Bsize)
	}
	blockSize := uint64(stat.Bsize)
	totalBlocks := uint64(stat.Blocks)
	freeBlocks := uint64(stat.Bavail)
	if totalBlocks == 0 {
		return diskFilesystemUsage{}, fmt.Errorf("statfs %s returned zero total blocks", absolute)
	}
	if freeBlocks > totalBlocks {
		return diskFilesystemUsage{}, fmt.Errorf("statfs %s returned available blocks greater than total blocks", absolute)
	}
	if totalBlocks > math.MaxUint64/blockSize || freeBlocks > math.MaxUint64/blockSize {
		return diskFilesystemUsage{}, fmt.Errorf("statfs %s byte count overflows uint64", absolute)
	}
	return diskFilesystemUsage{
		TotalBytes: totalBlocks * blockSize,
		FreeBytes:  freeBlocks * blockSize,
	}, nil
}

func evaluateConfiguredDiskGuard(paths config.Paths) diskGuardEvaluation {
	path := paths.Home
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	policy, err := config.LoadDiskGuardPolicy(paths)
	if err != nil {
		return diskGuardEvaluation{
			Path:   path,
			Policy: config.DefaultDiskGuardPolicy(),
			Err:    fmt.Errorf("load policy: %w", err),
		}
	}
	evaluation := diskGuardEvaluation{Path: path, Policy: policy}
	if !policy.Enabled {
		return evaluation
	}
	usage, err := measureDiskGuardFilesystem(path)
	if err != nil {
		evaluation.Err = err
		return evaluation
	}
	evaluation.Usage = usage
	evaluation.FreePercent = float64(usage.FreeBytes) / float64(usage.TotalBytes) * 100
	if math.IsNaN(evaluation.FreePercent) || math.IsInf(evaluation.FreePercent, 0) {
		evaluation.Err = fmt.Errorf("free-space percentage is not finite")
	}
	return evaluation
}

type diskGuardLogEntry struct {
	fingerprint string
	at          time.Time
}

var (
	diskGuardLogMu      sync.Mutex
	diskGuardLogEntries = map[string]diskGuardLogEntry{}
)

func diskGuardLogKey(kind, path string) string {
	return kind + "\x00" + path
}

func shouldLogDiskGuard(kind, path, fingerprint string, now time.Time) bool {
	diskGuardLogMu.Lock()
	defer diskGuardLogMu.Unlock()
	key := diskGuardLogKey(kind, path)
	previous, ok := diskGuardLogEntries[key]
	if ok && previous.fingerprint == fingerprint && now.Sub(previous.at) < diskGuardLogRepeat {
		return false
	}
	diskGuardLogEntries[key] = diskGuardLogEntry{fingerprint: fingerprint, at: now}
	return true
}

func shouldLogDiskGuardRefusal(path, fingerprint string, now time.Time) bool {
	return shouldLogDiskGuard(diskGuardRefusalLogKind, path, fingerprint, now)
}

func shouldLogDiskGuardEventError(path, fingerprint string, now time.Time) bool {
	return shouldLogDiskGuard(diskGuardEventErrorLogKind, path, fingerprint, now)
}

func clearDiskGuardRefusal(path string) {
	diskGuardLogMu.Lock()
	defer diskGuardLogMu.Unlock()
	delete(diskGuardLogEntries, diskGuardLogKey(diskGuardRefusalLogKind, path))
	delete(diskGuardLogEntries, diskGuardLogKey(diskGuardEventErrorLogKind, path))
}

func diskGuardPaths(worker jobWorker) (config.Paths, error) {
	if worker.Store == nil {
		return worker.configPaths()
	}
	storeHome := filepath.Dir(worker.Store.DatabasePath())
	if !worker.ConfigHomeExplicit && strings.TrimSpace(worker.ConfigHome) == "" {
		return config.Paths{
			Home:       storeHome,
			ConfigFile: filepath.Join(storeHome, config.ConfigName),
		}, nil
	}
	paths, err := worker.configPaths()
	if err != nil {
		return config.Paths{}, err
	}
	// The opened store is the authoritative resolved Gitmoot home. Measuring its
	// parent filesystem covers the database and the sibling worktree roots even
	// if a caller supplied a relative raw --home.
	paths.Home = storeHome
	return paths, nil
}

// diskGuardQueuedDispatch returns the queued jobs the disk guard lets dispatch:
// all of jobs while the disk is healthy; otherwise only the reviews that run on
// the remote backend (diskGuardRemoteReviews, opt-in), or none. It is called
// only by the normal queued-job dispatch listing. Daemon maintenance and
// reconciliation do not pass through this function, so future in-process
// reclaim remains able to free disk while agent dispatch is paused.
func diskGuardQueuedDispatch(ctx context.Context, worker jobWorker, jobs []db.Job, repoFilter, rootFilter string) []db.Job {
	matching := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if queuedJobMatchesRepo(job, repoFilter) && queuedJobMatchesSession(job, rootFilter) {
			matching = append(matching, job)
		}
	}
	if len(matching) == 0 {
		return jobs
	}

	paths, err := diskGuardPaths(worker)
	var evaluation diskGuardEvaluation
	if err != nil {
		evaluation = diskGuardEvaluation{
			Path:   strings.TrimSpace(worker.ConfigHome),
			Policy: config.DefaultDiskGuardPolicy(),
			Err:    fmt.Errorf("resolve Gitmoot home: %w", err),
		}
	} else {
		evaluation = evaluateConfiguredDiskGuard(paths)
	}
	if evaluation.allowsDispatch() {
		clearDiskGuardRefusal(evaluation.Path)
		return jobs
	}

	detail := evaluation.detail()
	var remote []db.Job
	if evaluation.Err == nil && evaluation.Policy.RemoteReviews {
		remote = diskGuardRemoteReviews(ctx, worker, matching, detail)
	}
	routed := make(map[string]bool, len(remote))
	for _, job := range remote {
		routed[job.ID] = true
	}
	fingerprint := evaluation.refusalFingerprint()
	now := time.Now()
	if shouldLogDiskGuardRefusal(evaluation.Path, fingerprint, now) {
		writeLine(worker.Stdout, "DISK GUARD REFUSED JOB DISPATCH: %s", detail)
	}
	failedEventWrites := 0
	var firstEventWriteErr error
	for _, job := range matching {
		if routed[job.ID] {
			continue
		}
		if err := worker.Store.AddJobEventIfAbsent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    diskGuardRefusalEventKind,
			Message: detail,
		}); err != nil {
			failedEventWrites++
			if firstEventWriteErr == nil {
				firstEventWriteErr = err
			}
		}
	}
	if failedEventWrites > 0 && shouldLogDiskGuardEventError(evaluation.Path, fingerprint, now) {
		writeLine(
			worker.Stdout,
			"DISK GUARD event writes failed: failed=%d matching_jobs=%d path=%s reason=%s first_error=%v",
			failedEventWrites,
			len(matching),
			evaluation.Path,
			evaluation.refusalReason(),
			firstEventWriteErr,
		)
	}
	return remote
}

const diskGuardRoutedRemoteEventKind = "disk_guard_routed_remote"

// diskGuardRemoteReviews picks, while the guard pauses local dispatch, the
// queued reviews that may run on the remote backend instead: reviews already
// set to remote, and reviews with no backend of their own that this pass
// switches to remote, as many as the [remote_exec] cost cap has room for. A
// review switched here is exactly one a requester could have sent with
// --exec-backend remote, so remote admission, the one-cloud-attempt rule and
// the cost cap still apply unchanged. An explicit local backend is respected.
func diskGuardRemoteReviews(ctx context.Context, worker jobWorker, jobs []db.Job, reason string) []db.Job {
	backend, cfg, err := daemonJobExecBackendFor(worker, string(execbackend.Remote), true)
	if err != nil || backend != execbackend.Remote {
		return nil
	}
	free, err := remoteSlotsFree(ctx, worker.Store, execBackendStoreCap(cfg.ExecBackendCost))
	if err != nil {
		return nil
	}
	var allowed []db.Job
	for _, job := range jobs {
		if !strings.EqualFold(strings.TrimSpace(job.Type), "review") {
			continue
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			continue
		}
		if name, present := payload.ExecBackendOverride(); present {
			if selected, err := execbackend.Parse(name); err == nil && selected == execbackend.Remote {
				allowed = append(allowed, job)
				free--
			}
			continue
		}
		if free <= 0 || !diskGuardCanRouteRemote(ctx, worker.Store, job, payload) {
			continue
		}
		payload.ExecBackend = string(execbackend.Remote)
		encoded, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		rerouted, err := worker.Store.RerouteQueuedJobPayload(ctx, job.ID, string(encoded), job.LifecycleGeneration, db.JobEvent{
			JobID:   job.ID,
			Kind:    diskGuardRoutedRemoteEventKind,
			Message: "disk guard paused local dispatch, so this review runs on the remote backend: " + reason,
		})
		if err != nil || !rerouted {
			continue
		}
		job.Payload = string(encoded)
		allowed = append(allowed, job)
		free--
	}
	return allowed
}

// diskGuardCanRouteRemote reports whether a local review can run remotely at
// all: an exact pull-request head, a registered agent whose runtime runs
// remotely, and no earlier cloud attempt (remote admission allows one).
func diskGuardCanRouteRemote(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload) bool {
	head := strings.ToLower(strings.TrimSpace(payload.HeadSHA))
	if payload.PullRequest <= 0 || head == "" || dispatchHeadSHAError(head) != nil || payload.Ephemeral != nil {
		return false
	}
	runtimeName := strings.TrimSpace(payload.RuntimeOverride)
	if runtimeName == "" {
		agent, err := store.GetAgent(ctx, job.Agent)
		if err != nil {
			return false
		}
		runtimeName = agent.Runtime
	}
	if !remoteCapableRuntime(runtimeName) {
		return false
	}
	attempts, err := store.ListExecBackendAttemptsForJob(ctx, job.ID)
	return err == nil && len(attempts) == 0
}

// remoteSlotsFree is how many more cloud attempts the cost cap admits now. It
// counts the attempts the cap already bills plus running remote jobs that have
// not reserved yet, so a pass cannot hand out a slot a just-started job is
// about to take.
func remoteSlotsFree(ctx context.Context, store *db.Store, cap db.ExecBackendCostCap) (int, error) {
	if !cap.Configured || cap.MaxReservedUSD <= 0 || cap.PerAttemptUSD <= 0 {
		return 0, nil
	}
	count, reserved, err := store.ExecBackendBillingLoad(ctx)
	if err != nil {
		return 0, err
	}
	billing, err := store.ExecBackendBillingJobIDs(ctx)
	if err != nil {
		return 0, err
	}
	running, err := store.ListJobsByState(ctx, string(workflow.JobRunning))
	if err != nil {
		return 0, err
	}
	for _, job := range running {
		if billing[job.ID] {
			continue
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			continue
		}
		if name, present := payload.ExecBackendOverride(); present {
			if selected, err := execbackend.Parse(name); err == nil && selected == execbackend.Remote {
				count++
				reserved += cap.PerAttemptUSD
			}
		}
	}
	free := int(math.Floor((cap.MaxReservedUSD-reserved)/cap.PerAttemptUSD + 1e-9))
	if cap.MaxConcurrent > 0 && cap.MaxConcurrent-count < free {
		free = cap.MaxConcurrent - count
	}
	return max(free, 0), nil
}

func daemonDiskGuardLine(paths config.Paths) string {
	evaluation := evaluateConfiguredDiskGuard(paths)
	switch {
	case !evaluation.Policy.Enabled:
		return "disk guard: disabled, " + evaluation.detail()
	case evaluation.allowsDispatch():
		return "disk guard: healthy, " + evaluation.detail()
	default:
		return "disk guard: UNHEALTHY, dispatch paused, " + evaluation.detail()
	}
}
