package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
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

// TestValidateAgentAcceptsFreshRefs: the session-safety path mints fresh refs
// for claude/kimi override jobs; agent validation must accept them.
func TestValidateAgentAcceptsFreshRefs(t *testing.T) {
	ref, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	for _, runtimeName := range []string{CodexRuntime, ClaudeRuntime, KimiRuntime, KimiCLIRuntime} {
		agent := Agent{Name: "audit", Role: "reviewer", Runtime: runtimeName, RuntimeRef: ref, RepoScope: "jerryfane/gitmoot"}
		if err := ValidateAgent(agent); err != nil {
			t.Fatalf("ValidateAgent rejected fresh ref for %s: %v", runtimeName, err)
		}
	}
}

// TestCodexDeliverFreshRef: a fresh ref must start a brand-new `codex exec`
// session — no `resume`, no session verification against the stored index —
// with the per-job model passed through.
func TestCodexDeliverFreshRef(t *testing.T) {
	ref, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "fresh output"}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: CodexRuntime, RuntimeRef: ref, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "do the thing", Model: "gpt-5.5-codex"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Raw != "fresh output" {
		t.Fatalf("Raw = %q", result.Raw)
	}
	runner.want(t, 0, "codex", "exec", "--json", "--model", "gpt-5.5-codex", "--", "do the thing")
	for _, arg := range runner.calls[0] {
		if arg == "resume" {
			t.Fatalf("fresh-ref delivery must not resume any session: %v", runner.calls[0])
		}
	}
}

// TestClaudeDeliverFreshRef: a fresh ref must deliver on a brand-new dedicated
// --session-id (never --resume/--continue), never return a RefreshedRuntimeRef
// (nothing may be re-pinned), and pass the per-job model through.
func TestClaudeDeliverFreshRef(t *testing.T) {
	ref, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	minted := "11111111-1111-4111-8111-111111111111"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"fresh claude output"}`}}}
	adapter := ClaudeAdapter{Runner: runner, NewRuntimeRef: func() (string, error) { return minted, nil }}
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: ref, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "do the thing", Model: "claude-opus-4"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "fresh claude output" {
		t.Fatalf("Summary = %q", result.Summary)
	}
	if result.RefreshedRuntimeRef != "" {
		t.Fatalf("fresh-ref delivery must not re-pin any session, got RefreshedRuntimeRef = %q", result.RefreshedRuntimeRef)
	}
	runner.want(t, 0, "claude", "--model", "claude-opus-4", "--session-id", minted, "-p", "--output-format", "json", "--", "do the thing")
	for _, arg := range runner.calls[0] {
		if arg == "--resume" || arg == "--continue" {
			t.Fatalf("fresh-ref delivery must not resume any session: %v", runner.calls[0])
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
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: ref, RepoScope: "jerryfane/gitmoot"}

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
