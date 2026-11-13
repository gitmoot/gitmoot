package cli

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestResolveJobRuntimeOverride(t *testing.T) {
	t.Run("no override", func(t *testing.T) {
		rt, ref, err := resolveJobRuntimeOverride("", "")
		if err != nil || rt != "" || ref != "" {
			t.Fatalf("got (%q, %q, %v), want empty no-op", rt, ref, err)
		}
	})
	t.Run("session without runtime", func(t *testing.T) {
		if _, _, err := resolveJobRuntimeOverride("", "some-session"); err == nil {
			t.Fatal("accepted --session without --runtime")
		}
	})
	t.Run("unknown runtime enumerates the registry", func(t *testing.T) {
		_, _, err := resolveJobRuntimeOverride("bogus", "")
		if err == nil {
			t.Fatal("accepted unknown runtime")
		}
		for _, supported := range runtime.SupportedRuntimes() {
			if !strings.Contains(err.Error(), supported) {
				t.Fatalf("error %q must enumerate supported runtime %q", err.Error(), supported)
			}
		}
	})
	t.Run("shell requires an explicit session command", func(t *testing.T) {
		if _, _, err := resolveJobRuntimeOverride(runtime.ShellRuntime, ""); err == nil {
			t.Fatal("accepted a shell override without a session command")
		}
	})
	t.Run("explicit session is honored", func(t *testing.T) {
		rt, ref, err := resolveJobRuntimeOverride(runtime.ShellRuntime, "printf ok")
		if err != nil || rt != runtime.ShellRuntime || ref != "printf ok" {
			t.Fatalf("got (%q, %q, %v)", rt, ref, err)
		}
	})
	t.Run("last is rejected on a non-shell runtime", func(t *testing.T) {
		// SESSION SAFETY: "last" resumes whichever session in the checkout is most
		// recent — possibly an agent's default-runtime session, mid-flight — while
		// the lock key would be the literal "runtime:<rt>:last" and so could never
		// serialize with the concrete session's lock.
		for _, rt := range []string{runtime.OmpRuntime} {
			if _, _, err := resolveJobRuntimeOverride(rt, runtime.LastRef); err == nil {
				t.Fatalf("accepted --session last with --runtime %s", rt)
			}
		}
		// Shell refs are commands, not resumable sessions: a literal "last"
		// command stays a valid (if odd) shell session.
		if _, _, err := resolveJobRuntimeOverride(runtime.ShellRuntime, runtime.LastRef); err != nil {
			t.Fatalf("shell session command %q rejected: %v", runtime.LastRef, err)
		}
	})
	t.Run("removed runtime is refused", func(t *testing.T) {
		_, _, err := resolveJobRuntimeOverride("codex", "")
		if err == nil || !strings.Contains(err.Error(), `runtime "codex" is no longer supported`) {
			t.Fatalf("err = %v, want removed-runtime refusal", err)
		}
	})
	t.Run("runtime without session mints a fresh per-job ref", func(t *testing.T) {
		rt, ref, err := resolveJobRuntimeOverride(runtime.OmpRuntime, "")
		if err != nil || rt != runtime.OmpRuntime {
			t.Fatalf("got (%q, %q, %v)", rt, ref, err)
		}
		if !runtime.IsFreshRef(ref) {
			t.Fatalf("ref = %q, want a fresh ref", ref)
		}
		_, other, err := resolveJobRuntimeOverride(runtime.OmpRuntime, "")
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		if ref == other {
			t.Fatalf("fresh refs must be unique per job, got %q twice", ref)
		}
	})
}

