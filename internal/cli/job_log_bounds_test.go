package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
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
// past the 32 MiB bound whose result is the LAST thing in it. The log keeps a
// head that ends in exactly one marker and a tail that ends with the run's
// terminal events, within the bound; the adapter still derives the result,
// which it can do only if the log's bounds never cut the stream it reads.
func TestJobLogCapWritesOneMarkerAndTheParserStillGetsEverything(t *testing.T) {
	var stream strings.Builder
	stream.WriteString(ompFixtureSession)
	// Kept event lines, each under the line cap, adding up past the bound.
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

	logPath := retainedTranscriptLogPathForTest(t, home, "capped")
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	head, ok := strings.CutSuffix(string(body), transcript.JobLogTruncatedMarker)
	if !ok || strings.Count(string(body), transcript.JobLogTruncatedMarker) != 1 {
		t.Fatalf("head does not end in exactly one marker; tail %q", body[max(0, len(body)-200):])
	}
	if !strings.HasPrefix(stream.String(), head) || !strings.HasSuffix(head, "\n") || len(head) < transcript.JobLogHeadBytes-(1<<20) {
		t.Fatalf("head is %d bytes, want whole lines of the stream filling the %d head", len(head), transcript.JobLogHeadBytes)
	}
	if disk := jobLogDiskBytesForTest(t, logPath); disk > transcript.JobLogMaxBytes {
		t.Fatalf("log uses %d bytes on disk, over the %d bound", disk, transcript.JobLogMaxBytes)
	}
	whole := readWholeJobLogForTest(t, logPath)
	if !strings.HasSuffix(whole, ompFixtureResultEnd("result past the cap")+ompFixtureAgentEnd) {
		t.Fatalf("log does not end with the run's terminal events: %q", whole[max(0, len(whole)-300):])
	}
}

// jobLogDiskBytesForTest is what a job log costs on disk: the log and every
// file beside it named after it. It does not depend on how the files are
// named, so it holds the old single-file layout to the same bound.
func jobLogDiskBytesForTest(t *testing.T, logPath string) int64 {
	t.Helper()
	matches, err := filepath.Glob(logPath + "*")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

// readWholeJobLogForTest reads a job log through the reader every consumer
// uses: readTranscriptEvents with the raw shell translator, which hands each
// line back as it is.
func readWholeJobLogForTest(t *testing.T, logPath string) string {
	t.Helper()
	translator, err := transcript.NewSnapshotTranslator(runtime.ShellRuntime)
	if err != nil {
		t.Fatal(err)
	}
	events, err := readTranscriptEvents(logPath, translator)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, event := range events {
		b.WriteString(event.RawLine + "\n")
	}
	return b.String()
}

// countJobLogMarkers counts truncation marker lines in every file of a log.
func countJobLogMarkers(t *testing.T, logPath string) int {
	t.Helper()
	matches, err := filepath.Glob(logPath + "*")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, match := range matches {
		body, err := os.ReadFile(match)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "[gitmoot: job log ") && strings.Contains(line, "truncated") {
				count++
			}
		}
	}
	return count
}

// fillJobLogPastItsBound writes more than 32 MiB of distinct lines through w.
func fillJobLogPastItsBound(t *testing.T, writers ...io.Writer) {
	t.Helper()
	filler := strings.Repeat("f", 100<<10)
	for i, written := 0, 0; written < transcript.JobLogMaxBytes+(4<<20); i++ {
		line := fmt.Sprintf("progress %d %s\n", i, filler)
		if _, err := writers[i%len(writers)].Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		written += len(line)
	}
}

