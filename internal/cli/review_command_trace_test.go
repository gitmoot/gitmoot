package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// #2371: an omp review's tool stream is decoded, so the job records each shell
// command with its exit code, tied to the execution-backend sandbox of that job
// generation, and the phase profile says the stream was traced.
func TestOmpReviewRecordsCommandTraceTiedToItsSandbox(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "omp-trace", "omp-reviewer", "omp")
	seedExecBackendAttemptAt(t, store, db.ExecBackendAttemptKey{JobID: "omp-trace", Attempt: 1, LifecycleGeneration: 0}, "sbx-remote-review", time.Now().Add(time.Hour))

	handle := openInstrumentedTranscript(t, home, "omp-trace", "omp", store)
	for _, line := range []string{
		`{"type":"session","id":"s1"}`,
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"go test ./internal/... GH_TOKEN=ghp_000000000000000000000000000000000000"}}`,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{"content":[{"type":"text","text":"ok  \tpkg\t0.1s\n"}],"details":{"wallTimeMs":12}},"isError":false}`,
		`{"type":"tool_execution_start","toolCallId":"c2","toolName":"bash","args":{"command":"python3 -c 'import PIL'"}}`,
		`{"type":"tool_execution_end","toolCallId":"c2","toolName":"bash","result":{"content":[{"type":"text","text":"ModuleNotFoundError: No module named 'PIL'\n\nCommand exited with code 1"}],"details":{"exitCode":1}},"isError":true}`,
		`{"type":"tool_execution_start","toolCallId":"c3","toolName":"read","args":{"path":"README.md"}}`,
		`{"type":"tool_execution_end","toolCallId":"c3","toolName":"read","result":{"content":[{"type":"text","text":"# readme"}]},"isError":false}`,
	} {
		if _, err := handle.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	profile := readPhaseProfile(t, store, "omp-trace")
	if profile.Trace != "traced" || profile.Coverage != phaseCoverageDecomposed || profile.Commands != 2 || profile.ToolEvents != 1 {
		t.Fatalf("profile = %+v, want traced/decomposed with 2 commands and 1 tool event", profile)
	}
	events, err := store.ListJobEvents(t.Context(), "omp-trace")
	if err != nil {
		t.Fatal(err)
	}
	var traces []commandTrace
	for _, event := range events {
		if event.Kind == reviewCommandTraceEventKind {
			var trace commandTrace
			if err := json.Unmarshal([]byte(event.Message), &trace); err != nil {
				t.Fatal(err)
			}
			traces = append(traces, trace)
		}
	}
	if len(traces) != 1 {
		t.Fatalf("want one %s event, got %d", reviewCommandTraceEventKind, len(traces))
	}
	trace := traces[0]
	if len(trace.Sandboxes) != 1 || trace.Sandboxes[0].SandboxID != "sbx-remote-review" {
		t.Fatalf("sandboxes = %+v, want the job's sandbox", trace.Sandboxes)
	}
	if len(trace.Commands) != 2 {
		t.Fatalf("commands = %+v, want the two shell commands", trace.Commands)
	}
	first, second := trace.Commands[0], trace.Commands[1]
	if first.ExitCode == nil || *first.ExitCode != 0 || first.Status != "ok" || second.ExitCode == nil || *second.ExitCode != 1 || second.Status != "error" {
		t.Fatalf("exit codes = %+v / %+v, want 0 and 1", first, second)
	}
	if strings.Contains(first.Command, "ghp_000000000000") || !strings.Contains(first.Command, "go test") {
		t.Fatalf("command = %q, want the command with its token redacted", first.Command)
	}
	if !strings.Contains(second.OutputTail, "PIL") || first.EndedAt == "" {
		t.Fatalf("trace entries lack output tail or end time: %+v", trace.Commands)
	}
}

