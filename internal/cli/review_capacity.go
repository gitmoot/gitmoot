package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const reviewCapacityInterval = 2 * time.Second

type reviewCapacityMeasurement struct {
	Device    uint64 `json:"device"`
	Path      string `json:"path"`
	Available uint64 `json:"available_bytes"`
	Reserve   uint64 `json:"reserve_bytes"`
}

// Remote source preparation still writes local controller state. The same hard
// floor applies; the warning band additionally forbids non-E2B preparation.
func reviewDiskGuardRefusal(evaluation diskGuardEvaluation, sourceOnly bool) error {
	if !evaluation.allowsDispatch() {
		return fmt.Errorf("disk guard refused review preparation: %s", evaluation.detail())
	}
	if !sourceOnly && evaluation.reviewsBelowThreshold() {
		return fmt.Errorf("review preparation requires E2B below %d available bytes; request it with --exec-backend remote (E2B) and an omp or shell runtime", evaluation.Policy.RemoteReviewsBelowBytes)
	}
	return nil
}

// Review admission uses the canonical reserve, not the cleanup-recovery latch.
// This is best-effort monitoring, not a quota or a promise about other writers.
//
// admitting distinguishes starting a review from monitoring one already
// running. Space thresholds (the 10GB floor, the canonical reserve, the 20GB
// warning band and Janitor's low-space states) decide only whether NEW work
// may start. Owner policy (#2466/#2479): do not terminate running work for low
// space. While running, only failed, unknown, stale or mismatched evidence
// (and, in the monitor, device or policy changes) stops a review. The one
// exception is the monitor's fallback when Janitor does not answer at all
// (janitorUnavailableLocalGuard, #2667): it stops a review only below the
// local [disk_guard] floor, where it used to stop it unconditionally.
func (a janitorAdmission) reviewMeasurements(ctx context.Context, store *db.Store, paths config.Paths, repo string, sourceOnly, admitting bool, checkouts ...string) (map[uint64]string, []reviewCapacityMeasurement, error) {
	if a.cfgErr != nil {
		return nil, nil, a.cfgErr
	}
	if !a.cfg.Enabled || (!sourceOnly && !a.cfg.ReviewFreshSpace) {
		return nil, nil, fmt.Errorf("fresh-space review admission is not enabled")
	}
	if a.readErr != nil {
		return nil, nil, a.readErr
	}
	evaluation := evaluateConfiguredDiskGuard(paths)
	if admitting {
		if err := reviewDiskGuardRefusal(evaluation, sourceOnly); err != nil {
			return nil, nil, err
		}
	} else if evaluation.Policy.Enabled && evaluation.Err != nil {
		return nil, nil, fmt.Errorf("disk guard measurement failed during review: %s", evaluation.detail())
	}
	devices, err := reviewHostDevices(ctx, store, paths, repo, sourceOnly, checkouts...)
	if err != nil {
		return nil, nil, err
	}
	homeDevice, err := janitorDeviceOf(paths.Home)
	if err != nil {
		return nil, nil, err
	}
	measurements := make([]reviewCapacityMeasurement, 0, len(devices))
	for device, path := range devices {
		var volume *janitorVolume
		for i := range a.snapshot.volumes {
			v := &a.snapshot.volumes[i]
			if v.hasDevice && v.device == device {
				if volume != nil {
					return nil, nil, fmt.Errorf("duplicate janitor capacity for device %d", device)
				}
				volume = v
			}
		}
		stateOK := janitorClearedStates[volume.stateOrEmpty()]
		if !admitting {
			// While running, known low-space pressure is not a reason to stop
			// (owner policy); unknown or unrecognised evidence still is.
			stateOK = stateOK || janitorPressureStates[volume.stateOrEmpty()]
		}
		if volume == nil || !volume.hasByteLimits || !volume.hasTarget || !stateOK {
			return nil, nil, fmt.Errorf("review device %d (%s) has unknown or unsafe canonical capacity", device, path)
		}
		usage, err := measureDiskGuardFilesystem(path)
		if err != nil {
			return nil, nil, err
		}
		currentDevice, err := janitorDeviceOf(path)
		if err != nil || currentDevice != device {
			return nil, nil, fmt.Errorf("review backing device changed at %s", path)
		}
		available := min(volume.available, usage.FreeBytes)
		measurements = append(measurements, reviewCapacityMeasurement{device, path, available, volume.reserve})
		if !admitting {
			continue
		}
		if available < volume.reserve {
			return nil, measurements, fmt.Errorf("review device %d (%s) has %d available bytes, requires at least canonical reserve %d", device, path, available, volume.reserve)
		}
		if !sourceOnly && available < volume.target {
			return nil, measurements, fmt.Errorf("review device %d (%s) has %d available bytes; E2B required below canonical target %d", device, path, available, volume.target)
		}
		if device == homeDevice {
			current := evaluation
			current.Usage.FreeBytes = available
			if current.Usage.TotalBytes > 0 {
				current.FreePercent = float64(available) / float64(current.Usage.TotalBytes) * 100
			}
			if err := reviewDiskGuardRefusal(current, sourceOnly); err != nil {
				return nil, measurements, err
			}
		}
	}
	if reason := a.expired(); reason != "" {
		return nil, nil, janitorSampleStaleError(reason)
	}
	return devices, measurements, nil
}

