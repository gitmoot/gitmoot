package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestForegroundRuntimePreflightUnsupportedRefusesBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")

	previous := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		return runtime.RuntimeContractResult{
			Runtime: runtime.ShellRuntime, Version: "stub 1.2.3", State: runtime.RuntimeContractUnsupported, Instrument: "binary-help",
			Requirements: []runtime.RuntimeRequirementResult{{Name: "flag --required", Source: "internal/runtime/test::args", Remedy: "install a compatible runtime", State: runtime.RuntimeContractUnsupported}},
		}
	}
	t.Cleanup(func() { localRuntimeContractPreflight = previous })

	_, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Instructions: "hello", Home: home})
	if err == nil || !strings.Contains(err.Error(), "--required") {
		t.Fatalf("dispatch error = %v, want greppable runtime preflight refusal", err)
	}
	jobs, listErr := store.ListJobs(ctx)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want no enqueue after foreground refusal", jobs)
	}
}

func TestForegroundRuntimePreflightUnknownRecordsAndDispatches(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")

	previousPreflight := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		return runtime.RuntimeContractResult{Runtime: runtime.ShellRuntime, Version: "unknown", State: runtime.RuntimeContractUnknown, Instrument: "binary-help"}
	}
	t.Cleanup(func() { localRuntimeContractPreflight = previousPreflight })
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	previousAdapter := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) { return adapter, nil }
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousAdapter })

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Instructions: "hello", Home: home})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob: %v", err)
	}
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !daemonWorkerHasEvent(events, "runtime_contract_unknown") {
		t.Fatalf("events = %+v, want runtime_contract_unknown", events)
	}
}
