package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// absentBinaryContract is the probe answer for a runtime whose declared CLI is
// not installed: honestly classified UNKNOWN, carrying the binary-present row
// the dispatch predicate keys on. Absence classification is never changed by
// this feature, only what the caller does about it.
func absentBinaryContract(runtimeName, binary string) runtime.RuntimeContractResult {
	return runtime.RuntimeContractResult{
		Runtime:    runtimeName,
		Version:    "unknown",
		State:      runtime.RuntimeContractUnknown,
		Instrument: "look-path",
		Requirements: []runtime.RuntimeRequirementResult{{
			Kind:       runtime.RuntimeRequirementBinaryPresent,
			Name:       `executable "` + binary + `"`,
			Source:     `runtime "` + runtimeName + `" contract binary`,
			Remedy:     "install " + binary + " on the host that will run this job, or dispatch it to an agent whose runtime is installed",
			State:      runtime.RuntimeContractUnknown,
			Instrument: "look-path",
			Detail:     `resolve ` + binary + `: exec: "` + binary + `": executable file not found in $PATH`,
		}},
	}
}

// presentBinaryContract is a runtime that DOES declare a CLI binary and whose
// binary resolves. Deliberately not ShellRuntime: shell declares no binary at
// all, so using it as the "present" arm proves nothing about presence.
func presentBinaryContract(runtimeName string) runtime.RuntimeContractResult {
	return runtime.RuntimeContractResult{
		Runtime:      runtimeName,
		Version:      "stub 1.2.3",
		State:        runtime.RuntimeContractSupported,
		Instrument:   "binary-help",
		ResolvedPath: "/usr/bin/" + runtimeName,
	}
}

func stubForegroundProbe(t *testing.T, result runtime.RuntimeContractResult) *int {
	t.Helper()
	calls := 0
	previous := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		calls++
		return result
	}
	t.Cleanup(func() { localRuntimeContractPreflight = previous })
	return &calls
}

func stubForegroundAdapter(t *testing.T) {
	t.Helper()
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	previous := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) { return adapter, nil }
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previous })
}

// readOnlySeatWorktreeCount counts allocated read-only seat worktrees under the
// dispatch home: the artifact that must not exist after a refusal.
func readOnlySeatWorktreeCount(t *testing.T, home string) int {
	t.Helper()
	count := 0
	_ = filepath.WalkDir(filepath.Join(config.PathsForHome(home).Home, "worktrees"), func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil || !entry.IsDir() {
			return nil //nolint:nilerr // a missing root is zero worktrees
		}
		if strings.Contains(entry.Name(), "readonly-seat") {
			count++
		}
		return nil
	})
	return count
}

func seedAbsentBinaryReviewRepo(t *testing.T) (*db.Store, string) {
	t.Helper()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ClaudeRuntime, "unused", []string{"review", "ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo")
	return store, home
}

// #1926-f6: THE BACKGROUND ARM, ANCHORED AT THE PRODUCTION COMMAND ENTRY.
//
// Driven through dispatchAgentCommand - the function `gitmoot agent review`
// actually calls - rather than through dispatchLocalAgentJob with the flag set
// by hand. The previous version set ExecsDeclaredBinary itself, so a mutant that
// bypassed the production wrapper left it green.
//
// Background is the route that matters: the foreground contract gate is guarded
// by !request.Background while the review arm allocates the read-only worktree
// at enqueue, so this is where a review dispatched --background used to reach
// allocation with no capability question asked.
func TestProductionBackgroundReviewRefusesAbsentBinaryBeforeRowOrWorktree(t *testing.T) {
	ctx := context.Background()
	store, home := seedAbsentBinaryReviewRepo(t)
	probeCalls := stubForegroundProbe(t, absentBinaryContract(runtime.ClaudeRuntime, "claude"))

	var stdout, stderr bytes.Buffer
	_, code := dispatchAgentCommand(agentRunOptions{
		repo: "owner/repo", agent: "responder", lead: "lead", message: "review it",
		prNumber: 1926, background: true, home: home,
	}, "review", "", "", &stdout, &stderr)

	if code == 0 {
		t.Fatalf("production background review accepted an absent runtime binary; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	for _, want := range []string{`runtime "claude"`, `executable "claude"`, "does not resolve on PATH", "install claude"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("production stderr does not name %q: %s", want, stderr.String())
		}
	}
	if *probeCalls == 0 {
		t.Fatal("the production route never consulted the runtime probe, so this test is not exercising the predicate")
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none: refusal must precede row creation", jobs)
	}
	if n := readOnlySeatWorktreeCount(t, home); n != 0 {
		t.Fatalf("read-only seat worktrees = %d, want 0: refusal must precede allocation", n)
	}
}

