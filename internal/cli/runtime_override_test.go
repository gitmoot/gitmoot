package cli

import (
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
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
	t.Run("last is rejected on a resumable runtime", func(t *testing.T) {
		// SESSION SAFETY: "last" resumes whichever session in the checkout is most
		// recent — possibly an agent's default-runtime session, mid-flight — while
		// the lock key would be the literal "runtime:<rt>:last" and so could never
		// serialize with the concrete session's lock.
		for _, rt := range []string{runtime.ClaudeRuntime, runtime.CodexRuntime, runtime.KimiRuntime} {
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
	t.Run("resumable runtime without session mints a fresh per-job ref", func(t *testing.T) {
		rt, ref, err := resolveJobRuntimeOverride(runtime.ClaudeRuntime, "")
		if err != nil || rt != runtime.ClaudeRuntime {
			t.Fatalf("got (%q, %q, %v)", rt, ref, err)
		}
		if !runtime.IsFreshRef(ref) {
			t.Fatalf("ref = %q, want a fresh ref", ref)
		}
		_, other, err := resolveJobRuntimeOverride(runtime.ClaudeRuntime, "")
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
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     "codex-session",
		Model:          "gpt-5.5-codex",
		RepoScope:      "owner/repo",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}

	unchanged := applyJobRuntimeOverride(agent, workflow.JobPayload{})
	if unchanged.Runtime != agent.Runtime || unchanged.RuntimeRef != agent.RuntimeRef || unchanged.Model != agent.Model {
		t.Fatalf("no override must be a no-op, got %+v", unchanged)
	}

	effective := applyJobRuntimeOverride(agent, workflow.JobPayload{RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok"})
	if effective.Runtime != runtime.ShellRuntime || effective.RuntimeRef != "printf ok" {
		t.Fatalf("effective runtime/ref = %q/%q", effective.Runtime, effective.RuntimeRef)
	}
	if effective.Model != "" {
		t.Fatalf("the agent's default model must never leak onto the override runtime, got %q", effective.Model)
	}
	if effective.Name != agent.Name || effective.Role != agent.Role || effective.AutonomyPolicy != agent.AutonomyPolicy {
		t.Fatalf("override must preserve identity/policy: %+v", effective)
	}
	// The stored/default agent value is untouched (value semantics, no aliasing).
	if agent.Runtime != runtime.CodexRuntime || agent.RuntimeRef != "codex-session" || agent.Model != "gpt-5.5-codex" {
		t.Fatalf("input agent mutated: %+v", agent)
	}
}

// TestOverrideRuntimeSessionResourceKey: the lock key always names the
// OVERRIDE runtime — resumable runtimes keep the normal runtime:<rt>:<ref>
// shape, and shell (normally lock-less) gets an override-scoped hashed key —
// so an override job can never collide with the default-runtime session lock.
func TestOverrideRuntimeSessionResourceKey(t *testing.T) {
	key, ok := overrideRuntimeSessionResourceKey(runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: "abc"})
	if !ok || key != "runtime:codex:abc" {
		t.Fatalf("codex key = %q ok=%v", key, ok)
	}

	freshRef, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef: %v", err)
	}
	key, ok = overrideRuntimeSessionResourceKey(runtime.Agent{Runtime: runtime.ClaudeRuntime, RuntimeRef: freshRef})
	if !ok || key != "runtime:claude:"+freshRef {
		t.Fatalf("claude fresh key = %q ok=%v", key, ok)
	}

	command := `printf '%s' '{"gitmoot_result":{}}'`
	key, ok = overrideRuntimeSessionResourceKey(runtime.Agent{Runtime: runtime.ShellRuntime, RuntimeRef: command})
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
