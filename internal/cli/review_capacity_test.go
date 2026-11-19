package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
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
		ActingOrgRole: "joltra",
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
				Background: true, ActingOrgRole: "joltra",
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
			// Low space no longer stops a running review (#2479), nor does a
			// Janitor that does not answer while the local floor holds (#2667);
			// an answer that arrived and was rejected still does, and is what
			// this test uses.
			var broken atomic.Bool
			previous := runJanitorStatus
			runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) {
				if broken.Load() {
					return []byte("janitor answered garbage"), nil
				}
				volumes := make([]string, 0, len(devices))
				for device := range devices {
					volumes = append(volumes, fmt.Sprintf(`{"filesystem_key":"device:%d","device":%d,"state":"cleanup_needed","admission_hold":true,"available_bytes":%d,"reserve_bytes":1048576,"target_bytes":1048576,"reason":"cleanup target held"}`, device, device, uint64(2<<30)))
				}
				return []byte(janitorStatusJSON(time.Now(), 4*time.Second, janitorTestPolicyHash, volumes...)), nil
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
			hideSeatStagingSources(t)
			runCtx, cancel := context.WithCancel(ctx)
			finished := make(chan struct{})
			var runErr error
			go func() { defer close(finished); runErr = worker.run(runCtx, job) }()
			defer func() { cancel(); <-finished }()
			select {
			case <-runner.started:
			case <-finished:
				t.Fatalf("worker stopped before review delivery: %v", runErr)
			case <-reviewCapacityTestEventWait(t):
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
			case <-reviewCapacityTestEventWait(t):
				t.Fatal("review kept running after its capacity evidence failed")
			}
			if runErr == nil || !strings.Contains(runErr.Error(), "janitor status is not valid JSON") {
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

// reviewCapacityTestEventWait bounds a wait for something that WILL happen
// (delivery starting, a fail-closed cancel, a capacity log row), so it only
// fails a hung run; it is not part of the behaviour. A fixed bound flaked on
// loaded hosts (delivery took over 143s at load 55-150, #2676), so the wait
// runs until shortly before the test binary's own deadline, leaving time to
// report the state and unwind the worker. The "still running" phases keep
// their exact monitor-interval sleeps.
func reviewCapacityTestEventWait(t *testing.T) <-chan time.Time {
	t.Helper()
	const unwindMargin = 30 * time.Second
	deadline, ok := t.Deadline()
	if !ok {
		// -timeout 0: nothing else bounds the run, so neither does this wait.
		return nil
	}
	return time.After(max(time.Until(deadline)-unwindMargin, time.Second))
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
		// #2500: one Janitor read timing out must not stop a running review
		// while the last good sample is fresh. #2667: once it is stale, reads
		// that keep failing fall back to the local [disk_guard] floor, which
		// stops the review here because free space is below it.
		{"janitor_read_failed", "local disk guard stopped the review while janitor did not answer"},
		// #2667: Janitor timing out long past its cached sample's validity
		// does not stop a review while the local floor holds; the fallback is
		// logged with its measurement. A failing local measurement still stops it.
		{"janitor_unavailable_above_floor", "statfs failed"},
		// #2667: a rejected answer after the local fallback took over still
		// fails closed, although the local floor holds.
		{"janitor_other_policy_after_fallback", "is not the approved policy"},
		// #2500: the cancel must commit even while destructive cleanup holds
		// the lifecycle lock longer than the old 10s wait; before, the run was
		// stopped with no cancel recorded and the job never ended cancelled.
		{"statfs_failed_while_cleanup_locked", "statfs failed"},
		// #2501 review: only a status command that did not answer may reuse
		// the cached sample. An answer for another policy is evidence and
		// stops the review although the cached sample is fresh for minutes.
		{"janitor_other_policy", "is not the approved policy"},
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
		Background: true, ActingOrgRole: "joltra",
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
	var janitorDown atomic.Bool
	var validity atomic.Int64
	validity.Store(int64(6 * time.Second))
	var policy atomic.Value
	policy.Store(janitorTestPolicyHash)
	runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) {
		if janitorDown.Load() {
			return nil, errors.New("janitor status unavailable")
		}
		volumes := make([]string, 0, len(devices))
		for device := range devices {
			volumes = append(volumes, fmt.Sprintf(`{"filesystem_key":"device:%d","device":%d,"state":%q,"admission_hold":false,"available_bytes":%d,"reserve_bytes":10000000000,"target_bytes":20000000000,"reason":"measured"}`, device, device, state.Load().(string), free.Load()))
		}
		return []byte(janitorStatusJSON(time.Now(), time.Duration(validity.Load()), policy.Load().(string), volumes...)), nil
	}
	t.Cleanup(func() { runJanitorStatus = previous })
	runner := &reviewCapacityBlockingRunner{started: make(chan struct{})}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.ExecutionBackendFactory = nil
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		return runtime.ShellAdapter{Dir: checkout, Runner: runner}, nil
	}
	worker.CommenterFactory = func(string) github.Client { return githubtest.NoopClient{} }
	hideSeatStagingSources(t)
	runCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	var runErr error
	go func() { defer close(finished); runErr = worker.run(runCtx, job) }()
	defer func() { cancel(); <-finished }()
	select {
	case <-runner.started:
	case <-finished:
		t.Fatalf("review did not start above the warning threshold: %v", runErr)
	case <-reviewCapacityTestEventWait(t):
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
	switch trigger {
	case "statfs_failed":
		broken.Store(true)
	case "janitor_other_policy":
		// Let a long-lived sample become the cached one, then answer for
		// another policy: the stop must not wait for that sample to expire,
		// nor depend on the local floor (free space is above it).
		free.Store(15_000_000_000)
		state.Store("cleanup_needed")
		validity.Store(int64(2 * time.Minute))
		time.Sleep(2*reviewCapacityInterval + time.Second)
		policy.Store(strings.Repeat("0", len(janitorTestPolicyHash)))
	case "janitor_unavailable_above_floor", "janitor_other_policy_after_fallback":
		free.Store(15_000_000_000)
		state.Store("cleanup_needed")
		janitorDown.Store(true)
		select {
		case <-finished:
			events, _ := store.ListJobEvents(ctx, job.ID)
			t.Fatalf("a Janitor that did not answer stopped the review above the local floor: %v events=%+v", runErr, events)
		case <-time.After(time.Duration(validity.Load()) + 3*reviewCapacityInterval + time.Second):
		}
		requireLocalGuardLogged(t, paths, 15_000_000_000)
		if trigger == "janitor_unavailable_above_floor" {
			broken.Store(true)
		} else {
			policy.Store(strings.Repeat("0", len(janitorTestPolicyHash)))
			janitorDown.Store(false)
		}
	case "statfs_failed_while_cleanup_locked":
		// Destructive cleanup holds the lifecycle lock exclusively for longer
		// than the old 10s cancel wait.
		_, releaseLock, err := store.AcquireCleanupLifecycle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		broken.Store(true)
		time.Sleep(12 * time.Second)
		releaseLock()
	case "janitor_read_failed":
		janitorDown.Store(true)
		time.Sleep(reviewCapacityInterval + reviewCapacityInterval/2)
		janitorDown.Store(false)
		select {
		case <-finished:
			events, _ := store.ListJobEvents(ctx, job.ID)
			t.Fatalf("one failed Janitor read stopped the review while its last sample was fresh: %v events=%+v", runErr, events)
		case <-time.After(2*reviewCapacityInterval + time.Second):
		}
		janitorDown.Store(true)
	default:
		state.Store("measurement_unknown")
	}
	select {
	case <-finished:
	case <-reviewCapacityTestEventWait(t):
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

// #2676: stopping the runtime makes the worker record the run failed at once,
// and the shared lifecycle lock the monitor holds does not hold that write
// back. The monitor therefore stops the runtime only after the cancel is
// committed; stopping it first let the worker's "failed" win under load, and
// CancelJob then refused the finished job, so a fail-closed stop ended failed
// with no recorded cause.
func TestCancelMonitoredReviewCommitsCancelBeforeStoppingRuntime(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "monitored-review", Agent: "reviewer", Type: "review", State: string(workflow.JobRunning), Payload: `{"repo":"owner/repo"}`},
		db.JobEvent{Kind: string(workflow.JobRunning), Message: "seed"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, "monitored-review")
	if err != nil {
		t.Fatal(err)
	}
	var stateAtStop []string
	stop := func() {
		latest, err := store.GetJob(ctx, job.ID)
		if err != nil {
			t.Errorf("read job when the runtime was stopped: %v", err)
			return
		}
		stateAtStop = append(stateAtStop, latest.State)
	}
	if err := cancelMonitoredReview(store, job.ID, job.LifecycleGeneration, "statfs failed", stop); err != nil {
		t.Fatalf("cancelMonitoredReview: %v", err)
	}
	if len(stateAtStop) == 0 {
		t.Fatal("cancelMonitoredReview never stopped the runtime")
	}
	for _, state := range stateAtStop {
		if state != string(workflow.JobCancelled) {
			t.Fatalf("runtime stopped while the job was %q (states at each stop: %v); the cancel must be committed first", state, stateAtStop)
		}
	}
}

// hideSeatStagingSources leaves only git on PATH for the worker run that
// follows. Read-only seat setup copies every Go installation and provider
// runtime it finds on PATH into the (fresh, per-test) home before delivery
// starts: 220+ MB per subtest, measured at 37s to 2m33s on a loaded host, and
// the cause of "review never reached delivery" (#2676). These tests are about
// the capacity monitor, not staging, so each Go and runtime is published
// unavailable instead (they declare no real adapter exec, so that does not
// refuse the review) and delivery starts within a second.
func hideSeatStagingSources(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
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

// requireLocalGuardLogged finds a janitor_unavailable_local_guard row in the
// capacity log that names the failed read and carries the local measurement.
func requireLocalGuardLogged(t *testing.T, paths config.Paths, wantFree uint64) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(paths.Home, "review-capacity.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry struct {
			Phase        string                      `json:"phase"`
			Detail       string                      `json:"detail"`
			Measurements []reviewCapacityMeasurement `json:"measurements"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("capacity log line %q: %v", line, err)
		}
		if entry.Phase == "janitor_unavailable_local_guard" && strings.Contains(entry.Detail, "janitor status unavailable") &&
			strings.Contains(entry.Detail, fmt.Sprintf("free_bytes=%d", wantFree)) &&
			len(entry.Measurements) == 1 && entry.Measurements[0].Available == wantFree {
			return
		}
	}
	t.Fatalf("capacity log has no janitor_unavailable_local_guard row with free_bytes=%d:\n%s", wantFree, raw)
}
