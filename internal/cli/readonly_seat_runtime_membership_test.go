package cli

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestResumableSessionRuntimeMembershipIsPinned pins the membership of
// resumableSessionRuntime, and pins it THROUGH BOTH consumers rather than by
// calling the predicate alone.
//
// One token feeds two production paths: applyReadOnlySeat's seat fresh-ref
// rewrite, and runtimeSessionResourceKey's lock key. Dropping a runtime from
// the set silently disables BOTH for it - the seat resumes the reviewer's real
// session instead of a fresh one, and the session stops locking - so a mutation
// that removes a name must fail here rather than anywhere downstream.
//
// A runtime is added to this set when its RuntimeRef names a resumable session.
// If that is genuinely no longer true for one of these, change this table in
// the same commit and say why; do not delete the assertion.
func TestResumableSessionRuntimeMembershipIsPinned(t *testing.T) {
	resumable := map[string]string{
		runtime.CodexRuntime:  "019fa4c8-69c1-7bc2-8628-00ade8fa43c5",
		runtime.ClaudeRuntime: "019fa4c8-69c1-7bc2-8628-00ade8fa43c6",
		runtime.KimiRuntime:   "019fa4c8-69c1-7bc2-8628-00ade8fa43c7",
	}
	for runtimeName, ref := range resumable {
		t.Run(runtimeName, func(t *testing.T) {
			if !resumableSessionRuntime(runtimeName) {
				t.Fatalf("resumableSessionRuntime(%q) = false: its seat would resume the reviewer's registered session and its lock key would vanish", runtimeName)
			}

			// Consumer 1: the seat must be rewritten onto a fresh per-job ref.
			agent := runtime.Agent{Runtime: runtimeName, RuntimeRef: ref}
			if err := applyReadOnlySeat(true, "", "job-membership", &agent); err != nil {
				t.Fatalf("applyReadOnlySeat: %v", err)
			}
			if agent.RuntimeRef == ref {
				t.Fatalf("%s seat kept the registered ref %q: seat isolation is off for this runtime", runtimeName, ref)
			}
			if !runtime.IsFreshRef(agent.RuntimeRef) {
				t.Fatalf("%s seat ref = %q, want a fresh ref", runtimeName, agent.RuntimeRef)
			}

			// Consumer 2: the registered session must still have a lock key.
			if _, ok := runtimeSessionResourceKey(runtime.Agent{Runtime: runtimeName, RuntimeRef: ref}); !ok {
				t.Fatalf("%s registered session has no lock key: its runtime sessions stopped serializing", runtimeName)
			}
		})
	}

	// The set is not "every runtime": a shell command ref is not a resumable
	// session, and pinning that keeps the assertion above honest.
	if resumableSessionRuntime(runtime.ShellRuntime) {
		t.Errorf("resumableSessionRuntime(%q) = true, want false: a shell command ref is not a resumable session", runtime.ShellRuntime)
	}
}
