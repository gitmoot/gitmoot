package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// jobLogStringKeeps are the string lengths shortenJSONLine tries, longest
// first. The shortest still keeps whole ids: omp's toolCallId runs to about
// 110 bytes, and a cut id would no longer pair a tool call with its result.
var jobLogStringKeeps = [...]int{64 << 10, 16 << 10, 4 << 10, 1 << 10, 256}

// shortenJSONLine turns an over-long JSON object line into a valid one of at
// most limit bytes by shortening its long string VALUES, keeping each one's
// head and a "…[N bytes truncated]" suffix inside the string. Everything else
// is kept: the key order (omp's "type" stays first, which is how readers
// classify a line without decoding it), every key, number, boolean and null.
// So a tool event keeps its type, ids, tool name, argument keys and status.
// It reports false when the line is not exactly one JSON object, or when even
// the shortest keep leaves it over the limit.
func shortenJSONLine(line []byte, limit int) ([]byte, bool) {
	for _, keep := range jobLogStringKeeps {
		out, err := shortenJSONStrings(line, keep)
		if err != nil {
			return nil, false
		}
		if len(out) <= limit {
			return out, true
		}
	}
	return nil, false
}

type jsonFrame struct {
	object bool
	items  int // tokens written in this container: keys and values alike
}

// shortenJSONStrings re-encodes one JSON object token by token, so key order
// and layout survive, with every string value longer than keep shortened.
func shortenJSONStrings(line []byte, keep int) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	out := bytes.NewBuffer(make([]byte, 0, JobLogMaxLineBytes))
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	var stack []jsonFrame
	started := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if !started || len(stack) != 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		if len(stack) == 0 {
			if started || tok != json.Delim('{') {
				return nil, errors.New("job log line is not one JSON object")
			}
			started = true
		}
		isKey := false
		if closing := tok == json.Delim('}') || tok == json.Delim(']'); !closing && len(stack) > 0 {
			top := &stack[len(stack)-1]
			switch {
			case top.object && top.items%2 == 1:
				out.WriteByte(':')
			case top.items > 0:
				out.WriteByte(',')
			}
			isKey = top.object && top.items%2 == 0
			top.items++
		}
		switch v := tok.(type) {
		case json.Delim:
			out.WriteByte(byte(v))
			switch v {
			case '{', '[':
				stack = append(stack, jsonFrame{object: v == '{'})
			default:
				stack = stack[:len(stack)-1]
			}
		case string:
			if !isKey && len(v) > keep {
				head := v[:utf8HeadLen([]byte(v[:keep]))]
				v = head + "…[" + strconv.Itoa(len(v)-len(head)) + " bytes truncated]"
			}
			if err := enc.Encode(v); err != nil {
				return nil, err
			}
			out.Truncate(out.Len() - 1) // Encode ends every value with a newline
		case json.Number:
			out.WriteString(v.String())
		case bool:
			out.WriteString(strconv.FormatBool(v))
		case nil:
			out.WriteString("null")
		}
	}
}