// FOREGROUND, same production entry, no --background.
func TestProductionForegroundAskRefusesAbsentBinaryBeforeRow(t *testing.T) {
	ctx := context.Background()
	store, home := seedAbsentBinaryReviewRepo(t)
	stubForegroundProbe(t, absentBinaryContract(runtime.ClaudeRuntime, "claude"))

	var stdout, stderr bytes.Buffer
	_, code := dispatchAgentCommand(agentRunOptions{
		repo: "owner/repo", agent: "responder", message: "hello", home: home,
	}, "ask", "", "", &stdout, &stderr)

	if code == 0 {
		t.Fatalf("production foreground ask accepted an absent runtime binary; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not resolve on PATH") {
		t.Fatalf("foreground refusal lost its cause: %s", stderr.String())
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none", jobs)
	}
}

// NEGATIVE ARM: an injected adapter with the SAME absent binary must still
// dispatch. This is the 24-test over-block regression in miniature, and it is
// what stops the predicate from keying on absence alone.
func TestInjectedAdapterWithAbsentBinaryStillDispatches(t *testing.T) {
	ctx := context.Background()
	store, home := seedAbsentBinaryReviewRepo(t)
	stubForegroundProbe(t, absentBinaryContract(runtime.ClaudeRuntime, "claude"))
	stubForegroundAdapter(t)

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Instructions: "hello", Home: home,
		// Deliberately NOT declared: an injected adapter execs nothing.
	})
	if err != nil {
		t.Fatalf("an injected adapter with an absent binary was refused: %v", err)
	}
	if strings.TrimSpace(out.JobID) == "" {
		t.Fatal("injected-adapter dispatch produced no job")
	}
}

// PRESENT-BINARY ARM, on a runtime that actually DECLARES a binary. The prior
// version used ShellRuntime, which declares none, so it could not distinguish
// "present" from "not applicable" - the reviewer was right about that.
func TestProductionDispatchWithPresentBinaryStillDispatches(t *testing.T) {
	ctx := context.Background()
	store, home := seedAbsentBinaryReviewRepo(t)
	stubForegroundProbe(t, presentBinaryContract(runtime.ClaudeRuntime))
	stubForegroundAdapter(t)

	var stdout, stderr bytes.Buffer
	_, code := dispatchAgentCommand(agentRunOptions{
		repo: "owner/repo", agent: "responder", message: "hello", home: home,
	}, "ask", "", "", &stdout, &stderr)

	if code != 0 {
		t.Fatalf("a PRESENT declared binary was refused by the production route; stderr=%s", stderr.String())
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) == 0 {
		t.Fatal("a present binary produced no job")
	}
}

// REMOTE BACKEND, and this arm now CONSUMES the resolver override it installs.
// The previous version installed localAgentDispatchExecBackendFor and then
// passed execbackend.Remote straight to the predicate, so the override was
// decorative and a broken backend resolver would not have been caught.
func TestProductionRemoteBackendWithAbsentLocalBinaryIsNotRefused(t *testing.T) {
	store, home := seedAbsentBinaryReviewRepo(t)
	probeCalls := stubForegroundProbe(t, absentBinaryContract(runtime.ClaudeRuntime, "claude"))

	previousBackend := localAgentDispatchExecBackendFor
	resolverCalls := 0
	localAgentDispatchExecBackendFor = func(string) (execbackend.Backend, error) {
		resolverCalls++
		return execbackend.Remote, nil
	}
	t.Cleanup(func() { localAgentDispatchExecBackendFor = previousBackend })

	var stdout, stderr bytes.Buffer
	_, _ = dispatchAgentCommand(agentRunOptions{
		repo: "owner/repo", agent: "responder", message: "hello", background: true, home: home,
	}, "ask", "", "", &stdout, &stderr)

	if resolverCalls == 0 {
		t.Fatal("the backend resolver override was never consumed, so this test says nothing about backend routing")
	}
	// The absence must NOT be the reason anything failed: a remote backend's
	// binary lives on another host, so the local probe answers about the wrong
	// machine and must not even be consulted.
	if strings.Contains(stderr.String(), "does not resolve on PATH") {
		t.Fatalf("a remote-backend dispatch was refused on a LOCAL binary absence: %s", stderr.String())
	}
	if *probeCalls != 0 {
		t.Fatalf("local binary probe ran %d times for a REMOTE backend; that answer is about the wrong host", *probeCalls)
	}
	_ = store
}

