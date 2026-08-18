package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Execution-backend P1 E2Es (#1536): the [remote_exec] selector names the
// seam, "local" is a byte-for-byte passthrough to the pre-#1536 runner
// composition, and any unknown backend FAILS LOUD at dispatch. All tests are
// deterministic, NO-LLM (shell runtime), offline, on isolated /tmp homes.

// execBackendLocalEventKindBaseline is the EXACT job-event kind sequence a
// shell ask job produces on main @ 0b95ac2d (captured by running this exact
// fixture with the selector code stashed — same sequence, same order). Any
// behavioural drift in the default path — an added/removed/reordered event —
// fails here, not just "the job succeeded".
var execBackendLocalEventKindBaseline = []string{
	"queued",
	"workflow_autolabeled",
	"route_selected",
	"readonly_worktree_allocated",
	"permission_policy_not_applied",
	"effective_runtime",
	"running",
	"succeeded",
	"advance_started",
	"delegation_worktree_removed",
	"advance_completed",
}

// execBackendDispatchAsk enqueues a background shell ask and returns its job id.
func execBackendDispatchAsk(t *testing.T, home string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "exec backend probe",
		"--home", home,
		"--repo", "owner/repo",
		"--background",
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("agent ask --background exit = %d, stderr=%s", code, errBuf.String())
	}
	var output localAgentJobOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatalf("parse ask output %q: %v", out.String(), err)
	}
	if output.State != string(workflow.JobQueued) {
		t.Fatalf("background ask state = %q, want queued", output.State)
	}
	return output.JobID
}

func execBackendRunOneTick(t *testing.T, home string, store *db.Store) {
	t.Helper()
	worker := defaultJobWorker(store, io.Discard, home)
	if err := runEnabledRepoWorkerTicksTracked(context.Background(), store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
}

func execBackendEventKinds(t *testing.T, store *db.Store, jobID string) []string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func execBackendAppendConfig(t *testing.T, home string, section string) {
	t.Helper()
	configFile := config.PathsForHome(home).ConfigFile
	existing, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(configFile, append(existing, []byte(section)...), 0o600); err != nil {
		t.Fatalf("append config: %v", err)
	}
}

// assertExecBackendLocalSucceeded asserts the ACCEPTANCE-1 contract: the job
// ran the shell fixture to terminal succeeded, the event-kind sequence is the
// pinned main baseline IN ORDER, and the stored payload carries no
// exec_backend key (byte-identical serialization).
func assertExecBackendLocalSucceeded(t *testing.T, store *db.Store, jobID, marker string) {
	t.Helper()
	ctx := context.Background()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("shell fixture did not run (marker missing): %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	if strings.Contains(job.Payload, "exec_backend") {
		t.Fatalf("payload carries exec_backend: %s\nwant byte-identical serialization for a local job", job.Payload)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.Result == nil || payload.Result.Decision != "approved" {
		t.Fatalf("job result = %+v, want the shell fixture's approved result", payload.Result)
	}
	if kinds := execBackendEventKinds(t, store, jobID); !reflect.DeepEqual(kinds, execBackendLocalEventKindBaseline) {
		t.Fatalf("event kinds = %v\nwant the main baseline %v", kinds, execBackendLocalEventKindBaseline)
	}
}

// TestExecBackendLocalDefaultDaemonE2E is ACCEPTANCE 1: no [remote_exec]
// config at all — the default path is byte-for-byte main.
func TestExecBackendLocalDefaultDaemonE2E(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "shell-ran-default")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
	jobID := execBackendDispatchAsk(t, home)
	execBackendRunOneTick(t, home, store)
	assertExecBackendLocalSucceeded(t, store, jobID, marker)
}

// TestExecBackendLocalExplicitDaemonE2E is ACCEPTANCE 2: an explicit
// backend = "local" behaves IDENTICALLY to the default path — same event
// sequence, same result contract.
func TestExecBackendLocalExplicitDaemonE2E(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "shell-ran-explicit-local")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
	execBackendAppendConfig(t, home, "\n[remote_exec]\nbackend = \"local\"\n")
	jobID := execBackendDispatchAsk(t, home)
	execBackendRunOneTick(t, home, store)
	assertExecBackendLocalSucceeded(t, store, jobID, marker)
}

