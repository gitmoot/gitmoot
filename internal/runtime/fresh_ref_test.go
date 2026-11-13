package runtime

import (
	"context"
	"strings"
	"testing"
)

// TestSupportedRuntimesMatchFactory proves the registry enumeration
// (SupportedRuntimes) and the actual adapter Factory cannot drift: every
// enumerated name constructs an adapter whose Name() matches, and an unknown
// name fails with an error that lists the enumerated set.
func TestSupportedRuntimesMatchFactory(t *testing.T) {
	names := SupportedRuntimes()
	if len(names) == 0 {
		t.Fatal("SupportedRuntimes returned no runtimes")
	}
	for _, name := range names {
		adapter, err := (Factory{}).Adapter(name)
		if err != nil {
			t.Fatalf("Factory.Adapter(%q) returned error: %v", name, err)
		}
		if adapter.Name() != name {
			t.Fatalf("adapter for %q reports Name() = %q", name, adapter.Name())
		}
	}
	if _, err := (Factory{}).Adapter("bogus"); err == nil {
		t.Fatal("Factory.Adapter accepted unknown runtime")
	} else {
		for _, name := range names {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("unsupported-runtime error %q must enumerate %q", err.Error(), name)
			}
		}
	}
}

func TestNewFreshRef(t *testing.T) {
	ref, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	if !IsFreshRef(ref) {
		t.Fatalf("NewFreshRef() = %q, want fresh: prefix", ref)
	}
	if !isUUID(strings.TrimPrefix(ref, FreshRefPrefix)) {
		t.Fatalf("NewFreshRef() suffix must be a UUID, got %q", ref)
	}
	other, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	if ref == other {
		t.Fatalf("fresh refs must be unique per mint, got %q twice", ref)
	}
	if IsFreshRef("550e8400-e29b-41d4-a716-446655440000") {
		t.Fatal("a plain UUID must not be a fresh ref")
	}
}

func TestFreshRefForJob(t *testing.T) {
	ref := FreshRefForJob("local-ask-a/delegation/review-codex")
	if !IsFreshRef(ref) {
		t.Fatalf("FreshRefForJob() = %q, want fresh ref", ref)
	}
	if !strings.HasPrefix(ref, FreshRefPrefix+"job:") {
		t.Fatalf("FreshRefForJob() = %q, want job-scoped suffix", ref)
	}
	if other := FreshRefForJob("local-ask-b/delegation/review-codex"); other == ref {
		t.Fatalf("different jobs must get different fresh refs: %q", ref)
	}
	if again := FreshRefForJob("local-ask-a/delegation/review-codex"); again != ref {
		t.Fatalf("FreshRefForJob must be deterministic: %q != %q", again, ref)
	}
}

// TestValidateAgentAcceptsFreshRefs: the session-safety path mints fresh refs
// for override jobs; agent validation must accept them.
func TestValidateAgentAcceptsFreshRefs(t *testing.T) {
	ref, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	for _, runtimeName := range []string{OmpRuntime} {
		agent := Agent{Name: "audit", Role: "reviewer", Runtime: runtimeName, RuntimeRef: ref, RepoScope: "gitmoot/gitmoot"}
		if err := ValidateAgent(agent); err != nil {
			t.Fatalf("ValidateAgent rejected fresh ref for %s: %v", runtimeName, err)
		}
	}
}

// TestShellDeliverFreshRefErrors: shell sessions are commands; a fresh ref can
// never be executed and must fail with an actionable error instead of running
// "sh -c fresh:<uuid>".
func TestShellDeliverFreshRefErrors(t *testing.T) {
	ref, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	runner := &fakeRunner{}
	adapter := ShellAdapter{Runner: runner}
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: ref, RepoScope: "gitmoot/gitmoot"}

	_, err = adapter.Deliver(context.Background(), agent, Job{Prompt: "do the thing"})
	if err == nil {
		t.Fatal("Deliver accepted a fresh ref on the shell runtime")
	}
	if !strings.Contains(err.Error(), "session command") {
		t.Fatalf("error %q must point at the explicit session command requirement", err.Error())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("no command may run for a fresh shell ref, got %v", runner.calls)
	}
}
