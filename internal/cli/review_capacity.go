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

// Review admission uses the canonical reserve, not the cleanup-recovery latch.
// This is best-effort monitoring, not a quota or a promise about other writers.
func (a janitorAdmission) reviewMeasurements(ctx context.Context, store *db.Store, paths config.Paths, repo string, sourceOnly bool, checkouts ...string) (map[uint64]string, []reviewCapacityMeasurement, error) {
	if a.cfgErr != nil {
		return nil, nil, a.cfgErr
	}
	if !a.cfg.Enabled || (!sourceOnly && !a.cfg.ReviewFreshSpace) {
		return nil, nil, fmt.Errorf("fresh-space review admission is not enabled")
	}
	if a.readErr != nil {
		return nil, nil, a.readErr
	}
	// Remote source preparation writes control state and logs, not local Git.
	hostPaths := []string{paths.Home}
	var root config.WorktreeRoot
	if !sourceOnly {
		var err error
		hostPaths, root, err = janitorHostPaths(ctx, store, paths, repo, checkouts...)
		if err != nil {
			return nil, nil, err
		}
	}
	devices, err := janitorHostDevices(hostPaths, root)
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
		if volume == nil || !volume.hasByteLimits || !janitorClearedStates[volume.state] {
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
		if available <= volume.reserve {
			return nil, measurements, fmt.Errorf("review device %d (%s) has %d available bytes, requires strictly more than canonical reserve %d", device, path, available, volume.reserve)
		}
	}
	if reason := a.expired(); reason != "" {
		return nil, nil, fmt.Errorf("%s", reason)
	}
	return devices, measurements, nil
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
		return ctx, func() error { return nil }, nil
	}
	ownsGeneration := generation != nil
	var ownedGeneration int64
	if ownsGeneration {
		ownedGeneration = *generation
	}
	check := func() (map[uint64]string, []reviewCapacityMeasurement, error) {
		current, err := config.LoadJanitorCapacity(paths)
		if err != nil {
			return nil, nil, err
		}
		if current != cfg {
			return nil, nil, fmt.Errorf("review capacity policy changed during execution")
		}
		a := loadJanitorAdmission(ctx, paths)
		return a.reviewMeasurements(ctx, store, paths, repo, sourceOnly, checkouts...)
	}
	devices, first, err := check()
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
				current, samples, err := check()
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
					err = errors.Join(err, cancelMonitoredReview(store, jobID, ownedGeneration, func() { cancel(err) }))
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

func cancelMonitoredReview(store *db.Store, jobID string, generation int64, stopRuntime func()) error {
	defer stopRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx, release, err := store.AcquireJobLifecycle(ctx)
	if err != nil {
		return err
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
		_, err = workflow.CancelJob(ctx, store, jobID)
		return err
	default:
		return nil
	}
}