// TestExecBackendUnknownFailsLoudDaemonE2E is ACCEPTANCE 3: an unknown
// backend — "e2b" (not implemented until P5) and the typo "loca" — FAILS LOUD
// at dispatch naming the value AND the allowed set; the job never runs.
func TestExecBackendUnknownFailsLoudDaemonE2E(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "e2b", value: "e2b"},
		{name: "typo", value: "loca"},
		{name: "explicit blank", value: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			marker := filepath.Join(t.TempDir(), "must-not-run")
			home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
			execBackendAppendConfig(t, home, "\n[remote_exec]\nbackend = \""+tc.value+"\"\n")
			jobID := execBackendDispatchAsk(t, home)
			execBackendRunOneTick(t, home, store)

			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("adapter ran with an unknown backend (marker err=%v)", err)
			}
			job, err := store.GetJob(ctx, jobID)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if job.State != string(workflow.JobFailed) {
				t.Fatalf("job state = %q, want failed", job.State)
			}
			events, err := store.ListJobEvents(ctx, jobID)
			if err != nil {
				t.Fatalf("ListJobEvents: %v", err)
			}
			var failedMessage string
			for _, event := range events {
				if event.Kind == "running" {
					t.Fatalf("job reached running with an unknown backend: %+v", event)
				}
				if event.Kind == string(workflow.JobFailed) {
					failedMessage = event.Message
				}
			}
			// The loud error must name the offending value AND the allowed set
			// AND its config source — not just be a non-zero exit.
			if !strings.Contains(failedMessage, `"`+tc.value+`"`) {
				t.Fatalf("failed event = %q, want it to name %q", failedMessage, tc.value)
			}
			if !strings.Contains(failedMessage, "allowed: local") {
				t.Fatalf("failed event = %q, want the allowed set named", failedMessage)
			}
			if !strings.Contains(failedMessage, "[remote_exec].backend") {
				t.Fatalf("failed event = %q, want the config key named", failedMessage)
			}
		})
	}
}

// TestExecBackendUnknownFailsLoudForegroundE2E pins the synchronous dispatch
// boundary: foreground jobs never reach jobWorker.run, so they must resolve the
// configured backend before enqueue or adapter execution.
func TestExecBackendUnknownFailsLoudForegroundE2E(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-run-foreground")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
	execBackendAppendConfig(t, home, "\n[remote_exec]\nbackend = \"e2b\"\n")

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "exec backend foreground probe",
		"--home", home,
		"--repo", "owner/repo",
		"--json",
	}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("foreground ask exit = 0, output=%s; want loud backend refusal", out.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("foreground adapter ran with an unknown backend (marker err=%v)", err)
	}
	if got := errBuf.String(); !strings.Contains(got, `"e2b"`) || !strings.Contains(got, "allowed: local") || !strings.Contains(got, "[remote_exec].backend") {
		t.Fatalf("foreground error = %q, want config source + value + allowed set", got)
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want no enqueued row after foreground preflight refusal", jobs)
	}
}

// TestExecBackendResolvedNonLocalCannotRunLocallyForegroundE2E models the P2
// boundary after resolution has accepted a real remote backend. Until that
// backend has an explicit foreground adapter arm, dispatch must fail rather
// than invoke the legacy local factory and report a false success.
func TestExecBackendResolvedNonLocalCannotRunLocallyForegroundE2E(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-run-resolved-remote")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))

	previousResolver := localAgentDispatchExecBackendFor
	localAgentDispatchExecBackendFor = func(string) (execbackend.Backend, error) {
		return execbackend.Backend("p2-probe"), nil
	}
	t.Cleanup(func() { localAgentDispatchExecBackendFor = previousResolver })

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "resolved backend foreground probe",
		"--home", home,
		"--repo", "owner/repo",
		"--json",
	}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("foreground ask exit = 0, output=%s; resolved non-local backend ran locally", out.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("foreground adapter ran locally for a resolved non-local backend (marker err=%v)", err)
	}
	if got := errBuf.String(); !strings.Contains(got, `"p2-probe"`) || !strings.Contains(got, "no execution implementation") {
		t.Fatalf("foreground error = %q, want resolved backend and missing implementation named", got)
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want no enqueued row after missing foreground implementation refusal", jobs)
	}
}

