package runtime

import (
	"context"
	"errors"
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

// TestParseKimiStreamJSONVersion0192NoUsage feeds a real kimi-code 0.19.2
// --output-format stream-json transcript (captured live for #659) through the
// parser. 0.19.2 emits only an `assistant` event and a `meta` session.resume_hint
// event — NO usage event anywhere — so the parser must extract the content, honor
// the resume hint's session id, return zero usage, and report no error. This pins
// the honest 0/0-by-upstream-limitation behavior; TestKimiDeliverCapturesUsage
// above keeps the older/newer usage-shape coverage for CLIs that do emit usage.
func TestParseKimiStreamJSONVersion0192NoUsage(t *testing.T) {
	stream := `{"role":"assistant","content":"hi"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session_3f466a4e-41d2-44ec-a0e5-d162efe3de58","command":"kimi -r session_3f466a4e-41d2-44ec-a0e5-d162efe3de58","content":"To resume this session: kimi -r session_3f466a4e-41d2-44ec-a0e5-d162efe3de58"}` + "\n"

	content, sessionID, usage, err := parseKimiStreamJSON(stream)
	if err != nil {
		t.Fatalf("parseKimiStreamJSON returned error: %v", err)
	}
	if content != "hi" {
		t.Fatalf("content = %q, want %q", content, "hi")
	}
	if sessionID != "session_3f466a4e-41d2-44ec-a0e5-d162efe3de58" {
		t.Fatalf("sessionID = %q, want session_3f466a4e-41d2-44ec-a0e5-d162efe3de58", sessionID)
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Fatalf("usage = (%d, %d), want (0, 0) — 0.19.2 emits no usage event", usage.InputTokens, usage.OutputTokens)
	}
}

// codexUsageTranscript is a real `codex exec --json` transcript captured live
// (2026-07-05). We read agent_message .text and the turn.completed usage; the
// thread.started/turn.started lines and cached/reasoning token fields are ignored.
const codexUsageTranscript = `{"type":"thread.started","thread_id":"019f3041-cfed-7e82-8766-b5ca75cf92da"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hi"}}
{"type":"turn.completed","usage":{"input_tokens":16504,"cached_input_tokens":9600,"output_tokens":20,"reasoning_output_tokens":13}}`

// TestCodexDeliverCapturesUsage pins the #658 token capture for the codex runtime:
// Deliver now runs `codex exec --json …` and surfaces the last turn.completed
// usage — but ONLY for fresh sessions (ephemeral delegation workers / per-job
// overrides, the #338 budget's target). input_tokens already includes cached
// input, so it is taken verbatim and the cached/reasoning fields are ignored.
// The agent_message .text becomes both Raw (what the engine scans for the
// gitmoot_result blob) and the Summary.
func TestCodexDeliverCapturesUsage(t *testing.T) {
	ref, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: codexUsageTranscript}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "impl", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: ref, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "implement"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Raw != "hi" || result.Summary != "hi" {
		t.Fatalf("raw/summary = (%q, %q), want (\"hi\", \"hi\")", result.Raw, result.Summary)
	}
	if result.InputTokens != 16504 || result.OutputTokens != 20 {
		t.Fatalf("codex usage = (%d, %d), want (16504, 20)", result.InputTokens, result.OutputTokens)
	}
	runner.want(t, 0, "codex", "exec", "--json", "--", "implement")
}

// TestCodexDeliverResumeReportsZeroUsage pins the resumed-session exclusion:
// codex's turn.completed usage on a resumed thread is SESSION-CUMULATIVE, not
// per-turn (probed live on codex-cli 0.142.4: three one-word turns reported
// input 16504 -> 85681 -> 103779, and one resumed ask on a long-lived agent
// reported 22.4M input tokens). Attributing the whole session history to each
// job would corrupt per-root budgets, so a resumed delivery keeps the parsed
// text but contributes 0 usage until per-session delta tracking exists.
func TestCodexDeliverResumeReportsZeroUsage(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: codexUsageTranscript}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "impl", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "implement"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Raw != "hi" || result.Summary != "hi" {
		t.Fatalf("raw/summary = (%q, %q), want (\"hi\", \"hi\")", result.Raw, result.Summary)
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatalf("resumed codex usage = (%d, %d), want (0, 0) — session-cumulative upstream", result.InputTokens, result.OutputTokens)
	}
	// --json is an `exec` option and sits before the resume subcommand.
	runner.want(t, 0, "codex", "exec", "--json", "resume", "--last", "--", "implement")
}

