package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestForegroundReviewCannotPrepareLocallyInWarningBand(t *testing.T) {
	declareNoRealAdapterExec(t)
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	paths := config.PathsForHome(home)
	appendConfig(t, paths, "\n[disk_guard]\nmin_free_bytes = 10000000000\nmin_free_percent = 0\nremote_reviews = true\nremote_reviews_below_bytes = 20000000000\n")
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 500_000_000_000, FreeBytes: 15_000_000_000}, nil
	})
	before, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		Home: home, RepoFlag: "owner/repo", Agent: "reviewer", Action: "review",
		PullRequest: 12, HeadSHA: head, Branch: "feature/review", Instructions: "review this",
		ActingOrgRole: "joltra", NoFixTarget: true,
	})
	if err == nil {
		t.Fatal("foreground local review bypassed the E2B-only warning band")
	}
	after, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("refused local review created a delivery: before=%+v after=%+v", before, after)
	}
}

func TestLocalReviewMonitorStopsOnlyOwnedGeneration(t *testing.T) {
	for _, requeued := range []bool{false, true} {
		t.Run(fmt.Sprintf("successor_generation=%t", requeued), func(t *testing.T) {
			declareNoRealAdapterExec(t)
			home, store, head := reviewRouterHome(t)
			ctx := context.Background()
			output, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
				Home: home, RepoFlag: "owner/repo", Agent: "reviewer", Action: "review",
				PullRequest: 12, HeadSHA: head, Branch: "feature/review", Instructions: "review this",
				Background: true, ActingOrgRole: "joltra", NoFixTarget: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			job, err := store.GetJob(ctx, output.JobID)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := daemonJobPayload(job)
			if err != nil {
				t.Fatal(err)
			}
			if payload.WorktreePath == "" {
				t.Fatal("local review did not acquire its owned worktree")
			}
			paths := config.PathsForHome(home)
			appendConfig(t, paths, fmt.Sprintf("\n[janitor_capacity]\nenabled = true\nreview_admission = \"fresh_space\"\nexecutable = \"/usr/local/bin/janitor\"\npolicy_hash = %q\n", janitorTestPolicyHash))
			appendConfig(t, paths, "\n[disk_guard]\nmin_free_percent = 0\n")
			hostPaths, root, err := janitorHostPaths(ctx, store, paths, "owner/repo", payload.WorktreePath)
			if err != nil {
				t.Fatal(err)
			}
			devices, err := janitorHostDevices(hostPaths, root)
			if err != nil {
				t.Fatal(err)
			}
			// Low space no longer stops a running review (#2479); failed or
			// unknown capacity evidence still does, and is what this test uses.
			var broken atomic.Bool
			previous := runJanitorStatus
			runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) {
				if broken.Load() {
					return nil, errors.New("janitor status unavailable")
				}
				volumes := make([]string, 0, len(devices))
				for device := range devices {
					volumes = append(volumes, fmt.Sprintf(`{"filesystem_key":"device:%d","device":%d,"state":"cleanup_needed","admission_hold":true,"available_bytes":%d,"reserve_bytes":1048576,"target_bytes":1048576,"reason":"cleanup target held"}`, device, device, uint64(2<<30)))
				}
				return []byte(janitorStatusJSON(time.Now(), time.Minute, janitorTestPolicyHash, volumes...)), nil
			}
			t.Cleanup(func() { runJanitorStatus = previous })
			queueDiskGuardJob(t, ctx, store, "unrelated-review", "reviewer", "review", "")
			runner := &reviewCapacityBlockingRunner{started: make(chan struct{})}
			worker := defaultJobWorker(store, io.Discard, home)
			worker.ExecutionBackendFactory = nil
			worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
				return runtime.ShellAdapter{Dir: checkout, Runner: runner}, nil
			}
			worker.CommenterFactory = func(string) github.Client { return githubtest.NoopClient{} }
			runCtx, cancel := context.WithCancel(ctx)
			finished := make(chan struct{})
			var runErr error
			go func() { defer close(finished); runErr = worker.run(runCtx, job) }()
			defer func() { cancel(); <-finished }()
			select {
			case <-runner.started:
			case <-finished:
				t.Fatalf("worker stopped before review delivery: %v", runErr)
			case <-time.After(30 * time.Second):
				observed, _ := store.GetJob(ctx, job.ID)
				events, _ := store.ListJobEvents(ctx, job.ID)
				t.Fatalf("local review never reached delivery under cleanup-only hold: job=%+v events=%+v", observed, events)
			}
			if requeued {
				changed, err := store.TransitionJobState(ctx, job.ID, string(workflow.JobRunning), string(workflow.JobQueued))
				if err != nil || !changed {
					t.Fatalf("queue successor generation: changed=%t error=%v", changed, err)
				}
			}
			broken.Store(true)
			select {
			case <-finished:
			case <-time.After(8 * time.Second):
				t.Fatal("review kept running after its capacity evidence failed")
			}
			if runErr == nil || !strings.Contains(runErr.Error(), "janitor status unavailable") {
				t.Fatalf("monitor outcome = %v, want fail-closed cancellation", runErr)
			}
			ended, err := store.GetJob(ctx, job.ID)
			wantState := string(workflow.JobCancelled)
			if requeued {
				wantState = string(workflow.JobQueued)
			}
			if err != nil || ended.State != wantState {
				t.Fatalf("owned run = %+v, %v; want %s", ended, err, wantState)
			}
			other, err := store.GetJob(ctx, "unrelated-review")
			if err != nil || other.State != string(workflow.JobQueued) {
				t.Fatalf("unrelated review was affected: %+v %v", other, err)
			}
			// Cancellation does not authorize immediate deletion of retryable source.
			// Existing aged-cleanup policy owns the retained worktree, including when
			// a successor generation has already acquired the job.
			if _, err := os.Stat(payload.WorktreePath); err != nil {
				t.Fatalf("monitor removed retryable review source: %v", err)
			}
		})
	}
}