// TestExecBackendResolvedNonLocalCannotRunLocallyDaemonE2E models the same P2
// boundary in the claiming worker. Resolution may accept a future backend, but
// the daemon must not run it until that backend has an explicit adapter arm.
func TestExecBackendResolvedNonLocalCannotRunLocallyDaemonE2E(t *testing.T) {
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "must-not-run-resolved-remote-daemon")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
	jobID := execBackendDispatchAsk(t, home)

	previousResolver := daemonJobExecBackendFor
	daemonJobExecBackendFor = func(jobWorker, string, bool) (execbackend.Backend, error) {
		return execbackend.Backend("p2-probe"), nil
	}
	t.Cleanup(func() { daemonJobExecBackendFor = previousResolver })

	checkoutCalls := 0
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		checkoutCalls++
		return t.TempDir(), nil
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob before run: %v", err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker run: %v", err)
	}
	if checkoutCalls != 0 {
		t.Fatalf("checkout validation calls = %d, want zero before non-local runner refusal", checkoutCalls)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("daemon adapter ran locally for a resolved non-local backend (marker err=%v)", err)
	}
	job, err = store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	for _, event := range execBackendEventKinds(t, store, jobID) {
		if event == string(workflow.JobRunning) {
			t.Fatalf("job reached running for a resolved non-local backend")
		}
	}
	failedMessages := execBackendEvents(t, store, jobID)
	if len(failedMessages) != 1 || !strings.Contains(failedMessages[0], `"p2-probe"`) || !strings.Contains(failedMessages[0], "no execution implementation") {
		t.Fatalf("failed events = %q, want resolved backend and missing daemon implementation named", failedMessages)
	}
}

// TestP2GapJobSubprocessRoutesRefuseLocalFallback pins the one consumption
// seam used before job-associated checkout and git work. The P2 overlay adds
// p2-probe to the implemented registry; a compile-valid mutation that maps it
// to ExecRunner must make this test fail rather than execute on the host.
func TestP2GapJobSubprocessRoutesRefuseLocalFallback(t *testing.T) {
	if backend, err := execbackend.ParseImplemented("p2-probe"); err == nil {
		t.Fatalf("%s became implemented; add its job subprocess runner before updating this guard", backend)
	}
	if _, err := jobSubprocessRunnerForBackend(execbackend.Backend("p2-probe")); err == nil {
		t.Fatal("p2-probe inherited the local job subprocess runner")
	}
}

