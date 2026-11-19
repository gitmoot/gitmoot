package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func openTestJobLog(t *testing.T, path string, ompJSON bool) *JobLogWriter {
	t.Helper()
	w, err := OpenJobLogWriter(path, ompJSON)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// openSmallJobLog bounds a log with a small head and small tail segments, so
// a test can cross both without writing tens of megabytes.
func openSmallJobLog(t *testing.T, path string, headLimit, segmentLimit int64) *JobLogWriter {
	t.Helper()
	w := openTestJobLog(t, path, false)
	w.headLimit, w.segmentLimit = headLimit, segmentLimit
	return w
}

// readWholeJobLog reads a log the way every consumer does.
func readWholeJobLog(t *testing.T, path string) string {
	t.Helper()
	log, err := OpenJobLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var b strings.Builder
	if _, err := io.Copy(&b, log); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func jobLogDiskBytes(t *testing.T, path string) int64 {
	t.Helper()
	var total int64
	tails := JobLogTailPaths(path)
	for _, p := range append([]string{path}, tails[:]...) {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
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

func numberedLine(i int) string {
	head := fmt.Sprintf("line-%02d ", i)
	return head + strings.Repeat("y", 1023-len(head)) + "\n"
}

func numberedLines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		b.WriteString(numberedLine(i))
	}
	return b.String()
}

// A job's retries append to one log, so the bounds are on the FILES: a
// reopened log counts what is already there, a full head never gets a second
// marker, and the output past it lands in the two tail segments, which keep
// the most recent part of the run. Reading the log returns the head, then the
// older and the newer segment.
func TestJobLogWriterHeadAndTailSpanReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	const headLimit, segmentLimit = 10 << 10, 2560 // a segment holds its header and two lines
	for _, lines := range [][2]int{{1, 6}, {7, 12}, {13, 15}} {
		w := openSmallJobLog(t, path, headLimit, segmentLimit)
		writeChunks(t, w, numberedLines(lines[0], lines[1]), 4096)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Lines 1-9 fill the head. 10-11 went to segment 1, 12-13 to segment 2,
	// and 14-15 to segment 3, which reused segment 1's file.
	want := numberedLines(1, 9) + JobLogTruncatedMarker +
		string(jobLogSegmentHeader(2)) + numberedLines(12, 13) +
		string(jobLogSegmentHeader(3)) + numberedLines(14, 15)
	if got := readWholeJobLog(t, path); got != want {
		t.Fatalf("log =\n%s\nwant\n%s", got, want)
	}
	if got := readJobLog(t, path); got != numberedLines(1, 9)+JobLogTruncatedMarker {
		t.Fatalf("head grew past its marker: %d bytes", len(got))
	}
	if disk := jobLogDiskBytes(t, path); disk > headLimit+2*segmentLimit {
		t.Fatalf("log uses %d bytes on disk, over %d", disk, headLimit+2*segmentLimit)
	}
}

// Two handles on one path share ONE budget: every decision is made from the
// files under flock, not from a count kept per handle. Alternating whole
// writes between them must leave exactly one marker, a head within its cap,
// and the run's last line on disk.
func TestJobLogWriterSharedHandlesShareOneBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	const headLimit, segmentLimit = 10 << 10, 2560
	a := openSmallJobLog(t, path, headLimit, segmentLimit)
	b := openSmallJobLog(t, path, headLimit, segmentLimit)
	for i := 1; i <= 40; i++ {
		w := a
		if i%2 == 0 {
			w = b
		}
		writeChunks(t, w, numberedLine(i), 1<<20)
	}
	for _, w := range []*JobLogWriter{a, b} {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	head := readJobLog(t, path)
	if len(head) > headLimit || strings.Count(head, JobLogTruncatedMarker) != 1 || !strings.HasSuffix(head, JobLogTruncatedMarker) {
		t.Fatalf("head = %d bytes with %d markers, want at most %d ending in one marker", len(head), strings.Count(head, JobLogTruncatedMarker), headLimit)
	}
	if disk := jobLogDiskBytes(t, path); disk > headLimit+2*segmentLimit {
		t.Fatalf("two handles used %d bytes on disk, over the shared %d", disk, headLimit+2*segmentLimit)
	}
	whole := readWholeJobLog(t, path)
	if !strings.HasSuffix(whole, numberedLine(40)) || !strings.Contains(whole, numberedLine(39)) {
		t.Fatalf("log does not end with the run's last lines: %q", whole[max(0, len(whole)-200):])
	}
}

// A follower whose segment was rotated away resumes at the oldest segment
// still on disk, from its start, rather than reading the reused file at the
// old offset (which would be the middle of a different segment).
func TestFollowJobLogTailSkipsARotatedSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openSmallJobLog(t, path, 10<<10, 2560)
	writeChunks(t, w, numberedLines(1, 10), 1<<20) // line 10 opens segment 1
	var got []string
	buffer := newLineBuffer(MaxLogicalLineBytes, func(line string) error {
		got = append(got, line)
		return nil
	})
	cursor := &jobLogTailCursor{}
	if _, err := followJobLogTail(path, cursor, buffer); err != nil {
		t.Fatal(err)
	}
	writeChunks(t, w, numberedLines(11, 15), 1<<20) // segments 2 and 3; 3 reuses 1's file
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := followJobLogTail(path, cursor, buffer); err != nil {
		t.Fatal(err)
	}
	want := []string{
		strings.TrimSuffix(string(jobLogSegmentHeader(1)), "\n"), strings.TrimSuffix(numberedLine(10), "\n"),
		strings.TrimSuffix(string(jobLogSegmentHeader(2)), "\n"), strings.TrimSuffix(numberedLine(12), "\n"), strings.TrimSuffix(numberedLine(13), "\n"),
		strings.TrimSuffix(string(jobLogSegmentHeader(3)), "\n"), strings.TrimSuffix(numberedLine(14), "\n"), strings.TrimSuffix(numberedLine(15), "\n"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("followed tail =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Follow carries on past a full head into the tail segments, so a watched run
// shows its end.
func TestFollowReadsThroughTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openSmallJobLog(t, path, 10<<10, 2560)
	writeChunks(t, w, numberedLines(1, 11), 1<<20)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	err := Follow(t.Context(), path, FollowOptions{PollInterval: time.Millisecond, Settled: func(context.Context) (bool, error) { return true, nil }}, func(line string) error {
		got.WriteString(line + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := readWholeJobLog(t, path); got.String() != want || !strings.HasSuffix(want, numberedLine(11)) {
		t.Fatalf("followed =\n%s\nwant\n%s", got.String(), want)
	}
}

// Once the head is full only the tail segments grow, so a log's progress
// evidence (the liveness sweep's) must include them.
func TestStatJobLogCountsTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openSmallJobLog(t, path, 10<<10, 2560)
	writeChunks(t, w, numberedLines(1, 10), 1<<20) // line 10 fills the head and opens the tail
	before, err := StatJobLog(path)
	if err != nil {
		t.Fatal(err)
	}
	headInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeChunks(t, w, numberedLines(11, 13), 1<<20)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := StatJobLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size != jobLogDiskBytes(t, path) || after.Size <= before.Size {
		t.Fatalf("progress size %d -> %d, want the whole log's %d", before.Size, after.Size, jobLogDiskBytes(t, path))
	}
	if head, _ := os.Stat(path); head.Size() != headInfo.Size() {
		t.Fatalf("fixture is wrong: the head grew after it was full")
	}
}

// Concurrent writers on separate handles: the lock makes every decision and
// its write one step, so however the writes interleave there is one marker,
// no segment over its cap, and no line lost or torn among those kept.
func TestJobLogWriterConcurrentHandlesStayWithinBounds(t *testing.T) {
	for round := range 20 {
		path := filepath.Join(t.TempDir(), "job.log")
		const headLimit, segmentLimit = 4 << 10, 4 << 10
		var wg sync.WaitGroup
		for writer := range 4 {
			w := openSmallJobLog(t, path, headLimit, segmentLimit)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 60 {
					line := fmt.Sprintf("w%d-%03d %s\n", writer, i, strings.Repeat("c", 200))
					if _, err := w.Write([]byte(line)); err != nil {
						t.Error(err)
						return
					}
				}
				if err := w.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		head := readJobLog(t, path)
		if len(head) > headLimit || strings.Count(head, JobLogTruncatedMarker) != 1 {
			t.Fatalf("round %d: head %d bytes with %d markers", round, len(head), strings.Count(head, JobLogTruncatedMarker))
		}
		seqs := map[string]bool{}
		for _, tail := range JobLogTailPaths(path) {
			body := readJobLog(t, tail)
			if len(body) > segmentLimit {
				t.Fatalf("round %d: segment %s is %d bytes, over %d", round, tail, len(body), segmentLimit)
			}
			header, _, _ := strings.Cut(body, "\n")
			if seqs[header] || strings.Count(body, jobLogSegmentHeaderPrefix) != 1 {
				t.Fatalf("round %d: segment %s has a duplicated or torn header: %q", round, tail, header)
			}
			seqs[header] = true
		}
		for _, line := range strings.Split(strings.TrimSuffix(readWholeJobLog(t, path), "\n"), "\n") {
			if !strings.HasPrefix(line, "[gitmoot: job log ") && (len(line) != 207 || line[0] != 'w') {
				t.Fatalf("round %d: torn line %q", round, line)
			}
		}
	}
}

// An over-long JSON event is shortened INTO a valid event: its long strings
// keep their heads, and the translator reads it as the same tool event, with
// the same id, tool name, status and exit code, and with its arguments still
// naming the file it opened.
func TestJobLogWriterShortensALongJSONEventIntoAValidOne(t *testing.T) {
	big := strings.Repeat("payload <&> ", 40<<10)
	start := fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":"call_02c0b95d6bb146aa979adabe089f0471|fc_043de0763a0232dc016afe90fd15f887d28047bc5b86773638","toolName":"write","args":{"path":"/home/user/evidence/01-proof.png","content":%q,"mode":420,"append":false,"note":null}}`, big)
	end := fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":"call_02c0b95d6bb146aa979adabe089f0471|fc_043de0763a0232dc016afe90fd15f887d28047bc5b86773638","toolName":"write","isError":true,"result":{"content":[{"type":"text","text":%q}],"details":{"exitCode":2}}}`, big)
	path := filepath.Join(t.TempDir(), "job.log")
	w := openTestJobLog(t, path, true)
	writeChunks(t, w, start+"\n"+end+"\n", 7919)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	kept := strings.Split(strings.TrimSuffix(readJobLog(t, path), "\n"), "\n")
	if len(kept) != 2 {
		t.Fatalf("log has %d lines, want 2", len(kept))
	}
	for i, original := range []string{start, end} {
		line := kept[i]
		if len(line) > JobLogMaxLineBytes || !json.Valid([]byte(line)) || !strings.Contains(line, "bytes truncated]") {
			t.Fatalf("line %d: %d bytes, valid JSON %v; want a valid shortened event within %d", i, len(line), json.Valid([]byte(line)), JobLogMaxLineBytes)
		}
		if !strings.HasPrefix(line, original[:40]) {
			t.Fatalf("line %d lost its leading type: %q", i, line[:60])
		}
	}
	translate := func(lines ...string) []Event {
		translator, err := NewSnapshotTranslator("omp")
		if err != nil {
			t.Fatal(err)
		}
		var events []Event
		for _, line := range lines {
			events = append(events, translator.Translate(line)...)
		}
		return events
	}
	want, got := translate(start, end), translate(kept...)
	if len(want) != 2 || len(got) != 2 {
		t.Fatalf("events: original %d, retained %d; want 2 each", len(want), len(got))
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].ToolID != want[i].ToolID || got[i].Name != want[i].Name || got[i].Status != want[i].Status || fmt.Sprint(deref(got[i].ExitCode)) != fmt.Sprint(deref(want[i].ExitCode)) {
			t.Fatalf("event %d: retained %+v, want the original's kind/id/name/status/exit %+v", i, got[i], want[i])
		}
	}
	if got[0].Kind != KindToolCall || !strings.Contains(got[0].InputDigest, "01-proof.png") {
		t.Fatalf("retained tool call %+v lost the file its arguments name", got[0])
	}
	if got[1].Kind != KindToolResult || got[1].Status != "error" || deref(got[1].ExitCode) != 2 {
		t.Fatalf("retained result %+v, want an error with exit code 2", got[1])
	}
}

func deref(p *int) int {
	if p == nil {
		return -999
	}
	return *p
}

// Only a line that does not decode gets the raw cut: a bounded head and the
// truncated count.
func TestJobLogWriterRawCutsOnlyUndecodableJSON(t *testing.T) {
	broken := `{"type":"x","text":"` + strings.Repeat("z", JobLogMaxLineBytes+10)
	path := filepath.Join(t.TempDir(), "job.log")
	w := openTestJobLog(t, path, false)
	writeChunks(t, w, broken+"\n", 1<<20)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := broken[:JobLogMaxLineBytes] + fmt.Sprintf("…[%d bytes truncated]\n", len(broken)-JobLogMaxLineBytes)
	if got := readJobLog(t, path); got != want {
		t.Fatalf("log ends %q, want %q", got[max(0, len(got)-60):], want[len(want)-60:])
	}
}

// keptJSONLine writes one line through a job log and returns the one line it
// kept, which must be valid JSON within the line cap.
func keptJSONLine(t *testing.T, line string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "job.log")
	w := openTestJobLog(t, path, true)
	writeChunks(t, w, line+"\n", 7919)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	kept, ok := strings.CutSuffix(readJobLog(t, path), "\n")
	if !ok || strings.Contains(kept, "\n") {
		t.Fatalf("log is not one line: %d bytes", len(kept))
	}
	if len(kept) > JobLogMaxLineBytes || !json.Valid([]byte(kept)) {
		t.Fatalf("kept %d bytes, valid JSON %v; want a valid event within %d (tail %q)", len(kept), json.Valid([]byte(kept)), JobLogMaxLineBytes, kept[max(0, len(kept)-80):])
	}
	return kept
}

// Review F2 on #2708, round 3: a valid event whose size comes from MANY SHORT
// values - 2,500 text parts of 100 characters - has no long string to cut.
// Its arrays are shortened instead, and it still reads as the same failed
// tool result, with its status and exit code. An array of numbers is cut the
// same way.
func TestJobLogWriterKeepsAValidEventOfManySmallValues(t *testing.T) {
	parts := make([]string, 2500)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"type":"text","text":%q}`, fmt.Sprintf("%04d ", i)+strings.Repeat("p", 95))
	}
	event := `{"type":"tool_execution_end","toolCallId":"call-many","toolName":"bash","isError":true,"result":{"content":[` +
		strings.Join(parts, ",") + `],"details":{"exitCode":3}}}`
	if len(event) < 300<<10 {
		t.Fatalf("fixture is %d bytes, want the reviewer's ~315 KB", len(event))
	}
	kept := keptJSONLine(t, event)
	if !strings.HasPrefix(kept, `{"type":"tool_execution_end",`) || !strings.Contains(kept, "elements truncated]") {
		t.Fatalf("kept line does not open with its type or does not mark the cut: %q", kept[:min(len(kept), 120)])
	}
	translator, err := NewSnapshotTranslator("omp")
	if err != nil {
		t.Fatal(err)
	}
	events := translator.Translate(kept)
	if len(events) != 1 || events[0].Kind != KindToolResult || events[0].ToolID != "call-many" || events[0].Status != "error" || deref(events[0].ExitCode) != 3 {
		t.Fatalf("kept event translates to %+v, want the failed tool result with exit code 3", events)
	}
	if !strings.Contains(events[0].OutputDigest, "0000 ppp") {
		t.Fatalf("kept result lost the head of its output: %q", events[0].OutputDigest[:min(len(events[0].OutputDigest), 80)])
	}

	numbers := `{"type":"x","values":[` + strings.TrimSuffix(strings.Repeat("12345,", 60<<10), ",") + `]}`
	if kept := keptJSONLine(t, numbers); !strings.Contains(kept, `"…[`) {
		t.Fatalf("number array kept without a marker: %q", kept[:min(len(kept), 120)])
	}
}

// When neither strings nor arrays can be cut enough - here one object of
// 40,000 short keys - the line becomes the envelope: its top-level scalars,
// type first, and "gitmoot_truncated":true. It is never raw-cut.
func TestJobLogWriterEnvelopesAnEventNothingElseShortens(t *testing.T) {
	var fields []string
	for i := range 40000 {
		fields = append(fields, fmt.Sprintf(`"k%05d":%d`, i, i))
	}
	event := `{"type":"tool_execution_end","toolCallId":"call-env","toolName":"bash","isError":true,"result":{"details":{` +
		strings.Join(fields, ",") + `}}}`
	kept := keptJSONLine(t, event)
	want := `{"type":"tool_execution_end","toolCallId":"call-env","toolName":"bash","isError":true,"gitmoot_truncated":true}`
	if kept != want {
		t.Fatalf("kept %q, want the envelope %q", kept[:min(len(kept), 200)], want)
	}
}

// Review F4 on #2708: a snapshot must not mix generations of a reused tail
// segment. The reader is opened over segments 1 and 2 and has read into
// segment 1; the writer then reuses segment 1's file for segment 3 and fills
// it to the same size. What the reader returns must still be exactly the log
// as it was at open: in order, one generation per segment.
func TestJobLogReaderIsASnapshotAcrossRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openSmallJobLog(t, path, 10<<10, 2560)
	writeChunks(t, w, numberedLines(1, 13), 1<<20) // head 1-9, segment 1: 10-11, segment 2: 12-13
	atOpen := readWholeJobLog(t, path)
	log, err := OpenJobLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	firstPart := len(numberedLines(1, 9)+JobLogTruncatedMarker+string(jobLogSegmentHeader(1))) + len(numberedLine(10))
	got := make([]byte, firstPart)
	if _, err := io.ReadFull(log, got); err != nil {
		t.Fatal(err)
	}
	writeChunks(t, w, numberedLines(14, 15), 1<<20) // segment 3 reuses segment 1's file
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(log)
	if err != nil {
		t.Fatal(err)
	}
	if whole := string(got) + string(rest); whole != atOpen {
		t.Fatalf("snapshot mixed generations:\n%s\nwant\n%s", whole, atOpen)
	}
}

// The snapshot copies each segment under a shared lock, so a rotation that
// wants to reuse that segment waits for the copy to finish rather than
// replacing it mid-copy.
func TestOpenJobLogHoldsOffRotationWhileCopying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openSmallJobLog(t, path, 10<<10, 2560)
	writeChunks(t, w, numberedLines(1, 13), 1<<20)
	atOpen := readWholeJobLog(t, path)
	rotated := make(chan error, 1)
	var once sync.Once
	jobLogSegmentLocked = func() {
		once.Do(func() {
			go func() {
				_, err := w.Write([]byte(numberedLines(14, 15))) // reuses segment 1, being copied now
				rotated <- err
			}()
			select {
			case err := <-rotated:
				t.Errorf("the writer reused a segment while it was being copied (err %v)", err)
				rotated <- err
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
	t.Cleanup(func() { jobLogSegmentLocked = nil })
	log, err := OpenJobLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	jobLogSegmentLocked = nil
	if err := <-rotated; err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	whole, err := io.ReadAll(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(whole) != atOpen {
		t.Fatalf("snapshot =\n%s\nwant\n%s", whole, atOpen)
	}
}

// A follower whose consumer stalls must not hold up the writer: it holds a
// segment's lock only while copying it, so the writer can rotate through the
// very segment the stalled follower is in.
func TestStalledFollowerDoesNotBlockTheWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	w := openSmallJobLog(t, path, 10<<10, 2560)
	writeChunks(t, w, numberedLines(1, 11), 1<<20) // segment 1 (tail.0): 10-11
	stalled, release := make(chan struct{}), make(chan struct{})
	var settled atomic.Bool
	followed := make(chan error, 1)
	go func() {
		followed <- Follow(context.Background(), path, FollowOptions{PollInterval: time.Millisecond, Settled: func(context.Context) (bool, error) { return settled.Load(), nil }}, func(line string) error {
			if strings.HasPrefix(line, "line-10 ") {
				close(stalled)
				<-release
			}
			return nil
		})
	}()
	<-stalled
	wrote := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte(numberedLines(12, 17))) // segments 2, 3 (reusing tail.0) and 4
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the writer is blocked by a stalled follower")
	}
	settled.Store(true)
	close(release)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-followed; err != nil {
		t.Fatal(err)
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

// Review on #2708 round 3: a flat object of 40,000 top-level scalars reaches
// the envelope with 40,000 candidates. The envelope keeps the longest prefix
// that fits in one pass: valid JSON within the line cap, type first, a prefix
// of the original fields in order, the marker last. Before, it re-encoded
// every shorter prefix in turn (about 21,800 encodings here).
func TestJobLogEnvelopeOfManyTopLevelScalarsIsOnePassPrefix(t *testing.T) {
	fields := []string{`"type":"tool_execution_end"`}
	for i := range 40000 {
		fields = append(fields, fmt.Sprintf(`"k%05d":%d`, i, i))
	}
	line := []byte("{" + strings.Join(fields, ",") + "}")
	out, ok := shortenJSONLine(line, JobLogMaxLineBytes)
	if !ok {
		t.Fatal("a JSON object was not shortened")
	}
	if len(out) > JobLogMaxLineBytes {
		t.Fatalf("envelope is %d bytes, over the %d cap", len(out), JobLogMaxLineBytes)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if decoded["gitmoot_truncated"] != true || decoded["type"] != "tool_execution_end" {
		t.Fatalf("envelope lost its type or marker: %.200s", out)
	}
	kept := len(decoded) - 2
	want := "{" + strings.Join(fields[:kept+1], ",") + `,"gitmoot_truncated":true}`
	if string(out) != want {
		t.Fatalf("envelope is not the longest in-order prefix plus the marker: kept %d fields, got %.120s…", kept, out)
	}
	// The longest prefix: one more field would not have fit.
	if next := len(want) + 1 + len(fields[kept+1]); next <= JobLogMaxLineBytes {
		t.Fatalf("kept %d fields, but %d would fit (%d bytes)", kept, kept+1, next)
	}
	allocs := testing.AllocsPerRun(1, func() { shortenJSONLine(line, JobLogMaxLineBytes) })
	// About 1.2M here (decoding 40,000 nodes and the 30 shortening passes); the
	// per-prefix envelope needed hundreds of millions.
	if allocs > 5_000_000 {
		t.Fatalf("shortening took %.0f allocations; the envelope must be built in one pass", allocs)
	}
}