// Jarvis 380078/380099, owner disk policy: low space decides only where NEW
// reviews start. A local review admitted above 20GB keeps running when free
// space falls into the 10-20GB band and even below the 10GB floor, also while
// Janitor reports known pressure states ("do not blindly terminate running
// work"). Failed or unknown capacity evidence still stops it fail-closed, and
// the cancel names its reason on the job.
func TestLocalReviewMonitorNeverStopsRunningReviewForLowSpace(t *testing.T) {
	for _, tc := range []struct{ trigger, wantReason string }{
		{"statfs_failed", "statfs failed"},
		{"janitor_measurement_unknown", "unknown or unsafe canonical capacity"},
	} {
		t.Run(tc.trigger, func(t *testing.T) { runLowSpaceMonitorScenario(t, tc.trigger, tc.wantReason) })
	}
}

func runLowSpaceMonitorScenario(t *testing.T, trigger, wantReason string) {
	declareNoRealAdapterExec(t)
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	output, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		Home: home, RepoFlag: "owner/repo", Agent: "reviewer", Action: "review",
		PullRequest: 12, HeadSHA: head, Branch: "feature/review", Instructions: "review this",
		Background: true, ActingOrgRole: "joltra", NoFixTarget: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, output.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	paths := config.PathsForHome(home)
	appendConfig(t, paths, fmt.Sprintf("\n[janitor_capacity]\nenabled = true\nreview_admission = \"fresh_space\"\nexecutable = \"/usr/local/bin/janitor\"\npolicy_hash = %q\n", janitorTestPolicyHash))
	appendConfig(t, paths, "\n[disk_guard]\nmin_free_bytes = 10000000000\nmin_free_percent = 0\nremote_reviews = true\nremote_reviews_below_bytes = 20000000000\n")
	hostPaths, root, err := janitorHostPaths(ctx, store, paths, "owner/repo", payload.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := janitorHostDevices(hostPaths, root)
	if err != nil {
		t.Fatal(err)
	}
	var free atomic.Uint64
	free.Store(25_000_000_000)
	var broken atomic.Bool
	var state atomic.Value
	state.Store("target_recovered")
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		if broken.Load() {
			return diskFilesystemUsage{}, errors.New("statfs failed")
		}
		return diskFilesystemUsage{TotalBytes: 500_000_000_000, FreeBytes: free.Load()}, nil
	})
	previous := runJanitorStatus
	runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) {
		volumes := make([]string, 0, len(devices))
		for device := range devices {
			volumes = append(volumes, fmt.Sprintf(`{"filesystem_key":"device:%d","device":%d,"state":%q,"admission_hold":false,"available_bytes":%d,"reserve_bytes":10000000000,"target_bytes":20000000000,"reason":"measured"}`, device, device, state.Load().(string), free.Load()))
		}
		return []byte(janitorStatusJSON(time.Now(), time.Minute, janitorTestPolicyHash, volumes...)), nil
	}
	t.Cleanup(func() { runJanitorStatus = previous })
	runner := &reviewCapacityBlockingRunner{started: make(chan struct{})}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.ExecutionBackendFactory = nil
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		return runtime.ShellAdapter{Dir: checkout, Runner: runner}, nil
	}
	worker.CommenterFactory = func(string) github.Client { return githubtest.NoopClient{} }
	runCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	var runErr error
	go func() { defer close(finished); runErr = worker.run(runCtx, job) }()
	defer func() { cancel(); <-finished }()
	select {
	case <-runner.started:
	case <-finished:
		t.Fatalf("review did not start above the warning threshold: %v", runErr)
	case <-time.After(30 * time.Second):
		t.Fatal("review never reached delivery")
	}
	// Into the warning band, then below the hard floor while Janitor reports
	// known pressure: each for at least two monitor intervals, still running.
	for _, phase := range []struct {
		bytes uint64
		state string
	}{{15_000_000_000, "cleanup_needed"}, {5_000_000_000, "critical"}, {5_000_000_000, "no_safe_reclaim"}} {
		free.Store(phase.bytes)
		state.Store(phase.state)
		select {
		case <-finished:
			events, _ := store.ListJobEvents(ctx, job.ID)
			t.Fatalf("running local review was stopped at %d free bytes (%s): %v events=%+v", phase.bytes, phase.state, runErr, events)
		case <-time.After(2*reviewCapacityInterval + time.Second):
		}
	}
	// Failed or unknown evidence: fail closed, and say why.
	if trigger == "statfs_failed" {
		broken.Store(true)
	} else {
		state.Store("measurement_unknown")
	}
	select {
	case <-finished:
	case <-time.After(8 * time.Second):
		t.Fatalf("review kept running after %s", trigger)
	}
	ended, err := store.GetJob(ctx, job.ID)
	if err != nil || ended.State != string(workflow.JobCancelled) {
		t.Fatalf("review after %s = %+v, %v; want cancelled", trigger, ended, err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	reason := ""
	for _, event := range events {
		if event.Kind == reviewCapacityCancelledEventKind {
			reason = event.Message
		}
	}
	if reason == "" || !strings.Contains(reason, "review capacity monitor cancelled this run") || !strings.Contains(reason, wantReason) {
		t.Fatalf("cancel has no recorded reason: %+v", events)
	}
}

type reviewCapacityBlockingRunner struct {
	started chan struct{}
	once    sync.Once
}

func (r *reviewCapacityBlockingRunner) Run(ctx context.Context, _ string, _ string, _ ...string) (subprocess.Result, error) {
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	return subprocess.Result{}, ctx.Err()
}

func (r *reviewCapacityBlockingRunner) RunEnv(ctx context.Context, dir string, _ []string, name string, args ...string) (subprocess.Result, error) {
	return r.Run(ctx, dir, name, args...)
}

func (r *reviewCapacityBlockingRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}
