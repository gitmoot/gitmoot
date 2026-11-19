package workflow

import (
	"sync"
	"unicode/utf8"
)

// RedactedTail is an io.Writer that keeps the last max bytes of a stream as
// RedactCommentText would redact the whole stream, in bounded memory. It
// never cuts raw text: bytes are redacted before anything is dropped, and a
// cut only lands where no redaction match crosses it, so a secret split by a
// cut cannot reach the tail without its prefix.
//
// It holds at most redactedTailRawMax unredacted bytes. When that fills, it
// redacts up to a safe cut at least redactedTailHold bytes before the end and
// keeps only the redacted last max bytes of it:
//   - the cut steps back over a key and its separator (`password =`,
//     `"token":`), so a value still to come keeps its prefix;
//   - the cut moves left of every redaction match that crosses it, so a match
//     is redacted whole;
//   - when no such cut exists (one run of non-space bytes fills the window),
//     a match that reaches the end is redacted and the rest of its run is
//     dropped up to the next whitespace, since every redactor's value stops at
//     whitespace.
//
// The one approximation is a value that whitespace alone separates from its
// key by more than redactedTailHold bytes, or a quoted value containing
// whitespace and longer than that: its later part is kept as ordinary text.
type RedactedTail struct {
	mu   sync.Mutex
	max  int
	done []byte
	raw  []byte
	// swallow drops bytes up to the next whitespace: they continue a match
	// already redacted.
	swallow bool
}

const (
	redactedTailHold   = 4 << 10
	redactedTailRawMax = 64 << 10
)

// NewRedactedTail keeps the redacted last max bytes written to it.
func NewRedactedTail(max int) *RedactedTail {
	return &RedactedTail{max: max}
}

func (t *RedactedTail) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := len(data)
	if t.swallow {
		index := 0
		for index < len(data) && !redactSpace(data[index]) {
			index++
		}
		if index == len(data) {
			return count, nil
		}
		t.swallow = false
		data = data[index:]
	}
	t.raw = append(t.raw, data...)
	if len(t.raw) > redactedTailRawMax {
		t.commit()
	}
	return count, nil
}

// String is the redacted tail: at most max bytes, starting on a rune boundary.
func (t *RedactedTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append(append([]byte(nil), t.done...), RedactCommentText(string(t.raw))...)
	if len(out) > t.max {
		out = out[len(out)-t.max:]
	}
	for len(out) > 0 && !utf8.RuneStart(out[0]) {
		out = out[1:]
	}
	return string(out)
}

// commit redacts the unredacted bytes up to a safe cut and keeps the rest.
func (t *RedactedTail) commit() {
	spans := commentRedactionSpans(t.raw)
	cut := clearOfSpans(beforeKeyValue(t.raw, len(t.raw)-redactedTailHold), spans)
	if cut <= 0 {
		// The window is one long run: cut inside it, unless a match crosses
		// the cut; then redact through the furthest such match, and drop the
		// rest of its run if it reaches the end.
		cut = len(t.raw) - redactedTailHold
		end := cut
		for _, span := range spans {
			if span[0] < cut && span[1] > cut {
				end = max(end, span[1])
			}
		}
		cut = end
		t.swallow = cut == len(t.raw)
	}
	t.done = append(t.done, RedactCommentText(string(t.raw[:cut]))...)
	if len(t.done) > t.max {
		t.done = append(t.done[:0], t.done[len(t.done)-t.max:]...)
	}
	t.raw = append(t.raw[:0], t.raw[cut:]...)
}

// beforeKeyValue steps back from cut over whitespace, a run, whitespace and
// a run: a `key = ` or `"key":` whose value has not arrived yet stays with it.
func beforeKeyValue(raw []byte, cut int) int {
	for _, space := range []bool{true, false, true, false} {
		for cut > 0 && redactSpace(raw[cut-1]) == space {
			cut--
		}
	}
	return cut
}

// clearOfSpans moves cut left until no span crosses it.
func clearOfSpans(cut int, spans [][2]int) int {
	for moved := true; moved; {
		moved = false
		for _, span := range spans {
			if span[0] < cut && span[1] > cut {
				cut, moved = span[0], true
			}
		}
	}
	return cut
}

// redactSpace is the redactors' \s: RE2's ASCII [\t\n\f\r ].
func redactSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\f' || b == '\r'
}