// TestApplyJobRuntimeOverride pins the model rule and the session-safety shape
// of the effective agent: override runtime + the job's own ref, and NEVER the
// agent's configured default model (it belongs to the default runtime and may
// be invalid on the override runtime; a per-job --model flows via job.Model).
func TestApplyJobRuntimeOverride(t *testing.T) {
	agent := runtime.Agent{
		Name:           "maintainer",
		Role:           "reviewer",
		Runtime:        runtime.OmpRuntime,
		RuntimeRef:     "omp-session",
		Model:          "openai/gpt-5.5",
		Effort:         "high",
		RepoScope:      "owner/repo",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}

	unchanged := applyJobRuntimeOverride(agent, workflow.JobPayload{})
	if unchanged.Runtime != agent.Runtime || unchanged.RuntimeRef != agent.RuntimeRef || unchanged.Model != agent.Model || unchanged.Effort != agent.Effort {
		t.Fatalf("no override must be a no-op, got %+v", unchanged)
	}

	effective := applyJobRuntimeOverride(agent, workflow.JobPayload{RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok"})
	if effective.Runtime != runtime.ShellRuntime || effective.RuntimeRef != "printf ok" {
		t.Fatalf("effective runtime/ref = %q/%q", effective.Runtime, effective.RuntimeRef)
	}
	if effective.Model != "" {
		t.Fatalf("the agent's default model must never leak onto the override runtime, got %q", effective.Model)
	}
	if effective.Effort != "" {
		t.Fatalf("the agent's default effort must never leak onto the override runtime, got %q", effective.Effort)
	}
	if effective.Name != agent.Name || effective.Role != agent.Role || effective.AutonomyPolicy != agent.AutonomyPolicy {
		t.Fatalf("override must preserve identity/policy: %+v", effective)
	}
	// The stored/default agent value is untouched (value semantics, no aliasing).
	if agent.Runtime != runtime.OmpRuntime || agent.RuntimeRef != "omp-session" || agent.Model != "openai/gpt-5.5" || agent.Effort != "high" {
		t.Fatalf("input agent mutated: %+v", agent)
	}
}

func TestScopeRegisteredFreshRefForJob(t *testing.T) {
	agent := runtime.Agent{
		Name:       "council-omp",
		Runtime:    runtime.OmpRuntime,
		RuntimeRef: "fresh:council-omp",
	}

	scoped := scopeRegisteredFreshRefForJob(agent, "local-ask-root/delegation/review-codex")
	if !runtime.IsFreshRef(scoped.RuntimeRef) {
		t.Fatalf("scoped ref = %q, want fresh ref", scoped.RuntimeRef)
	}
	if scoped.RuntimeRef == agent.RuntimeRef {
		t.Fatalf("registered fresh ref was not job-scoped: %q", scoped.RuntimeRef)
	}
	again := scopeRegisteredFreshRefForJob(agent, "local-ask-root/delegation/review-codex")
	if again.RuntimeRef != scoped.RuntimeRef {
		t.Fatalf("job scoping must be deterministic: %q != %q", again.RuntimeRef, scoped.RuntimeRef)
	}
	other := scopeRegisteredFreshRefForJob(agent, "local-ask-other/delegation/review-codex")
	if other.RuntimeRef == scoped.RuntimeRef {
		t.Fatalf("different jobs must not share fresh lock refs: %q", scoped.RuntimeRef)
	}

	pinned := runtime.Agent{Name: "reviewer", Runtime: runtime.OmpRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000"}
	if got := scopeRegisteredFreshRefForJob(pinned, "job-1"); got.RuntimeRef != pinned.RuntimeRef {
		t.Fatalf("non-fresh ref changed: %q", got.RuntimeRef)
	}
}

// TestOverrideRuntimeSessionResourceKey: the lock key always names the
// OVERRIDE runtime — shell (normally lock-less) gets an override-scoped hashed
// key — so an override job can never collide with the default-runtime session
// lock.
func TestOverrideRuntimeSessionResourceKey(t *testing.T) {
	command := `printf '%s' '{"gitmoot_result":{}}'`
	key, ok := overrideRuntimeSessionResourceKey(runtime.Agent{Runtime: runtime.ShellRuntime, RuntimeRef: command})
	if !ok || !strings.HasPrefix(key, "runtime:shell:") {
		t.Fatalf("shell key = %q ok=%v, want runtime:shell:<hash>", key, ok)
	}
	if strings.Contains(key, "gitmoot_result") {
		t.Fatalf("shell key must hash the command, got %q", key)
	}

	if _, ok := overrideRuntimeSessionResourceKey(runtime.Agent{Runtime: runtime.ShellRuntime}); ok {
		t.Fatal("an empty ref must produce no key")
	}
}
