package transcript

import (
	"strings"
	"testing"

	gitmootruntime "github.com/gitmoot/gitmoot/internal/runtime"
)

// TestEveryRegisteredRuntimeHasATranslator is the tripwire for the gap omp (#1428)
// was the first runtime to open: newTranslator's default branch is an ERROR, and
// exportAllJobTranscripts builds a snapshot translator INSIDE its per-job loop and
// returns that error — so a single job of a translator-less runtime aborts
// `gitmoot job transcript --all` for every OTHER job in the store, mid-stream.
// Registration is therefore not complete until the runtime has a case here, even a
// raw passthrough one.
func TestEveryRegisteredRuntimeHasATranslator(t *testing.T) {
	names := gitmootruntime.SupportedRuntimes()
	if len(names) == 0 {
		t.Fatal("SupportedRuntimes is empty; this test would pass vacuously")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if _, err := NewTranslator(name); err != nil {
				t.Fatalf("NewTranslator(%q) = %v; `job watch --transcript` would error for this runtime", name, err)
			}
			if _, err := NewSnapshotTranslator(name); err != nil {
				t.Fatalf("NewSnapshotTranslator(%q) = %v; one such job aborts `job transcript --all` for every other job", name, err)
			}
		})
	}
}

// TestOmpTranslatorKeepsNonToolLinesRaw: omp lines the translator does not
// decode (messages, agent_end) still pass through raw, one event per line, so no
// retained byte disappears from `job transcript`; a session line becomes the
// lifecycle event that proves the stream was read (#2371).
func TestOmpTranslatorKeepsNonToolLinesRaw(t *testing.T) {
	lines := []string{
		`{"type":"session","id":"5b0f3f8a-1f4d-4a9e-9a34-3f0f2f7b1c2d"}`,
		`{"type":"message_end","message":{"role":"assistant","content":"done","usage":{"input":10,"output":5}}}`,
		`{"type":"agent_end"}`,
	}
	translator, err := NewSnapshotTranslator(gitmootruntime.OmpRuntime)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ReadSnapshotEvents(strings.NewReader(strings.Join(lines, "\n")+"\n"), translator)
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, translator.Flush()...)
	if len(events) != len(lines) {
		t.Fatalf("events = %#v, want one per NDJSON line (%d)", events, len(lines))
	}
	if events[0].Kind != KindLifecycle || events[0].Phase != "session" {
		t.Fatalf("session line = %#v, want a session lifecycle event", events[0])
	}
	for i, event := range events[1:] {
		if event.Kind != KindRaw || event.RawLine != lines[i+1] {
			t.Fatalf("event %d = %#v, want the raw line %q", i+1, event, lines[i+1])
		}
	}
}

// TestOmpTranslatorDecodesShellToolExecution pins the omp tool stream shape
// observed in retained production transcripts (#2371): a failed bash call carries
// details.exitCode, a successful one carries none and is exit 0.
func TestOmpTranslatorDecodesShellToolExecution(t *testing.T) {
	lines := []string{
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"go test ./...","timeout":30},"intent":"Running tests"}`,
		`{"type":"tool_execution_start","toolCallId":"c2","toolName":"bash","args":{"command":"python3 -c 'import PIL'"}}`,
		`{"type":"tool_execution_end","toolCallId":"c2","toolName":"bash","result":{"content":[{"type":"text","text":"ModuleNotFoundError: No module named 'PIL'\n\nCommand exited with code 1"}],"details":{"timeoutSeconds":30,"wallTimeMs":35.6,"exitCode":1}},"isError":true}`,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{"content":[{"type":"text","text":"ok  \tpkg\t0.1s\n"}],"details":{"timeoutSeconds":30,"wallTimeMs":166.7}},"isError":false}`,
	}
	translator, err := NewSnapshotTranslator(gitmootruntime.OmpRuntime)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ReadSnapshotEvents(strings.NewReader(strings.Join(lines, "\n")+"\n"), translator)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %#v, want 4", events)
	}
	if events[0].Kind != KindToolCall || events[0].ToolID != "c1" || events[0].Name != "bash" || !strings.Contains(events[0].InputDigest, `"go test ./..."`) {
		t.Fatalf("call = %#v", events[0])
	}
	failed, passed := events[2], events[3]
	if failed.Kind != KindToolResult || failed.ToolID != "c2" || failed.Status != "error" || failed.ExitCode == nil || *failed.ExitCode != 1 || !strings.Contains(failed.OutputDigest, "PIL") {
		t.Fatalf("failed result = %#v", failed)
	}
	if passed.Kind != KindToolResult || passed.ToolID != "c1" || passed.Status != "ok" || passed.ExitCode == nil || *passed.ExitCode != 0 {
		t.Fatalf("passed result = %#v", passed)
	}
}
