package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// A read-only seat's runtime state dir is built EMPTY by
// prepareReadOnlyRuntimeState (RemoveAll + MkdirAll, credential file only), so
// the isolated home cannot contain the session a concrete ref names. Measured
// failure: codex review job local-review-g7-review-18d1b8d23a6061a8 reached
// running and died with "thread/resume: no rollout found for thread id
// 019fa4c8-69c1-7bc2-8628-00ade8fa43c5" while that rollout sat in the real
// ~/.codex/sessions tree the seat is sandboxed away from.
func TestApplyReadOnlySeatRunsOnFreshSessionNotTheAgentsOwn(t *testing.T) {
	const storedRef = "019fa4c8-69c1-7bc2-8628-00ade8fa43c5"
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: storedRef}

	if err := applyReadOnlySeat(true, "/profiles/reviewer", "local-review-g7-review-18d1b8d2", &agent); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}

	if agent.RuntimeRef == storedRef {
		t.Fatalf("read-only seat kept the agent's resumable ref %q; the isolated home holds no session for it", storedRef)
	}
	if !runtime.IsFreshRef(agent.RuntimeRef) {
		t.Fatalf("read-only seat ref = %q, want a fresh: ref so the runtime starts a new session instead of resuming", agent.RuntimeRef)
	}
	if want := runtime.FreshRefForJob("local-review-g7-review-18d1b8d2"); agent.RuntimeRef != want {
		t.Fatalf("read-only seat ref = %q, want the job-scoped %q", agent.RuntimeRef, want)
	}
}

// The seat isolates DELIVERY only. Locking must stay on the agent's registered
// session so #684 serialization is unchanged (that invariant is exercised end
// to end by TestRunAgentReviewRequeuesQueuedJobWhenRuntimeSessionBusy, which
// drives `agent review` with the registered session held by another owner).
// applyReadOnlySeat takes a pointer, so callers keep a pre-seat copy for the
// lock; this test pins that a copy taken before the call is unaffected.
func TestApplyReadOnlySeatLeavesTheSessionLockAgentIntact(t *testing.T) {
	stored := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: "019fa4c8-69c1-7bc2-8628-00ade8fa43c5"}
	sessionLockAgent := stored
	delivery := stored
	if err := applyReadOnlySeat(true, "", "job-seat-1", &delivery); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}

	lockKey, ok := runtimeSessionResourceKey(sessionLockAgent)
	if !ok {
		t.Fatal("registered codex session must have a lock key")
	}
	storedKey, _ := runtimeSessionResourceKey(stored)
	if lockKey != storedKey {
		t.Fatalf("session lock key = %q, want the registered %q", lockKey, storedKey)
	}
	deliveryKey, _ := runtimeSessionResourceKey(delivery)
	if deliveryKey == lockKey {
		t.Fatalf("delivery ref %q was not isolated from the locked session", delivery.RuntimeRef)
	}
}

// An already-fresh ref is a per-job runtime override's minted ref or a
// registered fresh:<seat> ref scoped by scopeRegisteredFreshRefForJob. Both are
// unique per job AND are the keys the scheduler gate computed, so rewriting
// them here would desync gate from acquisition.
func TestApplyReadOnlySeatKeepsAnAlreadyFreshRef(t *testing.T) {
	minted, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef: %v", err)
	}
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: minted}
	if err := applyReadOnlySeat(true, "", "job-override", &agent); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	if agent.RuntimeRef != minted {
		t.Fatalf("override ref rewritten to %q, want the enqueue-minted %q", agent.RuntimeRef, minted)
	}
}

// An ordinary (non-seat) job keeps its resumable session: this fix must not
// silently convert every job into a fresh session.
func TestApplyReadOnlySeatLeavesOrdinaryJobsResumable(t *testing.T) {
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: "019fa4c8-69c1-7bc2-8628-00ade8fa43c5"}
	if err := applyReadOnlySeat(false, "/profiles/reviewer", "job-ordinary", &agent); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	if agent.RuntimeRef != "019fa4c8-69c1-7bc2-8628-00ade8fa43c5" || agent.ReadOnlySeat {
		t.Fatalf("ordinary job mutated: ref=%q seat=%v", agent.RuntimeRef, agent.ReadOnlySeat)
	}
}

// A shell ref is a COMMAND, not a resumable session, and isolated shell
// pipeline stages run with ReadOnlySeat=true. Rewriting that ref to a fresh
// session ref would replace the stage's work with nothing, which is exactly
// what the first version of this fix did to every shell pipeline E2E.
func TestApplyReadOnlySeatKeepsShellCommandRef(t *testing.T) {
	const command = `printf '%s' '{"gitmoot_result":{}}'`
	agent := runtime.Agent{Runtime: runtime.ShellRuntime, RuntimeRef: command}
	if err := applyReadOnlySeat(true, "", "pipe-run-stage-0", &agent); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	if agent.RuntimeRef != command {
		t.Fatalf("shell command ref rewritten to %q, want %q", agent.RuntimeRef, command)
	}
	if !agent.ReadOnlySeat {
		t.Fatal("shell stage lost its read-only seat marker")
	}
}

