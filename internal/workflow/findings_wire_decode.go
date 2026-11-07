package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ONE ODD FIELD NO LONGER DROPS THE FINDING (tandem seat, message 412412,
// jerryfane/tandem#2).
//
// A finding rides as free-form JSON, and encoding/json fails the WHOLE object
// when any ONE field has another shape. The writer then fell back to "is it a
// bare string?", and since it was not, the finding was skipped. Review job
// local-review-review-router-18e5a038be33e82e-1 sent a complete P2 - severity,
// title, file, body - whose `evidence` was an array of two strings, and the
// ledger recorded 0 of 1. Measured over the 30 days to 2026-11-07: 608
// findings_ledger_skipped events across 293 review jobs were whole findings lost
// this way - `line` as a string such as "145-169" (281), `evidence` as an array
// (97) or object (63), `evidence_locator` as an array (86) or object (6), and
// `location` as {path,start_line,end_line} (75).
//
// So reviewFindingWire decodes FIELD BY FIELD, and each value shape has one
// rule, the same for every field:
//
//   - null does what encoding/json does with it, in key order: a slice, map,
//     pointer or interface field becomes nil, and any other field is left as it
//     was. So {"relevance_keys":["a"],"relevance_keys":null} ends nil, while
//     {"title":"a","title":null} keeps "a" - both exactly as json.Unmarshal.
//   - Text fields: a string is taken as is. An ARRAY is joined with "\n", one
//     element per line, each element rendered by these same rules. For the
//     locator-like fields (location, evidence_locator, locator) an OBJECT
//     carrying `file` or `path`, and optionally `line`, `start_line` or
//     `line_start`, renders as "path:line" (or "path"), the form
//     db.IsStructuralFindingLocator accepts. Any other value - an object, a
//     number, a boolean - keeps its compact JSON text, so nothing is invented
//     and nothing is lost.
//   - `line`: an integral number, or a string whose leading whole number is not
//     followed by a letter, digit or '.' - "145", "145-169", "14,27-28",
//     "2023 (with ...)" - gives that first number. Anything else leaves 0, keeps
//     the rest of the finding, and is noted (decodeNotes) for any later skip.
//   - `relevance_keys`: an array, each element by the text rule; a single string
//     is one key.
//
// NEWLINE, NOT "; ", FOR EVERY ARRAY, locators included: one rule instead of
// two, and pathFromLensEvidence reads the first whitespace-delimited token, so
// the first element's path is still found - where "a/b.go; c/d.go" would have
// produced the path "a/b.go;". A joined multi-element locator is not
// structural, so it lands in the detail fallback rather than as a locator.
//
// Key matching mirrors encoding/json, so no existing spelling changes meaning:
// the exact json tag first, else a case-insensitive match, and a later duplicate
// key overwrites an earlier one. Fields are found by reflection over the struct
// tags, so a field added to reviewFindingWire is decoded tolerantly without a
// second list to keep in step.
func (w *reviewFindingWire) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if string(trimmed) == "null" {
		return nil
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("finding is a JSON %s, not an object", jsonValueKind(trimmed))
	}
	members, err := orderedJSONMembers(trimmed)
	if err != nil {
		return err
	}
	value := reflect.ValueOf(w).Elem()
	for _, member := range members {
		index := reviewFindingWireFieldIndex(member.key)
		if index < 0 {
			continue
		}
		if string(bytes.TrimSpace(member.raw)) == "null" {
			switch field := value.Field(index); field.Kind() {
			case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Interface:
				field.SetZero()
			}
			continue
		}
		name := reviewFindingWireFieldNames[index]
		locatorLike := reviewFindingLocatorFields[name]
		switch target := value.Field(index).Addr().Interface().(type) {
		case *string:
			*target = lenientFindingText(member.raw, locatorLike)
		case *int:
			line, ok := lenientFindingLine(member.raw)
			*target = line
			if !ok {
				w.decodeNotes = append(w.decodeNotes,
					fmt.Sprintf("field %q value %s is not a line number, left 0", member.key, boundedJSONText(member.raw)))
			}
		case *[]string:
			*target = lenientFindingTexts(member.raw, locatorLike)
		default:
			if err := json.Unmarshal(member.raw, target); err != nil {
				w.decodeNotes = append(w.decodeNotes, fmt.Sprintf("field %q not read: %v", member.key, err))
			}
		}
	}
	return nil
}

// decodeReviewFinding reads one reported finding: an object field by field, a
// bare string through wireFromBareFindingText, and anything else - a number, a
// boolean, an array - is refused with a reason naming what it was.
func decodeReviewFinding(raw json.RawMessage) (reviewFindingWire, error) {
	var wire reviewFindingWire
	err := json.Unmarshal(raw, &wire)
	if err == nil {
		return wire, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return wireFromBareFindingText(text), nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return reviewFindingWire{}, fmt.Errorf("finding object could not be decoded: %v", err)
	}
	return reviewFindingWire{}, fmt.Errorf("finding is a JSON %s, neither an object nor a string", jsonValueKind(trimmed))
}

// decodeNoteSuffix renders the fields this finding's decode could not take, for
// a skip reason, so a finding that is still not recorded names the field too.
func (w reviewFindingWire) decodeNoteSuffix() string {
	if len(w.decodeNotes) == 0 {
		return ""
	}
	return " (decoded with: " + strings.Join(w.decodeNotes, "; ") + ")"
}

