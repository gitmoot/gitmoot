package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// isolatedShellPayload is a persisted shell-override stage payload that ran in
// its own detached read-only worktree (the #1016 shape the #1034 fix keys off).
func isolatedShellPayload(cmd, worktree string) workflow.JobPayload {
	return workflow.JobPayload{
		Repo:               "owner/repo",
		RuntimeOverride:    runtime.ShellRuntime,
		RuntimeOverrideRef: cmd,
		ReadOnlyWorktree:   true,
		WorktreePath:       worktree,
	}
}

func TestIsolatedShellStageRuntimeSessionKey(t *testing.T) {
	const cmd = `printf '%s' '{"gitmoot_result":{}}'`

	// An isolated shell stage keys by JOB, not by command: the key is
	// runtime:shell:job:<hash(job)> and never embeds the command.
	key, ok := isolatedShellStageRuntimeSessionKey(isolatedShellPayload(cmd, "/wt/a"), "job-a")
	if !ok || !strings.HasPrefix(key, "runtime:shell:job:") {
		t.Fatalf("isolated shell key = %q ok=%v, want runtime:shell:job:<hash>", key, ok)
	}
	if strings.Contains(key, "gitmoot_result") {
		t.Fatalf("isolated shell key must not embed the command, got %q", key)
	}

	// THE FIX: two isolated forks of the IDENTICAL command get DISTINCT keys, so
	// they never serialize on one another (#1034).
	keyA, okA := isolatedShellStageRuntimeSessionKey(isolatedShellPayload(cmd, "/wt/a"), "job-a")
	keyB, okB := isolatedShellStageRuntimeSessionKey(isolatedShellPayload(cmd, "/wt/b"), "job-b")
	if !okA || !okB {
		t.Fatalf("both isolated forks must key: okA=%v okB=%v", okA, okB)
	}
	if keyA == keyB {
		t.Fatalf("identical-command isolated forks must get distinct keys, both = %q", keyA)
	}

	// THE GUARANTEE IT MUST NOT BREAK: a NON-isolated shell stage (shared
	// checkout — no worktree) keeps the command-hash key, so two identical
	// commands still share a key and stay serialized.
	nonIso := workflow.JobPayload{Repo: "owner/repo", RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: cmd}
	if _, ok := isolatedShellStageRuntimeSessionKey(nonIso, "job-a"); ok {
		t.Fatal("a non-isolated shell stage must not get a job-scoped key")
	}
	sharedA, _ := overrideRuntimeSessionResourceKey(applyJobRuntimeOverride(runtime.Agent{}, nonIso))
	sharedB, _ := overrideRuntimeSessionResourceKey(applyJobRuntimeOverride(runtime.Agent{}, nonIso))
	if sharedA != sharedB || !strings.HasPrefix(sharedA, "runtime:shell:") || strings.HasPrefix(sharedA, "runtime:shell:job:") {
		t.Fatalf("non-isolated identical commands must share the command-hash key, got %q / %q", sharedA, sharedB)
	}

	// A resumable-runtime override with a worktree must NOT be job-scoped here:
	// only shell overrides get the treatment (codex keeps its session identity).
	codexIso := workflow.JobPayload{Repo: "owner/repo", RuntimeOverride: runtime.CodexRuntime, RuntimeOverrideRef: "abc", ReadOnlyWorktree: true, WorktreePath: "/wt/c"}
	if _, ok := isolatedShellStageRuntimeSessionKey(codexIso, "job-c"); ok {
		t.Fatal("a resumable-runtime override must not be treated as an isolated shell stage")
	}

	// COLLISION INVARIANT (#531): the job-scoped shell key still names the shell
	// runtime, so it can never collide with a resumable runtime's key or with the
	// command-hash key for the same command.
	if key == sharedA {
		t.Fatalf("isolated key %q must differ from the command-hash key %q", key, sharedA)
	}
	if !strings.HasPrefix(key, "runtime:shell:") {
		t.Fatalf("isolated key must stay shell-namespaced, got %q", key)
	}

	// Edge: worktree opted-in but path not allocated (fail-open fallback) → no
	// job-scoped key, so the normal derivation stays in force.
	if _, ok := isolatedShellStageRuntimeSessionKey(isolatedShellPayload(cmd, ""), "job-a"); ok {
		t.Fatal("an isolated shell stage with no worktree path must not get a job-scoped key")
	}
	// Edge: empty job id cannot be scoped.
	if _, ok := isolatedShellStageRuntimeSessionKey(isolatedShellPayload(cmd, "/wt/a"), ""); ok {
		t.Fatal("an empty job id must not get a job-scoped key")
	}
}

// TestQueuedJobRuntimeResourceKeyIsolatedShellStage proves the daemon SELECTOR
// gates on exactly the key the worker ACQUIRES: both resolve through
// isolatedShellStageRuntimeSessionKey, so the gate and the lock can never
// disagree (a mismatch would let two forks pass the gate and then collide at
// acquisition — the bug this fix must not introduce).
func TestQueuedJobRuntimeResourceKeyIsolatedShellStage(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	const cmd = `echo hi`

	newJob := func(id, worktree string) db.Job {
		encoded, err := json.Marshal(isolatedShellPayload(cmd, worktree))
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return db.Job{ID: id, Agent: "runner", Payload: string(encoded)}
	}

	jobA := newJob("pipe-run-stageX-0", "/wt/a")
	jobB := newJob("pipe-run-stageX-1", "/wt/b")

	gateA := queuedJobRuntimeResourceKey(ctx, store, jobA)
	gateB := queuedJobRuntimeResourceKey(ctx, store, jobB)

	// The selector returns the job-scoped key...
	if !strings.HasPrefix(gateA, "runtime:shell:job:") {
		t.Fatalf("selector key = %q, want runtime:shell:job:<hash>", gateA)
	}
	// ...distinct per job (identical command, different jobs), so the daemon
	// dispatches them concurrently instead of bouncing the second on the first.
	if gateA == gateB {
		t.Fatalf("identical-command isolated forks must gate on distinct keys, both = %q", gateA)
	}

	// gate == lock: the selector key equals the key the worker acquisition path
	// derives from the same payload + job id.
	lockA, ok := isolatedShellStageRuntimeSessionKey(isolatedShellPayload(cmd, "/wt/a"), jobA.ID)
	if !ok || gateA != lockA {
		t.Fatalf("selector key %q must equal the acquisition key %q (ok=%v)", gateA, lockA, ok)
	}
}
