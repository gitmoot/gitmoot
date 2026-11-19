package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func openTestJobLog(t *testing.T, path string, ompJSON bool) *JobLogWriter {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewJobLogWriter(file, ompJSON)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func writeChunks(t *testing.T, w *JobLogWriter, data string, chunk int) {
	t.Helper()
	for len(data) > 0 {
		n := min(chunk, len(data))
		if wrote, err := w.Write([]byte(data[:n])); err != nil || wrote != n {
			t.Fatalf("Write = %d, %v; want %d, nil", wrote, err, n)
		}
		data = data[n:]
	}
}

func readJobLog(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A single runaway line keeps its first JobLogMaxLineBytes and says how much
// was cut; the lines around it are untouched. It arrives in odd-sized chunks,
// as a pipe delivers it, so the cut cannot depend on where a write ends.
func TestJobLogWriterTruncatesOneHugeLineWithSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openTestJobLog(t, path, false)
	huge := strings.Repeat("h", JobLogMaxLineBytes+12345)
	writeChunks(t, w, "before\n"+huge+"\nafter\n", 7919)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := "before\n" + huge[:JobLogMaxLineBytes] + "…[12345 bytes truncated]\nafter\n"
	if got := readJobLog(t, path); got != want {
		t.Fatalf("log = %d bytes ending %q, want %d bytes ending %q", len(got), got[max(0, len(got)-60):], len(want), want[len(want)-60:])
	}
}

// The cut never splits a UTF-8 sequence: its leading bytes move into the
// truncated count, so the kept head stays valid text.
func TestJobLogWriterLineCutKeepsUTF8Whole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openTestJobLog(t, path, false)
	line := strings.Repeat("a", JobLogMaxLineBytes-1) + "é" + strings.Repeat("b", 10)
	writeChunks(t, w, line+"\n", 1<<20)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := readJobLog(t, path)
	want := strings.Repeat("a", JobLogMaxLineBytes-1) + "…[12 bytes truncated]\n"
	if got != want || !utf8.ValidString(got) {
		t.Fatalf("log ends %q, want %q", got[len(got)-40:], want[len(want)-40:])
	}
}

// The omp filter judges whole lines, so an update split across writes is
// still dropped and a kept line split at the same place is still kept; it
// matches the line's leading TYPE, so a kept line that merely mentions an
// update event, or a type that only starts like one, survives; and a runtime
// that is not omp keeps its update-looking lines.
func TestJobLogWriterOmpFilterJudgesWholeLines(t *testing.T) {
	stream := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"x"}}` + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"type\":\"message_update\"}"}]}}` + "\n" +
		`{"type":"tool_execution_update","toolCallId":"c","partialResult":{}}` + "\n" +
		`{"type":"tool_stream_update","toolCallId":"c","toolName":"edit"}` + "\n" +
		`{"type":"message_updates_are_new"}` + "\n" +
		`not json` + "\n" +
		`stderr: bad event {"type":"tool_execution_update"} ignored` + "\n" +
		`{"type":"agent_end","messages":[]}` + "\n"
	kept := `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"type\":\"message_update\"}"}]}}` + "\n" +
		`{"type":"tool_stream_update","toolCallId":"c","toolName":"edit"}` + "\n" +
		`{"type":"message_updates_are_new"}` + "\n" +
		`not json` + "\n" +
		`stderr: bad event {"type":"tool_execution_update"} ignored` + "\n" +
		`{"type":"agent_end","messages":[]}` + "\n"
	for _, chunk := range []int{1, 5, 17, len(stream)} {
		dir := t.TempDir()
		omp := openTestJobLog(t, filepath.Join(dir, "omp.log"), true)
		writeChunks(t, omp, stream, chunk)
		if err := omp.Close(); err != nil {
			t.Fatal(err)
		}
		if got := readJobLog(t, filepath.Join(dir, "omp.log")); got != kept {
			t.Fatalf("chunk %d: omp log =\n%s\nwant\n%s", chunk, got, kept)
		}
		other := openTestJobLog(t, filepath.Join(dir, "other.log"), false)
		writeChunks(t, other, stream, chunk)
		if err := other.Close(); err != nil {
			t.Fatal(err)
		}
		if got := readJobLog(t, filepath.Join(dir, "other.log")); got != stream {
			t.Fatalf("chunk %d: non-omp log dropped lines:\n%s", chunk, got)
		}
	}
}

// A job's retries append to one log, so the cap is on the FILE: a reopened log
// counts what is already there, and one already capped never gets a second
// marker.
func TestJobLogWriterCapSpansReopensWithOneMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	line := strings.Repeat("y", 1023) + "\n"
	first := openTestJobLog(t, path, false)
	first.limit = 10 << 10
	writeChunks(t, first, strings.Repeat(line, 6), 4096)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := openTestJobLog(t, path, false)
	second.limit = 10 << 10
	writeChunks(t, second, strings.Repeat(line, 6)+"unterminated", 4096)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat(line, 9) + JobLogTruncatedMarker
	if got := readJobLog(t, path); got != want {
		t.Fatalf("log = %d bytes, want %d: tail %q", len(got), len(want), got[max(0, len(got)-80):])
	}
	third := openTestJobLog(t, path, false)
	writeChunks(t, third, line, 4096)
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readJobLog(t, path); got != want {
		t.Fatalf("a capped log grew on reopen: %d bytes", len(got))
	}
}

// A final line the runtime never terminated is still kept, byte for byte: the
// log stays a copy of what the runtime printed.
func TestJobLogWriterCloseKeepsUnterminatedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openTestJobLog(t, path, true)
	writeChunks(t, w, "first\nlast words", 3)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readJobLog(t, path); got != "first\nlast words" {
		t.Fatalf("log = %q", got)
	}
}
