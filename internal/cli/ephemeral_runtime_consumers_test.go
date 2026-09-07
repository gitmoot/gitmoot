package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1952 correction round (directive 126863). The wall fix was correct but the
// CENSUS was not: three more execute-path consumers still resolved a queued
// job's runtime from the agents table while the job ran on its ephemeral spec.
// The class rule is override > ephemeral spec > registered row on every
// execute-path decision — refuse, hold, probe, reserve, serialize.
//
// Every test here plants a same-name agent row of a DIFFERENT runtime, because
// that is the condition under which the old code looked right.

func ephemeralConsumerStore(t *testing.T) (*db.Store, string) {
	t.Helper()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	return store, home
}

func ephemeralClaudePayload() workflow.JobPayload {
	return workflow.JobPayload{
		Repo:      "owner/repo",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.ClaudeRuntime},
	}
}

// countClaudeLiveChecks swaps the live Claude credential check for a counter.
// The count is the whole point: the defect was ZERO live probes for a job that
// runs on Claude, and "did it probe" cannot be inferred from the verdict alone
// because Unknown is also what a probe-less path returns.
func countClaudeLiveChecks(t *testing.T, result error) *int {
	t.Helper()
	calls := 0
	original := claudeAuthLiveCheck
	claudeAuthLiveCheck = func(context.Context, subprocess.Runner, string, []string) error {
		calls++
		return result
	}
	t.Cleanup(func() { claudeAuthLiveCheck = original })
	return &calls
}

// TestAuthProbeProbesTheEphemeralSpecRuntimeNotTheStoredRow is the P2 auth-probe
// regression. Stored row says shell, the spec says Claude, so the job runs on
// Claude — the probe must actually contact Claude and return a real verdict.
// Before the fix: zero probes and Unknown, and because authProbeAllowsRedispatch
// releases Unknown, a Claude-auth-deferred job re-dispatched with no credential
// check at all.
func TestAuthProbeProbesTheEphemeralSpecRuntimeNotTheStoredRow(t *testing.T) {
	store, home := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-probe", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	worker := defaultJobWorker(store, io.Discard, home)
	job := db.Job{ID: "eph-auth-probe", Agent: "eph-probe", Type: "ask"}

	calls := countClaudeLiveChecks(t, errors.Join(errors.New("rejected"), runtime.ErrClaudeAuthFailed))
	verdict := worker.defaultAuthProbe(context.Background(), job, ephemeralClaudePayload())

	if *calls == 0 {
		t.Fatalf("live Claude probe calls = 0: the job runs on Claude but nothing validated its credential")
	}
	if verdict == authProbeUnknown {
		t.Fatalf("verdict = unknown after %d live probe(s); an unknown verdict is released by authProbeAllowsRedispatch", *calls)
	}
	if verdict != authProbeInvalid {
		t.Fatalf("verdict = %v, want invalid: the stubbed credential check rejected", verdict)
	}
}

// TestAuthProbeLeavesANonEphemeralNonClaudeJobAlone is the should-SUCCEED
// control: the fix must not make everything probe Claude. An ordinary shell
// agent job still performs zero live checks and stays Unknown.
func TestAuthProbeLeavesANonEphemeralNonClaudeJobAlone(t *testing.T) {
	store, home := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "shell-only", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	worker := defaultJobWorker(store, io.Discard, home)
	job := db.Job{ID: "plain-shell-probe", Agent: "shell-only", Type: "ask"}

	calls := countClaudeLiveChecks(t, nil)
	if verdict := worker.defaultAuthProbe(context.Background(), job, workflow.JobPayload{Repo: "owner/repo"}); verdict != authProbeUnknown {
		t.Fatalf("non-Claude verdict = %v, want unknown", verdict)
	}
	if *calls != 0 {
		t.Fatalf("live Claude probe calls = %d for a shell job, want 0", *calls)
	}
}

// TestAuthProbeDedupKeyDistinguishesEphemeralRuntimes: the dedup key names the
// credential domain, so two ephemeral jobs on different runtimes must not share
// one verdict. With the key read off the stored row they collided on
// "runtime:shell" and one probe's result decided both.
func TestAuthProbeDedupKeyDistinguishesEphemeralRuntimes(t *testing.T) {
	store, home := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-probe", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	worker := defaultJobWorker(store, io.Discard, home)
	job := db.Job{ID: "eph-dedup", Agent: "eph-probe", Type: "ask"}
	ctx := context.Background()

	claudeKey := worker.authProbeDedupKey(ctx, job, ephemeralClaudePayload())
	codexPayload := ephemeralClaudePayload()
	codexPayload.Ephemeral = &workflow.EphemeralSpec{Runtime: runtime.CodexRuntime}
	codexKey := worker.authProbeDedupKey(ctx, job, codexPayload)
	storedKey := worker.authProbeDedupKey(ctx, job, workflow.JobPayload{Repo: "owner/repo"})

	if claudeKey == codexKey {
		t.Fatalf("ephemeral Claude and Codex jobs share the probe key %q: one verdict would decide both", claudeKey)
	}
	if claudeKey == storedKey {
		t.Fatalf("ephemeral Claude job shares the stored shell agent's key %q", storedKey)
	}
}