// reviewHostDevices resolves the devices a review writes to, keyed by st_dev.
// Remote source preparation writes control state and logs, not local Git.
func reviewHostDevices(ctx context.Context, store *db.Store, paths config.Paths, repo string, sourceOnly bool, checkouts ...string) (map[uint64]string, error) {
	hostPaths := []string{paths.Home}
	var root config.WorktreeRoot
	if !sourceOnly {
		var err error
		hostPaths, root, err = janitorHostPaths(ctx, store, paths, repo, checkouts...)
		if err != nil {
			return nil, err
		}
	}
	return janitorHostDevices(hostPaths, root)
}

// janitorSampleStaleError is a Janitor sample that expired before the check
// using it finished.
type janitorSampleStaleError string

func (e janitorSampleStaleError) Error() string { return string(e) }

// janitorUnavailableLocalGuardPhase is logged for each running-review check
// that Janitor did not answer and no fresh sample covered (#2667).
const janitorUnavailableLocalGuardPhase = "janitor_unavailable_local_guard"

// janitorUnavailableLocalGuard checks a RUNNING review when Janitor did not
// answer and no fresh sample remains (#2667). A slow Janitor says nothing about
// the disk, so it never stops a review on its own; gitmoot's own statfs of the
// configured [disk_guard] floor on the gitmoot home does, when that floor is
// crossed or cannot be measured. The backing devices are still resolved, so a
// path or device change still stops the review. detail is the local
// measurement for the capacity log.
func janitorUnavailableLocalGuard(ctx context.Context, store *db.Store, paths config.Paths, repo string, sourceOnly bool, checkouts ...string) (map[uint64]string, []reviewCapacityMeasurement, string, error) {
	devices, err := reviewHostDevices(ctx, store, paths, repo, sourceOnly, checkouts...)
	if err != nil {
		return nil, nil, "local disk guard not evaluated", err
	}
	evaluation := evaluateConfiguredDiskGuard(paths)
	detail := "local disk guard: " + evaluation.detail()
	var measurements []reviewCapacityMeasurement
	if evaluation.Policy.Enabled && evaluation.Err == nil {
		homeDevice, err := janitorDeviceOf(paths.Home)
		if err != nil {
			return nil, nil, detail, err
		}
		measurements = []reviewCapacityMeasurement{{homeDevice, evaluation.Path, evaluation.Usage.FreeBytes, evaluation.Policy.MinFreeBytes}}
	}
	if !evaluation.allowsDispatch() {
		return nil, measurements, detail, fmt.Errorf("local disk guard stopped the review while janitor did not answer: %s", evaluation.detail())
	}
	return devices, measurements, detail, nil
}