// reviewFindingLocatorFields are the text fields whose object form names a place
// in the tree rather than carrying prose.
var reviewFindingLocatorFields = map[string]bool{"location": true, "evidence_locator": true, "locator": true}

// reviewFindingWireFieldNames is the JSON key encoding/json would use for each
// field of reviewFindingWire, indexed like the struct; "" marks a field it would
// not decode (unexported, or tagged "-").
var reviewFindingWireFieldNames = func() []string {
	typ := reflect.TypeOf(reviewFindingWire{})
	names := make([]string, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = field.Name
		}
		names[i] = name
	}
	return names
}()

// reviewFindingWireFieldIndex resolves a JSON key the way encoding/json does: an
// exact name first, then a case-insensitive one. -1 means no field reads it.
func reviewFindingWireFieldIndex(key string) int {
	for i, name := range reviewFindingWireFieldNames {
		if name != "" && name == key {
			return i
		}
	}
	for i, name := range reviewFindingWireFieldNames {
		if name != "" && strings.EqualFold(name, key) {
			return i
		}
	}
	return -1
}

type jsonMember struct {
	key string
	raw json.RawMessage
}

// orderedJSONMembers splits an object into its members in document order, so a
// duplicate key resolves the way encoding/json resolves it: the later one wins.
func orderedJSONMembers(data []byte) ([]jsonMember, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var members []jsonMember
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("finding object key %v is not a string", token)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		members = append(members, jsonMember{key: key, raw: raw})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return members, nil
}

// lenientFindingText renders one value of a text field by the rules on
// UnmarshalJSON.
func lenientFindingText(raw json.RawMessage, locatorLike bool) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			return text
		}
	case '[':
		return strings.Join(lenientFindingTexts(trimmed, locatorLike), "\n")
	case '{':
		if locatorLike {
			if locator, ok := locatorFromFindingObject(trimmed); ok {
				return locator
			}
		}
	case 'n':
		if string(trimmed) == "null" {
			return ""
		}
	}
	return compactJSONText(trimmed)
}

// lenientFindingTexts renders a list field: each element of an array by the
// text rule, dropping null and blank elements, or any single value as one entry.
func lenientFindingTexts(raw json.RawMessage, locatorLike bool) []string {
	trimmed := bytes.TrimSpace(raw)
	var elements []json.RawMessage
	if len(trimmed) == 0 || trimmed[0] != '[' || json.Unmarshal(trimmed, &elements) != nil {
		if text := lenientFindingText(trimmed, locatorLike); strings.TrimSpace(text) != "" {
			return []string{text}
		}
		return nil
	}
	texts := make([]string, 0, len(elements))
	for _, element := range elements {
		if text := lenientFindingText(element, locatorLike); strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

// locatorFromFindingObject renders {file|path, line|start_line|line_start} as
// "path:line", or "path" when no usable line is given. An object naming no path
// is not a locator, and the caller keeps its JSON text instead.
func locatorFromFindingObject(raw json.RawMessage) (string, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return "", false
	}
	path := ""
	for _, key := range []string{"file", "path"} {
		var text string
		if json.Unmarshal(members[key], &text) == nil && strings.TrimSpace(text) != "" {
			path = strings.TrimSpace(text)
			break
		}
	}
	if path == "" {
		return "", false
	}
	for _, key := range []string{"line", "start_line", "line_start"} {
		if value, present := members[key]; present {
			if line, ok := lenientFindingLine(value); ok && line > 0 {
				// Concatenation, not Sprintf("%s:%d"): a source locator, not a
				// network address (same note as the STATIC arm's locator).
				return path + ":" + strconv.Itoa(line), true
			}
		}
	}
	return path, true
}

// lenientFindingLine reads a line number by the rule on UnmarshalJSON. ok is
// false when a value was present and could not be read as one.
func lenientFindingLine(raw json.RawMessage) (int, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return 0, true
	}
	switch {
	case trimmed[0] == '"':
		var text string
		if json.Unmarshal(trimmed, &text) != nil {
			return 0, false
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return 0, true
		}
		end := 0
		for end < len(text) && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, false
		}
		if end < len(text) {
			next, _ := utf8.DecodeRuneInString(text[end:])
			if next == '.' || unicode.IsLetter(next) || unicode.IsDigit(next) {
				return 0, false
			}
		}
		line, err := strconv.Atoi(text[:end])
		if err != nil {
			return 0, false
		}
		return line, true
	case trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9'):
		var number json.Number
		if json.Unmarshal(trimmed, &number) != nil {
			return 0, false
		}
		if line, err := strconv.ParseInt(number.String(), 10, 0); err == nil {
			return int(line), true
		}
		value, err := number.Float64()
		if err != nil || value != math.Trunc(value) || value > math.MaxInt32 || value < math.MinInt32 {
			return 0, false
		}
		return int(value), true
	}
	return 0, false
}

func compactJSONText(raw []byte) string {
	var buffer bytes.Buffer
	if json.Compact(&buffer, raw) != nil {
		return string(raw)
	}
	return buffer.String()
}

// boundedJSONText quotes a reviewer value in a note without letting a large one
// turn the note into a payload.
func boundedJSONText(raw json.RawMessage) string {
	text := compactJSONText(bytes.TrimSpace(raw))
	if len(text) > 80 {
		text, _, _ = truncateAtRune(text, 80)
		text += "..."
	}
	return text
}

func jsonValueKind(raw []byte) string {
	if len(raw) == 0 {
		return "empty value"
	}
	switch raw[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}