// TestAdmissionEstimateChargesTheEphemeralSpecRuntime is the P2 admission
// regression, asserting the measured VALUES (session true, Claude's 0.85 GB
// prior) rather than pinning a helper. Before the fix an ephemeral session
// evaded both opt-in admission caps entirely: session false, 0 GB.
func TestAdmissionEstimateChargesTheEphemeralSpecRuntime(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	// The stale same-name row is shell: not a session runtime at all, which is
	// how a real Claude session came to be counted as free.
	seedDaemonWorkerAgent(t, store, "eph-admit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "eph-admission", Agent: "eph-admit", Action: "ask", Repo: "owner/repo", Branch: "main",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.ClaudeRuntime},
	})
	job := mustWorkerJob(t, store, "eph-admission")
	policy := config.DefaultAdmissionPolicy()

	got := perJobAdmissionEstimate(context.Background(), store, job, policy)
	if !got.session || got.memGB != policy.ClaudeMemoryGB {
		t.Fatalf("estimate = %+v, want session=true memGB=%v (Claude's prior)", got, policy.ClaudeMemoryGB)
	}
	if key := queuedJobRuntimeResourceKey(context.Background(), store, job); key == "" {
		t.Fatalf("resource key is empty for a job that will hold a Claude session")
	}
}

// TestAdmissionEstimateStillFreesAGenuinelySessionlessJob is the should-SUCCEED
// control for admission: the fix must not reserve RAM for everything. A plain
// shell job takes no session and is charged nothing.
func TestAdmissionEstimateStillFreesAGenuinelySessionlessJob(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "shell-admit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "shell-admission", Agent: "shell-admit", Action: "ask", Repo: "owner/repo", Branch: "main",
	})
	job := mustWorkerJob(t, store, "shell-admission")

	got := perJobAdmissionEstimate(context.Background(), store, job, config.DefaultAdmissionPolicy())
	if got.session || got.memGB != 0 {
		t.Fatalf("estimate = %+v, want session=false memGB=0 for a session-less shell job", got)
	}
}

// TestEphemeralResourceKeyMatchesTheLockTheWorkerTakes pins the property that
// made the job-id form correct rather than merely non-empty: this key is a
// SERIALIZATION key (runtimeResourceLocked, inflightRuntimes), and
// runtime_override.go requires the scheduler gate and the worker's lock
// acquisition to agree. jobWorker.run rewrites a fresh ref to
// runtime.FreshRefForJob(job.ID) before locking, so the gate must produce that
// same string — and it must stay job-unique so two ephemeral jobs do not
// serialize against each other.
func TestEphemeralResourceKeyMatchesTheLockTheWorkerTakes(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-key", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	for _, id := range []string{"eph-key-a", "eph-key-b"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: id, Agent: "eph-key", Action: "ask", Repo: "owner/repo", Branch: "main",
			Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.ClaudeRuntime},
		})
	}
	ctx := context.Background()
	keyA := queuedJobRuntimeResourceKey(ctx, store, mustWorkerJob(t, store, "eph-key-a"))
	keyB := queuedJobRuntimeResourceKey(ctx, store, mustWorkerJob(t, store, "eph-key-b"))

	want, ok := runtimeSessionResourceKey(runtime.Agent{
		Runtime:    runtime.ClaudeRuntime,
		RuntimeRef: runtime.FreshRefForJob("eph-key-a"),
	})
	if !ok {
		t.Fatal("runtimeSessionResourceKey refused the worker's own fresh-ref form")
	}
	if keyA != want {
		t.Fatalf("gate key = %q, worker locks %q: gate and acquisition must agree", keyA, want)
	}
	if keyA == keyB {
		t.Fatalf("two ephemeral jobs share the key %q and would falsely serialize", keyA)
	}
}

// TestEphemeralResourceKeyTakesNoSessionForANonResumableSpec: a shell spec takes
// no session lock, exactly as a shell-registered agent does not. Without this a
// "make it non-empty" fix would invent a session for every ephemeral job.
func TestEphemeralResourceKeyTakesNoSessionForANonResumableSpec(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-shell", runtime.ClaudeRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "eph-shell-job", Agent: "eph-shell", Action: "ask", Repo: "owner/repo", Branch: "main",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.ShellRuntime},
	})
	job := mustWorkerJob(t, store, "eph-shell-job")
	if key := queuedJobRuntimeResourceKey(context.Background(), store, job); key != "" {
		t.Fatalf("shell-spec ephemeral job took session key %q, want none", key)
	}
	got := perJobAdmissionEstimate(context.Background(), store, job, config.DefaultAdmissionPolicy())
	if got.session || got.memGB != 0 {
		t.Fatalf("estimate = %+v, want session=false memGB=0", got)
	}
}
