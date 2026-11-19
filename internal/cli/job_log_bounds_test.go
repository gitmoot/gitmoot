package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2686: a job log is a copy of the runtime's output, and omp's `--mode=json`
// stream repeats its growing partial content in every message_update and
// tool_execution_update, so logs grew as the square of a turn. These tests run
// the production composition - openRetainedTranscriptLog, the transcript tee
// and the real omp adapter - over a real pipe, and check both halves of the
// contract: the log is bounded, and the adapter still parses every byte.

// ompFixtureRunner stands in for omp: whatever the adapter asks to run, it runs
// `cat <fixture>` through the real GroupRunner, so the stream reaches the
// adapter and the log through the same pipe, line splitter and tee a local omp's
// stdout does.
type ompFixtureRunner struct{ fixture string }

func (r ompFixtureRunner) Run(ctx context.Context, _ string, _ string, _ ...string) (subprocess.Result, error) {
	return subprocess.GroupRunner{}.Run(ctx, "", "cat", r.fixture)
}

func (r ompFixtureRunner) RunStream(ctx context.Context, _ string, out io.Writer, _ string, _ ...string) (subprocess.Result, error) {
	return subprocess.GroupRunner{}.RunStream(ctx, "", out, "cat", r.fixture)
}

func (ompFixtureRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }

func writeOmpFixture(t *testing.T, stream string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omp-stream.ndjson")
	if err := os.WriteFile(path, []byte(stream), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// deliverOmpFixture delivers one job through the omp adapter over fixture. A
// non-empty jobID tees the output into that job's retained log, exactly as the
// daemon worker does; an empty one is the untouched baseline.
func deliverOmpFixture(t *testing.T, home, jobID, fixture string) (runtime.Result, error) {
	t.Helper()
	var adapter workflow.DeliveryAdapter = runtime.OmpAdapter{Runner: ompFixtureRunner{fixture: fixture}}
	if jobID != "" {
		log, err := openRetainedTranscriptLog(home, jobID, "review", runtime.OmpRuntime, 0, nil)
		if err != nil || log == nil {
			t.Fatalf("open retained log = %v, %v", log, err)
		}
		defer func() {
			if err := log.Close(); err != nil {
				t.Fatalf("close retained log: %v", err)
			}
		}()
		teed, err := appendDeliveryAdapterOutput(adapter, log)
		if err != nil {
			t.Fatal(err)
		}
		adapter = teed
	}
	agent := runtime.Agent{Name: "reviewer", Role: "reviewer", Runtime: runtime.OmpRuntime, AutonomyPolicy: runtime.AutonomyPolicyDangerFullAccess}
	return adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "review this"})
}

func jobLogBoundsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatal(err)
	}
	return home
}

func ompFixtureResultEnd(summary string) string {
	text := fmt.Sprintf(`{"gitmoot_result":{"decision":"approved","summary":%q,"findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`, summary)
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%q}],`+
		`"usage":{"input":7,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":10},"stopReason":"stop","provider":"anthropic","model":"claude-opus-4"}}`, text) + "\n"
}

const ompFixtureSession = `{"type":"session","version":3,"id":"01920000-0000-7000-8000-000000002686","timestamp":"2026-11-19T00:00:00.000Z","cwd":"/repo"}` + "\n"

const ompFixtureAgentEnd = `{"type":"agent_end","messages":[],"isTerminal":true}` + "\n"