// TestP2GapEveryJobAdapterConstructionRouteRefusesLocalFallback supplies an
// already-resolved future backend directly to every job adapter construction
// seam. It is also run under an overlay that makes p2-probe parse as
// implemented: registry acceptance must not make any route invoke Local.
func TestP2GapEveryJobAdapterConstructionRouteRefusesLocalFallback(t *testing.T) {
	backend := execbackend.Backend("p2-probe")
	localDeliveryCalls := 0
	worker := jobWorker{
		AdapterFactory: func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
			localDeliveryCalls++
			return &cliWorkerFakeAdapter{}, nil
		},
	}
	agent := runtime.Agent{Runtime: runtime.ShellRuntime, ExecBackend: string(backend)}

	t.Run("foreground factory", func(t *testing.T) {
		if _, err := foregroundRuntimeAdapterFactoryFor(backend); err == nil {
			t.Fatal("foreground factory accepted p2-probe without an implementation")
		}
	})
	t.Run("daemon primary delivery", func(t *testing.T) {
		if _, _, err := worker.buildSeatAwareAdapterForBackend(backend, &agent, t.TempDir(), workflow.JobPayload{}); err == nil {
			t.Fatal("daemon primary delivery accepted p2-probe without an implementation")
		}
	})
	t.Run("temporary worker delivery", func(t *testing.T) {
		if _, err := worker.deliveryAdapterForBackend(backend, agent, t.TempDir()); err == nil {
			t.Fatal("temporary worker delivery accepted p2-probe without an implementation")
		}
	})
	t.Run("runtime composition rebuild", func(t *testing.T) {
		if _, err := buildRuntimeAdapter("", agent, t.TempDir(), nil); err == nil {
			t.Fatal("runtime composition accepted p2-probe without an implementation")
		}
	})
	t.Run("runtime session start", func(t *testing.T) {
		if _, err := startRuntimeAdapterForBackend(backend, "", runtime.ShellRuntime, t.TempDir()); err == nil {
			t.Fatal("runtime session start accepted p2-probe without an implementation")
		}
	})
	t.Run("one-shot runtime session", func(t *testing.T) {
		_, err := deliverOneShotRuntimePrompt(context.Background(), runtime.Agent{
			Runtime:     runtime.ShellRuntime,
			ExecBackend: string(backend),
		}, "must not run")
		if err == nil {
			t.Fatal("one-shot runtime session accepted p2-probe without an implementation")
		}
	})
	t.Run("live A/B resumed runtime session", func(t *testing.T) {
		runner := &agentStartRunner{results: []subprocess.Result{{Stdout: "silently-local"}}}
		restore := replaceRuntimeFactory(runtime.Factory{Runner: runner})
		t.Cleanup(restore)

		_, err := realSkillOptABDeliver(context.Background(), runtime.Agent{
			Name:        "live-ab-challenger",
			Role:        "reviewer",
			Runtime:     runtime.ShellRuntime,
			RuntimeRef:  "printf silently-local",
			RepoScope:   "owner/repo",
			ExecBackend: string(backend),
		}, "must not run")
		if err == nil {
			t.Fatal("live A/B resume accepted p2-probe without an implementation")
		}
		if len(runner.calls) != 0 {
			t.Fatalf("live A/B resume invoked the local runtime %d times, want 0", len(runner.calls))
		}
	})
	t.Run("runtime contract preflight", func(t *testing.T) {
		calls := 0
		if _, err := runtimeContractPreflightForBackend(backend, func() runtime.RuntimeContractResult {
			calls++
			return runtime.RuntimeContractResult{}
		}); err == nil {
			t.Fatal("runtime contract preflight accepted p2-probe without an implementation")
		}
		if calls != 0 {
			t.Fatalf("local runtime contract preflight calls = %d, want 0", calls)
		}
	})
	t.Run("deferred auth probe", func(t *testing.T) {
		calls := 0
		if _, err := authProbeForBackend(backend, func() authProbeVerdict {
			calls++
			return authProbeValid
		}); err == nil {
			t.Fatal("deferred auth probe accepted p2-probe without an implementation")
		}
		if calls != 0 {
			t.Fatalf("local deferred auth probe calls = %d, want 0", calls)
		}
	})
	if localDeliveryCalls != 0 {
		t.Fatalf("local delivery factory calls = %d, want 0", localDeliveryCalls)
	}
}

func TestExecBackendEphemeralResolvesBeforeRuntimeStart(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	const jobID = "exec-backend-ephemeral-preflight"
	const agentName = "reviewer-ephemeral-backend"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: agentName, Action: "ask", Repo: "owner/repo", Branch: "main",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.CodexRuntime},
	})

	previousResolver := daemonJobExecBackendFor
	daemonJobExecBackendFor = func(jobWorker, string, bool) (execbackend.Backend, error) {
		return "", errors.New("backend preflight refused")
	}
	t.Cleanup(func() { daemonJobExecBackendFor = previousResolver })

	startCalls := 0
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.StartAdapterFactory = func(execbackend.Backend, string, string) (runtime.Adapter, error) {
		startCalls++
		return &cliWorkerFakeAdapter{}, nil
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker run: %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("runtime start factory calls = %d, want 0 before backend validation", startCalls)
	}
	if _, err := store.GetAgent(ctx, agentName); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ephemeral agent lookup error = %v, want no materialized agent", err)
	}
	after, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(after): %v", err)
	}
	if after.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", after.State)
	}
}