// The audit exists before a job row does and survives an unsuccessful dispatch.
// It records only scope and capacity, never prompts, credentials or source text.
func recordReviewCapacity(paths config.Paths, jobID, repo, head, phase, detail string, measurements []reviewCapacityMeasurement) error {
	entry := struct {
		At           time.Time                   `json:"at"`
		Job          string                      `json:"job"`
		Repo         string                      `json:"repo"`
		Head         string                      `json:"head"`
		Phase        string                      `json:"phase"`
		Detail       string                      `json:"detail,omitempty"`
		CadenceMS    int64                       `json:"cadence_ms"`
		Measurements []reviewCapacityMeasurement `json:"measurements,omitempty"`
	}{time.Now().UTC(), jobID, repo, head, phase, detail, reviewCapacityInterval.Milliseconds(), measurements}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(paths.Home, "review-capacity.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

// No global slot is introduced: existing scheduler/provider limits still govern
// concurrency. The caller must stop the monitor only after owned cleanup returns.
func startReviewCapacityMonitor(ctx context.Context, store *db.Store, paths config.Paths, repo, head, jobID string, sourceOnly bool, generation *int64, checkouts ...string) (context.Context, func() error, error) {
	cfg, err := config.LoadJanitorCapacity(paths)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Enabled || (!sourceOnly && !cfg.ReviewFreshSpace) {
		return ctx, func() error { return nil }, reviewDiskGuardRefusal(evaluateConfiguredDiskGuard(paths), sourceOnly)
	}
	ownsGeneration := generation != nil
	var ownedGeneration int64
	if ownsGeneration {
		ownedGeneration = *generation
	}
	// lastGood is the newest Janitor sample that was read successfully. A
	// status command that did not answer (a timeout under host load) reuses
	// it while it is still fresh; the measurements are still taken live
	// (#2500: one timeout at 21:19Z cancelled five healthy reviews at once).
	// Once no fresh sample remains, a read that still does not answer falls
	// back to the local [disk_guard] floor instead of stopping the review
	// (#2667: Janitor stayed slow past its 2-minute sample and healthy reviews
	// were cancelled). An answer that arrived and was rejected (another
	// policy, bad schema, stale producer) is evidence, never masked by the
	// cached sample or the local guard.
	var lastGood *janitorAdmission
	localGuard := func(unanswered error) (map[uint64]string, []reviewCapacityMeasurement, error) {
		devices, measurements, detail, err := janitorUnavailableLocalGuard(ctx, store, paths, repo, sourceOnly, checkouts...)
		_ = recordReviewCapacity(paths, jobID, repo, head, janitorUnavailableLocalGuardPhase, "janitor did not answer and no fresh sample remains ("+unanswered.Error()+"); "+detail, measurements)
		if err != nil {
			return nil, measurements, fmt.Errorf("%w (current janitor read failed: %v)", err, unanswered)
		}
		return devices, measurements, nil
	}
	check := func(admitting bool) (map[uint64]string, []reviewCapacityMeasurement, error) {
		current, err := config.LoadJanitorCapacity(paths)
		if err != nil {
			return nil, nil, err
		}
		if current != cfg {
			return nil, nil, fmt.Errorf("review capacity policy changed during execution")
		}
		a := loadJanitorAdmission(ctx, paths)
		// Admission never tolerates a read that did not answer.
		unanswered := a.cfgErr == nil && !admitting && errors.As(a.readErr, new(janitorStatusExecError))
		var reusedAfter error
		switch {
		case a.cfgErr == nil && a.readErr == nil:
			good := a
			lastGood = &good
		case unanswered && lastGood != nil && lastGood.expired() == "":
			_ = recordReviewCapacity(paths, jobID, repo, head, "janitor_read_failed", "using the last good sample until it is stale: "+a.readErr.Error(), nil)
			reusedAfter = a.readErr
			a = *lastGood
		case unanswered:
			return localGuard(a.readErr)
		}
		devices, measurements, err := a.reviewMeasurements(ctx, store, paths, repo, sourceOnly, admitting, checkouts...)
		if err != nil && reusedAfter != nil {
			if errors.As(err, new(janitorSampleStaleError)) {
				// The reused sample expired during this check.
				return localGuard(reusedAfter)
			}
			// Name the read failure that made the cached sample the evidence.
			err = fmt.Errorf("%w (current janitor read failed: %v)", err, reusedAfter)
		}
		return devices, measurements, err
	}
	devices, first, err := check(true)
	if err != nil {
		return nil, nil, err
	}
	if err := recordReviewCapacity(paths, jobID, repo, head, "monitor_ready", "best-effort; concurrent writers can cross reserve between checks", first); err != nil {
		return nil, nil, err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(reviewCapacityInterval)
		defer ticker.Stop()
		last := first
		for {
			select {
			case <-stop:
				done <- nil
				return
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-ticker.C:
				current, samples, err := check(false)
				if err == nil && !maps.Equal(current, devices) {
					err = fmt.Errorf("review backing paths changed during execution")
				}
				if samples != nil {
					last = samples
				}
				if err == nil {
					continue
				}
				// A dispatch owns only its context. A worker also owns the captured
				// lifecycle, never a retry that reused this job ID during cleanup.
				if ownsGeneration {
					err = errors.Join(err, cancelMonitoredReview(store, jobID, ownedGeneration, err.Error(), func() { cancel(err) }))
				} else {
					cancel(err)
				}
				_ = recordReviewCapacity(paths, jobID, repo, head, "cancel_requested", err.Error(), last)
				done <- err
				return
			}
		}
	}()
	return runCtx, func() error {
		close(stop)
		err := <-done
		stoppedErr := recordReviewCapacity(paths, jobID, repo, head, "monitor_stopped", "owned call and cleanup returned; cancellation request is not itself proof of remote destruction", nil)
		if err == nil {
			err = stoppedErr
		}
		cancel(nil)
		return err
	}, nil
}

// reviewCapacityCancelLockWait bounds how long a monitor cancel waits for the
// job lifecycle lock. Destructive cleanup holds it exclusively while it works
// on the sshfs worktrees; a 10s wait timed out there under load, and the run
// was then stopped with no cancel committed, stranding the job "running" with
// nothing executing it (#2500). The runtime keeps running while this waits.
const reviewCapacityCancelLockWait = 2 * time.Minute

func cancelMonitoredReview(store *db.Store, jobID string, generation int64, reason string, stopRuntime func()) error {
	// Only after the whole wait failed is the runtime stopped without a
	// committed cancel: capacity evidence is fail-closed, and the error is
	// recorded beside the cancel request.
	defer stopRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), reviewCapacityCancelLockWait)
	defer cancel()
	ctx, release, err := store.AcquireJobLifecycle(ctx)
	if err != nil {
		return fmt.Errorf("review capacity cancel not committed: job lifecycle lock not acquired within %s: %w", reviewCapacityCancelLockWait, err)
	}
	defer release()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.LifecycleGeneration != generation {
		return nil
	}
	switch job.State {
	case string(workflow.JobQueued), string(workflow.JobRunning), string(workflow.JobBlocked):
		// Prevent the stopped worker from recording failure before cancellation
		// is committed. The lifecycle lock also fences any successor attempt.
		stopRuntime()
		// Name the cause on the job itself: a bare "cancel requested" reads as a
		// person cancelling it.
		_ = store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: reviewCapacityCancelledEventKind, Message: "review capacity monitor cancelled this run: " + reason})
		_, err = workflow.CancelJob(ctx, store, jobID)
		return err
	default:
		return nil
	}
}

// reviewCapacityCancelledEventKind records why the capacity monitor cancelled
// a running review (failed or unknown capacity evidence, device change, policy
// change, or the local [disk_guard] floor crossed while Janitor does not
// answer; never Janitor's low-space states), so the job history never shows an
// unexplained cancel.
const reviewCapacityCancelledEventKind = "review_capacity_cancelled"

// janitorPressureStates are Janitor states that report KNOWN low space. They
// hold new admissions but never stop a running review (#2479); every other
// non-cleared state, measurement_unknown included, is unknown evidence.
var janitorPressureStates = map[string]bool{"critical": true, "no_safe_reclaim": true}

// stateOrEmpty is nil-safe so the state lookup can precede the nil check.
func (v *janitorVolume) stateOrEmpty() string {
	if v == nil {
		return ""
	}
	return v.state
}
