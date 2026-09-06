package cli

import (
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
// this dispatch predicate keys on.
func absentBinaryContract() runtime.RuntimeContractResult {
	return runtime.RuntimeContractResult{
		Runtime:    runtime.ClaudeRuntime,
		Version:    "unknown",
		State:      runtime.RuntimeContractUnknown,
		Instrument: "look-path",
		Requirements: []runtime.RuntimeRequirementResult{{
			Kind:       runtime.RuntimeRequirementBinaryPresent,
			Name:       `executable "claude"`,
			Source:     `runtime "claude" contract binary`,
			Remedy:     "install claude on the host that will run this job, or dispatch it to an agent whose runtime is installed",
			State:      runtime.RuntimeContractUnknown,
			Instrument: "look-path",
			Detail:     `resolve claude: exec: "claude": executable file not found in $PATH`,
		}},
	}
}

func stubAbsentBinaryProbe(t *testing.T) {
	t.Helper()
	previous := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		return absentBinaryContract()
	}
	t.Cleanup(func() { localRuntimeContractPreflight = previous })
}

// readOnlySeatWorktreeCount counts allocated read-only seat worktrees under the
// dispatch home, which is the artifact the ruling requires must not exist.
func readOnlySeatWorktreeCount(t *testing.T, home string) int {
	t.Helper()
	root := filepath.Join(config.PathsForHome(home).Home, "worktrees")
	count := 0
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
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

// #1817 / ruling 123815: FOREGROUND real-adapter refusal, BEFORE any row or
// worktree exists.
func TestForegroundRealAdapterRefusesAbsentBinaryBeforeRowOrWorktree(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ClaudeRuntime, "unused", []string{"review"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo")
	stubAbsentBinaryProbe(t)

	_, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "review", Instructions: "review it",
		PullRequest: 1926, Home: home, LeadAgent: "lead",
		// What the production CLI entry declares.
		ExecsDeclaredBinary: true,
	})
	if err == nil {
		t.Fatal("a real-adapter dispatch with an absent runtime binary was accepted")
	}
	for _, want := range []string{`agent "responder"`, `runtime "claude"`, `executable "claude"`, "does not resolve on PATH", "install claude"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
	jobs, listErr := store.ListJobs(ctx)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none: the refusal must precede row creation", jobs)
	}
	if n := readOnlySeatWorktreeCount(t, home); n != 0 {
		t.Fatalf("read-only seat worktrees = %d, want 0: the refusal must precede allocation", n)
	}
}

// BACKGROUND is the arm that actually mattered and the one the previous heads
// could not satisfy: the foreground contract gate is guarded by
// `!request.Background`, while the review arm allocates the read-only worktree
// at enqueue. Every review on this box is dispatched --background, so this is
// the real path.
func TestBackgroundRealAdapterRefusesAbsentBinaryBeforeRowOrWorktree(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ClaudeRuntime, "unused", []string{"review"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo")
	stubAbsentBinaryProbe(t)

	_, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "review", Instructions: "review it",
		PullRequest: 1926, Home: home, LeadAgent: "lead", Background: true,
		ExecsDeclaredBinary: true,
	})
	if err == nil {
		t.Fatal("a BACKGROUND real-adapter dispatch with an absent runtime binary was accepted")
	}
	if !strings.Contains(err.Error(), "does not resolve on PATH") {
		t.Fatalf("background refusal lost its cause: %v", err)
	}
	jobs, listErr := store.ListJobs(ctx)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none: a background refusal must precede row creation", jobs)
	}
	if n := readOnlySeatWorktreeCount(t, home); n != 0 {
		t.Fatalf("read-only seat worktrees = %d, want 0: a background refusal must precede allocation", n)
	}
}

// NEGATIVE ARM, and the one that keeps this from repeating the 24-test
// regression: the SAME absent binary, but the caller does not declare that it
// will exec it - which is every dispatch that delivers through an injected
// adapter. It must still enqueue.
func TestInjectedAdapterWithAbsentBinaryStillDispatches(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ClaudeRuntime, "unused", []string{"ask"}, "owner/repo")
	stubAbsentBinaryProbe(t)

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	previousAdapter := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) { return adapter, nil }
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousAdapter })

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Instructions: "hello", Home: home,
		// Deliberately NOT set: an injected adapter execs nothing.
	})
	if err != nil {
		t.Fatalf("an injected adapter with an absent binary was refused: %v", err)
	}
	if strings.TrimSpace(out.JobID) == "" {
		t.Fatal("injected-adapter dispatch produced no job")
	}
}

// PRESENT binary, real adapter, everything else equal: must dispatch. Without
// this the refusal could be keyed on the declaration alone and still pass the
// arms above.
func TestRealAdapterWithPresentBinaryStillDispatches(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")

	previous := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		// Present and satisfied: no binary-present row at all.
		return runtime.RuntimeContractResult{Runtime: runtime.ShellRuntime, Version: "stub 1.2.3", State: runtime.RuntimeContractSupported, Instrument: "binary-help"}
	}
	t.Cleanup(func() { localRuntimeContractPreflight = previous })

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	previousAdapter := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) { return adapter, nil }
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousAdapter })

	if _, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Instructions: "hello", Home: home,
		ExecsDeclaredBinary: true,
	}); err != nil {
		t.Fatalf("a present binary was refused: %v", err)
	}
}