// TestExecBackendOverrideResolutionDaemonE2E covers the per-job override
// field: an unknown override fails loud (naming the override source) even
// with no [remote_exec] section, and an explicit "local" override resolves to
// the same local passthrough.
func TestExecBackendOverrideResolutionDaemonE2E(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown override fails loud", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "must-not-run-override")
		home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
		const jobID = "exec-backend-override-unknown"
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: jobID, Agent: "shell-asker", Type: "ask", State: string(workflow.JobQueued),
			Payload: `{"repo":"owner/repo","sender":"local","instructions":"probe","exec_backend":"e2b"}`,
		}, db.JobEvent{JobID: jobID, Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
			t.Fatalf("CreateJobWithEvent: %v", err)
		}
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		worker := defaultJobWorker(store, io.Discard, home)
		if err := worker.run(ctx, job); err != nil {
			t.Fatalf("worker run: %v", err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("adapter ran with an unknown override (marker err=%v)", err)
		}
		after, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob(after): %v", err)
		}
		if after.State != string(workflow.JobFailed) {
			t.Fatalf("job state = %q, want failed", after.State)
		}
		var failedMessage string
		for _, event := range execBackendEvents(t, store, jobID) {
			failedMessage = event
		}
		if !strings.Contains(failedMessage, "exec_backend") || !strings.Contains(failedMessage, `"e2b"`) || !strings.Contains(failedMessage, "allowed: local") {
			t.Fatalf("failed event = %q, want override source + value + allowed set", failedMessage)
		}
	})

	t.Run("explicit blank override fails loud", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "must-not-run-override-blank")
		home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
		const jobID = "exec-backend-override-blank"
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: jobID, Agent: "shell-asker", Type: "ask", State: string(workflow.JobQueued),
			Payload: `{"repo":"owner/repo","sender":"local","instructions":"probe","exec_backend":""}`,
		}, db.JobEvent{JobID: jobID, Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
			t.Fatalf("CreateJobWithEvent: %v", err)
		}
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		worker := defaultJobWorker(store, io.Discard, home)
		if err := worker.run(ctx, job); err != nil {
			t.Fatalf("worker run: %v", err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("adapter ran with an explicit blank override (marker err=%v)", err)
		}
		after, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob(after): %v", err)
		}
		if after.State != string(workflow.JobFailed) {
			t.Fatalf("job state = %q, want failed", after.State)
		}
		failedMessages := execBackendEvents(t, store, jobID)
		if len(failedMessages) == 0 || !strings.Contains(failedMessages[len(failedMessages)-1], `unknown execution backend ""`) {
			t.Fatalf("failed events = %q, want the explicit blank override named", failedMessages)
		}
	})

	t.Run("explicit local override passes through", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "shell-ran-override-local")
		home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
		const jobID = "exec-backend-override-local"
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: jobID, Agent: "shell-asker", Type: "ask", State: string(workflow.JobQueued),
			Payload: `{"repo":"owner/repo","sender":"local","instructions":"probe","exec_backend":"local","future_dispatch":{"mode":"isolated"}}`,
		}, db.JobEvent{JobID: jobID, Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
			t.Fatalf("CreateJobWithEvent: %v", err)
		}
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		worker := defaultJobWorker(store, io.Discard, home)
		if err := worker.run(ctx, job); err != nil {
			t.Fatalf("worker run: %v", err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("shell fixture did not run with a local override: %v", err)
		}
		after, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob(after): %v", err)
		}
		if after.State != string(workflow.JobSucceeded) {
			t.Fatalf("job state = %q, want succeeded", after.State)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(after.Payload), &envelope); err != nil {
			t.Fatalf("decode payload after execution: %v", err)
		}
		if got := string(envelope["future_dispatch"]); got != `{"mode":"isolated"}` {
			t.Fatalf("future_dispatch after execution = %s, want preserved unknown member; payload=%s", got, after.Payload)
		}
	})
}

func execBackendEvents(t *testing.T, store *db.Store, jobID string) []string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	messages := make([]string, 0, len(events))
	for _, event := range events {
		if event.Kind == string(workflow.JobFailed) {
			messages = append(messages, event.Message)
		}
	}
	return messages
}