// TestCodexDeliverJoinsAgentMessages pins that a turn emitting several
// agent_message items has their .text joined with a blank line into Raw/Summary,
// so a multi-message reply reaches the engine intact. Non-agent_message items
// (e.g. reasoning) are dropped.
func TestCodexDeliverJoinsAgentMessages(t *testing.T) {
	stdout := `{"type":"thread.started","thread_id":"t1"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}` + "\n" +
		`{"type":"item.completed","item":{"type":"reasoning","text":"ignored"}}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"second"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":4}}` + "\n"
	ref, err := NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "impl", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: ref, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "implement"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Raw != "first\n\nsecond" {
		t.Fatalf("raw = %q, want %q", result.Raw, "first\n\nsecond")
	}
	if result.InputTokens != 10 || result.OutputTokens != 4 {
		t.Fatalf("codex usage = (%d, %d), want (10, 4)", result.InputTokens, result.OutputTokens)
	}
}

// TestCodexDeliverNoUsageEventStaysZero pins the best-effort contract: a stream
// with an agent_message but no turn.completed still delivers the text (Raw) while
// contributing 0 to the budget rather than erroring.
func TestCodexDeliverNoUsageEventStaysZero(t *testing.T) {
	stdout := `{"type":"thread.started","thread_id":"t1"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "impl", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "implement"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Raw != "answer" {
		t.Fatalf("raw = %q, want %q", result.Raw, "answer")
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatalf("codex usage = (%d, %d), want (0, 0)", result.InputTokens, result.OutputTokens)
	}
}

// TestCodexDeliverFailsOpenOnNonJSONL pins the fail-open path: stdout that is not
// codex --json JSONL (no agent_message event — e.g. an unexpected shape or the
// plain-text fallback) is returned verbatim as Raw with 0 usage, so a delivery is
// never lost because usage parsing changed.
func TestCodexDeliverFailsOpenOnNonJSONL(t *testing.T) {
	const plain = "codex finished the task. plenty of tokens used but none reported."
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: plain}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "impl", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "implement"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Raw != plain {
		t.Fatalf("raw = %q, want %q", result.Raw, plain)
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatalf("codex usage = (%d, %d), want (0, 0)", result.InputTokens, result.OutputTokens)
	}
}

// TestCodexDeliverReRunsWithoutJSONWhenRejected pins the older-CLI fallback: when
// the first --json run fails with clap's "unexpected argument '--json'", Deliver
// re-runs the exact plain-text command once and uses its stdout (fail-open, 0
// usage). Mirrors the claude --output-format fallback.
func TestCodexDeliverReRunsWithoutJSONWhenRejected(t *testing.T) {
	const legacy = "legacy plain output with no json"
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "error: unexpected argument '--json' found"},
			{Stdout: legacy},
		},
		errs: []error{errors.New("exit status 2"), nil},
	}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "impl", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "implement"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Raw != legacy {
		t.Fatalf("raw = %q, want %q", result.Raw, legacy)
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatalf("codex usage = (%d, %d), want (0, 0)", result.InputTokens, result.OutputTokens)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("got %d codex calls, want 2 (json then fallback): %v", len(runner.calls), runner.calls)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "--last", "--", "implement")
	runner.want(t, 1, "codex", "exec", "resume", "--last", "--", "implement")
}

// TestParseCodexJSONResult unit-tests the parser directly, covering the real
// transcript, the no-agent_message fail-open signal, and non-JSONL noise.
func TestParseCodexJSONResult(t *testing.T) {
	raw, in, out, ok := parseCodexJSONResult(codexUsageTranscript)
	if !ok || raw != "hi" || in != 16504 || out != 20 {
		t.Fatalf("parseCodexJSONResult(transcript) = (%q, %d, %d, %v), want (\"hi\", 16504, 20, true)", raw, in, out, ok)
	}
	// No agent_message event -> ok=false so the caller fails open on raw stdout.
	if raw, in, out, ok := parseCodexJSONResult(`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":2}}`); ok || raw != "" || in != 0 || out != 0 {
		t.Fatalf("parseCodexJSONResult(no message) = (%q, %d, %d, %v), want (\"\", 0, 0, false)", raw, in, out, ok)
	}
	// Non-JSONL noise is skipped line-by-line, not fatal.
	if _, _, _, ok := parseCodexJSONResult("not json at all"); ok {
		t.Fatal("parseCodexJSONResult(non-json) ok = true, want false")
	}
}
