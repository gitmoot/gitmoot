package cli

import (
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// An ordinary (non-seat) job is left untouched: the seat must not silently
// rewrite every job's delivery.
func TestApplyReadOnlySeatLeavesOrdinaryJobsResumable(t *testing.T) {
	agent := runtime.Agent{Runtime: runtime.OmpRuntime, RuntimeRef: "019fa4c8-69c1-7bc2-8628-00ade8fa43c5"}
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

// Every registered runtime must declare a seat state policy. A new runtime
// added to the registry without a policy fails here rather than at dispatch.
func TestReadOnlySeatStatePolicyCoversEveryRegisteredRuntime(t *testing.T) {
	for _, name := range runtime.SupportedRuntimes() {
		t.Run(name, func(t *testing.T) {
			policy, needsState, err := readOnlySeatStatePolicyFor(name)
			switch name {
			case runtime.OmpRuntime:
				if err != nil || !needsState {
					t.Fatalf("omp needsState=%v err=%v, want isolated writable state", needsState, err)
				}
				if policy.relativeState != filepath.Join(".omp", "agent") ||
					policy.stateEnv != "PI_CODING_AGENT_DIR" ||
					policy.stateRootEnv != "PI_CONFIG_DIR" {
					t.Fatalf("omp state policy = %+v, want job-private OMP state and config roots", policy)
				}
			case runtime.ShellRuntime:
				if err != nil || needsState {
					t.Fatalf("shell needsState=%v err=%v, want no isolated state and no error", needsState, err)
				}
			default:
				t.Fatalf("registered runtime %q has no expected seat state policy in this test", name)
			}
		})
	}
}

// An unregistered or removed runtime must be refused rather than silently
// given an empty state dir.
func TestReadOnlySeatStatePolicyRefusesUnknownRuntime(t *testing.T) {
	for _, name := range []string{"codxe", "codex", "claude", "kimi"} {
		if _, _, err := readOnlySeatStatePolicyFor(name); err == nil {
			t.Fatalf("runtime %q must be refused", name)
		}
	}
}
