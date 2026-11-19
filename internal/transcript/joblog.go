package transcript

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"syscall"
	"unicode/utf8"
)

// Job log bounds (#2686). A job log is a retained COPY of the runtime's output:
// the adapter parses its own buffered stdout, so nothing here can change a
// job's result, only what an operator can read back afterwards.
//
// A log is a HEAD file (the job's .log) plus two alternating TAIL segments
// beside it. The head keeps the start of the run; once it is full, later lines
// go to the current tail segment, and when that fills the other one is emptied
// and takes over. So the end of a run, where its fatal error and terminal
// events are, is always on disk, while the job is still running too: recovery
// reads it after the writing process has died.
const (
	// JobLogMaxBytes bounds a log on disk: its head plus both tail segments.
	JobLogMaxBytes = JobLogHeadBytes + 2*JobLogTailSegmentBytes
	// JobLogHeadBytes caps the head file, truncation marker included.
	JobLogHeadBytes = 24 << 20
	// JobLogTailSegmentBytes caps one tail segment, its header line included.
	JobLogTailSegmentBytes = 4 << 20
	// JobLogMaxLineBytes caps one written line, newline excluded.
	JobLogMaxLineBytes = 256 << 10
	// jobLogMaxStructuredLineBytes is how much of a JSON line is held to shorten
	// it into a valid event; a longer one gets the raw cut.
	jobLogMaxStructuredLineBytes = 16 << 20
)

// JobLogTruncatedMarker is the last line of a full head. Output after it is in
// the tail segments, which keep only the most recent part of the run.
const JobLogTruncatedMarker = "[gitmoot: job log head truncated at 24 MiB; later output is kept in two rotating 4 MiB tail segments, so a middle part of the run may be missing]\n"

// jobLogSegmentHeaderPrefix opens the first line of every tail segment, which
// numbers it: readers order the segments by that number, and a gap between it
// and the head's marker is output that was rotated away.
const jobLogSegmentHeaderPrefix = "[gitmoot: job log tail segment "

// JobLogTailPaths are the two tail segment files of the log at path. Every
// writer, reader and remover of a job log derives them here.
func JobLogTailPaths(path string) [2]string {
	return [2]string{path + ".tail.0", path + ".tail.1"}
}

func jobLogSegmentHeader(seq int64) []byte {
	return []byte(jobLogSegmentHeaderPrefix + strconv.FormatInt(seq, 10) + "]\n")
}

// readJobLogSegmentSeq returns the number in a tail segment's header, or 0 for
// a segment with no header yet (created, or emptied, and not yet written).
func readJobLogSegmentSeq(file *os.File) (int64, error) {
	buf := make([]byte, len(jobLogSegmentHeaderPrefix)+24)
	n, err := file.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	rest, ok := bytes.CutPrefix(buf[:n], []byte(jobLogSegmentHeaderPrefix))
	if !ok {
		return 0, nil
	}
	end := bytes.Index(rest, []byte("]\n"))
	if end <= 0 {
		return 0, nil
	}
	seq, err := strconv.ParseInt(string(rest[:end]), 10, 64)
	if err != nil || seq <= 0 {
		return 0, nil
	}
	return seq, nil
}

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
//   - it bounds a line to JobLogMaxLineBytes: a JSON object keeps its structure
//     with its long strings shortened (shortenJSONLine), anything else keeps
//     its head and ends with "…[N bytes truncated]";
//   - it appends to the head until the head is full, then writes
//     JobLogTruncatedMarker once and continues in the rotating tail segments.
//
// Every placement decision is made under flock(LOCK_EX) on the head file from
// the file's REAL size and contents, never from a count this handle keeps, so
// any number of writers on one log share one budget and one marker: a retried
// job reopening its log, or two handles on the same path.
//
// Write reports every byte it was handed as consumed, dropped or not, so the
// runtime output that feeds it is never cut short by the log's bounds; only a
// failing write to the files themselves is returned. One handle is not safe
// for concurrent use; the runtime output writers that feed it serialize their
// writes.
type JobLogWriter struct {
	head           *os.File
	tailPaths      [2]string
	tails          [2]*os.File
	dropOmpUpdates bool
	headLimit      int64
	segmentLimit   int64
	// capped records that the head ends in the marker. It is only ever set from
	// the file itself, and a full head never grows again, so it can be kept.
	capped bool
	locked bool

	// line holds the current line; overflow counts the bytes of it that did
	// not fit. structured marks a line that opened with '{', which is held
	// whole (up to jobLogMaxStructuredLineBytes) so it can be shortened as JSON.
	line       []byte
	overflow   int64
	structured bool
	// decided and dropLine record the omp verdict on the current line, made as
	// soon as its first ompUpdateDecideBytes have arrived, so a dropped update
	// is never buffered beyond that.
	decided  bool
	dropLine bool
}