// TestJobLogKeepsFatalEvidencePastTheBound is review F1 on #2708: a run that
// prints its fatal error after 32 MiB must still have it on disk, because
// recovery reads the retained log after the writing process died, and so does
// `run transcript`. The log must stay within its bound all the same.
func TestJobLogKeepsFatalEvidencePastTheBound(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedCLIJob(t, store, db.Job{
		ID: "fatal-tail", Agent: "audit", Type: "ask", State: string(workflow.JobRunning),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, "running")
	log, err := openRetainedTranscriptLog(home, "fatal-tail", "ask", runtime.ShellRuntime, 0, nil)
	if err != nil || log == nil {
		t.Fatalf("open retained log = %v, %v", log, err)
	}
	fillJobLogPastItsBound(t, log)
	const fatal = "FATAL 7f3a9c: runtime aborted, out of credits"
	if _, err := log.Write([]byte(fatal + "\n")); err != nil {
		t.Fatal(err)
	}
	// No Close: the writer died. What recovery reads is what reached disk.

	if tail := recoveredJobLogTail(store, "fatal-tail"); !strings.Contains(tail, fatal) {
		t.Fatalf("recovery tail lacks the fatal line; it ends %q", tail[max(0, len(tail)-200):])
	}
	store.Close()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "transcript", "fatal-tail", "--home", home, "--export", "jsonl", "--runtime", runtime.ShellRuntime}, &stdout, &stderr); code != 0 {
		t.Fatalf("run transcript code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), fatal) {
		t.Fatal("run transcript export lacks the fatal line")
	}
	logPath := retainedTranscriptLogPathForTest(t, home, "fatal-tail")
	if disk := jobLogDiskBytesForTest(t, logPath); disk > transcript.JobLogMaxBytes+(64<<10) {
		t.Fatalf("log uses %d bytes on disk, over the %d bound", disk, transcript.JobLogMaxBytes)
	}
	_ = log.Close()
}

// TestJobLogHandlesOnOnePathShareOneBound is review F3 on #2708: two handles
// opened on one job's log before either writes, written alternately, must
// share one bound and one marker.
func TestJobLogHandlesOnOnePathShareOneBound(t *testing.T) {
	home := jobLogBoundsHome(t)
	var handles []io.Writer
	for range 2 {
		log, err := openRetainedTranscriptLog(home, "shared", "ask", runtime.ShellRuntime, 0, nil)
		if err != nil || log == nil {
			t.Fatalf("open retained log = %v, %v", log, err)
		}
		defer log.Close()
		handles = append(handles, log)
	}
	fillJobLogPastItsBound(t, handles...)
	logPath := retainedTranscriptLogPathForTest(t, home, "shared")
	if disk := jobLogDiskBytesForTest(t, logPath); disk > transcript.JobLogMaxBytes {
		t.Fatalf("two handles used %d bytes on disk, over the shared %d bound", disk, transcript.JobLogMaxBytes)
	}
	if n := countJobLogMarkers(t, logPath); n != 1 {
		t.Fatalf("log carries %d truncation markers, want exactly 1", n)
	}
}

// TestJobLogShortenedToolEventKeepsItsEvidence is review F2 on #2708: a tool
// event over the line cap must stay a decodable event, so the remote review
// evidence inspection still sees which file its arguments open.
func TestJobLogShortenedToolEventKeepsItsEvidence(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", 32<<10) // 512 KiB: over the line cap, under the 1 MiB snapshot cap
	event := fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":"call-proof","toolName":"bash","args":{"command":"file /home/user/evidence/01-proof.png && cat <<'EOF' > notes.txt\n%s\nEOF","timeout":60}}`, big)
	evidence := &workflow.ReviewEvidence{Files: []workflow.ReviewEvidenceFile{{SandboxName: "01-proof.png"}}}
	original := reviewEvidenceInspectionFrom("proof", evidence, translateOmpLinesForTest(t, event))
	if len(original.Opened) != 1 {
		t.Fatalf("fixture is wrong: the original event opens %v", original)
	}

	home := jobLogBoundsHome(t)
	log, err := openRetainedTranscriptLog(home, "proof", "review", runtime.OmpRuntime, 0, nil)
	if err != nil || log == nil {
		t.Fatalf("open retained log = %v, %v", log, err)
	}
	if _, err := log.Write([]byte(event + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	logPath := retainedTranscriptLogPathForTest(t, home, "proof")
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > transcript.JobLogMaxLineBytes+1 {
		t.Fatalf("retained event is %d bytes, over the line cap", len(body))
	}
	translator, err := transcript.NewSnapshotTranslator(runtime.OmpRuntime)
	if err != nil {
		t.Fatal(err)
	}
	events, err := readTranscriptEvents(logPath, translator)
	if err != nil {
		t.Fatal(err)
	}
	if retained := reviewEvidenceInspectionFrom("proof", evidence, events); !reflect.DeepEqual(retained, original) {
		t.Fatalf("retained event inspects as %+v, the original as %+v", retained, original)
	}
}

func translateOmpLinesForTest(t *testing.T, lines ...string) []transcript.Event {
	t.Helper()
	translator, err := transcript.NewSnapshotTranslator(runtime.OmpRuntime)
	if err != nil {
		t.Fatal(err)
	}
	var events []transcript.Event
	for _, line := range lines {
		events = append(events, translator.Translate(line)...)
	}
	return events
}