// The daemon SELECTOR gates on exactly the key the worker ACQUIRES, and a
// read-only seat must not change that: the worker locks the agent's registered
// session (jobWorker.run keeps a pre-seat copy) and the selector reads the
// stored agent, so both sides keep naming one key. A seat that moved its LOCK
// to a job-scoped ref would have to move the gate too, or the two disagree,
// which is the #1034 shape.
func TestQueuedJobRuntimeResourceKeyReadOnlySeat(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	stored := db.Agent{
		Name:       "gm-review-codex",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: "019fa4c8-69c1-7bc2-8628-00ade8fa43c5",
	}
	if err := store.UpsertAgent(ctx, stored); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	newJob := func(id string, seat bool) db.Job {
		encoded, err := json.Marshal(workflow.JobPayload{
			Repo:             "gitmoot/gitmoot",
			ReadOnlySeat:     seat,
			ReadOnlyWorktree: seat,
			WorktreePath:     "/wt/" + id,
		})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return db.Job{ID: id, Agent: stored.Name, Payload: string(encoded)}
	}

	storedKey, ok := runtimeSessionResourceKey(runtimeAgent(stored))
	if !ok {
		t.Fatal("registered codex session must have a lock key")
	}

	seatJob := newJob("local-review-seat-a", true)
	if gate := queuedJobRuntimeResourceKey(ctx, store, seatJob); gate != storedKey {
		t.Fatalf("seat gate = %q, want the registered session key %q", gate, storedKey)
	}
	if gate := queuedJobRuntimeResourceKey(ctx, store, newJob("implement-a", false)); gate != storedKey {
		t.Fatalf("ordinary gate = %q, want the registered session key %q", gate, storedKey)
	}

	// ...and the seat still delivers on an isolated fresh session, so the key it
	// gates on is deliberately NOT the session it talks to.
	delivery := runtimeAgent(stored)
	if err := applyReadOnlySeat(true, "", seatJob.ID, &delivery); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	if !runtime.IsFreshRef(delivery.RuntimeRef) {
		t.Fatalf("seat delivery ref = %q, want a fresh ref", delivery.RuntimeRef)
	}
}

// Every registered runtime must declare a seat staging policy. The seat has
// omitted a required startup input three times (openssl.cnf, codex sessions/,
// kimi config.toml), and the third one shipped because nothing forced a runtime
// to say what it reads at startup. A new runtime added to the registry without
// a policy fails here rather than at dispatch.
func TestReadOnlySeatStatePolicyCoversEveryRegisteredRuntime(t *testing.T) {
	userHome := t.TempDir()
	for _, name := range runtime.SupportedRuntimes() {
		t.Run(name, func(t *testing.T) {
			policy, needsState, err := readOnlySeatStatePolicyFor(name, userHome, false)
			switch name {
			case runtime.OmpRuntime:
				if err == nil {
					t.Fatal("omp must be refused: it has no isolated credential broker")
				}
				return
			case runtime.ShellRuntime:
				if err != nil || needsState {
					t.Fatalf("shell needsState=%v err=%v, want no isolated state and no error", needsState, err)
				}
				return
			}
			if err != nil || !needsState {
				t.Fatalf("%s needsState=%v err=%v, want a declared policy", name, needsState, err)
			}
			if strings.TrimSpace(policy.relativeState) == "" {
				t.Fatalf("%s policy declares no staging location", name)
			}
			if strings.TrimSpace(policy.defaultSourceDir) == "" {
				t.Fatalf("%s policy declares no host source dir", name)
			}
			if policy.credentialFile == "" && len(policy.requiredInputs) == 0 {
				t.Fatalf("%s policy stages nothing: a seat with an empty state dir starts and then refuses", name)
			}
		})
	}
}

// An unregistered runtime must be refused rather than silently given an empty
// state dir.
func TestReadOnlySeatStatePolicyRefusesUnknownRuntime(t *testing.T) {
	if _, _, err := readOnlySeatStatePolicyFor("codxe", t.TempDir(), false); err == nil {
		t.Fatal("unknown runtime must be refused")
	}
}

// A required input the host does not have must fail with a message that NAMES
// the file. The runtime's own diagnostic for this case ("No model configured",
// behind an auth error telling the reader to run kimi login) is what cost a day.
func TestPrepareReadOnlyRuntimeStateNamesAMissingRequiredInput(t *testing.T) {
	sourceDir := t.TempDir()
	credentialDir := filepath.Join(sourceDir, "credentials")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, "kimi-code.json"), []byte(`{"access_token":"host"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{Runtime: runtime.KimiRuntime, RuntimeConfigDir: sourceDir, ReadOnlySeat: true}

	_, _, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err == nil {
		t.Fatal("a kimi seat without config.toml must be refused, not launched")
	}
	if !strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("error must name the missing input, got: %v", err)
	}

	// With the input present the same call stages it verbatim.
	if err := os.WriteFile(filepath.Join(sourceDir, "config.toml"), []byte("default_model = \"kimi-code/k3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir, _, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err != nil {
		t.Fatalf("prepareReadOnlyRuntimeState: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(stateDir, "config.toml"))
	if err != nil || !strings.Contains(string(staged), "default_model") {
		t.Fatalf("staged config.toml = %q, err=%v", staged, err)
	}
}