// #2371: commands=0 must not read as "nothing ran" when nothing was read.
func TestOmpReviewWithoutAStreamIsNotTraced(t *testing.T) {
	for name, lines := range map[string][]string{
		"stream read, no command":   {`{"type":"session","id":"s1"}`, `{"type":"agent_end"}`},
		"no recognisable omp event": {"not json at all"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			jobID := nextInstrumentJobID("omp-untraced")
			store := seedInstrumentJob(t, paths, jobID, "omp-reviewer", "omp")
			handle := openInstrumentedTranscript(t, home, jobID, "omp", store)
			for _, line := range lines {
				if _, err := handle.Write([]byte(line + "\n")); err != nil {
					t.Fatal(err)
				}
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			profile := readPhaseProfile(t, store, jobID)
			want := "traced"
			if name == "no recognisable omp event" {
				want = "not_traced"
			}
			if profile.Trace != want || profile.Commands != 0 {
				t.Fatalf("profile = %+v, want trace %q and 0 commands", profile, want)
			}
		})
	}
}

// #2437 (P3 from #2435): the trace stays bounded and tied to the right generation.
// More than commandTraceMaxEntries commands are counted in omitted; a long output
// keeps only a bounded tail; a failure without a reported exit code is -1; and a
// sandbox from another lifecycle generation of the same job is not listed.
func TestCommandTraceBoundsAndGenerationFilter(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "omp-bounds", "omp-reviewer", "omp")
	seedExecBackendAttemptAt(t, store, db.ExecBackendAttemptKey{JobID: "omp-bounds", Attempt: 1, LifecycleGeneration: 0}, "sbx-this-generation", time.Now().Add(time.Hour))
	seedExecBackendAttemptAt(t, store, db.ExecBackendAttemptKey{JobID: "omp-bounds", Attempt: 1, LifecycleGeneration: 7}, "sbx-other-generation", time.Now().Add(time.Hour))

	handle := openInstrumentedTranscript(t, home, "omp-bounds", "omp", store)
	write := func(line string) {
		t.Helper()
		if _, err := handle.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("x", 5000) + "TAIL-END"
	write(`{"type":"tool_execution_start","toolCallId":"first","toolName":"bash","args":{"command":"long-output"}}`)
	write(`{"type":"tool_execution_end","toolCallId":"first","toolName":"bash","result":{"content":[{"type":"text","text":"` + long + `"}]},"isError":true}`)
	total := commandTraceMaxEntries + 5
	for i := 1; i < total; i++ {
		id := "c" + strconv.Itoa(i)
		write(`{"type":"tool_execution_start","toolCallId":"` + id + `","toolName":"bash","args":{"command":"true"}}`)
		write(`{"type":"tool_execution_end","toolCallId":"` + id + `","toolName":"bash","result":{"content":[]},"isError":false}`)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := store.ListJobEvents(t.Context(), "omp-bounds")
	if err != nil {
		t.Fatal(err)
	}
	var trace commandTrace
	for _, event := range events {
		if event.Kind == reviewCommandTraceEventKind {
			if err := json.Unmarshal([]byte(event.Message), &trace); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(trace.Commands) != commandTraceMaxEntries || trace.Omitted != total-commandTraceMaxEntries {
		t.Fatalf("commands=%d omitted=%d, want %d and %d", len(trace.Commands), trace.Omitted, commandTraceMaxEntries, total-commandTraceMaxEntries)
	}
	first := trace.Commands[0]
	if first.ExitCode == nil || *first.ExitCode != -1 {
		t.Fatalf("failed without a code = %v, want -1", first.ExitCode)
	}
	if !strings.HasSuffix(first.OutputTail, "TAIL-END") || len(first.OutputTail) > commandTraceMaxTailBytes+len("…") {
		t.Fatalf("output tail is %d bytes (%q...), want the last %d bytes", len(first.OutputTail), first.OutputTail[:20], commandTraceMaxTailBytes)
	}
	if len(trace.Sandboxes) != 1 || trace.Sandboxes[0].SandboxID != "sbx-this-generation" {
		t.Fatalf("sandboxes = %+v, want only this generation's", trace.Sandboxes)
	}
}