// TestJobLogDropsOmpStreamingUpdatesAndKeepsEveryOtherLine is the growth the
// issue measured: hundreds of message_update and tool_execution_update lines,
// each repeating the content so far. The log must be the stream with exactly
// those lines removed, and the delivered result must not change.
func TestJobLogDropsOmpStreamingUpdatesAndKeepsEveryOtherLine(t *testing.T) {
	var stream, kept strings.Builder
	keep := func(line string) {
		stream.WriteString(line)
		kept.WriteString(line)
	}
	keep(ompFixtureSession)
	keep(`{"type":"agent_start"}` + "\n")
	keep(`{"type":"turn_start"}` + "\n")
	keep(`{"type":"message_start","message":{"role":"assistant","content":[]}}` + "\n")
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&stream, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"word "},"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`+"\n", strings.Repeat("word ", i))
	}
	keep(`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"command":"go test ./..."}}` + "\n")
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&stream, `{"type":"tool_execution_update","toolCallId":"call-1","toolName":"bash","partialResult":{"content":[{"type":"text","text":%q}]}}`+"\n", strings.Repeat("ok  pkg 0.1s\n", i))
	}
	keep(`{"type":"tool_stream_update","toolCallId":"call-1","toolName":"bash"}` + "\n")
	keep(`{"type":"tool_execution_end","toolCallId":"call-1","toolName":"bash","isError":false,"result":{"content":[{"type":"text","text":"ok  pkg 0.1s\n"}]}}` + "\n")
	keep(`{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":10,"errorMessage":"overloaded"}` + "\n")
	keep(`{"type":"auto_retry_end","success":true,"attempt":1}` + "\n")
	keep(ompFixtureResultEnd("growing stream reviewed"))
	keep(`{"type":"turn_end","message":{"role":"assistant","content":[]},"toolResults":[]}` + "\n")
	keep(ompFixtureAgentEnd)
	fixture := writeOmpFixture(t, stream.String())

	baseline, baselineErr := deliverOmpFixture(t, "", "", fixture)
	if baselineErr != nil {
		t.Fatalf("baseline Deliver: %v", baselineErr)
	}
	if !strings.Contains(baseline.Summary, "growing stream reviewed") {
		t.Fatalf("baseline summary = %q, want the fixture's result", baseline.Summary)
	}

	home := jobLogBoundsHome(t)
	got, err := deliverOmpFixture(t, home, "growing-updates", fixture)
	if err != nil {
		t.Fatalf("teed Deliver: %v", err)
	}
	if !reflect.DeepEqual(got, baseline) {
		t.Fatalf("teed result differs from the untouched run:\n got %#v\nwant %#v", got, baseline)
	}

	body, err := os.ReadFile(retainedTranscriptLogPathForTest(t, home, "growing-updates"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != kept.String() {
		t.Fatalf("log is not the stream minus its update lines: got %d bytes (%d message_update, %d tool_execution_update lines), want %d bytes of a %d-byte stream",
			len(body), strings.Count(string(body), `{"type":"message_update"`), strings.Count(string(body), `{"type":"tool_execution_update"`), kept.Len(), stream.Len())
	}
	for _, want := range []string{`"type":"message_end"`, `"type":"agent_end"`, `"type":"tool_execution_end"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("log lost %s", want)
		}
	}
}

// TestJobLogCapWritesOneMarkerAndTheParserStillGetsEverything runs a stream
// past the 32 MiB cap whose result is the LAST thing in it. The log stops at
// the cap with exactly one marker; the adapter still derives the result, which
// it can do only if the log's cap never cut the stream the adapter reads.
func TestJobLogCapWritesOneMarkerAndTheParserStillGetsEverything(t *testing.T) {
	var stream strings.Builder
	stream.WriteString(ompFixtureSession)
	// Kept event lines, each under the line cap, adding up past the file cap.
	output := strings.Repeat("x", 200<<10)
	for i := 0; stream.Len() < transcript.JobLogMaxBytes+(4<<20); i++ {
		fmt.Fprintf(&stream, `{"type":"tool_execution_end","toolCallId":"call-%d","toolName":"bash","isError":false,"result":{"content":[{"type":"text","text":%q}]}}`+"\n", i, output)
	}
	stream.WriteString(ompFixtureResultEnd("result past the cap"))
	stream.WriteString(ompFixtureAgentEnd)
	fixture := writeOmpFixture(t, stream.String())

	baseline, baselineErr := deliverOmpFixture(t, "", "", fixture)
	if baselineErr != nil {
		t.Fatalf("baseline Deliver: %v", baselineErr)
	}
	home := jobLogBoundsHome(t)
	got, err := deliverOmpFixture(t, home, "capped", fixture)
	if err != nil {
		t.Fatalf("teed Deliver: %v", err)
	}
	if !strings.Contains(got.Summary, "result past the cap") || !reflect.DeepEqual(got, baseline) {
		t.Fatalf("teed result = %#v, want the untouched run's %#v", got, baseline)
	}

	body, err := os.ReadFile(retainedTranscriptLogPathForTest(t, home, "capped"))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > transcript.JobLogMaxBytes {
		t.Fatalf("log is %d bytes, over the %d cap", len(body), transcript.JobLogMaxBytes)
	}
	if n := strings.Count(string(body), "[gitmoot: job log truncated"); n != 1 {
		t.Fatalf("log carries %d truncation markers, want exactly 1", n)
	}
	head, ok := strings.CutSuffix(string(body), transcript.JobLogTruncatedMarker)
	if !ok {
		t.Fatalf("log does not END with the marker; tail %q", body[len(body)-200:])
	}
	if !strings.HasPrefix(stream.String(), head) || !strings.HasSuffix(head, "\n") {
		t.Fatal("log before the marker is not whole lines of the stream")
	}
	if len(head) < transcript.JobLogMaxBytes-(1<<20) {
		t.Fatalf("log stopped at %d bytes, well short of the cap", len(head))
	}
}