// TestExecBackendCompositionPreservedForLocal is ACCEPTANCE 4: the selector
// does NOT displace the two wrappers a naive selector would — for a
// claude/kimi produce job with path grants the Landlock WrappingRunner is
// still applied and still positioned with GroupRunner{} innermost, and a
// stamped "local" composes IDENTICALLY to an unstamped agent. An unknown
// stamped backend fails loud at the composition site itself.
//
// (The credgw half — runtimeJobRunner still yielding the *credgw.Runner the
// buildRuntimeAdapter type assertion needs — is exercised through the same
// modified buildRuntimeAdapter by the existing
// TestClaudeModelGatewayCredentialCustodyE2E, which builds its adapter with
// an unstamped agent and asserts full gateway behaviour.)
func TestExecBackendCompositionPreservedForLocal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home+"/.cache")
	work := t.TempDir()

	grants := runtime.Agent{
		Name:           "produce-agent",
		Role:           "producer",
		Runtime:        runtime.KimiRuntime,
		RuntimeRef:     "session_550e8400-e29b-41d4-a716-446655440000",
		RepoScope:      "owner/repo",
		ReadablePaths:  []string{"/data/input"},
		WritablePaths:  []string{"/data/out"},
		AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite,
	}
	buildRunner := func(execBackend string) subprocess.Runner {
		t.Helper()
		agent := grants
		agent.ExecBackend = execBackend
		adapter, err := buildRuntimeAdapter("", agent, work, nil)
		if err != nil {
			t.Fatalf("buildRuntimeAdapter(exec_backend=%q): %v", execBackend, err)
		}
		kimi, ok := adapter.(runtime.KimiAdapter)
		if !ok {
			t.Fatalf("adapter = %T, want KimiAdapter", adapter)
		}
		return kimi.Runner
	}

	unstamped := buildRunner("")
	stamped := buildRunner("local")
	// Byte-for-byte at the composition site: a stamped "local" must produce a
	// runner IDENTICAL to the pre-#1536 (unstamped) pipeline output.
	if !reflect.DeepEqual(unstamped, stamped) {
		t.Fatalf("stamped local runner = %#v\nunstamped runner = %#v\nwant identical composition", stamped, unstamped)
	}
	// The Landlock WrappingRunner is still applied and correctly positioned:
	// WrappingRunner OUTSIDE, GroupRunner{} innermost.
	wrapper, ok := stamped.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("runner = %T, want WrappingRunner (the Landlock produce wrap was displaced)", stamped)
	}
	if _, ok := wrapper.Inner.(subprocess.GroupRunner); !ok {
		t.Fatalf("wrapper inner = %T, want GroupRunner{} innermost", wrapper.Inner)
	}
	if !reflect.DeepEqual(wrapper.ReadablePaths, []string{"/data/input"}) {
		t.Fatalf("wrapper reads = %v, want the agent's readable grant", wrapper.ReadablePaths)
	}
	wantWrites := []string{"/data/out", filepath.Join(home, ".kimi-code")}
	if !reflect.DeepEqual(wrapper.WritablePaths, wantWrites) {
		t.Fatalf("wrapper writes = %v, want %v (grants + kimi state dir)", wrapper.WritablePaths, wantWrites)
	}

	// An unknown stamped backend fails loud AT THE COMPOSITION SITE — the
	// guard behind the dispatch validation, so a selector bypass can never
	// silently mis-compose a runner.
	bad := grants
	bad.ExecBackend = "e2b"
	_, err := buildRuntimeAdapter("", bad, work, nil)
	if err == nil {
		t.Fatal("buildRuntimeAdapter with exec_backend=e2b succeeded, want a loud error")
	}
	if !strings.Contains(err.Error(), `"e2b"`) || !strings.Contains(err.Error(), "allowed: local") {
		t.Fatalf("composition-site error = %q, want the value AND the allowed set", err)
	}

	// Advertising a name is not an implementation. A future registry edit that
	// omits the composition arm must fail here instead of inheriting Local.
	originalAllowed := append([]string(nil), execbackend.AllowedNames...)
	execbackend.AllowedNames = append(execbackend.AllowedNames, "future-remote")
	t.Cleanup(func() { execbackend.AllowedNames = originalAllowed })
	bad.ExecBackend = "future-remote"
	_, err = buildRuntimeAdapter("", bad, work, nil)
	if err == nil || !strings.Contains(err.Error(), `"future-remote"`) || !strings.Contains(err.Error(), "advertised but not implemented") {
		t.Fatalf("advertised-without-implementation composition error = %v, want loud refusal", err)
	}
}
