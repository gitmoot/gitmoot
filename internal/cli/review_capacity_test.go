package cli

import (
	"context"
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
			hostPaths, root, err := janitorHostPaths(ctx, store, paths, "owner/repo", payload.WorktreePath)
			if err != nil {
				t.Fatal(err)
			}
			devices, err := janitorHostDevices(hostPaths, root)
			if err != nil {
				t.Fatal(err)
			}
			var low atomic.Bool
			previous := runJanitorStatus
			runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) {
				available := uint64(2 << 30)
				if low.Load() {
					available = 1 << 20
				}
				volumes := make([]string, 0, len(devices))
				for device := range devices {
					volumes = append(volumes, fmt.Sprintf(`{"filesystem_key":"device:%d","device":%d,"state":"cleanup_needed","admission_hold":true,"available_bytes":%d,"reserve_bytes":1048576,"reason":"cleanup target held"}`, device, device, available))
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
			low.Store(true)
			select {
			case <-finished:
			case <-time.After(8 * time.Second):
				t.Fatal("review kept running after fresh available bytes reached the reserve")
			}
			if runErr == nil || !strings.Contains(runErr.Error(), "reserve") {
				t.Fatalf("monitor outcome = %v, want reserve cancellation", runErr)
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
