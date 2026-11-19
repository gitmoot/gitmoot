package transcript

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"unicode/utf8"
)

// Job log bounds (#2686). A job log is a retained COPY of the runtime's output:
// the adapter parses its own buffered stdout, so nothing here can change a
// job's result, only what an operator can read back afterwards.
const (
	// JobLogMaxBytes caps one job log file, marker included.
	JobLogMaxBytes = 32 << 20
	// JobLogMaxLineBytes caps the head kept of any one written line.
	JobLogMaxLineBytes = 256 << 10
)

// JobLogTruncatedMarker is the one line written when a job log reaches
// JobLogMaxBytes. Everything the runtime prints after it is dropped from the
// log, never from the stream the adapter parses.
const JobLogTruncatedMarker = "[gitmoot: job log truncated at 32 MiB; further output dropped]\n"

// ompStreamingUpdatePrefixes are the omp `--mode=json` event lines a job log
// does not keep. Both are streaming previews whose content omp writes again in
// full, once, in a later event:
//
//   - message_update carries one assistant-message delta (text, thinking or
//     tool-call arguments, and their *_end blocks); the finished message is
//     message_end, and agent_end repeats every message of the run.
//   - tool_execution_update carries the tool's ENTIRE partial result so far, so
//     a long tool repeats its growing output on every tick and one turn costs
//     the square of its length; the final result is tool_execution_end.
//
// tool_stream_update is kept: it is a fixed-size tick naming only the tool call,
// so it does not repeat growing content. The prefix includes the closing quote
// of the type value, so no other type can match, and omp writes "type" first
// (the same assumption ompEventType makes).
var ompStreamingUpdatePrefixes = [...][]byte{
	[]byte(`{"type":"message_update"`),
	[]byte(`{"type":"tool_execution_update"`),
}

// ompUpdateDecideBytes is how much of a line decides whether it is an omp
// streaming update: the longest prefix above.
const ompUpdateDecideBytes = len(`{"type":"tool_execution_update"`)

// JobLogWriter is the bounded writer beneath every retained job log. It writes
// whole lines only, so a line that arrives in several writes (the E2B backend
// forwards raw process chunks) is joined before it is judged, and it applies,
// in order:
//
//   - for an omp JSON stream, it drops the streaming update events above;
//   - it keeps at most JobLogMaxLineBytes of a line, ending a cut line with
//     "…[N bytes truncated]";
//   - once the file would pass JobLogMaxBytes it writes JobLogTruncatedMarker
//     once and drops everything after it.
//
// Write reports every byte it was handed as consumed, dropped or not, so the
// runtime output that feeds it is never cut short by the log's bounds; only a
// failing write to the file itself is returned. It is not safe for concurrent
// use; the runtime output writers that feed it serialize their writes.
type JobLogWriter struct {
	out            io.Writer
	dropOmpUpdates bool
	limit          int64
	written        int64
	capped         bool

	// line holds the head of the current line, at most JobLogMaxLineBytes;
	// overflow counts the bytes of it that did not fit.
	line     []byte
	overflow int64
	// decided and dropLine record the omp verdict on the current line, made as
	// soon as its first ompUpdateDecideBytes have arrived, so a dropped update
	// is never buffered beyond that.
	decided  bool
	dropLine bool
}

// NewJobLogWriter bounds an open, append-mode job log. The file's current size
// counts toward the cap because a retried job appends to the same log, and a
// log already ending in the marker stays capped, so a retry never writes a
// second one. ompJSON turns on the omp update filter; it is for a runtime
// whose stdout is omp's `--mode=json` stream.
func NewJobLogWriter(file *os.File, ompJSON bool) (*JobLogWriter, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	w := &JobLogWriter{out: file, dropOmpUpdates: ompJSON, limit: JobLogMaxBytes, written: info.Size()}
	if size := info.Size(); size >= int64(len(JobLogTruncatedMarker)) {
		tail := make([]byte, len(JobLogTruncatedMarker))
		if _, err := file.ReadAt(tail, size-int64(len(tail))); err != nil {
			return nil, err
		}
		w.capped = string(tail) == JobLogTruncatedMarker
	}
	return w, nil
}

func (w *JobLogWriter) Write(p []byte) (int, error) {
	rest := p
	for len(rest) > 0 && !w.capped {
		idx := bytes.IndexByte(rest, '\n')
		if idx < 0 {
			w.buffer(rest)
			break
		}
		w.buffer(rest[:idx])
		rest = rest[idx+1:]
		if err := w.endLine(true); err != nil {
			return len(p) - len(rest), err
		}
	}
	return len(p), nil
}

// Close writes a final line the runtime left unterminated, as it was (no
// newline is added), and closes the file.
func (w *JobLogWriter) Close() error {
	var err error
	if !w.capped && (len(w.line) > 0 || w.overflow > 0) {
		err = w.endLine(false)
	}
	if closer, ok := w.out.(io.Closer); ok {
		if closeErr := closer.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func (w *JobLogWriter) buffer(chunk []byte) {
	if w.dropLine {
		return
	}
	if room := JobLogMaxLineBytes - len(w.line); len(chunk) > room {
		w.overflow += int64(len(chunk) - room)
		chunk = chunk[:room]
	}
	w.line = append(w.line, chunk...)
	if w.dropOmpUpdates && !w.decided && len(w.line) >= ompUpdateDecideBytes {
		w.decide()
	}
}

func (w *JobLogWriter) decide() {
	w.decided = true
	for _, prefix := range ompStreamingUpdatePrefixes {
		if bytes.HasPrefix(w.line, prefix) {
			w.dropLine = true
			w.line = w.line[:0]
			w.overflow = 0
			return
		}
	}
}

func (w *JobLogWriter) endLine(terminated bool) error {
	if w.dropOmpUpdates && !w.decided {
		w.decide()
	}
	drop := w.dropLine
	w.decided, w.dropLine = false, false
	if drop {
		return nil
	}
	if w.overflow > 0 {
		w.trimPartialRune()
		w.line = append(w.line, "…["...)
		w.line = strconv.AppendInt(w.line, w.overflow, 10)
		w.line = append(w.line, " bytes truncated]"...)
		w.overflow = 0
	}
	if terminated {
		w.line = append(w.line, '\n')
	}
	if w.written+int64(len(w.line))+int64(len(JobLogTruncatedMarker)) > w.limit {
		w.capped = true
		w.line = nil
		n, err := io.WriteString(w.out, JobLogTruncatedMarker)
		w.written += int64(n)
		return err
	}
	n, err := w.out.Write(w.line)
	w.written += int64(n)
	w.line = w.line[:0]
	return err
}

// trimPartialRune moves an incomplete UTF-8 sequence at the end of a cut line
// into the truncated count, so the kept head stays valid text.
func (w *JobLogWriter) trimPartialRune() {
	start := len(w.line) - 1
	for start > 0 && start > len(w.line)-utf8.UTFMax && !utf8.RuneStart(w.line[start]) {
		start--
	}
	if start >= 0 && !utf8.FullRune(w.line[start:]) {
		w.overflow += int64(len(w.line) - start)
		w.line = w.line[:start]
	}
}