// #1926-f5: THE DAEMON CLAIM ROUTE, REAL ADAPTER, driven through the worker as
// it actually claims. This is the seam the previous head left open: a delegated
// or daemon-created job reached adapter construction with an absent executable
// and reproduced the original missing-claude failure.
func TestDaemonClaimRealAdapterRefusesAbsentBinary(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ClaudeRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-daemon-absent", Agent: "lead", Action: "implement", Repo: "owner/repo",
		Branch: "task-1", PullRequest: 41, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1",
	})

	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.CommenterFactory = func(string) github.Client { return &cliPollFakeGitHub{} }
	worker.RuntimePreflight = func(context.Context, runtime.Agent, runtime.RuntimeContractRequest) runtime.RuntimeContractResult {
		return absentBinaryContract(runtime.ClaudeRuntime, "claude")
	}
	// NOTE: AdapterFactory is left as defaultJobWorker set it, through
	// setRealAdapterFactory, so this worker declares a REAL adapter. That is the
	// whole point of the arm.

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-daemon-absent")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked: a real daemon adapter with an absent executable must be refused at claim", job.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	// ABSENCE CLASSIFICATION PRESERVED: the honest unknown fact is still recorded
	// alongside the refusal, not replaced by it.
	if !daemonWorkerHasEvent(events, "runtime_contract_unknown") {
		t.Fatalf("events = %+v, want runtime_contract_unknown retained beside the refusal", events)
	}
}

// NEGATIVE ARM ON THE SAME SEAM: an INJECTED daemon adapter with the same absent
// binary must still deliver. A test that injects assigns AdapterFactory
// directly, which is exactly what the ~22 existing fixtures do, so the
// discriminator stays false and they keep working unmodified.
func TestDaemonClaimInjectedAdapterWithAbsentBinaryStillDelivers(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ClaudeRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-daemon-injected", Agent: "lead", Action: "implement", Repo: "owner/repo",
		Branch: "task-1", PullRequest: 42, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1",
	})

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	// Direct assignment, as every injecting fixture does: no real-adapter claim.
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return adapter, nil }
	worker.CommenterFactory = func(string) github.Client { return &cliPollFakeGitHub{} }
	worker.RuntimePreflight = func(context.Context, runtime.Agent, runtime.RuntimeContractRequest) runtime.RuntimeContractResult {
		return absentBinaryContract(runtime.ClaudeRuntime, "claude")
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-daemon-injected")
	if err != nil {
		t.Fatal(err)
	}
	if job.State == string(workflow.JobBlocked) {
		t.Fatal("an INJECTED daemon adapter was refused for an absence it will never hit; that is the 24-test over-block regression")
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
}

// The production worker constructor must declare its adapter real. Without this
// the daemon seam is inert: a mutant reverting setRealAdapterFactory to a plain
// assignment leaves every arm above green except this one.
func TestProductionWorkerDeclaresRealAdapter(t *testing.T) {
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)
	if !worker.adapterIsDeclaredReal() {
		t.Fatal("defaultJobWorker did not declare its adapter real, so the daemon absent-binary seam can never fire in production")
	}
	// And the claim must be BOUND to that factory: replacing it withdraws the
	// claim, which is what keeps ~22 injecting fixtures exempt without edits.
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return nil, nil }
	if worker.adapterIsDeclaredReal() {
		t.Fatal("replacing AdapterFactory left the real-adapter claim standing; an injected fake would be refused for an absence it never reaches")
	}
}

// declareNoRealAdapterExec marks a test as one that drives a CLI dispatch entry
// WITHOUT ever execing a runtime binary (#1817). Its subject is dispatch
// bookkeeping, and the runner it must pass on has no runtime CLIs installed, so
// the production declaration would refuse it for an absence that never matters.
func declareNoRealAdapterExec(t *testing.T) {
	t.Helper()
	previous := cliDispatchExecsDeclaredBinary
	cliDispatchExecsDeclaredBinary = false
	t.Cleanup(func() { cliDispatchExecsDeclaredBinary = previous })
}
