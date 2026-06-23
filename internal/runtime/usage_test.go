package runtime

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// TestClaudeDeliverCapturesUsage pins the best-effort #338 Part B token capture
// for the claude runtime: when the --output-format json envelope carries a usage
// object, Deliver surfaces input_tokens/output_tokens on the Result.
func TestClaudeDeliverCapturesUsage(t *testing.T) {
	// A representative Claude Code --output-format json envelope. We read only
	// "result" and "usage.{input,output}_tokens"; the rest is ignored.
	stdout := `{"type":"result","subtype":"success","result":"done","session_id":"abc",` +
		`"usage":{"input_tokens":1234,"output_tokens":567,"cache_read_input_tokens":10}}`
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("summary = %q, want %q", result.Summary, "done")
	}
	if result.InputTokens != 1234 || result.OutputTokens != 567 {
		t.Fatalf("claude usage = (%d, %d), want (1234, 567)", result.InputTokens, result.OutputTokens)
	}
}

// TestClaudeDeliverNoUsageStaysZero pins that a claude response without a usage
// object (or that falls back to plain text) contributes 0 to the budget.
func TestClaudeDeliverNoUsageStaysZero(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stdout string
	}{
		{name: "json_without_usage", stdout: `{"result":"done"}`},
		{name: "plain_text_fallback", stdout: "just some text, not json"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: tt.stdout}}}
			adapter := ClaudeAdapter{Runner: runner}
			agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

			result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
			if err != nil {
				t.Fatalf("Deliver returned error: %v", err)
			}
			if result.InputTokens != 0 || result.OutputTokens != 0 {
				t.Fatalf("usage = (%d, %d), want (0, 0)", result.InputTokens, result.OutputTokens)
			}
		})
	}
}

// TestParseClaudeJSONResult unit-tests the envelope parser directly.
func TestParseClaudeJSONResult(t *testing.T) {
	summary, in, out := parseClaudeJSONResult(`{"result":"hi","usage":{"input_tokens":7,"output_tokens":3}}`)
	if summary != "hi" || in != 7 || out != 3 {
		t.Fatalf("parseClaudeJSONResult = (%q, %d, %d), want (\"hi\", 7, 3)", summary, in, out)
	}
	// Non-JSON returns the zero contribution so the caller falls back to raw text.
	if summary, in, out := parseClaudeJSONResult("not json"); summary != "" || in != 0 || out != 0 {
		t.Fatalf("parseClaudeJSONResult(non-json) = (%q, %d, %d), want (\"\", 0, 0)", summary, in, out)
	}
}

// TestKimiDeliverCapturesUsage pins the best-effort #338 Part B token capture for
// the kimi runtime: when the stream-json output emits an event carrying a usage
// object, Deliver surfaces the counts on the Result.
func TestKimiDeliverCapturesUsage(t *testing.T) {
	stdout := `{"role":"assistant","content":"answer"}` + "\n" +
		`{"role":"meta","type":"result","usage":{"input_tokens":800,"output_tokens":200}}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{Runner: runner, Dir: "/repo"}
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440000", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "answer" {
		t.Fatalf("summary = %q, want %q", result.Summary, "answer")
	}
	if result.InputTokens != 800 || result.OutputTokens != 200 {
		t.Fatalf("kimi usage = (%d, %d), want (800, 200)", result.InputTokens, result.OutputTokens)
	}
}

// TestKimiDeliverNoUsageStaysZero pins that a kimi stream without any usage event
// contributes 0 to the budget.
func TestKimiDeliverNoUsageStaysZero(t *testing.T) {
	stdout := `{"role":"assistant","content":"answer"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{Runner: runner, Dir: "/repo"}
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440000", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatalf("kimi usage = (%d, %d), want (0, 0)", result.InputTokens, result.OutputTokens)
	}
}

// TestCodexDeliverDoesNotCaptureUsage documents the honest per-runtime status:
// codex Deliver runs `codex exec resume … -- <prompt>` WITHOUT --json (plain
// text), so it exposes no machine-readable usage and contributes 0 to the per-root
// token budget. This is intentional (see Result doc + RESULT_CONTRACT.md); the
// budget under-counts codex rather than failing.
func TestCodexDeliverDoesNotCaptureUsage(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "codex finished the task. plenty of tokens used but none reported."}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "impl", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "last", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "implement"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatalf("codex usage = (%d, %d), want (0, 0) — codex Deliver reports no usage", result.InputTokens, result.OutputTokens)
	}
}