// OpenJobLogWriter opens (creating it 0600) the append-only log at path and
// bounds it. A log is append-only across a job's retries, so whatever is
// already on disk counts. ompJSON turns on the omp update filter; it is for a
// runtime whose stdout is omp's `--mode=json` stream.
func OpenJobLogWriter(path string, ompJSON bool) (*JobLogWriter, error) {
	head, err := openJobLogFile(path)
	if err != nil {
		return nil, err
	}
	return &JobLogWriter{
		head:           head,
		tailPaths:      JobLogTailPaths(path),
		dropOmpUpdates: ompJSON,
		headLimit:      JobLogHeadBytes,
		segmentLimit:   JobLogTailSegmentBytes,
	}, nil
}

// openJobLogFile opens a log file for appending, and for reading, which the
// writer needs to inspect the head's last line and a segment's header.
func openJobLogFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (w *JobLogWriter) Write(p []byte) (n int, err error) {
	// One lock for every line this call completes: a pipe chunk usually
	// carries several.
	defer func() {
		if unlockErr := w.unlock(); err == nil {
			err = unlockErr
		}
	}()
	rest := p
	for len(rest) > 0 {
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
// newline is added), and closes the files.
func (w *JobLogWriter) Close() error {
	var err error
	if len(w.line) > 0 || w.overflow > 0 || w.dropLine {
		err = w.endLine(false)
	}
	if unlockErr := w.unlock(); err == nil {
		err = unlockErr
	}
	for _, tail := range w.tails {
		if tail != nil {
			if closeErr := tail.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if closeErr := w.head.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (w *JobLogWriter) buffer(chunk []byte) {
	if w.dropLine {
		return
	}
	if len(w.line) == 0 && w.overflow == 0 && len(chunk) > 0 {
		w.structured = chunk[0] == '{'
	}
	limit := JobLogMaxLineBytes
	if w.structured {
		limit = jobLogMaxStructuredLineBytes
	}
	if room := limit - len(w.line); len(chunk) > room {
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
	defer w.resetLine()
	if w.dropLine {
		return nil
	}
	return w.place(w.render(terminated))
}

func (w *JobLogWriter) resetLine() {
	if cap(w.line) > 2*JobLogMaxLineBytes {
		w.line = nil // do not keep a structured line's buffer for the rest of the run
	}
	w.line = w.line[:0]
	w.overflow = 0
	w.structured, w.decided, w.dropLine = false, false, false
}

// render bounds the current line. A JSON object is shortened into a valid,
// smaller event, because a cut one no longer decodes and every reader would
// lose its type, ids and status. The raw cut is the fallback, for a line that
// is not a JSON object, does not decode, or stays too big after shortening.
func (w *JobLogWriter) render(terminated bool) []byte {
	if total := int64(len(w.line)) + w.overflow; total > JobLogMaxLineBytes {
		short, ok := []byte(nil), false
		if w.structured && w.overflow == 0 {
			short, ok = shortenJSONLine(w.line, JobLogMaxLineBytes)
		}
		if ok {
			w.line = append(w.line[:0], short...)
		} else {
			w.line = w.line[:JobLogMaxLineBytes]
			w.overflow = total - JobLogMaxLineBytes
			w.trimPartialRune()
			w.line = appendTruncatedSuffix(w.line, w.overflow)
		}
	}
	if terminated {
		w.line = append(w.line, '\n')
	}
	return w.line
}

func appendTruncatedSuffix(dst []byte, dropped int64) []byte {
	dst = append(dst, "…["...)
	dst = strconv.AppendInt(dst, dropped, 10)
	return append(dst, " bytes truncated]"...)
}

// trimPartialRune moves an incomplete UTF-8 sequence at the end of a cut line
// into the truncated count, so the kept head stays valid text.
func (w *JobLogWriter) trimPartialRune() {
	keep := utf8HeadLen(w.line)
	w.overflow += int64(len(w.line) - keep)
	w.line = w.line[:keep]
}

// utf8HeadLen is the length of b without an incomplete UTF-8 sequence at its
// end.
func utf8HeadLen(b []byte) int {
	start := len(b) - 1
	for start > 0 && start > len(b)-utf8.UTFMax && !utf8.RuneStart(b[start]) {
		start--
	}
	if start >= 0 && !utf8.FullRune(b[start:]) {
		return start
	}
	return len(b)
}

func (w *JobLogWriter) lock() error {
	if w.locked {
		return nil
	}
	if err := syscall.Flock(int(w.head.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	w.locked = true
	return nil
}

func (w *JobLogWriter) unlock() error {
	if !w.locked {
		return nil
	}
	w.locked = false
	return syscall.Flock(int(w.head.Fd()), syscall.LOCK_UN)
}

// place appends one rendered line where it belongs, under the lock.
func (w *JobLogWriter) place(line []byte) error {
	if err := w.lock(); err != nil {
		return err
	}
	if !w.capped {
		info, err := w.head.Stat()
		if err != nil {
			return err
		}
		size := info.Size()
		capped, err := endsWithMarker(w.head, size)
		if err != nil {
			return err
		}
		if !capped {
			if size+int64(len(line))+int64(len(JobLogTruncatedMarker)) <= w.headLimit {
				_, err := w.head.Write(line)
				return err
			}
			if _, err := w.head.Write([]byte(JobLogTruncatedMarker)); err != nil {
				return err
			}
		}
		w.capped = true
	}
	return w.appendTail(line)
}

func endsWithMarker(file *os.File, size int64) (bool, error) {
	if size < int64(len(JobLogTruncatedMarker)) {
		return false, nil
	}
	tail := make([]byte, len(JobLogTruncatedMarker))
	if _, err := file.ReadAt(tail, size-int64(len(tail))); err != nil {
		return false, err
	}
	return string(tail) == JobLogTruncatedMarker, nil
}

// appendTail appends a line to the current tail segment, the one with the
// highest header number, first rotating to the other segment when the line
// would overflow it. Rotation empties the other segment IN PLACE (no rename),
// so every handle on the log keeps addressing the same two files.
func (w *JobLogWriter) appendTail(line []byte) error {
	current, seq := -1, int64(0)
	var currentSize int64
	for i := range w.tails {
		file, err := w.tail(i)
		if err != nil {
			return err
		}
		segSeq, err := readJobLogSegmentSeq(file)
		if err != nil {
			return err
		}
		if segSeq > seq {
			info, err := file.Stat()
			if err != nil {
				return err
			}
			current, seq, currentSize = i, segSeq, info.Size()
		}
	}
	if current < 0 || currentSize+int64(len(line)) > w.segmentLimit {
		next := 0
		if current >= 0 {
			next = 1 - current
		}
		file := w.tails[next]
		if err := file.Truncate(0); err != nil {
			return err
		}
		if _, err := file.Write(jobLogSegmentHeader(seq + 1)); err != nil {
			return err
		}
		current = next
	}
	_, err := w.tails[current].Write(line)
	return err
}

func (w *JobLogWriter) tail(i int) (*os.File, error) {
	if w.tails[i] == nil {
		file, err := openJobLogFile(w.tailPaths[i])
		if err != nil {
			return nil, err
		}
		w.tails[i] = file
	}
	return w.tails[i], nil
}
