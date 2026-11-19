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

// jobLogArrayKeeps are how many elements shortenJSONLine keeps at EACH end of
// a long array, after first trying no array cut at all (-1). 0 replaces a
// whole array with its one marker element, which still keeps every scalar and
// key outside arrays at every depth - a tool result's isError and
// details.exitCode among them.
var jobLogArrayKeeps = [...]int{-1, 64, 16, 4, 1, 0}

// jsonNode is one decoded JSON value. Objects keep their key order, so omp's
// "type" stays first: readers classify a line by its first key without
// decoding it. A number, boolean or null is kept as its literal text.
type jsonNode struct {
	kind byte // '{', '[', '"' or 0 for a literal
	text string
	keys []string
	vals []*jsonNode
}

// shortenJSONLine turns an over-long JSON object line into a valid one of at
// most limit bytes. It shortens, in order of preference, long string values
// (keeping each one's head and a "…[N bytes truncated]" suffix inside the
// string), then long arrays (keeping their first and last elements around one
// marker element), and keeps every key and every scalar outside a cut array.
// When nothing else fits, the line becomes an envelope: the top-level scalars
// plus "gitmoot_truncated":true. So a line that decodes is never cut into
// invalid JSON. It reports false only when the line is not exactly one JSON
// object.
func shortenJSONLine(line []byte, limit int) ([]byte, bool) {
	root, err := decodeJSONTree(line)
	if err != nil || root.kind != '{' {
		return nil, false
	}
	s := jsonShortener{limit: limit}
	for _, arrayKeep := range jobLogArrayKeeps {
		for _, keep := range jobLogStringKeeps {
			s.keep, s.arrayKeep = keep, arrayKeep
			if out, ok := s.encode(root); ok {
				return out, true
			}
		}
	}
	return s.envelope(root), true
}

func decodeJSONTree(line []byte) (*jsonNode, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	root, err := decodeJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("job log line is not one JSON value")
	}
	return root, nil
}

func decodeJSONValue(dec *json.Decoder) (*jsonNode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		node := &jsonNode{kind: byte(v)}
		for dec.More() {
			if node.kind == '{' {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				node.keys = append(node.keys, key.(string))
			}
			val, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			node.vals = append(node.vals, val)
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return nil, err
		}
		return node, nil
	case string:
		return &jsonNode{kind: '"', text: v}, nil
	case json.Number:
		return &jsonNode{text: v.String()}, nil
	case bool:
		return &jsonNode{text: strconv.FormatBool(v)}, nil
	case nil:
		return &jsonNode{text: "null"}, nil
	default:
		return nil, errors.New("unexpected JSON token")
	}
}

// jsonShortener encodes a tree under one setting. It gives up as soon as the
// output passes limit, so trying a setting that does not fit costs at most
// limit bytes of encoding however large the tree is.
type jsonShortener struct {
	limit     int
	keep      int
	arrayKeep int
	out       []byte
	scratch   bytes.Buffer
	enc       *json.Encoder
}

func (s *jsonShortener) encode(root *jsonNode) ([]byte, bool) {
	s.out = s.out[:0]
	if !s.value(root) {
		return nil, false
	}
	return s.out, true
}

func (s *jsonShortener) value(n *jsonNode) bool {
	switch n.kind {
	case '{':
		s.out = append(s.out, '{')
		for i, key := range n.keys {
			if i > 0 {
				s.out = append(s.out, ',')
			}
			s.out = s.appendString(s.out, key)
			s.out = append(s.out, ':')
			if !s.value(n.vals[i]) {
				return false
			}
		}
		s.out = append(s.out, '}')
	case '[':
		s.out = append(s.out, '[')
		vals := n.vals
		if s.arrayKeep >= 0 && len(vals) > 2*s.arrayKeep {
			head, tail := vals[:s.arrayKeep], vals[len(vals)-s.arrayKeep:]
			vals = append(append(append([]*jsonNode{}, head...), arrayMarker(vals[s.arrayKeep:len(vals)-s.arrayKeep])), tail...)
		}
		for i, val := range vals {
			if i > 0 {
				s.out = append(s.out, ',')
			}
			if !s.value(val) {
				return false
			}
		}
		s.out = append(s.out, ']')
	case '"':
		s.out = s.appendString(s.out, shortenJSONString(n.text, s.keep))
	default:
		s.out = append(s.out, n.text...)
	}
	return len(s.out) <= s.limit
}

// arrayMarker stands in for the elements an array cut drops. It has the SAME
// kind as them when they are all objects: a consumer decoding the array into a
// slice of structs (omp's result.content is one) fails on a string element,
// and the whole event with it, while an object with an unknown key decodes as
// an empty element.
func arrayMarker(dropped []*jsonNode) *jsonNode {
	text := "…[" + strconv.Itoa(len(dropped)) + " elements truncated]"
	for _, n := range dropped {
		if n.kind != '{' {
			return &jsonNode{kind: '"', text: text}
		}
	}
	return &jsonNode{kind: '{', keys: []string{"gitmoot_truncated"}, vals: []*jsonNode{{kind: '"', text: text}}}
}

func shortenJSONString(v string, keep int) string {
	if len(v) <= keep {
		return v
	}
	head := v[:utf8HeadLen([]byte(v[:keep]))]
	return head + "…[" + strconv.Itoa(len(v)-len(head)) + " bytes truncated]"
}

// envelope is the last resort: the top-level scalars, in order, with strings
// at the shortest keep, and "gitmoot_truncated":true. It is built in ONE pass
// with room for the marker reserved, keeping the longest prefix of scalars
// that fits, so even an object of a million keys costs one encoding and still
// opens with its type.
func (s *jsonShortener) envelope(root *jsonNode) []byte {
	const marker = `"gitmoot_truncated":true}`
	s.keep, s.arrayKeep = jobLogStringKeeps[len(jobLogStringKeeps)-1], 0
	s.out = append(s.out[:0], '{')
	kept := 0
	for i, val := range root.vals {
		if val.kind == '{' || val.kind == '[' {
			continue
		}
		mark := len(s.out)
		if kept > 0 {
			s.out = append(s.out, ',')
		}
		s.out = s.appendString(s.out, root.keys[i])
		s.out = append(s.out, ':')
		s.value(val)
		// The marker needs its own separating comma after this pair.
		if len(s.out)+1+len(marker) > s.limit {
			s.out = s.out[:mark]
			break
		}
		kept++
	}
	if kept > 0 {
		s.out = append(s.out, ',')
	}
	return append(s.out, marker...)
}

func (s *jsonShortener) appendString(dst []byte, v string) []byte {
	if s.enc == nil {
		s.enc = json.NewEncoder(&s.scratch)
		s.enc.SetEscapeHTML(false)
	}
	s.scratch.Reset()
	_ = s.enc.Encode(v) // a Go string always encodes
	return append(dst, bytes.TrimSuffix(s.scratch.Bytes(), []byte("\n"))...)
}