// DAEMON CLAIM BEHAVIOUR, driven through the worker's real claim path rather
// than through the predicate helper (ruling 123815).
//
// The daemon gate is RETAINED UNCHANGED by this correction, and this test pins
// what that means: with the declared binary absent, the claim still DELIVERS -
// it does not block - and it records the honest runtime_contract_unknown fact.
// That is the behaviour the 24-test regression proved is required, because a
// claimed job's delivery may go through an adapter that never execs the binary.
// The refusal belongs at enqueue, which the two arms above cover.
func TestDaemonClaimWithAbsentBinaryStillDeliversAndRecordsUnknown(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ClaudeRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-absent-binary", Agent: "lead", Action: "implement", Repo: "owner/repo",
		Branch: "task-1", PullRequest: 31, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1",
	})

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return adapter, nil }
	worker.CommenterFactory = func(string) github.Client { return &cliPollFakeGitHub{} }
	// The claim-time probe reports the same honest absence the enqueue predicate
	// keys on. The daemon must NOT turn that into a refusal.
	worker.RuntimePreflight = func(context.Context, runtime.Agent, runtime.RuntimeContractRequest) runtime.RuntimeContractResult {
		return absentBinaryContract()
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-absent-binary")
	if err != nil {
		t.Fatal(err)
	}
	if job.State == string(workflow.JobBlocked) {
		t.Fatalf("the daemon claim BLOCKED on an absent binary; that is the 24-test regression, and the refusal belongs at enqueue")
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded: an injected adapter execs nothing and must still deliver", job.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !daemonWorkerHasEvent(events, "runtime_contract_unknown") {
		t.Fatalf("events = %+v, want runtime_contract_unknown: absence must still be recorded honestly", events)
	}
}

// THE PRODUCTION ENTRY MUST DECLARE THE REAL ADAPTER, and this is the arm that
// pins it. Every test above supplies ExecsDeclaredBinary itself, so all of them
// stay green if the production request builder stops setting it - measured: a
// mutant flipping agent.go's declaration to false SURVIVED the whole set until
// this test existed. An opt-in flag that production forgets is a gate that
// never fires, which is the quietest way for this fix to become inert.
func TestProductionDispatchEntryDeclaresRealAdapter(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ClaudeRuntime, "unused", []string{"ask"}, "owner/repo")
	stubAbsentBinaryProbe(t)

	// Through the PRODUCTION wrapper, with the declaration deliberately absent
	// from the request: the wrapper must supply it, so this refuses.
	_, err := dispatchLocalAgentJobFromCLI(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Instructions: "hello", Home: home,
	})
	if err == nil || !strings.Contains(err.Error(), "does not resolve on PATH") {
		t.Fatalf("the production dispatch entry did not declare the real adapter, so the absent-binary gate can never fire from the CLI: err=%v", err)
	}
}

// REMOTE/ATTACHED EXECUTION MUST REMAIN DISPATCHABLE: the binary lives on the
// other host, so a local PATH probe answers about the wrong machine. Pinned
// because a mutant that ignored the backend discriminator survived without it.
func TestRemoteBackendWithAbsentLocalBinaryStillDispatches(t *testing.T) {
	previousBackend := localAgentDispatchExecBackendFor
	localAgentDispatchExecBackendFor = func(string) (execbackend.Backend, error) { return execbackend.Remote, nil }
	t.Cleanup(func() { localAgentDispatchExecBackendFor = previousBackend })

	agent := runtime.Agent{Name: "responder", Runtime: runtime.ClaudeRuntime}
	previousProbe := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		return absentBinaryContract()
	}
	t.Cleanup(func() { localRuntimeContractPreflight = previousProbe })

	probes := 0
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		probes++
		return absentBinaryContract()
	}

	if err := refuseDispatchOnAbsentRuntimeBinary(context.Background(), execbackend.Remote, agent, true); err != nil {
		t.Fatalf("a remote-backend dispatch was refused on a LOCAL binary absence: %v", err)
	}
	// THE MECHANISM, not just the outcome: a remote backend must not even ask the
	// local PATH, because that answer is about the wrong machine.
	if probes != 0 {
		t.Fatalf("the local binary probe ran %d times for a REMOTE backend; it answers about the wrong host", probes)
	}
	// CONTROL, so the assertion above cannot pass because the probe is broken:
	// the same predicate on a LOCAL backend does probe, and does refuse.
	if err := refuseDispatchOnAbsentRuntimeBinary(context.Background(), execbackend.Local, agent, true); err == nil {
		t.Fatal("the local arm did not refuse, so the remote assertion proves nothing")
	}
	if probes != 1 {
		t.Fatalf("local probe invocations = %d, want 1", probes)
	}
}

// declareNoRealAdapterExec marks this test as one that drives a CLI dispatch
// entry WITHOUT ever execing a runtime binary (#1817). Its subject is dispatch
// bookkeeping, and the runner it must pass on has no runtime CLIs installed, so
// the production declaration would refuse it for an absence that never matters.
func declareNoRealAdapterExec(t *testing.T) {
	t.Helper()
	previous := cliDispatchExecsDeclaredBinary
	cliDispatchExecsDeclaredBinary = false
	t.Cleanup(func() { cliDispatchExecsDeclaredBinary = previous })
}
